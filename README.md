# Oriel

The Go backend of OrielHub, a desktop app that hands tasks to the coding agents
installed on your machine (Claude Code, Codex, pi, Grok, Gemini CLI, OpenCode,
Cursor, Copilot, Qwen Code and any agent from the ACP Registry), lets several
agents work on one task, and lets you follow and steer them from a phone.

This repository holds the backend only: the task model, the hub that runs
tasks, the agent adapters, storage, the desktop IPC and remote gateways, and
the relay server for remote control. The desktop interface is not part of it,
so building the root package gives you an app without its window content.

## Requirements

- Go 1.27.4 (see `go.mod`).
- macOS is fully supported, Linux is best effort, Windows comes later.
- No cgo: SQLite is loaded through MyGo's SQLite plugin.

## Build and test

```bash
go build ./...
go vet ./...
go tool staticcheck ./...
CGO_ENABLED=0 go test ./...
```

Race tests run on the packages with concurrency:

```bash
CGO_ENABLED=0 go test -race ./internal/proc ./internal/agent ./internal/hub ./internal/updater ./internal/usage ./internal/transport/mygoipc ./internal/runtime/acp ./internal/provider ./internal/relay/... ./internal/transport/remote
```

Tests never touch the agents installed on your machine: discovery runs against
fake executables in temporary directories.

## Relay server

`cmd/orielhub-relay` is the stateless relay that connects the Mac app and the
phone for remote control. Both ends talk end-to-end encrypted Noise over it, so
the relay sees only ciphertext. It speaks plain HTTP; put a TLS reverse proxy
in front of it.

```bash
go run ./cmd/orielhub-relay -addr :8080
```

`/healthz` answers `204` while it runs.

The relay limits the connections it holds; a refused client gets HTTP 503 or
429 and the apps retry with backoff.

| Flag | Default | Limit |
|---|---|---|
| `-max-conns` | 1024 | Connections at a time, including ones that have not said hello yet. Each can queue about 4 MiB of frames, so size it to the machine's memory. |
| `-max-conns-per-ip` | 32 | Connections from one client address; an IPv6 /64 counts as one address. |
| `-max-apps` | 16 | Phones connected to one Mac at a time. |
| `-client-ip-header` | none | Header in which your reverse proxy writes the client address, such as `X-Forwarded-For`; the last value counts. Set it behind a proxy, or every connection counts as the proxy's address. |

## Layout

| Package | Responsibility |
|---|---|
| `internal/task` | Pure domain core: the Task aggregate, commands, events, state machine and planned effects. No I/O. |
| `internal/hub` | Runs each live task as an actor, persists events with their projections, and declares the ports the other packages implement. |
| `internal/agent` | Agent catalog, discovery and profiles. |
| `internal/runtime/*` | One adapter per agent protocol: Claude, Codex and pi natively, everything else over ACP. |
| `internal/proc` | Starting external programs: login-shell environment, process groups. |
| `internal/store` | SQLite implementation of the hub's log and config. |
| `internal/workspace` | Git worktrees for task isolation and merging. |
| `internal/usage` | Token usage and quota snapshots read from the agents' local records. |
| `internal/provider` | AI providers' public status pages. |
| `internal/updater` | Background update checks and installs. |
| `internal/relay` | Relay protocol and Noise framing; `relay/server` is the relay itself. |
| `internal/transport` | Inbound adapters: `mygoipc` for the desktop window, `remote` for the phone. |
| `internal/app` | Composition root that wires everything together. |

Design documents are in [`docs/architecture`](docs/architecture/README.md),
starting with its README; behaviour is specified as scenarios in
[`docs/acceptance`](docs/acceptance). Both are written in Chinese.

## Contributing

Pull requests are welcome. This repository is published from the full
OrielHub source, so a merged change is applied there with you as co-author and
comes back here in the next sync, and the pull request is then closed with a
link to that commit. Read [AGENTS.md](AGENTS.md) for the project's rules
before changing behaviour: new behaviour starts from scenarios, and tests use
real implementations, never mocks.

## License

MIT, see [LICENSE](LICENSE).
