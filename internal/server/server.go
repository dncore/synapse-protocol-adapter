// Package server wires the HTTP mux, handlers, graceful shutdown, and
// health/readiness semantics. The proxy endpoint is POST /v1/responses;
// everything else is operational surface.
package server

import (
	"bufio"
	"context"
	"crypto/tls"
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
	"github.com/dncore/synapse-protocol-adapter/internal/tlscert"
	"github.com/dncore/synapse-protocol-adapter/internal/upstream"
	"github.com/dncore/synapse-protocol-adapter/internal/users"
)

// Version is stamped from main.version at startup (main gets the build
// version via -ldflags "-X main.version=...").
var Version = "dev"

// Server owns the HTTP server lifecycle.
type Server struct {
	cfg     config.Config
	logger  *slog.Logger
	metrics *metrics.Registry
	client  *upstream.Client
	ready   atomic.Bool
	sem     chan struct{}
	// userReg gates requests per API key; nil when users.enabled is false.
	userReg *users.Registry
	httpSrv *http.Server
	// tlsCert is the currently served certificate; swapped atomically on
	// SIGHUP so GetCertificate always hands out a consistent one.
	tlsCert atomic.Pointer[tls.Certificate]
	tlsCfg  *tls.Config
	// rawLn is the plain listener behind the dual-protocol accept loop;
	// closed at shutdown to release it (http.Server only tracks listeners
	// it accepted from itself).
	rawLn net.Listener
}

