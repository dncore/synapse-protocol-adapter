package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/dncore/synapse-protocol-adapter/internal/metrics"
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

	// Small bodies (declared Content-Length within the retry buffer) are
	// read into memory so a transient 429 can be retried with the same
	// bytes; larger or length-less bodies stream straight through, exactly
	// as before, and are not retried.
	buffered, err := s.replayableBody(r)
	if err != nil {
		s.metrics.ErrorsTotal.With("client_canceled").Inc()
		return // client aborted while sending its body
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
	resp, err := s.doUpstreamRetry(ctx, r, "passthrough", buffered != nil, attempt)
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

// stripAPIPrefix removes the client-facing API prefix (/v1 or /api/v1) so
// the remainder routes onto the configured base_url, whichever path
// convention the client was configured with.
func stripAPIPrefix(p string) string {
	if strings.HasPrefix(p, "/api/v1") {
		return strings.TrimPrefix(p, "/api/v1")
	}
	return strings.TrimPrefix(p, "/v1")
}

// replayableBody returns the request body verbatim when it is small enough
// and its length is known (Content-Length within buffer_max_bytes), so the
// passthrough route can re-send it after an upstream 429. When it returns
// nil the caller streams r.Body as before and the request is not retried.
func (s *Server) replayableBody(r *http.Request) ([]byte, error) {
	rc := s.cfg.UpstreamRetry
	if !rc.Enabled || rc.BufferMaxBytes <= 0 || r.Body == nil ||
		r.ContentLength < 0 || r.ContentLength > int64(rc.BufferMaxBytes) {
		return nil, nil
	}
	buf := make([]byte, r.ContentLength)
	if _, err := io.ReadFull(r.Body, buf); err != nil {
		return nil, err
	}
	s.metrics.BytesInTotal.Add(r.ContentLength)
	return buf, nil
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
