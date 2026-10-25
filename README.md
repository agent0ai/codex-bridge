# codex-bridge

A small Go proxy that puts OpenAI-compatible Chat Completions and Responses models in Codex CLI's `/model` picker, alongside your ChatGPT subscription. Switch models within a conversation; no profiles or patched Codex required.

Proof of concept tested with **Codex CLI 0.164.0** on Debian Linux. YAML and TOML parsers plus a WebSocket client for checking Codex's loaded models; no database or JavaScript runtime for the proxy. The running proxy used about 15 MiB RSS during the initial live tests. Linux and macOS process APIs are used; Windows support is not implemented.

## Ready-to-use test container

The running container is `codex-bridge-lab`. It has Codex, the bridge, three external models, and your subscription login. The disposable provider key is stored only in its private bridge config, not in this repository or image.

```bash
docker exec -it codex-bridge-lab bash
codex-bridge status
cd /work
codex --no-daemon
```

Inside Codex, enter `/model` and select `a0-qwen`, `a0-deepseek`, `a0-vision`, or a subscription model such as `GPT-6-Luna`. External models appear first, in config order. To start with a specific external model, use `codex --no-daemon -m a0-qwen`.

```bash
codex-bridge edit       # $VISUAL, then $EDITOR, then automatic editor detection
codex-bridge sync       # validate config and write the model catalog
codex --no-daemon       # load the current config without using the shared server
# Or, to use normal codex (this may interrupt active shared-server work):
codex-bridge sync --force
codex
codex-bridge stop
codex-bridge start
```

Routing and body parameter changes are loaded on the next request. Restart the proxy after changing its listen address or the environment variables it inherits. `stop` and `status` still work if you make a config syntax error or change the configured port.

## Install on your own machine

Install Codex CLI 0.164.0 and Go 1.24.4 or newer, then:

```bash
go build -trimpath -ldflags='-s -w' -o codex-bridge .
mkdir -p ~/.local/bin
install -m 755 codex-bridge ~/.local/bin/codex-bridge
codex-bridge init
codex-bridge edit
```

Put `~/.local/bin` on your `PATH`. The default config is `$XDG_CONFIG_HOME/codex-bridge/config.yaml` on Linux (normally `~/.config/codex-bridge/config.yaml`). The proxy also supports an explicit file: `codex-bridge --config /path/models.yaml start`. Legacy `.toml` files remain readable when passed explicitly with `--config`. Codex's own `~/.codex/config.toml` still uses TOML.

`edit` honors `$VISUAL`, then `$EDITOR`. With neither set, it searches `PATH` in this order: `micro`, `nano`, `mcedit`, `joe`, `nvim`, `vim`, the system `editor` command, then `vi`. The first available executable is used. An explicit editor setting that fails produces an error rather than silently switching editors. If no editor is found or the selected editor fails, the error includes the full config path so you can edit it manually.

For the supplied example, set your provider key in the shell **before starting the proxy**:

```bash
read -rs -p 'Provider API key: ' AGENT_ZERO_API_KEY; echo
export AGENT_ZERO_API_KEY
codex-bridge start
codex-bridge sync
codex login             # only if you have not already logged in
codex --no-daemon
```

The bridge uses Codex's existing subscription authentication and refresh logic. It never modifies `auth.json` or logs credentials. A subscription login is required for the normal mixed subscription/external setup. For external-only use without signing in, launch `codex -c model_providers.codex-bridge.requires_openai_auth=false -m a0-qwen`.

## Config

See [config.example.yaml](config.example.yaml) for the three tested models. A model entry looks like:

```yaml
listen: 127.0.0.1:8787
models:
  - name: my-model                       # unique name in Codex's picker
    model: actual-provider-model-id      # defaults to name
    base_url: https://provider.example/v1
    api: chat                            # chat or responses
    api_key_env: MY_PROVIDER_KEY          # or api_key: "literal key"
    context_window: 131072                # defaults to 32768
    vision: false                        # true for image-capable models
    reasoning_efforts: [low, high]        # omit if unsupported
    drop_params: [parallel_tool_calls]   # omit parameters the provider rejects
    params:
      temperature: 0.2
      max_tokens: 8192
      vendor_options:
        thinking: true
```

