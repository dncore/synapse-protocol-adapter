package anthropic

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/dncore/synapse-protocol-adapter/internal/completions"
	"github.com/dncore/synapse-protocol-adapter/internal/converter"
)

// ErrUnsupported marks a request the mapper deliberately refuses to translate.
// The caller falls back to plain passthrough, so the client sees exactly what
// it would have seen without the takeover (a 400 from the upstream) instead of
// a silently lossy translation.
var ErrUnsupported = errors.New("anthropic request shape not supported by the takeover mapper")

// ConvertRequest translates an Anthropic Messages request into a Chat
// Completions request, applying the same model-family dialect as the
// Responses route (converter.ApplyChatDialect).
func ConvertRequest(req *Request) (*completions.Request, error) {
	if len(req.Messages) == 0 {
		return nil, fmt.Errorf("%w: empty messages", ErrUnsupported)
	}

	msgs, err := convertSystem(req.System)
	if err != nil {
		return nil, err
	}
	turns, err := convertMessages(req.Messages)
	if err != nil {
		return nil, err
	}
	msgs = append(msgs, turns...)

	tools, err := convertTools(req.Tools)
	if err != nil {
		return nil, err
	}
	toolChoice, err := convertToolChoice(req.ToolChoice)
	if err != nil {
		return nil, err
	}

	out := &completions.Request{
		Model:       req.Model,
		Messages:    msgs,
		Tools:       tools,
		ToolChoice:  toolChoice,
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		Stream:      req.Stream,
	}
	out.Stop = req.StopSequences
	// The family dialect decides tool-vs-reasoning and the output-cap field
	// name; `thinking` needs no mapping because a gpt-6 tool turn can only be
	// served with reasoning off, which the dialect forces.
	converter.ApplyChatDialect(out.Model, out)
	if out.Stream {
		out.StreamOptions = &completions.StreamOptions{IncludeUsage: true}
	}
	return out, nil
}

// convertSystem folds the top-level system prompt (string or blocks) into one
// system message. Multiple blocks are joined because several gateways reject
// more than one leading system turn.
func convertSystem(raw json.RawMessage) ([]completions.Message, error) {
	text, err := flattenText(raw)
	if err != nil {
		return nil, err
	}
	if text == "" {
		return nil, nil
	}
	return []completions.Message{{Role: "system", Content: completions.MessageContent{String: text}}}, nil
}

// convertMessages maps the message list. Anthropic history shapes appear as:
//
//	user      text / image / tool_result blocks
//	assistant text / tool_use blocks
//	system    (Claude Code hooks emit these inside messages) text blocks
//
// Consecutive tool_use blocks become one assistant message with N tool_calls,
// mirroring the Responses route's handling of parallel calls.
func convertMessages(in []Message) ([]completions.Message, error) {
	var out []completions.Message
	for i := range in {
		m := &in[i]
		switch m.Role {
		case "user":
			blocks, err := decodeBlocks(m.Content)
			if err != nil {
				return nil, err
			}
			for _, b := range blocks {
				switch b.Type {
				case "text":
					out = append(out, completions.Message{Role: "user", Content: completions.MessageContent{String: b.Text}})
				case "image":
					part, err := imagePart(b)
					if err != nil {
						return nil, err
					}
					out = append(out, completions.Message{Role: "user", Content: completions.MessageContent{Parts: []completions.ContentPart{part}}})
				case "tool_result":
					content, err := flattenText(b.Content)
					if err != nil {
						return nil, err
					}
					out = append(out, completions.Message{Role: "tool", ToolCallID: b.ToolUseID, Content: completions.MessageContent{String: content}})
				case "thinking", "redacted_thinking":
					// History thinking blocks carry no information a gpt-6 tool
					// turn can use (that model never emits one here), so they are
					// dropped rather than failing the takeover.
				default:
					return nil, fmt.Errorf("%w: user content block %q", ErrUnsupported, b.Type)
				}
			}
		case "assistant":
			blocks, err := decodeBlocks(m.Content)
			if err != nil {
				return nil, err
			}
			for _, b := range blocks {
				switch b.Type {
				case "text":
					out = append(out, completions.Message{Role: "assistant", Content: completions.MessageContent{String: b.Text}})
				case "tool_use":
					args := "{}"
					if len(b.Input) > 0 {
						args = string(b.Input)
					}
					call := completions.ToolCall{ID: b.ID, Type: "function", Function: completions.FuncCall{Name: b.Name, Arguments: args}}
					if last := len(out) - 1; last >= 0 && out[last].Role == "assistant" {
						out[last].ToolCalls = append(out[last].ToolCalls, call)
					} else {
						out = append(out, completions.Message{Role: "assistant", ToolCalls: []completions.ToolCall{call}})
					}
				case "thinking", "redacted_thinking":
					// See the user-role case above.
				default:
					return nil, fmt.Errorf("%w: assistant content block %q", ErrUnsupported, b.Type)
				}
			}
		case "system":
			// Claude Code injects hook output as system-role turns. Fold them
			// into a system message rather than failing: the upstream's own
			// translator accepts this shape, so it is normal traffic.
			text, err := flattenText(m.Content)
			if err != nil {
				return nil, err
			}
			if text != "" {
				out = append(out, completions.Message{Role: "system", Content: completions.MessageContent{String: text}})
			}
		default:
			return nil, fmt.Errorf("%w: message role %q", ErrUnsupported, m.Role)
		}
	}
	return out, nil
}

