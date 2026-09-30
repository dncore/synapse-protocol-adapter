package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/dncore/synapse-protocol-adapter/internal/anthropic"
	"github.com/dncore/synapse-protocol-adapter/internal/completions"
	"github.com/dncore/synapse-protocol-adapter/internal/config"
	"github.com/dncore/synapse-protocol-adapter/internal/converter"
	"github.com/dncore/synapse-protocol-adapter/internal/streaming"
	"github.com/dncore/synapse-protocol-adapter/internal/upstream"
)

// Anthropic-mount takeover.
//
// The shared gateway translates /api/anthropic itself, and for most requests
// that translation is fine — so with upstream.anthropic_mode = gpt6-tools this
// proxy takes over ONLY the requests that translator cannot serve: a gpt-6
// model on a tool-bearing turn. There the gateway's own route forwards
// max_tokens and thinking verbatim and never emits reasoning_effort, which the
// model rejects ("Function tools with reasoning_effort are not supported…").
//
// Everything else — other models, tool-free requests, count_tokens, other
// anthropic endpoints — keeps flowing through the upstream, byte for byte
// (see passthrough.go). The takeover reuses the same chat dialect as the
// /responses route, and falls back to passthrough whenever it meets a body
// shape it does not fully understand, so a client can never receive a
// silently lossy translation.

// anthropicTakeover decides whether this request is one the takeover serves.
// It returns the decoded request when it is, nil when passthrough must handle
// it. The decision is deliberately conservative: mode must be enabled, the
// path must be the messages endpoint, the model must be in the family whose
// translation is broken, and the request must carry tools (the failing shape).
func (s *Server) anthropicTakeover(path string, body []byte) *anthropic.Request {
	if s.cfg.Upstream.AnthropicMode != config.AnthropicModeGPT6Tools {
		return nil
	}
	if !isAnthropicMessagesPath(path) || len(body) == 0 {
		return nil
	}
	var req anthropic.Request
	if err := json.Unmarshal(body, &req); err != nil {
		s.metrics.AnthropicFallbackTotal.With("undecodable").Inc()
		return nil
	}
	if !converter.IsGPT6(req.Model) || len(req.Tools) == 0 {
		return nil // upstream translation handles these; do not interfere
	}
	return &req
}

// handleAnthropicTakeover converts, forwards to the chat route and translates
// the answer back. It reports false when the body was not served, telling the
// caller to fall back to passthrough (the client then sees exactly what it
// would have seen without the takeover).
func (s *Server) handleAnthropicTakeover(w http.ResponseWriter, r *http.Request, req *anthropic.Request, start time.Time) bool {
	chatReq, err := anthropic.ConvertRequest(req)
	if err != nil {
		s.metrics.AnthropicFallbackTotal.With(fallbackReason(err)).Inc()
		return false
	}
	chatBody, err := json.Marshal(chatReq)
	if err != nil {
		s.metrics.AnthropicFallbackTotal.With("internal").Inc()
		return false
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.Timeouts.Request)
	defer cancel()

	harness := &anthropicTakeoverHarness{s: s, r: r, req: req, start: start}
	upstream.ForwardResponseHeaders(w.Header(), nil)
	// The chat route authenticates with `Authorization: Bearer`, while
	// Anthropic clients may send only `x-api-key` (ANTHROPIC_API_KEY) — the
	// upstream's own Anthropic mount accepts both, so the takeover has to
	// translate. Without this, x-api-key clients get 401 here.
	headers := r.Header.Clone()
	if headers.Get("Authorization") == "" {
		if k := headers.Get("X-Api-Key"); k != "" {
			headers.Set("Authorization", "Bearer "+k)
		}
	}
	attempt := func() (*http.Response, error) {
		return s.client.Do(ctx, chatBody, headers, req.Stream)
	}
	resp, err := s.doUpstreamRetry(ctx, r, harness.logLabel(), true, attempt)
	if err != nil {
		s.metrics.UpstreamErrorsTotal.With(classifyUpstreamError(err)).Inc()
		s.metrics.ErrorsTotal.With("upstream").Inc()
		if r.Context().Err() != nil {
			return true // client gone; nothing to write
		}
		if errors.Is(err, context.DeadlineExceeded) {
			harness.writeError(w, http.StatusGatewayTimeout, "upstream request timed out")
			return true
		}
		harness.writeError(w, http.StatusBadGateway, "upstream request failed: "+err.Error())
		return true
	}
	defer resp.Body.Close()
	s.metrics.UpstreamConnections.Inc()

	if resp.StatusCode >= 400 {
		s.metrics.UpstreamErrorsTotal.With(fmt.Sprintf("http_%dxx", resp.StatusCode/100)).Inc()
		if resp.StatusCode == http.StatusTooManyRequests && s.cfg.UpstreamRetry.Enabled {
			// Same budget semantics as the other routes, but the body must be
			// an Anthropic error envelope: Claude Code parses this protocol.
			ra := resp.Header.Get("Retry-After")
			if ra == "" {
				ra = "1"
			}
			w.Header().Set("Retry-After", ra)
			harness.writeError(w, http.StatusTooManyRequests, upstreamErrorMessage(resp))
			return true
		}
		harness.forwardError(w, resp)
		return true
	}

	if req.Stream {
		s.metrics.StreamingRequests.Inc()
		harness.stream(w, r, ctx, cancel, resp)
	} else {
		harness.encoded(w, resp)
	}
	return true
}

