# Cortex

Cortex is a self-hostable, agent-first coding workspace with a small Go backend and browser UI. It can run locally on a development machine or be hosted remotely, while OpenCode provides the underlying coding-agent execution loop.

## First version

- OpenCode-backed coding agent with streamed tool activity and response recovery.
- DeepSeek V4 Flash through OpenCode Zen for the first dogfood path.
- Browser-selected workspaces under a configurable root.
- Multiple independent browser sessions across different workspaces.
- Settings popup with provider selector and masked API-key storage.
- Copy session, Stop, Enter-to-run / Shift+Enter newline, tool status highlighting.
- Workspace-root confinement and isolated per-session OpenCode data.

## Install

GitHub Releases publish prebuilt Linux, macOS and Windows binaries. On Linux or macOS, the website installer can install the latest release to `~/.local/bin`:

```sh
curl -fsSL https://cortex-go.github.io/install.sh | sh
```

Go users can also install directly from the public module:

```sh
go install github.com/cortex-go/cortex/cmd/cortex@latest
```

Cortex currently expects `opencode` to be installed separately and available in its `PATH`.

## Build and run

```sh
go test ./...
go build -o cortex ./cmd/cortex
./cortex
```

Cortex listens on `127.0.0.1:7331` and uses your home directory as the default workspace root. Use `--root ~/Repositories` when you want a tighter browser-visible boundary.

> Remote binding is intentionally not the default. This first version does not yet provide browser authentication, so do not expose it directly to an untrusted network. Put it behind an authenticated private network/reverse proxy until Cortex gains its own remote-auth mode.

Open **Settings**, choose **OpenCode Zen**, and save your API key. The current agent execution path is OpenCode Zen only; the other provider entries reserve credential slots for future provider/model support and do not yet drive agent runs.