// New constructs the server. Call Run to start it.
func New(cfg config.Config, logger *slog.Logger, reg *metrics.Registry) *Server {
	var userReg *users.Registry
	if cfg.Users.Enabled {
		userReg = users.NewRegistry(&cfg.Users)
	}
	s := &Server{
		cfg:     cfg,
		logger:  logger,
		metrics: reg,
		client:  upstream.NewClient(cfg),
		sem:     newSemaphore(cfg.Limits.MaxConcurrency),
		userReg: userReg,
	}
	s.ready.Store(true)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /ready", s.handleReady)
	mux.HandleFunc("GET /metrics", s.handleMetrics)
	mux.HandleFunc("GET /version", s.handleVersion)
	mux.HandleFunc("GET /{$}", s.handleIndex)
	// Per-user queueing surface: live state and runtime limit changes
	// (non-persistent — the config file remains the source of truth).
	mux.HandleFunc("GET /users", s.handleUsersList)
	mux.HandleFunc("PUT /users/{id}", s.handleUserUpdate)
	// Clients configured in the provider's own path style hit /api/v1/*,
	// so both prefixes are served: /v1/responses and /api/v1/responses are
	// the same route, letting clients migrate by swapping the host only.
	if cfg.Upstream.ResponsesMode != "passthrough" {
		mux.HandleFunc("POST /v1/responses", s.handleResponses)
		mux.HandleFunc("POST /api/v1/responses", s.handleResponses)
	}
	// Everything else under the API prefixes transparently forwards to the
	// upstream (same method/path/query, streamed both ways). The more
	// specific /responses patterns win for the converted route.
	mux.HandleFunc("/v1/", s.handlePassthrough)
	mux.HandleFunc("/api/v1/", s.handlePassthrough)
	// Anthropic-protocol clients (ANTHROPIC_BASE_URL=http://host:port/api/anthropic)
	// hit /api/anthropic/*; gateways mount that protocol beside the
	// chat-completions base, so it forwards host-root-preserved.
	mux.HandleFunc("/api/anthropic/", s.handlePassthrough)
	// TLS bootstrap surface: the generated CA for download and a per-OS
	// import walkthrough. Read-only, registered only when TLS is on.
	if cfg.Server.TLS.Enabled {
		mux.HandleFunc("GET /ca.pem", s.handleCACert)
		mux.HandleFunc("GET /tls-help", s.handleTLSHelp)
	}

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
	// TLS material loads before the port opens so a certificate problem
	// fails startup outright instead of serving a broken listener.
	if s.cfg.Server.TLS.Enabled {
		mat, err := tlscert.Load(s.cfg.Server.TLS)
		if err != nil {
			return fmt.Errorf("server.tls: %w", err)
		}
		s.tlsCfg = s.makeTLSConfig()
		s.tlsCert.Store(&mat.Certificate)
		source := matSource(s.cfg.Server.TLS, mat)
		if mat.GeneratedCA {
			s.logger.Info("generated self-signed CA — import it into clients' trust stores (once)",
				"ca", mat.CACertPath,
				"help", "http(s)://<host>:<port>/tls-help walks every platform through it",
				"download", "GET /ca.pem",
				"curl", "--cacert "+mat.CACertPath,
				"node", "NODE_EXTRA_CA_CERTS="+mat.CACertPath,
				"python", "REQUESTS_CA_BUNDLE="+mat.CACertPath,
			)
		}
		s.logger.Info("tls enabled — listener auto-detects http and https per connection", "source", source)
	}

	ln, err := net.Listen("tcp", s.cfg.Server.Listen)
	if err != nil {
		return fmt.Errorf("listen %s: %w", s.cfg.Server.Listen, err)
	}
	s.rawLn = ln
	s.logger.Info("listening",
		"addr", s.cfg.Server.Listen,
		"tls", s.dualProto(),
		"upstream", s.cfg.Upstream.URL(),
		"max_concurrency", s.cfg.Limits.MaxConcurrency,
		"user_queue", s.cfg.Users.Enabled,
		"version", Version,
	)

	errCh := make(chan error, 1)
	go func() {
		// With TLS enabled the accept loop sniffs each connection's wire
		// protocol (plain Serve stays HTTP/1.1).
		if s.cfg.Server.TLS.Enabled {
			errCh <- s.serveDual(ln)
			return
		}
		errCh <- s.httpSrv.Serve(ln)
	}()

	sigCh := make(chan os.Signal, 3)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	if s.cfg.Server.TLS.Enabled {
		// SIGHUP reloads TLS material (re-signed leaf, new SANs) without
		// draining; `synapse tls add` sends it.
		signal.Notify(sigCh, syscall.SIGHUP)
	}
	for {
		shutdown := false
		select {
		case sig := <-sigCh:
			if sig == syscall.SIGHUP {
				s.reloadTLS()
				continue
			}
			s.logger.Info("shutdown signal received", "signal", sig.String())
			shutdown = true
		case <-ctx.Done():
			s.logger.Info("context canceled, shutting down")
			shutdown = true
		case err := <-errCh:
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		}
		if shutdown {
			return s.Shutdown()
		}
	}
}

// makeTLSConfig builds the per-connection TLS config. Certificates are
// served through GetCertificate so a SIGHUP can swap them without a
// restart. HTTP/2 is not offered: the dual-protocol path serves each
// connection via tls.Server, which bypasses ServeTLS's h2 setup —
// HTTP/1.1 on both protocols, which every client negotiates fine.
func (s *Server) makeTLSConfig() *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		NextProtos: []string{"http/1.1"},
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			if c := s.tlsCert.Load(); c != nil {
				return c, nil
			}
			return nil, errors.New("tls certificate not loaded")
		},
	}
}

// dualProto names the listener protocols for logs.
func (s *Server) dualProto() string {
	if s.cfg.Server.TLS.Enabled {
		return "http+https (auto per connection)"
	}
	return "false"
}

// serveDual accepts connections and serves each through the standard
// HTTP server after sniffing its wire protocol — one port, both
// protocols; plain-HTTP clients never see "Client sent an HTTP request
// to an HTTPS server" again.
func (s *Server) serveDual(ln net.Listener) error {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		go s.serveOneConn(conn)
	}
}

