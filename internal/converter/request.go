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
		TopP:              req.TopP,
		MaxTokens:         req.MaxOutputTokens,
		Stream:            req.Stream,
		ParallelToolCalls: req.ParallelToolCalls,
		User:              req.User,
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
func convertMessages(req *responses.Request) ([]completions.Message, error) {
	var msgs []completions.Message

	hasSystem := false
	if req.Input.String != "" {
		msgs = append(msgs, completions.Message{
			Role:    "user",
			Content: completions.MessageContent{String: req.Input.String},
		})
	} else {
		for i := range req.Input.Items {
			item := &req.Input.Items[i]
			switch item.Type {
			case "message":
				m, err := convertMessageItem(item)
				if err != nil {
					return nil, err
				}
				if m.Role == "system" || m.Role == "developer" {
					hasSystem = true
				}
				msgs = append(msgs, m)
			case "function_call":
				msgs = append(msgs, completions.Message{
					Role: "assistant",
					ToolCalls: []completions.ToolCall{{
						ID:   item.CallID,
						Type: "function",
						Function: completions.FuncCall{
							Name:      item.Name,
							Arguments: item.Arguments,
						},
					}},
				})
			case "function_call_output":
				msgs = append(msgs, completions.Message{
					Role:       "tool",
					ToolCallID: item.CallID,
					Content:    completions.MessageContent{String: item.Output},
				})
			case "reasoning":
				// Reasoning items carry no chat-completions equivalent and
				// no forward-relevant payload; drop them.
			default:
				return nil, fmt.Errorf("unsupported input item type %q", item.Type)
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
// message, mapping content part types.
func convertMessageItem(item *responses.Item) (completions.Message, error) {
	role := item.Role
	if role != "system" && role != "developer" && role != "user" && role != "assistant" && role != "tool" {
		return completions.Message{}, fmt.Errorf("unsupported message role %q", role)
	}

	m := completions.Message{Role: role}
	if item.Content.String != "" {
		m.Content = completions.MessageContent{String: item.Content.String}
	} else {
		parts := make([]completions.ContentPart, 0, len(item.Content.Parts))
		for _, p := range item.Content.Parts {
			switch p.Type {
			case "input_text", "output_text", "summary_text", "refusal":
				parts = append(parts, completions.ContentPart{Type: "text", Text: p.Text})
			case "input_image":
				parts = append(parts, completions.ContentPart{
					Type:     "image_url",
					ImageURL: &completions.ImageURL{URL: p.ImageURL},
				})
			default:
				return completions.Message{}, fmt.Errorf("unsupported content part type %q", p.Type)
			}
		}
		m.Content = completions.MessageContent{Parts: parts}
	}
	return m, nil
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
				Parameters:  t.Parameters,
				Strict:      t.Strict,
			},
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// convertToolChoice normalizes tool_choice between the two shapes. String
// forms ("auto", "none", "required") pass through; the object form
// {"type":"function","name":...} becomes {"type":"function","function":{"name":...}}.
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
		Name string `json:"name"` // Responses flattened form
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return json.RawMessage(`"auto"`)
	}
	if obj.Type != "function" {
		return json.RawMessage(`"auto"`)
	}
	name := obj.Name
	if obj.Function != nil {
		name = obj.Function.Name
	}
	if name == "" {
		return json.RawMessage(`"auto"`)
	}
	out, _ := json.Marshal(map[string]any{
		"type": "function",
		"function": map[string]any{
			"name": name,
		},
	})
	return out
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
