package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dncore/synapse-protocol-adapter/internal/config"
	"github.com/dncore/synapse-protocol-adapter/internal/metrics"
	"github.com/dncore/synapse-protocol-adapter/internal/responses"
)

func testConfig(upstreamURL string) config.Config {
	cfg := config.Defaults()
	cfg.Upstream.BaseURL = upstreamURL
	cfg.Limits.MaxConcurrency = 200
	return cfg
}

func newTestServer(t *testing.T, upstreamURL string) *Server {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(testConfig(upstreamURL), logger, metrics.NewRegistry())
}

// fakeUpstream is a chat completions provider recording the last request
// and able to serve non-streamed, streamed, and error responses.
type fakeUpstream struct {
	mu          sync.Mutex
	lastAuth    string
	lastHeaders http.Header
	lastBody    []byte

	stream  bool
	handler func(w http.ResponseWriter, r *http.Request) // optional override
}

func (f *fakeUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.lastAuth = r.Header.Get("Authorization")
	f.lastHeaders = r.Header.Clone()
	body, _ := io.ReadAll(r.Body)
	f.lastBody = body
	override := f.handler
	stream := f.stream
	f.mu.Unlock()

	if override != nil {
		override(w, r)
		return
	}

	var req struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &req)

	if !stream && !req.Stream {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		fmt.Fprint(w, `{"id":"chatcmpl-1","object":"chat.completion","created":1700000000,"model":"test",`+
			`"choices":[{"index":0,"message":{"role":"assistant","content":"hello world"},"finish_reason":"stop"}],`+
			`"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(200)
	fl, _ := w.(http.Flusher)
	flush := func() {
		if fl != nil {
			fl.Flush()
		}
	}
	fmt.Fprint(w, "data: {\"id\":\"chatcmpl-2\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"he\"}}]}\n\n")
	flush()
	fmt.Fprint(w, "data: {\"id\":\"chatcmpl-2\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"llo\"}}]}\n\n")
	flush()
	fmt.Fprint(w, "data: {\"id\":\"chatcmpl-2\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
	flush()
	fmt.Fprint(w, "data: {\"id\":\"chatcmpl-2\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":\"test\",\"choices\":[],\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":3,\"total_tokens\":12}}\n\n")
	flush()
	fmt.Fprint(w, "data: [DONE]\n\n")
	flush()
}

func responsesBody(stream bool) string {
	s := "false"
	if stream {
		s = "true"
	}
	return `{"model":"test","input":"hi","stream":` + s + `}`
}

func TestProxy_NonStreaming(t *testing.T) {
	up := &fakeUpstream{}
	upSrv := httptest.NewServer(up)
	defer upSrv.Close()

	s := newTestServer(t, upSrv.URL)
	proxySrv := httptest.NewServer(s.httpSrv.Handler)
	defer proxySrv.Close()

	req, _ := http.NewRequest("POST", proxySrv.URL+"/v1/responses", strings.NewReader(responsesBody(false)))
	req.Header.Set("Authorization", "Bearer client-secret-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var out responses.Response
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Object != "response" || out.Status != "completed" {
		t.Fatalf("bad response envelope: %+v", out)
	}
	if len(out.Output) != 1 || out.Output[0].Content.Parts[0].Text != "hello world" {
		t.Fatalf("bad output: %+v", out.Output)
	}
	if out.Usage == nil || out.Usage.InputTokens != 10 || out.Usage.OutputTokens != 5 {
		t.Fatalf("bad usage: %+v", out.Usage)
	}

	// Authorization must pass through verbatim.
	up.mu.Lock()
	auth := up.lastAuth
	up.mu.Unlock()
	if auth != "Bearer client-secret-token" {
		t.Fatalf("Authorization not forwarded verbatim: %q", auth)
	}
}

func TestProxy_Streaming(t *testing.T) {
	up := &fakeUpstream{stream: true}
	upSrv := httptest.NewServer(up)
	defer upSrv.Close()

	s := newTestServer(t, upSrv.URL)
	proxySrv := httptest.NewServer(s.httpSrv.Handler)
	defer proxySrv.Close()

	req, _ := http.NewRequest("POST", proxySrv.URL+"/v1/responses", strings.NewReader(responsesBody(true)))
	req.Header.Set("Authorization", "Bearer xyz")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content type: %s", ct)
	}

	var events []responses.Event
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 16<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			continue
		}
		var ev responses.Event
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			t.Fatalf("bad event JSON %q: %v", payload, err)
		}
		events = append(events, ev)
	}
	if len(events) == 0 {
		t.Fatal("no events")
	}
	if events[0].Type != "response.created" {
		t.Fatalf("first event: %s", events[0].Type)
	}
	var text strings.Builder
	completed := false
	for _, ev := range events {
		switch ev.Type {
		case "response.output_text.delta":
			text.WriteString(ev.Delta)
		case "response.completed":
			completed = true
			if ev.Response == nil || ev.Response.Usage == nil || ev.Response.Usage.InputTokens != 9 {
				t.Fatalf("completed event usage wrong: %+v", ev.Response.Usage)
			}
		}
	}
	if text.String() != "hello" {
		t.Fatalf("streamed text wrong: %q", text.String())
	}
	if !completed {
		t.Fatal("missing response.completed")
	}
}

func TestProxy_ToolCallsRoundTrip(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"c","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,`+
			`"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_9","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"SF\"}"}}]},`+
			`"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`)
	}))
	defer up.Close()

	s := newTestServer(t, up.URL)
	proxySrv := httptest.NewServer(s.httpSrv.Handler)
	defer proxySrv.Close()

	body := `{"model":"m","input":"weather?","tools":[{"type":"function","name":"get_weather","parameters":{"type":"object"}}]}`
	resp, err := http.Post(proxySrv.URL+"/v1/responses", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out responses.Response
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Output) != 1 || out.Output[0].Type != "function_call" || out.Output[0].CallID != "call_9" ||
		out.Output[0].Name != "get_weather" || out.Output[0].Arguments != `{"city":"SF"}` {
		t.Fatalf("tool call round trip broken: %+v", out.Output)
	}
}

