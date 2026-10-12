package server

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/dncore/synapse-protocol-adapter/internal/converter"
)

// Model-quirk normalization for the transparent routes.
//
// The passthrough routes forward bodies byte for byte; this file is the one
// deliberate exception, gated behind passthrough.model_quirks. It touches
// only the model families whose upstream refuses the shape a client sends —
// today the gpt-6 family on the shared gateway, where the following are
// measured facts (2026-10-10, live gateway; per-model detail in
// internal/converter/modelcaps.go):
//
//   - `max_tokens` is rejected ("Use 'max_completion_tokens' instead");
//     sending both fields is rejected too, so the rename replaces it.
//   - a tool-bearing request must carry an explicit `reasoning_effort` from
//     the model's own accepted set, and must not carry temperature/top_p:
//     the Sol ids reject "none" outright and 400 (4/4) on any sampling
//     parameter, and an omitted effort is not reliable either.
//   - luna keeps the older shape (tools ⇒ explicit "none") because that is
//     the only value that was observed to work on every channel for it.
//   - on the Anthropic mount the body carries max_tokens (mandatory there)
//     and clients may send `thinking`; the upstream forwards both into the
//     chat request, which the model then rejects. Neither can be expressed
//     as "reasoning off" through that mount, so tools+temperature requests
//     still fail there — a limitation of the upstream translator, not of
//     this rewriter.
//
// The converter's dialect rules (internal/converter/request.go) implement
// the same behavior for the /responses route, which builds a typed request
// instead of editing raw JSON.

// rewriteRules names the rules a request triggered, for metrics labels.
const (
	ruleMaxTokens       = "max_tokens"
	ruleReasoningEffort = "reasoning_effort"
	ruleSampling        = "sampling"
	ruleThinking        = "thinking"
)

// normalizeModelQuirks applies the model-family fixups to a raw request body.
// It returns the original slice when nothing applies, so callers can compare
// by identity (or use the changed flag) and keep the byte-for-byte promise
// for every body it does not understand: unparsable JSON, a body without a
// string `model`, or a model outside the affected families are all returned
// untouched.
func normalizeModelQuirks(path string, body []byte) (out []byte, rules []string) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(body, &doc); err != nil || doc == nil {
		return body, nil
	}
	var model string
	if err := json.Unmarshal(doc["model"], &model); err != nil || !converter.IsGPT6(model) {
		return body, nil
	}

	switch {
	case isAnthropicMessagesPath(path):
		rules = normalizeAnthropic(doc)
	case isChatCompletionsPath(path):
		rules = normalizeChat(model, doc)
	}
	if len(rules) == 0 {
		return body, nil
	}
	// A marshal of map[string]json.RawMessage cannot fail on values we just
	// parsed; if it somehow does, fall back to the original bytes rather
	// than dropping the request body.
	rewritten, err := json.Marshal(doc)
	if err != nil {
		return body, nil
	}
	return rewritten, rules
}

// normalizeChat applies the chat-completions rules in place.
func normalizeChat(model string, doc map[string]json.RawMessage) []string {
	var rules []string

	// max_tokens → max_completion_tokens. The rename replaces the field:
	// with both present the model still sees max_tokens and rejects it.
	if raw, ok := doc["max_tokens"]; ok {
		if _, exists := doc["max_completion_tokens"]; !exists {
			doc["max_completion_tokens"] = raw
		}
		delete(doc, "max_tokens")
		rules = append(rules, ruleMaxTokens)
	}

	effort := ""
	if raw, ok := doc["reasoning_effort"]; ok {
		_ = json.Unmarshal(raw, &effort)
	}
	hasTools := hasNonEmptyArray(doc["tools"])

	// The shape decision belongs to the converter: it knows each model's
	// accepted effort set (tools ⇒ an explicit value from that set, no tools
	// ⇒ keep an accepted value, drop a rejected one).
	effective := converter.NormalizeGPT6Effort(model, effort, hasTools)
	if effective != effort {
		if effective == "" {
			delete(doc, "reasoning_effort")
		} else {
			encoded, err := json.Marshal(effective)
			if err != nil {
				return rules
			}
			doc["reasoning_effort"] = encoded
		}
		rules = append(rules, ruleReasoningEffort)
	}

	if converter.GPT6StripsSampling(model, hasTools, effective) {
		for _, field := range []string{"temperature", "top_p"} {
			if _, ok := doc[field]; ok {
				delete(doc, field)
				rules = append(rules, ruleSampling)
			}
		}
	}
	return rules
}

// normalizeAnthropic applies the Anthropic-mount rules in place. Everything
// the upstream's anthropic→chat translator would forward into a rejected
// chat field is dropped or renamed here.
func normalizeAnthropic(doc map[string]json.RawMessage) []string {
	var rules []string
	if raw, ok := doc["max_tokens"]; ok {
		if _, exists := doc["max_completion_tokens"]; !exists {
			doc["max_completion_tokens"] = raw
		}
		delete(doc, "max_tokens")
		rules = append(rules, ruleMaxTokens)
	}
	// `thinking` is forwarded verbatim to the chat request, where the model
	// rejects it ("Unknown parameter: 'thinking'"). Effort cannot be set to
	// "none" through this mount, so thinking has no usable form at all.
	if _, ok := doc["thinking"]; ok {
		delete(doc, "thinking")
		rules = append(rules, ruleThinking)
	}
	// Sampling params can never be honored here for the same reason: the
	// translated chat request always has reasoning on.
	for _, field := range []string{"temperature", "top_p"} {
		if _, ok := doc[field]; ok {
			delete(doc, field)
			rules = append(rules, ruleSampling)
		}
	}
	return rules
}

// isAnthropicMessagesPath reports whether the path targets the Anthropic
// mount's message endpoint (…/api/anthropic/v1/messages).
func isAnthropicMessagesPath(path string) bool {
	return strings.Contains(path, "/api/anthropic/") && strings.HasSuffix(path, "/messages")
}

// isChatCompletionsPath reports whether the path targets a chat completions
// endpoint — the one body shape the chat rules understand.
func isChatCompletionsPath(path string) bool {
	return strings.HasSuffix(path, "/chat/completions")
}

// hasNonEmptyArray reports whether a raw JSON value is an array with at
// least one element. Absent, null and [] all count as "no tools".
func hasNonEmptyArray(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && trimmed[0] == '[' && !bytes.Equal(trimmed, []byte("[]"))
}

// substituteUnavailableModel rewrites the body's "model" when the requested
// one is marked unavailable and a fallback is configured for it. It returns
// the original slice when nothing applies, so the passthrough route keeps its
// byte-for-byte promise for every body it does not positively recognize.
func (s *Server) substituteUnavailableModel(body []byte) (out []byte, from, to string) {
	if len(s.cfg.ModelHealth.Fallbacks) == 0 || len(body) == 0 {
		return body, "", ""
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(body, &doc); err != nil || doc == nil {
		return body, "", ""
	}
	var model string
	if err := json.Unmarshal(doc["model"], &model); err != nil || model == "" {
		return body, "", ""
	}
	fallback, ok := s.substitutedModel(model, hasNonEmptyArray(doc["tools"]))
	if !ok {
		return body, "", ""
	}
	encoded, err := json.Marshal(fallback)
	if err != nil {
		return body, "", ""
	}
	doc["model"] = encoded
	rewritten, err := json.Marshal(doc)
	if err != nil {
		return body, "", ""
	}
	return rewritten, model, fallback
}
