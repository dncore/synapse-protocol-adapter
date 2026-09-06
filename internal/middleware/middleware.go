// Package middleware provides HTTP middlewares: panic recovery, request
// IDs, and structured access logging. Middlewares never log header values
// or bodies; Authorization and prompts stay out of logs by construction.
package middleware

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"time"
)

type ctxKey int

const requestIDKey ctxKey = 1

// RequestIDHeader is the header echoed back to clients so they can
// correlate requests with proxy logs.
const RequestIDHeader = "X-Request-Id"

// RequestID assigns a request ID (honoring an inbound X-Request-Id) and
// stores it in the context.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(RequestIDHeader)
		if id == "" || len(id) > 128 {
			var b [8]byte
			if _, err := rand.Read(b[:]); err != nil {
				id = time.Now().UTC().Format("20060102T150405.000000000")
			} else {
				id = "req_" + hex.EncodeToString(b[:])
			}
		}
		w.Header().Set(RequestIDHeader, id)
		next.ServeHTTP(w, r.WithContext(withRequestID(r.Context(), id)))
	})
}

// RequestIDOf returns the request ID of the context, if any.
func RequestIDOf(r *http.Request) string {
	if v, ok := r.Context().Value(requestIDKey).(string); ok {
		return v
	}
	return ""
}

func withRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}

// Recover converts panics in downstream handlers into 500 responses and
// logs the stack. A panic in one request must never kill the daemon.
func Recover(logger *slog.Logger, errorCounter func(string)) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					if rec == http.ErrAbortHandler {
						panic(rec) // net/http's own control-flow panic
					}
					logger.Error("panic recovered",
						"request_id", RequestIDOf(r),
						"method", r.Method,
						"path", r.URL.Path,
						"panic", rec,
					)
					errorCounter("panic")
					// The response may be unstarted; attempt a best-effort 500.
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusInternalServerError)
					_, _ = w.Write([]byte(`{"error":{"message":"internal proxy error","type":"proxy_error"}}`))
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// statusRecorder captures the response status for access logging.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (sr *statusRecorder) WriteHeader(code int) {
	sr.status = code
	sr.ResponseWriter.WriteHeader(code)
}

func (sr *statusRecorder) Write(b []byte) (int, error) {
	if sr.status == 0 {
		sr.status = http.StatusOK
	}
	n, err := sr.ResponseWriter.Write(b)
	sr.bytes += n
	return n, err
}

// Flush forwards flushes so SSE streaming works through the recorder.
func (sr *statusRecorder) Flush() {
	if f, ok := sr.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap supports http.NewResponseController.
func (sr *statusRecorder) Unwrap() http.ResponseWriter {
	return sr.ResponseWriter
}

// AccessLog logs one structured line per request. Extra fields can be
// attached by handlers via the returned request-scoped logger.
func AccessLog(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w}
			next.ServeHTTP(rec, r)
			if rec.status == 0 {
				rec.status = http.StatusOK
			}
			logger.Info("request",
				"request_id", RequestIDOf(r),
				"method", r.Method,
				"path", r.URL.Path,
				"status", rec.status,
				"bytes_out", rec.bytes,
				"latency_ms", time.Since(start).Milliseconds(),
			)
		})
	}
}
