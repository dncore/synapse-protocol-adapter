package server

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newPassthroughTestServer(t *testing.T, upstreamHandler http.Handler) (*httptest.Server, *httptest.Server) {
	t.Helper()
	up := httptest.NewServer(upstreamHandler)
	t.Cleanup(up.Close)

	s := newTestServer(t, up.URL)
	proxy := httptest.NewServer(s.httpSrv.Handler)
	t.Cleanup(proxy.Close)
	return proxy, up
}

func TestPassthrough_PostBodyByteExact(t *testing.T) {
	var gotBody, gotMethod, gotAuth, gotPath string
	proxy, _ := newPassthroughTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotAuth = r.Method, r.URL.Path, r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"id":"c1","choices":[{"message":{"content":"ok"}}]}`))
	}))

	body := `{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":false}`
	req, _ := http.NewRequest("POST", proxy.URL+"/v1/chat/completions?api-version=2", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer pt-key")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if gotMethod != "POST" || gotPath != "/chat/completions" {
		t.Fatalf("upstream saw %s %s", gotMethod, gotPath)
	}
	if gotAuth != "Bearer pt-key" {
		t.Fatalf("Authorization not forwarded: %q", gotAuth)
	}
	if gotBody != body {
		t.Fatalf("request body modified:\n got %q\nwant %q", gotBody, body)
	}
	got, _ := io.ReadAll(resp.Body)
	if string(got) != `{"id":"c1","choices":[{"message":{"content":"ok"}}]}` {
		t.Fatalf("response body modified: %s", got)
	}
}

func TestPassthrough_QueryForwarded(t *testing.T) {
	var gotQuery string
	proxy, _ := newPassthroughTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_, _ = w.Write([]byte("ok"))
	}))

	resp, err := http.Get(proxy.URL + "/v1/models?limit=10&foo=a%20b")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if gotQuery != "limit=10&foo=a%20b" {
		t.Fatalf("query lost or re-encoded: %q", gotQuery)
	}
}

func TestPassthrough_GetModels(t *testing.T) {
	var gotMethod, gotPath string
	proxy, _ := newPassthroughTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"m1"}]}`))
	}))

	resp, err := http.Get(proxy.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if gotMethod != "GET" || gotPath != "/models" {
		t.Fatalf("upstream saw %s %s", gotMethod, gotPath)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestPassthrough_ErrorVerbatim(t *testing.T) {
	proxy, _ := newPassthroughTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Upstream-Trace", "t-42")
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limited","code":"429"}}`))
	}))

	resp, err := http.Post(proxy.URL+"/v1/chat/completions", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 429 {
		t.Fatalf("status rewritten: %d", resp.StatusCode)
	}
	if resp.Header.Get("X-Upstream-Trace") != "t-42" {
		t.Fatalf("upstream response headers lost")
	}
	got, _ := io.ReadAll(resp.Body)
	if string(got) != `{"error":{"message":"rate limited","code":"429"}}` {
		t.Fatalf("error body must pass through verbatim, got: %s", got)
	}
}

func TestPassthrough_StreamingPerChunkFlush(t *testing.T) {
	second := make(chan struct{})
	firstSeen := make(chan struct{})
	proxy, _ := newPassthroughTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fl := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: first\n\n")
		fl.Flush()
		<-firstSeen // hold the second event until the client proves it got the first
		_, _ = io.WriteString(w, "data: second\n\n")
		fl.Flush()
		close(second)
	}))

	req, _ := http.NewRequest("POST", proxy.URL+"/v1/chat/completions", strings.NewReader(`{"stream":true}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	sc := bufio.NewScanner(resp.Body)
	if !sc.Scan() || sc.Text() != "data: first" {
		t.Fatalf("first event not delivered before second was written: %q", sc.Text())
	}
	close(firstSeen)

	select {
	case <-second:
	case <-time.After(5 * time.Second):
		t.Fatal("second event never arrived")
	}
	for sc.Scan() {
		if sc.Text() == "data: second" {
			return // success: true incremental delivery
		}
	}
	t.Fatal("second event lost")
}

func TestPassthrough_ClientDisconnectCancelsUpstream(t *testing.T) {
	var canceled atomic.Int32
	proxy, _ := newPassthroughTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		<-r.Context().Done()
		canceled.Add(1)
	}))

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "POST", proxy.URL+"/v1/chat/completions", strings.NewReader(`{}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	cancel()

	deadline := time.Now().Add(5 * time.Second)
	for canceled.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if canceled.Load() == 0 {
		t.Fatal("upstream request not canceled after client disconnect")
	}
}

func TestPassthrough_LargeBodyUnbuffered(t *testing.T) {
	// A multi-MiB request body must stream through without proxy-side
	// buffering limits applying (max_body_bytes guards the parsed
	// /v1/responses route only).
	var got int64
	proxy, _ := newPassthroughTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, _ := io.Copy(io.Discard, r.Body)
		got = n
		w.WriteHeader(200)
	}))

	big := strings.NewReader(strings.Repeat("x", 8<<20)) // 8 MiB
	resp, err := http.Post(proxy.URL+"/v1/chat/completions", "application/json", big)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got != 8<<20 {
		t.Fatalf("body truncated or buffered: got %d bytes upstream", got)
	}
}

func TestMetrics_PassthroughCounter(t *testing.T) {
	proxy, _ := newPassthroughTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	http.Get(proxy.URL + "/v1/models")
	http.Get(proxy.URL + "/v1/models")

	m, err := http.Get(proxy.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(m.Body)
	m.Body.Close()
	if !strings.Contains(string(body), "protocol_proxy_passthrough_requests_total 2") {
		t.Fatalf("passthrough counter missing or wrong:\n%s", string(body)[:min(400, len(body))])
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
