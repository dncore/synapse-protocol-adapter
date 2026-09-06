package converter

import (
	"fmt"

	"github.com/dncore/synapse-protocol-adapter/internal/completions"
	"github.com/dncore/synapse-protocol-adapter/internal/responses"
)

// Streamer converts a stream of chat completion chunks into a stream of
// Responses API events. One Streamer serves exactly one request; it is not
// safe for concurrent use.
//
// Lifecycle: Feed the first chunk (which carries the response id/model via
// Start), then every subsequent chunk; Finish emits the terminal event
// sequence. Events returned by calls are already ordered and carry correct
// sequence numbers.
type Streamer struct {
	req *responses.Request

	seq     int64
	started bool

	// Response skeleton assembled from the first chunk.
	respID   string
	created  int64
	model    string

	// Reasoning output state (DeepSeek-style reasoning_content).
	reasoningOpen    bool
	reasoningItemID  string
	reasoningBuf     []byte
	reasoningOutIdx  int

	// Text output state.
	textItemOpen  bool
	textItemID    string
	textBuf       []byte
	refusalBuf    []byte
	textOutputIdx int
	outputIndex   int // next output index to assign

	// Tool call accumulators, keyed by chat delta index.
	tools map[int]*toolState

	usage    *completions.Usage
	finished bool
	// finishReason as reported upstream; drives completed vs incomplete.
	finishReason string
	final        *responses.Response
}

type toolState struct {
	callID    string
	name      string
	arguments string
	itemID    string
	outputIdx int
	added     bool // output_item.added emitted
}

// NewStreamer creates a streamer bound to the original client request
// (used to echo configuration into the final response object).
func NewStreamer(req *responses.Request) *Streamer {
	return &Streamer{
		req:   req,
		tools: make(map[int]*toolState),
	}
}

// Feed processes one upstream chat chunk and returns the Responses events
// it maps to (possibly none, e.g. for the bare usage chunk).
func (s *Streamer) Feed(chunk *completions.StreamChunk) []*responses.Event {
	// Usage arrives on a trailing chunk with empty choices, AFTER the
	// finish_reason chunk; record it regardless of finished state.
	if chunk.Usage != nil {
		s.usage = chunk.Usage
	}
	if s.finished {
		return nil
	}
	var events []*responses.Event

	if !s.started {
		s.respID = chunk.ID
		s.created = chunk.Created
		s.model = chunk.Model
		s.started = true
		events = append(events, s.startEvents()...)
	}

	for i := range chunk.Choices {
		ch := &chunk.Choices[i]
		events = append(events, s.feedChoice(ch)...)
	}

	return events
}

// startEvents emits response.created and response.in_progress.
func (s *Streamer) startEvents() []*responses.Event {
	resp := s.skeleton()
	return []*responses.Event{
		{Type: "response.created", SequenceNumber: s.nextSeq(), Response: resp},
		{Type: "response.in_progress", SequenceNumber: s.nextSeq(), Response: s.skeleton()},
	}
}

// skeleton builds the in-progress response object echoed by lifecycle events.
func (s *Streamer) skeleton() *responses.Response {
	return &responses.Response{
		ID:                s.respID,
		Object:            "response",
		CreatedAt:         s.created,
		Status:            "in_progress",
		Model:             s.model,
		Output:            []responses.Item{},
		ParallelToolCalls: boolOrDefault(s.req.ParallelToolCalls, true),
		Tools:             s.req.Tools,
		ToolChoice:        "auto",
		Temperature:       s.req.Temperature,
		TopP:              s.req.TopP,
		MaxOutputTokens:   s.req.MaxOutputTokens,
	}
}

