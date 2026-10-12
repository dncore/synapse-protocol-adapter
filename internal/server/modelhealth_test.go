package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dncore/synapse-protocol-adapter/internal/config"
)

// channelRotationError is the body the shared gateway returns when a request
// lands on a channel that is not provisioned for its shape.
const channelRotationError = `{"detail":{"message":"GPT-6: gpt-6.1-sol tool calling requires /v1/responses or configured Chat-to-Responses routing"}}`

func healthTestConfig(fallbacks map[string]string) config.Config {
	cfg := fastRetryConfig()
	cfg.ModelHealth = config.ModelHealth{
		FailureThreshold: 2,
		Cooldown:         cfg.UpstreamRetry.Budget, // long enough for the test's lifetime
		Fallbacks:        fallbacks,
	}
	// 替换模型与 quirks 共用同一条改写闸门(见 passthrough.go)。
	cfg.Passthrough.ModelQuirks = true
	return cfg
}

// After the threshold is reached the model is marked unavailable, and a
// request for it stops spending the retry budget: inside a real failure
// window (measured: 15+ minutes) every retry fails the same way, so the
// retries only add latency before the client gets the same error.
func TestModelHealth_MarkedModelSkipsRetries(t *testing.T) {
	cfg := healthTestConfig(nil)
	s := &Server{cfg: cfg, logger: newTestLogger(), metrics: newTestRegistry(),
		health: newModelHealth(cfg.ModelHealth.FailureThreshold, cfg.ModelHealth.Cooldown)}

	fullBudget := int32(cfg.UpstreamRetry.MaxAttempts) // 一次用满预算的请求打多少次上游
	call := func(calls *atomic.Int32) int32 {
		before := calls.Load()
		resp, err := s.doUpstreamRetry(context.Background(),
			httptest.NewRequest("POST", "/v1/chat/completions", nil), "convert", true, "gpt-6.1-sol",
			func() (*http.Response, error) {
				calls.Add(1)
				return mkResp(400, "", channelRotationError), nil
			})
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != 400 {
			t.Fatalf("want the upstream's 400, got %d", resp.StatusCode)
		}
		return calls.Load() - before
	}
	var calls atomic.Int32
	first := call(&calls)  // 未标记:用满重试预算
	second := call(&calls) // 标记发生在这次结束时(阈值 = 2)
	if first != fullBudget {
		t.Fatalf("首个请求应打满预算 %d 次,实际 %d", fullBudget, first)
	}
	if second != fullBudget {
		t.Fatalf("第二次请求开始时还没到阈值,应照常打满 %d 次,实际 %d", fullBudget, second)
	}
	// 现在模型处于「不可用」窗口:请求不再花重试预算,只打一次(兼作恢复探测)。
	if third := call(&calls); third != 1 {
		t.Fatalf("已标记不可用的模型应只打一次上游,实际 %d 次", third)
	}

	// 恢复探测:一次成功即清除标记,重试预算随之恢复。
	if resp, err := s.doUpstreamRetry(context.Background(),
		httptest.NewRequest("POST", "/v1/chat/completions", nil), "convert", true, "gpt-6.1-sol",
		func() (*http.Response, error) { return mkResp(200, "", `{"choices":[]}`), nil }); err != nil || resp.StatusCode != 200 {
		t.Fatalf("probe failed: %v %v", resp, err)
	}
	if s.health.unavailable("gpt-6.1-sol") {
		t.Fatal("成功后标记应被清除")
	}
}

// A configured fallback model takes over while the original is unavailable —
// this is what keeps an agent working through a gateway failure window.
// Opt-in: with no fallback configured the request is not silently served by
// another model.
func TestModelHealth_FallbackSubstitution(t *testing.T) {
	cfg := healthTestConfig(map[string]string{"gpt-6.1-sol": "gpt-6-luna"})
	var gotModels []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotModels = append(gotModels, string(body))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer upstream.Close()

	s := newTestServerWithConfig(t, upstream.URL, cfg)
	proxy := httptest.NewServer(s.httpSrv.Handler)
	defer proxy.Close()

	// Mark the model unavailable without going through upstream.
	s.health.noteFailure("gpt-6.1-sol")
	s.health.noteFailure("gpt-6.1-sol")

	req, _ := http.NewRequest("POST", proxy.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-6.1-sol","messages":[{"role":"user","content":"hi"}]}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("want 200 through the fallback, got %d", resp.StatusCode)
	}
	if len(gotModels) != 1 || !strings.Contains(gotModels[0], `"model":"gpt-6-luna"`) {
		t.Fatalf("上游应收到替代模型: %v", gotModels)
	}
	if got := s.metrics.ModelFallbackTotal.With("gpt-6.1-sol").Value(); got != 1 {
		t.Fatalf("fallback 计数 = %d, want 1", got)
	}
}

