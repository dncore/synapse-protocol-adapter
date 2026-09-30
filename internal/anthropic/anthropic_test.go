package anthropic

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/dncore/synapse-protocol-adapter/internal/completions"
)

// The request bodies are the shapes Claude Code actually sends (captured from
// the real client on 2026-09-30): system as a block array, text/image/
// tool_use/tool_result blocks, system-role hook turns inside messages, 12-ish
// tools with input_schema, max_tokens, thinking=adaptive, output_config.
func TestConvertRequest(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantErr bool
		check   func(t *testing.T, out *completions.Request)
	}{
		{
			name: "claude code turn: system blocks, tools, max_tokens, thinking",
			body: `{
				"model": "gpt-6-luna",
				"max_tokens": 32000,
				"stream": true,
				"thinking": {"type": "adaptive", "display": "omitted"},
				"output_config": {"effort": "xhigh"},
				"system": [{"type": "text", "text": "You are a coding agent.", "cache_control": {"type": "ephemeral"}},
				           {"type": "text", "text": "Be concise."}],
				"tools": [{"name": "Read", "description": "read a file", "input_schema": {"type": "object", "properties": {"file_path": {"type": "string"}}, "required": ["file_path"]}}],
				"messages": [{"role": "user", "content": [{"type": "text", "text": "hi"}]}]
			}`,
			check: func(t *testing.T, out *completions.Request) {
				if len(out.Messages) != 2 || out.Messages[0].Role != "system" {
					t.Fatalf("system prompt must fold into one leading system message, got %+v", out.Messages)
				}
				if out.Messages[0].Content.String != "You are a coding agent.\n\nBe concise." {
					t.Fatalf("system blocks must join with a blank line, got %q", out.Messages[0].Content.String)
				}
				// The family dialect: max_completion_tokens + reasoning off.
				if out.MaxCompletionTokens == nil || *out.MaxCompletionTokens != 32000 || out.MaxTokens != nil {
					t.Fatalf("gpt-6 must send max_completion_tokens only, got max_tokens=%v max_completion_tokens=%v", out.MaxTokens, out.MaxCompletionTokens)
				}
				if out.ReasoningEffort != "none" {
					t.Fatalf("tools must force reasoning_effort=none, got %q", out.ReasoningEffort)
				}
				if len(out.Tools) != 1 || out.Tools[0].Function.Name != "Read" {
					t.Fatalf("tools must map to nested functions, got %+v", out.Tools)
				}
				if !strings.Contains(string(out.Tools[0].Function.Parameters), `"file_path"`) {
					t.Fatalf("input_schema must become the function parameters, got %s", out.Tools[0].Function.Parameters)
				}
				if out.StreamOptions == nil || !out.StreamOptions.IncludeUsage {
					t.Fatal("streamed requests must ask upstream for usage")
				}
			},
		},
		{
			name: "tool round trip: assistant tool_use then user tool_result",
			body: `{"model": "gpt-6-luna", "max_tokens": 100, "tools": [{"name": "Read", "input_schema": {"type": "object"}}],
				"messages": [
					{"role": "user", "content": "read it"},
					{"role": "assistant", "content": [{"type": "text", "text": "reading"}, {"type": "tool_use", "id": "toolu_1", "name": "Read", "input": {"file_path": "/etc/hostname"}}]},
					{"role": "user", "content": [{"type": "tool_result", "tool_use_id": "toolu_1", "is_error": false, "content": "inwin"}]}
				]}`,
			check: func(t *testing.T, out *completions.Request) {
				var assistant *completions.Message
				var tool *completions.Message
				for i := range out.Messages {
					switch out.Messages[i].Role {
					case "assistant":
						assistant = &out.Messages[i]
					case "tool":
						tool = &out.Messages[i]
					}
				}
				if assistant == nil || len(assistant.ToolCalls) != 1 {
					t.Fatalf("assistant tool_use must become one tool_calls entry, got %+v", assistant)
				}
				call := assistant.ToolCalls[0]
				if call.ID != "toolu_1" || call.Function.Name != "Read" {
					t.Fatalf("tool call mapped wrong: %+v", call)
				}
				// The client's own JSON bytes are forwarded verbatim (whitespace
				// included); compare semantically.
				var args map[string]any
				if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
					t.Fatalf("tool call arguments are not JSON: %q", call.Function.Arguments)
				}
				if args["file_path"] != "/etc/hostname" {
					t.Fatalf("tool call arguments lost data: %+v", args)
				}
				if tool == nil || tool.ToolCallID != "toolu_1" || tool.Content.String != "inwin" {
					t.Fatalf("tool_result must become a tool message, got %+v", tool)
				}
			},
		},
		{
			name: "parallel tool_use blocks merge into one assistant message",
			body: `{"model": "gpt-6-luna", "tools": [{"name": "Read", "input_schema": {"type": "object"}}],
				"messages": [{"role": "assistant", "content": [
					{"type": "tool_use", "id": "a", "name": "Read", "input": {}},
					{"type": "tool_use", "id": "b", "name": "Read", "input": {}}]}]}`,
			check: func(t *testing.T, out *completions.Request) {
				if len(out.Messages) != 1 || len(out.Messages[0].ToolCalls) != 2 {
					t.Fatalf("parallel calls must merge, got %+v", out.Messages)
				}
			},
		},
		{
			name: "claude code system-role hook turn inside messages is kept",
			body: `{"model": "gpt-6-luna", "tools": [{"name": "Read", "input_schema": {"type": "object"}}],
				"messages": [{"role": "user", "content": "hi"}, {"role": "system", "content": [{"type": "text", "text": "SessionStart hook ok"}]}]}`,
			check: func(t *testing.T, out *completions.Request) {
				if len(out.Messages) != 2 || out.Messages[1].Role != "system" || out.Messages[1].Content.String != "SessionStart hook ok" {
					t.Fatalf("hook turn must survive as a system message, got %+v", out.Messages)
				}
			},
		},
		{
			name: "image block becomes an image_url part",
			body: `{"model": "gpt-6-luna", "tools": [{"name": "Read", "input_schema": {"type": "object"}}],
				"messages": [{"role": "user", "content": [{"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "AAA"}}]}]}`,
			check: func(t *testing.T, out *completions.Request) {
				parts := out.Messages[0].Content.Parts
				if len(parts) != 1 || parts[0].ImageURL == nil || parts[0].ImageURL.URL != "data:image/png;base64,AAA" {
					t.Fatalf("image must map to a data URI part, got %+v", parts)
				}
			},
		},
		{
			name: "tool_choice mappings",
			body: `{"model": "gpt-6-luna", "max_tokens": 10, "tools": [{"name": "Read", "input_schema": {"type": "object"}}],
				"tool_choice": {"type": "tool", "name": "Read"}, "messages": [{"role": "user", "content": "hi"}]}`,
			check: func(t *testing.T, out *completions.Request) {
				if !strings.Contains(string(out.ToolChoice), `"name":"Read"`) {
					t.Fatalf("named tool_choice must map to the chat function form, got %s", out.ToolChoice)
				}
			},
		},
		{
			name: "thinking history blocks are dropped rather than failing",
			body: `{"model": "gpt-6-luna", "tools": [{"name": "Read", "input_schema": {"type": "object"}}],
				"messages": [{"role": "assistant", "content": [{"type": "thinking", "thinking": "…"}, {"type": "text", "text": "done"}]}]}`,
			check: func(t *testing.T, out *completions.Request) {
				if len(out.Messages) != 1 || out.Messages[0].Content.String != "done" {
					t.Fatalf("thinking blocks must be skipped, got %+v", out.Messages)
				}
			},
		},
		{
			name:    "unknown content block is unsupported (caller falls back)",
			body:    `{"model": "gpt-6-luna", "tools": [{"name": "Read", "input_schema": {"type": "object"}}], "messages": [{"role": "user", "content": [{"type": "document", "source": {}}]}]}`,
			wantErr: true,
		},
		{
			name:    "unknown role is unsupported",
			body:    `{"model": "gpt-6-luna", "tools": [{"name": "Read", "input_schema": {"type": "object"}}], "messages": [{"role": "tool", "content": "x"}]}`,
			wantErr: true,
		},
		{
			name:    "tool without name is unsupported",
			body:    `{"model": "gpt-6-luna", "tools": [{"input_schema": {"type": "object"}}], "messages": [{"role": "user", "content": "hi"}]}`,
			wantErr: true,
		},
		{
			name:    "empty messages is unsupported",
			body:    `{"model": "gpt-6-luna", "tools": [{"name": "Read", "input_schema": {"type": "object"}}], "messages": []}`,
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var req Request
			if err := json.Unmarshal([]byte(tc.body), &req); err != nil {
				t.Fatalf("test body is not valid JSON: %v", err)
			}
			out, err := ConvertRequest(&req)
			if tc.wantErr {
				if !errors.Is(err, ErrUnsupported) {
					t.Fatalf("want ErrUnsupported, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.check != nil {
				tc.check(t, out)
			}
		})
	}
}