`params` accepts JSON-compatible YAML values and recursively merges them into the upstream JSON body after translation. Unknown config fields, duplicate keys, multiple YAML documents, and parameters with non-string map keys are rejected. `drop_params` removes top-level fields first. Routing and protocol fields (`model`, `input`, `messages`, `tools`, `stream`) are controlled by the bridge. Credentials use a Bearer token; omit both key settings for an unauthenticated local API. URLs may include an arbitrary path and query parameters, and may point to the API base or complete endpoint.

Changing `context_window` does not enlarge the provider's real context limit. Set it to a value the model actually supports. An API-compatible model still needs good tool-call support to perform coding tasks reliably.

## Commands and configuration safety

| Command | Effect |
| --- | --- |
| `init` | Create the example config with private permissions; never overwrite an existing file. |
| `edit` | Open the config with your terminal editor. |
| `sync` | Validate config, generate a version-matched model catalog, and select the local bridge provider. |
| `sync --force` | Sync, then immediately invoke `codex app-server daemon restart`; active work may be interrupted. |
| `start` | Start one background proxy; repeated starts reuse the running instance. |
| `stop` | Stop the listener and allow active requests up to ten seconds to finish. |
| `status` | Show proxy health, configured model count, and whether Codex's provider/catalog match the bridge. |
| `serve` | Run in the foreground; stop with Ctrl-C. |
| `unsync` | Restore the prior provider/catalog settings while preserving unrelated config changes. |

`sync` writes `$CODEX_HOME/config.toml` and `codex-bridge-models.json` (normally under `~/.codex`). It preserves unrelated settings and comments, creates `config.toml.bridge-backup`, and is idempotent. It refuses to overwrite an existing unmanaged `codex-bridge` provider or a separate custom model catalog. Profiles are never created. Model aliases must not collide with built-in model names.

Codex 0.164.0 can reconnect new terminals to an existing shared background server. That server keeps its startup model catalog, so **closing and reopening the terminal does not refresh it**. Use `codex --no-daemon` to load the synced files immediately, or run `codex-bridge sync --force` and reopen Codex. The force option restarts the shared server through Codex's own daemon command, which may interrupt active work and takes time to shut down gracefully. Normal `sync` never restarts it. If restarting fails, the command exits with an error and clearly reports that the files were still synced.

`status` verifies the provider, local token, listen address, and model catalog files, then connects to the **running** Codex server's Unix socket and queries its [`model/list` API](https://learn.chatgpt.com/docs/app-server#list-models-modellist). It checks external model names, order, visibility, descriptions, reasoning options, and input modalities, including removed models still loaded by the server. It reports one result: `synced`, `unsynced`, or `could not verify`. A stopped/unreachable server, failed query, or malformed response produces `could not verify`; a confirmed difference produces `unsynced`. Reasons appear only when verification fails or finds a mismatch.

The query is read-only: it does not start/restart Codex or send inference requests. It follows pagination and has a ten-second deadline for the server check. Changing API keys or upstream parameters does not require sync; changing model metadata does. The API exposes picker metadata, not all internal settings such as retained context limits or provider credentials. Separate profiles, command-line overrides, and upstream model access are outside this check.

```text
Running at http://127.0.0.1:8787
Config: /root/.config/codex-bridge/config.yaml
Models: 1 external
Codex sync: synced
Codex config: /root/.codex/config.toml
```

All selected models go through the proxy while synced, so keep it running. To return to your original setup:

```bash
codex-bridge unsync
codex-bridge stop
# Use codex --no-daemon, or restart the shared server and reopen Codex.
# If you last selected an external alias, choose a built-in model.
```

