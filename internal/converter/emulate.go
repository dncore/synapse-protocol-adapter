package converter

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/dncore/synapse-protocol-adapter/internal/completions"
)

// Tool-call emulation for models whose upstream channel cannot do native
// function calling.
//
// The shared gateway routes gpt-6-sol / gpt-6.1-sol to a channel that is not
// provisioned for tool calling, for windows that last tens of minutes
// (measured 2026-10-12: one window ran 40+ minutes; during it every shape —
// chat, /responses, /api/anthropic, streaming or not — failed for those two
// ids while gpt-6-luna and every other model answered normally). The gateway
// will not be changed, so the missing "chat → responses routing" is emulated
// here instead:
//
//   - the tools are described in the prompt instead of the `tools` field,
//     with a strict "reply with this JSON" protocol;
//   - tool history is reframed as plain text (the model's own calls are
//     replayed in the protocol's syntax, tool results become user messages);
//   - the model's reply is parsed back into a real tool call.
//
// Measured against the live gateway inside a failure window: 4/4 parseable
// tool calls, and a second tool round inside the reframed history also
// parsed. Emulation is a fallback, never the default path: it changes how
// the model sees the conversation, so it is only used while the model is
// known to be failing (see model_health) and only for ids the operator lists.

// emulatedToolCall is the shape the protocol asks the model to emit.
type emulatedToolCall struct {
	ToolCalls []struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"tool_calls"`
}

// contentText returns the text a message content actually carries: a plain
// string, or the joined text parts of a parts array. Codex sends its messages
// as parts arrays (String empty), and MessageContent.MarshalJSON writes Parts
// and ignores String whenever Parts is non-nil — so anything that reads or
// edits content must work on the form that is actually there. Measured
// 2026-10-12 with real Codex: the protocol was appended to String, silently
// shadowed by the existing Parts, and the model — with `tools` already
// removed — answered "no shell tool is available in this session".
func contentText(c completions.MessageContent) string {
	if c.Parts == nil {
		return c.String
	}
	var b strings.Builder
	for _, p := range c.Parts {
		if p.Type != "text" || p.Text == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(p.Text)
	}
	return b.String()
}

// appendContentText appends a block to the content form that is actually in
// use: a text part for parts arrays, string concatenation otherwise.
func appendContentText(c *completions.MessageContent, text string) {
	if c.Parts != nil {
		c.Parts = append(c.Parts, completions.ContentPart{Type: "text", Text: text})
		return
	}
	c.String = strings.TrimRight(c.String, "\n") + "\n\n" + text
}

// EmulateToolProtocol rewrites a chat request that carries tools into the
// prompt protocol. It reports false when there is nothing to emulate (no
// tools, or a request shape it does not fully understand), in which case the
// request is left untouched.
func EmulateToolProtocol(req *completions.Request) bool {
	if req == nil || len(req.Tools) == 0 {
		return false
	}
	tools := make([]map[string]any, 0, len(req.Tools))
	for _, t := range req.Tools {
		entry := map[string]any{"name": t.Function.Name}
		if t.Function.Description != "" {
			entry["description"] = t.Function.Description
		}
		if len(t.Function.Parameters) > 0 {
			var schema any
			if err := json.Unmarshal(t.Function.Parameters, &schema); err == nil {
				entry["parameters"] = schema
			}
		}
		tools = append(tools, entry)
	}
	schemas, err := json.Marshal(tools)
	if err != nil {
		return false
	}

	protocol := "You have tools available. Call one by replying with ONLY this JSON, " +
		"no prose and no code fences:\n" +
		"{\"tool_calls\":[{\"name\":\"<tool name>\",\"arguments\":{...}}]}\n" +
		"If no tool is needed, answer normally in text. Tool results arrive as user " +
		"messages starting with [tool result]; use them, and call another tool only " +
		"if you still need data.\n" +
		"Available tools (JSON Schema):\n" + string(schemas)

	// Reframe tool history as text, in one pass: a tool message needs the name
	// of the call it answers, which only the preceding assistant message knows.
	names := map[string]string{}
	reframed := make([]completions.Message, 0, len(req.Messages))
	for _, m := range req.Messages {
		switch m.Role {
		case "assistant":
			if len(m.ToolCalls) > 0 {
				calls := make([]map[string]any, 0, len(m.ToolCalls))
				for _, c := range m.ToolCalls {
					names[c.ID] = c.Function.Name
					var args any
					if err := json.Unmarshal([]byte(c.Function.Arguments), &args); err != nil {
						args = c.Function.Arguments
					}
					calls = append(calls, map[string]any{"name": c.Function.Name, "arguments": args})
				}
				payload, err := json.Marshal(map[string]any{"tool_calls": calls})
				if err != nil {
					return false
				}
				text := string(payload)
				if prior := contentText(m.Content); prior != "" {
					text = prior + "\n" + text
				}
				reframed = append(reframed, completions.Message{
					Role:    "assistant",
					Content: completions.MessageContent{String: text},
				})
				continue
			}
		case "tool":
			name := names[m.ToolCallID]
			if name == "" {
				name = "tool"
			}
			reframed = append(reframed, completions.Message{
				Role: "user",
				Content: completions.MessageContent{String: fmt.Sprintf("[tool result] %s -> %s",
					name, contentText(m.Content))},
			})
			continue
		}
		reframed = append(reframed, m)
	}

	// The protocol goes in front: as the system message when the caller
	// already has one, otherwise as a new leading system message.
	inserted := false
	for i := range reframed {
		if reframed[i].Role == "system" {
			appendContentText(&reframed[i].Content, protocol)
			inserted = true
			break
		}
	}
	if !inserted {
		reframed = append([]completions.Message{{
			Role:    "system",
			Content: completions.MessageContent{String: protocol},
		}}, reframed...)
	}

	req.Messages = reframed
	req.Tools = nil
	req.ToolChoice = nil
	return true
}

// ParseEmulatedToolCall extracts the first tool call from the model's reply.
// The reply may carry prose around the JSON (models do add a sentence even
// when told not to), so the JSON object is located rather than assumed.
func ParseEmulatedToolCall(content string) (name, arguments string, ok bool) {
	start := strings.Index(content, "{\"tool_calls\"")
	if start < 0 {
		// Tolerate spacing inside the key.
		if idx := strings.Index(content, "tool_calls"); idx >= 0 {
			brace := strings.LastIndex(content[:idx], "{")
			if brace >= 0 {
				start = brace
			}
		}
	}
	if start < 0 {
		return "", "", false
	}
	dec := json.NewDecoder(strings.NewReader(content[start:]))
	var out emulatedToolCall
	if err := dec.Decode(&out); err != nil || len(out.ToolCalls) == 0 {
		return "", "", false
	}
	first := out.ToolCalls[0]
	if strings.TrimSpace(first.Name) == "" {
		return "", "", false
	}
	args := strings.TrimSpace(string(first.Arguments))
	if args == "" || args == "null" {
		args = "{}"
	}
	if !json.Valid([]byte(args)) {
		return "", "", false
	}
	return first.Name, args, true
}

// ApplyEmulatedToolCall turns a parsed protocol reply into a real tool call on
// the response, so every downstream transformer sees the shape it already
// knows.
func ApplyEmulatedToolCall(resp *completions.Response, callID, name, arguments string) bool {
	if resp == nil || len(resp.Choices) == 0 {
		return false
	}
	msg := &resp.Choices[0].Message
	msg.Content = completions.MessageContent{}
	msg.ToolCalls = []completions.ToolCall{{
		ID:   callID,
		Type: "function",
		Function: completions.FuncCall{
			Name:      name,
			Arguments: arguments,
		},
	}}
	resp.Choices[0].FinishReason = "tool_calls"
	return true
}

// SynthesizeChatSSE renders a complete chat response as the chunk stream the
// SSE transformers consume, so an emulated (necessarily complete) response
// can travel the ordinary streaming path.
func SynthesizeChatSSE(resp *completions.Response) []byte {
	var b strings.Builder
	write := func(v any) {
		payload, err := json.Marshal(v)
		if err != nil {
			return
		}
		b.WriteString("data: ")
		b.Write(payload)
		b.WriteString("\n\n")
	}
	base := map[string]any{
		"id": resp.ID, "object": "chat.completion.chunk",
		"created": resp.Created, "model": resp.Model,
	}
	withDelta := func(delta map[string]any, finish any) map[string]any {
		chunk := map[string]any{}
		for k, v := range base {
			chunk[k] = v
		}
		chunk["choices"] = []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}
		return chunk
	}

	if len(resp.Choices) == 0 {
		write(withDelta(map[string]any{}, "stop"))
		b.WriteString("data: [DONE]\n\n")
		return []byte(b.String())
	}
	choice := resp.Choices[0]
	write(withDelta(map[string]any{"role": "assistant"}, nil))
	if choice.Message.Content.String != "" {
		write(withDelta(map[string]any{"content": choice.Message.Content.String}, nil))
	}
	for i, call := range choice.Message.ToolCalls {
		write(withDelta(map[string]any{"tool_calls": []any{map[string]any{
			"index": i, "id": call.ID, "type": "function",
			"function": map[string]any{"name": call.Function.Name, "arguments": call.Function.Arguments},
		}}}, nil))
	}
	finish := choice.FinishReason
	if finish == "" {
		finish = "stop"
	}
	write(withDelta(map[string]any{}, finish))
	if resp.Usage != nil {
		usage := map[string]any{}
		for k, v := range base {
			usage[k] = v
		}
		usage["choices"] = []any{}
		usage["usage"] = resp.Usage
		write(usage)
	}
	b.WriteString("data: [DONE]\n\n")
	return []byte(b.String())
}