// decodeBlocks accepts a message content that is either a plain string or a
// block array; an empty content yields no blocks.
func decodeBlocks(raw json.RawMessage) ([]Block, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s == "" {
			return nil, nil
		}
		return []Block{{Type: "text", Text: s}}, nil
	}
	var blocks []Block
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, fmt.Errorf("%w: message content is neither string nor blocks", ErrUnsupported)
	}
	return blocks, nil
}

// flattenText renders a string, a block array, or absent content as text.
// Only text blocks contribute; anything else is unsupported.
func flattenText(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	var blocks []Block
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", fmt.Errorf("%w: content is neither string nor blocks", ErrUnsupported)
	}
	var parts []string
	for _, b := range blocks {
		switch b.Type {
		case "text":
			parts = append(parts, b.Text)
		case "image":
			return "", fmt.Errorf("%w: image inside flattened content", ErrUnsupported)
		default:
			return "", fmt.Errorf("%w: content block %q in text position", ErrUnsupported, b.Type)
		}
	}
	return strings.Join(parts, "\n\n"), nil
}

// imagePart maps an Anthropic image block onto a chat image_url part.
func imagePart(b Block) (completions.ContentPart, error) {
	if b.Source == nil {
		return completions.ContentPart{}, fmt.Errorf("%w: image without source", ErrUnsupported)
	}
	switch b.Source.Type {
	case "base64":
		if b.Source.MediaType == "" || b.Source.Data == "" {
			return completions.ContentPart{}, fmt.Errorf("%w: base64 image without media_type/data", ErrUnsupported)
		}
		return completions.ContentPart{
			Type:     "image_url",
			ImageURL: &completions.ImageURL{URL: "data:" + b.Source.MediaType + ";base64," + b.Source.Data},
		}, nil
	case "url":
		if b.Source.URL == "" {
			return completions.ContentPart{}, fmt.Errorf("%w: url image without url", ErrUnsupported)
		}
		return completions.ContentPart{Type: "image_url", ImageURL: &completions.ImageURL{URL: b.Source.URL}}, nil
	default:
		return completions.ContentPart{}, fmt.Errorf("%w: image source type %q", ErrUnsupported, b.Source.Type)
	}
}

// convertTools maps Anthropic tool definitions onto chat functions. The chat
// route on this gateway requires the nested {"type":"function","function":…}
// shape; the flat Responses form is rejected with a 422.
func convertTools(tools []Tool) ([]completions.Tool, error) {
	if len(tools) == 0 {
		return nil, nil
	}
	out := make([]completions.Tool, 0, len(tools))
	for _, t := range tools {
		if t.Name == "" {
			return nil, fmt.Errorf("%w: tool without name", ErrUnsupported)
		}
		schema := json.RawMessage(`{"type":"object"}`)
		if len(t.InputSchema) > 0 {
			var m map[string]json.RawMessage
			if err := json.Unmarshal(t.InputSchema, &m); err != nil {
				return nil, fmt.Errorf("%w: tool %q input_schema is not an object", ErrUnsupported, t.Name)
			}
			if _, ok := m["type"]; !ok {
				m["type"] = json.RawMessage(`"object"`)
			}
			merged, err := json.Marshal(m)
			if err != nil {
				return nil, err
			}
			schema = merged
		}
		out = append(out, completions.Tool{Type: "function", Function: completions.Function{
			Name: t.Name, Description: t.Description, Parameters: schema,
		}})
	}
	return out, nil
}

// convertToolChoice maps the Anthropic tool_choice object onto the chat form.
func convertToolChoice(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var tc ToolChoice
	if err := json.Unmarshal(raw, &tc); err != nil {
		return nil, fmt.Errorf("%w: tool_choice is not an object", ErrUnsupported)
	}
	switch tc.Type {
	case "", "auto":
		return json.RawMessage(`"auto"`), nil
	case "any", "required":
		return json.RawMessage(`"required"`), nil
	case "none":
		return json.RawMessage(`"none"`), nil
	case "tool", "function":
		if tc.Name == "" {
			return nil, fmt.Errorf("%w: tool_choice without name", ErrUnsupported)
		}
		name, err := json.Marshal(tc.Name)
		if err != nil {
			return nil, err
		}
		return json.RawMessage(`{"type":"function","function":{"name":` + string(name) + `}}`), nil
	default:
		return nil, fmt.Errorf("%w: tool_choice type %q", ErrUnsupported, tc.Type)
	}
}