The proxy binds only to a loopback IP and requires a random local token written by `sync`. Browser-origin requests are rejected. Third-party requests are built with only that model's credentials; subscription headers are sent only to fixed OpenAI destinations. HTTP redirects are not followed for third-party requests. Logs contain model, status, and timing rather than prompts or keys. The log is next to the config as `config.yaml.log`.

## Translation and limits

Supported: streamed text, multiple tool calls, tool results and multi-turn history, namespaced tools, custom/freeform tools (including `apply_patch`), reasoning summaries and replay, image URLs/data URLs, inline file data, structured-output schemas, usage counts, token-limit endings, upstream errors, cancellation, and native Responses forwarding. Summary-only reasoning is converted to text when crossing into Responses so OpenAI does not attempt to retrieve invented server-side item IDs.

This is a Codex-focused adapter, not a complete implementation of every Responses API feature:

- Chat Completions has no hosted OpenAI web search. The bridge removes that tool and tells the model it is unavailable. Use an MCP search tool or provider-specific `params`. Subscription web search remains intact.
- Audio, video, uploaded file IDs/URLs, computer-use tools, background jobs, and `previous_response_id` storage are not emulated for Chat Completions. Unsupported inputs fail explicitly. Codex's HTTP mode sends complete history and works without a response database.
- Encrypted OpenAI reasoning cannot be converted to another provider's private reasoning format. Available summaries and visible conversation/tool history are preserved.
- Tool arguments are buffered until complete and validated before execution. Truncated or malformed calls produce an error rather than executing a partial command.
- Native subscription capabilities, eligibility, and rate limits still apply. The bundled model catalog includes models your account might not have. GPT-6.1-Sol reported capacity errors during the live tests; GPT-6-Luna completed the mixed-provider test.
- Switching models does not raise the destination model's context limit. Large or unusual provider-specific conversations need further testing. Do not assume every provider-specific extension is interchangeable.

## Images and files

Use an image-capable model with `vision = true`:

```bash
codex -m a0-vision --image screenshot.png -- 'Describe this screenshot.'
codex -m a0-qwen 'Read @notes.txt and summarize it.'
```

Live checks passed for a single PNG, two PNGs in order, `view_image` tool output, a local text-file reference read through Codex tools, and retaining an image when switching external → subscription → external. Image/file media returned by tools is placed after all parallel tool results, because Chat tool messages accept only text. Inline Responses `input_file.file_data` maps to Chat's file content part; the destination must support that file type. This mapping has fixture coverage; PDF interpretation was not tested live.

## Prompt caching

The bridge preserves `prompt_cache_key`, `prompt_cache_retention`, `prompt_cache_options`, and content-part cache controls. It generates stable tool aliases and deterministic JSON, without inserting a new timestamp or random value into the upstream prompt on every request. Regression tests check that identical input produces identical output and adding a turn preserves the earlier message prefix.

The live test captured each upstream `usage` object during four consecutive turns in one Codex thread per model:

| Model | Turn | Input tokens | Cached input | Hit ratio |
| --- | ---: | ---: | ---: | ---: |
| Qwen Coder | 1 | 74,666 | 0 | 0% |
| Qwen Coder | 2 | 74,697 | 74,640 | 99.92% |
| Qwen Coder | 3 | 74,728 | 74,672 | 99.93% |
| Qwen Coder | 4 | 74,759 | 74,704 | 99.93% |
| DeepSeek V3.2 | 1 | 68,518 | 0 | 0% |
| DeepSeek V3.2 | 2 | 68,544 | 68,416 | 99.81% |
| DeepSeek V3.2 | 3 | 68,570 | 68,416 | 99.78% |
| DeepSeek V3.2 | 4 | 68,596 | 68,480 | 99.83% |

Hit ratio is `cached_input_tokens / input_tokens`. Follow-up turns had weighted hit ratios of **99.93% for Qwen** and **99.81% for DeepSeek**; a final Qwen rerun measured **99.92%**. Every earlier message, tool definition, and cache key stayed unchanged across those requests. The script uses a private proxy and an in-memory upstream tap; reports contain usage counters, not prompts or credentials. These were short answers to a fixed reference prompt, without tool execution in the cache measurement itself; tool execution is covered separately by the smoke tests.

