# Synapse Protocol Adapter

**[English](README.md)** | **[中文文档](README.zh-CN.md)**

A small, stateless, high-concurrency protocol adapter that exposes the
**OpenAI Responses API** to your clients and speaks the **OpenAI Chat
Completions API** to your upstream provider. Point Codex (or any
Responses-API agent) at it, run any chat-completions backend behind it.

```
Client / Codex / Agent              (speaks Responses API)
        |
        |  POST /v1/responses
        v
+-------------------------------------------+
|         synapse-protocol-adapter          |
|                                           |
|   Responses  ->  Chat Completions         |
|   Streaming SSE conversion                |
|   Tool-call conversion                    |
|   Header passthrough                      |
+-------------------------------------------+
        |
        |  POST /v1/chat/completions
        v
Custom LLM Provider                 (speaks Chat Completions API)
```

It is a **transparent protocol adapter** — nothing more:

- ❌ No API key management — never generates, stores, or rewrites credentials
- ❌ No accounts, OAuth, billing, or key pools
- ❌ No session/conversation storage — fully stateless across restarts
- ✅ Forwards client `Authorization` and all other headers **verbatim** to upstream
- ✅ True chunk-by-chunk SSE streaming with immediate flush
- ✅ Client disconnect cancels the upstream request immediately
- ✅ Single static binary · Docker · systemd · graceful drain on SIGTERM

## Architecture

```
                        ┌───────────────────────────────────────────────────────────────┐
                        │                    synapse-protocol-adapter                   │
                        │                                                               │
  Codex / Agents        │  ┌────────────┐    ┌────────────┐    ┌────────────────────┐   │      LLM Provider
  (Responses API)       │  │  responses │    │ converter  │    │    completions     │   │   (Chat Completions)
                        │  │   types    │───▶│ pure funcs │───▶│       types        │───┼───────────────────▶
   POST /v1/responses   │  │  (wire)    │    │            │    │   (upstream IR)    │   │  POST {base}/chat/
  ──────────────────────┼─▶│            │    └────────────┘    └────────────────────┘   │       completions
   Authorization ───────┼─▶│            │          │                        ▲           │
   X-*, OpenAI-* ───────┼─▶│            │          ▼                        │           │
                        │  │            │    ┌────────────┐                 │           │
                        │  │            │    │ Streamer   │  chunk in,      │           │
                        │  │            │    │ (state     │  events out     │           │
                        │  │            │    │  machine)  │                 │           │
                        │  │            │    └────────────┘                 │           │
                        │  └────────────┘          │                        │           │
                        │         ▲                ▼                        │           │
                        │  ┌────────────┐    ┌────────────┐    ┌────────────────────┐   │
   SSE events           │  │ streaming: │◀───│ streaming: │◀───│  upstream client   │───┼──── SSE / JSON
  ◀─────────────────────┼──│ writer     │    │ reader     │    │  (header filtering,│   │◀──────────────────
   flushed per event    │  │ (flush/evt)│    │ (split-    │    │   keep-alive pool) │   │
                        │  └────────────┘    │  safe)     │    └────────────────────┘   │
                        │                    └────────────┘                             │
                        │  middleware: request-id -> access-log -> recover               │
                        │  ops: /health /ready /metrics   lifecycle: graceful shutdown   │
                        └───────────────────────────────────────────────────────────────┘
```

Streaming pipeline (no full-response buffering anywhere):

```
upstream SSE ──▶ line-incremental parser ──▶ converter.Streamer ──▶ Responses event ──▶ flush ──▶ client
   (bytes)         (TCP/UTF-8/JSON split-      (per-request state:     (sequence_number       (immediate,
                    safe, bounded buffers)      output items, tool       assigned, ordered)     per event)
                                                 call accumulators)
```

Design invariants:

