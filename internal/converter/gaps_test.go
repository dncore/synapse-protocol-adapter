package converter

// Gap-closure tests: behaviors added after auditing the Responses API
// surface against comparable open-source adapters.

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/dncore/synapse-protocol-adapter/internal/completions"
	"github.com/dncore/synapse-protocol-adapter/internal/responses"
)

func TestStreamer_IncompleteOnLength(t *testing.T) {
	s := NewStreamer(&responses.Request{Model: "m"})
	s.Feed(&completions.StreamChunk{ID: "c1", Model: "m", Choices: []completions.StreamChoice{{
		Delta: completions.Delta{Content: "trunc"}, FinishReason: strPtr("length"),
	}}})
	evs := s.Finish()
	last := evs[len(evs)-1]
	if last.Type != "response.incomplete" {
		t.Fatalf("terminal event = %s, want response.incomplete", last.Type)
	}
	if last.Response.Status != "incomplete" || last.Response.IncompleteDetails == nil ||
		last.Response.IncompleteDetails.Reason != "max_output_tokens" {
		t.Fatalf("incomplete details wrong: %+v", last.Response)
	}
}

func TestStreamer_IncompleteOnContentFilter(t *testing.T) {
	s := NewStreamer(&responses.Request{Model: "m"})
	s.Feed(&completions.StreamChunk{ID: "c1", Model: "m", Choices: []completions.StreamChoice{{
		Delta: completions.Delta{}, FinishReason: strPtr("content_filter"),
	}}})
	evs := s.Finish()
	if evs[len(evs)-1].Type != "response.incomplete" {
		t.Fatalf("want response.incomplete")
	}
	if evs[len(evs)-1].Response.IncompleteDetails.Reason != "content_filter" {
		t.Fatalf("reason wrong")
	}
}

func TestStreamer_RefusalFlow(t *testing.T) {
	s := NewStreamer(&responses.Request{Model: "m"})
	var all []*responses.Event
	all = append(all, s.Feed(&completions.StreamChunk{ID: "c1", Model: "m", Choices: []completions.StreamChoice{{
		Delta: completions.Delta{Refusal: "can"},
	}}})...)
	if got := typesOf(all); !contains(got, "response.refusal.delta") {
		t.Fatalf("refusal delta missing: %v", got)
	}
	all = append(all, s.Feed(&completions.StreamChunk{Choices: []completions.StreamChoice{{
		Delta: completions.Delta{Refusal: "not"}, FinishReason: strPtr("stop"),
	}}})...)
	all = append(all, s.Finish()...)
	joined := strings.Join(typesOf(all), ",")
	if !strings.Contains(joined, "response.refusal.done") {
		t.Fatalf("refusal.done missing: %s", joined)
	}
	final := s.FinalResponse()
	if len(final.Output) != 1 {
		t.Fatalf("final output wrong: %+v", final.Output)
	}
	part := final.Output[0].Content.Parts[0]
	if part.Type != "refusal" || part.Refusal != "cannot" {
		t.Fatalf("refusal part wrong: %+v", part)
	}
}

func TestConvertResponse_Refusal(t *testing.T) {
	chat := &completions.Response{
		ID: "c1", Model: "m",
		Choices: []completions.Choice{{Message: completions.Message{
			Role: "assistant", Refusal: "policy",
		}, FinishReason: "stop"}},
	}
	out := ConvertResponse(chat, &responses.Request{})
	if len(out.Output) != 1 || out.Output[0].Content.Parts[0].Type != "refusal" ||
		out.Output[0].Content.Parts[0].Refusal != "policy" {
		t.Fatalf("refusal output wrong: %+v", out.Output)
	}
}

func TestConvertRequest_ToolOutputAsParts(t *testing.T) {
	req := &responses.Request{
		Model: "m",
		Input: responses.Input{Items: []responses.Item{
			{Type: "message", Role: "user", Content: responses.ItemContent{String: "hi"}},
			{Type: "function_call", CallID: "c1", Name: "f", Arguments: "{}"},
			{Type: "function_call_output", CallID: "c1",
				Output: responses.OutputContent{Parts: []responses.ContentPart{
					{Type: "output_text", Text: "{\"ok\":"},
					{Type: "output_text", Text: "true}"},
				}}},
		}},
	}
	out, err := ConvertRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	tool := out.Messages[2]
	if tool.Role != "tool" || len(tool.Content.Parts) != 2 || tool.Content.Parts[1].Text != "true}" {
		t.Fatalf("part-array tool output misconverted: %+v", tool.Content)
	}
}

