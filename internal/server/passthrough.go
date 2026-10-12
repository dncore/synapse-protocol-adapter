package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/dncore/synapse-protocol-adapter/internal/completions"
	"github.com/dncore/synapse-protocol-adapter/internal/config"
	"github.com/dncore/synapse-protocol-adapter/internal/converter"
	"github.com/dncore/synapse-protocol-adapter/internal/metrics"
	"github.com/dncore/synapse-protocol-adapter/internal/middleware"
	"github.com/dncore/synapse-protocol-adapter/internal/upstream"
)

// handlePassthrough transparently forwards any /v1/* or /api/v1/*
// request (other than the /v1/responses routes the converter owns) to
// the configured upstream: same method, same path below the prefix, same
// query, body streamed unmodified, response streamed byte-for-byte with
// a flush per read. /api/anthropic/* additionally forwards with the path
// preserved against the upstream HOST root — Anthropic-protocol mounts
// live beside the chat-completions base_url, not under it. Upstream
// errors pass through verbatim — this route never rewrites semantics.
//
// This makes the proxy a scoped reverse proxy for its single upstream
// (not an open proxy): everything under the served prefixes reaches that
// provider and nothing else, which is exactly the local-forwarder role
// socat plays — with pooling, cancellation propagation, backpressure,
// and metrics that socat does not have.
func (s *Server) handlePassthrough(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	s.metrics.RequestsTotal.Inc()
	s.metrics.PassthroughRequestsTotal.Inc()
	s.metrics.ActiveRequests.Inc()
	defer s.metrics.ActiveRequests.Dec()

	releaseUser := s.enterUser(w, r)
	if releaseUser == nil {
		return
	}
	defer releaseUser()

	release := s.acquireSlot(r)
	if release == nil {
		return // client went away while queued
	}
	defer release()

	// Small bodies (declared Content-Length within the buffer budget) are
	// read into memory so a transient 429 can be retried with the same
	// bytes, and so the model-quirk rewriter can edit them; larger or
	// length-less bodies stream straight through, exactly as before, and
	// are neither retried nor rewritten.
	buffered, err := s.bufferRequestBody(r)
	if err != nil {
		s.metrics.ErrorsTotal.With("client_canceled").Inc()
		return // client aborted while sending its body
	}
	// The rewrite budget is a hard cap on what the rewriter touches, even
	// when the retry buffer happened to hold a larger body.
	if buffered != nil && s.cfg.Passthrough.ModelQuirks && int64(len(buffered)) <= s.cfg.Passthrough.MaxRewriteBytes {
		if rewritten, rules := normalizeModelQuirks(r.URL.Path, buffered); len(rules) > 0 {
			for _, rule := range rules {
				s.metrics.PassthroughRewritesTotal.With(rule).Inc()
			}
			buffered = rewritten
		}
	}

	// A model the gateway has been failing may have a configured fallback;
	// the substitution rides the same rewrite gate as the quirk rules (the
	// route's byte-for-byte promise, and this one exception to it).
	if buffered != nil && s.cfg.Passthrough.ModelQuirks {
		if rewritten, from, to := s.substituteUnavailableModel(buffered); to != "" {
			s.metrics.ModelFallbackTotal.With(from).Inc()
			s.logger.Warn("model marked unavailable; serving with its configured fallback",
				"request_id", middleware.RequestIDOf(r), "from", from, "to", to)
			buffered = rewritten
		}
	}

	// Anthropic-mount takeover: only the requests the upstream's own
	// translator cannot serve are translated locally (see anthropic.go). A
	// candidate that turns out unsupported falls back to the passthrough
	// path below, so the client sees the upstream's answer as before.
	if buffered != nil {
		if areq := s.anthropicTakeover(r.URL.Path, buffered); areq != nil {
			s.metrics.AnthropicTakeoverTotal.Inc()
			if s.handleAnthropicTakeover(w, r, areq, start) {
				return
			}
		}
	}

	// Tool-call emulation for chat clients while the model's native tool
	// calling is failing — the same contract as the Responses and Anthropic
	// routes (tool_emulation config, marked models only): the tools move into
	// the prompt, the upstream call becomes non-streaming (the protocol reply
	// must be complete before it can be parsed), and the reply is parsed back
	// into a real tool_calls answer. Streaming clients get synthesized chat
	// chunks.
	emulating := false
	model, hasTools := bodyModelShape(buffered)
	var clientStreams bool
	if buffered != nil && s.shouldEmulate(model, hasTools) {
		var req completions.Request
		if err := json.Unmarshal(buffered, &req); err == nil {
			toolCount := len(req.Tools)
			if converter.EmulateToolProtocol(&req) {
				clientStreams = req.Stream
				req.Stream = false
				req.StreamOptions = nil
				if nb, merr := json.Marshal(&req); merr == nil {
					buffered = nb
					emulating = true
					s.metrics.ToolEmulationTotal.With(model).Inc()
					s.logger.Warn("emulating tool calls in the prompt (upstream tool calling unavailable)",
						"request_id", middleware.RequestIDOf(r), "model", model, "tools", toolCount)
				}
			}
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.Timeouts.Request)
	defer cancel()

	upStart := time.Now()
	attempt := func() (*http.Response, error) {
		var body io.Reader
		if buffered != nil {
			body = bytes.NewReader(buffered)
		} else {
			body = &countingReader{r: r.Body, total: s.metrics.BytesInTotal}
		}
		// /api/anthropic/* keeps its full path against the upstream host
		// root; the /v1 styles strip their prefix onto the base_url.
		if strings.HasPrefix(r.URL.Path, "/api/anthropic/") {
			return s.client.DoPassthroughRoot(ctx, r.Method, r.URL.Path, r.URL.RawQuery, body, r.Header)
		}
		return s.client.DoPassthrough(ctx, r.Method,
			stripAPIPrefix(r.URL.Path), r.URL.RawQuery, body, r.Header)
	}
	// model/hasTools were read off the body before the emulation rewrite,
	// purely for the per-model health bookkeeping.
	healthModel := healthKey(model, hasTools)
	if emulating {
		// An emulated success is our prompting, not the upstream's tool
		// calling recovering — never health evidence (same as the other
		// routes).
		healthModel = ""
	}
	resp, err := s.doUpstreamRetry(ctx, r, "passthrough", buffered != nil, healthModel, attempt)
	if err != nil {
		s.metrics.UpstreamErrorsTotal.With(classifyUpstreamError(err)).Inc()
		s.metrics.ErrorsTotal.With("upstream").Inc()
		if r.Context().Err() != nil {
			return // client gone; nothing to write to
		}
		if errors.Is(err, context.DeadlineExceeded) {
			s.writeProxyError(w, http.StatusGatewayTimeout, "upstream request timed out")
			return
		}
		s.writeProxyError(w, http.StatusBadGateway, "upstream request failed: "+err.Error())
		return
	}
	defer resp.Body.Close()
	s.metrics.UpstreamConnections.Inc()
	s.metrics.UpstreamDuration.Observe(time.Since(upStart).Seconds())

	if resp.StatusCode == http.StatusTooManyRequests && s.cfg.UpstreamRetry.Enabled {
		// A 429 that survived the retry budget (or a non-replayable
		// body): answer with Retry-After instead of mirroring it bare.
		s.metrics.UpstreamErrorsTotal.With("http_4xx").Inc()
		s.rejectUpstream429(w, resp)
		return
	}
	if emulating && resp.StatusCode < http.StatusBadRequest {
		s.writeEmulatedChatResponse(w, r, resp, clientStreams)
		return
	}

	upstream.ForwardResponseHeaders(w.Header(), resp.Header)
	if resp.Uncompressed {
		// Go transparently decoded the body; forwarding the encoding header
		// would advertise gzip that is no longer there.
		w.Header().Del("Content-Encoding")
	}
	w.WriteHeader(resp.StatusCode)

	rc := http.NewResponseController(w)
	// Flush headers the moment upstream's headers arrive: clients must see
	// the response start without waiting for the first body byte, and the
	// server only begins client-disconnect detection once bytes hit the
	// wire.
	_ = rc.Flush()

	buf := make([]byte, 32*1024)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			// Per-write deadline: a stalled client may hold the stream for
			// at most stream_write, while a healthy long stream never trips
			// a total-duration ceiling.
			_ = rc.SetWriteDeadline(time.Now().Add(s.cfg.Timeouts.StreamWrite))
			if _, werr := w.Write(buf[:n]); werr != nil {
				cancel() // client vanished mid-stream: abort upstream now
				s.metrics.ErrorsTotal.With("client_canceled").Inc()
				return
			}
			_ = rc.Flush()
			s.metrics.BytesOutTotal.Add(int64(n))
		}
		if rerr != nil {
			break // io.EOF or upstream cut the stream
		}
	}
	s.metrics.ResponsesTotal.Inc()
	s.metrics.RequestDuration.Observe(time.Since(start).Seconds())
}