| Invariant | How |
|---|---|
| Stateless | All conversion state lives inside one request's `converter.Streamer`; nothing survives the response |
| Streaming first | Each upstream chunk is converted and flushed the moment it arrives |
| Cancellation first | `r.Context()` → upstream request context; mid-stream client-write failure cancels explicitly |
| Backpressure | Zero buffering: a blocked client write stops the pump reading upstream, propagating TCP backpressure |
| Transparent | All headers pass through except RFC hop-by-hop + `Host`/`Content-Length`/`Accept-Encoding` |

## Quick start

Binary:

```bash
git clone https://github.com/dncore/synapse-protocol-adapter
cd synapse-protocol-adapter
make build
PROXY_UPSTREAM_BASE_URL="http://127.0.0.1:8000/v1" ./protocol-proxy
```

Docker:

```bash
docker build -t protocol-proxy .
docker run --rm -p 8787:8787 \
  -e PROXY_UPSTREAM_BASE_URL="http://host.docker.internal:8000/v1" \
  protocol-proxy
```

Then point any Responses-API client at `http://<host>:8787/v1`.

## Protocol conversion semantics

| Endpoint | Description |
|---|---|
| `POST /v1/responses` | Full conversion, streaming and non-streaming |
| `ANY /v1/*` (except `/v1/responses`) | **Transparent forward** to the upstream — same method/path/query, body streamed unmodified, response streamed byte-for-byte (see below) |
| `GET /health` | Liveness — 200 while the process is up |
| `GET /ready` | Readiness — 503 once shutdown begins |
| `GET /metrics` | Prometheus text format |
| `GET /version` | Build version |
| `GET /` | Endpoint listing |

### Transparent passthrough (`/v1/chat/completions`, `/v1/models`, …)

Everything under `/v1/` except `/v1/responses` is forwarded verbatim to the
configured upstream: the `/v1` prefix is stripped, the path, query, method,
and headers (per the same filtering rules) go through, the request body is
**never parsed or buffered**, and the response — including error statuses
and bodies — is streamed back byte-for-byte with a flush per read.

This means one port speaks both protocols: Responses clients hit
`/v1/responses` (converted), native chat-completions clients hit
`/v1/chat/completions` (forwarded as-is), and Codex's model picker gets a
real answer from `/v1/models`. It replaces a local `socat`/TCP forwarder
for this upstream — with connection pooling, client-disconnect
cancellation, backpressure, and metrics that socat does not provide.

Scope note: this is a scoped reverse proxy for the **single configured
upstream**, not an open proxy — `/v1/*` reaches that provider and nothing
else. Only HTTP is forwarded (no WebSocket upgrades).

### Request mapping (Responses → Chat Completions)

| Responses | Chat Completions |
|---|---|
| `model` | `model` |
| `instructions` | prepended `system` message (skipped if input already has one) |
| `input` (string) | one `user` message |
| `prompt` (legacy alternative) | honored when `input` is absent |
| `developer` role messages | normalized to `system` (semantically identical, universally accepted) |
| consecutive `function_call` items (parallel calls) | merged into ONE assistant message with N `tool_calls` |
| `function_call` with only `id`, no `call_id` | `id` used as the tool call id |
| `reasoning.effort` | forwarded as chat `reasoning_effort` (unsupported providers ignore it) |
| tool `parameters` without top-level `type` | `{"type":"object"}` injected (several providers require it) |
| `input[]` `message` items | messages (`system`/`developer`/`user`/`assistant` roles pass through) |
| `input_text` / `output_text` / `summary_text` parts | `{type:"text"}` parts |
| `input_image` parts | `{type:"image_url", image_url:{url}}` parts |
| `function_call` items | assistant message with `tool_calls[]` |
| `function_call_output` items | `{role:"tool", tool_call_id, content}` (string **or** content-part array outputs) |
| `refusal` content parts (history) | text parts carrying the refusal message |
| `reasoning` items | dropped (no chat equivalent) |
| `tools[]` (flattened) | `tools[]` (nested `function` object) |
| `tool_choice` (`"auto"`/`"none"`/`"required"` or object form) | normalized; Cursor-style `{"type":"none"}` etc. map to their string forms, `{"type":"tool"}` → `"required"`, `{"type":"function","name"}` re-nested |
| `max_output_tokens` | `max_tokens` |
| `temperature`, `top_p`, `parallel_tool_calls`, `user` | same names |
| `response_format` / `text.format` (`json_schema`/`json_object`/`text`) | `response_format` |
| `stream: true` | `stream: true` + `stream_options:{include_usage:true}` |
| `store`, `metadata`, `include`, `truncation`, `reasoning` | ignored |
| `previous_response_id` | **rejected with 400** — requires server-side session state this proxy deliberately does not have; send the full conversation in `input` (Codex does this by default) |