// feedChoice converts the deltas of one choice in one chunk.
func (s *Streamer) feedChoice(ch *completions.StreamChoice) []*responses.Event {
	var events []*responses.Event

	if r := reasoningDelta(ch.Delta); r != "" {
		if !s.reasoningOpen {
			s.reasoningOpen = true
			s.reasoningItemID = "rs_" + s.respID
			s.reasoningOutIdx = s.outputIndex
			s.outputIndex++
			events = append(events,
				&responses.Event{
					Type: "response.output_item.added", SequenceNumber: s.nextSeq(),
					OutputIndex: s.reasoningOutIdx,
					Item: &responses.Item{
						Type: "reasoning", ID: s.reasoningItemID, Status: "in_progress",
						Summary: []responses.ContentPart{},
					},
				},
				&responses.Event{
					Type: "response.reasoning_summary_part.added", SequenceNumber: s.nextSeq(),
					ItemID: s.reasoningItemID, OutputIndex: s.reasoningOutIdx, ContentIndex: 0,
					Item: &responses.Item{Type: "reasoning_summary_part", ID: s.reasoningItemID, Status: "in_progress"},
				},
			)
		}
		s.reasoningBuf = append(s.reasoningBuf, r...)
		events = append(events, &responses.Event{
			Type: "response.reasoning_summary_text.delta", SequenceNumber: s.nextSeq(),
			ItemID: s.reasoningItemID, OutputIndex: s.reasoningOutIdx, ContentIndex: 0,
			Delta: r,
		})
	}

	// Close the reasoning item as soon as anything else arrives — OpenAI's
	// native streams close reasoning before the message/tool items open,
	// and clients key on that ordering.
	if s.reasoningOpen && (ch.Delta.Content != "" || ch.Delta.Refusal != "" ||
		len(ch.Delta.ToolCalls) > 0 || (ch.FinishReason != nil && *ch.FinishReason != "")) {
		events = append(events, s.closeReasoning()...)
	}

	if ch.Delta.Content != "" {
		if !s.textItemOpen {
			s.textItemOpen = true
			s.textItemID = "msg_" + s.respID
			s.textOutputIdx = s.outputIndex
			s.outputIndex++
			item := &responses.Item{
				Type:    "message",
				ID:      s.textItemID,
				Status:  "in_progress",
				Role:    "assistant",
				Content: responses.ItemContent{Parts: []responses.ContentPart{{Type: "output_text", Text: ""}}},
			}
			events = append(events,
				&responses.Event{
					Type: "response.output_item.added", SequenceNumber: s.nextSeq(),
					OutputIndex: s.textOutputIdx, Item: item,
				},
				&responses.Event{
					Type: "response.content_part.added", SequenceNumber: s.nextSeq(),
					ItemID: s.textItemID, OutputIndex: s.textOutputIdx, ContentIndex: 0,
					Item: &responses.Item{
						Type: "output_text", ID: s.textItemID,
						Status: "in_progress", Role: "assistant",
						Content: responses.ItemContent{Parts: []responses.ContentPart{{Type: "output_text", Text: ""}}},
					},
				},
			)
		}
		s.textBuf = append(s.textBuf, ch.Delta.Content...)
		events = append(events, &responses.Event{
			Type: "response.output_text.delta", SequenceNumber: s.nextSeq(),
			ItemID: s.textItemID, OutputIndex: s.textOutputIdx, ContentIndex: 0,
			Delta: ch.Delta.Content,
		})
	}

	for _, tc := range ch.Delta.ToolCalls {
		idx := 0
		if tc.Index != nil {
			idx = *tc.Index
		}
		st, ok := s.tools[idx]
		if !ok {
			// Item IDs must be unique per call: parallel calls sharing one
			// id break clients that key items by id.
			st = &toolState{itemID: fmt.Sprintf("fc_%s_%d", s.respID, idx)}
			s.tools[idx] = st
		}
		if tc.ID != "" {
			st.callID = tc.ID
		}
		if tc.Function.Name != "" {
			st.name += tc.Function.Name
		}
		if !st.added {
			st.added = true
			st.outputIdx = s.outputIndex
			s.outputIndex++
			events = append(events, &responses.Event{
				Type: "response.output_item.added", SequenceNumber: s.nextSeq(),
				OutputIndex: st.outputIdx,
				Item: &responses.Item{
					Type: "function_call", ID: st.itemID, CallID: st.callID,
					Name: st.name, Arguments: "", Status: "in_progress",
				},
			})
		}
		if tc.Function.Arguments != "" {
			st.arguments += tc.Function.Arguments
			events = append(events, &responses.Event{
				Type: "response.function_call_arguments.delta", SequenceNumber: s.nextSeq(),
				ItemID: st.itemID, OutputIndex: st.outputIdx,
				Delta: tc.Function.Arguments,
			})
		}
	}

	if ch.Delta.Refusal != "" {
		// Upstream refusals stream as refusal deltas; surface them through
		// the message part so the terminal event carries them.
		if !s.textItemOpen {
			s.textItemOpen = true
			s.textItemID = "msg_" + s.respID
			s.textOutputIdx = s.outputIndex
			s.outputIndex++
			events = append(events, &responses.Event{
				Type: "response.output_item.added", SequenceNumber: s.nextSeq(),
				OutputIndex: s.textOutputIdx,
				Item: &responses.Item{
					Type: "message", ID: s.textItemID, Status: "in_progress", Role: "assistant",
					Content: responses.ItemContent{Parts: []responses.ContentPart{{Type: "output_text", Text: ""}}},
				},
			})
		}
		s.refusalBuf = append(s.refusalBuf, ch.Delta.Refusal...)
		events = append(events, &responses.Event{
			Type: "response.refusal.delta", SequenceNumber: s.nextSeq(),
			ItemID: s.textItemID, OutputIndex: s.textOutputIdx, ContentIndex: 0,
			Delta: ch.Delta.Refusal,
		})
	}

	if ch.FinishReason != nil && *ch.FinishReason != "" && !s.finished {
		s.finishReason = *ch.FinishReason
		events = append(events, s.closeOutput(*ch.FinishReason)...)
		s.finished = true
	}

	return events
}

