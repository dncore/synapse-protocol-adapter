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
// measured facts (2026-09-30, live gateway):
//
//   - `max_tokens` is rejected ("Use 'max_completion_tokens' instead");
//     sending both fields is rejected too, so the rename replaces it.
//   - function tools together with `reasoning_effort` are rejected, and an
//     omitted effort counts as non-none — a tools request must send "none".
//   - `temperature`/`top_p` are rejected while reasoning is on ("does not
//     support 0.7 with this model. Only the default (1) value is supported")
//     but accepted once the effort is "none".
//   - on the Anthropic mount the body carries max_tokens (mandatory there)
//     and clients may send `thinking`; the upstream forwards both into the
//     chat request, which the model then rejects. Neither can be expressed
//     as "reasoning off" through that mount, so tools+temperature requests
//     still fail there — a limitation of the upstream translator, not of
//     this rewriter.
//
// The converter's dialect rules (internal/converter/request.go) implement
// the same family behavior for the /responses route, which builds a typed
// request instead of editing raw JSON.

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
		rules = normalizeChat(doc)
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
func normalizeChat(doc map[string]json.RawMessage) []string {
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

	// With tools the only accepted effort is "none" — the gateway reads an
	// omitted effort as non-none, so it must be sent explicitly.
	if hasTools && effort != "none" {
		doc["reasoning_effort"] = json.RawMessage(`"none"`)
		effort = "none"
		rules = append(rules, ruleReasoningEffort)
	}

	// Reasoning on (no tools, effort not "none") refuses sampling params.
	if effort != "none" {
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
