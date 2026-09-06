// Package server wires the HTTP mux, handlers, graceful shutdown, and
// health/readiness semantics. The proxy endpoint is POST /v1/responses;
// everything else is operational surface.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/dncore/synapse-protocol-adapter/internal/completions"
	"github.com/dncore/synapse-protocol-adapter/internal/config"
	"github.com/dncore/synapse-protocol-adapter/internal/converter"
	"github.com/dncore/synapse-protocol-adapter/internal/metrics"
	"github.com/dncore/synapse-protocol-adapter/internal/middleware"
	"github.com/dncore/synapse-protocol-adapter/internal/responses"
	"github.com/dncore/synapse-protocol-adapter/internal/streaming"
	"github.com/dncore/synapse-protocol-adapter/internal/upstream"
)

// Version is set at build time via -ldflags "-X ...server.Version=...".
var Version = "dev"

// Server owns the HTTP server lifecycle.
type Server struct {
	cfg     config.Config
	logger  *slog.Logger
	metrics *metrics.Registry
	client  *upstream.Client
	ready   atomic.Bool
	sem     chan struct{}
	httpSrv *http.Server
}

// New constructs the server. Call Run to start it.
func New(cfg config.Config, logger *slog.Logger, reg *metrics.Registry) *Server {
	s := &Server{
		cfg:     cfg,
		logger:  logger,
		metrics: reg,
		client:  upstream.NewClient(cfg),
		sem:     make(chan struct{}, cfg.Limits.MaxConcurrency),
	}
	s.ready.Store(true)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /ready", s.handleReady)
	mux.HandleFunc("GET /metrics", s.handleMetrics)
	mux.HandleFunc("GET /version", s.handleVersion)
	mux.HandleFunc("GET /{$}", s.handleIndex)
	// In passthrough mode the upstream's native /responses endpoint wins:
	// the /v1/ catch-all forwards it untouched instead of converting.
	if cfg.Upstream.ResponsesMode != "passthrough" {
		mux.HandleFunc("POST /v1/responses", s.handleResponses)
	}
	// Everything else under /v1/ transparently forwards to the upstream
	// (same method/path/query, streamed both ways): /v1/chat/completions,
	// /v1/models, ... The more specific /v1/responses pattern wins for the
	// converted route.
	mux.HandleFunc("/v1/", s.handlePassthrough)

	// Middleware order (outermost first): RequestID assigns the ID so every
	// inner layer (including logs and panics) can reference it; AccessLog
	// wraps Recover so handler panics are logged within the access entry.
	handler := middleware.Recover(logger, func(t string) { s.metrics.ErrorsTotal.With(t).Inc() })(mux)
	handler = middleware.AccessLog(logger)(handler)
	handler = middleware.RequestID(handler)

	s.httpSrv = &http.Server{
		Handler: handler,
		// ReadTimeout/WriteTimeout are deliberately unset: streaming LLM
		// responses are long-lived by design. Per-request bounds come from
		// timeouts.request (context) and ResponseController write deadlines.
		ReadHeaderTimeout: cfg.Timeouts.HeaderWrite,
		IdleTimeout:       cfg.Timeouts.Idle,
		MaxHeaderBytes:    1 << 20,
		ConnState:         s.trackConns,
	}
	return s
}

// trackConns maintains the active_connections gauge across connection
// lifecycle transitions.
func (s *Server) trackConns(conn net.Conn, state http.ConnState) {
	switch state {
	case http.StateNew:
		s.metrics.ActiveConnections.Inc()
	case http.StateClosed, http.StateHijacked:
		s.metrics.ActiveConnections.Dec()
	}
}

// Run listens, serves, and blocks until SIGTERM/SIGINT (or ctx done), then
// drains gracefully.
func (s *Server) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.cfg.Server.Listen)
	if err != nil {
		return fmt.Errorf("listen %s: %w", s.cfg.Server.Listen, err)
	}
	s.logger.Info("listening",
		"addr", s.cfg.Server.Listen,
		"upstream", s.cfg.Upstream.URL(),
		"max_concurrency", s.cfg.Limits.MaxConcurrency,
		"version", Version,
	)

	errCh := make(chan error, 1)
	go func() { errCh <- s.httpSrv.Serve(ln) }()

	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	select {
	case sig := <-sigCh:
		s.logger.Info("shutdown signal received", "signal", sig.String())
	case <-ctx.Done():
		s.logger.Info("context canceled, shutting down")
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
	return s.Shutdown()
}

// Shutdown flips ready to false, stops accepting connections, waits for
// in-flight requests (including streams) up to the drain timeout, then
// force-closes and drops upstream idle connections.
func (s *Server) Shutdown() error {
	s.ready.Store(false)

	drainCtx, cancel := context.WithTimeout(context.Background(), s.cfg.Shutdown.Timeout)
	defer cancel()

	if err := s.httpSrv.Shutdown(drainCtx); err != nil {
		s.logger.Warn("graceful shutdown deadline exceeded, forcing close", "err", err.Error())
		_ = s.httpSrv.Close()
	}
	s.client.CloseIdleConnections()
	s.logger.Info("shutdown complete",
		"active_connections", s.metrics.ActiveConnections.Value())
	return nil
}

