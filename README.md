# codex-bridge

Use external models alongside your ChatGPT subscription in Codex CLI.
Requires Codex CLI; tested with **0.164.0**.

## Install

Install in `~/.local/bin` for your user. No `sudo` needed.

### Linux

**x86_64 / amd64**

```sh
curl -fL https://github.com/agent0ai/codex-bridge/releases/latest/download/codex-bridge-linux-amd64 -o codex-bridge-linux-amd64 &&
mkdir -p "$HOME/.local/bin" &&
install -m 755 codex-bridge-linux-amd64 "$HOME/.local/bin/codex-bridge"
```

**ARM64 / aarch64**

```sh
curl -fL https://github.com/agent0ai/codex-bridge/releases/latest/download/codex-bridge-linux-arm64 -o codex-bridge-linux-arm64 &&
mkdir -p "$HOME/.local/bin" &&
install -m 755 codex-bridge-linux-arm64 "$HOME/.local/bin/codex-bridge"
```

### macOS (Apple Silicon and Intel)

Mac release assets are prepared but not yet published. Build from source with Git and [Go 1.24.4 or newer](https://go.dev/dl/), using native Go for your Mac: `arm64` on Apple Silicon, `amd64` on Intel (`go env GOARCH`):

```sh
git clone https://github.com/agent0ai/codex-bridge.git
cd codex-bridge
go build -trimpath -ldflags="-s -w -X main.version=$(git describe --tags --always --dirty)" -o codex-bridge . &&
mkdir -p "$HOME/.local/bin" &&
install -m 755 codex-bridge "$HOME/.local/bin/codex-bridge"
```

The default Mac config is `~/Library/Application Support/codex-bridge/config.yaml`; `codex-bridge edit` opens it without needing to enter that path.

### PATH

If `~/.local/bin` is not already on PATH, add it for this shell and put the same line in `~/.zshrc` (macOS's default Zsh) or `~/.bashrc` (Bash) for future terminals:

```sh
export PATH="$HOME/.local/bin:$PATH"
```

## Quick start

```sh
# Edit the models config and replace the example API key
codex-bridge edit

# Start the proxy service
codex-bridge start

# Sync models (--force restarts the Codex server)
codex-bridge sync --force

# Start Codex as usual and choose a model with /model
codex
```

[Config](config.example.yaml) · [Guide](docs/guide.md) · [MIT License](LICENSE) · [Contributing](CONTRIBUTING.md) · [Third-party notices](THIRD_PARTY_NOTICES.md)

---

<p align="center">
  Made by <a href="https://agent-zero.ai">Agent Zero</a>
</p>
<p align="center">
  <a href="https://agent-zero.ai">Website</a> ·
  <a href="https://github.com/agent0ai">GitHub</a> ·
  <a href="https://discord.gg/B8KZKNsPpj">Discord</a> ·
  <a href="https://x.com/Agent0ai">X</a> ·
  <a href="https://www.youtube.com/@AgentZeroFW">YouTube</a> ·
  <a href="https://github.com/sponsors/agent0ai">Sponsor</a>
</p>
