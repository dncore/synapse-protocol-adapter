package anthropic

import (
	"encoding/json"

	"github.com/dncore/synapse-protocol-adapter/internal/completions"
)

// Event is one Anthropic SSE event: the wire `event:` name plus the payload
// that goes on the `data:` line (the payload repeats the type, per Anthropic's
// schema).
type Event struct {
	Type string
	Data any
}

// Event names.
const (
	EventMessageStart      = "message_start"
	EventContentBlockStart = "content_block_start"
	EventContentBlockDelta = "content_block_delta"
	EventContentBlockStop  = "content_block_stop"
	EventMessageDelta      = "message_delta"
	EventMessageStop       = "message_stop"
	EventError             = "error"
)

// Streamer converts a chat completion SSE stream into Anthropic Messages
// events. One Streamer serves one request and is not safe for concurrent use.
//
// The event sequence is the one Claude Code consumes (verified against the
// real client): message_start → per block content_block_start / deltas /
// content_block_stop → message_delta → message_stop. Tool calls stream as
// tool_use blocks whose arguments arrive as input_json_delta fragments, and a
// block only opens once its function name is known (providers may send the
// name after the first argument fragment).
type Streamer struct {
	model   string
	msgID   string
	started bool
	done    bool

	nextIndex int
	textOpen  bool
	textIndex int

	tools map[int]*toolBlock
	order []int

	usage      *completions.Usage
	stopReason string
}

type toolBlock struct {
	id      string
	name    string
	index   int
	open    bool
	argsBuf []byte
}

// NewStreamer prepares a streamer for one request.
func NewStreamer(model string) *Streamer {
	return &Streamer{model: model, tools: map[int]*toolBlock{}, stopReason: "end_turn"}
}

// Start emits message_start. It is safe to call once; the server calls it
// before feeding chunks so the client sees the response begin immediately.
func (s *Streamer) Start(chatID, chatModel string) []Event {
	if s.started {
		return nil
	}
	s.started = true
	s.msgID = messageID(chatID)
	msg := map[string]any{
		"id":            s.msgID,
		"type":          "message",
		"role":          "assistant",
		"model":         modelOf(chatModel, s.model),
		"content":       []any{},
		"stop_reason":   nil,
		"stop_sequence": nil,
		"usage":         map[string]int{"input_tokens": s.usageInput(), "output_tokens": 0},
	}
	return []Event{{Type: EventMessageStart, Data: map[string]any{"type": "message_start", "message": msg}}}
}

// Chunk consumes one chat chunk and returns the events it produced.
func (s *Streamer) Chunk(chunk *completions.StreamChunk) []Event {
	if s.done {
		return nil
	}
	var events []Event
	if !s.started {
		events = append(events, s.Start(chunk.ID, chunk.Model)...)
	}
	if chunk.Usage != nil {
		s.usage = chunk.Usage
	}
	if chunk.Error != nil {
		s.done = true
		return append(events, s.errorEvent(chunk.Error.Message))
	}
	if len(chunk.Choices) == 0 {
		return events
	}
	choice := chunk.Choices[0]
	// Reasoning traces have no Anthropic counterpart on this route (the
	// takeover only serves gpt-6 tool turns, which never emit one).
	if choice.Delta.Content != "" {
		events = append(events, s.openText()...)
		events = append(events, Event{Type: EventContentBlockDelta, Data: map[string]any{
			"type":  "content_block_delta",
			"index": s.textIndex,
			"delta": map[string]any{"type": "text_delta", "text": choice.Delta.Content},
		}})
	}
	for _, call := range choice.Delta.ToolCalls {
		events = append(events, s.toolDelta(call)...)
	}
	if choice.FinishReason != nil && *choice.FinishReason != "" {
		s.stopReason = stopReasonFromPartial(*choice.FinishReason, s.stopReason)
	}
	return events
}

