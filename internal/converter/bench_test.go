package converter

import (
	"encoding/json"
	"testing"

	"github.com/dncore/synapse-protocol-adapter/internal/completions"
	"github.com/dncore/synapse-protocol-adapter/internal/responses"
)

func benchRequest(nTools int) *responses.Request {
	tools := make([]responses.Tool, nTools)
	for i := range tools {
		tools[i] = responses.Tool{
			Type: "function", Name: "tool_" + string(rune('a'+i%26)),
			Parameters: json.RawMessage(`{"type":"object","properties":{"x":{"type":"string"}}}`),
		}
	}
	return &responses.Request{
		Model: "bench",
		Input: responses.Input{Items: []responses.Item{
			{Type: "message", Role: "system", Content: responses.ItemContent{String: "sys prompt"}},
			{Type: "message", Role: "user", Content: responses.ItemContent{String: "question"}},
			{Type: "function_call", CallID: "call_1", Name: "tool_a", Arguments: `{"x":"1"}`},
			{Type: "function_call_output", CallID: "call_1", Output: `{"ok":true}`},
		}},
		Tools: tools,
	}
}

func BenchmarkConvertRequest(b *testing.B) {
	req := benchRequest(10)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := ConvertRequest(req); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkConvertResponse(b *testing.B) {
	chat := &completions.Response{
		ID: "c", Model: "m",
		Choices: []completions.Choice{{Message: completions.Message{
			Role:    "assistant",
			Content: completions.MessageContent{String: "answer"},
		}, FinishReason: "stop"}},
	}
	req := benchRequest(0)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = ConvertResponse(chat, req)
	}
}

// BenchmarkStreamer simulates a realistic stream: role chunk, N text
// deltas, finish chunk, usage chunk.
func BenchmarkStreamer(b *testing.B) {
	const deltas = 200
	chunks := make([]*completions.StreamChunk, 0, deltas+3)
	chunks = append(chunks, &completions.StreamChunk{ID: "c", Model: "m",
		Choices: []completions.StreamChoice{{Delta: completions.Delta{Role: "assistant", Content: "a"}}}})
	for i := 0; i < deltas; i++ {
		chunks = append(chunks, &completions.StreamChunk{
			Choices: []completions.StreamChoice{{Delta: completions.Delta{Content: " token"}}}})
	}
	fr := "stop"
	chunks = append(chunks, &completions.StreamChunk{
		Choices: []completions.StreamChoice{{Delta: completions.Delta{}, FinishReason: &fr}}})
	chunks = append(chunks, &completions.StreamChunk{
		Usage: &completions.Usage{PromptTokens: 100, CompletionTokens: deltas, TotalTokens: 100 + deltas}})

	req := benchRequest(0)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s := NewStreamer(req)
		for _, c := range chunks {
			s.Feed(c)
		}
		s.Finish()
	}
}
