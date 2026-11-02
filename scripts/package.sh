#!/usr/bin/env bash
# scripts/package.sh — assemble the source release ZIP (no credentials, no
# node_modules). Used locally and mirrored by the GitHub Actions release.
set -euo pipefail
cd "$(dirname "$0")/.."

VERSION="${1:-0.1.0}"
NAME="rup.ai-v${VERSION}-source"
OUT="release"

mkdir -p "$OUT" "$NAME"

# copy the project, excluding junk and the staging dirs themselves
tar -cf - \
  --exclude node_modules \
  --exclude 'frontend/dist' \
  --exclude build \
  --exclude release \
  --exclude '.git' \
  --exclude '*.log' \
  --exclude "$NAME" \
  . | tar -xf - -C "$NAME"
# drop the accidental empty staging dir if tar captured it before exclusion
rm -rf "$NAME/$NAME"

# include the prebuilt web UI if it exists (generated assets are allowed)
if [ -d frontend/dist ]; then
  mkdir -p "$NAME/frontend/dist"
  cp -r frontend/dist/. "$NAME/frontend/dist/"
fi

zip -qr "$OUT/$NAME.zip" "$NAME"
rm -rf "$NAME"
echo "packaged: $OUT/$NAME.zip"