// Without a configured fallback an unavailable model is not substituted: the
// caller sees the upstream's own error (never a silently different model).
func TestModelHealth_NoFallbackMeansNoSubstitution(t *testing.T) {
	cfg := healthTestConfig(nil)
	var bodies []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(body))
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer upstream.Close()

	s := newTestServerWithConfig(t, upstream.URL, cfg)
	proxy := httptest.NewServer(s.httpSrv.Handler)
	defer proxy.Close()

	s.health.noteFailure("gpt-6.1-sol")
	s.health.noteFailure("gpt-6.1-sol")

	req, _ := http.NewRequest("POST", proxy.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-6.1-sol","messages":[]}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if len(bodies) != 1 || !strings.Contains(bodies[0], `"model":"gpt-6.1-sol"`) {
		t.Fatalf("未配置 fallback 时不该替换模型: %v", bodies)
	}
}

func TestModelHealth_SubstitutionNeedsFallbacks(t *testing.T) {
	s := &Server{cfg: healthTestConfig(map[string]string{"gpt-6-sol": "gpt-6-luna"}),
		logger: newTestLogger(), metrics: newTestRegistry(),
		health: newModelHealth(1, 0)}
	if got, sub := s.substitutedModel("gpt-6-sol", true); sub || got != "gpt-6-sol" {
		t.Fatalf("未标记时不该替换: %q %v", got, sub)
	}
	s.health.noteFailure(healthKey("gpt-6-sol", true))
	if got, sub := s.substitutedModel("gpt-6-sol", true); !sub || got != "gpt-6-luna" {
		t.Fatalf("标记后应替换为配置的模型: %q %v", got, sub)
	}
	if got, sub := s.substitutedModel("gpt-6.1-sol", true); sub || got != "gpt-6.1-sol" {
		t.Fatalf("未配置的 id 不该替换: %q %v", got, sub)
	}
}

// 形状分桶(2026-10-12 部署实测缺陷):渠道窗口只坏「带工具」的形状,不带工具的
// 请求在窗口内照样成功 —— 它不能清掉工具桶的标记,否则下一个工具请求又要白等
// 满重试预算再吃 400。
func TestModelHealth_ShapesHaveSeparateBuckets(t *testing.T) {
	cfg := healthTestConfig(nil)
	s := &Server{cfg: cfg, logger: newTestLogger(), metrics: newTestRegistry(),
		health: newModelHealth(cfg.ModelHealth.FailureThreshold, cfg.ModelHealth.Cooldown)}

	toolKey := healthKey("gpt-6.1-sol", true)
	s.health.noteFailure(toolKey)
	// 达到阈值(两次)。
	s.health.noteFailure(toolKey)
	if !s.health.unavailable(toolKey) {
		t.Fatal("工具形状连续失败应标记不可用")
	}

	// 无工具桶的成功:不同的桶,不得清除工具标记。
	s.health.noteSuccess(healthKey("gpt-6.1-sol", false))
	if !s.health.unavailable(toolKey) {
		t.Fatal("无工具请求的成功不能清除工具桶的标记")
	}

	// 工具桶自己的成功才清除。
	s.health.noteSuccess(toolKey)
	if s.health.unavailable(toolKey) {
		t.Fatal("工具形状成功后标记应被清除")
	}
}

// 冷却到期后只探一次:长窗口里(实测一次窗口 1h45m 还在继续)若每次探测都花满
// 重试预算,用户每隔一个冷却就要白等二十多秒。
func TestModelHealth_ProbationProbesOnce(t *testing.T) {
	cfg := healthTestConfig(nil)
	cfg.ModelHealth.Cooldown = 30 * time.Millisecond // 测试里把冷却压到毫秒级
	s := &Server{cfg: cfg, logger: newTestLogger(), metrics: newTestRegistry(),
		health: newModelHealth(cfg.ModelHealth.FailureThreshold, cfg.ModelHealth.Cooldown)}

	var calls atomic.Int32
	call := func() int32 {
		before := calls.Load()
		_, _ = s.doUpstreamRetry(context.Background(),
			httptest.NewRequest("POST", "/v1/chat/completions", nil), "convert", true, "gpt-6.1-sol",
			func() (*http.Response, error) {
				calls.Add(1)
				return mkResp(400, "", channelRotationError), nil
			})
		return calls.Load() - before
	}
	call() // 首次请求:用满预算
	call() // 达到阈值,标记不可用
	if third := call(); third != 1 {
		t.Fatalf("标记期间应只打一次,实际 %d", third)
	}
	// 冷却到期 → 探测期:仍然只打一次(而不是又回到满预算)
	time.Sleep(2 * cfg.ModelHealth.Cooldown)
	if probed := call(); probed != 1 {
		t.Fatalf("探测期应只打一次,实际 %d", probed)
	}
	if !s.health.unavailable("gpt-6.1-sol") {
		t.Fatal("探测失败后应立即重新标记")
	}
}

