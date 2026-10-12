package converter

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/dncore/synapse-protocol-adapter/internal/completions"
	"github.com/dncore/synapse-protocol-adapter/internal/responses"
)

func toolRequest() *completions.Request {
	return &completions.Request{
		Model: "gpt-6.1-sol",
		Tools: []completions.Tool{{
			Type: "function",
			Function: completions.Function{
				Name:        "get_time",
				Description: "Get the current time in a city",
				Parameters:  json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`),
			},
		}},
		Messages: []completions.Message{
			{Role: "user", Content: completions.MessageContent{String: "Call get_time for Shanghai."}},
			{Role: "assistant", Content: completions.MessageContent{}, ToolCalls: []completions.ToolCall{{
				ID: "call_1", Type: "function",
				Function: completions.FuncCall{Name: "get_time", Arguments: `{"city":"Shanghai"}`},
			}}},
			{Role: "tool", ToolCallID: "call_1", Content: completions.MessageContent{String: "09:55 CST"}},
			{Role: "user", Content: completions.MessageContent{String: "Answer in one sentence."}},
		},
	}
}

// 协议改写:tools 进提示词、工具历史变纯文本、tools 字段清空 ——
// 上游那条渠道不认 tools,但认得这些。
func TestEmulateToolProtocol(t *testing.T) {
	req := toolRequest()
	if !EmulateToolProtocol(req) {
		t.Fatal("应改写成功")
	}
	if len(req.Tools) != 0 {
		t.Fatalf("tools 字段必须清空(上游不认): %+v", req.Tools)
	}
	if req.Messages[0].Role != "system" {
		t.Fatalf("协议应作为首条 system 消息: %+v", req.Messages[0])
	}
	sys := req.Messages[0].Content.String
	for _, want := range []string{"tool_calls", "get_time", "Get the current time in a city", "[tool result]"} {
		if !strings.Contains(sys, want) {
			t.Fatalf("协议文本缺 %q:\n%s", want, sys)
		}
	}
	// assistant 的 tool_calls 变成协议语法的文本
	if got := req.Messages[2]; got.Role != "assistant" || !strings.Contains(got.Content.String, `"name":"get_time"`) {
		t.Fatalf("assistant 工具调用应改写成文本: %+v", got)
	}
	// tool 结果变成带工具名的 user 消息
	if got := req.Messages[3]; got.Role != "user" || !strings.Contains(got.Content.String, "get_time") || !strings.Contains(got.Content.String, "09:55 CST") {
		t.Fatalf("工具结果应改写成 user 文本并带上工具名: %+v", got)
	}
	// 没有 tools 的请求不改写
	plain := &completions.Request{Model: "m", Messages: []completions.Message{{Role: "user"}}}
	if EmulateToolProtocol(plain) {
		t.Fatal("无 tools 不该改写")
	}
}

// 既有 system 消息:协议追加,而不是插两条 system。
func TestEmulateToolProtocolAppendsToSystem(t *testing.T) {
	req := toolRequest()
	req.Messages = append([]completions.Message{{
		Role: "system", Content: completions.MessageContent{String: "You are a coding agent."},
	}}, req.Messages...)
	if !EmulateToolProtocol(req) {
		t.Fatal("应改写成功")
	}
	if n := strings.Count(strings.Join([]string{req.Messages[0].Content.String}, ""), "You are a coding agent."); n != 1 {
		t.Fatalf("原 system 内容应保留: %q", req.Messages[0].Content.String)
	}
	if !strings.Contains(req.Messages[0].Content.String, "tool_calls") {
		t.Fatal("协议应追加到既有 system 消息里")
	}
	if len(req.Messages) != 5 { // 没有多插一条 system
		t.Fatalf("消息条数 = %d, want 5", len(req.Messages))
	}
}

// Codex 的消息内容是 parts 数组(String 为空),而 MessageContent 的序列化
// 在 Parts 非 nil 时只写 parts —— 协议必须追加进 parts,否则会被遮蔽,等于
// 根本没发给模型(2026-10-12 真实 Codex 实测缺陷:tools 已清空、协议没到,
// 模型自称「没有 shell 工具」)。
func TestEmulateToolProtocolAppendsToPartsContent(t *testing.T) {
	req := toolRequest()
	req.Messages = append([]completions.Message{{
		Role: "system",
		Content: completions.MessageContent{Parts: []completions.ContentPart{
			{Type: "text", Text: "You are Codex."},
			{Type: "text", Text: "Sandbox: read-only."},
		}},
	}}, req.Messages...)
	req.Messages[1] = completions.Message{Role: "user", Content: completions.MessageContent{
		Parts: []completions.ContentPart{{Type: "text", Text: "Call get_time for Shanghai."}},
	}}
	if !EmulateToolProtocol(req) {
		t.Fatal("应改写成功")
	}
	raw, err := json.Marshal(req.Messages)
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	for _, want := range []string{"You are Codex.", "Sandbox: read-only.", "You have tools available", "get_time"} {
		if !strings.Contains(body, want) {
			t.Fatalf("最终消息体缺 %q(协议被 parts 遮蔽或原内容被覆盖):\n%s", want, body)
		}
	}
}

// 解析:模型常带一句多余的话(甚至代码围栏),都要能取出来。
func TestParseEmulatedToolCall(t *testing.T) {
	cases := []struct {
		name, in string
		ok       bool
		wantArgs string
	}{
		{"纯净 JSON", `{"tool_calls":[{"name":"get_time","arguments":{"city":"Shanghai"}}]}`, true, `{"city":"Shanghai"}`},
		{"前后带废话", "Sure!\n{\"tool_calls\":[{\"name\":\"get_time\",\"arguments\":{\"city\":\"Tokyo\"}}]}\n", true, `{"city":"Tokyo"}`},
		{"代码围栏", "```json\n{\"tool_calls\":[{\"name\":\"get_time\",\"arguments\":{}}]}\n```", true, `{}`},
		{"参数缺失", `{"tool_calls":[{"name":"get_time"}]}`, true, `{}`},
		{"纯文本回答", "It is 09:55 in Shanghai.", false, ""},
		{"名字为空", `{"tool_calls":[{"name":"","arguments":{}}]}`, false, ""},
		{"非法 JSON", `{"tool_calls":[{"name":"get_time","arguments":{"city":}}]}`, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name, args, ok := ParseEmulatedToolCall(tc.in)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v (input %q)", ok, tc.ok, tc.in)
			}
			if ok && (name != "get_time" || args != tc.wantArgs) {
				t.Fatalf("解析结果 = (%q, %q), want (get_time, %q)", name, args, tc.wantArgs)
			}
		})
	}
}

// 解析出来的调用要变成真正的 tool_calls,且 finish_reason 跟着改 ——
// 下游所有转换器只认这个形状。
func TestApplyEmulatedToolCall(t *testing.T) {
	resp := &completions.Response{ID: "chatcmpl-x", Choices: []completions.Choice{{
		Message:      completions.Message{Role: "assistant", Content: completions.MessageContent{String: `{"tool_calls":[]}`}},
		FinishReason: "stop",
	}}}
	if !ApplyEmulatedToolCall(resp, "call_emu_1", "get_time", `{"city":"Shanghai"}`) {
		t.Fatal("应应用成功")
	}
	msg := resp.Choices[0].Message
	if msg.Content.String != "" {
		t.Fatalf("正文应清空(它只是协议载荷): %q", msg.Content.String)
	}
	if len(msg.ToolCalls) != 1 || msg.ToolCalls[0].Function.Name != "get_time" || msg.ToolCalls[0].ID != "call_emu_1" {
		t.Fatalf("tool_calls 未写入: %+v", msg.ToolCalls)
	}
	if resp.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("finish_reason = %q", resp.Choices[0].FinishReason)
	}
}

