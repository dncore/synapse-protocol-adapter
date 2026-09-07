// Package upstream issues chat completions requests to the configured
// provider. Its defining behavior is transparency: client headers are
// forwarded verbatim except for hop-by-hop headers and the few fields the
// HTTP protocol forces the proxy to recompute. No Authorization is ever
// generated, stored, or rewritten here.
package upstream

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/dncore/synapse-protocol-adapter/internal/config"
)

// hopByHop lists the connection-scoped headers RFC 9110 forbids proxies
// from forwarding. Everything else — Authorization, OpenAI-*, X-* — flows
// through untouched.
var hopByHop = map[string]struct{}{
	"Connection":          {},
	"Keep-Alive":          {},
	"Proxy-Authenticate":  {},
	"Proxy-Authorization": {},
	"Te":                  {},
	"Trailer":             {},
	"Transfer-Encoding":   {},
	"Upgrade":             {},
}

// mustRecompute lists end-to-end headers the proxy necessarily replaces:
// routing (Host), framing (Content-Length), and compression (we must parse
// the SSE body, so the transport negotiates its own encoding).
var mustRecompute = map[string]struct{}{
	"Host":            {},
	"Content-Length":  {},
	"Accept-Encoding": {},
}

// Client is the upstream HTTP client. It is safe for concurrent use.
type Client struct {
	http *http.Client
	url  string // converted-route endpoint (base + configured path)
	base string // base_url without trailing slash, for passthrough routing
	root string // scheme://host of base_url, for host-root passthrough (/api/anthropic/*)
	cfg  config.Config
}

// NewClient builds a tuned client. The transport keeps a large per-host
// idle pool (LLM providers benefit from keep-alive and HTTP/2 multiplexing)
// and imposes no response-header timeout: reasoning models can legitimately
// take minutes before the first byte; total request duration is bounded by
// the per-request context instead.

// idleConnsPerHost sizes the per-host idle pool: matched to the
// concurrency cap when one is set, large-but-bounded when unlimited.
func idleConnsPerHost(maxConcurrency int) int {
	if maxConcurrency > 0 {
		return maxConcurrency
	}
	return 10000
}

func NewClient(cfg config.Config) *Client {
	tr := &http.Transport{
		Proxy: nil, // never route the data path through a corporate proxy silently
		DialContext: (&net.Dialer{
			Timeout:   cfg.Timeouts.Connect,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          cfg.Limits.MaxConcurrency, // 0 = no limit
		// PerHost 0 would fall back to net/http's default of 2 idle
		// conns — fatal for a high-concurrency proxy — so unlimited
		// mode gets a large explicit pool instead.
		MaxIdleConnsPerHost:   idleConnsPerHost(cfg.Limits.MaxConcurrency),
		MaxConnsPerHost:       0, // unbounded; concurrency is bounded upstream of us
		IdleConnTimeout:       cfg.Timeouts.Idle,
		TLSHandshakeTimeout:   cfg.Timeouts.Connect,
		ExpectContinueTimeout: time.Second,
		ForceAttemptHTTP2:     true,
		// DisableCompression left false: with Accept-Encoding stripped, Go
		// negotiates gzip itself and transparently decompresses, so the SSE
		// parser sees plain bytes.
	}
	root := ""
	if u, err := url.Parse(cfg.Upstream.BaseURL); err == nil {
		root = u.Scheme + "://" + u.Host
	}
	return &Client{
		http: &http.Client{Transport: tr},
		url:  cfg.Upstream.URL(),
		base: strings.TrimRight(cfg.Upstream.BaseURL, "/"),
		root: root,
		cfg:  cfg,
	}
}

// DoPassthrough issues an arbitrary request to the configured upstream,
// targeting base_url + path with the client's headers attached. It is the
// transparent-forwarding counterpart of Do: no body transformation, no
// endpoint assumptions. path is the request path below /v1 (e.g.
// "/chat/completions"), joined onto the configured base_url; rawQuery
// is forwarded verbatim.
func (c *Client) DoPassthrough(ctx context.Context, method, path, rawQuery string, body io.Reader, clientHeaders http.Header) (*http.Response, error) {
	return c.do(ctx, c.base+path, method, rawQuery, body, clientHeaders)
}

// DoPassthroughRoot forwards with the path preserved against the
// upstream HOST root, ignoring any path in base_url. Anthropic-protocol
// mounts live beside (not under) the chat-completions base — e.g.
// gateway OpenAI at /api/v1/*, Anthropic at /api/anthropic/* — so a
// client's /api/anthropic/v1/messages must land at exactly that path.
func (c *Client) DoPassthroughRoot(ctx context.Context, method, path, rawQuery string, body io.Reader, clientHeaders http.Header) (*http.Response, error) {
	return c.do(ctx, c.root+path, method, rawQuery, body, clientHeaders)
}

func (c *Client) do(ctx context.Context, u, method, rawQuery string, body io.Reader, clientHeaders http.Header) (*http.Response, error) {
	if rawQuery != "" {
		u += "?" + rawQuery
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, fmt.Errorf("build upstream request: %w", err)
	}
	ForwardRequestHeaders(req.Header, clientHeaders)
	return c.http.Do(req)
}

// ForwardRequestHeaders copies the client's headers onto the upstream
// request, dropping hop-by-hop and recomputed fields. extra carries values
// the proxy itself sets (Content-Type, Accept).
func ForwardRequestHeaders(dst http.Header, src http.Header) {
	for name, vals := range src {
		if _, hop := hopByHop[name]; hop {
			continue
		}
		if _, re := mustRecompute[name]; re {
			continue
		}
		for _, v := range vals {
			dst.Add(name, v)
		}
	}
}

// responseDropped lists upstream response headers the proxy rewrites:
// hop-by-hop framing plus content framing (the proxy re-serializes bodies).
var responseDropped = map[string]struct{}{
	"Connection":          {},
	"Keep-Alive":          {},
	"Proxy-Authenticate":  {},
	"Proxy-Authorization": {},
	"Te":                  {},
	"Trailer":             {},
	"Transfer-Encoding":   {},
	"Upgrade":             {},
	"Content-Length":      {},
	"Content-Type":        {},
}

// ForwardResponseHeaders copies upstream response headers back to the
// client, dropping hop-by-hop and framing fields the proxy rewrites.
func ForwardResponseHeaders(dst http.Header, src http.Header) {
	for name, vals := range src {
		if _, drop := responseDropped[name]; drop {
			continue
		}
		for _, v := range vals {
			dst.Add(name, v)
		}
	}
}

// Do sends the converted chat completions body upstream, with the client's
// headers attached and cancellation wired to ctx.
func (c *Client) Do(ctx context.Context, body []byte, clientHeaders http.Header, streaming bool) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build upstream request: %w", err)
	}
	req.ContentLength = int64(len(body))
	ForwardRequestHeaders(req.Header, clientHeaders)
	req.Header.Set("Content-Type", "application/json")
	if streaming {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}

	return c.http.Do(req)
}

// CloseIdleConnections drops pooled keep-alive connections; called during
// graceful shutdown so no upstream socket outlives the process.
func (c *Client) CloseIdleConnections() {
	c.http.CloseIdleConnections()
}

// ReadAllWithLimit drains r into memory, failing once the total exceeds
// limit bytes.
func ReadAllWithLimit(r io.Reader, limit int64) ([]byte, error) {
	buf, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(buf)) > limit {
		return nil, fmt.Errorf("body exceeds %d byte limit", limit)
	}
	return buf, nil
}
