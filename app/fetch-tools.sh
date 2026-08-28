#!/usr/bin/env bash
# Ensure app/.tools/{ffmpeg,yt-dlp} exist so build.sh can bundle them into
# MacDM.app. Both are self-contained macOS binaries.
#
#   ffmpeg  — static build from evermeet.cx (or copied from a local install)
#   yt-dlp  — the yt-dlp_macos standalone from GitHub Releases
#
# Re-run to refresh; delete app/.tools to force a clean fetch.

set -euo pipefail
cd "$(dirname "$0")"
TOOLS=".tools"
mkdir -p "$TOOLS"

SUPPORT="$HOME/Library/Application Support/MacDM/bin"

# --- ffmpeg ---
if [[ ! -x "$TOOLS/ffmpeg" ]]; then
  if [[ -x "$SUPPORT/ffmpeg" ]]; then
    echo "ffmpeg: copying from $SUPPORT/ffmpeg"
    cp -L "$SUPPORT/ffmpeg" "$TOOLS/ffmpeg"
  else
    echo "ffmpeg: downloading static build from evermeet.cx"
    tmp="$(mktemp -d)"
    curl -fSL "https://evermeet.cx/ffmpeg/getrelease/ffmpeg/zip" -o "$tmp/ffmpeg.zip"
    unzip -o -q "$tmp/ffmpeg.zip" -d "$tmp"
    mv "$tmp/ffmpeg" "$TOOLS/ffmpeg"
    rm -rf "$tmp"
  fi
  chmod +x "$TOOLS/ffmpeg"
fi

# --- yt-dlp (nightly: site fixes land here first; MacDM auto-updates it too) ---
if [[ ! -x "$TOOLS/yt-dlp" ]]; then
  echo "yt-dlp: downloading yt-dlp_macos (nightly) from GitHub Releases"
  curl -fSL "https://github.com/yt-dlp/yt-dlp-nightly-builds/releases/latest/download/yt-dlp_macos" \
    -o "$TOOLS/yt-dlp"
  chmod +x "$TOOLS/yt-dlp"
fi

echo "tools ready:"
"$TOOLS/ffmpeg" -version | head -1
printf 'yt-dlp '; "$TOOLS/yt-dlp" --version
