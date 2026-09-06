package converter

import (
	"encoding/json"
	"testing"

	"github.com/dncore/synapse-protocol-adapter/internal/completions"
	"github.com/dncore/synapse-protocol-adapter/internal/responses"
)

func TestConvertRequest_StringInput(t *testing.T) {
	req := &responses.Request{Model: "m1", Input: responses.Input{String: "hello"}}
	out, err := ConvertRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Messages) != 1 || out.Messages[0].Role != "user" || out.Messages[0].Content.String != "hello" {
		t.Fatalf("unexpected messages: %+v", out.Messages)
	}
	if out.Model != "m1" {
		t.Fatalf("model not forwarded")
	}
}

func TestConvertRequest_InstructionsBecomeSystem(t *testing.T) {
	req := &responses.Request{
		Model:        "m",
		Instructions: "be terse",
		Input:        responses.Input{String: "hi"},
	}
	out, err := ConvertRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Messages) != 2 {
		t.Fatalf("want 2 messages, got %d", len(out.Messages))
	}
	if out.Messages[0].Role != "system" || out.Messages[0].Content.String != "be terse" {
		t.Fatalf("instructions not prepended as system: %+v", out.Messages[0])
	}
}

func TestConvertRequest_InstructionsDuplicatedWhenSystemPresent(t *testing.T) {
	req := &responses.Request{
		Model:        "m",
		Instructions: "be terse",
		Input: responses.Input{Items: []responses.Item{
			{Type: "message", Role: "system", Content: responses.ItemContent{String: "existing"}},
			{Type: "message", Role: "user", Content: responses.ItemContent{String: "hi"}},
		}},
	}
	out, err := ConvertRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Messages) != 2 {
		t.Fatalf("instructions must not duplicate when system message exists; got %d messages", len(out.Messages))
	}
}

func TestConvertRequest_RolesAndParts(t *testing.T) {
	req := &responses.Request{
		Model: "m",
		Input: responses.Input{Items: []responses.Item{
			{Type: "message", Role: "developer", Content: responses.ItemContent{String: "dev rules"}},
			{Type: "message", Role: "user", Content: responses.ItemContent{Parts: []responses.ContentPart{
				{Type: "input_text", Text: "look"},
				{Type: "input_image", ImageURL: "data:image/png;base64,AAA"},
			}}},
			{Type: "message", Role: "assistant", Content: responses.ItemContent{String: "sure"}},
		}},
	}
	out, err := ConvertRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Messages) != 3 {
		t.Fatalf("want 3 messages, got %d", len(out.Messages))
	}
	if out.Messages[0].Role != "system" {
		t.Fatalf("developer must normalize to system for provider compatibility: %+v", out.Messages[0])
	}
	parts := out.Messages[1].Content.Parts
	if len(parts) != 2 || parts[0].Type != "text" || parts[1].Type != "image_url" || parts[1].ImageURL.URL != "data:image/png;base64,AAA" {
		t.Fatalf("content parts misconverted: %+v", parts)
	}
}

func TestConvertRequest_ToolLoop(t *testing.T) {
	req := &responses.Request{
		Model: "m",
		Input: responses.Input{Items: []responses.Item{
			{Type: "message", Role: "user", Content: responses.ItemContent{String: "weather?"}},
			{Type: "function_call", CallID: "call_1", Name: "get_weather", Arguments: `{"city":"SF"}`},
			{Type: "function_call_output", CallID: "call_1", Output: responses.OutputContent{String: `{"temp":21}`}},
		}},
	}
	out, err := ConvertRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Messages) != 3 {
		t.Fatalf("want 3 messages, got %d", len(out.Messages))
	}
	assistant := out.Messages[1]
	if assistant.Role != "assistant" || len(assistant.ToolCalls) != 1 {
		t.Fatalf("function_call item misconverted: %+v", assistant)
	}
	tc := assistant.ToolCalls[0]
	if tc.ID != "call_1" || tc.Type != "function" || tc.Function.Name != "get_weather" || tc.Function.Arguments != `{"city":"SF"}` {
		t.Fatalf("tool call fields wrong: %+v", tc)
	}
	toolMsg := out.Messages[2]
	if toolMsg.Role != "tool" || toolMsg.ToolCallID != "call_1" || toolMsg.Content.String != `{"temp":21}` {
		t.Fatalf("function_call_output misconverted: %+v", toolMsg)
	}
}

