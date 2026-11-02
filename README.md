<div align="center">

<img src="assets/logo.png" alt="rup.ai logo" width="96" />

# rup.ai

**A terminal-first AI coding agent.**

Plan, write, run and fix real code — straight from your command prompt.

`Go` · cross-platform · cloud + local AI providers · localhost companion UI

</div>

---

## What rup.ai does

rup.ai is a CLI tool (not an IDE). You describe what you want in plain language and it:

1. inspects your workspace (files, manifests, git state, docs),
2. creates a plan,
3. creates and edits real files,
4. installs dependencies and runs build/test commands,
5. reads the *actual* errors, fixes them, and re-verifies,
6. reports exactly what happened — files changed, commands run, test results.

It competes with tools like Claude Code, OpenAI Codex CLI and Gemini CLI, and it
works with **many** AI providers — cloud, subscription-document, or fully local.

```bash
rup.ai                                            # interactive session (first run: setup wizard)
rup.ai "Build a REST API for a todo app with SQLite and tests"
```

---

## Installation

### Windows

1. Download `rup.ai-windows-amd64.exe` from the release (or build it: see BUILD.md).
2. Rename it to `rup.ai.exe` and put it in a folder that is on your `PATH`
   (e.g. `C:\Tools` — add it via *System Properties → Environment Variables*).

PowerShell:

```powershell
New-Item -ItemType Directory -Force "$HOME\Tools" | Out-Null
Copy-Item .\rup.ai-windows-amd64.exe "$HOME\Tools\rup.ai.exe"
$env:PATH += ";$HOME\Tools"          # for this session
# add permanently: [Environment]::SetEnvironmentVariable("Path", $env:PATH + ";$HOME\Tools", "User")
rup.ai --version
```

CMD:

```cmd
mkdir %USERPROFILE%\Tools 2>nul
copy rup.ai-windows-amd64.exe %USERPROFILE%\Tools\rup.ai.exe
set PATH=%PATH%;%USERPROFILE%\Tools
rup.ai --version
```

Works in Windows Terminal, PowerShell and CMD — including the classic Windows
console. rup.ai enables VT rendering when available and automatically
translates colors into native console calls on older consoles, so the banner,
animation and colors never print as literal escape codes. `--plain` forces a
zero-color, zero-animation mode (`NO_COLOR` and non-TTY output do the same).

### macOS / Linux

```bash
mv rup.ai-darwin-arm64 rup.ai          # or rup.ai-linux-amd64
chmod +x rup.ai
sudo mv rup.ai /usr/local/bin/
rup.ai --version
```

Or build from source: see **BUILD.md**.

---

## First launch

Run `rup.ai` in any folder. A setup wizard appears:

```
Welcome to rup.ai

Choose your AI connection:

  [1] OpenAI-compatible API
  [2] Anthropic-compatible API
  [3] Sign in using a supported provider authentication method
  [4] Connect to Ollama
  [5] Connect to llama.cpp
  [6] Connect to KoboldCpp
  [7] Load a local GGUF model
  [8] Configure a custom provider
  [9] Configure this later
```

The wizard asks for credentials (input hidden), can discover models, and
**tests the connection before saving**. Nothing is saved until you confirm.
Settings go to your OS config directory; credentials go to your OS credential
store (Windows Credential Manager / macOS Keychain / Secret Service). On
headless Linux without a keyring, rup.ai falls back to a `0600` file and tells
you so.

If you skip setup, run `rup.ai setup` later, or configure via environment
variables (handy for CI):

```bash
export RUPAI_PROVIDER=zai RUPAI_MODEL=glm-4.6 RUPAI_API_KEY=... RUPAI_BASE_URL=https://api.z.ai/api/paas/v4
```

---

## Supported providers

`rup.ai providers` lists what's configured; `rup.ai models` discovers models
where the provider supports it.

| Preset | Wire API | Notes |
|---|---|---|
| OpenAI | OpenAI `/v1/chat/completions` | API key from platform.openai.com |
| Anthropic | Anthropic Messages API | API key from console.anthropic.com |
| DeepSeek | OpenAI-compatible | api.deepseek.com |
| Qwen (DashScope) | OpenAI-compatible | intl + CN endpoints |
| Kimi (Moonshot) | OpenAI-compatible | intl (.ai) + CN (.cn) endpoints |
| Z.ai / GLM | OpenAI-compatible | glm-4.6 etc.; CN mirror documented |
| MiniMax | OpenAI-compatible | region-dependent base URL editable |
| OpenRouter | OpenAI-compatible | one key, many models |
| Groq, Together, Fireworks, SiliconFlow | OpenAI-compatible | |
| Ollama | local | auto-detects installed models |
| llama.cpp server | local | point at `llama-server` |
| KoboldCpp | local | OpenAI-compatible endpoint |
| Custom | either | any OpenAI- or Anthropic-compatible URL |

Anything OpenAI-compatible works — vLLM, LM Studio, LiteLLM, corporate
gateways — via `custom` or by editing `base_url` in `rup.ai config`.

