# MacDM

An Internet Download Manager–style download manager for macOS, built to
demonstrate **both** techniques IDM-class tools use, in one app:

| | how it finds the media | when you use it |
|---|---|---|
| **Sniffer** | A browser extension watches every request the page makes (`chrome.webRequest`) and hands the media URL + the exact session headers to the engine. The browser already did the auth / signature / manifest work — MacDM just reuses the request. | Any site, while a video is playing. Click the floating **⬇ MacDM** button or the toolbar popup. |
| **Extractor** | You give it a *page* URL. `yt-dlp` actively resolves the real streams (running the site's cipher logic, picking formats), then ffmpeg merges video+audio. | YouTube / Vimeo / etc., paste-and-go, no browser needed. |

Both paths feed **one engine** (multi-connection HTTP with resume, HLS/DASH
assembly) and **one UI** (a menu-bar app; a CLI; a local web page).

See [Intentional limitations](#intentional-limitations) for what MacDM
deliberately will not do, and the current rough edges.

---

## Install

Build the disk image (`dist/MacDM-<version>.dmg`):

```bash
scripts/make-dmg.sh
```

It contains a self-contained `MacDM.app` (daemon, engine, **ffmpeg and yt-dlp
bundled**), the unpacked browser extension, and `INSTALL.txt`. To install:

1. **Drag `MacDM.app` onto Applications.**
2. **First launch:** right-click `MacDM.app` → **Open** → **Open** (the app is
   ad-hoc signed, not notarised — this is a one-time Gatekeeper step). The
   menu-bar arrow icon means it's running. The app registers the native-
   messaging host and clears its own quarantine flag automatically.
3. **Add the extension:**
   - **Chrome / Edge / Brave / Vivaldi / Arc:** `chrome://extensions` → enable
     **Developer mode** → **Load unpacked** → pick the **MacDM Extension
     (Chrome)** folder (copy it somewhere permanent first).
   - **Firefox:** regular Firefox refuses unsigned add-ons. On Firefox Developer
     Edition / Nightly / ESR, set `xpinstall.signatures.required = false` in
     `about:config`, then open **MacDM Extension (Firefox).xpi**. On any Firefox,
     `about:debugging` → *This Firefox* → *Load Temporary Add-on* works until the
     next restart.

   Then click the toolbar icon; it should say *daemon connected*.

The daemon keeps its bundled `yt-dlp` current (daily check, or **Settings →
Bundled tools → Update now**); the fresh copy lands in
`~/Library/Application Support/MacDM/bin/` and takes precedence over the one in
the app. Because site fixes ship in `yt-dlp` itself, MacDM rarely needs a
release to keep working.

---

## Intentional limitations

These are design choices, not bugs — MacDM stops where a download manager should.

**Never attempted (by design):**

- **DRM.** Widevine, PlayReady, FairPlay; HLS `SAMPLE-AES`; DASH
  `<ContentProtection>` / `cenc`. These are *detected* and the job is marked
  `drm_protected`. MacDM ships no CDM, does no key exchange, and does not
  decrypt protected segments. Netflix/Spotify/Disney+/etc. are out of scope.
- **CAPTCHA / bot-check solving.** No challenge solving, no automated clicking
  through "are you human" gates.
- **Account & credential automation.** MacDM never logs in, creates accounts,
  or types passwords. Authenticated downloads work only by *reusing* a session
  you already have: the sniffer replays the browser's own request, or the
  extractor uses `yt-dlp --cookies-from-browser <name>` (which prompts you via
  the macOS Keychain). Set `"cookies_from"` in `config.json` to enable it.
- **Rate-limit / throttle evasion.** No IP rotation, no proxy pools, no token
  forging. YouTube's `n`-parameter throttling is accepted as-is (slow YouTube
  downloads are expected). MacDM only parallelises with normal HTTP `Range`
  requests the server already supports.
- **P2P.** No BitTorrent, no usenet, no magnet links.
- **Browser-download take-over is opt-in.** The extension only *observes* by
  default. Turn on "Catch every browser download" in the popup for IDM-style
  interception.

**Rough edges (would be fixed with more time):**

- **Live streams** are refused: DASH `type="dynamic"`, live/event HLS, and
  low-latency HLS. No DVR-window assembly.
- **HLS** `#EXT-X-MEDIA` alternate audio/subtitle renditions are not merged —
  you get the variant's own muxed program only. `#EXT-X-BYTERANGE` and
  re-timing across `#EXT-X-DISCONTINUITY` are not handled.
- **DASH** supports only `SegmentTemplate` (with `SegmentTimeline`) and
  single-file `BaseURL`. `SegmentList`, `SegmentBase`-index-only multi-segment,
  and multi-`Period` concatenation are not supported. Embedded subtitles are
  not fetched.
- **Segment assembly buffers each segment fully in RAM.** Fine for normal
  videos; a multi-GB single-file DASH track will spike memory.
- **MV3 header visibility.** Chrome does not reliably expose `Cookie`,
  `User-Agent`, or `Authorization` on caught requests, so a *direct* media
  catch from a login-walled site may 403. Use the extractor + cookies path
  there.
- **Safari** is not supported (it needs an Xcode-wrapped App Extension).
- **Unsigned dev builds.** Gatekeeper will block the native-messaging host
  until the app is codesigned and notarised (see [Packaging](#packaging-for-distribution-not-done-in-this-repo)).
- **Loopback / single user.** The daemon listens only on `127.0.0.1`. No remote
  control, no multi-user.

---

## Architecture

```
Browser + extension ──native messaging (stdio)──► macdm-nmhost ──HTTP──┐
                                                                        │
menu-bar app  (MacDM.app)  ──HTTP + SSE──►  ┌───────────────────────────▼──┐
CLI           (macdm)      ──HTTP────────►  │        macdmd (daemon)       │
web page      (:7345/)     ──HTTP + SSE──►  │  engine · sniff · hls · dash │
                                            │  extractor · mux · store     │
                                            └──────────┬───────────────────┘
                                                       ▼
                                              yt-dlp   ffmpeg
```

| component | language | what it is |
|---|---|---|
| `cmd/macdmd` | Go | the daemon: job store + engine + loopback REST/SSE API (`127.0.0.1:7345`) |
| `cmd/macdm` | Go | CLI client (`add`, `ls`, `pause`, `resume`, `rm`, `watch`, `daemon`) |
| `cmd/macdm-nmhost` | Go | native-messaging relay the browser spawns; forwards to the daemon |
| `internal/engine` | Go | multi-connection ranged downloader + resume sidecar |
| `internal/hls` `internal/dash` | Go | manifest parsing + concurrent segment assembly (+ AES-128) |
| `internal/extractor` | Go | `yt-dlp` wrapper (format probe, download, progress) |
| `internal/mux` | Go | `ffmpeg` wrapper (remux / combine tracks, stream copy only) |
| `extension/` | JS | MV3 extension; shared JS, `extension/firefox/manifest.json` for the Firefox build (`scripts/build-firefox-xpi.sh`) |
| `app/` | Swift | AppKit menu-bar app (`NSStatusItem` + popover) |

---

## Build from source

```bash
# Go pieces
go build -o bin/macdmd       ./cmd/macdmd
go build -o bin/macdm        ./cmd/macdm
go build -o bin/macdm-nmhost ./cmd/macdm-nmhost

# menu-bar app (Command Line Tools are enough — no full Xcode)
( cd app && ./build.sh bundle )        # -> app/.build/MacDM.app

# distributable disk image (also builds the Firefox .xpi)
scripts/make-dmg.sh                    # -> dist/MacDM-<version>.dmg

# just the Firefox extension
scripts/build-firefox-xpi.sh          # -> build/firefox-extension/ + dist/MacDM-firefox.xpi
```

`app/build.sh bundle` runs `app/fetch-tools.sh`, which downloads the
`yt-dlp_macos` standalone and a static `ffmpeg` into `app/.tools/` and copies
them into `MacDM.app/Contents/Resources/bin/`. For a plain dev run without the
bundle, `brew install ffmpeg yt-dlp` and `go run ./cmd/macdmd`.

For local extension development (unpacked, without the app), register the
native-messaging host manually:

```bash
scripts/install-host.sh                # points at ./bin/macdm-nmhost
```

Run the tests:

```bash
go test ./...
```

---

## Use it

### 1. The daemon

You don't have to start it manually — any `bin/macdm` command spawns `macdmd`
in the background if it isn't already listening. To run it in the foreground
(to watch its log) use `bin/macdm daemon` or `bin/macdmd` directly.

Downloads land in `~/Downloads/MacDM` (change it in
`~/Library/Application Support/MacDM/config.json`); the auto-started daemon logs
to `~/Library/Application Support/MacDM/macdmd.log`.

### 2a. CLI

```bash
bin/macdm add "https://example.com/big.zip" -n 8
bin/macdm add "https://www.youtube.com/watch?v=aqz-KE-bpKQ"   # extractor path
bin/macdm add "https://example.com/stream/master.m3u8"        # HLS path
bin/macdm ls
bin/macdm watch            # live progress
```

### 2b. Menu-bar app (the IDM-style GUI)

```bash
( cd app && ./build.sh bundle )      # -> app/.build/MacDM.app
open app/.build/MacDM.app
```

A ⬇ icon appears in the menu bar (it auto-starts `macdmd`). Its menu opens
**MacDM** — a window listing every download (File / Size / Status / Speed / Time
left / %). Double-click a row for the **per-download detail window**, modelled on
IDM's: address, status, file size, "Downloaded X (Y %)", transfer rate, time
left, resume capability, a **segmented progress bar** (one block per connection),
a **Show details** disclosure revealing the *"progress by connections"* table
(# / Downloaded / Info), and Pause / Resume / Cancel / Open-folder.

- **Add URL…** and every caught download open the **New Download dialog**: file
  name, **Save to** folder (remembered), category, **Quality** menu
  (360p / 480p / 720p / 1080p60 / … or "Audio only"), **Connections** stepper
  (1–32), Download / Download Later / Cancel.
- **Settings…**: max connections, default folder, auto-accept toggle.

### 2c. Web page

Open <http://127.0.0.1:7345/> — same REST/SSE backend, with the segmented bar and
the connection table. Handy for headless boxes.

### 3. Browser sniffer

1. **Chrome/Edge/Brave:** `chrome://extensions` → enable *Developer mode* →
   *Load unpacked* → select `extension/`. The manifest pins a `key`, so the
   extension ID is always `bpdoaihjlkkbkkmeiccefmbalbhcppho` — it does **not**
   change when you reload.
2. Register the native-messaging host (no ID argument needed):

   ```bash
   go build -o bin/macdm-nmhost ./cmd/macdm-nmhost
   scripts/install-host.sh
   ```
3. Restart the browser. Click the MacDM toolbar icon — it should say
   **daemon connected** (the MacDM app or `bin/macdmd` must be running).
4. Play a video or start any download. Either hover a video and click
   **⬇ MacDM**, or open the popup (caught items are grouped
   video / audio / archive / document), or right-click → *Download with MacDM*.
   Each catch raises the **New Download dialog** in the app (or auto-starts if
   the app is closed / `auto_accept` is set).
5. *Optional, aggressive:* tick **"Catch every browser download"** in the popup
   to have MacDM intercept `.zip` / `.dmg` / `.pdf` / … downloads the way IDM
   does. Off by default.

**Firefox:** `scripts/build-firefox-xpi.sh` assembles the shared JS with
`extension/firefox/manifest.json` into `build/firefox-extension/` and
`dist/MacDM-firefox.xpi`. Load the folder via `about:debugging` → *This Firefox*
→ *Load Temporary Add-on* (gone on restart), or the `.xpi` on Developer
Edition / Nightly / ESR after setting `xpinstall.signatures.required = false`.
Regular Firefox needs an AMO-signed build. `background.js` drops the Chrome-only
`extraHeaders` spec when it detects Firefox; the app / `scripts/install-host.sh`
register the native-messaging host (fixed id `macdm@example.invalid`).

---

## How each path actually works

### Sniffer

`background.js` registers `webRequest.onSendHeaders` (to capture `Referer` /
`Origin` / `User-Agent` / `Cookie` where MV3 still exposes them) and
`onHeadersReceived` (to read `Content-Type` / `Content-Length`). `classify.js`
flags anything that looks like media — `video/*`, `audio/*`,
`application/vnd.apple.mpegurl`, `application/dash+xml`, `…/videoplayback`, big
`.mp4`, etc. — and drops individual `.ts` / `.m4s` fragments (it wants the
manifest, not every chunk). Hits are kept per-tab and badged.

On "download", the captured URL **and its headers** go to the daemon, which
replays exactly that request from its multi-connection engine — so a
signed/tokenised CDN URL keeps working.

### Extractor

`yt-dlp -J <page>` returns the resolved formats (yt-dlp has already run the
site's `n`/`sig` transforms). `Info.QualityChoices()` collapses them into the
quality menu (`1080p60`, `720p`, … + "Audio only"), each carrying a `-f`
selector. The daemon runs `yt-dlp -f <selector> --merge-output-format mp4
--progress --progress-template … --print after_move:filepath`, parses live
progress, and reads the final path back. Default selector is capped at 1080p.
Works on any of yt-dlp's ~1800 sites — YouTube (incl. Shorts), Instagram,
TikTok, Vimeo, Reddit, SoundCloud, …. For private/authed content set
`"cookies_from": "chrome"` in config; yt-dlp handles the Keychain access.

### Engine

`ProbeURL` does a `Range: bytes=0-0` GET to learn size + range support. If the
server supports ranges and the file is big enough, the file is split into N
contiguous chunks fetched in parallel with `WriteAt` into a pre-sized `.part`
file. A `<file>.macdm` sidecar records each chunk's progress; pause = cancel the
context (sidecar flushed), resume = re-request only the missing bytes. Each
chunk's byte range + downloaded count + state (`connecting` / `receiving` /
`done`) is reported to the UI as one row of the connections table and one block
of the segmented bar.

### HLS / DASH

The manifest URL (caught by the sniffer or given directly) is fetched and
parsed. HLS: master → best variant → media playlist → segments (+ `EXT-X-MAP`
init, + AES-128 decrypt when the key URI is fetchable). DASH: `SegmentTemplate`
with `$Number$`/`$Time$` and `SegmentTimeline`, best video + best audio
Representation. Segments download concurrently, concatenate in order, then
`ffmpeg -c copy` into an `.mp4`. `SAMPLE-AES` / `<ContentProtection>` / dynamic
manifests are refused.

---

## Packaging for distribution (not done in this repo)

The runnable pieces above are unsigned/dev builds. A shipped MacDM would need:

- **`MacDM.app` bundle** — `Contents/MacOS/{MacDM,macdmd,macdm-nmhost}`,
  `Contents/Resources/bin/{ffmpeg,yt-dlp}`, an `Info.plist` with
  `LSUIElement=true`.
- **Codesign + notarize** — Developer ID cert, Hardened Runtime, `codesign
  --deep --options runtime`, `notarytool submit`, `stapler staple`. Without this
  Gatekeeper blocks the native-messaging host.
- **Daemon as a LaunchAgent** — `~/Library/LaunchAgents/com.macdm.daemon.plist`
  with `RunAtLoad` + `KeepAlive`, instead of the app spawning a child.
- **Extension** — publish to the Chrome Web Store / AMO so the host manifest can
  pin a stable ID; the install script currently takes the unpacked dev ID.
- **ffmpeg licensing** — ship an LGPL build (or provide source) and include
  `COPYING` for ffmpeg and yt-dlp (Unlicense).
- **Safari** — would need an Xcode App-wrapped Web Extension target; out of scope
  here.

---

## Layout

```
cmd/          macdmd, macdm, macdm-nmhost
internal/
  engine/     ranged downloader, per-connection progress, resume sidecar
  hls/ dash/  manifest parse, quality lists, concurrent segment assembly
  extractor/  yt-dlp wrapper + QualityChoices
  sniff/      capture classifier (video/audio/archive/document), page-host list
  manager/    job lifecycle, probe, proposal ("New Download") flow, event bus
  mux tools api store config
extension/    manifest.json, background.js, classify.js, content.js, popup.*  (+ firefox/)
app/Sources/MacDM/
  MainWindow, DownloadDetailWindow, NewDownloadDialog, SettingsWindow,
  SegmentedBar, DaemonClient, DaemonProcess, AppDelegate
hosts/        native-messaging host manifest templates
scripts/      install-host.sh
```
