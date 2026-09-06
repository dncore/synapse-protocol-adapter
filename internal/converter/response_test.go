package converter

import (
	"testing"

	"github.com/dncore/synapse-protocol-adapter/internal/completions"
	"github.com/dncore/synapse-protocol-adapter/internal/responses"
)

func TestConvertResponse_Text(t *testing.T) {
	chat := &completions.Response{
		ID: "chatcmpl-1", Created: 1700000000, Model: "m",
		Choices: []completions.Choice{{Message: completions.Message{
			Role: "assistant", Content: completions.MessageContent{String: "hi there"},
		}, FinishReason: "stop"}},
		Usage: &completions.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
	}
	out := ConvertResponse(chat, &responses.Request{Model: "m"})
	if out.Object != "response" || out.Status != "completed" {
		t.Fatalf("status wrong: %s", out.Status)
	}
	if len(out.Output) != 1 || out.Output[0].Type != "message" {
		t.Fatalf("output wrong: %+v", out.Output)
	}
	if out.Output[0].Content.Parts[0].Text != "hi there" {
		t.Fatalf("text wrong")
	}
	if out.Usage.InputTokens != 10 || out.Usage.OutputTokens != 5 || out.Usage.TotalTokens != 15 {
		t.Fatalf("usage wrong: %+v", out.Usage)
	}
	if out.IncompleteDetails != nil {
		t.Fatalf("should be complete")
	}
}

func TestConvertResponse_ToolCalls(t *testing.T) {
	chat := &completions.Response{
		ID: "c1", Model: "m",
		Choices: []completions.Choice{{
			Message: completions.Message{
				Role: "assistant",
				ToolCalls: []completions.ToolCall{
					{ID: "call_a", Type: "function", Function: completions.FuncCall{Name: "f1", Arguments: `{"x":1}`}},
					{ID: "call_b", Type: "function", Function: completions.FuncCall{Name: "f2", Arguments: `{"y":2}`}},
				},
			},
			FinishReason: "tool_calls",
		}},
	}
	out := ConvertResponse(chat, &responses.Request{})
	if out.Status != "completed" {
		t.Fatalf("tool_calls finish must map to completed, got %s", out.Status)
	}
	if len(out.Output) != 2 {
		t.Fatalf("want 2 function_call items, got %d", len(out.Output))
	}
	for i, want := range []string{"call_a", "call_b"} {
		if out.Output[i].Type != "function_call" || out.Output[i].CallID != want {
			t.Fatalf("item %d wrong: %+v", i, out.Output[i])
		}
	}
}

func TestConvertResponse_FinishReasons(t *testing.T) {
	cases := []struct {
		finish  string
		status  string
		details string
	}{
		{"stop", "completed", ""},
		{"tool_calls", "completed", ""},
		{"function_call", "completed", ""},
		{"length", "incomplete", "max_output_tokens"},
		{"content_filter", "incomplete", "content_filter"},
	}
	for _, tc := range cases {
		chat := &completions.Response{
			ID: "c", Model: "m",
			Choices: []completions.Choice{{Message: completions.Message{
				Content: completions.MessageContent{String: "x"},
			}, FinishReason: tc.finish}},
		}
		out := ConvertResponse(chat, &responses.Request{})
		if out.Status != tc.status {
			t.Fatalf("finish %q: status %s want %s", tc.finish, out.Status, tc.status)
		}
		if tc.details != "" && (out.IncompleteDetails == nil || out.IncompleteDetails.Reason != tc.details) {
			t.Fatalf("finish %q: details %+v want %s", tc.finish, out.IncompleteDetails, tc.details)
		}
	}
}

func TestConvertResponse_NoChoices(t *testing.T) {
	chat := &completions.Response{ID: "c", Model: "m"}
	out := ConvertResponse(chat, &responses.Request{})
	if out.Status != "failed" || out.Output == nil {
		t.Fatalf("empty choices must yield failed with empty output, got %+v", out)
	}
}

func TestConvertResponse_EmptyContent(t *testing.T) {
	chat := &completions.Response{
		ID: "c", Model: "m",
		Choices: []completions.Choice{{Message: completions.Message{Role: "assistant"}, FinishReason: "stop"}},
	}
	out := ConvertResponse(chat, &responses.Request{})
	if len(out.Output) != 0 {
		t.Fatalf("empty content should produce no message item, got %+v", out.Output)
	}
}
