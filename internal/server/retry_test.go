package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dncore/synapse-protocol-adapter/internal/config"
)

// retryTestServer builds a bare server sufficient for doUpstreamRetry and
// the rejection helpers.
func retryTestServer(cfg config.Config) *Server {
	return &Server{cfg: cfg, logger: newTestLogger(), metrics: newTestRegistry()}
}

// fastRetryConfig keeps absorption semantics but with millisecond timings.
func fastRetryConfig() config.Config {
	cfg := config.Defaults()
	cfg.UpstreamRetry.InitialBackoff = time.Millisecond
	cfg.UpstreamRetry.MaxBackoff = 4 * time.Millisecond
	cfg.UpstreamRetry.Budget = 2 * time.Second
	return cfg
}

func mkResp(code int, retryAfter, body string) *http.Response {
	h := http.Header{}
	if retryAfter != "" {
		h.Set("Retry-After", retryAfter)
	}
	return &http.Response{StatusCode: code, Header: h, Body: io.NopCloser(strings.NewReader(body))}
}

func TestUpstreamRetry_AbsorbsThenSucceeds(t *testing.T) {
	var calls atomic.Int64
	s := retryTestServer(fastRetryConfig())

	resp, err := s.doUpstreamRetry(context.Background(),
		httptest.NewRequest("POST", "/v1/chat/completions", nil), "passthrough", true,
		func() (*http.Response, error) {
			if calls.Add(1) < 3 {
				return mkResp(429, "0", `{"detail":"too many"}`), nil
			}
			return mkResp(200, "", "ok"), nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || calls.Load() != 3 {
		t.Fatalf("want 200 after 3 calls, got %d after %d", resp.StatusCode, calls.Load())
	}
	if got := s.metrics.Upstream429Total.With("passthrough").Value(); got != 2 {
		t.Fatalf("upstream_429_total = %d, want 2", got)
	}
	if got := s.metrics.RetriesAbsorbedTotal.Value(); got != 2 {
		t.Fatalf("absorbed = %d, want 2", got)
	}
	if got := s.metrics.RetryExhaustedTotal.Value(); got != 0 {
		t.Fatalf("exhausted = %d, want 0", got)
	}
}

func TestUpstreamRetry_ExhaustsBudget(t *testing.T) {
	var calls atomic.Int64
	cfg := fastRetryConfig()
	cfg.UpstreamRetry.MaxAttempts = 3
	s := retryTestServer(cfg)

	resp, err := s.doUpstreamRetry(context.Background(),
		httptest.NewRequest("POST", "/v1/chat/completions", nil), "convert", true,
		func() (*http.Response, error) {
			calls.Add(1)
			return mkResp(429, "", `{"detail":"too many"}`), nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 429 || calls.Load() != 3 {
		t.Fatalf("want final 429 after 3 calls, got %d after %d", resp.StatusCode, calls.Load())
	}
	if got := s.metrics.RetriesAbsorbedTotal.Value(); got != 2 {
		t.Fatalf("absorbed = %d, want 2", got)
	}
	if got := s.metrics.RetryExhaustedTotal.Value(); got != 1 {
		t.Fatalf("exhausted = %d, want 1", got)
	}
	if got := s.metrics.Upstream429Total.With("convert").Value(); got != 3 {
		t.Fatalf("upstream_429_total = %d, want 3", got)
	}
}

func TestUpstreamRetry_OnlyRetries429(t *testing.T) {
	var calls atomic.Int64
	s := retryTestServer(fastRetryConfig())

	resp, err := s.doUpstreamRetry(context.Background(),
		httptest.NewRequest("POST", "/v1/chat/completions", nil), "convert", true,
		func() (*http.Response, error) {
			calls.Add(1)
			return mkResp(500, "", "boom"), nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 500 || calls.Load() != 1 {
		t.Fatalf("500 must not be retried: status %d after %d calls", resp.StatusCode, calls.Load())
	}
	if got := s.metrics.Upstream429Total.With("convert").Value(); got != 0 {
		t.Fatalf("429 counter moved on a 500: %d", got)
	}
}

func TestUpstreamRetry_DisabledPassthroughVerbatim(t *testing.T) {
	var calls atomic.Int64
	cfg := fastRetryConfig()
	cfg.UpstreamRetry.Enabled = false
	s := retryTestServer(cfg)

	resp, err := s.doUpstreamRetry(context.Background(),
		httptest.NewRequest("POST", "/v1/chat/completions", nil), "passthrough", true,
		func() (*http.Response, error) {
			calls.Add(1)
			return mkResp(429, "", "verbatim"), nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 429 || calls.Load() != 1 {
		t.Fatalf("disabled retry must pass through once: status %d after %d calls", resp.StatusCode, calls.Load())
	}
	if got := s.metrics.RetriesAbsorbedTotal.Value(); got != 0 {
		t.Fatalf("absorbed = %d, want 0", got)
	}
}

func TestUpstreamRetry_NonReplayableNotRetried(t *testing.T) {
	var calls atomic.Int64
	s := retryTestServer(fastRetryConfig())

	resp, err := s.doUpstreamRetry(context.Background(),
		httptest.NewRequest("POST", "/v1/chat/completions", nil), "passthrough", false,
		func() (*http.Response, error) {
			calls.Add(1)
			return mkResp(429, "", "streamed body"), nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 429 || calls.Load() != 1 {
		t.Fatalf("streamed body must not be retried: status %d after %d calls", resp.StatusCode, calls.Load())
	}
	if got := s.metrics.Upstream429Total.With("passthrough").Value(); got != 1 {
		t.Fatalf("upstream_429_total = %d, want 1 (still counted)", got)
	}
}

func TestUpstreamRetry_BudgetStopsRetrying(t *testing.T) {
	var calls atomic.Int64
	cfg := fastRetryConfig()
	cfg.UpstreamRetry.InitialBackoff = 50 * time.Millisecond
	cfg.UpstreamRetry.MaxBackoff = 50 * time.Millisecond
	cfg.UpstreamRetry.Budget = 80 * time.Millisecond
	cfg.UpstreamRetry.MaxAttempts = 10
	s := retryTestServer(cfg)

	resp, err := s.doUpstreamRetry(context.Background(),
		httptest.NewRequest("POST", "/v1/chat/completions", nil), "convert", true,
		func() (*http.Response, error) {
			calls.Add(1)
			return mkResp(429, "", "too many"), nil
		})
	if err != nil {
		t.Fatal(err)
	}
	// Attempt 1, wait 50ms, attempt 2; next wait would start past the
	// 80ms budget, so it stops there.
	if resp.StatusCode != 429 || calls.Load() != 2 {
		t.Fatalf("budget must stop after 2 calls, got %d after %d", resp.StatusCode, calls.Load())
	}
	if got := s.metrics.RetryExhaustedTotal.Value(); got != 1 {
		t.Fatalf("exhausted = %d, want 1", got)
	}
}

func TestUpstreamRetry_ContextCancelDuringBackoff(t *testing.T) {
	var calls atomic.Int64
	cfg := fastRetryConfig()
	cfg.UpstreamRetry.InitialBackoff = time.Second
	cfg.UpstreamRetry.MaxBackoff = time.Second
	s := retryTestServer(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	_, err := s.doUpstreamRetry(ctx,
		httptest.NewRequest("POST", "/v1/chat/completions", nil), "convert", true,
		func() (*http.Response, error) {
			calls.Add(1)
			return mkResp(429, "", "too many"), nil
		})
	if err == nil || !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("want context cancellation, got %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1", calls.Load())
	}
}

func TestUpstreamRetry_RetryAfterClampedByMaxBackoff(t *testing.T) {
	var calls atomic.Int64
	cfg := fastRetryConfig()
	cfg.UpstreamRetry.MaxBackoff = 10 * time.Millisecond
	cfg.UpstreamRetry.MaxAttempts = 2
	s := retryTestServer(cfg)

	start := time.Now()
	_, err := s.doUpstreamRetry(context.Background(),
		httptest.NewRequest("POST", "/v1/chat/completions", nil), "convert", true,
		func() (*http.Response, error) {
			calls.Add(1)
			return mkResp(429, "3600", "too many"), nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Retry-After must be clamped to max_backoff, waited %s", elapsed)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2", calls.Load())
	}
}

func TestParseRetryAfter(t *testing.T) {
	if _, ok := parseRetryAfter(""); ok {
		t.Fatal("empty must not parse")
	}
	if _, ok := parseRetryAfter("soon"); ok {
		t.Fatal("garbage must not parse")
	}
	if d, ok := parseRetryAfter("3"); !ok || d != 3*time.Second {
		t.Fatalf("delta seconds: %v %v", d, ok)
	}
	when := time.Now().Add(2 * time.Second).UTC().Format(http.TimeFormat)
	if d, ok := parseRetryAfter(when); !ok || d <= 0 || d > 3*time.Second {
		t.Fatalf("http-date: %v %v", d, ok)
	}
}

func TestRejectUpstream429_ClientContract(t *testing.T) {
	s := retryTestServer(fastRetryConfig())
	w := httptest.NewRecorder()
	resp := mkResp(429, "7", `{"detail":"Too many concurrent requests. Maximum allowed: 100"}`)
	s.rejectUpstream429(w, resp)

	if w.Code != 429 {
		t.Fatalf("status = %d", w.Code)
	}
	if got := w.Header().Get("Retry-After"); got != "7" {
		t.Fatalf("Retry-After = %q, want upstream's 7", got)
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	emsg := out["error"].(map[string]any)["message"]
	if !strings.Contains(emsg.(string), "Too many concurrent requests") {
		t.Fatalf("upstream message lost: %v", out)
	}
}

// The converter route absorbs a 429 and still delivers the completed SSE
// stream; the client sees no error at all.
func TestResponses_429AbsorbedThenStreams(t *testing.T) {
	var hits atomic.Int64
	upSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(429)
			fmt.Fprint(w, `{"detail":"Too many concurrent requests. Maximum allowed: 100"}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		f := w.(http.Flusher)
		fmt.Fprint(w, "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"t\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hi\"}}]}\n\n")
		f.Flush()
		fmt.Fprint(w, "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"t\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		f.Flush()
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer upSrv.Close()

	s := New(testConfig(upSrv.URL), newTestLogger(), newTestRegistry())
	proxySrv := httptest.NewServer(s.httpSrv.Handler)
	defer proxySrv.Close()

	resp, err := http.Post(proxySrv.URL+"/v1/responses", "application/json",
		strings.NewReader(responsesBody(true)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(body), "response.completed") {
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
	if hits.Load() != 2 {
		t.Fatalf("upstream hits = %d, want 2", hits.Load())
	}
	if got := s.metrics.RetriesAbsorbedTotal.Value(); got != 1 {
		t.Fatalf("absorbed = %d, want 1", got)
	}
}

// When the budget runs out, the converter route answers 429 + Retry-After
// with the upstream's message preserved.
func TestResponses_429ExhaustedContract(t *testing.T) {
	var hits atomic.Int64
	upSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(429)
		fmt.Fprint(w, `{"detail":"Too many concurrent requests. Maximum allowed: 100"}`)
	}))
	defer upSrv.Close()

	cfg := testConfig(upSrv.URL)
	cfg.UpstreamRetry.MaxAttempts = 3
	s := New(cfg, newTestLogger(), newTestRegistry())
	proxySrv := httptest.NewServer(s.httpSrv.Handler)
	defer proxySrv.Close()

	resp, err := http.Post(proxySrv.URL+"/v1/responses", "application/json",
		strings.NewReader(responsesBody(false)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 429 {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}
	if got := resp.Header.Get("Retry-After"); got == "" {
		t.Fatal("missing Retry-After on exhausted 429")
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "Too many concurrent requests") {
		t.Fatalf("upstream message lost: %s", body)
	}
	if hits.Load() != 3 {
		t.Fatalf("upstream hits = %d, want 3", hits.Load())
	}
	if got := s.metrics.RetryExhaustedTotal.Value(); got != 1 {
		t.Fatalf("exhausted = %d, want 1", got)
	}
}

// A passthrough body within the replay buffer is re-sent byte-identically
// after a 429.
func TestPassthrough_RetryReplaysBody(t *testing.T) {
	var hits atomic.Int64
	var mu sync.Mutex
	var bodies [][]byte

	upSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, b)
		mu.Unlock()
		if hits.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(429)
			fmt.Fprint(w, `{"error":{"message":"rate limited"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		fmt.Fprint(w, `{"id":"c1","choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer upSrv.Close()

	s := New(testConfig(upSrv.URL), newTestLogger(), newTestRegistry())
	proxySrv := httptest.NewServer(s.httpSrv.Handler)
	defer proxySrv.Close()

	payload := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(proxySrv.URL+"/v1/chat/completions", "application/json", strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200 after absorption", resp.StatusCode)
	}
	if hits.Load() != 2 {
		t.Fatalf("upstream hits = %d, want 2", hits.Load())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 || string(bodies[0]) != payload || string(bodies[1]) != payload {
		t.Fatalf("replayed body differs: %q vs %q", bodies[0], bodies[1])
	}
}

// A body beyond the replay buffer streams through and a 429 on it is not
// retried (only reshaped for the client).
func TestPassthrough_LargeBodyNotRetried(t *testing.T) {
	var hits atomic.Int64
	upSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = io.ReadAll(r.Body)
		w.WriteHeader(429)
		fmt.Fprint(w, `{"detail":"too many"}`)
	}))
	defer upSrv.Close()

	cfg := testConfig(upSrv.URL)
	cfg.UpstreamRetry.BufferMaxBytes = 8
	s := New(cfg, newTestLogger(), newTestRegistry())
	proxySrv := httptest.NewServer(s.httpSrv.Handler)
	defer proxySrv.Close()

	resp, err := http.Post(proxySrv.URL+"/v1/chat/completions", "application/json",
		strings.NewReader("0123456789abcdef")) // 16 bytes > 8
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 429 {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}
	if hits.Load() != 1 {
		t.Fatalf("large body must not be retried; upstream hits = %d", hits.Load())
	}
	if got := resp.Header.Get("Retry-After"); got != "1" {
		t.Fatalf("Retry-After = %q, want synthesized 1", got)
	}
}
