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
  rm -rf "$APP"
  mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources"
  cp .build/MacDM "$APP/Contents/MacOS/MacDM"
  # bundle the daemon + host next to the app so it runs self-contained
  for b in macdmd macdm-nmhost; do
    [[ -f "../bin/$b" ]] && cp "../bin/$b" "$APP/Contents/MacOS/$b"
  done
  cat > "$APP/Contents/Info.plist" <<'PLIST'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>CFBundleName</key><string>MacDM</string>
  <key>CFBundleIdentifier</key><string>com.macdm.app</string>
  <key>CFBundleExecutable</key><string>MacDM</string>
  <key>CFBundlePackageType</key><string>APPL</string>
  <key>CFBundleShortVersionString</key><string>0.1.0</string>
  <key>LSMinimumSystemVersion</key><string>12.0</string>
  <key>LSUIElement</key><true/>
</dict>
</plist>
PLIST
  echo "built $APP  (unsigned — see README 'Packaging')"
fi