// Finish closes open blocks and emits the terminal events. Calling it twice
// is a no-op; a stream that already ended in an error returns nothing.
func (s *Streamer) Finish() []Event {
	if s.done {
		return nil
	}
	s.done = true
	var events []Event
	if !s.started {
		events = append(events, s.Start("", s.model)...)
	}
	events = append(events, s.closeText()...)
	// Tool blocks that never received a name still must be closed so the
	// client's block state stays consistent.
	for _, idx := range s.order {
		tb := s.tools[idx]
		if tb.open {
			events = append(events, Event{Type: EventContentBlockStop, Data: map[string]any{
				"type": "content_block_stop", "index": tb.index,
			}})
		}
	}
	usage := map[string]any{"output_tokens": s.usageOutput()}
	if in := s.usageInput(); in > 0 {
		usage["input_tokens"] = in
	}
	events = append(events,
		Event{Type: EventMessageDelta, Data: map[string]any{
			"type":  "message_delta",
			"delta": map[string]any{"stop_reason": s.finalStopReason(), "stop_sequence": nil},
			"usage": usage,
		}},
		Event{Type: EventMessageStop, Data: map[string]any{"type": "message_stop"}},
	)
	return events
}

// openText opens the text block once.
func (s *Streamer) openText() []Event {
	if s.textOpen {
		return nil
	}
	s.textOpen = true
	s.textIndex = s.nextIndex
	s.nextIndex++
	return []Event{{Type: EventContentBlockStart, Data: map[string]any{
		"type":          "content_block_start",
		"index":         s.textIndex,
		"content_block": map[string]any{"type": "text", "text": ""},
	}}}
}

// closeText closes the text block when it is open.
func (s *Streamer) closeText() []Event {
	if !s.textOpen {
		return nil
	}
	s.textOpen = false
	return []Event{{Type: EventContentBlockStop, Data: map[string]any{
		"type": "content_block_stop", "index": s.textIndex,
	}}}
}

// toolDelta accumulates one streamed tool-call fragment, opening the block as
// soon as the function name is known.
func (s *Streamer) toolDelta(call completions.ToolCall) []Event {
	idx := 0
	if call.Index != nil {
		idx = *call.Index
	}
	tb, ok := s.tools[idx]
	if !ok {
		tb = &toolBlock{index: -1}
		s.tools[idx] = tb
		s.order = append(s.order, idx)
	}
	if call.ID != "" {
		tb.id = call.ID
	}
	if call.Function.Name != "" {
		tb.name = call.Function.Name
	}
	var events []Event
	// Text and tool blocks cannot interleave: once a tool call starts the
	// text block is done (the same ordering the Responses route assumes).
	events = append(events, s.closeText()...)
	if !tb.open && tb.name != "" {
		tb.open = true
		tb.index = s.nextIndex
		s.nextIndex++
		input := tb.argsBuf
		if len(input) == 0 {
			input = []byte("{}")
		}
		events = append(events, Event{Type: EventContentBlockStart, Data: map[string]any{
			"type":  "content_block_start",
			"index": tb.index,
			"content_block": map[string]any{
				"type": "tool_use", "id": tb.id, "name": tb.name, "input": json.RawMessage(input),
			},
		}})
		tb.argsBuf = nil
	}
	if call.Function.Arguments != "" {
		if tb.open {
			events = append(events, Event{Type: EventContentBlockDelta, Data: map[string]any{
				"type":  "content_block_delta",
				"index": tb.index,
				"delta": map[string]any{"type": "input_json_delta", "partial_json": call.Function.Arguments},
			}})
		} else {
			tb.argsBuf = append(tb.argsBuf, call.Function.Arguments...)
		}
	}
	return events
}

func (s *Streamer) errorEvent(message string) Event {
	return Event{Type: EventError, Data: map[string]any{
		"type":  "error",
		"error": map[string]any{"type": "api_error", "message": message},
	}}
}

// finalStopReason reports tool_use when any tool block opened.
func (s *Streamer) finalStopReason() string {
	for _, idx := range s.order {
		if s.tools[idx].open {
			return "tool_use"
		}
	}
	return s.stopReason
}

func (s *Streamer) usageInput() int {
	if s.usage == nil {
		return 0
	}
	return s.usage.PromptTokens
}

func (s *Streamer) usageOutput() int {
	if s.usage == nil {
		return 0
	}
	return s.usage.CompletionTokens
}
