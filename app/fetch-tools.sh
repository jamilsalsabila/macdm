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
# BOTH builds are bundled, because which one works is a property of the *user's*
# Mac, not the build machine:
#   yt-dlp        the tiny zipapp — fast, but needs a working python3
#   yt-dlp_macos  self-contained — works with no python3 at all, but the
#                 PyInstaller build re-extracts ~40 MB per run (50s+ on Intel)
# tools.YtDlpInvocation prefers the zipapp and falls back to the standalone.
# Shipping only the zipapp meant every extractor download failed on a Mac
# without the Command Line Tools.
if [[ ! -x "$TOOLS/yt-dlp" ]]; then
  echo "yt-dlp: downloading yt-dlp zipapp (nightly)"
  curl -fSL "https://github.com/yt-dlp/yt-dlp-nightly-builds/releases/latest/download/yt-dlp" \
    -o "$TOOLS/yt-dlp"
  chmod +x "$TOOLS/yt-dlp"
fi
if [[ ! -x "$TOOLS/yt-dlp_macos" ]]; then
  echo "yt-dlp: downloading yt-dlp_macos (nightly, python3-free fallback)"
  curl -fSL "https://github.com/yt-dlp/yt-dlp-nightly-builds/releases/latest/download/yt-dlp_macos" \
    -o "$TOOLS/yt-dlp_macos"
  chmod +x "$TOOLS/yt-dlp_macos"
fi

echo "tools ready:"
"$TOOLS/ffmpeg" -version | head -1
printf 'yt-dlp '; "$TOOLS/yt-dlp" --version
