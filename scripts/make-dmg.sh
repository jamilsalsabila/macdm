#!/usr/bin/env bash
# Build MacDM.app and wrap it in a simple .dmg for distribution.
#
#   scripts/make-dmg.sh   ->   dist/MacDM-<version>.dmg
#
# The .dmg contains:
#   MacDM.app          the app (daemon + engine + ffmpeg + yt-dlp bundled)
#   Applications       symlink, so the user can drag the app in
#   MacDM Extension/   the unpacked browser extension (Load unpacked)
#   INSTALL.txt        the three setup steps

set -euo pipefail
REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO"

VER=$(grep -oE '"[0-9]+\.[0-9]+\.[0-9]+"' internal/config/config.go | head -1 | tr -d '"')
VER=${VER:-0.0.0}

echo "==> building MacDM.app"
( cd app && ./build.sh bundle )
echo "==> building Firefox .xpi"
./scripts/build-firefox-xpi.sh

STAGE="$(mktemp -d)"
trap 'rm -rf "$STAGE"' EXIT

cp -R "app/.build/MacDM.app" "$STAGE/MacDM.app"
ln -s /Applications "$STAGE/Applications"
cp -R "extension" "$STAGE/MacDM Extension (Chrome)"
rm -rf "$STAGE/MacDM Extension (Chrome)/firefox"
cp "dist/MacDM-firefox.xpi" "$STAGE/MacDM Extension (Firefox).xpi"

cat > "$STAGE/INSTALL.txt" <<'TXT'
MacDM — install

1. Drag MacDM.app onto the Applications folder in this window.

2. First launch only: open Applications, right-click MacDM, choose "Open",
   then click "Open" in the dialog. (macOS blocks unsigned apps on a normal
   double-click. You only do this once.)
   The MacDM icon appears in the Dock and in the menu bar when it's running.

3. Add the browser extension:

   Chrome / Edge / Brave / Vivaldi / Arc:
   - copy "MacDM Extension (Chrome)" out of this disk image to somewhere
     permanent (e.g. your home folder) — it must keep existing
   - open  chrome://extensions , turn on "Developer mode" (top right)
   - click "Load unpacked" and choose that folder

   Firefox:
   - regular Firefox refuses unsigned add-ons. Use Firefox Developer Edition,
     Nightly, or ESR: open  about:config , set
     xpinstall.signatures.required = false , then open
     "MacDM Extension (Firefox).xpi" from this image.
   - OR any Firefox, temporarily (gone on restart): about:debugging ->
     "This Firefox" -> "Load Temporary Add-on" -> pick the .xpi.

   Then reload the page and click the MacDM toolbar icon — it should say
   "daemon connected".

That's it. Hover any video and click the "⬇ MacDM" button, or click the
toolbar icon to grab whatever the page is loading.

Uninstall: drag MacDM.app to the Trash, remove the extension, and delete
~/Library/Application Support/MacDM
TXT

mkdir -p dist
DMG="dist/MacDM-${VER}.dmg"
rm -f "$DMG"

echo "==> creating $DMG"
hdiutil create \
  -volname "MacDM ${VER}" \
  -srcfolder "$STAGE" \
  -ov -format UDZO \
  "$DMG" >/dev/null

echo
echo "done: $DMG  ($(du -h "$DMG" | cut -f1))"
