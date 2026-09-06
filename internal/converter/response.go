package converter

import (
	"github.com/dncore/synapse-protocol-adapter/internal/completions"
	"github.com/dncore/synapse-protocol-adapter/internal/responses"
)

// ConvertResponse translates a non-streamed chat completions response into
// a Responses API response object. It is pure: derived item IDs are
// deterministic functions of the upstream response ID.
func ConvertResponse(chat *completions.Response, req *responses.Request) *responses.Response {
	out := &responses.Response{
		ID:               chat.ID,
		Object:           "response",
		CreatedAt:        chat.Created,
		Model:             chat.Model,
		ParallelToolCalls: boolOrDefault(req.ParallelToolCalls, true),
		ToolChoice:        "auto",
		Temperature:       req.Temperature,
		TopP:              req.TopP,
		MaxOutputTokens:   req.MaxOutputTokens,
		Usage:             convertUsage(chat.Usage),
		Tools:             req.Tools,
	}
	if out.ID == "" {
		out.ID = "resp_unknown"
	}
	if len(chat.Choices) == 0 {
		out.Status = "failed"
		out.Output = []responses.Item{}
		return out
	}

	choice := &chat.Choices[0]
	out.Status = "completed"
	switch choice.FinishReason {
	case "length":
		out.Status = "incomplete"
		out.IncompleteDetails = &responses.IncompleteDetails{Reason: "max_output_tokens"}
	case "content_filter":
		out.Status = "incomplete"
		out.IncompleteDetails = &responses.IncompleteDetails{Reason: "content_filter"}
	case "refusal":
		// The refusal itself rides in the message content parts; the
		// overall turn is still reported incomplete, matching upstream
		// adapters.
		out.Status = "incomplete"
	}

	if rc := choice.Message.ReasoningContent; rc != "" {
		out.Output = append(out.Output, responses.Item{
			Type:   "reasoning",
			ID:     "rs_" + chat.ID,
			Status: "completed",
			Summary: []responses.ContentPart{{
				Type: "summary_text",
				Text: rc,
			}},
		})
	}

	if content := messageText(choice.Message); content != "" || choice.Message.Refusal != "" {
		parts := []responses.ContentPart{}
		if content != "" {
			parts = append(parts, responses.ContentPart{Type: "output_text", Text: content})
		}
		if choice.Message.Refusal != "" {
			parts = append(parts, responses.ContentPart{Type: "refusal", Refusal: choice.Message.Refusal})
		}
		out.Output = append(out.Output, responses.Item{
			Type:    "message",
			ID:      "msg_" + chat.ID,
			Status:  "completed",
			Role:    "assistant",
			Content: responses.ItemContent{Parts: parts},
		})
	}

	for _, tc := range choice.Message.ToolCalls {
		out.Output = append(out.Output, responses.Item{
			Type:      "function_call",
			ID:        "fc_" + chat.ID,
			CallID:    tc.ID,
			Name:      tc.Function.Name,
			Arguments: tc.Function.Arguments,
			Status:    "completed",
		})
	}

	if out.Output == nil {
		out.Output = []responses.Item{}
	}
	return out
}

// messageText extracts the text of a chat assistant message, joining
// multimodal text parts if the provider returned parts.
func messageText(m completions.Message) string {
	if m.Content.String != "" {
		return m.Content.String
	}
	s := ""
	for _, p := range m.Content.Parts {
		if p.Type == "text" {
			s += p.Text
		}
	}
	return s
}

// convertUsage maps chat completions token names to responses token names,
// preserving provider-reported cache and reasoning breakdowns.
func convertUsage(u *completions.Usage) *responses.Usage {
	if u == nil {
		return nil
	}
	out := &responses.Usage{
		InputTokens:  u.PromptTokens,
		OutputTokens: u.CompletionTokens,
		TotalTokens:  u.TotalTokens,
	}
	if u.PromptTokensDetails != nil && u.PromptTokensDetails.CachedTokens > 0 {
		out.InputTokensDetails = &responses.InputTokensDetails{
			CachedTokens: u.PromptTokensDetails.CachedTokens,
		}
	}
	if u.CompletionTokensDetails != nil {
		out.OutputTokensDetails = &responses.OutputTokensDetails{
			ReasoningTokens: u.CompletionTokensDetails.ReasoningTokens,
		}
	}
	return out
}

func boolOrDefault(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}
