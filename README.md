# codex-bridge

Use external models alongside your ChatGPT subscription in Codex CLI.
Requires Codex CLI; tested with **0.164.0**.

## Install

Linux. Run as root, or prefix `install` with `sudo`.

**x86_64 / amd64**

```sh
curl -fL https://github.com/agent0ai/codex-bridge/releases/latest/download/codex-bridge-linux-amd64 -o codex-bridge-linux-amd64 &&
install -m 755 codex-bridge-linux-amd64 /usr/local/bin/codex-bridge
```

**ARM64 / aarch64**

```sh
curl -fL https://github.com/agent0ai/codex-bridge/releases/latest/download/codex-bridge-linux-arm64 -o codex-bridge-linux-arm64 &&
install -m 755 codex-bridge-linux-arm64 /usr/local/bin/codex-bridge
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
