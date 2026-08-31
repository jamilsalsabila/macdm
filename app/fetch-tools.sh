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

# Pinned rather than "latest": osxexperts publishes one file per release, so a
# floating URL is not on offer, and an unpinned one would change under us.
FFMPEG_ARM_URL="${FFMPEG_ARM_URL:-https://www.osxexperts.net/ffmpeg711arm.zip}"

# --- ffmpeg (universal) ---
# Assembled from two single-architecture static builds, because nobody
# publishes a universal one. Both slices matter: MacDM shells out to ffmpeg for
# every merge of separate video and audio streams, so an Intel-only ffmpeg
# inside an Apple Silicon app would demand Rosetta before YouTube worked at all.
#   x86_64  evermeet.cx
#   arm64   osxexperts.net
if [[ ! -x "$TOOLS/ffmpeg-x86_64" ]]; then
  if [[ -x "$SUPPORT/ffmpeg" ]] && lipo -archs "$SUPPORT/ffmpeg" 2>/dev/null | grep -qw x86_64; then
    echo "ffmpeg(x86_64): copying from $SUPPORT/ffmpeg"
    cp -L "$SUPPORT/ffmpeg" "$TOOLS/ffmpeg-x86_64"
  else
    echo "ffmpeg(x86_64): downloading static build from evermeet.cx"
    tmp="$(mktemp -d)"
    curl -fSL "https://evermeet.cx/ffmpeg/getrelease/ffmpeg/zip" -o "$tmp/ffmpeg.zip"
    unzip -o -q "$tmp/ffmpeg.zip" -d "$tmp"
    mv "$tmp/ffmpeg" "$TOOLS/ffmpeg-x86_64"
    rm -rf "$tmp"
  fi
  chmod +x "$TOOLS/ffmpeg-x86_64"
fi

if [[ ! -x "$TOOLS/ffmpeg-arm64" ]]; then
  echo "ffmpeg(arm64): downloading static build from osxexperts.net"
  tmp="$(mktemp -d)"
  curl -fSL "$FFMPEG_ARM_URL" -o "$tmp/ffmpeg.zip"
  unzip -o -q "$tmp/ffmpeg.zip" -d "$tmp"
  # The archive carries a __MACOSX/._ffmpeg resource fork alongside the binary.
  found="$(find "$tmp" -name ffmpeg -type f -not -path '*__MACOSX*' | head -1)"
  [[ -n "$found" ]] || { echo "no ffmpeg inside $FFMPEG_ARM_URL" >&2; exit 1; }
  mv "$found" "$TOOLS/ffmpeg-arm64"
  rm -rf "$tmp"
  chmod +x "$TOOLS/ffmpeg-arm64"
fi

# Refuse to build a bundle whose "universal" ffmpeg is secretly one-sided.
for a in x86_64 arm64; do
  lipo -archs "$TOOLS/ffmpeg-$a" | grep -qw "$a" || {
    echo "$TOOLS/ffmpeg-$a is not $a" >&2; exit 1; }
done
if [[ ! -x "$TOOLS/ffmpeg" || "$TOOLS/ffmpeg-arm64" -nt "$TOOLS/ffmpeg" || "$TOOLS/ffmpeg-x86_64" -nt "$TOOLS/ffmpeg" ]]; then
  echo "ffmpeg: joining both slices into a universal binary"
  lipo -create "$TOOLS/ffmpeg-x86_64" "$TOOLS/ffmpeg-arm64" -output "$TOOLS/ffmpeg"
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
printf 'ffmpeg   '; lipo -archs "$TOOLS/ffmpeg"
# Only the slice matching this Mac can actually be run here.
"$TOOLS/ffmpeg" -version 2>/dev/null | head -1 || true
printf 'yt-dlp '; "$TOOLS/yt-dlp" --version
