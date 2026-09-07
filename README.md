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
PROXY_UPSTREAM_BASE_URL="http://127.0.0.1:8000/v1" ./synapse
```

Docker:

```bash
docker build -t synapse .
docker run --rm -p 8787:8787 \
  -e PROXY_UPSTREAM_BASE_URL="http://host.docker.internal:8000/v1" \
  synapse
```

npm (prebuilt binary; Node is only the launcher — esbuild-style
distribution):

```bash
npm install -g @dncore/synapse
synapse init                    # writes the annotated ~/.config/synapse/config.yaml
$EDITOR ~/.config/synapse/config.yaml   # set upstream.base_url
synapse service install --now   # systemd user unit / launchd agent, autostart
synapse status                  # service state + /health /ready + version
```

`service install` bakes the npm-resolved binary path into a generated
unit file, so no hand-written unit with guessed paths. For a one-off
foreground run instead: `synapse --config <path>`. See
[`synapse --help`](cmd/synapse/main.go) for the full command surface.

Or grab a release binary directly:

```bash
curl -LO https://github.com/dncore/synapse-protocol-adapter/releases/latest/download/synapse-linux-x64
chmod +x synapse-linux-x64 && ./synapse-linux-x64 --version
```

Then point any Responses-API client at `http://<host>:8787/v1`.

## Running the daemon

One static binary, three interchangeable hosting options. Behavior —
configuration, routing (`responses_mode`), streaming, metrics — is
identical in all three; pick by environment:

