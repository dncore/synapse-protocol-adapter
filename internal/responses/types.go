// Package responses defines the OpenAI Responses API wire types, which are
// the proxy's client-facing protocol. Types intentionally mirror the public
// API shapes so clients such as Codex can point at the proxy unchanged.
package responses

import (
	"encoding/json"
	"errors"
)

// Request is the client request body for POST /v1/responses.
type Request struct {
	Model               string           `json:"model"`
	Input               Input            `json:"input"`
	Instructions        string           `json:"instructions,omitempty"`
	MaxOutputTokens     *int             `json:"max_output_tokens,omitempty"`
	Metadata            json.RawMessage  `json:"metadata,omitempty"`
	ParallelToolCalls   *bool            `json:"parallel_tool_calls,omitempty"`
	PreviousResponseID  *string          `json:"previous_response_id,omitempty"`
	Reasoning           *Reasoning       `json:"reasoning,omitempty"`
	Store               *bool            `json:"store,omitempty"`
	Temperature         *float64         `json:"temperature,omitempty"`
	TopP                *float64         `json:"top_p,omitempty"`
	Tools               []Tool           `json:"tools,omitempty"`
	ToolChoice          json.RawMessage  `json:"tool_choice,omitempty"`
	ResponseFormat      json.RawMessage  `json:"response_format,omitempty"`
	Text                *TextFormat      `json:"text,omitempty"`
	Stream              bool             `json:"stream,omitempty"`
	Include             []string         `json:"include,omitempty"`
	Truncation          string           `json:"truncation,omitempty"`
	User                string           `json:"user,omitempty"`
}

// Reasoning carries thinking-effort hints. Chat completions has no
// equivalent, so the converter drops it.
type Reasoning struct {
	Effort  string `json:"effort,omitempty"`
	Summary string `json:"summary,omitempty"`
}

// TextFormat is the legacy text-format container; its Format field holds
// the same shape as ResponseFormat.
type TextFormat struct {
	Format json.RawMessage `json:"format,omitempty"`
}

// Input is the request input: a plain string (one user turn) or a list of
// content items covering the whole conversation history.
type Input struct {
	String string
	Items  []Item
}

func (i Input) MarshalJSON() ([]byte, error) {
	if i.Items != nil {
		return json.Marshal(i.Items)
	}
	return json.Marshal(i.String)
}

func (i *Input) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		return json.Unmarshal(b, &i.String)
	}
	if string(b) == "null" {
		return nil
	}
	return json.Unmarshal(b, &i.Items)
}

// Item is one entry of the input item list. Items are distinguished by
// their Type field; only the fields relevant to each type are populated.
type Item struct {
	// Common
	Type   string `json:"type"` // "message" | "function_call" | "function_call_output" | ...
	ID     string `json:"id,omitempty"`
	Status string `json:"status,omitempty"`

	// message
	Role    string        `json:"role,omitempty"`
	Content ItemContent   `json:"content,omitempty"`

	// function_call
	Name      string `json:"name,omitempty"`
	CallID    string `json:"call_id,omitempty"`
	Arguments string `json:"arguments,omitempty"`

	// function_call_output
	Output string `json:"output,omitempty"`
}

// ItemContent is message content: a plain string or a list of typed parts.
type ItemContent struct {
	String string
	Parts  []ContentPart
}

func (c ItemContent) MarshalJSON() ([]byte, error) {
	if c.Parts != nil {
		return json.Marshal(c.Parts)
	}
	return json.Marshal(c.String)
}

func (c *ItemContent) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		return json.Unmarshal(b, &c.String)
	}
	return json.Unmarshal(b, &c.Parts)
}

// ContentPart is one typed part of a message: input_text, output_text,
// input_image, summary_text, ...
type ContentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"` // Responses spells it image_url (string) on input
	Detail   string `json:"detail,omitempty"`
}

// Tool is a function tool definition in Responses shape (flattened,
// unlike chat completions' nested form).
type Tool struct {
	Type        string          `json:"type"` // "function"
	Name        string          `json:"name,omitempty"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`
}

// Response is the non-streamed Responses API response object.
type Response struct {
	ID                string             `json:"id"`
	Object            string             `json:"object"`
	CreatedAt         int64              `json:"created_at"`
	Status            string             `json:"status"` // "completed" | "incomplete" | "failed"
	IncompleteDetails *IncompleteDetails `json:"incomplete_details,omitempty"`
	Model             string             `json:"model"`
	Output            []Item             `json:"output"`
	ParallelToolCalls bool               `json:"parallel_tool_calls"`
	Tools             []Tool             `json:"tools"`
	ToolChoice        string             `json:"tool_choice"`
	Temperature       *float64           `json:"temperature,omitempty"`
	TopP              *float64           `json:"top_p,omitempty"`
	MaxOutputTokens   *int               `json:"max_output_tokens,omitempty"`
	Usage             *Usage             `json:"usage,omitempty"`
}

// IncompleteDetails explains why a response ended incomplete.
type IncompleteDetails struct {
	Reason string `json:"reason"` // "max_output_tokens" | "content_filter"
}

// Usage is the Responses-style token accounting block.
type Usage struct {
	InputTokens         int                  `json:"input_tokens"`
	OutputTokens        int                  `json:"output_tokens"`
	TotalTokens         int                  `json:"total_tokens"`
	OutputTokensDetails *OutputTokensDetails `json:"output_tokens_details,omitempty"`
}

// OutputTokensDetails breaks down output token accounting.
type OutputTokensDetails struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

// Event is one SSE event of a streamed Responses API session. Which fields
// are meaningful depends on Type; see the API's event reference.
type Event struct {
	Type           string    `json:"type"`
	SequenceNumber int64     `json:"sequence_number"`
	Response       *Response `json:"response,omitempty"`
	Item           *Item     `json:"item,omitempty"`
	OutputIndex    int       `json:"output_index"`
	ItemID         string    `json:"item_id,omitempty"`
	ContentIndex   int       `json:"content_index"`
	Delta          string    `json:"delta,omitempty"`
	Text           string    `json:"text,omitempty"`
	Arguments      string    `json:"arguments,omitempty"`
	Name           string    `json:"name,omitempty"`
	CallID         string    `json:"call_id,omitempty"`
}

// ErrItemUnsupported is returned by the converter for input item types it
// cannot translate.
var ErrItemUnsupported = errors.New("unsupported input item type")
