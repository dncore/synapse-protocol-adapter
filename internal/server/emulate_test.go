package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dncore/synapse-protocol-adapter/internal/config"
)

// 上游(模拟网关的坏渠道):不认 tools,但按提示词协议给出调用。
const emulatedReply = `{"choices":[{"index":0,"message":{"role":"assistant","content":"{\"tool_calls\":[{\"name\":\"get_time\",\"arguments\":{\"city\":\"Shanghai\"}}]}"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":12,"total_tokens":22}}`

func emulationConfig() config.Config {
	cfg := fastRetryConfig()
	cfg.ModelHealth = config.ModelHealth{FailureThreshold: 1, Cooldown: cfg.UpstreamRetry.Budget}
	cfg.ToolEmulation = config.ToolEmulation{Enabled: true, Models: []string{"gpt-6.1-sol"}}
	return cfg
}

func newEmulationProxy(t *testing.T, cfg config.Config, upstreamBody string, record *[]string) *httptest.Server {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		*record = append(*record, string(body))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(upstreamBody))
	}))
	t.Cleanup(up.Close)
	s := newTestServerWithConfig(t, up.URL, cfg)
	proxy := httptest.NewServer(s.httpSrv.Handler)
	t.Cleanup(proxy.Close)
	// 标记该模型不可用(等价于窗口内连续失败两次;按形状分桶,标记工具桶)
	s.health.noteFailure(healthKey("gpt-6.1-sol", true))
	return proxy
}