### Response mapping (Chat Completions → Responses)

| Upstream | Client sees |
|---|---|
| `choices[0].message.content` | `output[]: {type:"message", content:[{type:"output_text"}]}` |
| `choices[0].message.refusal` | `{type:"refusal"}` content part |
| `choices[0].message.reasoning_content` (DeepSeek-style) | `{type:"reasoning"}` output item with `summary_text` |
| `choices[0].message.tool_calls[]` | one `{type:"function_call"}` output item each (parallel calls preserved, unique ids) |
| `finish_reason: stop / tool_calls` | `status: "completed"` + `response.completed` |
| `finish_reason: length` | `status: "incomplete"` + `response.incomplete` event + `max_output_tokens` reason |
| `finish_reason: content_filter` | `status: "incomplete"` + `response.incomplete` event + `content_filter` reason |
| `finish_reason: refusal` | `status: "incomplete"` + refusal content part |
| `usage.prompt/completion/total_tokens` | `usage.input/output/total_tokens` |
| `usage.prompt_tokens_details.cached_tokens` | `usage.input_tokens_details.cached_tokens` |
| `usage.completion_tokens_details.reasoning_tokens` | `usage.output_tokens_details.reasoning_tokens` |
| upstream HTTP error | same status, Responses-shaped error body |

### Streaming event mapping

```
upstream chunk                          Responses event(s)
--------------------------------------- -------------------------------------------
first chunk                             response.created, response.in_progress
delta.reasoning_content (DeepSeek)      response.output_item.added (reasoning),
                                        response.reasoning_summary_part.added
delta.reasoning_content                 response.reasoning_summary_text.delta
delta.content (first)                   response.output_item.added (message),
                                        response.content_part.added
delta.content                           response.output_text.delta
delta.refusal                           response.refusal.delta
delta.tool_calls[i] (first appearance)  response.output_item.added (function_call)
delta.tool_calls[i].function.arguments  response.function_call_arguments.delta
finish_reason                           reasoning_summary_text.done (if any),
                                        output_text.done / refusal.done,
                                        content_part.done, output_item.done,
                                        function_call_arguments.done
final usage chunk (empty choices)       (accounted into usage)
[DONE]                                  response.completed — or response.incomplete
                                        when finish_reason was length/content_filter
```

Events carry strictly increasing `sequence_number`. Tool-call arguments
accumulate per delta `index`, so interleaved parallel tool calls reassemble
correctly. A stream that dies mid-flight produces an in-band `error` SSE
event (headers are already committed at that point).

### Header handling

Everything not listed below is forwarded upstream unchanged — including
`Authorization`, `OpenAI-*`, `X-*`, and custom headers:

- Hop-by-hop (RFC 9110): `Connection`, `Keep-Alive`, `Proxy-Authenticate`,
  `Proxy-Authorization`, `TE`, `Trailer`, `Transfer-Encoding`, `Upgrade`
- Recomputed by the proxy: `Host`, `Content-Length`
- `Accept-Encoding`: the proxy must parse the SSE body, so Go's transport
  negotiates gzip itself and decompresses transparently

## Configuration

YAML file + environment overrides. **The configuration contains no
credentials by design.** See [`config.example.yaml`](config.example.yaml).

