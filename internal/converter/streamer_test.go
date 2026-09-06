package converter

import (
	"strings"
	"testing"

	"github.com/dncore/synapse-protocol-adapter/internal/completions"
	"github.com/dncore/synapse-protocol-adapter/internal/responses"
)

// typesOf collects the event type sequence.
func typesOf(evs []*responses.Event) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = e.Type
	}
	return out
}

func strPtr(s string) *string { return &s }
func idxPtr(i int) *int       { return &i }

func TestStreamer_TextFlow(t *testing.T) {
	s := NewStreamer(&responses.Request{Model: "m"})

	ev1 := s.Feed(&completions.StreamChunk{ID: "c1", Created: 123, Model: "m",
		Choices: []completions.StreamChoice{{Delta: completions.Delta{Role: "assistant", Content: "hel"}}}})
	if got := typesOf(ev1); strings.Join(got, ",") != "response.created,response.in_progress,response.output_item.added,response.content_part.added,response.output_text.delta" {
		t.Fatalf("first chunk events: %v", got)
	}

	ev2 := s.Feed(&completions.StreamChunk{
		Choices: []completions.StreamChoice{{Delta: completions.Delta{Content: "lo"}}}})
	if got := typesOf(ev2); strings.Join(got, ",") != "response.output_text.delta" {
		t.Fatalf("second chunk events: %v", got)
	}

	fin := s.Feed(&completions.StreamChunk{
		Choices: []completions.StreamChoice{{Delta: completions.Delta{}, FinishReason: strPtr("stop")}}})
	if got := typesOf(fin); strings.Join(got, ",") != "response.output_text.done,response.content_part.done,response.output_item.done" {
		t.Fatalf("finish events: %v", got)
	}

	done := s.Finish()
	if got := typesOf(done); got[0] != "response.completed" || len(done) != 1 {
		t.Fatalf("final events: %v", got)
	}
	final := s.FinalResponse()
	if final.Status != "completed" || final.ID != "c1" || final.Model != "m" {
		t.Fatalf("final response wrong: %+v", final)
	}
	if len(final.Output) != 1 || final.Output[0].Content.Parts[0].Text != "hello" {
		t.Fatalf("aggregated text wrong: %+v", final.Output)
	}

	// Sequence numbers must be strictly increasing 1..N.
	all := append(append(append(ev1, ev2...), fin...), done...)
	for i, e := range all {
		if e.SequenceNumber != int64(i+1) {
			t.Fatalf("seq at %d = %d", i, e.SequenceNumber)
		}
	}
}

func TestStreamer_ParallelToolCalls(t *testing.T) {
	s := NewStreamer(&responses.Request{Model: "m"})

	ev1 := s.Feed(&completions.StreamChunk{ID: "c1", Model: "m", Choices: []completions.StreamChoice{{
		Delta: completions.Delta{ToolCalls: []completions.ToolCall{
			{Index: idxPtr(0), ID: "call_a", Type: "function", Function: completions.FuncCall{Name: "f1"}},
		}},
	}}})
	if got := typesOf(ev1); got[len(got)-1] != "response.function_call_arguments.delta" && got[len(got)-1] != "response.output_item.added" {
		t.Fatalf("unexpected first tool events: %v", got)
	}

	// Second tool opens before the first completes (parallel).
	ev2 := s.Feed(&completions.StreamChunk{Choices: []completions.StreamChoice{{
		Delta: completions.Delta{ToolCalls: []completions.ToolCall{
			{Index: idxPtr(1), ID: "call_b", Type: "function", Function: completions.FuncCall{Name: "f2"}},
		}},
	}}})

	ev3 := s.Feed(&completions.StreamChunk{Choices: []completions.StreamChoice{{
		Delta: completions.Delta{ToolCalls: []completions.ToolCall{
			{Index: idxPtr(0), Function: completions.FuncCall{Arguments: `{"x":`}},
		}},
	}}})
	ev4 := s.Feed(&completions.StreamChunk{Choices: []completions.StreamChoice{{
		Delta: completions.Delta{ToolCalls: []completions.ToolCall{
			{Index: idxPtr(1), Function: completions.FuncCall{Arguments: `{"y":2}`}},
		}},
	}}})
	ev5 := s.Feed(&completions.StreamChunk{Choices: []completions.StreamChoice{{
		Delta: completions.Delta{ToolCalls: []completions.ToolCall{
			{Index: idxPtr(0), Function: completions.FuncCall{Arguments: `1}`}},
		}},
		FinishReason: strPtr("tool_calls"),
	}}})

	_ = ev2
	all := ev1
	all = append(all, ev2...)
	all = append(all, ev3...)
	all = append(all, ev4...)
	all = append(all, ev5...)
	joined := typesOf(all)

	// Two output_item.added (one per call) and a terminal close per call.
	added := count(joined, "response.output_item.added")
	doneArgs := count(joined, "response.function_call_arguments.done")
	doneItems := count(joined, "response.output_item.done")
	if added != 2 || doneArgs != 2 || doneItems != 2 {
		t.Fatalf("added=%d doneArgs=%d doneItems=%d; events=%v", added, doneArgs, doneItems, joined)
	}

	final := s.FinalResponse()
	if final == nil {
		final2 := s.Finish()
		_ = final2
		final = s.FinalResponse()
	}
	if len(final.Output) != 2 {
		t.Fatalf("want 2 aggregated tool calls, got %d", len(final.Output))
	}
	// Arguments must be accumulated per index, interleaving-safe.
	if final.Output[0].Arguments != `{"x":1}` || final.Output[1].Arguments != `{"y":2}` {
		t.Fatalf("argument accumulation wrong: %+v", final.Output)
	}
	if final.Output[0].CallID != "call_a" || final.Output[1].CallID != "call_b" {
		t.Fatalf("call ids wrong: %+v", final.Output)
	}
}