// closeOutput emits the terminal sequence for all open items, in output
// order: reasoning first (if still open), then text, then tool calls
// ordered by their chat delta index.
func (s *Streamer) closeOutput(finishReason string) []*responses.Event {
	var events []*responses.Event

	events = append(events, s.closeReasoning()...)

	if s.textItemOpen {
		text := string(s.textBuf)
		refusal := string(s.refusalBuf)
		parts := s.textParts()
		if text != "" {
			events = append(events, &responses.Event{
				Type: "response.output_text.done", SequenceNumber: s.nextSeq(),
				ItemID: s.textItemID, OutputIndex: s.textOutputIdx, ContentIndex: 0, Text: text,
			})
		}
		if refusal != "" {
			events = append(events, &responses.Event{
				Type: "response.refusal.done", SequenceNumber: s.nextSeq(),
				ItemID: s.textItemID, OutputIndex: s.textOutputIdx, ContentIndex: 0, Refusal: refusal,
			})
		}
		events = append(events,
			&responses.Event{
				Type: "response.content_part.done", SequenceNumber: s.nextSeq(),
				ItemID: s.textItemID, OutputIndex: s.textOutputIdx, ContentIndex: 0,
				Item: &responses.Item{
					Type: "output_text", ID: s.textItemID, Status: "completed", Role: "assistant",
					Content: responses.ItemContent{Parts: parts},
				},
			},
			&responses.Event{
				Type: "response.output_item.done", SequenceNumber: s.nextSeq(),
				OutputIndex: s.textOutputIdx,
				Item: &responses.Item{
					Type: "message", ID: s.textItemID, Status: "completed", Role: "assistant",
					Content: responses.ItemContent{Parts: parts},
				},
			},
		)
		s.textItemOpen = false
	}

	for _, idx := range sortedToolIndexes(s.tools) {
		st := s.tools[idx]
		if !st.added {
			continue
		}
		events = append(events,
			&responses.Event{
				Type: "response.function_call_arguments.done", SequenceNumber: s.nextSeq(),
				ItemID: st.itemID, OutputIndex: st.outputIdx, Arguments: st.arguments,
			},
			&responses.Event{
				Type: "response.output_item.done", SequenceNumber: s.nextSeq(),
				OutputIndex: st.outputIdx,
				Item: &responses.Item{
					Type: "function_call", ID: st.itemID, CallID: st.callID,
					Name: st.name, Arguments: st.arguments, Status: "completed",
				},
			},
		)
	}

	return events
}