func TestProxy_HeaderPassthroughMatrix(t *testing.T) {
	var got http.Header
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		fmt.Fprint(w, `{"id":"c","model":"m","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer up.Close()

	s := newTestServer(t, up.URL)
	proxySrv := httptest.NewServer(s.httpSrv.Handler)
	defer proxySrv.Close()

	req, _ := http.NewRequest("POST", proxySrv.URL+"/v1/responses", strings.NewReader(responsesBody(false)))
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("OpenAI-Beta", "responses=v1")
	req.Header.Set("X-Session-Id", "sess-42")
	req.Header.Set("X-Custom-Thing", "custom-value")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Accept-Encoding", "gzip, br")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if got.Get("Authorization") != "Bearer tok" {
		t.Fatalf("Authorization lost")
	}
	if got.Get("OpenAI-Beta") != "responses=v1" {
		t.Fatalf("OpenAI-* lost")
	}
	if got.Get("X-Session-Id") != "sess-42" || got.Get("X-Custom-Thing") != "custom-value" {
		t.Fatalf("X-* lost: %v", got)
	}
	// The client's own Accept-Encoding must not be forwarded (we must parse
	// the body); Go's transport may negotiate plain gzip itself, which it
	// transparently decompresses. "gzip, br" verbatim would break that.
	if ae := got.Get("Accept-Encoding"); strings.Contains(ae, "br") || ae == "gzip, br" {
		t.Fatalf("client Accept-Encoding leaked through: %q", ae)
	}
}

func TestProxy_ClientDisconnectCancelsUpstream(t *testing.T) {
	var upstreamCanceled atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		// Hold the stream open until the upstream request is canceled.
		<-r.Context().Done()
		upstreamCanceled.Add(1)
	}))
	defer up.Close()

	s := newTestServer(t, up.URL)
	proxySrv := httptest.NewServer(s.httpSrv.Handler)
	defer proxySrv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "POST", proxySrv.URL+"/v1/responses", strings.NewReader(responsesBody(true)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	// Headers received; now the client vanishes.
	resp.Body.Close()
	cancel()

	deadline := time.Now().Add(5 * time.Second)
	for upstreamCanceled.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if upstreamCanceled.Load() == 0 {
		t.Fatal("upstream request was not canceled after client disconnect")
	}
}

func TestProxy_UpstreamErrorForwarded(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		fmt.Fprint(w, `{"error":{"message":"rate limited","type":"rate_limit_error","code":"429"}}`)
	}))
	defer up.Close()

	s := newTestServer(t, up.URL)
	proxySrv := httptest.NewServer(s.httpSrv.Handler)
	defer proxySrv.Close()

	resp, err := http.Post(proxySrv.URL+"/v1/responses", "application/json", strings.NewReader(responsesBody(false)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 429 {
		t.Fatalf("status must be preserved, got %d", resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	emsg := out["error"].(map[string]any)["message"]
	if emsg != "rate limited" {
		t.Fatalf("error message not relayed: %v", out)
	}
}

func TestProxy_BadRequests(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream must not be called")
	}))
	defer up.Close()

	s := newTestServer(t, up.URL)
	proxySrv := httptest.NewServer(s.httpSrv.Handler)
	defer proxySrv.Close()

	cases := []struct {
		name   string
		body   string
		status int
	}{
		{"malformed json", `{not json`, 400},
		{"previous_response_id", `{"model":"m","input":"x","previous_response_id":"resp_1"}`, 400},
		{"unsupported item", `{"model":"m","input":[{"type":"computer_call"}]}`, 400},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := http.Post(proxySrv.URL+"/v1/responses", "application/json", strings.NewReader(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.status {
				t.Fatalf("status %d want %d", resp.StatusCode, tc.status)
			}
		})
	}
}

func TestProxy_Concurrency(t *testing.T) {
	up := &fakeUpstream{}
	upSrv := httptest.NewServer(up)
	defer upSrv.Close()

	s := newTestServer(t, upSrv.URL)
	proxySrv := httptest.NewServer(s.httpSrv.Handler)
	defer proxySrv.Close()

	for _, n := range []int{10, 50, 100, 200} {
		t.Run(fmt.Sprintf("%d-concurrent", n), func(t *testing.T) {
			var wg sync.WaitGroup
			errs := make(chan error, n)
			for i := 0; i < n; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					resp, err := http.Post(proxySrv.URL+"/v1/responses", "application/json", strings.NewReader(responsesBody(false)))
					if err != nil {
						errs <- err
						return
					}
					defer resp.Body.Close()
					if resp.StatusCode != 200 {
						errs <- fmt.Errorf("status %d", resp.StatusCode)
						return
					}
					var out responses.Response
					if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
						errs <- err
						return
					}
					if out.Output[0].Content.Parts[0].Text != "hello world" {
						errs <- fmt.Errorf("wrong text")
					}
				}()
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				t.Error(err)
			}
		})
	}
}

// Unlimited mode (max_concurrency: 0): no semaphore, no queueing — all
// requests go straight through regardless of count.
func TestProxy_UnlimitedConcurrency(t *testing.T) {
	up := &fakeUpstream{}
	upSrv := httptest.NewServer(up)
	defer upSrv.Close()

	cfg := testConfig(upSrv.URL)
	cfg.Limits.MaxConcurrency = 0
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := New(cfg, logger, metrics.NewRegistry())
	proxySrv := httptest.NewServer(s.httpSrv.Handler)
	defer proxySrv.Close()

	const n = 64
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.Post(proxySrv.URL+"/v1/responses", "application/json", strings.NewReader(responsesBody(false)))
			if err != nil {
				errs <- err
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode != 200 {
				errs <- fmt.Errorf("status %d", resp.StatusCode)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// In-band rejection: gateway answers HTTP 200 but streams {"error":{...}}
// as the (only) chunk. The client must see a clear error event, never a
// silently empty response.completed.
func TestProxy_StreamInBandError(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, "data: {\"error\":{\"message\":\"Model access denied\",\"type\":\"forbidden\",\"model\":\"x\"}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer up.Close()

	s := newTestServer(t, up.URL)
	proxySrv := httptest.NewServer(s.httpSrv.Handler)
	defer proxySrv.Close()

	resp, err := http.Post(proxySrv.URL+"/v1/responses", "application/json", strings.NewReader(responsesBody(true)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	text := string(body)
	if !strings.Contains(text, `"type":"error"`) || !strings.Contains(text, "Model access denied") {
		t.Fatalf("in-band error not surfaced:\n%s", text)
	}
	if strings.Contains(text, "response.completed") {
		t.Fatalf("must not complete after in-band rejection:\n%s", text)
	}
}

func TestOperationalEndpoints(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer up.Close()

	s := newTestServer(t, up.URL)
	proxySrv := httptest.NewServer(s.httpSrv.Handler)
	defer proxySrv.Close()

	// /health
	resp, _ := http.Get(proxySrv.URL + "/health")
	if resp.StatusCode != 200 {
		t.Fatalf("/health = %d", resp.StatusCode)
	}
	resp.Body.Close()

	// /ready
	resp, _ = http.Get(proxySrv.URL + "/ready")
	if resp.StatusCode != 200 {
		t.Fatalf("/ready = %d", resp.StatusCode)
	}
	resp.Body.Close()

	// /metrics
	resp, _ = http.Get(proxySrv.URL + "/metrics")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	text := string(body)
	for _, want := range []string{
		"protocol_proxy_requests_total",
		"protocol_proxy_request_duration_seconds",
		"protocol_proxy_first_byte_latency_seconds",
		"protocol_proxy_active_requests",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("/metrics missing %s", want)
		}
	}
}

func TestShutdown_ReadyFlipsTo503(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer up.Close()

	s := newTestServer(t, up.URL)
	proxySrv := httptest.NewServer(s.httpSrv.Handler)
	defer proxySrv.Close()

	resp, _ := http.Get(proxySrv.URL + "/ready")
	if resp.StatusCode != 200 {
		t.Fatalf("ready before shutdown = %d", resp.StatusCode)
	}
	resp.Body.Close()

	go func() { _ = s.Shutdown() }()

	deadline := time.Now().Add(2 * time.Second)
	for {
		resp, err := http.Get(proxySrv.URL + "/ready")
		if err == nil {
			code := resp.StatusCode
			resp.Body.Close()
			if code == 503 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("/ready never flipped to 503")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// --- per-user concurrency queueing ---

// gatingUpstream holds every request until the test releases it, so
// concurrency limits are observable.
type gatingUpstream struct {
	mu       sync.Mutex
	inFlight int
	peak     int
	release  chan struct{}
}

func newGatingUpstream() *gatingUpstream {
	return &gatingUpstream{release: make(chan struct{})}
}

func (g *gatingUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	g.inFlight++
	if g.inFlight > g.peak {
		g.peak = g.inFlight
	}
	g.mu.Unlock()

	<-g.release

	g.mu.Lock()
	g.inFlight--
	g.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprint(w, `{"id":"chatcmpl-1","object":"chat.completion","created":1700000000,"model":"test",`+
		`"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],`+
		`"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
}

func (g *gatingUpstream) snapshot() (inFlight, peak int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.inFlight, g.peak
}

// pulse lets one held upstream request complete.
func (g *gatingUpstream) pulse() { g.release <- struct{}{} }

func (g *gatingUpstream) waitForInFlight(t *testing.T, n int) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		if in, _ := g.snapshot(); in == n {
			return
		}
		select {
		case <-deadline:
			in, peak := g.snapshot()
			t.Fatalf("upstream in-flight never reached %d (now %d, peak %d)", n, in, peak)
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
}

func usersTestConfig(upstreamURL string, ucfg config.Users) config.Config {
	cfg := testConfig(upstreamURL)
	cfg.Users = ucfg
	return cfg
}

func postResponses(t *testing.T, url, auth string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", url+"/v1/responses", strings.NewReader(responsesBody(false)))
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func usersCfg(defaultConcurrency, maxQueue int) config.Users {
	return config.Users{
		Enabled:            true,
		DefaultConcurrency: defaultConcurrency,
		MaxQueue:           maxQueue,
		QueueTimeout:       30 * time.Second,
		Keys: []config.UserKey{{
			Name:    "dean",
			KeyHash: config.HashCredential("Bearer dean-key"),
		}},
	}
}

// The third request with the same key queues behind the limit; releasing
// one upstream slot admits it, and the upstream never sees more than the
// cap concurrently.
func TestUsersQueue_ThirdRequestWaitsForSlot(t *testing.T) {
	up := newGatingUpstream()
	upSrv := httptest.NewServer(up)
	defer upSrv.Close()

	cfg := usersTestConfig(upSrv.URL, usersCfg(2, 8))
	s := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), metrics.NewRegistry())
	proxySrv := httptest.NewServer(s.httpSrv.Handler)
	defer proxySrv.Close()

	results := make(chan int, 3)
	for i := 0; i < 3; i++ {
		go func() {
			resp := postResponses(t, proxySrv.URL, "Bearer dean-key")
			results <- resp.StatusCode
			resp.Body.Close()
		}()
	}

	up.waitForInFlight(t, 2) // two at the upstream, third queued
	time.Sleep(50 * time.Millisecond)
	if in, _ := up.snapshot(); in != 2 {
		t.Fatalf("limit violated: %d in flight", in)
	}

	// One finishes; the queued request takes its slot: the first result
	// arrives and the upstream is back at the cap of 2 (never above it).
	up.pulse()
	if code := <-results; code != 200 {
		t.Fatalf("first request failed: %d", code)
	}
	up.waitForInFlight(t, 2)
	up.pulse()
	up.pulse()
	for i := 0; i < 2; i++ {
		if code := <-results; code != 200 {
			t.Fatalf("queued request failed: %d", code)
		}
	}
	if _, peak := up.snapshot(); peak != 2 {
		t.Fatalf("upstream peak %d — the cap was exceeded or never reached", peak)
	}

	// Slots fully released: a fresh request goes straight through.
	done := make(chan int, 1)
	go func() {
		resp := postResponses(t, proxySrv.URL, "Bearer dean-key")
		done <- resp.StatusCode
		resp.Body.Close()
	}()
	up.waitForInFlight(t, 1)
	up.pulse()
	if code := <-done; code != 200 {
		t.Fatalf("post-drain request: %d", code)
	}
}

// Beyond max_queue the proxy itself answers 429 (+Retry-After) instead
// of queueing forever.
func TestUsersQueue_RejectsWith429BeyondMaxQueue(t *testing.T) {
	up := newGatingUpstream()
	upSrv := httptest.NewServer(up)
	defer upSrv.Close()

	cfg := usersTestConfig(upSrv.URL, usersCfg(1, 1))
	s := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), metrics.NewRegistry())
	proxySrv := httptest.NewServer(s.httpSrv.Handler)
	defer proxySrv.Close()

	results := make(chan *http.Response, 3)
	for i := 0; i < 3; i++ {
		go func() { results <- postResponses(t, proxySrv.URL, "Bearer dean-key") }()
	}
	up.waitForInFlight(t, 1)

	// The only response available now is the queue-overflow 429 (the
	// in-flight and queued requests are still parked).
	rejected := <-results
	if rejected.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("want 429 beyond max_queue, got %d", rejected.StatusCode)
	}
	if ra := rejected.Header.Get("Retry-After"); ra == "" {
		t.Fatal("429 without Retry-After")
	}
	var body map[string]any
	if err := json.NewDecoder(rejected.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	rejected.Body.Close()
	if et := body["error"].(map[string]any)["type"]; et != "rate_limit_error" {
		t.Fatalf("error type %v, want rate_limit_error", et)
	}

	// Drain: first pulse frees the in-flight request, admitting the
	// queued one; second pulse frees that.
	up.pulse()
	resp := <-results
	resp.Body.Close()
	up.pulse()
	resp = <-results
	resp.Body.Close()
}

// Unregistered keys get exactly the same treatment as configured ones:
// they queue at default_concurrency (no bypass, no special casing).
func TestUsersQueue_UnknownKeyQueuesAtDefault(t *testing.T) {
	up := newGatingUpstream()
	upSrv := httptest.NewServer(up)
	defer upSrv.Close()

	cfg := usersTestConfig(upSrv.URL, usersCfg(2, 8))
	s := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), metrics.NewRegistry())
	proxySrv := httptest.NewServer(s.httpSrv.Handler)
	defer proxySrv.Close()

	for i := 0; i < 4; i++ {
		go func() {
			resp := postResponses(t, proxySrv.URL, "Bearer stranger")
			resp.Body.Close()
		}()
	}
	up.waitForInFlight(t, 2) // capped at the default, not all 4 through
	time.Sleep(50 * time.Millisecond)
	if in, _ := up.snapshot(); in != 2 {
		t.Fatalf("unknown key bypassed the cap: %d in flight", in)
	}
	for i := 0; i < 4; i++ {
		up.pulse()
	}
	if _, peak := up.snapshot(); peak > 2 {
		t.Fatalf("cap exceeded for unknown key: peak %d", peak)
	}
}

// GET /users reports live state; PUT /users/{id} raises the limit live
// and the queued request is admitted without any restart.
func TestUsersQueue_ListAndLiveUpdate(t *testing.T) {
	up := newGatingUpstream()
	upSrv := httptest.NewServer(up)
	defer upSrv.Close()

	cfg := usersTestConfig(upSrv.URL, usersCfg(1, 8))
	s := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), metrics.NewRegistry())
	proxySrv := httptest.NewServer(s.httpSrv.Handler)
	defer proxySrv.Close()

	go func() {
		resp := postResponses(t, proxySrv.URL, "Bearer dean-key")
		resp.Body.Close()
	}()
	up.waitForInFlight(t, 1)

	queued := make(chan int, 1)
	go func() {
		resp := postResponses(t, proxySrv.URL, "Bearer dean-key")
		queued <- resp.StatusCode
		resp.Body.Close()
	}()
	deadline := time.After(3 * time.Second)
	for {
		list := s.userReg.All()
		if len(list) == 1 && list[0].Queued == 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("request never queued")
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}

	// GET /users (via the HTTP surface, as operators see it).
	resp, err := http.Get(proxySrv.URL + "/users")
	if err != nil {
		t.Fatal(err)
	}
	var listing struct {
		Enabled            bool  `json:"enabled"`
		DefaultConcurrency int   `json:"default_concurrency"`
		QueueDepth         int64 `json:"queue_depth"`
		Users              []struct {
			ID          string `json:"id"`
			Name        string `json:"name"`
			Concurrency int    `json:"concurrency"`
			Active      int    `json:"active"`
			Queued      int    `json:"queued"`
			Requests    int64  `json:"requests_total"`
		} `json:"users"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&listing); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !listing.Enabled || listing.QueueDepth != 1 || len(listing.Users) != 1 {
		t.Fatalf("listing wrong: %+v", listing)
	}
	u := listing.Users[0]
	if u.Name != "dean" || u.Concurrency != 1 || u.Active != 1 || u.Queued != 1 || u.Requests < 2 {
		t.Fatalf("user stats wrong: %+v", u)
	}

	// PUT /users/dean: raise to 2 — the queued request must be admitted.
	put, _ := http.NewRequest(http.MethodPut, proxySrv.URL+"/users/dean", strings.NewReader(`{"concurrency":2}`))
	resp, err = http.DefaultClient.Do(put)
	if err != nil {
		t.Fatal(err)
	}
	var updated struct {
		Concurrency int `json:"concurrency"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&updated); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if updated.Concurrency != 2 {
		t.Fatalf("update not applied: %+v", updated)
	}
	up.waitForInFlight(t, 2)
	up.pulse()
	up.pulse()
	if code := <-queued; code != 200 {
		t.Fatalf("queued request after raise: %d", code)
	}
}

// Feature off (the default): /users reports disabled and no limiting
// happens — exactly today's behavior.
func TestUsersQueue_DisabledByDefault(t *testing.T) {
	up := newGatingUpstream()
	upSrv := httptest.NewServer(up)
	defer upSrv.Close()

	s := newTestServer(t, upSrv.URL)
	proxySrv := httptest.NewServer(s.httpSrv.Handler)
	defer proxySrv.Close()

	for i := 0; i < 3; i++ {
		go func() {
			resp := postResponses(t, proxySrv.URL, "Bearer dean-key")
			resp.Body.Close()
		}()
	}
	up.waitForInFlight(t, 3)
	for i := 0; i < 3; i++ {
		up.pulse()
	}

	resp, err := http.Get(proxySrv.URL + "/users")
	if err != nil {
		t.Fatal(err)
	}
	var listing map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&listing); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if v, _ := listing["enabled"].(bool); v {
		t.Fatalf("users must report disabled by default: %v", listing)
	}
}
