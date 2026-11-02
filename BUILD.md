# BUILD.md — building, packaging and releasing rup.ai

## 1. Development environment

| Tool | Version | Needed for |
|---|---|---|
| Go | ≥ 1.24 (tested with 1.27) | the CLI (`backend/`) |
| Node.js + npm | ≥ 20 | companion web UI (`frontend/`), optional |
| git | any | `git init` repos, release tooling |

No cgo, no Docker, no Python required. Third-party Go dependencies:
`golang.org/x/term` (terminal handling) and `github.com/zalando/go-keyring`
(credential stores) — fetched by the Go module proxy at build time.

## 2. Build commands

```bash
# CLI only (frontend not needed for the agent to work)
go build -ldflags "-X main.version=v0.1.2" -o rup.ai ./backend

# everything, reproducibly (frontend build + binary + layout checks)
scripts/build.sh            # → build/rup.ai (+ frontend/dist bundled layout)

# run the test suite (builds a test binary; local test servers only)
go test ./tests -v

# vet + frontend typecheck
go vet ./... && (cd frontend-src && npx tsc --noEmit)   # see scripts/build.sh
```

The binary name is literally `rup.ai` (`rup.ai.exe` on Windows). Shells run
dot-names fine on Windows, macOS and Linux.

## 3. Windows packaging

```powershell
# from a Windows machine (or cross-compile from Linux/macOS)
$env:GOOS="windows"; $env:GOARCH="amd64"
go build -trimpath -ldflags "-s -w -X main.version=v0.1.2" -o rup.ai-windows-amd64.exe ./backend
```

- Ship the `.exe` zipped: `Compress-Archive rup.ai.exe rup.ai-windows-windows-amd64.zip`.
- Optional Start-Menu/desktop shortcuts: document or script a
  `$WshShell.CreateShortcut(...)` step; an MSI installer is intentionally not
  bundled (keep the tool lightweight).
- Optional exe icon/metadata: generate a `.syso` with
  `github.com/josephspurrier/goversioninfo` using `assets/logo.png` as the
  icon source. This is optional and not wired into the default build.

## 4. Linux packaging

```bash
GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w -X main.version=v0.1.2" \
  -o rup.ai-linux-amd64 ./backend
# arm64:
GOOS=linux GOARCH=arm64 go build ... -o rup.ai-linux-arm64 ./backend
```

Optional `.deb`: put the binary in `/usr/bin/rup.ai` via
`dpkg-deb --build` with a minimal `DEBIAN/control` (`Package: rup-ai`,
`Architecture: amd64`, `Depends: libc6`). Not bundled by default.

## 5. macOS packaging

**Build on macOS** (the keyring dependency uses the Security framework via
cgo; cross-compiling darwin from Windows/Linux is not supported):

```bash
GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags "-s -w -X main.version=v0.1.2" \
  -o rup.ai-darwin-arm64 ./backend     # Apple Silicon (macos-latest runner)
GOOS=darwin GOARCH=amd64 go build ... -o rup.ai-darwin-amd64 ./backend  # Intel
```

Codesigning/notarization is optional and out of scope of this repo; unsigned
binaries require the usual right-click → Open on first run.

## 6. GitHub Actions releases

Two workflows ship with the project:

- `.github/workflows/ci.yml` — on every push/PR: `go vet`, `go test ./tests`
  (Linux), frontend typecheck + build, file-count verification
  (`scripts/verify.sh`).
- `.github/workflows/release.yml` — on pushing a tag (`v*`): builds
  Windows (x64), Linux (x64/arm64) and macOS (arm64, x64) artifacts **on their
  native runners**, plus the frontend bundle, and attaches zips to a GitHub
  Release.

Release procedure from a Windows machine:

```powershell
git tag v0.1.2
git push origin v0.1.2        # Actions builds and attaches the artifacts
```

Do not claim macOS testing unless the artifacts were run on macOS; CI build
success on a macOS runner is not the same as user-verified testing.

## 7. GGUF runtime packaging

rup.ai parses and manages GGUF files itself (pure Go) but **delegates
inference** to a llama.cpp-compatible server:

- install [llama.cpp](https://github.com/ggml-org/llama.cpp) and ensure
  `llama-server` (or `llamafile`, `llama-cli`) is on `PATH`;
- `rup.ai models --serve model.gguf --ctx 8192 --threads 4 --gpu-layers 0`
  starts it as a managed process on `127.0.0.1:8081` and configures the
  `llamacpp` provider automatically;
- rup.ai never bundles or auto-downloads model weights or llama.cpp binaries;
- RAM sufficiency is checked against real system memory before launch.

## 8. Cross-compilation limitations

| Target | From Linux | From Windows | From macOS |
|---|---|---|---|
| windows/amd64 | ✅ (used in CI/tests) | ✅ | ✅ |
| linux/amd64, arm64 | ✅ | ✅ | ✅ |
| darwin (any arch) | ⚠️ only with `CGO_ENABLED=0`, which **disables the macOS Keychain** (file fallback used) | ⚠️ same | ✅ recommended |

Everything else is pure Go and cross-compiles freely. The companion web UI is
built with npm/Vite and is platform-independent static output.

## 9. Source layout invariants

The project spec fixes the first-party source surface:

- `backend/` contains **exactly 15** `.go` application source files
  (no `_test.go` files — tests live in `tests/`);
- `frontend/` contains **exactly 5** first-party files
  (`index.html`, `main.ts`, `app.ts`, `styles.css`, `preview.ts`);
  generated build output lives in `frontend/dist/`.

`scripts/verify.sh` enforces both counts and runs the build gates. CI fails
on violations.
