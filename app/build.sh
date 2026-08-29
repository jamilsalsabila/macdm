#!/usr/bin/env bash
# Build the MacDM menu-bar app.
#
# Uses swiftc directly so it works with only the Command Line Tools installed
# (SwiftPM's PackageDescription module needs a full Xcode). If you have Xcode,
# `swift build -c release` via Package.swift works too.
#
#   ./build.sh          -> .build/MacDM        (debug)
#   ./build.sh release  -> .build/MacDM        (optimised)
#   ./build.sh bundle   -> .build/MacDM.app    (minimal .app, unsigned)

set -euo pipefail
cd "$(dirname "$0")"
mkdir -p .build

MODE="${1:-debug}"
FLAGS=(-o .build/MacDM)
[[ "$MODE" == "release" || "$MODE" == "bundle" ]] && FLAGS=(-O "${FLAGS[@]}")

swiftc "${FLAGS[@]}" Sources/MacDM/*.swift
echo "built .build/MacDM"

if [[ "$MODE" == "bundle" ]]; then
  APP=.build/MacDM.app
  VER=$(grep -oE '"[0-9]+\.[0-9]+\.[0-9]+"' ../internal/config/config.go | head -1 | tr -d '"')
  VER=${VER:-0.0.0}

  # Go pieces + bundled external tools.
  ( cd .. && go build -o bin/macdmd ./cmd/macdmd \
                     && go build -o bin/macdm-nmhost ./cmd/macdm-nmhost )
  ./fetch-tools.sh

  rm -rf "$APP"
  mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources/bin"
  cp .build/MacDM "$APP/Contents/MacOS/MacDM"
  # NB: no `macdm` CLI here — it collides with `MacDM` on a case-insensitive
  # filesystem, and the app spawns the daemon itself so it isn't needed.
  cp ../bin/macdmd        "$APP/Contents/MacOS/macdmd"
  cp ../bin/macdm-nmhost  "$APP/Contents/MacOS/macdm-nmhost"
  cp .tools/ffmpeg .tools/yt-dlp "$APP/Contents/Resources/bin/"

  cat > "$APP/Contents/Info.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>CFBundleName</key><string>MacDM</string>
  <key>CFBundleIdentifier</key><string>com.macdm.app</string>
  <key>CFBundleExecutable</key><string>MacDM</string>
  <key>CFBundlePackageType</key><string>APPL</string>
  <key>CFBundleShortVersionString</key><string>${VER}</string>
  <key>CFBundleVersion</key><string>${VER}</string>
  <key>LSMinimumSystemVersion</key><string>12.0</string>
  <key>LSUIElement</key><true/>
  <key>NSAppTransportSecurity</key>
  <dict><key>NSAllowsLocalNetworking</key><true/></dict>
  <key>NSDownloadsFolderUsageDescription</key>
  <string>MacDM saves your downloads here.</string>
  <key>NSDocumentsFolderUsageDescription</key>
  <string>MacDM saves downloads to the folder you choose.</string>
  <key>NSDesktopFolderUsageDescription</key>
  <string>MacDM saves downloads to the folder you choose.</string>
</dict>
</plist>
PLIST

  # Ad-hoc sign the helpers first, then the main executable last (signing
  # Contents/MacOS/MacDM seals the whole bundle and requires every sibling
  # already signed). No Apple Developer ID: the app clears its own quarantine
  # flag on first launch and the user does right-click -> Open once.
  codesign --force --timestamp=none -s - \
    "$APP/Contents/Resources/bin/ffmpeg" \
    "$APP/Contents/Resources/bin/yt-dlp" \
    "$APP/Contents/MacOS/macdmd" \
    "$APP/Contents/MacOS/macdm-nmhost"
  codesign --force --timestamp=none -s - "$APP"

  codesign --verify --deep --strict "$APP" && echo "  signature OK"
  echo "built $APP  v${VER}  (ad-hoc signed — not notarised)"
fi