| Key | Default | Env override | Description |
|---|---|---|---|
| `server.listen` | `0.0.0.0:8787` | `PROXY_SERVER_LISTEN` | Listen address; `0.0.0.0` exposes to the LAN |
| `upstream.base_url` | `http://127.0.0.1:8000/v1` | `PROXY_UPSTREAM_BASE_URL` | Everything before the completions path |
| `upstream.path` | `/chat/completions` | `PROXY_UPSTREAM_PATH` | Appended to `base_url`; `/completions` for legacy backends |
| `limits.max_concurrency` | `200` | `PROXY_LIMITS_MAX_CONCURRENCY` | Max in-flight requests; others queue (backpressure) |
| `limits.max_body_bytes` | `67108864` | `PROXY_LIMITS_MAX_BODY_BYTES` | Max client request body (bytes) |
| `limits.max_sse_line_bytes` | `16777216` | — | Max single SSE data line (large tool arguments) |
| `timeouts.connect` | `10s` | `PROXY_TIMEOUTS_CONNECT` | TCP/TLS connect to upstream |
| `timeouts.request` | `30m` | `PROXY_TIMEOUTS_REQUEST` | Total per-request ceiling (long generations) |
| `timeouts.idle` | `5m` | `PROXY_TIMEOUTS_IDLE` | Idle keep-alive connections |
| `timeouts.stream_write` | `5m` | `PROXY_TIMEOUTS_STREAM_WRITE` | Max stall writing to a slow streaming client |
| `timeouts.header_write` | `60s` | — | Max wait for client request headers |
| `shutdown.timeout` | `30s` | `PROXY_SHUTDOWN_TIMEOUT` | Grace period for in-flight requests on SIGTERM |
| `log.format` | `json` | `PROXY_LOG_FORMAT` | `json` or `text` |
| `log.level` | `info` | `PROXY_LOG_LEVEL` | `debug`/`info`/`warn`/`error` |

Validate without starting:

```bash
protocol-proxy check-config --config config.yaml
```

Invalid configuration exits non-zero with the exact field and expectation.

## Docker

Multi-stage build ending in **distroless/static** (no shell, no package
manager, CA certificates included) running as the `nonroot` user:

```bash
docker build -t protocol-proxy .

docker run --rm -p 8787:8787 \
  -v "$PWD/config.example.yaml:/etc/protocol-proxy/config.yaml:ro" \
  protocol-proxy
```

Behind a restricted network, build with a Go module mirror:

```bash
docker build --build-arg GOPROXY=https://goproxy.cn,direct -t protocol-proxy .
```

The image defines a `HEALTHCHECK` backed by the binary's built-in
`healthcheck` subcommand (distroless ships no curl):

```dockerfile
HEALTHCHECK --interval=30s --timeout=5s --retries=3 \
    CMD ["/protocol-proxy", "healthcheck"]
```

### Docker Compose — upstream on the host

```yaml
services:
  protocol-proxy:
    image: protocol-proxy:latest
    build: .
    ports: ["8787:8787"]
    environment:
      PROXY_UPSTREAM_BASE_URL: "http://host.docker.internal:8000/v1"
    restart: unless-stopped
    stop_grace_period: 35s   # > shutdown.timeout so SIGTERM drains fully
```

### Docker Compose — everything containerized

See [`docker-compose.example.yml`](docker-compose.example.yml); the proxy
reaches the provider by its service name on the compose network:

```yaml
PROXY_UPSTREAM_BASE_URL: "http://llm-provider:8000/v1"
```

Upstream addressing cheat sheet:

| Upstream location | `upstream.base_url` |
|---|---|
| Same host, proxy on bare metal | `http://127.0.0.1:8000/v1` |
| Same host, proxy in Docker (Linux) | `http://172.17.0.1:8000/v1`, or `--add-host=host.docker.internal:host-gateway` |
| Same host, proxy in Docker (macOS/Windows) | `http://host.docker.internal:8000/v1` |
| Another container on the same compose network | `http://llm-provider:8000/v1` |
| Remote machine | `http://10.1.2.3:8000/v1` |

On SIGTERM, Docker's `stop_grace_period` (keep it above
`shutdown.timeout`) lets in-flight streams drain; readiness flips to 503
immediately, the process exits 0 when drained.