// A model outside the family keeps the client's own field names; only the
// gpt-6 dialect renames them.
func TestConvertRequest_NonFamilyKeepsMaxTokens(t *testing.T) {
	var req Request
	if err := json.Unmarshal([]byte(`{"model": "claude-sonnet-5", "max_tokens": 100, "messages": [{"role": "user", "content": "hi"}]}`), &req); err != nil {
		t.Fatal(err)
	}
	out, err := ConvertRequest(&req)
	if err != nil {
		t.Fatal(err)
	}
	if out.MaxTokens == nil || *out.MaxTokens != 100 || out.MaxCompletionTokens != nil {
		t.Fatalf("non-family models keep max_tokens, got %v/%v", out.MaxTokens, out.MaxCompletionTokens)
	}
}

func TestConvertResponse(t *testing.T) {
	chat := &completions.Response{
		ID:    "chatcmpl-1",
		Model: "openai/gpt-6-luna",
		Choices: []completions.Choice{{
			Message: completions.Message{
				Role:    "assistant",
				Content: completions.MessageContent{String: "reading it"},
				ToolCalls: []completions.ToolCall{
					{ID: "call_1", Type: "function", Function: completions.FuncCall{Name: "Read", Arguments: `{"file_path":"/etc/hostname"}`}},
				},
			},
			FinishReason: "tool_calls",
		}},
		Usage: &completions.Usage{PromptTokens: 11, CompletionTokens: 7},
	}
	out := ConvertResponse(chat, "gpt-6-luna")
	if out.Type != "message" || out.Role != "assistant" || out.Model != "openai/gpt-6-luna" || !strings.HasPrefix(out.ID, "msg_") {
		t.Fatalf("envelope wrong: %+v", out)
	}
	if out.StopReason != "tool_use" {
		t.Fatalf("finish_reason tool_calls must map to tool_use, got %q", out.StopReason)
	}
	if len(out.Content) != 2 || out.Content[0].Type != "text" || out.Content[1].Type != "tool_use" {
		t.Fatalf("content blocks wrong: %+v", out.Content)
	}
	if string(out.Content[1].Input) != `{"file_path":"/etc/hostname"}` || out.Content[1].Name != "Read" || out.Content[1].ID != "call_1" {
		t.Fatalf("tool_use block wrong: %+v", out.Content[1])
	}
	if out.Usage.InputTokens != 11 || out.Usage.OutputTokens != 7 {
		t.Fatalf("usage must map 1:1, got %+v", out.Usage)
	}
}