// serveOneConn peeks the first bytes: 0x16 0x03 is a TLS ClientHello
// (handshake, legacy version major 3); anything else is HTTP.
func (s *Server) serveOneConn(conn net.Conn) {
	// Bound the peek so a silent half-open connection cannot park here
	// forever; the HTTP server applies its own deadlines afterwards.
	_ = conn.SetReadDeadline(time.Now().Add(s.cfg.Timeouts.HeaderWrite))
	br := bufio.NewReaderSize(conn, 4096)
	head, err := br.Peek(2)
	_ = conn.SetReadDeadline(time.Time{})
	if err != nil {
		_ = conn.Close()
		return
	}

	c := net.Conn(&sniffedConn{Conn: conn, r: br})
	if head[0] == 0x16 && head[1] == 0x03 {
		c = tls.Server(c, s.tlsCfg)
	}
	_ = s.httpSrv.Serve(&oneShotListener{conn: c})
}

// sniffedConn replays the peeked bytes on the first Read.
type sniffedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *sniffedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// oneShotListener yields exactly one connection, then reports closed —
// enough for http.Server.Serve to run its full per-connection loop
// (keep-alive, timeouts, ConnState metrics) on it.
type oneShotListener struct {
	conn net.Conn
	done bool
}

func (l *oneShotListener) Accept() (net.Conn, error) {
	if l.done {
		return nil, net.ErrClosed
	}
	l.done = true
	return l.conn, nil
}
func (l *oneShotListener) Close() error   { return nil }
func (l *oneShotListener) Addr() net.Addr { return l.conn.RemoteAddr() }

// reloadTLS re-reads the TLS material (which re-signs the leaf when SANs
// or host addresses drifted) and swaps the served certificate atomically.
// In-flight connections keep the certificate they negotiated.
func (s *Server) reloadTLS() {
	mat, err := tlscert.Load(s.cfg.Server.TLS)
	if err != nil {
		s.logger.Error("tls reload failed, keeping current certificate", "err", err.Error())
		return
	}
	s.tlsCert.Store(&mat.Certificate)
	s.logger.Info("tls certificate reloaded", "source", matSource(s.cfg.Server.TLS, mat))
}

// matSource names the certificate provenance for logs.
func matSource(cfg config.TLS, mat tlscert.Material) string {
	if cfg.CertFile != "" {
		return "cert " + cfg.CertFile
	}
	return "auto (" + mat.CACertPath + ")"
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
	// Release the dual-protocol accept loop: http.Server only closes
	// listeners it accepted from itself, and this one is ours.
	if s.rawLn != nil {
		_ = s.rawLn.Close()
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
	// The queue-depth gauge lives in the users registry (waiters are
	// counted where they enqueue/leave); expose it in the same scrape.
	if s.userReg != nil {
		fmt.Fprintf(&sb, "# HELP protocol_proxy_queue_depth Requests currently waiting in per-user concurrency queues.\n"+
			"# TYPE protocol_proxy_queue_depth gauge\nprotocol_proxy_queue_depth %d\n", s.userReg.Depth())
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, sb.String())
}

func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"version": Version})
}

// handleUsersList reports the per-user queueing state: configuration
// summary plus each user's live slots, queue, and counters. Like the
// other operational endpoints it is unauthenticated — see README
// (Security) for the trusted-network model.
func (s *Server) handleUsersList(w http.ResponseWriter, r *http.Request) {
	if s.userReg == nil {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":             true,
		"default_concurrency": s.cfg.Users.DefaultConcurrency,
		"max_queue":           s.cfg.Users.MaxQueue,
		"queue_timeout":       s.cfg.Users.QueueTimeout.String(),
		"queue_depth":         s.userReg.Depth(),
		"users":               s.userReg.All(),
	})
}

