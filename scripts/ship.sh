#!/usr/bin/env bash
# Rebuild everything after a change and roll it out locally:
#   1. build MacDM.app + Firefox .xpi + dist/MacDM-<ver>.dmg   (scripts/make-dmg.sh)
#   2. replace the installed /Applications/MacDM.app with the fresh build
#   3. relaunch it
#
# Run this on every update so the .dmg and the app in the Dock never lag behind
# the source.
#
#   scripts/ship.sh            build, reinstall, relaunch
#   scripts/ship.sh --no-open  build + reinstall, don't relaunch

set -euo pipefail
REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO"

VER=$(grep -oE '"[0-9]+\.[0-9]+\.[0-9]+"' internal/config/config.go | head -1 | tr -d '"')
DEST="/Applications/MacDM.app"

# drop stale installers so dist/ only ever holds the current version
find dist -maxdepth 1 -name 'MacDM-*.dmg' ! -name "MacDM-${VER}.dmg" -delete 2>/dev/null || true

./scripts/make-dmg.sh

echo "==> stopping any running MacDM"
pkill -f "MacDM.app/Contents/MacOS/MacDM"   2>/dev/null || true
pkill -f "MacDM.app/Contents/MacOS/macdmd"  2>/dev/null || true
sleep 1

echo "==> installing $DEST  (v${VER})"
rm -rf "$DEST"
ditto "app/.build/MacDM.app" "$DEST"
xattr -dr com.apple.quarantine "$DEST" 2>/dev/null || true
codesign --verify --deep --strict "$DEST" && echo "   signature OK"

# nudge Finder/Dock to pick up a changed icon
touch "$DEST"
/usr/bin/killall Dock 2>/dev/null || true

if [[ "${1:-}" != "--no-open" ]]; then
  echo "==> launching"
  open "$DEST"
fi

echo
echo "shipped v${VER}:  $DEST  +  dist/MacDM-${VER}.dmg"
