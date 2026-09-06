// Package completions defines the OpenAI Chat Completions API wire types.
// The proxy treats this protocol as its intermediate representation:
// Responses requests are converted into a ChatRequest, and both streamed
// and non-streamed upstream output is converted back into Responses types.
package completions

import "encoding/json"

// Request is the upstream request body (chat completions).
type Request struct {
	Model              string          `json:"model"`
	Messages           []Message       `json:"messages"`
	Tools              []Tool          `json:"tools,omitempty"`
	ToolChoice         json.RawMessage `json:"tool_choice,omitempty"`
	Temperature        *float64        `json:"temperature,omitempty"`
	TopP               *float64        `json:"top_p,omitempty"`
	MaxTokens          *int            `json:"max_tokens,omitempty"`
	Stream             bool            `json:"stream,omitempty"`
	StreamOptions      *StreamOptions  `json:"stream_options,omitempty"`
	ParallelToolCalls  *bool           `json:"parallel_tool_calls,omitempty"`
	ResponseFormat     json.RawMessage `json:"response_format,omitempty"`
	User               string          `json:"user,omitempty"`
	Seed               *int            `json:"seed,omitempty"`
	Stop               []string        `json:"stop,omitempty"`
	FrequencyPenalty   *float64        `json:"frequency_penalty,omitempty"`
	PresencePenalty    *float64        `json:"presence_penalty,omitempty"`
	LogitBias          map[string]int  `json:"logit_bias,omitempty"`
}

// StreamOptions requests usage accounting on the final streamed chunk.
type StreamOptions struct {
	IncludeUsage bool `json:"include_usage,omitempty"`
}

// Message is one chat message. Content is either a string or a slice of
// content parts depending on role and provider; both shapes are accepted.
type Message struct {
	Role         string         `json:"role"`
	Content      MessageContent `json:"content,omitempty"`
	Name         string         `json:"name,omitempty"`
	ToolCallID   string         `json:"tool_call_id,omitempty"`
	ToolCalls    []ToolCall     `json:"tool_calls,omitempty"`
}

// MessageContent marshals as either a JSON string or an array of parts.
// A nil slice with empty String marshals as null; callers should set
// exactly one of the two fields.
type MessageContent struct {
	String string
	Parts  []ContentPart
}

func (m MessageContent) MarshalJSON() ([]byte, error) {
	if m.Parts != nil {
		return json.Marshal(m.Parts)
	}
	return json.Marshal(m.String)
}

func (m *MessageContent) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		return json.Unmarshal(b, &m.String)
	}
	return json.Unmarshal(b, &m.Parts)
}

// Empty reports whether the content carries no payload at all.
func (m MessageContent) Empty() bool {
	return m.String == "" && m.Parts == nil
}

// ContentPart is a multimodal content part of a chat message.
type ContentPart struct {
	Type     string    `json:"type"` // "text" | "image_url"
	Text     string    `json:"text,omitempty"`
	ImageURL *ImageURL `json:"image_url,omitempty"`
}

// ImageURL is the image reference of an image_url content part. Either a
// data: URI or an https: URL.
type ImageURL struct {
	URL string `json:"url,omitempty"`
}

// Tool is a function tool definition in chat completions shape.
type Tool struct {
	Type     string   `json:"type"` // always "function" today
	Function Function `json:"function"`
}

// Function is the function descriptor nested inside a chat Tool.
type Function struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`
}

// ToolCall is an assistant-requested tool invocation.
type ToolCall struct {
	// Index is only meaningful inside streamed deltas, where it identifies
	// which accumulated call a delta fragment belongs to.
	Index    *int     `json:"index,omitempty"`
	ID       string   `json:"id,omitempty"`
	Type     string   `json:"type,omitempty"`
	Function FuncCall `json:"function"`
}

// FuncCall carries the function name and (possibly partial, when streamed)
// JSON arguments of a tool call.
type FuncCall struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// Response is the upstream non-streamed response body.
type Response struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   *Usage   `json:"usage,omitempty"`
}

// Choice is one completion alternative. The proxy forwards only the first.
type Choice struct {
	Index        int     `json:"index"`
	Message      Message `json:"message"`
	FinishReason string  `json:"finish_reason"`
}

// Usage is the token accounting block.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// StreamChunk is one SSE data payload of a streamed chat completion.
type StreamChunk struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []StreamChoice `json:"choices"`
	Usage   *Usage        `json:"usage,omitempty"`
}

// StreamChoice is one choice inside a StreamChunk. Delta carries the
// incremental payload; FinishReason is set only on the terminating chunk
// for that choice.
type StreamChoice struct {
	Index        int     `json:"index"`
	Delta        Delta   `json:"delta"`
	FinishReason *string `json:"finish_reason"`
}

// Delta is the incremental content of a streamed choice.
type Delta struct {
	Role      string     `json:"role,omitempty"`
	Content   string     `json:"content"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
}

// Error is the upstream error envelope: {"error":{...}}.
type Error struct {
	Error ErrorBody `json:"error"`
}

// ErrorBody is the payload of an upstream error response.
type ErrorBody struct {
	Message string  `json:"message"`
	Type    string  `json:"type,omitempty"`
	Code    string  `json:"code,omitempty"`
	Param   string  `json:"param,omitempty"`
}
