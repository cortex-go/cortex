# Cortex

Cortex is a self-hosted, agent-first coding agent with a small Go backend and a Nift-built browser UI. It can run locally on a development machine or be hosted remotely, while OpenCode provides the underlying coding-agent execution loop.

## First version

- OpenCode-backed coding agent with streamed tool activity and response recovery.
- OpenCode-backed provider execution for OpenCode Zen, OpenRouter, OpenAI, Anthropic, Google AI and DeepSeek, with editable model IDs.
- Browser-selected workspaces under a configurable root.
- Multiple independent browser sessions across different workspaces.
- Settings popup with provider/model selection, masked API-key storage, and OpenCode OAuth reuse for ChatGPT Plus/Pro and GitHub Copilot.
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

Cortex dogfoods [Nift](https://nift.dev) for its application frontend. `content/` and `templates/` are the source of truth; `public/` is generated and embedded into the Go binary. Build the frontend before compiling Go after changing frontend source:

```sh
nift build
go test ./...
go build -o cortex ./cmd/cortex
./cortex
```

`nift status` should report the three tracked frontend outputs as current before committing. Do not edit `public/` directly.

Cortex listens on `127.0.0.1:7331` and uses your home directory as the default workspace root. Use `--root ~/Repositories` when you want a tighter browser-visible boundary.

> Remote binding is intentionally not the default. Cortex now requires browser authentication after first-run password setup, with optional TOTP and Google sign-in. For an Internet-facing deployment, TLS and normal host/reverse-proxy hardening are still recommended.

Open **Settings**, choose a provider and model, then save the provider credential. API-key providers are injected only into the isolated OpenCode process.

For ChatGPT Plus/Pro or GitHub Copilot subscriptions, authenticate OpenCode once on the Cortex host:

```sh
opencode auth login --provider openai
opencode auth login --provider github-copilot
```

Cortex detects those OAuth credentials and copies the selected provider credential into the isolated session data when that subscription is used. Exact model availability remains determined by the installed OpenCode version and provider account.
