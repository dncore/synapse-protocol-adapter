package streaming

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// Writer emits Responses API events as SSE with immediate flush after
// every event, minimizing first-chunk and per-chunk latency.
type Writer struct {
	w       http.ResponseWriter
	flusher http.Flusher
}

// NewWriter prepares an SSE response. It writes the response headers and
// the initial retry hint; the caller must have set the status code (200)
// before the first event is written.
func NewWriter(w http.ResponseWriter) *Writer {
	flusher, _ := w.(http.Flusher)
	sw := &Writer{w: w, flusher: flusher}
	return sw
}

// Start sends the SSE content type and headers. Call exactly once, before
// the first event, and only when the response will succeed.
func (sw *Writer) Start() {
	h := sw.w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	sw.w.WriteHeader(http.StatusOK)
	sw.Flush()
}

// WriteEvent serializes one event payload (already a Responses event
// object) as `event: <type>\ndata: <json>\n\n` and flushes.
func (sw *Writer) WriteEvent(eventType string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal sse event: %w", err)
	}
	if _, err := fmt.Fprintf(sw.w, "event: %s\ndata: %s\n\n", eventType, data); err != nil {
		return err
	}
	sw.Flush()
	return nil
}

// Flush pushes buffered bytes to the client. It is a no-op when the
// ResponseWriter does not support flushing (tests).
func (sw *Writer) Flush() {
	if sw.flusher != nil {
		sw.flusher.Flush()
	}
}