func TestStreamer_UsageChunkWithoutChoices(t *testing.T) {
	s := NewStreamer(&responses.Request{Model: "m"})
	s.Feed(&completions.StreamChunk{ID: "c", Model: "m", Choices: []completions.StreamChoice{{
		Delta: completions.Delta{Content: "x"}, FinishReason: strPtr("stop"),
	}}})
	// The trailing usage chunk has empty choices and must not emit anything
	// or prematurely finish.
	ev := s.Feed(&completions.StreamChunk{
		Choices: []completions.StreamChoice{},
		Usage:   &completions.Usage{PromptTokens: 3, CompletionTokens: 2, TotalTokens: 5},
	})
	if len(ev) != 0 {
		t.Fatalf("usage chunk must emit no events: %v", typesOf(ev))
	}
	s.Finish()
	if u := s.FinalResponse().Usage; u == nil || u.InputTokens != 3 || u.OutputTokens != 2 {
		t.Fatalf("usage not carried to final response: %+v", u)
	}
}

func TestStreamer_UpstreamEndsWithoutFinishReason(t *testing.T) {
	s := NewStreamer(&responses.Request{Model: "m"})
	s.Feed(&completions.StreamChunk{ID: "c", Model: "m", Choices: []completions.StreamChoice{{
		Delta: completions.Delta{Content: "partial"},
	}}})
	evs := s.Finish()
	joined := strings.Join(typesOf(evs), ",")
	if !strings.Contains(joined, "response.output_item.done") || !strings.Contains(joined, "response.completed") {
		t.Fatalf("graceful close missing: %v", joined)
	}
	if s.FinalResponse().Output[0].Content.Parts[0].Text != "partial" {
		t.Fatalf("text lost on graceful close")
	}
}

func TestStreamer_ChunksAfterFinishIgnored(t *testing.T) {
	s := NewStreamer(&responses.Request{Model: "m"})
	s.Feed(&completions.StreamChunk{ID: "c", Model: "m", Choices: []completions.StreamChoice{{
		Delta: completions.Delta{Content: "x"}, FinishReason: strPtr("stop"),
	}}})
	s.Finish()
	ev := s.Feed(&completions.StreamChunk{Choices: []completions.StreamChoice{{
		Delta: completions.Delta{Content: "late"},
	}}})
	if len(ev) != 0 {
		t.Fatalf("events after finish must be suppressed: %v", typesOf(ev))
	}
}

func count(list []string, s string) int {
	n := 0
	for _, v := range list {
		if v == s {
			n++
		}
	}
	return n
}
