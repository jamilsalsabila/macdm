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

# The two architectures a shipped bundle must carry. Debug and release builds
# stay native so the edit-compile loop is not paying for a second slice.
ARCHES=(x86_64 arm64)
MACOS_MIN=12.0

if [[ "$MODE" == "bundle" ]]; then
  # Universal: build each slice separately and join them. Cross-compiling to
  # arm64 from an Intel Mac works with the Command Line Tools alone, though the
  # first arm64 build may pause to construct a matching standard library.
  for a in "${ARCHES[@]}"; do
    echo "compiling MacDM ($a)"
    swiftc -O -target "$a-apple-macos$MACOS_MIN" -o ".build/MacDM-$a" Sources/MacDM/*.swift
  done
  lipo -create "${ARCHES[@]/#/.build/MacDM-}" -output .build/MacDM
else
  FLAGS=(-o .build/MacDM)
  [[ "$MODE" == "release" ]] && FLAGS=(-O "${FLAGS[@]}")
  swiftc "${FLAGS[@]}" Sources/MacDM/*.swift
fi
echo "built .build/MacDM ($(lipo -archs .build/MacDM))"

if [[ "$MODE" == "bundle" ]]; then
  APP=.build/MacDM.app
  VER=$(grep -oE '"[0-9]+\.[0-9]+\.[0-9]+"' ../internal/config/config.go | head -1 | tr -d '"')
  VER=${VER:-0.0.0}

  # Go pieces (universal too) + bundled external tools.
  for prog in macdmd macdm-nmhost; do
    for a in "${ARCHES[@]}"; do
      goarch=amd64; [[ "$a" == "arm64" ]] && goarch=arm64
      ( cd .. && GOOS=darwin GOARCH="$goarch" go build -o "bin/$prog-$a" "./cmd/$prog" )
    done
    ( cd .. && lipo -create "bin/$prog-x86_64" "bin/$prog-arm64" -output "bin/$prog" \
                && rm -f "bin/$prog-x86_64" "bin/$prog-arm64" )
  done
  ./fetch-tools.sh

  rm -rf "$APP"
  mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources/bin"
  cp .build/MacDM "$APP/Contents/MacOS/MacDM"
  # NB: no `macdm` CLI here — it collides with `MacDM` on a case-insensitive
  # filesystem, and the app spawns the daemon itself so it isn't needed.
  cp ../bin/macdmd        "$APP/Contents/MacOS/macdmd"
  cp ../bin/macdm-nmhost  "$APP/Contents/MacOS/macdm-nmhost"
  cp .tools/ffmpeg .tools/yt-dlp .tools/yt-dlp_macos "$APP/Contents/Resources/bin/"

  # A bundle that is universal everywhere except one binary is not universal:
  # that binary is the one that fails on the other kind of Mac. yt-dlp is the
  # exception on purpose — it is a Python zipapp, so it has no architecture.
  for f in "$APP/Contents/MacOS/"* "$APP/Contents/Resources/bin/ffmpeg" \
           "$APP/Contents/Resources/bin/yt-dlp_macos"; do
    have=$(lipo -archs "$f" 2>/dev/null || echo "?")
    for a in "${ARCHES[@]}"; do
      grep -qw "$a" <<<"$have" || {
        echo "FATAL: $(basename "$f") is missing the $a slice (has: $have)" >&2
        exit 1
      }
    done
    printf '  %-16s %s\n' "$(basename "$f")" "$have"
  done

  # App icon (Dock + Finder). Regenerate if the generator is newer.
  if [[ ! -f AppIcon.icns || make-icon.swift -nt AppIcon.icns ]]; then
    swift make-icon.swift
  fi
  cp AppIcon.icns "$APP/Contents/Resources/AppIcon.icns"

  cat > "$APP/Contents/Info.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>CFBundleName</key><string>MacDM</string>
  <key>CFBundleIdentifier</key><string>com.macdm.app</string>
  <key>CFBundleExecutable</key><string>MacDM</string>
  <key>CFBundleIconFile</key><string>AppIcon</string>
  <key>CFBundlePackageType</key><string>APPL</string>
  <key>CFBundleShortVersionString</key><string>${VER}</string>
  <key>CFBundleVersion</key><string>${VER}</string>
  <key>LSMinimumSystemVersion</key><string>12.0</string>
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
    "$APP/Contents/Resources/bin/yt-dlp_macos" \
    "$APP/Contents/MacOS/macdmd" \
    "$APP/Contents/MacOS/macdm-nmhost"
  codesign --force --timestamp=none -s - "$APP"

  codesign --verify --deep --strict "$APP" && echo "  signature OK"
  echo "built $APP  v${VER}  (ad-hoc signed — not notarised)"
fi