// handleUserUpdate changes one user's concurrency limit live (non-
// persistent: a restart restores the config file's value). {id} is the
// hash prefix shown by GET /users or the configured name.
func (s *Server) handleUserUpdate(w http.ResponseWriter, r *http.Request) {
	if s.userReg == nil {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "per-user queueing is disabled (users.enabled is false)",
		})
		return
	}
	u := s.userReg.Find(r.PathValue("id"))
	if u == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown user"})
		return
	}
	var body struct {
		Concurrency int `json:"concurrency"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "body must be JSON: {\"concurrency\": N}"})
		return
	}
	if body.Concurrency < 1 || body.Concurrency > 1_000_000 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "concurrency must be between 1 and 1000000"})
		return
	}
	u.SetLimit(body.Concurrency)
	s.logger.Info("user concurrency updated",
		"request_id", middleware.RequestIDOf(r),
		"name", u.Snapshot().Name,
		"concurrency", body.Concurrency,
	)
	writeJSON(w, http.StatusOK, u.Snapshot())
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"service": "synapse",
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

// newSemaphore returns a counting channel of size n, or nil when n <= 0
// (unlimited — acquireSlot becomes a no-op).
func newSemaphore(n int) chan struct{} {
	if n <= 0 {
		return nil
	}
	return make(chan struct{}, n)
}

// acquireSlot bounds concurrency with a semaphore; blocking acquire
// applies backpressure and honors client cancellation while queued. It
// returns nil when the client went away before a slot freed up.
func (s *Server) acquireSlot(r *http.Request) func() {
	if s.sem == nil { // unlimited
		return func() {}
	}
	select {
	case s.sem <- struct{}{}:
		return func() { <-s.sem }
	case <-r.Context().Done():
		s.metrics.ErrorsTotal.With("client_canceled").Inc()
		return nil
	}
}

// enterUser gates a request through its API key's per-user concurrency
// queue. It runs BEFORE the global semaphore so requests waiting in a
// user queue hold no global slot (one user's backlog cannot starve the
// others). Returns the release func, or nil when the request was
// answered already (429) or the client went away while queued.
func (s *Server) enterUser(w http.ResponseWriter, r *http.Request) func() {
	if s.userReg == nil {
		return func() {} // feature disabled
	}
	u := s.userReg.Identify(r.Header)
	waited, err := u.Acquire(r.Context())
	if err != nil {
		switch err {
		case users.ErrQueueFull:
			s.rejectQueued(w, "queue_full",
				"user concurrency queue is full; retry after in-flight requests finish")
		case users.ErrQueueTimeout:
			s.rejectQueued(w, "queue_timeout",
				"request waited past users.queue_timeout in the concurrency queue")
		default: // client canceled while queued
			s.metrics.ErrorsTotal.With("client_canceled").Inc()
		}
		return nil
	}
	if waited > 0 {
		s.metrics.QueuedRequestsTotal.Inc()
		s.metrics.QueueWait.Observe(waited.Seconds())
	}
	return u.Release
}

// rejectQueued answers a queue rejection with 429 + Retry-After.
func (s *Server) rejectQueued(w http.ResponseWriter, reason, msg string) {
	s.metrics.QueueRejectedTotal.With(reason).Inc()
	w.Header().Set("Retry-After", "1")
	s.writeProxyError(w, http.StatusTooManyRequests, msg)
}

// handleResponses is the whole protocol conversion pipeline.
func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	s.metrics.RequestsTotal.Inc()
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

		// In-band rejection (HTTP 200 + {"error":...} chunk): a gateway
		// refusing the model or quota. Fail loudly instead of completing
		// an empty response.
		if chunk.Error != nil && chunk.Error.Message != "" {
			s.metrics.UpstreamErrorsTotal.With("stream_in_band_error").Inc()
			s.logger.Warn("upstream rejected request in-band",
				"request_id", middleware.RequestIDOf(r),
				"error_type", chunk.Error.Type, "code", chunk.Error.Code)
			_ = sw.WriteEvent("error", map[string]any{
				"type": "error",
				"error": map[string]any{
					"code":    orDefault(chunk.Error.Code, "upstream_error"),
					"message": chunk.Error.Message,
					"type":    orDefault(chunk.Error.Type, "upstream_error"),
				},
			})
			return
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
	case status == http.StatusTooManyRequests:
		errType = "rate_limit_error"
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
