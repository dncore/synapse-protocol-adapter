package server

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"
	"testing"
)

// The bodies below are the shapes real clients send, as captured against the
// live upstream on 2026-09-30 (grok CLI / opencode: max_tokens + tools, no
// effort; Claude Code: Anthropic mount with max_tokens, thinking, tools).
// Each rewritten expectation is the shape the same upstream answered 200 to.
func TestNormalizeModelQuirks(t *testing.T) {
	const chatPath = "/api/v1/chat/completions"
	const anthropicPath = "/api/anthropic/v1/messages"

	tests := []struct {
		name string
		path string
		body string
		// unchanged means "returned byte-identical" — the passthrough promise
		// holds for everything the rewriter does not positively recognize.
		unchanged bool
		rules     []string
		// check runs on the rewritten document (skipped when unchanged).
		check func(t *testing.T, doc map[string]json.RawMessage)
	}{
		{
			name:  "chat: grok/opencode shape (max_tokens + tools, no effort) gets the working shape",
			path:  chatPath,
			body:  `{"model":"gpt-6-luna","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"read_file"}}],"max_tokens":32000,"stream":true}`,
			rules: []string{ruleMaxTokens, ruleReasoningEffort},
			check: func(t *testing.T, doc map[string]json.RawMessage) {
				wantRaw(t, doc, "max_completion_tokens", "32000")
				if _, ok := doc["max_tokens"]; ok {
					t.Fatal("max_tokens must be gone: the model rejects a request that still carries it")
				}
				wantRaw(t, doc, "reasoning_effort", `"none"`)
				wantRaw(t, doc, "stream", "true")
			},
		},
		{
			name:  "chat: both cap fields present — max_tokens is dropped, not kept beside the rename",
			path:  chatPath,
			body:  `{"model":"gpt-6-luna","messages":[],"tools":[{"type":"function","function":{"name":"f"}}],"max_tokens":10,"max_completion_tokens":20}`,
			rules: []string{ruleMaxTokens, ruleReasoningEffort},
			check: func(t *testing.T, doc map[string]json.RawMessage) {
				if _, ok := doc["max_tokens"]; ok {
					t.Fatal("max_tokens must be dropped even when max_completion_tokens already exists")
				}
				wantRaw(t, doc, "max_completion_tokens", "20")
			},
		},
		{
			// The client's effort survives a tool turn: the passthrough route
			// shares the converter's capability table, which stopped forcing
			// "none" for luna (re-measured 2026-10-13).
			name:      "chat: tools no longer override the user effort",
			path:      chatPath,
			body:      `{"model":"gpt-6-luna","messages":[],"tools":[{"type":"function","function":{"name":"f"}}],"reasoning_effort":"high"}`,
			unchanged: true,
		},
		{
			name:  "chat: an effort outside the model's set falls back on a tool turn",
			path:  chatPath,
			body:  `{"model":"gpt-6-luna","messages":[],"tools":[{"type":"function","function":{"name":"f"}}],"reasoning_effort":"max"}`,
			rules: []string{ruleReasoningEffort},
			check: func(t *testing.T, doc map[string]json.RawMessage) {
				wantRaw(t, doc, "reasoning_effort", `"none"`)
			},
		},
		{
			name:      "chat: sampling params survive when tools force reasoning off",
			path:      chatPath,
			body:      `{"model":"gpt-6-luna","messages":[],"tools":[{"type":"function","function":{"name":"f"}}],"reasoning_effort":"none","temperature":0.7,"top_p":0.9}`,
			unchanged: true,
		},
		{
			name:      "chat: sampling params survive an explicit none without tools",
			path:      chatPath,
			body:      `{"model":"gpt-6-luna","messages":[],"reasoning_effort":"none","temperature":0.7}`,
			unchanged: true,
		},
		{
			name:  "chat: sampling params are dropped while reasoning is on",
			path:  chatPath,
			body:  `{"model":"gpt-6-luna","messages":[],"reasoning_effort":"high","temperature":0.7,"top_p":0.9}`,
			rules: []string{ruleSampling, ruleSampling},
			check: func(t *testing.T, doc map[string]json.RawMessage) { wantAbsent(t, doc, "temperature", "top_p") },
		},
		{
			name:  "chat: omitted effort counts as reasoning on upstream",
			path:  chatPath,
			body:  `{"model":"gpt-6-luna","messages":[],"temperature":0.7}`,
			rules: []string{ruleSampling},
			check: func(t *testing.T, doc map[string]json.RawMessage) { wantAbsent(t, doc, "temperature") },
		},
		{
			name:      "chat: other models keep their bytes untouched",
			path:      chatPath,
			body:      `{"model":"glm-5.3","messages":[],"max_tokens":32,"temperature":0.7}`,
			unchanged: true,
		},
		{
			name:      "chat: gpt-6 with nothing to fix keeps its bytes untouched",
			path:      chatPath,
			body:      `{"model":"gpt-6-luna","messages":[{"role":"user","content":"hi"}]}`,
			unchanged: true,
		},
		{
			name:      "chat: an empty tools array is not proof of tools",
			path:      chatPath,
			body:      `{"model":"gpt-6-luna","messages":[],"tools":[]}`,
			unchanged: true,
		},
		{
			name:  "chat: unknown fields survive the rewrite",
			path:  chatPath,
			body:  `{"model":"gpt-6-luna","messages":[],"tools":[{"type":"function","function":{"name":"f"}}],"max_tokens":5,"service_tier":"auto","vendor_ext":{"depth":3}}`,
			rules: []string{ruleMaxTokens, ruleReasoningEffort},
			check: func(t *testing.T, doc map[string]json.RawMessage) {
				wantRaw(t, doc, "service_tier", `"auto"`)
				wantRaw(t, doc, "vendor_ext", `{"depth":3}`)
			},
		},
		{
			name:  "anthropic: Claude Code shape (max_tokens + thinking + tools)",
			path:  anthropicPath,
			body:  `{"model":"gpt-6-luna","max_tokens":1024,"thinking":{"type":"disabled"},"temperature":1,"messages":[{"role":"user","content":"hi"}],"tools":[{"name":"read_file","input_schema":{"type":"object"}}]}`,
			rules: []string{ruleMaxTokens, ruleThinking, ruleSampling},
			check: func(t *testing.T, doc map[string]json.RawMessage) {
				wantRaw(t, doc, "max_completion_tokens", "1024")
				wantAbsent(t, doc, "max_tokens", "thinking", "temperature", "top_p")
			},
		},
		{
			name:  "anthropic: bare cap rename",
			path:  anthropicPath,
			body:  `{"model":"gpt-6-luna","max_tokens":64,"messages":[]}`,
			rules: []string{ruleMaxTokens},
			check: func(t *testing.T, doc map[string]json.RawMessage) {
				wantRaw(t, doc, "max_completion_tokens", "64")
			},
		},
		{
			name:      "anthropic: other endpoints on the mount are left alone",
			path:      "/api/anthropic/v1/models",
			body:      `{"model":"gpt-6-luna","max_tokens":64}`,
			unchanged: true,
		},
		{
			name:      "anthropic: other models keep their bytes untouched",
			path:      anthropicPath,
			body:      `{"model":"claude-sonnet-5","max_tokens":64,"thinking":{"type":"enabled"}}`,
			unchanged: true,
		},
		{
			name:      "not JSON is never touched",
			path:      chatPath,
			body:      `model=gpt-6-luna&max_tokens=5`,
			unchanged: true,
		},
		{
			name:      "JSON without a model field is never touched",
			path:      chatPath,
			body:      `{"messages":[],"max_tokens":5}`,
			unchanged: true,
		},
		{
			name:      "unrelated paths are never touched",
			path:      "/api/v1/models",
			body:      `{"model":"gpt-6-luna","max_tokens":5}`,
			unchanged: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, rules := normalizeModelQuirks(tc.path, []byte(tc.body))
			if tc.unchanged {
				if string(out) != tc.body {
					t.Fatalf("body must be returned unchanged, got %s", out)
				}
				if len(rules) != 0 {
					t.Fatalf("no rules may fire, got %v", rules)
				}
				return
			}
			gotRules := append([]string(nil), rules...)
			wantRules := append([]string(nil), tc.rules...)
			sort.Strings(gotRules)
			sort.Strings(wantRules)
			if strings.Join(gotRules, ",") != strings.Join(wantRules, ",") {
				t.Fatalf("rules = %v, want %v", rules, tc.rules)
			}
			var doc map[string]json.RawMessage
			if err := json.Unmarshal(out, &doc); err != nil {
				t.Fatalf("rewritten body is not JSON: %v\n%s", err, out)
			}
			if tc.check != nil {
				tc.check(t, doc)
			}
		})
	}
}

func wantRaw(t *testing.T, doc map[string]json.RawMessage, key, want string) {
	t.Helper()
	raw, ok := doc[key]
	if !ok {
		t.Fatalf("%s missing from rewritten body", key)
	}
	if compact := compactJSON(t, string(raw)); compact != want {
		t.Fatalf("%s = %s, want %s", key, compact, want)
	}
}

func wantAbsent(t *testing.T, doc map[string]json.RawMessage, keys ...string) {
	t.Helper()
	for _, key := range keys {
		if _, ok := doc[key]; ok {
			t.Fatalf("%s must be absent from the rewritten body", key)
		}
	}
}

// compactJSON normalizes whitespace so the want-strings above can stay short.
func compactJSON(t *testing.T, s string) string {
	t.Helper()
	var buf bytes.Buffer
	if err := json.Compact(&buf, []byte(s)); err != nil {
		t.Fatalf("compacting %s: %v", s, err)
	}
	return buf.String()
}
