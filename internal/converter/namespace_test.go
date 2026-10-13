package converter

import (
	"testing"

	"github.com/dncore/synapse-protocol-adapter/internal/completions"
	"github.com/dncore/synapse-protocol-adapter/internal/responses"
)

// codexTools is the shape Codex CLI 0.162.1 sends: plain functions plus one
// namespace wrapper per MCP server (captured 2026-10-13).
func codexTools() []responses.Tool {
	return []responses.Tool{
		{Type: "function", Name: "exec_command", Description: "run a command"},
		{Type: "web_search", Name: ""},
		{
			Type: "namespace", Name: "mcp__chrome_devtools",
			Tools: []responses.Tool{
				{Type: "function", Name: "click", Description: "click an element"},
				{Type: "function", Name: "take_snapshot", Description: "snapshot the page"},
			},
		},
		{
			Type: "namespace", Name: "multi_agent_v1",
			Tools: []responses.Tool{
				{Type: "function", Name: "close_agent", Description: "close an agent"},
			},
		},
	}
}

// A namespace wrapper has no chat-completions representation, but dropping it
// wholesale left the model unable to call any MCP tool.
func TestConvertTools_FlattensNamespaces(t *testing.T) {
	out := convertTools(codexTools())
	var names []string
	for _, tl := range out {
		names = append(names, tl.Function.Name)
	}
	want := []string{
		"exec_command",
		"mcp__chrome_devtools__click",
		"mcp__chrome_devtools__take_snapshot",
		"multi_agent_v1__close_agent",
	}
	if len(names) != len(want) {
		t.Fatalf("tools = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("tools = %v, want %v", names, want)
		}
	}
	// The nested description must come along, or the model cannot tell what
	// the tool does.
	if out[1].Function.Description != "click an element" {
		t.Fatalf("nested description lost: %q", out[1].Function.Description)
	}
	for _, tl := range out {
		if tl.Type != "function" {
			t.Fatalf("forwarded tool type = %q, want function", tl.Type)
		}
	}
}

// A plain top-level tool wins its own name: flattening must not shadow it.
func TestConvertTools_NamespaceDoesNotShadowTopLevelName(t *testing.T) {
	tools := []responses.Tool{
		{Type: "function", Name: "mcp__x__run"},
		{Type: "namespace", Name: "mcp__x", Tools: []responses.Tool{{Type: "function", Name: "run"}}},
	}
	idx := namespaceIndex(tools)
	if _, ok := idx["mcp__x__run"]; ok {
		t.Fatal("a flat name must not shadow an identical top-level function")
	}
	if name, ns := resolveCallName(idx, "mcp__x__run"); name != "mcp__x__run" || ns != "" {
		t.Fatalf("shadowed name was rewritten: name=%q ns=%q", name, ns)
	}
}

// The round trip Codex actually resolves (measured 2026-10-13: the flattened
// spelling is rejected with "unsupported call"; the bare name plus the
// wrapper is dispatched to the MCP server).
func TestResolveCallName_RoundTrip(t *testing.T) {
	idx := namespaceIndex(codexTools())
	name, ns := resolveCallName(idx, "mcp__chrome_devtools__take_snapshot")
	if name != "take_snapshot" || ns != "mcp__chrome_devtools" {
		t.Fatalf("resolved to name=%q ns=%q, want take_snapshot / mcp__chrome_devtools", name, ns)
	}
	if name, ns := resolveCallName(idx, "exec_command"); name != "exec_command" || ns != "" {
		t.Fatalf("a plain tool must pass through: name=%q ns=%q", name, ns)
	}
}

// A replayed call comes back the way the client stores it; the model only ever
// knew the flat spelling.
func TestHistoryCallName(t *testing.T) {
	idx := namespaceIndex(codexTools())
	item := &responses.Item{Type: "function_call", Name: "take_snapshot", Namespace: "mcp__chrome_devtools"}
	if got := historyCallName(idx, item); got != "mcp__chrome_devtools__take_snapshot" {
		t.Fatalf("history replay = %q, want the flat name", got)
	}
	plain := &responses.Item{Type: "function_call", Name: "exec_command"}
	if got := historyCallName(idx, plain); got != "exec_command" {
		t.Fatalf("plain history call = %q, want unchanged", got)
	}
}

// End to end, non-streaming: the model answers with the flat name and the
// client must receive the pair it dispatches on.
func TestConvertResponse_NamespacedToolCall(t *testing.T) {
	req := &responses.Request{Model: "gpt-6-luna", Tools: codexTools()}
	chat := &completions.Response{
		ID: "chatcmpl-1",
		Choices: []completions.Choice{{
			Index: 0,
			Message: completions.Message{
				Role: "assistant",
				ToolCalls: []completions.ToolCall{{
					ID:       "call_1",
					Type:     "function",
					Function: completions.FuncCall{Name: "mcp__chrome_devtools__click", Arguments: `{"uid":"1_1"}`},
				}},
			},
			FinishReason: "tool_calls",
		}},
	}
	out := ConvertResponse(chat, req)
	if len(out.Output) != 1 {
		t.Fatalf("output = %+v", out.Output)
	}
	got := out.Output[0]
	if got.Name != "click" || got.Namespace != "mcp__chrome_devtools" {
		t.Fatalf("name=%q namespace=%q, want click / mcp__chrome_devtools", got.Name, got.Namespace)
	}
}

// The same, streamed: every event that carries the name must agree, including
// the aggregated output on the terminal event.
func TestStreamer_NamespacedToolCall(t *testing.T) {
	req := &responses.Request{Model: "gpt-6-luna", Tools: codexTools()}
	s := NewStreamer(req)
	idx := 0

	first := &completions.StreamChunk{Choices: []completions.StreamChoice{{
		Delta: completions.Delta{Role: "assistant", ToolCalls: []completions.ToolCall{{
			Index: &idx, ID: "call_1", Type: "function",
			Function: completions.FuncCall{Name: "multi_agent_v1__close_agent"},
		}}},
	}}}
	var added *responses.Item
	for _, ev := range s.Feed(first) {
		if ev.Type == "response.output_item.added" && ev.Item != nil {
			added = ev.Item
		}
	}
	if added == nil {
		t.Fatal("no output_item.added emitted")
	}
	if added.Name != "close_agent" || added.Namespace != "multi_agent_v1" {
		t.Fatalf("added item: name=%q namespace=%q, want close_agent / multi_agent_v1", added.Name, added.Namespace)
	}

	args := `{"id":"a1"}`
	stop := "tool_calls"
	s.Feed(&completions.StreamChunk{Choices: []completions.StreamChoice{{
		Delta:        completions.Delta{ToolCalls: []completions.ToolCall{{Index: &idx, Function: completions.FuncCall{Arguments: args}}}},
		FinishReason: &stop,
	}}})

	var terminal *responses.Response
	for _, ev := range s.Finish() {
		if ev.Response != nil {
			terminal = ev.Response
		}
	}
	if terminal == nil || len(terminal.Output) != 1 {
		t.Fatalf("terminal output = %+v", terminal)
	}
	if terminal.Output[0].Name != "close_agent" || terminal.Output[0].Namespace != "multi_agent_v1" {
		t.Fatalf("terminal item: name=%q namespace=%q", terminal.Output[0].Name, terminal.Output[0].Namespace)
	}
}
