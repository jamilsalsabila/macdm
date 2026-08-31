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

**[Download the latest release](https://github.com/jamilsalsabila/macdm/releases/latest)**
(`MacDM-<version>.dmg`) — everything is inside the bundle, so there is nothing
to install alongside it.

The disk image holds a self-contained `MacDM.app` (daemon, engine, **ffmpeg and
yt-dlp bundled**), both browser extensions, and `INSTALL.txt`. All binaries are
universal, so it runs on Intel and Apple Silicon — though only the Intel side
has been exercised on real hardware so far.

Prefer to build it yourself? `scripts/make-dmg.sh` produces the same
`dist/MacDM-<version>.dmg`.

To install:

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
- **HLS** handles alternate audio and subtitle renditions
  (`#EXT-X-MEDIA`), `#EXT-X-BYTERANGE` segments (including a byte-range
  `#EXT-X-MAP`), and `#EXT-X-DISCONTINUITY` — the remux regenerates timestamps
  across the join. `#EXT-X-I-FRAME-STREAM-INF` trick-play streams are ignored
  (they are keyframes only, not a downloadable rendition) and
  `#EXT-X-DATERANGE`/SCTE-35 ad markers are metadata — their segments download
  like any other.
- **DASH** supports `SegmentTemplate` (with `SegmentTimeline`), `SegmentList`
  addressed by `media` URLs, single-file `BaseURL`, and multi-`Period`
  presentations (each period is assembled and muxed separately, then joined
  with ffmpeg's concat demuxer so the differing timelines are re-timed).
  Byte-range addressing (`SegmentURL@mediaRange`, `SegmentBase`-index-only) is
  refused rather than mis-assembled.
- **Extractor (yt-dlp) downloads** can pick a dubbed audio track and write
  subtitles. The New Download dialog lists what the video actually offers —
  those rows are hidden when there is nothing to choose — and Settings holds a
  default (e.g. `id` and `id,en`) for downloads that skip the dialog. Subtitles land as `.srt` sidecars, and the merged audio is
  tagged with its real language. Left blank, yt-dlp picks audio by bitrate —
  which on a multi-language video is not necessarily the original.
- **Subtitles** from HLS/DASH are saved as a sidecar `.vtt` next to the video (named
  `<video>.<lang>.vtt`), not muxed into the container. HLS `TYPE=SUBTITLES`
  renditions are merged across segments with their `X-TIMESTAMP-MAP` applied;
  DASH text AdaptationSets are fetched as one file or merged when segmented.
  Not extracted to a sidecar: in-band CEA-608/708 captions (they ride inside the
  video stream and survive the remux, so a player that decodes them still shows
  them) and segmented TTML (ffmpeg has no TTML decoder, and gluing XML documents
  end to end parses as nothing — it is refused rather than mis-assembled). A
  subtitle failure never fails the video download.
- **MV3 header visibility.** Chrome does not reliably expose `Cookie`,
  `User-Agent`, or `Authorization` on caught requests, so a *direct* media
  catch from a login-walled site may 403. Use the extractor + cookies path
  there.
- **Safari** is not supported (it needs an Xcode-wrapped App Extension).
- **yt-dlp needs no Python.** The app bundles both the zipapp (fast, needs a
  working `python3`) and the self-contained `yt-dlp_macos`, and falls back to
  the latter automatically — a Mac without the Command Line Tools has only a
  `python3` stub that cannot run.
- **Unsigned dev builds.** Gatekeeper will block the native-messaging host
  until the app is codesigned and notarised (see [Packaging](#packaging-for-distribution)).
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

## Packaging for distribution

`scripts/make-dmg.sh` builds what the [releases](https://github.com/jamilsalsabila/macdm/releases)
carry, and `scripts/ship.sh` additionally replaces the installed copy in
`/Applications`. What that already does:

- **`MacDM.app` bundle** — `Contents/MacOS/{MacDM,macdmd,macdm-nmhost}`,
  `Contents/Resources/bin/{ffmpeg,yt-dlp,yt-dlp_macos}`, an `Info.plist` with
  `LSUIElement=true`.
- **Universal binaries** — every executable carries x86_64 and arm64. ffmpeg is
  assembled from two single-architecture static builds, because nobody
  publishes a universal one; the build fails outright if any binary is missing
  a slice.
- **Ad-hoc codesign** — enough for the app to run after the one-time
  right-click → Open, and enough for Chrome to launch the native-messaging
  host once the app clears its own quarantine flag on first launch.

What a properly distributed build would still need:

- **Notarization** — Developer ID cert, Hardened Runtime, `notarytool submit`,
  `stapler staple`. Needs a paid Apple Developer account; without it Gatekeeper
  demands the right-click → Open step.
- **Apple Silicon verification** — the arm64 slices are built and valid, but
  have not been run on real hardware.
- **Daemon as a LaunchAgent** — `~/Library/LaunchAgents/com.macdm.daemon.plist`
  with `RunAtLoad` + `KeepAlive`, instead of the app spawning a child.
- **Extension** — publish to the Chrome Web Store / AMO so the host manifest can
  pin a stable ID; the install script currently takes the unpacked dev ID.
  Firefox refuses unsigned add-ons outright, so the `.xpi` is temporary-only
  until AMO signs it.
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
