package converter

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/dncore/synapse-protocol-adapter/internal/completions"
	"github.com/dncore/synapse-protocol-adapter/internal/responses"
)

// TestStreamer_WireShapes pins the exact JSON of the events Codex parses
// strictly. Each assertion here corresponds to a live-verified failure
// mode: a missing key or a string where an array is expected makes Codex
// silently drop the item, and every following delta then errors out
// ("OutputTextDelta without active item").
func TestStreamer_WireShapes(t *testing.T) {
	s := NewStreamer(&responses.Request{Model: "m"})
	var all []*responses.Event
	all = append(all, s.Feed(&completions.StreamChunk{ID: "c1", Created: 1, Model: "m",
		Choices: []completions.StreamChoice{{Delta: completions.Delta{ReasoningContent: "think"}}}})...)
	all = append(all, s.Feed(&completions.StreamChunk{
		Choices: []completions.StreamChoice{{Delta: completions.Delta{Content: "hi"}}}})...)
	all = append(all, s.Feed(&completions.StreamChunk{
		Choices: []completions.StreamChoice{{Delta: completions.Delta{ToolCalls: []completions.ToolCall{{
			Index: idxPtr(0), ID: "call_a", Type: "function",
			Function: completions.FuncCall{Name: "f", Arguments: `{"x":1}`},
		}}}, FinishReason: strPtr("tool_calls")}}})...)
	all = append(all, s.Finish()...)

	marshal := func(e *responses.Event) map[string]any {
		t.Helper()
		b, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	nth := func(typ string, n int) map[string]any {
		t.Helper()
		for _, e := range all {
			if e.Type != typ {
				continue
			}
			if n == 0 {
				return marshal(e)
			}
			n--
		}
		t.Fatalf("no %s event #%d", typ, n)
		return nil
	}
	item := func(ev map[string]any) map[string]any {
		t.Helper()
		it, ok := ev["item"].(map[string]any)
		if !ok {
			t.Fatalf("event has no item object: %v", ev)
		}
		return it
	}

	// The junk `Output` field must never appear on any event.
	for _, e := range all {
		if strings.Contains(string(mustJSON(t, e)), `"Output"`) {
			t.Fatalf("nonstandard Output field leaked: %s", mustJSON(t, e))
		}
	}

	// reasoning item: summary must be an empty array, content absent.
	ritem := item(nth("response.output_item.added", 0))
	if ritem["type"] != "reasoning" {
		t.Fatalf("first added item is %v", ritem["type"])
	}
	if sum, ok := ritem["summary"].([]any); !ok || len(sum) != 0 {
		t.Fatalf("reasoning summary must serialize as [], got %v", ritem["summary"])
	}
	if _, present := ritem["content"]; present {
		t.Fatalf("reasoning item must not carry content: %v", ritem)
	}

	// reasoning summary part.added: part + summary_index, no fake item.
	spa := nth("response.reasoning_summary_part.added", 0)
	if idx, ok := spa["summary_index"].(float64); !ok || idx != 0 {
		t.Fatalf("summary_index missing: %v", spa)
	}
	if _, present := spa["item"]; present {
		t.Fatalf("summary part.added must not carry item: %v", spa)
	}
	if part, _ := spa["part"].(map[string]any); part["type"] != "summary_text" {
		t.Fatalf("summary part wrong: %v", spa["part"])
	}

	// reasoning summary delta carries summary_index (Codex drops it
	// silently without the key).
	sd := nth("response.reasoning_summary_text.delta", 0)
	if _, ok := sd["summary_index"].(float64); !ok {
		t.Fatalf("summary delta missing summary_index: %v", sd)
	}

	// message item at added: content is [] — not a part whose empty text
	// key gets omitted (that exact shape broke Codex live).
	mitem := item(nth("response.output_item.added", 1))
	if mitem["type"] != "message" {
		t.Fatalf("second added item is %v", mitem["type"])
	}
	if c, ok := mitem["content"].([]any); !ok || len(c) != 0 {
		t.Fatalf("message content must serialize as [], got %v", mitem["content"])
	}

	// content_part.added: part with text and annotations present.
	cpa := nth("response.content_part.added", 0)
	part, _ := cpa["part"].(map[string]any)
	if part["type"] != "output_text" || part["text"] != "" {
		t.Fatalf("content part wrong: %v", cpa["part"])
	}
	if ann, ok := part["annotations"].([]any); !ok || len(ann) != 0 {
		t.Fatalf("annotations must serialize as []: %v", cpa["part"])
	}

	// function_call item: neither content nor summary keys, and the
	// arguments key present even while empty (Codex requires it).
	fitem := item(nth("response.output_item.added", 2))
	if fitem["type"] != "function_call" {
		t.Fatalf("third added item is %v", fitem["type"])
	}
	for _, key := range []string{"content", "summary"} {
		if _, present := fitem[key]; present {
			t.Fatalf("function_call must not carry %s: %v", key, fitem)
		}
	}
	if args, present := fitem["arguments"]; !present || args != "" {
		t.Fatalf("function_call at added must carry arguments:\"\", got %v", fitem)
	}

	// content_part.done and the message item.done carry the final text.
	cpd := nth("response.content_part.done", 0)
	if part, _ := cpd["part"].(map[string]any); part["text"] != "hi" {
		t.Fatalf("content part.done text wrong: %v", cpd["part"])
	}
	mdone := item(nth("response.output_item.done", 1)) // 0 = reasoning done
	if c, _ := mdone["content"].([]any); len(c) != 1 || c[0].(map[string]any)["text"] != "hi" {
		t.Fatalf("message item.done content wrong: %v", mdone)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