// --- operational handlers ---

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	if !s.ready.Load() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "shutting_down"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	var sb strings.Builder
	s.metrics.WriteText(&sb)
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, sb.String())
}

func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"version": Version})
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"service": "protocol-proxy",
		"version": Version,
		"endpoints": []string{
			"POST /v1/responses",
			"GET /health",
			"GET /ready",
			"GET /metrics",
			"GET /version",
		},
	})
}

// --- the proxy endpoint ---

// acquireSlot bounds concurrency with a semaphore; blocking acquire
// applies backpressure and honors client cancellation while queued. It
// returns nil when the client went away before a slot freed up.
func (s *Server) acquireSlot(r *http.Request) func() {
	select {
	case s.sem <- struct{}{}:
		return func() { <-s.sem }
	case <-r.Context().Done():
		s.metrics.ErrorsTotal.With("client_canceled").Inc()
		return nil
	}
}

// handleResponses is the whole protocol conversion pipeline.
func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	s.metrics.RequestsTotal.Inc()
	s.metrics.ActiveRequests.Inc()
	defer s.metrics.ActiveRequests.Dec()

	release := s.acquireSlot(r)
	if release == nil {
		return // client went away while queued
	}
	defer release()

	body, err := upstream.ReadAllWithLimit(r.Body, int64(s.cfg.Limits.MaxBodyBytes))
	if err != nil {
		s.writeProxyError(w, http.StatusRequestEntityTooLarge, "request body exceeds proxy limit")
		s.metrics.ErrorsTotal.With("body_too_large").Inc()
		return
	}
	s.metrics.BytesInTotal.Add(int64(len(body)))

	var req responses.Request
	if err := json.Unmarshal(body, &req); err != nil {
		s.writeProxyError(w, http.StatusBadRequest, "invalid Responses API request JSON: "+err.Error())
		s.metrics.ErrorsTotal.With("bad_request").Inc()
		return
	}

	chatReq, err := converter.ConvertRequest(&req)
	if err != nil {
		s.writeProxyError(w, http.StatusBadRequest, err.Error())
		s.metrics.ErrorsTotal.With("bad_request").Inc()
		return
	}
	chatBody, err := json.Marshal(chatReq)
	if err != nil {
		s.writeProxyError(w, http.StatusInternalServerError, "internal conversion error")
		s.metrics.ErrorsTotal.With("internal").Inc()
		return
	}

	// Per-request lifetime. This context is the cancellation pipeline:
	// client disconnect (r.Context) or request timeout aborts the upstream
	// call; a mid-stream client write failure cancels it explicitly.
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.Timeouts.Request)
	defer cancel()

	upStart := time.Now()
	resp, err := s.client.Do(ctx, chatBody, r.Header, req.Stream)
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

	if resp.StatusCode >= 400 {
		s.metrics.UpstreamErrorsTotal.With(fmt.Sprintf("http_%dxx", resp.StatusCode/100)).Inc()
		s.forwardUpstreamError(w, resp)
		return
	}

	if req.Stream {
		s.metrics.StreamingRequests.Inc()
		s.streamResponse(w, r, ctx, cancel, resp, &req, start)
	} else {
		s.encodedResponse(w, resp, &req, start)
	}
}

// encodedResponse handles the non-streaming path.
func (s *Server) encodedResponse(w http.ResponseWriter, resp *http.Response, req *responses.Request, start time.Time) {
	body, err := upstream.ReadAllWithLimit(resp.Body, int64(s.cfg.Limits.MaxBodyBytes))
	if err != nil {
		s.writeProxyError(w, http.StatusBadGateway, "upstream response unreadable or too large")
		s.metrics.UpstreamErrorsTotal.With("body_read").Inc()
		return
	}

	var chat completions.Response
	if err := json.Unmarshal(body, &chat); err != nil {
		s.writeProxyError(w, http.StatusBadGateway, "upstream returned malformed JSON")
		s.metrics.UpstreamErrorsTotal.With("malformed_json").Inc()
		return
	}

	out := converter.ConvertResponse(&chat, req)
	outJSON, err := json.Marshal(out)
	if err != nil {
		s.writeProxyError(w, http.StatusInternalServerError, "internal conversion error")
		return
	}

	upstream.ForwardResponseHeaders(w.Header(), resp.Header)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	n, _ := w.Write(outJSON)
	s.metrics.BytesOutTotal.Add(int64(n))
	s.observeToolCalls(out.Output)
	s.metrics.ResponsesTotal.Inc()
	s.metrics.RequestDuration.Observe(time.Since(start).Seconds())
}

