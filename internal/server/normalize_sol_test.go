package server

import (
	"encoding/json"
	"testing"
)

// The Sol ids are the reason the tools rule had to stop being family-wide:
// the upstream rejects "none" for them ("must be low, medium, high, xhigh, or
// max") and 400s on any sampling parameter while tools are in the body, while
// an omitted effort is not accepted either (3/4 400s on gpt-6-sol, measured
// 2026-10-10). The shapes below are the ones the same upstream answered 200
// tool_calls to.
func TestNormalizeModelQuirksSol(t *testing.T) {
	const chatPath = "/api/v1/chat/completions"

	tests := []struct {
		name      string
		body      string
		unchanged bool
		rules     []string
		check     func(t *testing.T, doc map[string]json.RawMessage)
	}{
		{
			name:  "chat: sol tools with no effort get the floor effort",
			body:  `{"model":"gpt-6-sol","messages":[],"tools":[{"type":"function","function":{"name":"f"}}],"max_tokens":100}`,
			rules: []string{ruleMaxTokens, ruleReasoningEffort},
			check: func(t *testing.T, doc map[string]json.RawMessage) {
				wantRaw(t, doc, "reasoning_effort", `"low"`)
			},
		},
		{
			name:  "chat: sol tools with none are clamped, not rejected",
			body:  `{"model":"gpt-6.1-sol","messages":[],"tools":[{"type":"function","function":{"name":"f"}}],"reasoning_effort":"none"}`,
			rules: []string{ruleReasoningEffort},
			check: func(t *testing.T, doc map[string]json.RawMessage) {
				wantRaw(t, doc, "reasoning_effort", `"low"`)
			},
		},
		{
			name:      "chat: sol tools with an accepted effort are left byte-identical",
			body:      `{"model":"gpt-6-sol","messages":[],"tools":[{"type":"function","function":{"name":"f"}}],"reasoning_effort":"high"}`,
			unchanged: true,
		},
		{
			name: "chat: sol tools never carry sampling (upstream 400s on it)",
			body: `{"model":"gpt-6.1-sol","messages":[],"tools":[{"type":"function","function":{"name":"f"}}],"reasoning_effort":"low","temperature":0.3,"top_p":0.9}`,
			check: func(t *testing.T, doc map[string]json.RawMessage) {
				for _, field := range []string{"temperature", "top_p"} {
					if _, ok := doc[field]; ok {
						t.Fatalf("%s must be dropped on a Sol tool turn", field)
					}
				}
			},
		},
		{
			name:      "chat: sol without tools keeps sampling (accepted there)",
			body:      `{"model":"gpt-6-sol","messages":[],"reasoning_effort":"low","temperature":0.3}`,
			unchanged: true,
		},
		{
			name:      "chat: sol without tools drops an unaccepted effort instead of 400ing",
			body:      `{"model":"gpt-6-sol","messages":[],"reasoning_effort":"minimal"}`,
			unchanged: false,
			rules:     []string{ruleReasoningEffort},
			check: func(t *testing.T, doc map[string]json.RawMessage) {
				if _, ok := doc["reasoning_effort"]; ok {
					t.Fatal("minimal is outside the Sol set — it must be dropped, not forwarded")
				}
			},
		},
		{
			name:      "chat: unknown gpt-6 id takes the safe shape",
			body:      `{"model":"gpt-6-nebula","messages":[],"tools":[{"type":"function","function":{"name":"f"}}],"temperature":0.5}`,
			rules:     []string{ruleReasoningEffort, ruleSampling},
			unchanged: false,
			check: func(t *testing.T, doc map[string]json.RawMessage) {
				wantRaw(t, doc, "reasoning_effort", `"low"`)
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, rules := normalizeModelQuirks(chatPath, []byte(tc.body))
			if tc.unchanged {
				if string(out) != tc.body {
					t.Fatalf("body must be returned unchanged, got %s", out)
				}
				return
			}
			if string(out) == tc.body {
				t.Fatal("body must be rewritten, got the original back")
			}
			var doc map[string]json.RawMessage
			if err := json.Unmarshal(out, &doc); err != nil {
				t.Fatalf("rewritten body is not JSON: %v", err)
			}
			if tc.rules != nil {
				if len(rules) != len(tc.rules) {
					t.Fatalf("rules = %v, want %v", rules, tc.rules)
				}
				for i := range rules {
					if rules[i] != tc.rules[i] {
						t.Fatalf("rules = %v, want %v", rules, tc.rules)
					}
				}
			}
			if tc.check != nil {
				tc.check(t, doc)
			}
		})
	}
}