func TestConvertResponse_StopReasonMapping(t *testing.T) {
	tests := []struct {
		finish       string
		withToolCall bool
		want         string
	}{
		{finish: "stop", want: "end_turn"},
		{finish: "length", want: "max_tokens"},
		{finish: "tool_calls", withToolCall: true, want: "tool_use"},
		{finish: "function_call", withToolCall: true, want: "tool_use"},
		{finish: "content_filter", want: "refusal"},
		{finish: "", want: "end_turn"},
	}
	for _, tc := range tests {
		msg := completions.Message{Content: completions.MessageContent{String: "x"}}
		if tc.withToolCall {
			msg.ToolCalls = []completions.ToolCall{{ID: "call_1", Type: "function", Function: completions.FuncCall{Name: "Read", Arguments: "{}"}}}
		}
		chat := &completions.Response{Choices: []completions.Choice{{
			Message:      msg,
			FinishReason: tc.finish,
		}}}
		if got := ConvertResponse(chat, "gpt-6-luna").StopReason; got != tc.want {
			t.Fatalf("finish_reason %q → %q, want %q", tc.finish, got, tc.want)
		}
	}
}

// tool_calls announced without an actual call must not claim tool_use.
func TestConvertResponse_ToolUseWithoutToolCalls(t *testing.T) {
	chat := &completions.Response{Choices: []completions.Choice{{
		Message:      completions.Message{Content: completions.MessageContent{String: "done"}},
		FinishReason: "tool_calls",
	}}}
	if got := ConvertResponse(chat, "gpt-6-luna").StopReason; got != "end_turn" {
		t.Fatalf("stop_reason = %q, want end_turn", got)
	}
}

