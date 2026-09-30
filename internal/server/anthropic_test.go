package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dncore/synapse-protocol-adapter/internal/config"
)

// newTakeoverTestServer wires a proxy in gpt6-tools mode against a fake
// upstream, returning both so tests can inspect what the upstream received.
func newTakeoverTestServer(t *testing.T, upstreamHandler http.Handler) (*httptest.Server, *httptest.Server) {
	t.Helper()
	up := httptest.NewServer(upstreamHandler)
	t.Cleanup(up.Close)

	cfg := testConfig(up.URL)
	cfg.Upstream.AnthropicMode = config.AnthropicModeGPT6Tools
	s := New(cfg, newTestLogger(), newTestRegistry())
	proxy := httptest.NewServer(s.httpSrv.Handler)
	t.Cleanup(proxy.Close)
	return proxy, up
}

const claudeCodeBody = `{"model":"gpt-6-luna","max_tokens":32000,"stream":false,
	"thinking":{"type":"adaptive","display":"omitted"},
	"output_config":{"effort":"xhigh"},
	"system":[{"type":"text","text":"You are a coding agent."}],
	"tools":[{"name":"Read","description":"read a file","input_schema":{"type":"object","properties":{"file_path":{"type":"string"}},"required":["file_path"]}}],
	"messages":[{"role":"user","content":[{"type":"text","text":"read /etc/hostname"}]}]}`

func postAnthropic(t *testing.T, base, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest("POST", base+"/api/anthropic/v1/messages", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "sk-test")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// The failing shape is taken over: the upstream must receive a chat request on
// the chat route, with the family dialect applied (max_completion_tokens,
// reasoning_effort=none, no thinking) and the Anthropic body's content mapped.
func TestAnthropicTakeover_ConvertsToolTurn(t *testing.T) {
	var gotPath, gotBody, gotAuth string
	proxy, _ := newTakeoverTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","model":"gpt-6-luna","choices":[{"index":0,"message":{"role":"assistant","content":"reading","tool_calls":[{"id":"call_1","type":"function","function":{"name":"Read","arguments":"{\"file_path\":\"/etc/hostname\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":4}}`))
	}))

	resp := postAnthropic(t, proxy.URL, claudeCodeBody)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("takeover must answer 200, got %d: %s", resp.StatusCode, body)
	}
	if gotPath != "/chat/completions" {
		t.Fatalf("takeover must call the chat route, upstream saw %s", gotPath)
	}

	// The client authenticated with x-api-key (ANTHROPIC_API_KEY); the chat
	// route wants Bearer, so the takeover must synthesize it.
	if gotAuth != "Bearer sk-test" {
		t.Fatalf("x-api-key must be translated to a Bearer header, upstream saw %q", gotAuth)
	}

	var chat map[string]json.RawMessage
	if err := json.Unmarshal([]byte(gotBody), &chat); err != nil {
		t.Fatalf("upstream body is not JSON: %v", err)
	}
	if string(chat["max_completion_tokens"]) != "32000" {
		t.Fatalf("must send max_completion_tokens, got %s", gotBody)
	}
	if _, ok := chat["max_tokens"]; ok {
		t.Fatalf("max_tokens must not be forwarded, got %s", gotBody)
	}
	if string(chat["reasoning_effort"]) != `"none"` {
		t.Fatalf("tools must force reasoning_effort=none, got %s", gotBody)
	}
	if _, ok := chat["thinking"]; ok {
		t.Fatalf("thinking must not reach the upstream, got %s", gotBody)
	}
	if !strings.Contains(gotBody, `"tools"`) || !strings.Contains(gotBody, `"function"`) {
		t.Fatalf("tools must be mapped to nested functions, got %s", gotBody)
	}

	// The reply must be Anthropic-shaped.
	var out struct {
		Type    string `json:"type"`
		Role    string `json:"role"`
		Content []struct {
			Type  string         `json:"type"`
			Text  string         `json:"text"`
			ID    string         `json:"id"`
			Name  string         `json:"name"`
			Input map[string]any `json:"input"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	body, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("reply is not JSON: %v", err)
	}
	if out.Type != "message" || out.Role != "assistant" || out.StopReason != "tool_use" {
		t.Fatalf("reply envelope wrong: %s", body)
	}
	if len(out.Content) != 2 || out.Content[0].Type != "text" || out.Content[1].Type != "tool_use" {
		t.Fatalf("reply blocks wrong: %s", body)
	}
	if out.Content[1].Name != "Read" || out.Content[1].Input["file_path"] != "/etc/hostname" {
		t.Fatalf("tool_use block wrong: %s", body)
	}
	if out.Usage.InputTokens != 10 || out.Usage.OutputTokens != 4 {
		t.Fatalf("usage wrong: %s", body)
	}
}

