#!/usr/bin/env bash
# Assemble the Firefox build of the MacDM extension.
#
# The JS/HTML live in extension/ (shared with the Chrome build); only the
# manifest differs (extension/firefox/manifest.json — gecko id, background
# scripts instead of a service worker). This copies them together into
#
#   build/firefox-extension/    unpacked  -> about:debugging "Load Temporary Add-on"
#   dist/MacDM-firefox.xpi      zipped    -> Developer Edition / Nightly / ESR
#
# Regular Firefox rejects an unsigned .xpi; see README "Firefox".

set -euo pipefail
REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO"

OUT="build/firefox-extension"
rm -rf "$OUT"
mkdir -p "$OUT" dist

cp extension/background.js extension/content.js extension/tiktok-main.js \
   extension/popup.html extension/popup.js "$OUT/"
cp extension/firefox/manifest.json "$OUT/manifest.json"

XPI="dist/MacDM-firefox.xpi"
rm -f "$XPI"
( cd "$OUT" && zip -qr -X "../../$XPI" . )

echo "unpacked: $OUT/         (about:debugging -> Load Temporary Add-on -> manifest.json)"
echo "packaged: $XPI"
command -v web-ext >/dev/null && web-ext lint --source-dir "$OUT" --warnings-as-errors=false || true
