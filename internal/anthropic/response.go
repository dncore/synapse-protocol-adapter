package anthropic

import (
	"encoding/json"
	"strings"

	"github.com/dncore/synapse-protocol-adapter/internal/completions"
)

// ConvertResponse maps a non-streamed chat completion onto an Anthropic
// Messages response: assistant text becomes one text block, each tool call
// becomes a tool_use block, and the finish reason maps onto Anthropic's
// stop_reason vocabulary.
func ConvertResponse(chat *completions.Response, model string) *Response {
	out := &Response{
		ID:         messageID(chat.ID),
		Type:       "message",
		Role:       "assistant",
		Model:      modelOf(chat.Model, model),
		Content:    []OutBlock{},
		StopReason: "end_turn",
	}
	if len(chat.Choices) > 0 {
		msg := chat.Choices[0].Message
		if text := textOf(msg); text != "" {
			out.Content = append(out.Content, OutBlock{Type: "text", Text: text})
		}
		for _, call := range msg.ToolCalls {
			out.Content = append(out.Content, toolUseBlock(call))
		}
		out.StopReason = stopReason(chat.Choices[0].FinishReason)
	}
	if out.StopReason == "tool_use" && !hasToolUse(out.Content) {
		out.StopReason = "end_turn" // upstream said tool_calls but sent none
	}
	if chat.Usage != nil {
		out.Usage = ResponseUsage{InputTokens: chat.Usage.PromptTokens, OutputTokens: chat.Usage.CompletionTokens}
	}
	return out
}

// textOf extracts the assistant text, falling back to the DeepSeek-style
// reasoning trace when a provider puts everything there (the gateway does this
// for some backends). A turn that carries tool calls already has content.
func textOf(msg completions.Message) string {
	if msg.Content.String != "" {
		return msg.Content.String
	}
	var parts []string
	for _, p := range msg.Content.Parts {
		if p.Type == "text" && p.Text != "" {
			parts = append(parts, p.Text)
		}
	}
	return strings.Join(parts, "\n\n")
}

// toolUseBlock renders one chat tool call as an Anthropic tool_use block.
func toolUseBlock(call completions.ToolCall) OutBlock {
	input := json.RawMessage(`{}`)
	if call.Function.Arguments != "" {
		if json.Valid([]byte(call.Function.Arguments)) {
			input = json.RawMessage(call.Function.Arguments)
		} else {
			// Anthropic requires an object. A provider that streamed
			// malformed JSON still gets its bytes to the client, under a
			// key that makes the truncation obvious instead of silently
			// turning into an empty call.
			raw, err := json.Marshal(map[string]string{"_raw_arguments": call.Function.Arguments})
			if err == nil {
				input = raw
			}
		}
	}
	return OutBlock{Type: "tool_use", ID: call.ID, Name: call.Function.Name, Input: input}
}

// stopReason maps a chat finish_reason onto Anthropic's stop_reason.
func stopReason(finish string) string {
	switch finish {
	case "tool_calls", "function_call":
		return "tool_use"
	case "length":
		return "max_tokens"
	case "content_filter":
		return "refusal"
	case "stop", "":
		return "end_turn"
	default:
		return "end_turn"
	}
}

// stopReasonFromPartial maps a streamed finish_reason, keeping the current
// value when the upstream omitted it.
func stopReasonFromPartial(finish, current string) string {
	if finish == "" {
		return current
	}
	return stopReason(finish)
}

// messageID keeps the upstream id when present (Claude Code logs it) and
// otherwise synthesizes an Anthropic-looking one.
func messageID(chatID string) string {
	if chatID == "" {
		return "msg_synapse"
	}
	if strings.HasPrefix(chatID, "msg_") {
		return chatID
	}
	return "msg_" + chatID
}

// modelOf prefers the upstream-reported model, falling back to the requested
// one so the client always sees a model id it recognizes.
func modelOf(fromChat, requested string) string {
	if fromChat != "" {
		return fromChat
	}
	return requested
}

func hasToolUse(blocks []OutBlock) bool {
	for _, b := range blocks {
		if b.Type == "tool_use" {
			return true
		}
	}
	return false
}