// Streaming: the client must receive the Anthropic event sequence, including
// tool_use deltas as input_json_delta.
func TestAnthropicTakeover_StreamsAnthropicEvents(t *testing.T) {
	proxy, _ := newTakeoverTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		chunks := []string{
			`{"id":"chatcmpl-1","model":"gpt-6-luna","choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
			`{"id":"chatcmpl-1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"Read","arguments":"{\"file_path\":"}}]}}]}`,
			`{"id":"chatcmpl-1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"/etc/hostname\"}"}}]}}]}`,
			`{"id":"chatcmpl-1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":4}}`,
			`[DONE]`,
		}
		for _, c := range chunks {
			_, _ = w.Write([]byte("data: " + c + "\n\n"))
			w.(http.Flusher).Flush()
		}
	}))

	body := strings.Replace(claudeCodeBody, `"stream":false`, `"stream":true`, 1)
	resp := postAnthropic(t, proxy.URL, body)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("streaming reply must be SSE, got %q", ct)
	}
	out, _ := io.ReadAll(resp.Body)
	text := string(out)
	for _, want := range []string{
		"event: message_start",
		"event: content_block_start",
		"event: content_block_delta",
		"event: content_block_stop",
		"event: message_delta",
		"event: message_stop",
		`"type":"tool_use"`,
		`"type":"input_json_delta"`,
		`"stop_reason":"tool_use"`,
		`"output_tokens":4`,
	} {
		if !strings.Contains(strings.ReplaceAll(text, " ", ""), strings.ReplaceAll(want, " ", "")) {
			t.Fatalf("stream missing %s\n---\n%s", want, text)
		}
	}
}

// Requests the upstream translator handles stay untouched: other models and
// tool-free turns never reach the conversion.
func TestAnthropicTakeover_LeavesOtherRequestsAlone(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"other model", strings.Replace(claudeCodeBody, "gpt-6-luna", "deepseek-v4-flash", 1)},
		{"no tools", `{"model":"gpt-6-luna","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotPath, gotBody string
			proxy, _ := newTakeoverTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				b, _ := io.ReadAll(r.Body)
				gotBody = string(b)
				_, _ = w.Write([]byte(`{"ok":true}`))
			}))
			resp := postAnthropic(t, proxy.URL, tc.body)
			resp.Body.Close()
			if gotPath != "/api/anthropic/v1/messages" {
				t.Fatalf("must stay on the anthropic mount, upstream saw %s", gotPath)
			}
			if gotBody != tc.body {
				t.Fatalf("body must be forwarded byte-for-byte:\n got %s\nwant %s", gotBody, tc.body)
			}
		})
	}
}

// A shape the mapper refuses falls back to passthrough: the upstream sees the
// original bytes on the anthropic mount.
func TestAnthropicTakeover_FallsBackOnUnsupportedBody(t *testing.T) {
	var gotPath, gotBody string
	proxy, _ := newTakeoverTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"detail":{"message":"upstream said no"}}`))
	}))

	body := strings.Replace(claudeCodeBody,
		`[{"type":"text","text":"read /etc/hostname"}]`,
		`[{"type":"document","source":{"type":"base64","data":"AAA"}}]`, 1)
	resp := postAnthropic(t, proxy.URL, body)
	defer resp.Body.Close()

	if gotPath != "/api/anthropic/v1/messages" {
		t.Fatalf("fallback must use the anthropic mount, upstream saw %s", gotPath)
	}
	if gotBody != body {
		t.Fatalf("fallback must forward the original bytes")
	}
	if resp.StatusCode != 400 {
		t.Fatalf("fallback must relay the upstream status, got %d", resp.StatusCode)
	}
}

// With the mode off (the default) nothing changes: no conversion, no buffering
// requirement, byte-for-byte passthrough.
func TestAnthropicTakeover_DisabledByDefault(t *testing.T) {
	var gotPath, gotBody string
	proxy, _ := newPassthroughTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	resp := postAnthropic(t, proxy.URL, claudeCodeBody)
	resp.Body.Close()
	if gotPath != "/api/anthropic/v1/messages" || gotBody != claudeCodeBody {
		t.Fatalf("default mode must not touch the request (path=%s)", gotPath)
	}
}

// Upstream errors on the takeover path are relayed in the Anthropic error
// envelope, keeping the upstream's message.
func TestAnthropicTakeover_RelaysErrorsAsAnthropicEnvelope(t *testing.T) {
	proxy, _ := newTakeoverTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"error":{"message":"quota exhausted","type":"quota"}}`))
	}))
	resp := postAnthropic(t, proxy.URL, claudeCodeBody)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 429 {
		t.Fatalf("status must be relayed, got %d", resp.StatusCode)
	}
	var out struct {
		Type  string `json:"type"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("error is not JSON: %v (%s)", err, body)
	}
	if out.Type != "error" || !strings.Contains(out.Error.Message, "quota exhausted") {
		t.Fatalf("error envelope wrong: %s", body)
	}
}