func TestConvertRequest_ToolsAndChoice(t *testing.T) {
	strict := true
	req := &responses.Request{
		Model: "m",
		Input: responses.Input{String: "hi"},
		Tools: []responses.Tool{{
			Type: "function", Name: "get_weather", Description: "d",
			Parameters: json.RawMessage(`{"type":"object"}`), Strict: &strict,
		}},
		ToolChoice: json.RawMessage(`{"type":"function","name":"get_weather"}`),
	}
	out, err := ConvertRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Tools) != 1 {
		t.Fatalf("tools lost")
	}
	tool := out.Tools[0]
	if tool.Type != "function" || tool.Function.Name != "get_weather" ||
		tool.Function.Description != "d" || string(tool.Function.Parameters) != `{"type":"object"}` || tool.Function.Strict != &strict {
		t.Fatalf("tool def misconverted: %+v", tool)
	}
	var choice struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(out.ToolChoice, &choice); err != nil {
		t.Fatal(err)
	}
	if choice.Type != "function" || choice.Function.Name != "get_weather" {
		t.Fatalf("tool_choice misconverted: %s", out.ToolChoice)
	}
}

func TestConvertRequest_ParametersPassthrough(t *testing.T) {
	maxTok := 100
	temp := 0.5
	req := &responses.Request{
		Model:           "m",
		Input:           responses.Input{String: "hi"},
		MaxOutputTokens: &maxTok,
		Temperature:     &temp,
		Stream:          true,
	}
	out, err := ConvertRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if *out.MaxTokens != 100 || *out.Temperature != 0.5 {
		t.Fatalf("scalar params lost: %+v", out)
	}
	if !out.Stream || out.StreamOptions == nil || !out.StreamOptions.IncludeUsage {
		t.Fatalf("streaming must request usage: %+v", out.StreamOptions)
	}
}

func TestConvertRequest_ResponseFormatJSONSchema(t *testing.T) {
	req := &responses.Request{
		Model:           "m",
		Input:           responses.Input{String: "hi"},
		ResponseFormat:  json.RawMessage(`{"type":"json_schema","json_schema":{"name":"out","schema":{"type":"object"},"strict":true}}`),
	}
	out, err := ConvertRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	var rf struct {
		Type       string `json:"type"`
		JSONSchema struct {
			Name   string          `json:"name"`
			Schema json.RawMessage `json:"schema"`
			Strict bool            `json:"strict"`
		} `json:"json_schema"`
	}
	if err := json.Unmarshal(out.ResponseFormat, &rf); err != nil {
		t.Fatal(err)
	}
	if rf.Type != "json_schema" || rf.JSONSchema.Name != "out" || !rf.JSONSchema.Strict || string(rf.JSONSchema.Schema) != `{"type":"object"}` {
		t.Fatalf("json_schema misconverted: %s", out.ResponseFormat)
	}
}

func TestConvertRequest_Errors(t *testing.T) {
	prev := "resp_1"
	cases := []struct {
		name string
		req  *responses.Request
	}{
		{"previous_response_id", &responses.Request{Model: "m", Input: responses.Input{String: "x"}, PreviousResponseID: &prev}},
		{"empty input", &responses.Request{Model: "m"}},
		{"only unknown items", &responses.Request{Model: "m", Input: responses.Input{Items: []responses.Item{{Type: "web_search_call"}}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ConvertRequest(tc.req); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

// Unknown item types and roles in replayed history must not kill the
// session: they are dropped while the rest of the conversation converts.
func TestConvertRequest_LenientHistory(t *testing.T) {
	req := &responses.Request{
		Model: "m",
		Input: responses.Input{Items: []responses.Item{
			{Type: "message", Role: "user", Content: responses.ItemContent{String: "hi"}},
			{Type: "custom_tool_call", Name: "patch"},
			{Type: "local_shell_call"},
			{Type: "message", Role: "wizard", Content: responses.ItemContent{String: "??"}},
			{Type: "message", Role: "user", Content: responses.ItemContent{String: "there"}},
		}},
	}
	out, err := ConvertRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Messages) != 2 {
		t.Fatalf("want 2 messages after lenient dropping, got %d: %+v", len(out.Messages), out.Messages)
	}
}

func TestConvertRequest_ReasoningItemDropped(t *testing.T) {
	req := &responses.Request{
		Model: "m",
		Input: responses.Input{Items: []responses.Item{
			{Type: "reasoning"},
			{Type: "message", Role: "user", Content: responses.ItemContent{String: "hi"}},
		}},
	}
	out, err := ConvertRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Messages) != 1 {
		t.Fatalf("reasoning item should be dropped; got %d messages", len(out.Messages))
	}
}

// roundTrip marshals a chat request and asserts the JSON shape compiles.
func TestConvertRequest_Marshalable(t *testing.T) {
	req := &responses.Request{
		Model: "m",
		Input: responses.Input{String: "hi"},
		Tools: []responses.Tool{{Type: "function", Name: "f"}},
	}
	out, err := ConvertRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := json.Marshal(out); err != nil {
		t.Fatal(err)
	}
	_ = completions.Request{} // keep the package anchored for future tests
}