Standard `prompt_tokens_details.cached_tokens` maps to Responses `input_tokens_details.cached_tokens`; DeepSeek's `prompt_cache_hit_tokens` is a fallback. Input/output detail fields are retained. Codex CLI reports cumulative thread usage after `exec resume`, so the test subtracts the previous total and checks that input, cached input, and output counts exactly match the upstream turn. See [cache-verification.json](cache-verification.json) for the original usage objects and verified deltas.

The supplied gateway replaces `prompt_cache_key` with its own stable account key. Preserving Codex's key fixes a portability issue for other providers, even though that gateway already supplied one. A later YAML-config verification run measured 36.28% weighted follow-up reuse, with an unchanged prefix and exact usage accounting; its raw usage is also in the report. Cache availability, retention, routing, model changes, compaction, and real prompt changes still affect reuse; these measurements do not guarantee a particular hit ratio for other workloads.

## Verification and reproducible Docker image

```bash
go test -race ./...
go vet ./...
# Real terminal picker regression; isolated daemon, dummy key, no model requests:
python3 tests/picker.py
docker build -t codex-bridge:proof .
docker run -d --name my-codex-bridge --init codex-bridge:proof
docker exec -it my-codex-bridge bash
# Run init/edit/start/sync and codex login inside this new container.
```

No ports need publishing: Codex and the proxy share the container's loopback interface. This image uses Debian slim, a static Go bridge, the native Codex binary, Node for Codex's JavaScript tool, and basic coding tools. The image contains no credentials. Keep or back up the container's home directory before removing it.

The live smoke test makes real provider requests and creates a temporary marker file using Codex tools. The cache script uses PyYAML, included in the image (`apt install python3-yaml` when running it separately on Debian):

```bash
docker exec codex-bridge-lab python3 /opt/bridge-smoke.py
# Or without a subscription login:
docker exec codex-bridge-lab python3 /opt/bridge-smoke.py --external-only
# Image/file checks, including subscription model switching:
docker exec codex-bridge-lab python3 /opt/bridge-attachments.py
# Four-turn cache checks with raw usage reports:
docker exec codex-bridge-lab python3 /opt/bridge-cache.py --model a0-qwen --report /tmp/cache-qwen.json
docker exec codex-bridge-lab python3 /opt/bridge-cache.py --model a0-deepseek --report /tmp/cache-deepseek.json
```

The picker regression requires a complete packaged Codex 0.164.0 installation for daemon management; pass `--codex /path/to/package/bin/codex` if your default CLI is a standalone binary. It reproduces a stale catalog after normal sync, checks `--no-daemon`, then checks that `sync --force` makes the external model appear first in a normal terminal session. It also checks live status before/after restart, stale image support for an existing alias, and the stopped-server result. It uses a temporary `CODEX_HOME` and leaves existing servers alone.

Unit tests cover translation, fragmented parallel tool calls, custom tools, reasoning, attachments, cache controls and accounting, stable prompt prefixes, JSON schemas, truncated streams, credential separation, native Responses forwarding, URL handling, validation, sync/unsync preservation, force restart, and restart failures. Live tests cover Qwen file edits and Python tests, DeepSeek reasoning and shell tools, image and file attachments, cache reuse, the interactive picker, and switching external/subscription models within one thread. See [verification.txt](verification.txt) for the recorded results.

Implementation references: [Codex configuration](https://learn.chatgpt.com/docs/config-file/config-reference), [Codex v0.164.0 source](https://github.com/openai/codex/tree/rust-v0.164.0), and [MetaFARS/codex-relay](https://github.com/MetaFARS/codex-relay) (translation edge cases). The supplied gateway's `/v1/models` returned `400 Missing model ID`; tested IDs were obtained from [Venice's model catalog](https://docs.venice.ai/models/overview).