// Finish emits the terminal response.completed (or response.incomplete
// when upstream reported length/content_filter) event carrying the
// aggregated response object. It must be called after the upstream [DONE]
// sentinel; if the upstream stream ended without a finish_reason it closes
// any open items gracefully first.
func (s *Streamer) Finish() []*responses.Event {
	var out []*responses.Event
	if !s.finished {
		out = append(out, s.closeOutput("stop")...)
		s.finished = true
	}

	resp := s.skeleton()
	switch s.finishReason {
	case "length":
		resp.Status = "incomplete"
		resp.IncompleteDetails = &responses.IncompleteDetails{Reason: "max_output_tokens"}
	case "content_filter":
		resp.Status = "incomplete"
		resp.IncompleteDetails = &responses.IncompleteDetails{Reason: "content_filter"}
	case "refusal":
		resp.Status = "incomplete"
	default:
		resp.Status = "completed"
	}

	for _, idx := range sortedToolIndexes(s.tools) {
		st := s.tools[idx]
		resp.Output = append(resp.Output, responses.Item{
			Type: "function_call", ID: st.itemID, CallID: st.callID,
			Name: st.name, Arguments: st.arguments, Status: "completed",
		})
	}
	if s.textItemOpen || len(s.textBuf) > 0 || len(s.refusalBuf) > 0 {
		resp.Output = append([]responses.Item{{
			Type: "message", ID: s.textItemID, Status: "completed", Role: "assistant",
			Content: responses.ItemContent{Parts: s.textParts()},
		}}, resp.Output...)
	}
	if len(s.reasoningBuf) > 0 {
		resp.Output = append([]responses.Item{{
			Type: "reasoning", ID: s.reasoningItemID, Status: "completed",
			Summary: []responses.ContentPart{{Type: "summary_text", Text: string(s.reasoningBuf)}},
		}}, resp.Output...)
	}

	resp.Usage = convertUsage(s.usage)
	s.final = resp

	eventType := "response.completed"
	if resp.Status == "incomplete" {
		eventType = "response.incomplete"
	}
	out = append(out, &responses.Event{
		Type: eventType, SequenceNumber: s.nextSeq(), Response: resp,
	})
	return out
}

// closeReasoning emits the terminal sequence for the reasoning item and
// marks it closed; it is a no-op when reasoning never streamed.
func (s *Streamer) closeReasoning() []*responses.Event {
	if !s.reasoningOpen {
		return nil
	}
	s.reasoningOpen = false
	sum := string(s.reasoningBuf)
	return []*responses.Event{
		{
			Type: "response.reasoning_summary_text.done", SequenceNumber: s.nextSeq(),
			ItemID: s.reasoningItemID, OutputIndex: s.reasoningOutIdx, ContentIndex: 0, Text: sum,
		},
		{
			Type: "response.reasoning_summary_part.done", SequenceNumber: s.nextSeq(),
			ItemID: s.reasoningItemID, OutputIndex: s.reasoningOutIdx, ContentIndex: 0,
			Item: &responses.Item{
				Type: "reasoning_summary_part", ID: s.reasoningItemID, Status: "completed",
				Content: responses.ItemContent{Parts: []responses.ContentPart{{Type: "summary_text", Text: sum}}},
			},
		},
		{
			Type: "response.output_item.done", SequenceNumber: s.nextSeq(),
			OutputIndex: s.reasoningOutIdx,
			Item: &responses.Item{
				Type: "reasoning", ID: s.reasoningItemID, Status: "completed",
				Summary: []responses.ContentPart{{Type: "summary_text", Text: sum}},
			},
		},
	}
}

// textParts assembles the final content parts of the assistant message:
// output text when present, refusal when present, and a single empty
// output_text part when neither streamed anything.
func (s *Streamer) textParts() []responses.ContentPart {
	parts := make([]responses.ContentPart, 0, 2)
	if len(s.textBuf) > 0 {
		parts = append(parts, responses.ContentPart{Type: "output_text", Text: string(s.textBuf)})
	}
	if len(s.refusalBuf) > 0 {
		parts = append(parts, responses.ContentPart{Type: "refusal", Refusal: string(s.refusalBuf)})
	}
	if len(parts) == 0 {
		parts = append(parts, responses.ContentPart{Type: "output_text", Text: ""})
	}
	return parts
}

// reasoningDelta extracts the thinking-trace increment of a chat delta,
// accepting both spellings providers use.
func reasoningDelta(d completions.Delta) string {
	if d.ReasoningContent != "" {
		return d.ReasoningContent
	}
	return d.Reasoning
}

// FinalResponse returns the aggregated response object emitted by the last
// response.completed event, for metrics accounting by the caller.
func (s *Streamer) FinalResponse() *responses.Response {
	return s.final
}

func (s *Streamer) nextSeq() int64 {
	s.seq++
	return s.seq
}

func sortedToolIndexes(m map[int]*toolState) []int {
	idxs := make([]int, 0, len(m))
	for k := range m {
		idxs = append(idxs, k)
	}
	for i := 1; i < len(idxs); i++ {
		for j := i; j > 0 && idxs[j] < idxs[j-1]; j-- {
			idxs[j], idxs[j-1] = idxs[j-1], idxs[j]
		}
	}
	return idxs
}
