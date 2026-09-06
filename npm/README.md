# @dncore/synapse (npm distribution)

The synapse protocol adapter distributed as prebuilt native binaries —
Node.js is only the launcher; the daemon is the original Go binary
(esbuild-style distribution, zero JS dependencies).

## Quick start

```bash
npm install -g @dncore/synapse   # npm >= 7 (platform package is an optionalDependency)
synapse init                     # writes the annotated ~/.config/synapse/config.yaml
$EDITOR ~/.config/synapse/config.yaml   # set upstream.base_url at minimum
synapse service install --now    # autostart service, started immediately
synapse status
```

Point any Responses-API client (e.g. Codex) at `http://127.0.0.1:8787/v1`.

## Commands

| Command | What it does |
|---|---|
| `synapse init [--config P]` | Write the self-documenting default config (refuses to overwrite) |
| `synapse start / stop / restart` | Control the installed service (graceful drain on stop) |
| `synapse status [--config P]` | One screen: service state, unit/config paths, listen, upstream, live `/health` `/ready` `/version` probes |
| `synapse service install [--config P] [--now]` | Generate the unit file with the **resolved binary path** (works from any npm prefix), enable boot autostart |
| `synapse service uninstall` | Stop, disable autostart, remove the unit — **keeps** your config |
| `synapse check-config [--config P]` | Validate config and print the effective values |
| `synapse healthcheck [--url U]` | One-shot health probe (used by the Docker image) |
| `synapse [--config P]` | Run in the foreground (Ctrl-C drains gracefully) |
| `synapse --version` / `--help` | Version / full command surface |

## Where things live

| Platform | Service | Unit file | Config | Logs |
|---|---|---|---|---|
| Linux | systemd **user** unit | `~/.config/systemd/user/synapse.service` | `~/.config/synapse/config.yaml` | `journalctl --user -u synapse -f` |
| macOS | launchd agent | `~/Library/LaunchAgents/com.synapse.proxy.plist` | `~/.config/synapse/config.yaml` | `~/Library/Logs/synapse.log` |

On Linux, `service install` also runs `loginctl enable-linger` so the
daemon starts at boot without anyone logged in.

## Configuration

`synapse init` writes a fully commented config; the one setting you must
set is `upstream.base_url` (your OpenAI-compatible chat-completions
provider). Every value can instead be overridden with `PROXY_*`
environment variables (`PROXY_UPSTREAM_BASE_URL`,
`PROXY_SERVER_LISTEN`, … — the full list is in the generated file).
The proxy stores no credentials: client `Authorization` headers are
forwarded verbatim.

After editing the config, apply it with `synapse restart` (drains
in-flight streams first).

## Troubleshooting

- **`start failed … service not loaded`** — the service is not
  installed: `synapse service install --now` first, or run bare with
  `synapse --config <path>`.
- **Port already in use** — another process owns `server.listen`
  (default `0.0.0.0:8787`); change it in the config and
  `synapse restart`. `synapse status` shows what the service actually
  listens on.
- **Upstream unreachable through a system proxy** — set `NO_PROXY`
  (or unset `http_proxy`/`https_proxy`) in the unit's environment if
  your upstream is local.
- **Logs** — Linux: `journalctl --user -u synapse -f` (structured
  JSON); macOS: `tail -f ~/Library/Logs/synapse.log`.
- **Platform package missing** — npm skipped optional dependencies
  (`--no-optional` / `--omit=optional`); reinstall without those flags,
  or point `SYNAPSE_BINARY` at a binary you installed yourself.

See the [repo README](https://github.com/dncore/synapse-protocol-adapter)
for protocol semantics, Docker deployment, performance numbers, and the
security model.
