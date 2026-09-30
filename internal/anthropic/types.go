// Package anthropic implements the targeted Anthropic Messages ⇄ Chat
// Completions translation used when the shared gateway's own Anthropic route
// cannot serve a request (see internal/server/anthropic.go for the election
// rule).
//
// It is deliberately NOT a complete Anthropic adapter: it maps exactly the
// shapes Claude Code sends, and answers ErrUnsupported for anything else so
// the caller can fall back to plain passthrough. Nothing is ever dropped
// silently — an unknown content block fails the conversion instead.
package anthropic

import "encoding/json"

// Request is the subset of an Anthropic Messages request this package maps.
// Unknown top-level fields (output_config, context_management, safeguards,
// metadata, β flags …) are simply not modeled: the upstream's own translator
// reads them, and the takeover path deliberately does not depend on them.
type Request struct {
	Model         string          `json:"model"`
	MaxTokens     *int            `json:"max_tokens"`
	System        json.RawMessage `json:"system"` // string or []Block
	Messages      []Message       `json:"messages"`
	Tools         []Tool          `json:"tools"`
	ToolChoice    json.RawMessage `json:"tool_choice"`
	Temperature   *float64        `json:"temperature"`
	TopP          *float64        `json:"top_p"`
	StopSequences []string        `json:"stop_sequences"`
	Stream        bool            `json:"stream"`
	Thinking      json.RawMessage `json:"thinking"`
}

// Message is one conversation turn.
type Message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"` // string or []Block
}

// Block is one content block inside a message.
type Block struct {
	Type string `json:"type"`

	// type == "text"
	Text string `json:"text"`

	// type == "image"
	Source *ImageSource `json:"source"`

	// type == "tool_use"
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`

	// type == "tool_result"
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"` // string or []Block
	IsError   bool            `json:"is_error"`
}

// ImageSource carries an inline (base64) or referenced image.
type ImageSource struct {
	Type      string `json:"type"` // base64 | url
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
	URL       string `json:"url"`
}

// Tool is a client-side function definition.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// ToolChoice is the Anthropic tool_choice object form.
type ToolChoice struct {
	Type string `json:"type"` // auto | any | tool | none
	Name string `json:"name"`
}

// Response is an Anthropic Messages response.
type Response struct {
	ID           string        `json:"id"`
	Type         string        `json:"type"`
	Role         string        `json:"role"`
	Model        string        `json:"model"`
	Content      []OutBlock    `json:"content"`
	StopReason   string        `json:"stop_reason"`
	StopSequence *string       `json:"stop_sequence"`
	Usage        ResponseUsage `json:"usage"`
}

// OutBlock is one content block of a response (text or tool_use).
type OutBlock struct {
	Type  string          `json:"type"`
	Text  string          `json:"text,omitempty"`
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

// ResponseUsage mirrors Anthropic's token accounting for what the chat route
// reports (cache fields are omitted: the gateway does not expose them).
type ResponseUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// ErrorResponse is the Anthropic error envelope.
type ErrorResponse struct {
	Type  string    `json:"type"`
	Error ErrorBody `json:"error"`
}

// ErrorBody is the inner error object.
type ErrorBody struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}
