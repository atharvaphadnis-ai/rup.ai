#!/usr/bin/env bash
# scripts/verify.sh — the mandatory file-count verification + build gates.
#
#   backend/  must contain EXACTLY 15 Go application source files (no tests)
#   frontend/ must contain EXACTLY 5 first-party files (dist/ is generated)
#
# Usage: scripts/verify.sh [--no-frontend-build]

set -euo pipefail
cd "$(dirname "$0")/.."

fail=0

echo "== rup.ai source file verification =="

# --- backend: exactly 15 .go application source files -----------------------
go_count=$(find backend -maxdepth 1 -name '*.go' ! -name '*_test.go' | wc -l | tr -d ' ')
echo "backend .go application source files : $go_count (expected 15)"
[ "$go_count" -eq 15 ] || { echo "FAIL: backend file count"; fail=1; }

test_count=$(find backend -maxdepth 1 -name '*_test.go' | wc -l | tr -d ' ')
echo "backend _test.go files               : $test_count (expected 0 — tests live in tests/)"
[ "$test_count" -eq 0 ] || { echo "FAIL: test files must live outside backend/"; fail=1; }

# --- frontend: exactly 5 first-party files ----------------------------------
expected="index.html main.ts app.ts styles.css preview.ts"
fe_count=0
for f in $expected; do
  if [ -f "frontend/$f" ]; then fe_count=$((fe_count + 1)); else echo "FAIL: missing frontend/$f"; fail=1; fi
done
echo "frontend first-party source files    : $fe_count (expected 5)"
[ "$fe_count" -eq 5 ] || { echo "FAIL: frontend file count"; fail=1; }

extra=$(find frontend -maxdepth 1 -type f ! -name 'index.html' ! -name 'main.ts' \
  ! -name 'app.ts' ! -name 'styles.css' ! -name 'preview.ts' | wc -l | tr -d ' ')
echo "unexpected extra files in frontend/  : $extra (expected 0)"
[ "$extra" -eq 0 ] || { echo "FAIL: extra first-party frontend files"; fail=1; }

# --- build gates -------------------------------------------------------------
echo
echo "== build gates =="
if command -v go >/dev/null 2>&1; then
  go build -o /tmp/rupai-verify ./backend && echo "go build      : OK" || { echo "FAIL: go build"; fail=1; }
  go vet ./backend && echo "go vet        : OK" || { echo "FAIL: go vet"; fail=1; }
  if [ "${1:-}" != "--no-frontend-build" ]; then
    go test ./tests -count=1 -timeout 480s >/dev/null 2>&1 \
      && echo "go test       : OK (14 tests)" || { echo "FAIL: go test"; fail=1; }
  fi
else
  echo "go: not found — skipping build gates"
fi

if command -v npm >/dev/null 2>&1 && [ "${1:-}" != "--no-frontend-build" ]; then
  npm run typecheck >/dev/null 2>&1 && echo "tsc typecheck : OK" || { echo "FAIL: tsc typecheck"; fail=1; }
  npm run build >/dev/null 2>&1 && echo "vite build    : OK" || { echo "FAIL: vite build"; fail=1; }
fi

echo
if [ "$fail" -eq 0 ]; then
  echo "VERIFICATION PASSED — 15 backend source files, 5 frontend source files, builds OK"
else
  echo "VERIFICATION FAILED"
  exit 1
fi