| Option | Start | Best for |
|---|---|---|
| **Bare process** | `./synapse --config config.yaml` | Development, quick experiments |
| **systemd service** | `systemctl enable --now synapse` | Linux servers, always-on daemons (auto-restart, journald, graceful drain) |
| **launchd agent** | `launchctl bootstrap gui/$(id -u) …` | macOS hosts; see [macOS (launchd)](#macos-launchd) |
| **Docker / Compose** | `docker run -p 8787:8787 synapse` | Containerized environments; see [Docker](#docker), [systemd](#systemd), [macOS (launchd)](#macos-launchd) for full setup |

How the daemon is hosted is orthogonal to how it routes: the
`upstream.responses_mode` knob (`convert` vs `passthrough`) and the
transparent `/v1/*` forwarding behave the same under all three.

## Performance

**Measured, end-to-end** (`tests/load` against a deterministic local
upstream, i7-14650HX, all three components on one busy dev machine —
treat as conservative real-world numbers, not lab conditions):

| Path (100 concurrent) | Throughput | p50 | p95 | p99 |
|---|---|---|---|---|
| Direct to upstream (no proxy, baseline) | 41,700 req/s | 1.25 ms | 8.1 ms | 12.6 ms |
| **Convert** (`POST /v1/responses`) | ~35,000 req/s | 2.41 ms | 6.5 ms | 10.2 ms |
| **Passthrough** (`POST /v1/chat/completions`) | ~35,000 req/s | 2.65 ms | 6.4 ms | 8.6 ms |

- **Proxy tax: ~+1.2 ms p50** for full Responses↔Chat conversion
  (parse, convert, re-serialize), ~+1.4 ms for byte-forwarding — while
  sustaining ~35k req/s on a single core-pair. Zero errors across the
  runs.
- Concurrency sweep (convert): 28.0k req/s @ 10 workers (p99 0.87 ms),
  34.8k req/s @ 200 workers (p99 21.8 ms) — no collapse, latency scales
  gently.
- **Streaming** (100 concurrent streams, 204-chunk responses):
  3,580 streams/s ≈ **730k converted SSE events/s**; first-event latency
  p50 17.9 ms / p95 46.8 ms (includes the local upstream's own 200
  sequential chunks; against a real LLM, provider time-to-first-token
  dominates and the proxy adds ~1 ms).
- **Footprint**: RSS 11 MB idle → ~26–46 MB under sustained load,
  stable (no growth across runs); single static binary, no runtime deps.

Conversion-layer micro-benchmarks (`make bench`) — where the nanoseconds
go:

```
BenchmarkConvertRequest-16     2612932    897.9 ns/op
BenchmarkConvertResponse-16   11060269    210.9 ns/op
BenchmarkStreamer-16             82387  29467 ns/op   # ~203-chunk stream ≈ 145 ns/chunk
```

Reproduce with:

```bash
make bench                                                   # micro
go run ./tests/load -url http://127.0.0.1:8787/v1/responses \
    -concurrency 100 -duration 30s                           # e2e
go run ./tests/load -url http://127.0.0.1:8787/v1/responses \
    -concurrency 100 -duration 30s -stream                   # first-byte percentiles
```

In production the LLM provider is the bottleneck by 2–4 orders of
magnitude; the proxy layer stays out of the way.

Runtime observability is built in: `/metrics` exposes request, upstream,
and first-byte latency histograms (p50/p95/p99 derivable) plus gauges
for active connections/requests — so you can capture the same numbers
against your real provider at any time.

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
| `upstream.responses_mode` | `convert` | `PROXY_UPSTREAM_RESPONSES_MODE` | `convert` translates `POST /v1/responses` to chat completions; `passthrough` forwards it to the upstream's **native** `/responses` endpoint (for providers that already implement the Responses API — keeps reasoning items, server-side tools, store semantics intact) |
| `limits.max_concurrency` | `200` | `PROXY_LIMITS_MAX_CONCURRENCY` | Max in-flight requests; others queue (backpressure). `0` = unlimited |
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
synapse check-config --config config.yaml
```

Invalid configuration exits non-zero with the exact field and expectation.

## Docker

Multi-stage build ending in **distroless/static** (no shell, no package
manager, CA certificates included) running as the `nonroot` user:

```bash
docker build -t synapse .

docker run --rm -p 8787:8787 \
  -v "$PWD/config.example.yaml:/etc/synapse/config.yaml:ro" \
  synapse
```

Behind a restricted network, build with a Go module mirror:

```bash
docker build --build-arg GOPROXY=https://goproxy.cn,direct -t synapse .
```

The image defines a `HEALTHCHECK` backed by the binary's built-in
`healthcheck` subcommand (distroless ships no curl):

```dockerfile
HEALTHCHECK --interval=30s --timeout=5s --retries=3 \
    CMD ["/synapse", "healthcheck"]
```

### Docker Compose — upstream on the host

```yaml
services:
  synapse:
    image: synapse:latest
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

Installed via npm, or without root? Skip this block — `synapse init &&
synapse service install --now` generates a **user** unit
(`~/.config/systemd/user/synapse.service`) with the resolved binary
path, enables linger for boot autostart, and the same lifecycle
(`synapse start|stop|restart|status`, `journalctl --user -u synapse -f`).
The root setup below is for system-wide installs:

```bash
sudo useradd --system --home /nonexistent --shell /usr/sbin/nologin synapse || true
sudo install -Dm755 synapse /usr/local/bin/synapse
sudo install -Dm644 config.example.yaml /etc/synapse/config.yaml
sudo cp deploy/systemd/synapse.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now synapse

journalctl -u synapse -f        # structured JSON logs in journald
sudo systemctl restart synapse  # drains, then restarts
```

The unit runs as a dedicated non-root system user, restarts on failure,
and gives SIGTERM 35 s to drain.

## macOS (launchd)

The native always-on mechanism on macOS is launchd. npm users can skip
the manual steps: `synapse init && synapse service install --now`
generates `~/Library/LaunchAgents/com.synapse.proxy.plist` with the
resolved paths (logs at `~/Library/Logs/synapse.log`). The repo also
ships a ready LaunchAgent at [`deploy/launchd/`](deploy/launchd/) for
release-binary installs:

**1. Cross-compile** (from any machine; pure Go, no CGO — Intel Macs use
`DARWIN_ARCH=amd64`):

```bash
make build-darwin                # → synapse-darwin-arm64
scp synapse-darwin-arm64 mac:/usr/local/bin/synapse
```

**2. Install config** at `/usr/local/etc/synapse/config.yaml`
(same format as Linux).

**3. Install the agent** (per-user, starts at login, no root):

```bash
mkdir -p ~/Library/LaunchAgents
cp deploy/launchd/com.synapse.proxy.plist ~/Library/LaunchAgents/
# adjust binary/config paths inside the plist if you installed elsewhere
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.synapse.proxy.plist
```

**4. Manage** — SIGTERM triggers the same graceful drain as systemd:

```bash
launchctl kickstart -k gui/$(id -u)/com.synapse.proxy   # restart
launchctl bootout      gui/$(id -u)/com.synapse.proxy   # stop & unload
tail -f /tmp/synapse.log                                   # logs
curl -s http://127.0.0.1:8787/health                              # verify
```

Notes vs Linux: logs go to the plist's `StandardOutPath` file instead of
journald; `KeepAlive` replaces `Restart=`; for LAN access, allow inbound
connections in System Settings → Network → Firewall. Docker on macOS
also works unchanged, with `host.docker.internal` resolving to the Mac
itself.

## LAN deployment

```
Developer Machine A (LLM + proxy)          Developer Machine B (Codex)
  synapse :8787   <------ LAN ----->   base_url http://A:8787/v1
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
model_provider = "synapse"

[model_providers.synapse]
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

## Testing

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
