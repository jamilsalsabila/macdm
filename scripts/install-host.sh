#!/usr/bin/env bash
# Register the MacDM native messaging host with every Chromium-family browser
# and Firefox found on this Mac.
#
# Usage:
#   scripts/install-host.sh <chrome-extension-id>
#
# The extension id is shown on chrome://extensions (with Developer mode on) after
# you "Load unpacked" the extension/ directory. For Firefox the id is fixed
# (macdm@example.invalid, from extension/firefox/manifest.json) so it is not
# needed there.
#
# Run scripts/install-host.sh --uninstall to remove the manifests.

set -euo pipefail

REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
HOST_BIN="${MACDM_HOST_BIN:-$REPO_DIR/bin/macdm-nmhost}"
NAME="com.macdm.nmhost"
SUPPORT="$HOME/Library/Application Support"

CHROMIUM_DIRS=(
  "$SUPPORT/Google/Chrome"
  "$SUPPORT/Google/Chrome Beta"
  "$SUPPORT/Google/Chrome Canary"
  "$SUPPORT/Chromium"
  "$SUPPORT/Microsoft Edge"
  "$SUPPORT/BraveSoftware/Brave-Browser"
  "$SUPPORT/Vivaldi"
  "$SUPPORT/Arc/User Data"
)
FIREFOX_DIRS=(
  "$SUPPORT/Mozilla"
  "$SUPPORT/zen"                       # Zen browser
  "$SUPPORT/librewolf"
)

if [[ "${1:-}" == "--uninstall" ]]; then
  for base in "${CHROMIUM_DIRS[@]}" "${FIREFOX_DIRS[@]}"; do
    f="$base/NativeMessagingHosts/$NAME.json"
    [[ -f "$f" ]] && rm -v "$f"
  done
  echo "done."
  exit 0
fi

# The Chrome manifest pins a "key", so the unpacked extension always gets this
# id. Pass a different id as $1 only if you repacked with your own key.
EXT_ID="${1:-bpdoaihjlkkbkkmeiccefmbalbhcppho}"

if [[ ! -x "$HOST_BIN" ]]; then
  echo "error: host binary not found at $HOST_BIN" >&2
  echo "build it first:  go build -o bin/macdm-nmhost ./cmd/macdm-nmhost" >&2
  exit 1
fi

install_one() { # $1 = target dir, $2 = template
  local dir="$1/NativeMessagingHosts"
  mkdir -p "$dir"
  sed -e "s|__HOST_PATH__|$HOST_BIN|g" \
      -e "s|__EXTENSION_ID__|$EXT_ID|g" \
      "$2" > "$dir/$NAME.json"
  echo "  installed $dir/$NAME.json"
}

echo "Chromium-family:"
for base in "${CHROMIUM_DIRS[@]}"; do
  [[ -d "$base" ]] || continue
  install_one "$base" "$REPO_DIR/hosts/com.macdm.nmhost.chrome.json"
done

echo "Firefox-family:"
for base in "${FIREFOX_DIRS[@]}"; do
  [[ -d "$base" ]] || continue
  install_one "$base" "$REPO_DIR/hosts/com.macdm.nmhost.firefox.json"
done

echo
echo "Done. Restart the browser, then click the MacDM toolbar icon to confirm"
echo "'daemon connected' (make sure macdmd is running: bin/macdm daemon)."
