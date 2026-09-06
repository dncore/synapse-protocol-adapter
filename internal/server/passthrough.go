package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/dncore/synapse-protocol-adapter/internal/metrics"
	"github.com/dncore/synapse-protocol-adapter/internal/upstream"
)

// handlePassthrough transparently forwards any /v1/* request (other than
// /v1/responses, which the converter owns) to the configured upstream:
// same method, same path below /v1, same query, body streamed unmodified,
// response streamed byte-for-byte with a flush per read. Upstream errors
// pass through verbatim — this route never rewrites semantics.
//
// This makes the proxy a scoped reverse proxy for its single upstream
// (not an open proxy): everything under /v1/ reaches that provider and
// nothing else, which is exactly the local-forwarder role socat plays —
// with pooling, cancellation propagation, backpressure, and metrics that
// socat does not have.
func (s *Server) handlePassthrough(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	s.metrics.RequestsTotal.Inc()
	s.metrics.PassthroughRequestsTotal.Inc()
	s.metrics.ActiveRequests.Inc()
	defer s.metrics.ActiveRequests.Dec()

	release := s.acquireSlot(r)
	if release == nil {
		return // client went away while queued
	}
	defer release()

	// The request body is never parsed or buffered here; it streams to the
	// upstream with a counting wrapper for metrics.
	body := &countingReader{r: r.Body, total: s.metrics.BytesInTotal}

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.Timeouts.Request)
	defer cancel()

	upStart := time.Now()
	resp, err := s.client.DoPassthrough(ctx, r.Method,
		stripAPIPrefix(r.URL.Path), r.URL.RawQuery, body, r.Header)
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
