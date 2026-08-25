# Cortex

Cortex is a self-hostable coding-agent workspace with a small Go backend and browser UI. It can run locally on a development machine or be hosted remotely, while OpenCode provides the underlying coding-agent execution loop.

## First version

- OpenCode-backed coding agent with streamed tool activity and response recovery.
- DeepSeek V4 Flash through OpenCode Zen for the first dogfood path.
- Workspace file browser and text preview alongside the agent.
- Settings popup with provider selector and masked API-key storage.
- Copy session, Stop, Enter-to-run / Shift+Enter newline, tool status highlighting.
- Workspace-root confinement and isolated per-run OpenCode config/data directories.

## Build and run

```sh
go test ./...
go build -o cortex ./cmd/cortex
./cortex --root ~/Repositories
```

Cortex listens on `127.0.0.1:7331` by default.

> Remote binding is intentionally not the default. This first version does not yet provide browser authentication, so do not expose it directly to an untrusted network. Put it behind an authenticated private network/reverse proxy until Cortex gains its own remote-auth mode.

Open **Settings**, choose **OpenCode Zen**, and save your API key. The first agent integration expects `opencode` to be installed and available in Cortex's `PATH`.