// 关键链路:合成的 chat 分片必须能被既有 Responses 流式转换器吃下去,
// 并吐出 function_call 事件 —— 否则模拟路径在流式客户端上等于没做。
func TestSynthesizedChatSSEFeedsResponsesStreamer(t *testing.T) {
	chat := &completions.Response{
		ID: "chatcmpl-emu", Model: "gpt-6.1-sol",
		Choices: []completions.Choice{{
			Message: completions.Message{
				Role: "assistant",
				ToolCalls: []completions.ToolCall{{
					ID: "call_emu_1", Type: "function",
					Function: completions.FuncCall{Name: "get_time", Arguments: `{"city":"Shanghai"}`},
				}},
			},
			FinishReason: "tool_calls",
		}},
	}
	sse := string(SynthesizeChatSSE(chat))
	if !strings.Contains(sse, "data: [DONE]") {
		t.Fatalf("缺少结束标记:\n%s", sse)
	}

	req := &responses.Request{Model: "gpt-6.1-sol"}
	streamer := NewStreamer(req)
	var out strings.Builder
	for _, line := range strings.Split(sse, "\n\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			break
		}
		var chunk completions.StreamChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			t.Fatalf("合成分片不是合法 JSON: %v\n%s", err, payload)
		}
		for _, ev := range streamer.Feed(&chunk) {
			raw, _ := json.Marshal(ev)
			out.Write(raw)
		}
	}
	got := out.String()
	for _, want := range []string{"response.output_item.added", "function_call_arguments.delta", "get_time"} {
		if !strings.Contains(got, want) {
			t.Fatalf("Responses 事件流缺 %q:\n%s", want, got)
		}
	}
}