// writeEmulatedChatResponse turns an emulated (necessarily complete) upstream
// chat reply into the client's answer: the protocol payload is parsed back
// into a real tool_calls response and re-rendered — as chat JSON, or as
// synthesized chat SSE chunks for streaming clients. A reply that is not the
// protocol (the model answered in text) passes through unchanged, and so does
// any body that does not parse as a chat response at all.
func (s *Server) writeEmulatedChatResponse(w http.ResponseWriter, r *http.Request, resp *http.Response, clientStreams bool) {
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		s.writeProxyError(w, http.StatusBadGateway, "upstream read failed: "+err.Error())
		return
	}
	var out completions.Response
	if json.Unmarshal(raw, &out) != nil {
		// Not a shape the parser knows: hand the upstream bytes through
		// untouched rather than losing them.
		upstream.ForwardResponseHeaders(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		n, _ := w.Write(raw)
		s.metrics.BytesOutTotal.Add(int64(n))
		s.metrics.ResponsesTotal.Inc()
		return
	}
	if len(out.Choices) > 0 {
		msg := &out.Choices[0].Message
		if name, args, ok := converter.ParseEmulatedToolCall(msg.Content.String); ok {
			if converter.ApplyEmulatedToolCall(&out, emulatedCallID(), name, args) {
				s.metrics.ToolEmulationParsedTotal.Inc()
				s.logger.Info("emulated tool call parsed from the model's reply",
					"request_id", middleware.RequestIDOf(r), "tool", name,
					"call_id", msg.ToolCalls[0].ID)
			}
		}
	}
	var body []byte
	if clientStreams {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		body = converter.SynthesizeChatSSE(&out)
	} else {
		w.Header().Set("Content-Type", "application/json")
		body, err = json.Marshal(&out)
		if err != nil {
			s.writeProxyError(w, http.StatusInternalServerError, "internal conversion error")
			return
		}
	}
	w.WriteHeader(http.StatusOK)
	n, _ := w.Write(body)
	s.metrics.BytesOutTotal.Add(int64(n))
	s.metrics.ResponsesTotal.Inc()
}