### Authentication: API access vs subscription access (please read)

| Access type | What it is | rup.ai support |
|---|---|---|
| **API key** | Officially documented, pay-per-token keys for every cloud provider above | ✅ fully supported (`rup.ai login`) |
| **Subscription sign-in** (ChatGPT Plus/Pro, Claude Pro/Max) | OAuth login **restricted by OpenAI and Anthropic to their own first-party apps** | ❌ not available to third-party tools — rup.ai does not impersonate official clients |
| **Official CLI authentication** (e.g. Qwen Code device login, Codex CLI sign-in) | tied to that specific CLI | ❌ not reusable by third-party tools |
| **OAuth2 (device / PKCE)** | implemented in rup.ai and used **only** where a provider or your own gateway publishes third-party OAuth endpoints (configurable per provider) | ✅ available |
| **Local inference** (Ollama / llama.cpp / KoboldCpp / GGUF) | no account at all | ✅ fully supported |

`rup.ai login --info` prints this explanation inside the CLI. rup.ai never
extracts browser cookies, scrapes private endpoints, or bypasses subscription
restrictions, and it never promises unlimited usage.

---

## Local models (Ollama, llama.cpp, GGUF)

- **Ollama**: wizard auto-detects the server and lists installed models
  (`rup.ai models --provider ollama`).
- **llama.cpp / KoboldCpp**: point at the server URL; rup.ai talks to their
  OpenAI-compatible endpoints.
- **GGUF files**: `rup.ai models --import model.gguf` parses the file (pure
  Go: architecture, quantization, context length, size), estimates the RAM
  needed **against your actual system RAM** and warns when it won't fit.
  `rup.ai models --serve model.gguf --gpu-layers 12 --ctx 8192` launches a
  llama.cpp-compatible runtime (`llama-server` / `llamafile`) if one is on
  your `PATH`.

Notes, honestly stated:

- rup.ai **does not bundle** model weights or llama.cpp binaries, and it never
  downloads models automatically. Install llama.cpp yourself (BUILD.md).
- CPU inference works; GPU layers (`-ngl`) are optional and depend on your
  llama.cpp build.
- Cloud providers work with **no** local runtime installed.

---

## Commands

| Command | Purpose |
|---|---|
| `rup.ai` | interactive session (setup wizard on first run) |
| `rup.ai "objective"` | run one task and exit |
| `rup.ai setup` | re-run the setup wizard |
| `rup.ai providers` | list configured + available providers |
| `rup.ai models` | discover models; `--import`/`--serve` GGUF |
| `rup.ai login [id]` | store credentials (`--info` explains auth types) |
| `rup.ai logout [id]` | remove stored credentials |
| `rup.ai config` | interactive settings (`config export` prints without secrets) |
| `rup.ai doctor` | environment health check |
| `rup.ai preview` | start the localhost companion web UI |
| `rup.ai doctor` / `--help` / `--version` | diagnostics / help / version |

Global flags: `--provider`, `--model`, `--cwd`, `--mode`, `--no-anim`,
`--plain`, `--approve-all` (permissive policy for CI; catastrophic commands
stay blocked).

## Interactive session commands

```
/help /exit /clear            session control
/plan <objective>             plan first, change nothing
/approve /reject              approve or discard the pending plan
/mode <m>                     ask | plan | code | agent | review | debug
/provider /model /login /settings
/files /diff /undo            track & revert this session's changes
/usage /status /compact       tokens, state, context compaction
/terminal                     spawn your shell in the workspace
/preview                      live localhost web preview
@path/to/file                 attach a file to your prompt
```

Ctrl+C cancels the running task cleanly (no partial file corruption;
in-flight writes are backed up first).

---

## Agent modes

| Mode | Behavior |
|---|---|
| **ask** | answers questions; no file changes |
| **plan** | inspects the project, produces a plan; waits for `/approve` |
| **code** | targeted edits; verifies compilation when practical |
| **agent** | full autonomy within policy: create, run, test, fix, report |
| **review** | bug / security / maintainability / performance findings with file:line refs |
| **debug** | reproduce, find root cause, fix, verify |

Modes change both the system prompt and which tools the model can call
(read-only modes never see mutating tools).

---

## Full-stack generation

Ask for a whole app:

```
rup.ai "Build a complete task manager with registration, login, tasks CRUD,
filtering, a dashboard, SQLite storage and tests. Install dependencies,
run the tests, start the app and fix any errors."
```

rup.ai inspects the workspace, picks a stack, writes the code, installs
dependencies with the detected package manager, runs the real test suite,
reads the real failures and iterates. It starts dev servers as **managed
background processes** and reports their local URLs (`/preview` shows the
running app in the browser). If something needs external services or
credentials you haven't provided (a real database server, SMTP, payment
keys), rup.ai says exactly what's missing instead of pretending.

---

## Terminal security

Model-generated commands are untrusted. rup.ai applies:

- **Workspace jail** — all file tools resolve and verify paths inside the
  workspace (symlink-aware). Escapes are refused and reported to the model.
