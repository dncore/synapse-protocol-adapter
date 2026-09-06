// Package converter implements the pure protocol translation between the
// Responses API (client side) and Chat Completions (upstream side). All
// functions are pure or hold only per-request state, so the package is
// safe for arbitrary concurrent use.
package converter

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/dncore/synapse-protocol-adapter/internal/completions"
	"github.com/dncore/synapse-protocol-adapter/internal/responses"
)

// ConvertRequest translates a Responses API request into a Chat Completions
// request. It returns an error suitable for surfacing to the client as 400
// when the request cannot be represented losslessly (e.g. it relies on
// server-side session state).
func ConvertRequest(req *responses.Request) (*completions.Request, error) {
	if req.PreviousResponseID != nil && *req.PreviousResponseID != "" {
		return nil, errors.New("previous_response_id is not supported: this proxy is stateless and does not store responses; send the full conversation in input")
	}

	msgs, err := convertMessages(req)
	if err != nil {
		return nil, err
	}

	out := &completions.Request{
		Model:             req.Model,
		Messages:          msgs,
		Tools:             convertTools(req.Tools),
		ToolChoice:        convertToolChoice(req.ToolChoice),
		Temperature:       req.Temperature,
		TopP:               req.TopP,
		MaxTokens:          req.MaxOutputTokens,
		Stream:             req.Stream,
		ParallelToolCalls:  req.ParallelToolCalls,
		User:               req.User,
	}
	// Responses reasoning effort has a direct chat equivalent on providers
	// that support it (OpenAI reasoning_effort, Gemini thinking budget
	// bridges); providers without it ignore unknown fields.
	if req.Reasoning != nil && req.Reasoning.Effort != "" {
		out.ReasoningEffort = req.Reasoning.Effort
	}
	if req.Stream {
		out.StreamOptions = &completions.StreamOptions{IncludeUsage: true}
	}
	if rf, err := convertResponseFormat(req); err != nil {
		return nil, err
	} else if rf != nil {
		out.ResponseFormat = rf
	}
	return out, nil
}

// convertMessages builds the chat message list from instructions + input.
// The legacy `prompt` field is honored as a fallback when `input` is empty.
func convertMessages(req *responses.Request) ([]completions.Message, error) {
	input := req.Input
	if input.String == "" && len(input.Items) == 0 {
		input = req.Prompt
	}

	var msgs []completions.Message

	hasSystem := false
	if input.String != "" {
		msgs = append(msgs, completions.Message{
			Role:    "user",
			Content: completions.MessageContent{String: input.String},
		})
	} else {
		for i := range input.Items {
			item := &input.Items[i]
			switch item.Type {
			case "message":
				m, err := convertMessageItem(item)
				if err != nil {
					return nil, err
				}
				if m == nil {
					continue // unknown role in replayed history; skip
				}
				if m.Role == "system" {
					hasSystem = true
				}
				msgs = append(msgs, *m)
			case "function_call":
				call := completions.ToolCall{
					// Some clients put the id only in `id` when replaying
					// history; accept either key.
					ID:   orEmpty(item.CallID, item.ID),
					Type: "function",
					Function: completions.FuncCall{
						Name:      item.Name,
						Arguments: item.Arguments,
					},
				}
				// Merge into the previous assistant message when the client
				// replayed parallel calls as consecutive items: chat
				// semantics are ONE assistant message with N tool_calls,
				// and several providers reject back-to-back assistant turns.
				if n := len(msgs); n > 0 && msgs[n-1].Role == "assistant" {
					msgs[n-1].ToolCalls = append(msgs[n-1].ToolCalls, call)
				} else {
					msgs = append(msgs, completions.Message{
						Role:      "assistant",
						ToolCalls: []completions.ToolCall{call},
					})
				}
			case "function_call_output":
				msgs = append(msgs, completions.Message{
					Role:       "tool",
					ToolCallID: item.CallID,
					Content:    toolOutputContent(item.Output),
				})
			case "reasoning":
				// Reasoning items carry no chat-completions equivalent and
				// no forward-relevant payload; drop them.
			default:
				// Unknown item types (e.g. custom_tool_call, local_shell_call
				// produced by a native Responses endpoint in an earlier turn)
				// are dropped rather than rejected: replayed history must not
				// kill the session over items this route cannot represent.
				continue
			}
		}
	}

	if req.Instructions != "" && !hasSystem {
		sys := completions.Message{
			Role:    "system",
			Content: completions.MessageContent{String: req.Instructions},
		}
		msgs = append([]completions.Message{sys}, msgs...)
	}

	if len(msgs) == 0 {
		return nil, errors.New("input must not be empty")
	}
	return msgs, nil
}

// convertMessageItem translates a Responses message item into a chat
// message, mapping content part types. A nil return means "skip": the item
// carries no chat representation (unknown role in replayed history).
func convertMessageItem(item *responses.Item) (*completions.Message, error) {
	role := item.Role
	switch role {
	case "developer":
		// OpenAI chat treats developer≈system; many OpenAI-compatible
		// backends only accept "system", and nothing is lost by unifying.
		role = "system"
	case "system", "user", "assistant", "tool":
	default:
		return nil, nil
	}

	m := completions.Message{Role: role}
	if item.Content.String != "" {
		m.Content = completions.MessageContent{String: item.Content.String}
	} else {
		parts := make([]completions.ContentPart, 0, len(item.Content.Parts))
		for _, p := range item.Content.Parts {
			switch p.Type {
			case "input_text", "output_text", "summary_text":
				parts = append(parts, completions.ContentPart{Type: "text", Text: p.Text})
			case "refusal":
				// Prior refusals replayed as conversation history: surface
				// the refusal text as plain text for chat backends.
				parts = append(parts, completions.ContentPart{Type: "text", Text: p.Refusal})
			case "input_image":
				parts = append(parts, completions.ContentPart{
					Type:     "image_url",
					ImageURL: &completions.ImageURL{URL: p.ImageURL},
				})
			default:
				// Unknown part types (input_file, guarded_text, ...) are
				// dropped rather than rejected.
				continue
			}
		}
		m.Content = completions.MessageContent{Parts: parts}
	}
	return &m, nil
}