// stripAPIPrefix removes the client-facing API prefix (/v1 or /api/v1) so
// the remainder routes onto the configured base_url, whichever path
// convention the client was configured with.
func stripAPIPrefix(p string) string {
	if strings.HasPrefix(p, "/api/v1") {
		return strings.TrimPrefix(p, "/api/v1")
	}
	return strings.TrimPrefix(p, "/v1")
}

// bufferRequestBody returns the request body verbatim when it is small
// enough and its length is known, so the passthrough route can (a) re-send
// it after an upstream 429 and (b) rewrite known upstream-model quirks. The
// budget is the largest of the enabled consumers': the retry buffer
// (upstream_retry.buffer_max_bytes) and the rewriter limit
// (passthrough.max_rewrite_bytes). When it returns nil the caller streams
// r.Body as before — no retry, no rewrite.
func (s *Server) bufferRequestBody(r *http.Request) ([]byte, error) {
	limit := int64(0)
	if rc := s.cfg.UpstreamRetry; rc.Enabled && rc.BufferMaxBytes > 0 {
		limit = int64(rc.BufferMaxBytes)
	}
	if s.cfg.Passthrough.ModelQuirks && rewriteEligible(r) && s.cfg.Passthrough.MaxRewriteBytes > limit {
		limit = s.cfg.Passthrough.MaxRewriteBytes
	}
	// The Anthropic takeover needs the decoded body to decide, so eligible
	// requests are buffered up to the same ceiling the converted route uses.
	if s.anthropicTakeoverEligible(r) && int64(s.cfg.Limits.MaxBodyBytes) > limit {
		limit = int64(s.cfg.Limits.MaxBodyBytes)
	}
	if limit == 0 || r.Body == nil || r.ContentLength < 0 || r.ContentLength > limit {
		return nil, nil
	}
	buf := make([]byte, r.ContentLength)
	if _, err := io.ReadFull(r.Body, buf); err != nil {
		return nil, err
	}
	s.metrics.BytesInTotal.Add(r.ContentLength)
	return buf, nil
}

// anthropicTakeoverEligible reports whether the request could be served by
// the Anthropic-mount takeover, i.e. whether its body must be buffered for the
// election to happen at all. The model/tools decision needs the decoded body
// (anthropicTakeover).
func (s *Server) anthropicTakeoverEligible(r *http.Request) bool {
	return s.cfg.Upstream.AnthropicMode == config.AnthropicModeGPT6Tools &&
		r.Method == http.MethodPost && isAnthropicMessagesPath(r.URL.Path)
}

// rewriteEligible reports whether the model-quirk rewriter could ever apply
// to this request: a POST to one of the two endpoints whose bodies the
// affected clients shape (chat completions and the Anthropic mount's
// messages). Whether the body actually needs rewriting — JSON, affected
// model, quirks present — is decided by normalizeModelQuirks on the bytes.
func rewriteEligible(r *http.Request) bool {
	if r.Method != http.MethodPost {
		return false
	}
	p := r.URL.Path
	if strings.Contains(p, "/api/anthropic/") {
		return strings.HasSuffix(p, "/messages")
	}
	return strings.HasSuffix(p, "/chat/completions")
}

// countingReader streams through while tallying bytes into a counter.
type countingReader struct {
	r     io.ReadCloser
	total *metrics.Counter
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if n > 0 {
		c.total.Add(int64(n))
	}
	return n, err
}

func (c *countingReader) Close() error { return c.r.Close() }