- **Command classification** — every command is screened:
  - *blocked* (always, even with `--approve-all`): `rm -rf /`, `format`/`mkfs`,
    `dd of=/dev/…`, fork bombs, `shutdown`, `curl | sh`, registry tampering…
  - *dangerous* (`rm -r`, `git push --force`, `drop database`, `sudo`…):
    explicit approval even under permissive policy.
  - *confirm*: normal writes/commands — approval under **standard** policy.
  - *safe*: read-only allowlist (`ls`, `git status`, `go test`…) — auto-run.
- **Approval policies** — `restricted` (confirm everything), `standard`
  (recommended), `permissive` (auto except dangerous). Set in `/settings`,
  `RUPAI_APPROVAL`, or `--approve-all`.
- **Secret hygiene** — `.env`-style files are redacted before being read into
  model context; API keys, tokens, AWS/GitHub/Anthropic/Google keys and PEM
  blocks are redacted from tool output; credentials live in the OS keyring
  (0600-file fallback), never in the repo, and `rup.ai config export` never
  includes them. Everything security-relevant is appended to a user-level
  audit log (`data dir/audit.log`).
- **Protected paths** — `~/.ssh/id_*`, `~/.aws/credentials`, `~/.netrc`,
  `~/.kube/config` are refused even if the model asks.

---

## Companion web UI (optional)

`/preview` (or `rup.ai preview`) starts a **localhost-only** web interface:

- agent status, provider/model selection, approval policy,
- managed dev servers (start/stop, live output tail),
- **live preview** of your actual running application in responsive viewports,
- light/dark theme (persisted).

It binds to `127.0.0.1` by default, is token-protected and cross-origin
guarded. It is a companion — the CLI is the product. The UI requires a build
step (`npm install && npm run build`, see BUILD.md); without it the CLI still
runs apps and reports URLs, and the API stays available.

---

## Project context & git

- Workspace tree indexing with `.gitignore` + `.rupaiignore` support and
  always-on defaults (`node_modules`, build output, media, lockfiles…).
- Token-budgeted context assembly: relevance-ranked files, bounded tree,
  README/`AGENTS.md` guidance, `@file` expansion — never the whole repo.
- Automatic conversation compaction when history outgrows the budget.
- Git awareness: repo detection, status/diff/log tools, staging and commit
  tools. rup.ai **never pushes** automatically and refuses commit messages
  that look like they contain credentials.

---

## Performance

Targets ordinary hardware (Core i3-class CPU, 8 GB RAM, SSD):

- single static Go binary, ~10 MB, no runtime dependencies,
- no Docker, no Python, no GPU, no background services,
- streaming responses, bounded loops (steps + wall-clock budget),
- efficient, budgeted context retrieval; local models strictly optional.

---

## Troubleshooting

| Symptom | Fix |
|---|---|
| "no provider configured" | run `rup.ai setup`, or set `RUPAI_*` env vars |
| "connection test failed" | check key/base URL; `rup.ai doctor` probes reachability |
| colors look wrong on Windows | update to v0.1.1+ (auto-translates on old consoles); `rup.ai --plain` always works |
| keyring unavailable (headless Linux) | install `dbus` + `gnome-keyring`/`kwallet`, or accept the 0600 file fallback (`RUPAI_STORE=file` forces it) |
| GGUF "won't fit in RAM" | pick a smaller quantization or smaller model; the estimate uses your real system RAM |
| agent asks too often / too rarely | adjust the approval policy in `/settings` |
| task hits the step budget | raise `max_agent_steps` in settings, or `/compact` and continue |
| model discovery fails | some endpoints don't implement `/models` — type the model id manually |
| `web UI not built` | `npm install && npm run build` (BUILD.md), or use the CLI only |

## Known limitations

- Subscription login for OpenAI/Anthropic is exclusive to their own apps; API
  keys are the documented third-party route (see the table above).
- Direct in-process GGUF inference is **not** embedded; rup.ai shells out to a
  llama.cpp-compatible runtime you install. Parsing/management/RAM checks are
  built in.
- Provider endpoints and model names change; presets are editable in settings.
- MiniMax and other "OpenAI-compatible" APIs occasionally deviate from the
  de-facto standard; use a custom provider entry if a preset misbehaves.
- Windows colors are handled automatically (VT enabled where supported,
  translated to native console calls elsewhere); `--plain` forces plain text.
- macOS .exe-style cross-builds from Windows/Linux require care with the
  keyring dependency — build macOS binaries **on macOS** (BUILD.md).

---

## Development

```
rupai/
  backend/    exactly 15 Go source files (the entire CLI)
  frontend/   exactly 5 first-party files (index.html, main.ts, app.ts, styles.css, preview.ts)
  tests/      automated test suite (black-box, local protocol test servers)
  scripts/    verify.sh · build.sh · package.sh
  .github/    CI + release workflows
```

```bash
go test ./tests        # full suite (builds the binary, no network needed)
scripts/verify.sh      # file counts + build + vet + frontend build
```

## License

MIT — see [LICENSE](LICENSE).