// toolOutputContent flattens a tool output (string or typed parts) into a
// chat tool-message content. String outputs pass through verbatim; part
// arrays are joined as their text/image-url parts.
func toolOutputContent(out responses.OutputContent) completions.MessageContent {
	if out.Parts == nil {
		return completions.MessageContent{String: out.String}
	}
	parts := make([]completions.ContentPart, 0, len(out.Parts))
	for _, p := range out.Parts {
		switch p.Type {
		case "input_image", "output_image", "image_url":
			parts = append(parts, completions.ContentPart{
				Type:     "image_url",
				ImageURL: &completions.ImageURL{URL: p.ImageURL},
			})
		default: // output_text and anything text-like
			parts = append(parts, completions.ContentPart{Type: "text", Text: p.Text})
		}
	}
	if len(parts) == 0 {
		return completions.MessageContent{String: ""}
	}
	return completions.MessageContent{Parts: parts}
}

// convertTools maps flattened Responses tool definitions to the nested chat
// completions shape. Non-function tool types are dropped: chat completions
// has no representation for them.
func convertTools(tools []responses.Tool) []completions.Tool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]completions.Tool, 0, len(tools))
	for _, t := range tools {
		if t.Type != "" && t.Type != "function" {
			continue
		}
		out = append(out, completions.Tool{
			Type: "function",
			Function: completions.Function{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  withObjectType(t.Parameters),
				Strict:      t.Strict,
			},
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// withObjectType ensures a JSON-schema parameters blob carries the
// top-level "type":"object" several providers require; a missing or empty
// schema gets a minimal one.
func withObjectType(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || string(raw) == "null" {
		return json.RawMessage(`{"type":"object"}`)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return raw // not an object schema; pass through untouched
	}
	if _, ok := m["type"]; !ok {
		m["type"] = json.RawMessage(`"object"`)
		if out, err := json.Marshal(m); err == nil {
			return out
		}
	}
	return raw
}

func orEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// convertToolChoice normalizes tool_choice between the two shapes.
// String forms ("auto", "none", "required") pass through. Object forms
// seen in the wild:
//   - {"type":"function","name":...}             (Responses flattened)
//   - {"type":"function","function":{"name":…}}  (chat shape echoed back)
//   - {"type":"auto"|"none"|"required"|"tool"}    (Cursor IDE style)
//
// Notably {"type":"none"} must NOT degrade to "auto": that would re-enable
// the tools the client explicitly disabled.
func convertToolChoice(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch s {
		case "auto", "none", "required":
			return raw
		}
		return json.RawMessage(`"auto"`)
	}

	var obj struct {
		Type     string `json:"type"`
		Function *struct {
			Name string `json:"name"`
		} `json:"function"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return json.RawMessage(`"auto"`)
	}

	switch obj.Type {
	case "auto", "none":
		return json.RawMessage(`"` + obj.Type + `"`)
	case "required", "tool", "any":
		// "tool"/"any" without a function name mean "use some tool".
		return json.RawMessage(`"required"`)
	case "function":
		name := obj.Name
		if obj.Function != nil {
			name = obj.Function.Name
		}
		if name == "" {
			return json.RawMessage(`"required"`)
		}
		out, err := json.Marshal(map[string]any{
			"type": "function",
			"function": map[string]any{
				"name": name,
			},
		})
		if err != nil {
			return json.RawMessage(`"auto"`)
		}
		return out
	}
	return json.RawMessage(`"auto"`)
}

// convertResponseFormat resolves the effective response format from either
// response_format or the legacy text.format field and, when it is a
// json_schema variant, rewrites it into chat completions shape. The chat and
// responses shapes are identical for json_object and text, and differ only
// in nesting for json_schema, so only that branch rewrites the payload.
func convertResponseFormat(req *responses.Request) (json.RawMessage, error) {
	raw := req.ResponseFormat
	if len(raw) == 0 && req.Text != nil {
		raw = req.Text.Format
	}
	if len(raw) == 0 {
		return nil, nil
	}

	var probe struct {
		Type       string          `json:"type"`
		JSONSchema json.RawMessage `json:"json_schema"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("invalid response_format: %w", err)
	}

	switch probe.Type {
	case "json_schema":
		var schema struct {
			Name   string          `json:"name"`
			Schema json.RawMessage `json:"schema"`
			Strict *bool           `json:"strict"`
		}
		if err := json.Unmarshal(probe.JSONSchema, &schema); err != nil {
			return nil, fmt.Errorf("invalid json_schema in response_format: %w", err)
		}
		type chatSchema struct {
			Name   string          `json:"name"`
			Schema json.RawMessage `json:"schema"`
			Strict *bool           `json:"strict,omitempty"`
		}
		out, err := json.Marshal(struct {
			Type       string     `json:"type"`
			JSONSchema chatSchema `json:"json_schema"`
		}{"json_schema", chatSchema{Name: schema.Name, Schema: schema.Schema, Strict: schema.Strict}})
		if err != nil {
			return nil, err
		}
		return out, nil
	case "json_object", "text", "":
		return raw, nil
	default:
		return nil, fmt.Errorf("unsupported response_format type %q", probe.Type)
	}
}