// 分类必须只认「网关自己模板里的整句」，不能被回显的请求内容骗到：
// 错误体是客户端可影响的，而命中会改写跨用户的全局状态。
func TestTransientClassificationIsNotForgeable(t *testing.T) {
	// 真实的五条(都出现在消息开头)——必须命中
	real := []string{
		`{"detail":{"message":"GPT-6: gpt-6.1-sol tool calling requires /v1/responses or configured Chat-to-Responses routing  (type: api_upstream_error) (request_id: x)"}}`,
		`{"detail":{"message":"GPT-6: gpt-6-sol reasoning effort must be low, medium, high, xhigh, or max  (type: api_upstream_error) (request_id: x)"}}`,
		`{"detail":{"message":"Unsupported value: 'reasoning_effort' does not support 'none' with this model. Supported values are: 'low', 'medium', 'high', and 'xhigh'. (request_id: x)"}}`,
		`{"detail":{"message":"Unsupported value: 'none' is not supported with the 'gpt-6.1-sol-2026-09-29' model. Supported values are: 'low','medium'. (request_id: x)"}}`,
		`{"detail":{"message":"Model resources are currently busy. Please try again later. (request_id: x)"}}`,
		`{"detail":{"message":"Insufficient quota available for instant inference. (type: deployment_disabled) (request_id: x)"}}`,
	}
	for _, body := range real {
		if !isTransientUpstreamResponse(400, []byte(body)) {
			t.Fatalf("真实文案应判为渠道轮换: %s", body)
		}
	}
	// Anthropic 挂载会把内部错误再包一层 JSON 字符串——也要认出来
	wrapped := `{"type":"error","error":{"type":"api_error","message":"{\"detail\":{\"message\":\"GPT-6: gpt-6.1-sol tool calling requires /v1/responses or configured Chat-to-Responses routing\"}}"}}`
	if !isTransientUpstreamResponse(400, []byte(wrapped)) {
		t.Fatal("嵌套的网关消息应判为渠道轮换")
	}

	// 伪造:把标记短语塞进客户端可控的位置(模型名、参数值、其它字段)
	forged := []string{
		// 模型名里带短语,网关回显它 → 不能命中
		`{"detail":{"message":"Invalid model 'x requires /v1/responses or configured Chat-to-Responses routing'."}}`,
		// 参数值里带短语
		`{"detail":{"message":"Unsupported value: 'temperature' does not support 0.3 with this model. Note: reasoning effort must be low, medium, high, xhigh, or max"}}`,
		// 短语出现在别的字段而不是 message
		`{"detail":{"message":"Bad request","marker":"Model resources are currently busy"}}`,
		`{"error":{"message":"boom","hint":"Insufficient quota available for instant inference"}}`,
		// 完全不是这套模板
		`{"detail":{"message":"Invalid 'messages[0]': expected a string"}}`,
		// 客户端把整段当 content 传进来,网关原样回显
		`{"detail":{"message":"Model resources are currently busy. Please try again later."}} trailing client text`,
	}
	for _, body := range forged {
		if isTransientUpstreamResponse(400, []byte(body)) {
			t.Fatalf("伪造的响应体不应判为渠道轮换: %s", body)
		}
	}
	// 状态码约束不变:2xx/5xx 不看
	if isTransientUpstreamResponse(200, []byte(real[0])) || isTransientUpstreamResponse(500, []byte(real[0])) {
		t.Fatal("只有 400/503 才可能是渠道轮换")
	}
}

// 客户端可控的模型 id 不能把状态表撑爆,也不能超长键。
func TestModelHealth_TrackingIsBounded(t *testing.T) {
	h := newModelHealth(1, time.Minute)
	for i := 0; i < maxTrackedModels+50; i++ {
		h.noteFailure(fmt.Sprintf("model-%d", i))
	}
	if got := len(h.failures); got > maxTrackedModels {
		t.Fatalf("状态表未受上限约束: %d", got)
	}
	long := strings.Repeat("m", maxModelIDLen+1)
	if h.noteFailure(long) {
		t.Fatal("超长 id 不应被记账")
	}
	if h.unavailable(long) {
		t.Fatal("超长 id 不应被标记")
	}
}
