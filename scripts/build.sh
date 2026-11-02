#!/usr/bin/env bash
# scripts/build.sh — full reproducible build: frontend + backend binary.
# Output: build/rup.ai(.exe) plus frontend/dist for the companion web UI.
set -euo pipefail
cd "$(dirname "$0")/.."

VERSION="${1:-dev}"

echo "==> building companion web UI (optional but recommended)"
if command -v npm >/dev/null 2>&1; then
  npm install --no-audit --no-fund
  npm run typecheck
  npm run build
else
  echo "    npm not found — skipping frontend (CLI works without it)"
fi

echo "==> building rup.ai $VERSION"
mkdir -p build
GOOS="$(go env GOHOSTOS)" GOARCH="$(go env GOHOSTARCH)" \
  go build -trimpath -ldflags "-s -w -X main.version=$VERSION" \
  -o "build/rup.ai$( [ "$(go env GOHOSTOS)" = windows ] && echo .exe )" ./backend

echo "==> done: build/rup.ai — run it from any project directory"
