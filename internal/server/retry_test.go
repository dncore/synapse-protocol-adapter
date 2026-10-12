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

// mkSSEResp builds a streaming (text/event-stream) 200 response.
func mkSSEResp(body string) *http.Response {
	h := http.Header{}
	h.Set("Content-Type", "text/event-stream")
	return &http.Response{StatusCode: 200, Header: h, Body: io.NopCloser(strings.NewReader(body))}
}

// 流式呈现的渠道轮换失败:200 + 首帧错误(2026-10-12 实测——流式客户端收到的
// "Provider returned 400" 就是这一帧;按状态分类对它完全不生效,必须按首帧文本)。
func TestTransientStreamErrorClassification(t *testing.T) {
	errorFrame := "data: {\"error\": {\"message\": \"Provider returned 400\", \"type\": \"api_error\", \"http_status\": 400}}\n\n"
	if !isTransientStreamError([]byte(errorFrame)) {
		t.Fatal("网关流式错误帧应判为渠道轮换")
	}
	normal := "data: {\"id\":\"chatcmpl-x\",\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n"
	if isTransientStreamError([]byte(normal)) {
		t.Fatal("正常分片不该命中")
	}
	// 伪造:整句之外任何附加内容都不认(全文锚定)
	forged := "data: {\"error\": {\"message\": \"Provider returned 400 with details\"}}\n\n"
	if isTransientStreamError([]byte(forged)) {
		t.Fatal("伪造文案不该命中")
	}
}