// streamResponse pumps the upstream SSE stream, converting each chunk and
// flushing per event. Backpressure is implicit: a blocked write to the
// client stops this loop reading upstream, propagating TCP backpressure
// to the provider without buffering.
func (s *Server) streamResponse(w http.ResponseWriter, r *http.Request, ctx context.Context, cancel context.CancelFunc, resp *http.Response, req *responses.Request, start time.Time) {
	rc := http.NewResponseController(w)
	defer func() { _ = rc.SetWriteDeadline(time.Time{}) }()

	upstream.ForwardResponseHeaders(w.Header(), resp.Header)
	sw := streaming.NewWriter(w)
	sw.Start()
	s.metrics.ResponsesTotal.Inc()

	reader := streaming.NewReader(resp.Body, s.cfg.Limits.MaxSSELine)
	streamer := converter.NewStreamer(req)
	firstByte := true

	writeEvent := func(ev *responses.Event) bool {
		// Per-write deadline: stream_write bounds client stall time, not
		// total stream duration — a healthy 30-minute generation must not
		// trip it.
		_ = rc.SetWriteDeadline(time.Now().Add(s.cfg.Timeouts.StreamWrite))
		if err := sw.WriteEvent(ev.Type, ev); err != nil {
			// Client vanished or write deadline hit: abort upstream now.
			cancel()
			s.metrics.ErrorsTotal.With("client_canceled").Inc()
			return false
		}
		if firstByte {
			s.metrics.FirstByteLatency.Observe(time.Since(start).Seconds())
			firstByte = false
		}
		return true
	}

	for {
		data, err := reader.Next()
		if err != nil {
			if errors.Is(err, streaming.ErrDone) || errors.Is(err, io.EOF) {
				break // normal end ([DONE]) or upstream closed cleanly
			}
			if ctx.Err() != nil || r.Context().Err() != nil {
				s.metrics.ErrorsTotal.With("canceled").Inc()
				return
			}
			s.metrics.UpstreamErrorsTotal.With("stream_broken").Inc()
			// SSE headers are already sent; signal failure in-band.
			_ = sw.WriteEvent("error", map[string]any{
				"type":  "error",
				"error": map[string]any{"code": "upstream_error", "message": "upstream stream terminated unexpectedly: " + err.Error()},
			})
			return
		}
		if data == "" {
			continue
		}

		var chunk completions.StreamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			// Skip keep-alive noise rather than killing a live stream.
			s.logger.Warn("skipping malformed upstream chunk",
				"request_id", middleware.RequestIDOf(r), "bytes", len(data))
			continue
		}

		for _, ev := range streamer.Feed(&chunk) {
			if !writeEvent(ev) {
				return
			}
		}
	}

	for _, ev := range streamer.Finish() {
		if !writeEvent(ev) {
			return
		}
	}
	if final := streamer.FinalResponse(); final != nil {
		s.observeToolCalls(final.Output)
	}
	s.metrics.RequestDuration.Observe(time.Since(start).Seconds())
}

// forwardUpstreamError relays an HTTP-level upstream error to the client,
// reshaping the body into a Responses-style error while preserving status.
func (s *Server) forwardUpstreamError(w http.ResponseWriter, resp *http.Response) {
	body, _ := upstream.ReadAllWithLimit(resp.Body, 1<<20)

	var parsed completions.Error
	msg := ""
	if err := json.Unmarshal(body, &parsed); err == nil && parsed.Error.Message != "" {
		msg = parsed.Error.Message
	}
	if msg == "" {
		// FastAPI-style error envelope: {"detail":"..."}.
		var detail struct {
			Detail string `json:"detail"`
		}
		if err := json.Unmarshal(body, &detail); err == nil && detail.Detail != "" {
			msg = detail.Detail
		}
	}
	if msg == "" {
		msg = strings.TrimSpace(string(body))
	}
	if msg == "" {
		msg = http.StatusText(resp.StatusCode)
	}

	out := map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    orDefault(parsed.Error.Type, "upstream_error"),
			"code":    orDefault(parsed.Error.Code, strconv.Itoa(resp.StatusCode)),
		},
	}
	upstream.ForwardResponseHeaders(w.Header(), resp.Header)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_ = json.NewEncoder(w).Encode(out)
}

// writeProxyError emits a Responses-shaped error produced by the proxy
// itself, with OpenAI-conventional error type values.
func (s *Server) writeProxyError(w http.ResponseWriter, status int, msg string) {
	errType := "server_error"
	switch {
	case status >= 400 && status < 500:
		errType = "invalid_request_error"
	case status == http.StatusBadGateway || status == http.StatusGatewayTimeout:
		errType = "upstream_error"
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    errType,
			"code":    strconv.Itoa(status),
		},
	})
}

func (s *Server) observeToolCalls(items []responses.Item) {
	n := 0
	for _, it := range items {
		if it.Type == "function_call" {
			n++
		}
	}
	if n > 0 {
		s.metrics.ToolCallsTotal.Add(int64(n))
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// classifyUpstreamError buckets transport failures into low-cardinality
// metric labels.
func classifyUpstreamError(err error) string {
	var netErr net.Error
	switch {
	case errors.As(err, &netErr) && netErr.Timeout():
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	default:
		return "transport"
	}
}