func TestConvertRequest_ToolOutputJSONRoundTrip(t *testing.T) {
	// The wire form can be either a string or an array; both must parse.
	body := `{"model":"m","input":[
		{"type":"message","role":"user","content":"hi"},
		{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"},
		{"type":"function_call_output","call_id":"c1",
		 "output":[{"type":"output_text","text":"result"}]}]}`
	var req responses.Request
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	if _, err := ConvertRequest(&req); err != nil {
		t.Fatal(err)
	}
}

func TestConvertRequest_PromptFieldFallback(t *testing.T) {
	req := &responses.Request{
		Model:  "m",
		Prompt: responses.Input{Items: []responses.Item{
			{Type: "message", Role: "user", Content: responses.ItemContent{String: "via prompt"}},
		}},
	}
	out, err := ConvertRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Messages) != 1 || out.Messages[0].Content.String != "via prompt" {
		t.Fatalf("prompt fallback failed: %+v", out.Messages)
	}
}

func TestConvertUsage_DetailsMapping(t *testing.T) {
	u := &completions.Usage{
		PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150,
		PromptTokensDetails:     &completions.TokenDetails{CachedTokens: 80},
		CompletionTokensDetails: &completions.TokenDetails{ReasoningTokens: 30},
	}
	got := convertUsage(u)
	if got.InputTokensDetails == nil || got.InputTokensDetails.CachedTokens != 80 {
		t.Fatalf("cached tokens lost: %+v", got.InputTokensDetails)
	}
	if got.OutputTokensDetails == nil || got.OutputTokensDetails.ReasoningTokens != 30 {
		t.Fatalf("reasoning tokens lost: %+v", got.OutputTokensDetails)
	}
}

func TestStreamer_UniqueToolItemIDs(t *testing.T) {
	s := NewStreamer(&responses.Request{Model: "m"})
	s.Feed(&completions.StreamChunk{ID: "c1", Model: "m", Choices: []completions.StreamChoice{{
		Delta: completions.Delta{ToolCalls: []completions.ToolCall{
			{Index: idxPtr(0), ID: "a", Type: "function", Function: completions.FuncCall{Name: "f"}},
			{Index: idxPtr(1), ID: "b", Type: "function", Function: completions.FuncCall{Name: "g"}},
		}},
	}}})
	s.Finish()
	ids := map[string]bool{}
	for _, item := range s.FinalResponse().Output {
		if item.Type == "function_call" {
			if ids[item.ID] {
				t.Fatalf("duplicate item id: %s", item.ID)
			}
			ids[item.ID] = true
		}
	}
	if len(ids) != 2 {
		t.Fatalf("want 2 unique ids, got %v", ids)
	}
}

func TestStreamer_ReasoningContentFlow(t *testing.T) {
	s := NewStreamer(&responses.Request{Model: "m"})
	var all []*responses.Event
	all = append(all, s.Feed(&completions.StreamChunk{ID: "c1", Model: "m", Choices: []completions.StreamChoice{{
		Delta: completions.Delta{ReasoningContent: "think "},
	}}})...)
	all = append(all, s.Feed(&completions.StreamChunk{Choices: []completions.StreamChoice{{
		Delta: completions.Delta{ReasoningContent: "hard"},
	}}})...)
	all = append(all, s.Feed(&completions.StreamChunk{Choices: []completions.StreamChoice{{
		Delta: completions.Delta{Content: "answer"}, FinishReason: strPtr("stop"),
	}}})...)
	all = append(all, s.Finish()...)

	joined := strings.Join(typesOf(all), ",")
	for _, want := range []string{
		"response.output_item.added",
		"response.reasoning_summary_part.added",
		"response.reasoning_summary_text.delta",
		"response.reasoning_summary_text.done",
		"response.reasoning_summary_part.done",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %s in %s", want, joined)
		}
	}
	final := s.FinalResponse()
	if final.Output[0].Type != "reasoning" || final.Output[0].Summary[0].Text != "think hard" {
		t.Fatalf("reasoning aggregation wrong: %+v", final.Output[0])
	}
	if final.Output[1].Type != "message" {
		t.Fatalf("message must follow reasoning: %+v", final.Output)
	}
}