// anthropicTakeoverHarness carries the per-request state of one takeover so
// the response helpers stay free of server plumbing.
type anthropicTakeoverHarness struct {
	s     *Server
	r     *http.Request
	req   *anthropic.Request
	start time.Time
}

// logLabel names the route for retry logging and metrics.
func (h *anthropicTakeoverHarness) logLabel() string { return "anthropic-takeover" }

// encoded writes the non-streamed answer as an Anthropic message.
func (h *anthropicTakeoverHarness) encoded(w http.ResponseWriter, resp *http.Response) {
	body, err := upstream.ReadAllWithLimit(resp.Body, int64(h.s.cfg.Limits.MaxBodyBytes))
	if err != nil {
		h.writeError(w, http.StatusBadGateway, "upstream response unreadable or too large")
		h.s.metrics.UpstreamErrorsTotal.With("body_read").Inc()
		return
	}
	var chat completions.Response
	if err := json.Unmarshal(body, &chat); err != nil {
		h.writeError(w, http.StatusBadGateway, "upstream returned malformed JSON")
		h.s.metrics.UpstreamErrorsTotal.With("malformed_json").Inc()
		return
	}
	out := anthropic.ConvertResponse(&chat, h.req.Model)
	outJSON, err := json.Marshal(out)
	if err != nil {
		h.writeError(w, http.StatusInternalServerError, "internal conversion error")
		return
	}
	upstream.ForwardResponseHeaders(w.Header(), resp.Header)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	n, _ := w.Write(outJSON)
	h.s.metrics.BytesOutTotal.Add(int64(n))
	h.s.metrics.ResponsesTotal.Inc()
	h.s.metrics.RequestDuration.Observe(time.Since(h.start).Seconds())
}

// stream pumps the chat SSE stream, emitting Anthropic events per chunk.
func (h *anthropicTakeoverHarness) stream(w http.ResponseWriter, r *http.Request, ctx context.Context, cancel context.CancelFunc, resp *http.Response) {
	rc := http.NewResponseController(w)
	defer func() { _ = rc.SetWriteDeadline(time.Time{}) }()

	sw := streaming.NewWriter(w)
	upstream.ForwardResponseHeaders(w.Header(), resp.Header)
	sw.Start()
	h.s.metrics.ResponsesTotal.Inc()

	reader := streaming.NewReader(resp.Body, h.s.cfg.Limits.MaxSSELine)
	streamer := anthropic.NewStreamer(h.req.Model)
	firstByte := true

	write := func(events []anthropic.Event) bool {
		for _, ev := range events {
			_ = rc.SetWriteDeadline(time.Now().Add(h.s.cfg.Timeouts.StreamWrite))
			if err := sw.WriteEvent(ev.Type, ev.Data); err != nil {
				cancel()
				h.s.metrics.ErrorsTotal.With("client_canceled").Inc()
				return false
			}
			if firstByte {
				h.s.metrics.FirstByteLatency.Observe(time.Since(h.start).Seconds())
				firstByte = false
			}
		}
		return true
	}

	// Intro event first: Claude Code expects the message shell before any
	// block, and it lets the client render immediately even though the
	// upstream has not sent its first chunk yet.
	if !write(streamer.Start("", h.req.Model)) {
		return
	}

	for {
		data, err := reader.Next()
		if err != nil {
			if errors.Is(err, streaming.ErrDone) || errors.Is(err, io.EOF) {
				break
			}
			if ctx.Err() != nil || r.Context().Err() != nil {
				h.s.metrics.ErrorsTotal.With("canceled").Inc()
				return
			}
			// Upstream cut the stream: close the message so the client does
			// not hang waiting for a terminal event.
			if !write(streamer.Finish()) {
				return
			}
			h.s.metrics.UpstreamErrorsTotal.With("stream_read").Inc()
			return
		}
		var chunk completions.StreamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue // keep-alive or non-JSON keepalive line
		}
		if !write(streamer.Chunk(&chunk)) {
			return
		}
	}
	if !write(streamer.Finish()) {
		return
	}
	h.s.metrics.RequestDuration.Observe(time.Since(h.start).Seconds())
}

// forwardError relays an upstream error body as an Anthropic error envelope,
// keeping the upstream's message text so operators can still read it.
func (h *anthropicTakeoverHarness) forwardError(w http.ResponseWriter, resp *http.Response) {
	// upstreamErrorMessage reads the body and always yields a usable string
	// (the upstream's own message, else the raw body, else the status text).
	message := upstreamErrorMessage(resp)
	status := resp.StatusCode
	if status < 400 || status > 599 {
		status = http.StatusBadGateway
	}
	h.writeError(w, status, message)
}

// writeError writes the Anthropic error envelope.
func (h *anthropicTakeoverHarness) writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(anthropic.ErrorResponse{
		Type:  "error",
		Error: anthropic.ErrorBody{Type: "api_error", Message: message},
	})
	h.s.metrics.ErrorsTotal.With("anthropic_takeover").Inc()
}

// fallbackReason labels why a takeover candidate was handed back to
// passthrough, so a real deployment can tell an unmapped client shape from an
// internal bug.
func fallbackReason(err error) string {
	if errors.Is(err, anthropic.ErrUnsupported) {
		return "unsupported_body"
	}
	return "internal"
}