// 200 + 首帧错误:记一次失败并按预算重试;重试成功后的流式字节原样保留(含首帧)。
func TestUpstreamRetryAbsorbsStreamedErrorFrame(t *testing.T) {
	cfg := fastRetryConfig()
	s := retryTestServer(cfg)
	s.health = newModelHealth(cfg.ModelHealth.FailureThreshold, cfg.ModelHealth.Cooldown)
	var calls atomic.Int64
	resp, err := s.doUpstreamRetry(context.Background(),
		httptest.NewRequest("POST", "/v1/chat/completions", nil), "convert", true, "gpt-6.1-sol",
		func() (*http.Response, error) {
			if calls.Add(1) == 1 {
				return mkSSEResp("data: {\"error\": {\"message\": \"Provider returned 400\", \"type\": \"api_error\", \"http_status\": 400}}\n\n"), nil
			}
			return mkSSEResp("data: {\"id\":\"chatcmpl-1\",\"choices\":[]}\n\ndata: [DONE]\n\n"), nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got < 2 {
		t.Fatalf("首帧错误的 200 应触发重试,实际尝试 %d 次", got)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "chatcmpl-1") || !strings.Contains(string(body), "[DONE]") {
		t.Fatalf("重试后的流式字节被破坏: %q", body)
	}
	// 被重试吸收:与 429 语义一致,成功即清零。
	if s.health.unavailable("gpt-6.1-sol") || s.health.failureCount("gpt-6.1-sol") != 0 {
		t.Fatal("重试吸收成功后的健康状态应干净")
	}
}

// 已标记模型:流式错误 200 不重试、不当作恢复——标记必须保持(否则流式客户端
// 在窗口内永远无法触发模拟接管)。
func TestUpstreamStreamErrorFrameKeepsMark(t *testing.T) {
	cfg := fastRetryConfig()
	s := retryTestServer(cfg)
	s.health = newModelHealth(cfg.ModelHealth.FailureThreshold, cfg.ModelHealth.Cooldown)
	s.health.noteFailure("gpt-6.1-sol")
	s.health.noteFailure("gpt-6.1-sol")
	var calls atomic.Int64
	_, err := s.doUpstreamRetry(context.Background(),
		httptest.NewRequest("POST", "/v1/chat/completions", nil), "convert", true, "gpt-6.1-sol",
		func() (*http.Response, error) {
			calls.Add(1)
			return mkSSEResp("data: {\"error\": {\"message\": \"Provider returned 400\", \"type\": \"api_error\", \"http_status\": 400}}\n\n"), nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("已标记模型应只打一次,实际 %d", got)
	}
	if !s.health.unavailable("gpt-6.1-sol") {
		t.Fatal("流式错误 200 不得清掉标记")
	}
}

func TestUpstreamRetry_AbsorbsThenSucceeds(t *testing.T) {
	var calls atomic.Int64
	s := retryTestServer(fastRetryConfig())

	resp, err := s.doUpstreamRetry(context.Background(),
		httptest.NewRequest("POST", "/v1/chat/completions", nil), "passthrough", true, "",
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
		httptest.NewRequest("POST", "/v1/chat/completions", nil), "convert", true, "",
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

// 只有 429 与「渠道轮换」类错误会重试:普通 5xx 原样交回客户端。
func TestUpstreamRetry_ServerErrorIsNotRetried(t *testing.T) {
	var calls atomic.Int64
	s := retryTestServer(fastRetryConfig())

	resp, err := s.doUpstreamRetry(context.Background(),
		httptest.NewRequest("POST", "/v1/chat/completions", nil), "convert", true, "",
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
		httptest.NewRequest("POST", "/v1/chat/completions", nil), "passthrough", true, "",
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
		httptest.NewRequest("POST", "/v1/chat/completions", nil), "passthrough", false, "",
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
		httptest.NewRequest("POST", "/v1/chat/completions", nil), "convert", true, "",
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
		httptest.NewRequest("POST", "/v1/chat/completions", nil), "convert", true, "",
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
		httptest.NewRequest("POST", "/v1/chat/completions", nil), "convert", true, "",
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

// 网关在渠道间轮换:同一个请求体,部分渠道没配 chat→responses 路由,
// 会回特定文案的 400/503。这类失败与客户端参数无关(实测同一形状
// 8/8 失败 → 几分钟后 20/20 成功),所以代理按文案识别后原样重发。
func TestUpstreamRetry_TransientChannelErrorIsRetried(t *testing.T) {
	var bodies []string
	s := retryTestServer(fastRetryConfig())

	resp, err := s.doUpstreamRetry(context.Background(),
		httptest.NewRequest("POST", "/v1/chat/completions", nil), "passthrough", true, "",
		func() (*http.Response, error) {
			bodies = append(bodies, "attempt")
			if len(bodies) < 2 {
				return mkResp(400, "", `{"detail":{"message":"GPT-6: gpt-6.1-sol tool calling requires /v1/responses or configured Chat-to-Responses routing"}}`), nil
			}
			return mkResp(200, "", "ok"), nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || len(bodies) != 2 {
		t.Fatalf("want 200 after 2 attempts, got %d after %d", resp.StatusCode, len(bodies))
	}
	if got := s.metrics.RetriesAbsorbedTotal.Value(); got != 1 {
		t.Fatalf("retry not counted: %d", got)
	}
}

// 客户端自身参数错误的 400 不能重试:重试只会同样失败,白等一轮。
func TestUpstreamRetry_ClientErrorIsNotRetried(t *testing.T) {
	calls := 0
	s := retryTestServer(fastRetryConfig())

	resp, err := s.doUpstreamRetry(context.Background(),
		httptest.NewRequest("POST", "/v1/chat/completions", nil), "passthrough", true, "",
		func() (*http.Response, error) {
			calls++
			return mkResp(400, "", `{"detail":{"message":"Invalid 'messages[0]': expected a string"}}`), nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 400 || calls != 1 {
		t.Fatalf("client error must not be retried: status %d after %d calls", resp.StatusCode, calls)
	}
	// 错误体必须原样留给调用方
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "messages[0]") {
		t.Fatalf("error body was consumed by the probe: %q", body)
	}
}

// 持续坏窗口:重试到预算用尽后,把最后一次的上游响应原样交回。
func TestUpstreamRetry_TransientErrorExhaustsBudget(t *testing.T) {
	calls := 0
	cfg := fastRetryConfig()
	cfg.UpstreamRetry.MaxAttempts = 3
	s := retryTestServer(cfg)

	resp, err := s.doUpstreamRetry(context.Background(),
		httptest.NewRequest("POST", "/v1/chat/completions", nil), "passthrough", true, "",
		func() (*http.Response, error) {
			calls++
			return mkResp(503, "", `{"detail":{"message":"Model resources are currently busy."}}`), nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 503 || calls != 3 {
		t.Fatalf("want 503 after 3 attempts, got %d after %d", resp.StatusCode, calls)
	}
}