func TestConvertResponse_ReasoningContent(t *testing.T) {
	chat := &completions.Response{
		ID: "c1", Model: "m",
		Choices: []completions.Choice{{Message: completions.Message{
			Role: "assistant", Content: completions.MessageContent{String: "ans"},
			ReasoningContent: "because",
		}, FinishReason: "stop"}},
	}
	out := ConvertResponse(chat, &responses.Request{})
	if out.Output[0].Type != "reasoning" || out.Output[0].Summary[0].Text != "because" {
		t.Fatalf("reasoning item wrong: %+v", out.Output[0])
	}
	if out.Output[1].Type != "message" || out.Output[1].Content.Parts[0].Text != "ans" {
		t.Fatalf("message item wrong: %+v", out.Output[1])
	}
}

func TestStreamer_ReasoningProviderAlias(t *testing.T) {
	s := NewStreamer(&responses.Request{Model: "m"})
	ev := s.Feed(&completions.StreamChunk{ID: "c1", Model: "m", Choices: []completions.StreamChoice{{
		Delta: completions.Delta{Reasoning: "alt"},
	}}})
	if !contains(typesOf(ev), "response.reasoning_summary_text.delta") {
		t.Fatalf("reasoning alias not handled: %v", typesOf(ev))
	}
}

func TestConvertToolChoice_CursorDictForms(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{`"auto"`, `"auto"`},
		{`"none"`, `"none"`},
		{`{"type":"auto"}`, `"auto"`},
		{`{"type":"none"}`, `"none"`}, // must NOT degrade to auto
		{`{"type":"required"}`, `"required"`},
		{`{"type":"tool"}`, `"required"`},
		{`{"type":"any"}`, `"required"`},
		{`{"type":"function","name":"f"}`, `{"type":"function","function":{"name":"f"}}`},
		{`{"type":"function","function":{"name":"f"}}`, `{"type":"function","function":{"name":"f"}}`},
	}
	for _, tc := range cases {
		got := convertToolChoice(json.RawMessage(tc.in))
		var gotV, wantV any
		if err := json.Unmarshal(got, &gotV); err != nil {
			t.Errorf("convertToolChoice(%s) produced invalid JSON: %v", tc.in, err)
			continue
		}
		if err := json.Unmarshal([]byte(tc.want), &wantV); err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(gotV) != fmt.Sprint(wantV) {
			t.Errorf("convertToolChoice(%s) = %s, want %s", tc.in, got, tc.want)
		}
	}
}

func TestConvertRequest_MergesConsecutiveFunctionCalls(t *testing.T) {
	req := &responses.Request{
		Model: "m",
		Input: responses.Input{Items: []responses.Item{
			{Type: "message", Role: "user", Content: responses.ItemContent{String: "go"}},
			{Type: "function_call", CallID: "c1", Name: "a", Arguments: "{}"},
			{Type: "function_call", CallID: "c2", Name: "b", Arguments: "{}"},
			{Type: "function_call_output", CallID: "c1", Output: responses.OutputContent{String: "1"}},
			{Type: "function_call_output", CallID: "c2", Output: responses.OutputContent{String: "2"}},
		}},
	}
	out, err := ConvertRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	// user, assistant(with 2 tool_calls), tool, tool
	if len(out.Messages) != 4 {
		t.Fatalf("want 4 messages after merge, got %d: %+v", len(out.Messages), out.Messages)
	}
	assistant := out.Messages[1]
	if assistant.Role != "assistant" || len(assistant.ToolCalls) != 2 ||
		assistant.ToolCalls[0].ID != "c1" || assistant.ToolCalls[1].ID != "c2" {
		t.Fatalf("merge failed: %+v", assistant)
	}
}

