# Synapse Protocol Adapter

**[English](README.md)** | **[中文文档](README.zh-CN.md)**

一个小型、无状态、高并发的协议适配器：对客户端暴露 **OpenAI Responses API**，对 upstream 使用 **OpenAI Chat Completions API**。让 Codex（或任何 Responses API 客户端）直接接入任意 chat-completions 后端。

```
Client / Codex / Agent              (Responses API)
        |
        |  POST /v1/responses   (http:// 或 https://)
        v
+-------------------------------------------+
|         synapse-protocol-adapter          |
|                                           |
|   Responses  ->  Chat Completions         |
|   Streaming SSE 转换                      |
|   Tool-call 转换                          |
|   Header 透传                             |
|   可选 TLS 监听                           |
+-------------------------------------------+
        |
        |  POST /v1/chat/completions   (http:// 或 https://)
        v
Custom LLM Provider                 (Chat Completions API)
```

它是一个**透明的协议适配器**，仅此而已：

- ❌ 不管理 API Key——从不生成、存储或改写凭据
- ❌ 无账号体系、无 OAuth、无计费、无 Key 池
- ❌ 不保存会话/对话——重启后完全无状态
- ✅ 客户端的 `Authorization` 及其他 header **原样透传**到 upstream
- ✅ 真正逐 chunk 的 SSE 流式转换，逐事件 flush
- ✅ 客户端断开立即取消 upstream 请求
- ✅ 可选 HTTPS 监听（`server.tls`）——自动生成自签名 CA 或自带证书，供强制 `https://` base_url 的 agent 使用
- ✅ 单静态二进制 · Docker · systemd · SIGTERM 优雅退出

## 架构

```
                        ┌───────────────────────────────────────────────────────────────┐
                        │                    synapse-protocol-adapter                   │
                        │                                                               │
  Codex / Agents        │  ┌────────────┐    ┌────────────┐    ┌────────────────────┐   │      LLM Provider
  (Responses API)       │  │  responses │    │ converter  │    │    completions     │   │   (Chat Completions)
                        │  │   types    │───▶│ 纯函数转换  │───▶│   (upstream IR)    │───┼───────────────────▶
   POST /v1/responses   │  │  (wire)    │    │            │    │                    │   │  POST {base}/chat/
  ──────────────────────┼─▶│            │    └────────────┘    └────────────────────┘   │       completions
   Authorization ───────┼─▶│            │          │                        ▲           │
   X-*, OpenAI-* ───────┼─▶│            │          ▼                        │           │
                        │  │            │    ┌────────────┐                 │           │
                        │  │            │    │ Streamer   │  chunk 进,       │           │
                        │  │            │    │ (流式状态机)│  events 出       │           │
                        │  │            │    └────────────┘                 │           │
                        │  └────────────┘          │                        │           │
                        │         ▲                ▼                        │           │
                        │  ┌────────────┐    ┌────────────┐    ┌────────────────────┐   │
   SSE events           │  │ streaming: │◀───│ streaming: │◀───│  upstream client   │───┼──── SSE / JSON
  ◀─────────────────────┼──│ writer     │    │ reader     │    │ (header 过滤,      │   │◀──────────────────
   逐事件 flush          │  │ (逐事件flush)│   │ (抗拆分)    │    │  keep-alive 连接池)│   │
                        │  └────────────┘    └────────────┘    └────────────────────┘   │
                        │  listener: 明文 HTTP 或 TLS 终结 (HTTP/2):                     │
                        │  自动自签 CA 或自带证书 (server.tls)                           │
                        │  middleware: request-id -> access-log -> recover               │
                        │  运维面: /health /ready /metrics   生命周期: graceful shutdown  │
                        └───────────────────────────────────────────────────────────────┘
```

流式管线（任何环节都不缓冲完整响应）：

```
upstream SSE ──▶ 逐行增量解析 ──────▶ converter.Streamer ──▶ Responses event ──▶ flush ──▶ client
   (字节流)      (TCP/UTF-8/JSON 拆分安全,   (每请求状态: 输出 item、    (携带递增             (立即、
                 有界缓冲区)               tool call 累积器)         sequence_number)      逐事件)
```

设计不变量：

| 不变量 | 实现方式 |
|---|---|
| Stateless | 所有转换状态都在单次请求的 `converter.Streamer` 内，响应结束即释放 |
| Streaming first | 每个 upstream chunk 到达即转换、即 flush |
| Cancellation first | `r.Context()` → upstream 请求 context；流中客户端写失败显式 cancel |
| Backpressure | 零缓冲：客户端写阻塞 → 停止读 upstream → TCP 背压自然传导 |
| Transparent | 除 RFC hop-by-hop + `Host`/`Content-Length`/`Accept-Encoding` 外全部透传 |