## systemd

```bash
sudo useradd --system --home /nonexistent --shell /usr/sbin/nologin protocol-proxy || true
sudo install -Dm755 protocol-proxy /usr/local/bin/protocol-proxy
sudo install -Dm644 config.example.yaml /etc/protocol-proxy/config.yaml
sudo cp deploy/systemd/protocol-proxy.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now protocol-proxy

journalctl -u protocol-proxy -f        # structured JSON logs in journald
sudo systemctl restart protocol-proxy  # drains, then restarts
```

The unit runs as a dedicated non-root system user, restarts on failure,
and gives SIGTERM 35 s to drain.

## LAN deployment

```
Developer Machine A (LLM + proxy)          Developer Machine B (Codex)
  protocol-proxy :8787   <------ LAN ----->   base_url http://A:8787/v1
```

1. On A: `server.listen: "0.0.0.0:8787"`, start the proxy.
2. Open the port:

```bash
sudo firewall-cmd --add-port=8787/tcp --permanent && sudo firewall-cmd --reload   # firewalld
sudo ufw allow 8787/tcp                                                          # ufw
sudo iptables -A INPUT -p tcp --dport 8787 -j ACCEPT                             # iptables
```

3. Point clients on B at `http://<A's-LAN-IP>:8787/v1`.

## Codex configuration

Codex believes it is talking to a real Responses API; the adapter
translates internally. Your API key handling stays exactly as it is — the
`Authorization` header Codex sends is forwarded upstream unchanged.

```toml
model = "your-model"
model_provider = "protocol-proxy"

[model_providers.protocol-proxy]
name = "Custom Completions Provider"
base_url = "http://192.168.1.100:8787/v1"
wire_api = "responses"
```

## curl examples

Non-streaming:

```bash
curl http://127.0.0.1:8787/v1/responses \
  -H "Authorization: Bearer $UPSTREAM_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"your-model","input":"Explain backpressure in one sentence."}'
```

Streaming:

```bash
curl -N http://127.0.0.1:8787/v1/responses \
  -H "Authorization: Bearer $UPSTREAM_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"your-model","input":"Count from 1 to 5.","stream":true}'
```

Tool-call round trip:

```bash
# Turn 1: model requests the tool
curl http://127.0.0.1:8787/v1/responses -H "Authorization: Bearer $K" -d '{
  "model":"your-model",
  "input":"What is the weather in Tokyo?",
  "tools":[{"type":"function","name":"get_weather",
            "parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}]
}'
# → output contains {"type":"function_call","call_id":"...","name":"get_weather","arguments":"{\"city\":\"Tokyo\"}"}

# Turn 2: feed the call + your tool result back
curl http://127.0.0.1:8787/v1/responses -H "Authorization: Bearer $K" -d '{
  "model":"your-model",
  "input":[
    {"type":"message","role":"user","content":"What is the weather in Tokyo?"},
    {"type":"function_call","call_id":"call_abc","name":"get_weather","arguments":"{\"city\":\"Tokyo\"}"},
    {"type":"function_call_output","call_id":"call_abc","output":"{\"temp_c\":18,\"sky\":\"cloudy\"}"}
  ],
  "tools":[{"type":"function","name":"get_weather","parameters":{"type":"object"}}]
}'
```

## Health / Ready / Metrics

```bash
curl http://127.0.0.1:8787/health   # 200 {"status":"ok"}     liveness
curl http://127.0.0.1:8787/ready    # 200 {"status":"ready"}  readiness (503 while shutting down)
curl http://127.0.0.1:8787/metrics  # Prometheus text format
```

Metrics (all low-cardinality; never any header, credential, or prompt data):

