# @dncore/synapse (npm distribution)

The synapse protocol adapter distributed as prebuilt native binaries —
the Node.js runtime is only a launcher; the daemon itself is the original
Go binary (esbuild-style distribution).

```bash
npm install -g @dncore/synapse
synapse --config config.yaml
```

- Requires npm >= 7 (optionalDependencies install the matching platform
  package automatically).
- `SYNAPSE_BINARY=/path/to/binary` overrides resolution (e.g. when
  installed outside npm).
- Signals (Ctrl-C, SIGTERM) are forwarded to the daemon, which drains
  gracefully.

Platform packages: `@dncore/synapse-linux-x64`, `-linux-arm64`,
`-darwin-x64`, `-darwin-arm64`.

See the [repo README](https://github.com/dncore/synapse-protocol-adapter)
for configuration, deployment, and protocol semantics.
