package converter

import (
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

	// Text output state.
	textItemOpen  bool
	textItemID    string
	textBuf       []byte
	textOutputIdx int
	outputIndex   int // next output index to assign

	// Tool call accumulators, keyed by chat delta index.
	tools map[int]*toolState

	usage    *completions.Usage
	finished bool
	final    *responses.Response
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
			st = &toolState{itemID: "fc_" + s.respID}
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

	if ch.FinishReason != nil && *ch.FinishReason != "" && !s.finished {
		events = append(events, s.closeOutput(*ch.FinishReason)...)
		s.finished = true
	}

	return events
}

// closeOutput emits the terminal sequence for all open items, in output
// order: text item first (it opened first), then tool calls ordered by
// their chat delta index.
func (s *Streamer) closeOutput(finishReason string) []*responses.Event {
	var events []*responses.Event

	if s.textItemOpen {
		text := string(s.textBuf)
		events = append(events,
			&responses.Event{
				Type: "response.output_text.done", SequenceNumber: s.nextSeq(),
				ItemID: s.textItemID, OutputIndex: s.textOutputIdx, ContentIndex: 0, Text: text,
			},
			&responses.Event{
				Type: "response.content_part.done", SequenceNumber: s.nextSeq(),
				ItemID: s.textItemID, OutputIndex: s.textOutputIdx, ContentIndex: 0,
				Item: &responses.Item{
					Type: "output_text", ID: s.textItemID, Status: "completed", Role: "assistant",
					Content: responses.ItemContent{Parts: []responses.ContentPart{{Type: "output_text", Text: text}}},
				},
			},
			&responses.Event{
				Type: "response.output_item.done", SequenceNumber: s.nextSeq(),
				OutputIndex: s.textOutputIdx,
				Item: &responses.Item{
					Type: "message", ID: s.textItemID, Status: "completed", Role: "assistant",
					Content: responses.ItemContent{Parts: []responses.ContentPart{{Type: "output_text", Text: text}}},
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

// Finish emits the final response.completed event carrying the aggregated
// response object. It must be called after the upstream [DONE] sentinel;
// if the upstream stream ended without a finish_reason it closes any open
// items gracefully first.
func (s *Streamer) Finish() []*responses.Event {
	var out []*responses.Event
	if !s.finished {
		out = append(out, s.closeOutput("stop")...)
		s.finished = true
	}

	resp := s.skeleton()
	resp.Status = "completed"

	for _, idx := range sortedToolIndexes(s.tools) {
		st := s.tools[idx]
		resp.Output = append(resp.Output, responses.Item{
			Type: "function_call", ID: st.itemID, CallID: st.callID,
			Name: st.name, Arguments: st.arguments, Status: "completed",
		})
	}
	if s.textItemOpen || len(s.textBuf) > 0 {
		resp.Output = append([]responses.Item{{
			Type: "message", ID: s.textItemID, Status: "completed", Role: "assistant",
			Content: responses.ItemContent{Parts: []responses.ContentPart{{Type: "output_text", Text: string(s.textBuf)}}},
		}}, resp.Output...)
	}

	resp.Usage = convertUsage(s.usage)
	s.final = resp
	out = append(out, &responses.Event{
		Type: "response.completed", SequenceNumber: s.nextSeq(), Response: resp,
	})
	return out
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