| Metric | Kind | Meaning |
|---|---|---|
| `protocol_proxy_requests_total` | counter | requests received |
| `protocol_proxy_responses_total` | counter | responses sent |
| `protocol_proxy_errors_total{type}` | counter | proxy-side errors |
| `protocol_proxy_upstream_errors_total{class}` | counter | upstream failures |
| `protocol_proxy_streaming_requests_total` | counter | SSE streams started |
| `protocol_proxy_passthrough_requests_total` | counter | requests transparently forwarded (no conversion) |
| `protocol_proxy_tool_calls_total` | counter | tool calls observed in output |
| `protocol_proxy_bytes_in_total` / `_bytes_out_total` | counter | body bytes in/out |
| `protocol_proxy_upstream_requests_total` | counter | upstream calls issued |
| `protocol_proxy_active_connections` / `_active_requests` | gauge | live connections / in-flight requests |
| `protocol_proxy_request_duration_seconds` | histogram | end-to-end latency |
| `protocol_proxy_upstream_duration_seconds` | histogram | time to upstream response headers |
| `protocol_proxy_first_byte_latency_seconds` | histogram | request start → first SSE event flushed |

Logging is structured JSON (slog): `timestamp`, `level`, `request_id`,
`method`, `path`, `status`, `bytes_out`, `latency_ms`. The proxy never
logs `Authorization` headers, API keys, prompts, or bodies — there is no
code path that could.

## Performance

Conversion-layer micro-benchmarks (`make bench`, i7-14650HX):

```
BenchmarkConvertRequest-16     2612932    897.9 ns/op
BenchmarkConvertResponse-16   11060269    210.9 ns/op
BenchmarkStreamer-16             82387  29467 ns/op   # ~203-chunk stream ≈ 145 ns/chunk
```

The adapter adds well under a millisecond of CPU per request and ~150 ns
per streamed chunk — the proxy layer is not the bottleneck at 100+
concurrent streams.

Load test against a running instance:

```bash
go run ./tests/load -url http://127.0.0.1:8787/v1/responses -concurrency 100 -duration 30s
go run ./tests/load -url http://127.0.0.1:8787/v1/responses -concurrency 100 -duration 30s -stream
```

Reports req/s, errors, throughput, latency p50/p95/p99, and — in streaming
mode — first-SSE-event latency percentiles.

```bash
make test    # unit + e2e suite
make race    # same, under the race detector
```

The suite covers converter tables, SSE parsing under one-byte-per-read
segmentation, UTF-8 split boundaries, streaming event sequences with
parallel tool calls, Authorization passthrough, client-disconnect
cancellation propagation, upstream error relay, readiness flip on
shutdown, and 10/50/100/200-way concurrency.

## Troubleshooting

| Symptom | Likely cause / fix |
|---|---|
| `400 previous_response_id is not supported` | Client relies on server-side session state; send full history in `input` (Codex default). |
| `502 ... connection refused` | Provider unreachable; check `upstream.base_url`. From Docker to host use `host.docker.internal` (+`--add-host` on Linux). |
| SSE never streams / buffered | A reverse proxy in front buffers (nginx); set `proxy_buffering off;` (the adapter already sends `X-Accel-Buffering: no`). |
| `400 unsupported input item type` | Client sent item types with no chat-completions equivalent (e.g. `computer_call`). |
| Streams cut at exactly 5 min | `timeouts.stream_write` hit on a stalled client; raise if legitimate. |
| Tool calls not parsed by client | Provider must emit `delta.tool_calls[].index` (standard OpenAI shape); check tools are enabled on the provider. |

## Security notes

> **Do not expose this adapter directly to the public internet.**

It performs **no authentication** and forwards whatever `Authorization`
header the client sends — by design. Anyone who can reach the port can
spend your upstream's quota. Intended placement:

```
Internet  ✗  (never direct)
Trusted LAN / tailnet / VPN  →  synapse-protocol-adapter  →  LLM provider
```

If you must expose it beyond a trusted network, put an authenticating
reverse proxy in front (mTLS, OAuth2-proxy, Cloudflare Access, Tailscale
Funnel, …) or add your own auth middleware — the middleware layer is
isolated precisely to make that a one-file addition.

## License

[MIT](LICENSE) © 2026 dncore