## 快速开始

二进制：

```bash
git clone https://github.com/dncore/synapse-protocol-adapter
cd synapse-protocol-adapter
make build
PROXY_UPSTREAM_BASE_URL="http://127.0.0.1:8000/v1" ./synapse
```

Docker：

```bash
docker build -t synapse .
docker run --rm -p 8787:8787 \
  -e PROXY_UPSTREAM_BASE_URL="http://host.docker.internal:8000/v1" \
  synapse
```

npm（预编译二进制，Node 只是启动器——esbuild 式分发）：

```bash
npm install -g @dncore/synapse
synapse init                    # 生成带注释的 ~/.config/synapse/config.yaml
$EDITOR ~/.config/synapse/config.yaml   # 至少改好 upstream.base_url
synapse service install --now   # 生成 systemd 用户服务 / launchd agent，开机自启
synapse status                  # 服务状态 + /health /ready + 版本，一屏汇总
```

`service install` 会把 npm 解析出的真实二进制路径写入生成的 unit
文件，无需手写 unit 猜路径。临时前台运行用 `synapse --config <path>`。
完整命令面见 `synapse --help`。

或直接下载 Release 二进制：

```bash
curl -LO https://github.com/dncore/synapse-protocol-adapter/releases/latest/download/synapse-linux-x64
chmod +x synapse-linux-x64 && ./synapse-linux-x64 --version
```

