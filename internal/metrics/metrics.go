// Package metrics implements a minimal, allocation-free Prometheus text
// exposition registry. Counters and histogram buckets are atomically
// incremented; gauges are atomically loaded/stored. No third-party
// dependency, which keeps the runtime image empty of protobuf baggage.
package metrics

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// Registry holds all metrics of the proxy.
type Registry struct {
	// Counters (monotonic).
	RequestsTotal        *Counter
	ResponsesTotal       *Counter
	ErrorsTotal          *LabeledCounter
	UpstreamErrorsTotal  *LabeledCounter
	StreamingRequests    *Counter
	ToolCallsTotal       *Counter
	BytesInTotal         *Counter
	BytesOutTotal        *Counter
	UpstreamConnections  *Counter

	// Gauges.
	ActiveConnections *Gauge
	ActiveRequests    *Gauge

	// Histograms (seconds).
	RequestDuration    *Histogram
	UpstreamDuration   *Histogram
	FirstByteLatency   *Histogram
}

var defaultBuckets = []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300}

// NewRegistry constructs the proxy's metric set.
func NewRegistry() *Registry {
	return &Registry{
		RequestsTotal:       &Counter{Name: "protocol_proxy_requests_total", Help: "Total requests received on proxy endpoints."},
		ResponsesTotal:      &Counter{Name: "protocol_proxy_responses_total", Help: "Total responses sent to clients (non-error)."},
		ErrorsTotal:         &LabeledCounter{Name: "protocol_proxy_errors_total", Help: "Total errors by type.", Label: "type"},
		UpstreamErrorsTotal: &LabeledCounter{Name: "protocol_proxy_upstream_errors_total", Help: "Total upstream request failures by class.", Label: "class"},
		StreamingRequests:   &Counter{Name: "protocol_proxy_streaming_requests_total", Help: "Total streaming (SSE) responses started."},
		ToolCallsTotal:      &Counter{Name: "protocol_proxy_tool_calls_total", Help: "Total tool calls observed in upstream output."},
		BytesInTotal:        &Counter{Name: "protocol_proxy_bytes_in_total", Help: "Bytes received from clients (request bodies)."},
		BytesOutTotal:       &Counter{Name: "protocol_proxy_bytes_out_total", Help: "Bytes sent to clients (response bodies)."},
		UpstreamConnections: &Counter{Name: "protocol_proxy_upstream_requests_total", Help: "Total requests issued to upstream."},

		ActiveConnections: &Gauge{Name: "protocol_proxy_active_connections", Help: "Currently open client connections."},
		ActiveRequests:    &Gauge{Name: "protocol_proxy_active_requests", Help: "Requests currently being proxied."},

		RequestDuration:  NewHistogram("protocol_proxy_request_duration_seconds", "End-to-end request duration.", defaultBuckets),
		UpstreamDuration: NewHistogram("protocol_proxy_upstream_duration_seconds", "Upstream request duration (until response headers).", defaultBuckets),
		FirstByteLatency: NewHistogram("protocol_proxy_first_byte_latency_seconds", "Latency from request start to first SSE event flushed to client.", defaultBuckets),
	}
}

// Counter is a monotonically increasing metric.
type Counter struct {
	Name string
	Help string
	v    atomic.Int64
}

// Inc adds one.
func (c *Counter) Inc() { c.v.Add(1) }

// Add accumulates n (must be >= 0).
func (c *Counter) Add(n int64) {
	if n > 0 {
		c.v.Add(n)
	}
}

// Value returns the current count.
func (c *Counter) Value() int64 { return c.v.Load() }

// LabeledCounter is a counter with one bounded-cardinality string label.
// Label values must be a small fixed set (e.g. error class names); the
// registry format output would otherwise grow without bound.
type LabeledCounter struct {
	Name  string
	Help  string
	Label string

	mu     sync.Mutex
	values map[string]*Counter
}

// With binds a label value, registering it on first use. Registration is
// mutex-guarded; subsequent increments hit only the atomic Counter.
func (lc *LabeledCounter) With(value string) *Counter {
	lc.mu.Lock()
	defer lc.mu.Unlock()
	if c, ok := lc.values[value]; ok {
		return c
	}
	if lc.values == nil {
		lc.values = make(map[string]*Counter)
	}
	c := &Counter{Name: lc.Name, Help: lc.Help}
	lc.values[value] = c
	return c
}