func TestConvertRequest_CallIDFallsBackToID(t *testing.T) {
	req := &responses.Request{
		Model: "m",
		Input: responses.Input{Items: []responses.Item{
			{Type: "message", Role: "user", Content: responses.ItemContent{String: "x"}},
			{Type: "function_call", ID: "fc_123", Name: "f", Arguments: "{}"},
			{Type: "function_call_output", CallID: "fc_123", Output: responses.OutputContent{String: "ok"}},
		}},
	}
	out, err := ConvertRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if got := out.Messages[1].ToolCalls[0].ID; got != "fc_123" {
		t.Fatalf("id fallback failed: %q", got)
	}
}

func TestConvertRequest_ReasoningEffortForwarded(t *testing.T) {
	req := &responses.Request{
		Model:     "m",
		Input:     responses.Input{String: "x"},
		Reasoning: &responses.Reasoning{Effort: "high", Summary: "auto"},
	}
	out, err := ConvertRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if out.ReasoningEffort != "high" {
		t.Fatalf("reasoning effort lost: %+v", out)
	}
}

func TestConvertResponse_RefusalFinishReasonIncomplete(t *testing.T) {
	chat := &completions.Response{
		ID: "c", Model: "m",
		Choices: []completions.Choice{{Message: completions.Message{
			Role: "assistant", Refusal: "no",
		}, FinishReason: "refusal"}},
	}
	out := ConvertResponse(chat, &responses.Request{})
	if out.Status != "incomplete" {
		t.Fatalf("refusal finish must be incomplete, got %s", out.Status)
	}
}

func TestStreamer_RefusalFinishIncomplete(t *testing.T) {
	s := NewStreamer(&responses.Request{Model: "m"})
	s.Feed(&completions.StreamChunk{ID: "c", Model: "m", Choices: []completions.StreamChoice{{
		Delta: completions.Delta{Refusal: "no"}, FinishReason: strPtr("refusal"),
	}}})
	evs := s.Finish()
	if evs[len(evs)-1].Type != "response.incomplete" {
		t.Fatalf("want response.incomplete, got %s", evs[len(evs)-1].Type)
	}
}

func TestStreamer_ReasoningClosesBeforeMessageOpens(t *testing.T) {
	s := NewStreamer(&responses.Request{Model: "m"})
	all := s.Feed(&completions.StreamChunk{ID: "c", Model: "m", Choices: []completions.StreamChoice{{
		Delta: completions.Delta{ReasoningContent: "think"},
	}}})
	all = append(all, s.Feed(&completions.StreamChunk{Choices: []completions.StreamChoice{{
		Delta: completions.Delta{Content: "answer"},
	}}})...)

	types := typesOf(all)
	// The reasoning item's done (the first output_item.done) must precede
	// the message item's added (the last output_item.added).
	rsDone := indexOf(types, "response.output_item.done")
	msgAdded := lastIndexOf(types, "response.output_item.added")
	if rsDone == -1 || msgAdded == -1 || rsDone > msgAdded {
		t.Fatalf("reasoning not closed before message: %v", types)
	}
}

func TestConvertTools_ObjectTypeInjection(t *testing.T) {
	req := &responses.Request{
		Model: "m",
		Input: responses.Input{String: "x"},
		Tools: []responses.Tool{
			{Name: "no_params"},
			{Name: "loose_params", Parameters: json.RawMessage(`{"properties":{"a":{"type":"string"}}}`)},
		},
	}
	out, err := ConvertRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(out.Tools[0].Function.Parameters, &schema); err != nil {
		t.Fatal(err)
	}
	if schema["type"] != "object" {
		t.Fatalf("missing parameters must gain type:object: %s", out.Tools[0].Function.Parameters)
	}
	if err := json.Unmarshal(out.Tools[1].Function.Parameters, &schema); err != nil {
		t.Fatal(err)
	}
	if schema["type"] != "object" {
		t.Fatalf("schema without type must gain type:object: %s", out.Tools[1].Function.Parameters)
	}
}

func indexOf(list []string, s string) int {
	for i, v := range list {
		if v == s {
			return i
		}
	}
	return -1
}

func lastIndexOf(list []string, s string) int {
	for i := len(list) - 1; i >= 0; i-- {
		if list[i] == s {
			return i
		}
	}
	return -1
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