随后把任意 Responses API 客户端指向 `http://<host>:8787/v1`——打开
`server.tls.enabled: true` 即变 `https://`（见
[HTTPS 监听](#https-监听-tls)）。

## 运行 daemon 的三种方式

同一个静态二进制，三种可互换的托管方式。行为——配置、路由
（`responses_mode`）、流式、metrics——三者完全一致，按环境选择：

| 方式 | 启动 | 适用 |
|---|---|---|
| **裸进程** | `./synapse --config config.yaml` | 开发调试、快速实验 |
| **systemd 服务** | `systemctl enable --now synapse` | Linux 服务器常驻（自动重启、journald 日志、优雅退出） |
| **launchd 代理** | `launchctl bootstrap gui/$(id -u) …` | macOS 主机；见 [macOS（launchd）](#macos-launchd) |
| **Docker / Compose** | `docker run -p 8787:8787 synapse` | 容器化环境；完整步骤见 [Docker](#docker)、[systemd](#systemd)、[macOS（launchd）](#macos-launchd) |

托管方式与路由方式正交：`upstream.responses_mode`（`convert` 与
`passthrough`）和 `/v1/*` 透明转发在三种方式下行为相同。

## 性能

**实测端到端数据**（`tests/load` 对本地确定性 upstream，i7-14650HX，
三个组件同机且机器有其他负载——视为保守的真实环境值，非实验室条件）：

| 路径（100 并发） | 吞吐 | p50 | p95 | p99 |
|---|---|---|---|---|
| 直连 upstream（无代理，基线） | 41,700 req/s | 1.25 ms | 8.1 ms | 12.6 ms |
| **转换**（`POST /v1/responses`） | ~35,000 req/s | 2.41 ms | 6.5 ms | 10.2 ms |
| **透传**（`POST /v1/chat/completions`） | ~35,000 req/s | 2.65 ms | 6.4 ms | 8.6 ms |

- **代理税：p50 约 +1.2 ms**（完整 Responses↔Chat 转换：解析、转换、
  重组序列化），字节级透传约 +1.4 ms——单核对上即可支撑 ~3.5 万 req/s，
  全程零错误
- 并发扫描（转换路径）：10 并发 28.0k req/s（p99 0.87 ms）、
  200 并发 34.8k req/s（p99 21.8 ms）——无塌陷，延迟缓升
- **流式**（100 并发流、每流 204 chunk）：3,580 流/s ≈
  **每秒 73 万次 SSE 事件转换**；首事件延迟 p50 17.9 ms / p95 46.8 ms
  （含本地 upstream 自身 200 个串行 chunk；对接真实 LLM 时，
  provider 的首 token 时间占绝对主导，代理只加 ~1 ms）
- **资源占用**：空闲 RSS 11 MB → 持续负载下 26–46 MB，稳定无增长；
  单静态二进制，零运行时依赖

转换层微基准（`make bench`）——纳秒花在哪：

```
BenchmarkConvertRequest-16     2612932    897.9 ns/op
BenchmarkConvertResponse-16   11060269    210.9 ns/op
BenchmarkStreamer-16             82387  29467 ns/op   # ~203-chunk 流 ≈ 145 ns/chunk
```

复现方式：

```bash
make bench                                                   # 微基准
go run ./tests/load -url http://127.0.0.1:8787/v1/responses \
    -concurrency 100 -duration 30s                           # 端到端
go run ./tests/load -url http://127.0.0.1:8787/v1/responses \
    -concurrency 100 -duration 30s -stream                   # 首字节分位数
```

生产环境中 LLM provider 才是瓶颈（差 2–4 个数量级）；代理层保持在
视线之外。

运行时可观测性内建：`/metrics` 暴露请求、upstream、首字节延迟直方图
（可导出 p50/p95/p99）与活跃连接/请求 gauge——随时可对你的真实
provider 复测同样指标。

## 协议转换语义

| 端点 | 说明 |
|---|---|
| `POST /v1/responses` | 完整转换，流式与非流式 |
| `ANY /v1/*`（除 `/v1/responses`） | **透明转发**到 upstream——同方法/路径/query，请求 body 不解析不缓冲，响应逐字节回传（见下） |
| `GET /health` | 存活——进程运行即 200 |
| `GET /ready` | 就绪——shutdown 开始即 503 |
| `GET /metrics` | Prometheus 文本格式 |
| `GET /version` | 构建版本 |
| `GET /` | 端点列表 |

### 透明转发（`/v1/chat/completions`、`/v1/models` 等）

`/v1/` 下除 `/v1/responses` 外的一切原样转发到已配置的 upstream：剥掉
`/v1` 前缀，路径、query、方法、header（同一套过滤规则）透传，请求
body **从不解析、从不缓冲**，响应——包括错误状态码与错误体——逐字节
流式回传，每读必 flush。

一个端口同时说两种协议：Responses 客户端走 `/v1/responses`（转换），
原生 chat-completions 客户端走 `/v1/chat/completions`（原样转发），
Codex 的模型选择器也能从 `/v1/models` 拿到真实结果。它可以取代本地
的 `socat`/TCP 转发——且多了连接复用、客户端断开取消、背压和
metrics 这些 socat 没有的能力。

**Anthropic 协议客户端**：`/api/anthropic/` 下的一切同样透明转发，
但路径**按 upstream 主机根保留**、而非拼接到 `base_url`——网关
（new-api 风格）把 Anthropic 协议挂在 chat-completions base
（`/api/v1/*`）**旁边**而不是其下。把
`ANTHROPIC_BASE_URL=http://<代理主机>:<端口>/api/anthropic` 指向代理，
`/v1/messages` 就会落到网关的 `/api/anthropic/v1/messages`（Claude Code
同理）。

范围说明：这是对**单一已配置 upstream** 的限定反向代理，不是开放代
理——`/v1/*` 只能到达该 provider，到不了任何其他主机。仅转发 HTTP
（不支持 WebSocket 升级）。

### 请求映射（Responses → Chat Completions）

| Responses | Chat Completions |
|---|---|
| `model` | `model` |
| `instructions` | 前置 `system` 消息（input 已有 system 则跳过） |
| `input`（字符串） | 一条 `user` 消息 |
| `prompt`（legacy 备选） | `input` 缺失时启用 |
| `developer` 角色消息 | 归一为 `system`（语义等价，普遍被接受） |
| 连续 `function_call` item（并行调用） | 合并为一条带 N 个 `tool_calls` 的 assistant 消息 |
| 只有 `id` 没有 `call_id` 的 `function_call` | 用 `id` 作为 tool call id |
| `reasoning.effort` | 作为 chat 的 `reasoning_effort` 转发（不支持的 provider 会忽略） |
| 顶层缺 `type` 的工具 `parameters` | 注入 `{"type":"object"}`（部分 provider 强制要求） |
| `input[]` 的 `message` item | 消息（`system`/`developer`/`user`/`assistant` 角色直传） |
| `input_text` / `output_text` / `summary_text` part | `{type:"text"}` part |
| `input_image` part | `{type:"image_url", image_url:{url}}` part |
| `function_call` item | 带 `tool_calls[]` 的 assistant 消息 |
| `function_call_output` item | `{role:"tool", tool_call_id, content}`（字符串**或**内容 part 数组） |
| `refusal` 内容 part（历史） | 携带拒绝文本的 text part |
| `reasoning` item | 丢弃（chat 侧无对应） |
| `tools[]`（扁平结构） | `tools[]`（嵌套 `function` 对象） |
| `tool_choice`（`"auto"`/`"none"`/`"required"` 或对象形式） | 归一化；Cursor 风格 `{"type":"none"}` 映射为字符串形式，`{"type":"tool"}` → `"required"`，`{"type":"function","name"}` 重新嵌套 |
| `max_output_tokens` | `max_tokens` |
| `temperature`、`top_p`、`parallel_tool_calls`、`user` | 同名 |
| `response_format` / `text.format`（`json_schema`/`json_object`/`text`） | `response_format` |
| `stream: true` | `stream: true` + `stream_options:{include_usage:true}` |
| `store`、`metadata`、`include`、`truncation`、`reasoning` | 忽略 |
| `previous_response_id` | **返回 400**——它依赖服务端会话状态，而本代理刻意不保存任何会话；请把完整对话放进 `input`（Codex 默认如此） |

### 响应映射（Chat Completions → Responses）

| Upstream | 客户端看到 |
|---|---|
| `choices[0].message.content` | `output[]: {type:"message", content:[{type:"output_text"}]}` |
| `choices[0].message.refusal` | `{type:"refusal"}` 内容 part |
| `choices[0].message.reasoning_content`（DeepSeek 风格） | `{type:"reasoning"}` 输出 item（summary_text） |
| `choices[0].message.tool_calls[]` | 每项一个 `{type:"function_call"}` 输出 item（并行保留，id 唯一） |
| `finish_reason: stop / tool_calls` | `status: "completed"` + `response.completed` |
| `finish_reason: length` | `status: "incomplete"` + `response.incomplete` 事件 + `max_output_tokens` |
| `finish_reason: content_filter` | `status: "incomplete"` + `response.incomplete` 事件 + `content_filter` |
| `finish_reason: refusal` | `status: "incomplete"` + refusal 内容 part |
| `usage.prompt/completion/total_tokens` | `usage.input/output/total_tokens` |
| `usage.prompt_tokens_details.cached_tokens` | `usage.input_tokens_details.cached_tokens` |
| `usage.completion_tokens_details.reasoning_tokens` | `usage.output_tokens_details.reasoning_tokens` |
| upstream HTTP 错误 | 状态码保留，错误体重塑为 Responses 形状 |

### 流式事件映射

```
upstream chunk                          Responses 事件
--------------------------------------- -------------------------------------------
首个 chunk                             response.created, response.in_progress
delta.reasoning_content (DeepSeek)      response.output_item.added (reasoning),
                                        response.reasoning_summary_part.added
delta.reasoning_content                 response.reasoning_summary_text.delta
delta.content（首次）                   response.output_item.added (message),
                                        response.content_part.added
delta.content                           response.output_text.delta
delta.refusal                           response.refusal.delta
delta.tool_calls[i]（首次出现）         response.output_item.added (function_call)
delta.tool_calls[i].function.arguments  response.function_call_arguments.delta
finish_reason                           reasoning_summary_text.done（若有）,
                                        output_text.done / refusal.done,
                                        content_part.done, output_item.done,
                                        function_call_arguments.done
末尾 usage chunk（空 choices）           （计入 usage）
[DONE]                                  response.completed —— finish_reason 为
                                        length/content_filter 时为 response.incomplete
```

事件携带严格递增的 `sequence_number`。tool-call 参数按 delta `index`
累积，交错到达的并行 tool call 也能正确重组。中途断流的流会产生带内
`error` SSE 事件（此时 HTTP header 已提交）。

### Header 处理

以下清单之外的**所有** header 原样转发到 upstream——包括
`Authorization`、`OpenAI-*`、`X-*` 与自定义 header：

- Hop-by-hop（RFC 9110）：`Connection`、`Keep-Alive`、`Proxy-Authenticate`、
  `Proxy-Authorization`、`TE`、`Trailer`、`Transfer-Encoding`、`Upgrade`
- 由代理重算：`Host`、`Content-Length`
- `Accept-Encoding`：代理必须解析 SSE body，因此由 Go transport 自行
  协商 gzip 并透明解压

## 配置

YAML 文件 + 环境变量覆盖。**配置中刻意不含任何凭据。**
参见 [`config.example.yaml`](config.example.yaml)。

| 配置项 | 默认值 | 环境变量 | 说明 |
|---|---|---|---|
| `server.listen` | `0.0.0.0:8787` | `PROXY_SERVER_LISTEN` | 监听地址；`0.0.0.0` 对局域网开放 |
| `server.tls.enabled` | `false` | `PROXY_SERVER_TLS_ENABLED` | 监听端口改用 HTTPS——供强制 `https://` base_url 的 agent 使用（见 [HTTPS 监听](#https-监听-tls)）。与 upstream 的协议无关 |
| `server.tls.cert_file` / `key_file` | — | `PROXY_SERVER_TLS_CERT_FILE` / `_KEY_FILE` | 自带 PEM 证书（Let's Encrypt、内部 CA、mkcert）；两者须成对设置。不设 = 自动生成自签名 CA |
| `server.tls.auto_dir` | `~/.config/synapse/tls` | `PROXY_SERVER_TLS_AUTO_DIR` | 生成的 CA 与证书的持久化目录 |
| `server.tls.sans` | — | `PROXY_SERVER_TLS_SANS`（逗号分隔） | 追加到生成证书 SAN 的域名/IP（默认已覆盖 localhost、主机名、所有网卡 IP） |
| `upstream.base_url` | `http://127.0.0.1:8000/v1` | `PROXY_UPSTREAM_BASE_URL` | completions 路径之前的部分 |
| `upstream.path` | `/chat/completions` | `PROXY_UPSTREAM_PATH` | 拼接在 `base_url` 后；legacy 后端设为 `/completions` |
| `upstream.responses_mode` | `convert` | `PROXY_UPSTREAM_RESPONSES_MODE` | `convert` 把 `POST /v1/responses` 翻译成 chat completions；`passthrough` 转发到 upstream 的**原生** `/responses` 端点（适用于已实现 Responses API 的 provider——保留 reasoning item、服务端工具、store 语义） |
| `limits.max_concurrency` | `200` | `PROXY_LIMITS_MAX_CONCURRENCY` | 最大在途请求数；超出排队（背压）。`0` = 不限制 |
| `limits.max_body_bytes` | `67108864` | `PROXY_LIMITS_MAX_BODY_BYTES` | 客户端请求体上限（字节） |
| `limits.max_sse_line_bytes` | `16777216` | — | 单条 SSE data 行上限（大 tool 参数） |
| `timeouts.connect` | `10s` | `PROXY_TIMEOUTS_CONNECT` | 到 upstream 的 TCP/TLS 连接超时 |
| `timeouts.request` | `30m` | `PROXY_TIMEOUTS_REQUEST` | 单请求总时长上限（长生成） |
| `timeouts.idle` | `5m` | `PROXY_TIMEOUTS_IDLE` | keep-alive 空闲连接超时 |
| `timeouts.stream_write` | `5m` | `PROXY_TIMEOUTS_STREAM_WRITE` | 写慢速流式客户端的最大停滞时间 |
| `timeouts.header_write` | `60s` | — | 等待客户端请求头超时 |
| `shutdown.timeout` | `30s` | `PROXY_SHUTDOWN_TIMEOUT` | SIGTERM 后在途请求的宽限期 |
| `log.format` | `json` | `PROXY_LOG_FORMAT` | `json` 或 `text` |
| `log.level` | `info` | `PROXY_LOG_LEVEL` | `debug`/`info`/`warn`/`error` |

不启动服务即可校验配置：

```bash
synapse check-config --config config.yaml
```

配置非法时以非零码退出，并精确指出字段与期望值。

## Docker

Multi-stage 构建，最终镜像为 **distroless/static**（无 shell、无包管理器、
含 CA 证书），以 `nonroot` 用户运行：

```bash
docker build -t synapse .

docker run --rm -p 8787:8787 \
  -v "$PWD/config.example.yaml:/etc/synapse/config.yaml:ro" \
  synapse
```

受限网络下用 Go 模块镜像构建：

```bash
docker build --build-arg GOPROXY=https://goproxy.cn,direct -t synapse .
```

镜像内置 `HEALTHCHECK`，由二进制自带的 `healthcheck` 子命令实现
（distroless 没有 curl）：

```dockerfile
HEALTHCHECK --interval=30s --timeout=5s --retries=3 \
    CMD ["/synapse", "healthcheck"]
```

### Docker Compose——upstream 在宿主机

```yaml
services:
  synapse:
    image: synapse:latest
    build: .
    ports: ["8787:8787"]
    environment:
      PROXY_UPSTREAM_BASE_URL: "http://host.docker.internal:8000/v1"
    restart: unless-stopped
    stop_grace_period: 35s   # 必须大于 shutdown.timeout，SIGTERM 才能完整 drain
```

### Docker Compose——全部容器化

参见 [`docker-compose.example.yml`](docker-compose.example.yml)；代理通过
compose 网络内的服务名访问 provider：

```yaml
PROXY_UPSTREAM_BASE_URL: "http://llm-provider:8000/v1"
```

upstream 地址速查：

| Upstream 位置 | `upstream.base_url` |
|---|---|
| 同宿主机，代理跑裸机 | `http://127.0.0.1:8000/v1` |
| 同宿主机，代理在 Docker（Linux） | `http://172.17.0.1:8000/v1`，或 `--add-host=host.docker.internal:host-gateway` |
| 同宿主机，代理在 Docker（macOS/Windows） | `http://host.docker.internal:8000/v1` |
| 同 compose 网络的另一个容器 | `http://llm-provider:8000/v1` |
| 远程机器 | `http://10.1.2.3:8000/v1` |

SIGTERM 时，Docker 的 `stop_grace_period`（保持大于
`shutdown.timeout`）让在途流 drain；readiness 立即变 503，drain 完成后
进程以 0 退出。

## systemd

npm 安装或无 root 权限？直接跳过本节——`synapse init && synapse
service install --now` 会生成**用户级** unit
（`~/.config/systemd/user/synapse.service`，内嵌解析后的二进制路径），
并启用 linger 实现开机自启；生命周期用 `synapse start|stop|restart|
status` 与 `journalctl --user -u synapse -f` 管理。下面的 root 流程
面向系统级安装：

```bash
sudo useradd --system --home /nonexistent --shell /usr/sbin/nologin synapse || true
sudo install -Dm755 synapse /usr/local/bin/synapse
sudo install -Dm644 config.example.yaml /etc/synapse/config.yaml
sudo cp deploy/systemd/synapse.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now synapse

journalctl -u synapse -f        # journald 中的结构化 JSON 日志
sudo systemctl restart synapse  # 先 drain 再重启
```

unit 以专用非 root 系统用户运行，故障自动重启，SIGTERM 有 35 秒 drain
时间。

## macOS（launchd）

macOS 原生的常驻机制是 launchd。npm 用户可跳过手工步骤：`synapse
init && synapse service install --now` 直接生成带解析路径的
`~/Library/LaunchAgents/com.synapse.proxy.plist`（日志在
`~/Library/Logs/synapse.log`）。Release 二进制用户可用仓库自带的
LaunchAgent 模板：
[`deploy/launchd/`](deploy/launchd/)。

**1. 交叉编译**（任意机器可编；纯 Go 无 CGO。Intel Mac 用
`DARWIN_ARCH=amd64`）：

```bash
make build-darwin                # → synapse-darwin-arm64
scp synapse-darwin-arm64 mac:/usr/local/bin/synapse
```

**2. 配置**安装到 `/usr/local/etc/synapse/config.yaml`
（格式与 Linux 相同）。

**3. 安装 agent**（用户级、登录自启、无需 root）：

```bash
mkdir -p ~/Library/LaunchAgents
cp deploy/launchd/com.synapse.proxy.plist ~/Library/LaunchAgents/
# 若安装路径不同，修改 plist 内的二进制/配置路径
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.synapse.proxy.plist
```

**4. 管理**——SIGTERM 触发与 systemd 相同的优雅 drain：

```bash
launchctl kickstart -k gui/$(id -u)/com.synapse.proxy   # 重启
launchctl bootout      gui/$(id -u)/com.synapse.proxy   # 停止并卸载
tail -f /tmp/synapse.log                                   # 日志
curl -s http://127.0.0.1:8787/health                              # 验证
```

与 Linux 的差异：日志写入 plist 的 `StandardOutPath` 文件而非
journald；`KeepAlive` 对应 `Restart=`；局域网访问需在 系统设置 →
网络 → 防火墙 放行入站。Docker 在 macOS 上同样可用，且
`host.docker.internal` 原生解析到 Mac 本机。

## 局域网部署

```
开发机 A（LLM + 代理）                     开发机 B（Codex）
  synapse :8787   <------ 局域网 ---->  base_url http://A:8787/v1
```

1. A 上设 `server.listen: "0.0.0.0:8787"`，启动代理。
2. 开放端口：

```bash
sudo firewall-cmd --add-port=8787/tcp --permanent && sudo firewall-cmd --reload   # firewalld
sudo ufw allow 8787/tcp                                                          # ufw
sudo iptables -A INPUT -p tcp --dport 8787 -j ACCEPT                             # iptables
```

3. B 上的客户端指向 `http://<A的局域网IP>:8787/v1`。

## HTTPS 监听 (TLS)

有些 agent 强制要求 provider base_url 必须是 `https://`。打开一个开关，
监听端口即改说 HTTPS——而代理访问 upstream 的方式完全不变，两个方向互相
独立：

```yaml
server:
  listen: "0.0.0.0:8787"
  tls:
    enabled: true
upstream:
  base_url: "http://127.0.0.1:8000/v1"   # 保持 http——这正是本功能的意义
```

未配置 `cert_file`/`key_file` 时，首次启动会**自动生成自签名 CA 与服务器
证书**（mkcert 模式，ECDSA P-256，CA 有效期 10 年，叶子证书 825 天、到期
自动续签），持久化在 `~/.config/synapse/tls/`。证书 SAN 覆盖 `localhost`、
主机名、所有网卡 IP，以及 `sans:` 追加的条目。客户端需要信任该 CA（一次
即可）：

```bash
# Debian/Ubuntu
sudo cp ~/.config/synapse/tls/ca.pem /usr/local/share/ca-certificates/synapse-ca.crt && sudo update-ca-certificates
# Fedora/Arch
sudo trust anchor --store ~/.config/synapse/tls/ca.pem
# macOS
sudo security add-trusted-cert -d -r trustRoot -k /Library/Keychains/System.keychain ~/.config/synapse/tls/ca.pem
```

agent 侧不动系统信任库的替代方案：

| 客户端运行时 | 信任方式 |
|---|---|
| curl | `--cacert ~/.config/synapse/tls/ca.pem` |
| Node（Claude Code 等） | `NODE_EXTRA_CA_CERTS=~/.config/synapse/tls/ca.pem` |
| Python | `REQUESTS_CA_BUNDLE=~/.config/synapse/tls/ca.pem`（或 `SSL_CERT_FILE`） |
| Go | `SSL_CERT_FILE=~/.config/synapse/tls/ca.pem` |

已有真证书（内部 CA、DNS 挑战的 Let's Encrypt、mkcert）则跳过生成：

```yaml
server:
  tls:
    enabled: true
    cert_file: /etc/synapse/cert.pem
    key_file: /etc/synapse/key.pem
```

说明：

- TLS 下自动协商 HTTP/2，流式行为完全一致。
- `synapse status` 与 `synapse healthcheck` 会按配置改走 `https` 探测
  （自探测不校验证书）。Docker 的 `HEALTHCHECK` 无需改动——挂载配置或设
  `PROXY_SERVER_TLS_ENABLED=true` 即可。
- Docker：自动生成的 CA 在容器文件系统里——把 `server.tls.auto_dir` 挂成
  volume，否则每次重启都会换一个客户端不认识的新 CA。挂载证书文件的部署
  不受影响。
- **TLS 只是传输加密，不是认证**——代理仍原样转发收到的 `Authorization`
  header。参见 [安全说明](#安全说明)。

## Codex 配置

Codex 认为自己连的是真正的 Responses API，适配器在内部完成翻译。你的
API Key 管理方式完全不变——Codex 发出的 `Authorization` header 原样转发
到 upstream。

```toml
model = "your-model"
model_provider = "synapse"

[model_providers.synapse]
name = "Custom Completions Provider"
base_url = "http://192.168.1.100:8787/v1"
wire_api = "responses"
```

Agent 强制 `https://`？打开 `server.tls.enabled: true`，base_url 换成
`https://…` 即可——见 [HTTPS 监听](#https-监听-tls)。

## curl 示例

非流式：

```bash
curl http://127.0.0.1:8787/v1/responses \
  -H "Authorization: Bearer $UPSTREAM_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"your-model","input":"Explain backpressure in one sentence."}'
```

流式：

```bash
curl -N http://127.0.0.1:8787/v1/responses \
  -H "Authorization: Bearer $UPSTREAM_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"your-model","input":"Count from 1 to 5.","stream":true}'
```

Tool-call 多轮：

```bash
# 第 1 轮：模型请求调用工具
curl http://127.0.0.1:8787/v1/responses -H "Authorization: Bearer $K" -d '{
  "model":"your-model",
  "input":"What is the weather in Tokyo?",
  "tools":[{"type":"function","name":"get_weather",
            "parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}]
}'
# → output 含 {"type":"function_call","call_id":"...","name":"get_weather","arguments":"{\"city\":\"Tokyo\"}"}

# 第 2 轮：把调用与工具结果回传
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
curl http://127.0.0.1:8787/health   # 200 {"status":"ok"}     存活
curl http://127.0.0.1:8787/ready    # 200 {"status":"ready"}  就绪（shutdown 期间 503）
curl http://127.0.0.1:8787/metrics  # Prometheus 文本格式
```

指标（全部低基数；绝不包含 header、凭据或 prompt 内容）：

| 指标 | 类型 | 含义 |
|---|---|---|
| `protocol_proxy_requests_total` | counter | 收到的请求 |
| `protocol_proxy_responses_total` | counter | 发出的响应 |
| `protocol_proxy_errors_total{type}` | counter | 代理侧错误 |
| `protocol_proxy_upstream_errors_total{class}` | counter | upstream 失败 |
| `protocol_proxy_streaming_requests_total` | counter | 开始的 SSE 流 |
| `protocol_proxy_passthrough_requests_total` | counter | 透明转发的请求（无转换） |
| `protocol_proxy_tool_calls_total` | counter | 输出中观察到的 tool call |
| `protocol_proxy_bytes_in_total` / `_bytes_out_total` | counter | 出入 body 字节 |
| `protocol_proxy_upstream_requests_total` | counter | 发起的 upstream 调用 |
| `protocol_proxy_active_connections` / `_active_requests` | gauge | 活跃连接 / 在途请求 |
| `protocol_proxy_request_duration_seconds` | histogram | 端到端延迟 |
| `protocol_proxy_upstream_duration_seconds` | histogram | 到 upstream 响应头的时间 |
| `protocol_proxy_first_byte_latency_seconds` | histogram | 请求开始 → 首个 SSE 事件 flush |

日志为结构化 JSON（slog）：`timestamp`、`level`、`request_id`、
`method`、`path`、`status`、`bytes_out`、`latency_ms`。代理**从不**记录
`Authorization` header、API Key、prompt 或 body——代码里根本不存在这样
的路径。

## 测试

```bash
make test    # 单元 + e2e 测试
make race    # 同上，开启 race detector
```

测试覆盖：转换器表驱动用例、逐字节喂入的 SSE 拆分解析、UTF-8 拆分
边界、并行 tool call 的流式事件序列、Authorization 透传、客户端断开的
取消传导、upstream 错误转发、shutdown 时 readiness 翻转、TLS 端到端
（用生成的 CA 验证握手：受信客户端正常服务、不受信客户端在握手阶段被
拒）、10/50/100/200 并发。

## 故障排查

| 现象 | 可能原因 / 处理 |
|---|---|
| `400 previous_response_id is not supported` | 客户端依赖服务端会话状态；把完整历史放进 `input`（Codex 默认如此）。 |
| `502 ... connection refused` | provider 不可达；检查 `upstream.base_url`。Docker 访问宿主机用 `host.docker.internal`（Linux 需 `--add-host`）。 |
| SSE 不流式 / 被缓冲 | 前置反向代理缓冲了（nginx）；设 `proxy_buffering off;`（适配器已发送 `X-Accel-Buffering: no`）。 |
| `400 unsupported input item type` | 客户端发送了 chat-completions 无对应的 item 类型（如 `computer_call`）。 |
| 流恰好在 5 分钟处被切断 | 慢客户端触发 `timeouts.stream_write`；合理场景可调大。 |
| 客户端解析不到 tool call | provider 必须以 `delta.tool_calls[].index` 形式输出（标准 OpenAI 形状）；检查 provider 是否启用 tools。 |
| `curl: (60) SSL certificate problem` | 客户端不信任自动生成的 CA——导入 `~/.config/synapse/tls/ca.pem`（见 [HTTPS 监听](#https-监听-tls)），或用 `--cacert`/`NODE_EXTRA_CA_CERTS`。 |
| agent 报 https 证书错误 | 同上；走系统信任库的 agent 必须导入 CA，环境变量方式只对支持它的运行时有效。 |

## 安全说明

> **不要把本适配器直接暴露到公网。**

它**不做任何认证**，客户端发什么 `Authorization` 就转发什么——这是设计
使然。任何能访问端口的人都能消耗你 upstream 的配额。预期部署位置：

```
Internet  ✗  （绝不直连）
可信局域网 / tailnet / VPN  →  synapse-protocol-adapter  →  LLM provider
```

如果必须暴露到可信网络之外，请在前面加认证层（mTLS、OAuth2-proxy、
Cloudflare Access、Tailscale Funnel……）或自行添加认证 middleware——
middleware 层是独立隔离的，加一个文件即可。

## 许可证

[MIT](LICENSE) © 2026 dncore