func TestStreamerEventSequence(t *testing.T) {
	s := NewStreamer("gpt-6-luna")
	var types []string
	emit := func(evs []Event) {
		for _, e := range evs {
			types = append(types, e.Type)
		}
	}

	finish := "tool_calls"
	toolChunk := func(call completions.ToolCall) *completions.StreamChunk {
		return &completions.StreamChunk{Choices: []completions.StreamChoice{{Delta: completions.Delta{ToolCalls: []completions.ToolCall{call}}}}}
	}
	idx := 0

	emit(s.Start("chatcmpl-1", "openai/gpt-6-luna"))
	emit(s.Chunk(&completions.StreamChunk{Choices: []completions.StreamChoice{{Delta: completions.Delta{Role: "assistant"}}}}))
	emit(s.Chunk(&completions.StreamChunk{Choices: []completions.StreamChoice{{Delta: completions.Delta{Content: "let me read"}}}}))
	// Arguments may arrive before the name: the block opens once the name lands.
	emit(s.Chunk(toolChunk(completions.ToolCall{Index: &idx, ID: "call_1", Function: completions.FuncCall{Arguments: `{"file_`}})))
	emit(s.Chunk(toolChunk(completions.ToolCall{Index: &idx, Function: completions.FuncCall{Name: "Read", Arguments: `path":"/etc/hostname"}`}})))
	emit(s.Chunk(&completions.StreamChunk{Choices: []completions.StreamChoice{{FinishReason: &finish}}, Usage: &completions.Usage{PromptTokens: 9, CompletionTokens: 4}}))
	emit(s.Finish())

	want := []string{
		EventMessageStart,
		EventContentBlockStart, EventContentBlockDelta, // text
		EventContentBlockStop,                                                 // text closes when the tool block opens
		EventContentBlockStart, EventContentBlockDelta, EventContentBlockStop, // tool_use
		EventMessageDelta, EventMessageStop,
	}
	if strings.Join(types, ",") != strings.Join(want, ",") {
		t.Fatalf("event sequence:\n got %v\nwant %v", types, want)
	}
}

// The terminal message_delta must carry the upstream usage and the mapped
// stop reason, and the tool block must stream its arguments as partial JSON.
func TestStreamerPayloads(t *testing.T) {
	s := NewStreamer("gpt-6-luna")
	s.Start("", "gpt-6-luna")
	idx := 0
	s.Chunk(&completions.StreamChunk{Choices: []completions.StreamChoice{{Delta: completions.Delta{
		ToolCalls: []completions.ToolCall{{Index: &idx, ID: "call_1", Function: completions.FuncCall{Name: "Read", Arguments: `{"a":1}`}}},
	}}}})
	finish := "tool_calls"
	s.Chunk(&completions.StreamChunk{Choices: []completions.StreamChoice{{FinishReason: &finish}}, Usage: &completions.Usage{PromptTokens: 12, CompletionTokens: 5}})
	events := s.Finish()

	last := events[len(events)-2]
	if last.Type != EventMessageDelta {
		t.Fatalf("penultimate event must be message_delta, got %s", last.Type)
	}
	payload := last.Data.(map[string]any)
	if payload["delta"].(map[string]any)["stop_reason"] != "tool_use" {
		t.Fatalf("stop_reason must be tool_use, got %+v", payload)
	}
	usage := payload["usage"].(map[string]any)
	if usage["output_tokens"] != 5 || usage["input_tokens"] != 12 {
		t.Fatalf("usage must be forwarded, got %+v", usage)
	}
}

// An in-band upstream error ends the stream with an Anthropic error event and
// no message_stop.
func TestStreamerInBandError(t *testing.T) {
	s := NewStreamer("gpt-6-luna")
	s.Start("", "gpt-6-luna")
	events := s.Chunk(&completions.StreamChunk{Error: &completions.ErrorBody{Message: "quota exceeded", Type: "quota"}})
	if len(events) != 1 || events[0].Type != EventError {
		t.Fatalf("want one error event, got %+v", events)
	}
	if more := s.Finish(); len(more) != 0 {
		t.Fatalf("a failed stream must not emit terminal events, got %+v", more)
	}
}