// Gauge is a point-in-time value.
type Gauge struct {
	Name string
	Help string
	v    atomic.Int64
}

// Inc adds one.
func (g *Gauge) Inc() { g.v.Add(1) }

// Dec subtracts one.
func (g *Gauge) Dec() { g.v.Add(-1) }

// Value returns the current value.
func (g *Gauge) Value() int64 { return g.v.Load() }

// Histogram counts observations into fixed upper-bound buckets.
type Histogram struct {
	Name    string
	Help    string
	bounds []float64
	counts []atomic.Int64
	sumN   atomic.Int64 // observation count
	sumV   atomic.Int64 // sum in nanoseconds to avoid float atomics
}

// NewHistogram builds a histogram with the given (sorted) upper bounds.
func NewHistogram(name, help string, bounds []float64) *Histogram {
	return &Histogram{
		Name:   name,
		Help:   help,
		bounds: append([]float64(nil), bounds...),
		counts: make([]atomic.Int64, len(bounds)+1),
	}
}

// Observe records one duration in seconds.
func (h *Histogram) Observe(seconds float64) {
	ns := int64(math.Round(seconds * 1e9))
	h.sumN.Add(1)
	h.sumV.Add(ns)
	for i, b := range h.bounds {
		if seconds <= b {
			h.counts[i].Add(1)
			return
		}
	}
	h.counts[len(h.bounds)].Add(1)
}

// WriteText renders the registry in Prometheus text exposition format.
func (r *Registry) WriteText(sb *strings.Builder) {
	writeCounter := func(c *Counter, extra string) {
		fmt.Fprintf(sb, "# HELP %s %s\n# TYPE %s counter\n%s%s %d\n", c.Name, c.Help, c.Name, c.Name, extra, c.v.Load())
	}

	writeCounter(r.RequestsTotal, "")
	writeCounter(r.ResponsesTotal, "")
	writeCounter(r.StreamingRequests, "")
	writeCounter(r.ToolCallsTotal, "")
	writeCounter(r.BytesInTotal, "")
	writeCounter(r.BytesOutTotal, "")
	writeCounter(r.UpstreamConnections, "")

	writeLabeled := func(lc *LabeledCounter) {
		fmt.Fprintf(sb, "# HELP %s %s\n# TYPE %s counter\n", lc.Name, lc.Help, lc.Name)
		lc.mu.Lock()
		keys := make([]string, 0, len(lc.values))
		for k := range lc.values {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		snapshot := make(map[string]int64, len(keys))
		for _, k := range keys {
			snapshot[k] = lc.values[k].Value()
		}
		lc.mu.Unlock()
		for _, k := range keys {
			fmt.Fprintf(sb, "%s{%s=%q} %d\n", lc.Name, lc.Label, k, snapshot[k])
		}
	}
	writeLabeled(r.ErrorsTotal)
	writeLabeled(r.UpstreamErrorsTotal)

	writeGauge := func(g *Gauge) {
		fmt.Fprintf(sb, "# HELP %s %s\n# TYPE %s gauge\n%s %d\n", g.Name, g.Help, g.Name, g.Name, g.v.Load())
	}
	writeGauge(r.ActiveConnections)
	writeGauge(r.ActiveRequests)

	writeHistogram := func(h *Histogram) {
		fmt.Fprintf(sb, "# HELP %s %s\n# TYPE %s histogram\n", h.Name, h.Help, h.Name)
		cumulative := int64(0)
		for i, b := range h.bounds {
			cumulative += h.counts[i].Load()
			fmt.Fprintf(sb, "%s_bucket{le=\"%v\"} %d\n", h.Name, b, cumulative)
		}
		cumulative += h.counts[len(h.bounds)].Load()
		fmt.Fprintf(sb, "%s_bucket{le=\"+Inf\"} %d\n", h.Name, cumulative)
		fmt.Fprintf(sb, "%s_sum %s\n", h.Name, formatFloat(float64(h.sumV.Load())/1e9))
		fmt.Fprintf(sb, "%s_count %d\n", h.Name, h.sumN.Load())
	}
	writeHistogram(r.RequestDuration)
	writeHistogram(r.UpstreamDuration)
	writeHistogram(r.FirstByteLatency)
}

func formatFloat(f float64) string {
	// Render with enough precision for seconds values; avoid scientific
	// notation which Prometheus text format allows but some scrapers choke on.
	return strings.TrimSuffix(fmt.Sprintf("%.9f", f), "00000000")
}