// 窗口内:客户端要的 tools 被搬进提示词,模型按协议回的调用被还原成真正的
// Responses function_call —— 客户端看到的是一个正常的工具调用回合。
func TestConvert_EmulatesToolCallsWhenModelUnavailable(t *testing.T) {
	var upstream []string
	proxy := newEmulationProxy(t, emulationConfig(), emulatedReply, &upstream)

	reqBody := `{"model":"gpt-6.1-sol","input":"Call get_time for Shanghai.",
	  "tools":[{"type":"function","name":"get_time","description":"time","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}]}`
	req, _ := http.NewRequest("POST", proxy.URL+"/v1/responses", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var out struct {
		Output []struct {
			Type      string `json:"type"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
			CallID    string `json:"call_id"`
		} `json:"output"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Output) != 1 || out.Output[0].Type != "function_call" || out.Output[0].Name != "get_time" {
		t.Fatalf("应还原成 function_call: %+v", out.Output)
	}
	if !strings.Contains(out.Output[0].Arguments, "Shanghai") || out.Output[0].CallID == "" {
		t.Fatalf("调用参数/ID 缺失: %+v", out.Output[0])
	}
	// 上游收到的必须是「无 tools 字段 + 协议在 system 里 + 非流式」
	if len(upstream) != 1 {
		t.Fatalf("上游请求数 = %d", len(upstream))
	}
	sent := upstream[0]
	for _, banned := range []string{`"tools"`, `"stream":true`} {
		if strings.Contains(sent, banned) {
			t.Fatalf("上游不该收到 %s:\n%s", banned, sent)
		}
	}
	// 协议文本在 JSON 里是转义过的("tool_calls"),所以这里查不带引号的片段。
	if !strings.Contains(sent, "tool_calls") || !strings.Contains(sent, "get_time") || !strings.Contains(sent, "[tool result]") {
		t.Fatalf("协议未写进提示词:\n%s", sent)
	}
}

// 同一个模拟回合,流式客户端拿到的应是完整的 Responses 事件流(含参数增量),
// 而不是被降级成非流式。
func TestConvert_EmulatedTurnStreamsResponsesEvents(t *testing.T) {
	var upstream []string
	proxy := newEmulationProxy(t, emulationConfig(), emulatedReply, &upstream)

	reqBody := `{"model":"gpt-6.1-sol","stream":true,"input":"Call get_time for Shanghai.",
	  "tools":[{"type":"function","name":"get_time","description":"time","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}]}`
	req, _ := http.NewRequest("POST", proxy.URL+"/v1/responses", strings.NewReader(reqBody))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	body := string(raw)
	for _, want := range []string{
		"event: response.output_item.added",
		"event: response.function_call_arguments.delta",
		"event: response.completed",
		"get_time",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("事件流缺 %q:\n%s", want, body)
		}
	}
}

// chat 直通路径:窗口内工具回合同样由模拟接管(此前只有 /responses 与
// Anthropic 挂载有模拟,pi/reasonix 这类 chat 客户端在窗口里拿不到)。
// 流式客户端应拿到合成的 chat 分片流。
func TestPassthrough_EmulatesToolCallsWhenModelUnavailable(t *testing.T) {
	var upstream []string
	proxy := newEmulationProxy(t, emulationConfig(), emulatedReply, &upstream)

	reqBody := `{"model":"gpt-6.1-sol","messages":[{"role":"user","content":"Call get_time for Shanghai."}],
	  "tools":[{"type":"function","function":{"name":"get_time","description":"time","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}]}`
	req, _ := http.NewRequest("POST", proxy.URL+"/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var out struct {
		Choices []struct {
			Message struct {
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Choices) != 1 || len(out.Choices[0].Message.ToolCalls) != 1 ||
		out.Choices[0].Message.ToolCalls[0].Function.Name != "get_time" ||
		out.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("应还原成 chat tool_calls: %+v", out)
	}
	// 上游收到:无 tools 字段 + 协议在提示词里 + 非流式
	if len(upstream) != 1 {
		t.Fatalf("上游请求数 = %d", len(upstream))
	}
	sent := upstream[0]
	if strings.Contains(sent, `"tools"`) || strings.Contains(sent, `"stream":true`) {
		t.Fatalf("上游不该收到 tools 或 stream:true:\n%s", sent)
	}
	if !strings.Contains(sent, "tool_calls") || !strings.Contains(sent, "[tool result]") {
		t.Fatalf("协议未写进提示词:\n%s", sent)
	}

	// 流式客户端:合成 chat 分片,含 tool_calls 与结束标记。
	streamBody := `{"model":"gpt-6.1-sol","stream":true,"messages":[{"role":"user","content":"Call get_time for Shanghai."}],
	  "tools":[{"type":"function","function":{"name":"get_time","description":"time","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}]}`
	sreq, _ := http.NewRequest("POST", proxy.URL+"/v1/chat/completions", strings.NewReader(streamBody))
	sreq.Header.Set("Content-Type", "application/json")
	sresp, err := http.DefaultClient.Do(sreq)
	if err != nil {
		t.Fatal(err)
	}
	defer sresp.Body.Close()
	sraw, _ := io.ReadAll(sresp.Body)
	s := string(sraw)
	for _, want := range []string{"chat.completion.chunk", `"tool_calls"`, "get_time", "data: [DONE]"} {
		if !strings.Contains(s, want) {
			t.Fatalf("流式分片缺 %q:\n%s", want, s)
		}
	}
}

// 不在配置名单里的模型、或没被标记不可用的模型:一律走原生 tools,不做模拟。

func TestConvert_NoEmulationOutsideItsConditions(t *testing.T) {
	cfg := emulationConfig()
	cfg.ToolEmulation.Models = []string{"gpt-6-sol"} // 当前模型不在名单
	var upstream []string
	proxy := newEmulationProxy(t, cfg, emulatedReply, &upstream)

	reqBody := `{"model":"gpt-6.1-sol","input":"hi","tools":[{"type":"function","name":"get_time","parameters":{"type":"object","properties":{}}}]}`
	req, _ := http.NewRequest("POST", proxy.URL+"/v1/responses", strings.NewReader(reqBody))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if len(upstream) != 1 || !strings.Contains(upstream[0], `"tools"`) {
		t.Fatalf("不在名单内应保持原生 tools:\n%v", upstream)
	}
}
