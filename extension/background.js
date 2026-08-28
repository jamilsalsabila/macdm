// MacDM background service worker.
//
//  1. Observe every request the browser makes (webRequest) and remember the ones
//     that look like downloadable media/files, per tab, with the request headers
//     the browser used (Referer / Origin / User-Agent where MV3 exposes them).
//  2. Expose that catch list to the popup and content script.
//  3. Relay "download this" commands to the native messaging host → macdmd.
//
// The capture heuristic is inlined here (no importScripts, no shared globals) to
// avoid any identifier-collision at service-worker registration. It mirrors
// internal/sniff/sniff.go; the daemon re-classifies authoritatively.

const MacDMClassify = (() => {
  const VIDEO_CT = new Set([
    "video/mp4", "video/webm", "video/x-matroska", "video/quicktime",
    "video/mp2t", "video/mpeg", "video/3gpp", "video/ogg", "video/x-flv", "video/x-m4v",
  ]);
  const AUDIO_CT = new Set([
    "audio/mpeg", "audio/mp4", "audio/aac", "audio/ogg", "audio/webm",
    "audio/x-m4a", "audio/flac", "audio/wav", "audio/x-wav", "audio/opus",
  ]);
  const DOWNLOAD_CT = {
    "application/zip": "archive", "application/x-zip-compressed": "archive",
    "application/x-rar-compressed": "archive", "application/vnd.rar": "archive",
    "application/x-7z-compressed": "archive", "application/x-tar": "archive",
    "application/gzip": "archive", "application/x-gzip": "archive",
    "application/x-bzip2": "archive", "application/x-xz": "archive",
    "application/x-apple-diskimage": "archive", "application/x-msdownload": "archive",
    "application/x-msi": "archive", "application/x-iso9660-image": "archive",
    "application/vnd.android.package-archive": "archive",
    "application/vnd.debian.binary-package": "archive",
    "application/octet-stream": "other", "application/x-bittorrent": "other",
    "application/pdf": "document", "application/epub+zip": "document",
    "application/rtf": "document", "application/msword": "document",
    "application/vnd.openxmlformats-officedocument.wordprocessingml.document": "document",
    "application/vnd.ms-excel": "document",
    "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet": "document",
    "application/vnd.ms-powerpoint": "document",
    "application/vnd.openxmlformats-officedocument.presentationml.presentation": "document",
  };
  const EXT_CAT = {
    mp4: "video", m4v: "video", webm: "video", mkv: "video", mov: "video",
    flv: "video", avi: "video", wmv: "video", mpg: "video", mpeg: "video",
    "3gp": "video", ogv: "video",
    mp3: "audio", m4a: "audio", aac: "audio", flac: "audio", wav: "audio",
    ogg: "audio", opus: "audio", wma: "audio",
    zip: "archive", rar: "archive", "7z": "archive", tar: "archive", gz: "archive",
    tgz: "archive", bz2: "archive", xz: "archive", zst: "archive", dmg: "archive",
    pkg: "archive", exe: "archive", msi: "archive", iso: "archive", apk: "archive",
    deb: "archive", rpm: "archive", appimage: "archive",
    pdf: "document", epub: "document", mobi: "document", azw3: "document",
    doc: "document", docx: "document", xls: "document", xlsx: "document",
    ppt: "document", pptx: "document", csv: "document", rtf: "document",
    psd: "other", ai: "other", blend: "other", torrent: "other",
  };
  const MANIFEST_RE = /\.(m3u8|mpd)(\?|$)/i;
  const FRAGMENT_RE = /\.(ts|m4s)(\?|$)/i;

  function extOf(u) {
    u = u.split(/[?#]/)[0];
    const slash = u.lastIndexOf("/");
    if (slash >= 0) u = u.slice(slash);
    const dot = u.lastIndexOf(".");
    return dot >= 0 ? u.slice(dot + 1).toLowerCase() : "";
  }
  function excludedType(ct) {
    return (
      ct === "text/html" || ct === "text/css" || ct === "text/plain" ||
      ct === "application/javascript" || ct === "text/javascript" ||
      ct === "application/json" || ct === "application/manifest+json" ||
      ct === "application/xml" || ct === "text/xml" || ct === "image/svg+xml" ||
      ct.startsWith("image/") || ct.startsWith("font/") || ct.startsWith("text/")
    );
  }
  function classify({ url, contentType, contentLength, disposition }) {
    const ct = (contentType || "").split(";")[0].trim().toLowerCase();
    const lower = url.split(/[?#]/)[0].toLowerCase();
    const len = contentLength === undefined || contentLength === null ? -1 : contentLength;
    const attachment = /attachment/i.test(disposition || "");

    if (/\.mpd(\?|$)/i.test(url) || ct === "application/dash+xml")
      return { url, kind: "dash", category: "video", contentType: ct, contentLength: len };
    if (MANIFEST_RE.test(url) || ct === "application/vnd.apple.mpegurl" ||
        ct === "application/x-mpegurl" || ct === "application/mpegurl")
      return { url, kind: "hls", category: "video", contentType: ct, contentLength: len };

    if (FRAGMENT_RE.test(url)) return null;

    const extCat = EXT_CAT[extOf(lower)];
    if (attachment)
      return { url, kind: "http", category: extCat || DOWNLOAD_CT[ct] || "other", contentType: ct, contentLength: len };
    if (VIDEO_CT.has(ct)) return { url, kind: "http", category: "video", contentType: ct, contentLength: len };
    if (AUDIO_CT.has(ct)) return { url, kind: "http", category: "audio", contentType: ct, contentLength: len };
    if (ct in DOWNLOAD_CT) {
      if (ct === "application/octet-stream" && len >= 0 && len < 512 * 1024 && !extCat) return null;
      return { url, kind: "http", category: extCat || DOWNLOAD_CT[ct], contentType: ct, contentLength: len };
    }
    if (excludedType(ct)) return null;
    if (extCat) {
      const big = len <= 0 || len > 512 * 1024;
      if (extCat === "document" || extCat === "audio" || big)
        return { url, kind: "http", category: extCat, contentType: ct, contentLength: len };
    }
    if (/videoplayback/i.test(lower) && (len <= 0 || len > 512 * 1024))
      return { url, kind: "http", category: "video", contentType: ct, contentLength: len };
    return null;
  }
  return { classify };
})();

const NATIVE_HOST = "com.macdm.nmhost";

// A URL yt-dlp can resolve to a full video: a permalink-shaped path
// (…/reel/<id>, …/p/<id>, …/watch?v=…, …/video/<id>…) on a real page host, or a
// known video host with any path. Bare origins and raw CDN media URLs are NOT.
const PERMALINK_PATH = /\/(p|reel|reels|tv|watch|shorts|video|photo|status|clip|v|e|embed)\/[\w-]{3,}/i;
// Canonical page hosts only — `www.`/`m.`/`vm.`/`vt.` or bare. This deliberately
// does NOT match media-CDN subdomains like v16-webapp-prime.tiktok.com.
const CANON_PAGE_HOSTS = /^(?:(?:www|m|mobile|vm|vt)\.)?(youtube\.com|youtu\.be|vimeo\.com|tiktok\.com|instagram\.com|facebook\.com|fb\.watch|twitter\.com|x\.com|threads\.net|reddit\.com|twitch\.tv|dailymotion\.com|bilibili\.com|soundcloud\.com|streamable\.com|rumble\.com|vk\.com|nicovideo\.jp|ok\.ru|bitchute\.com|odysee\.com)$/i;
// Raw media CDNs — a URL here is a signed, short-lived stream chunk, useless to
// yt-dlp (403). TikTok uses v<NN>-*.tiktok.com plus several bytedance domains.
const MEDIA_CDN_HOSTS = /(^|\.)(tiktokcdn[\w-]*\.com|tiktokv\.com|muscdn\.com|byteoversea\.com|ibyte[\w-]*\.com|akamaized\.net|fbcdn\.net|cdninstagram\.com|googlevideo\.com)$/i;
const TIKTOK_MEDIA_HOST = /^v\d+[\w-]*\.tiktok\.com$/i;

function isExtractableURL(u) {
  let p;
  try { p = new URL(u); } catch { return false; }
  if (!/^https?:$/.test(p.protocol)) return false;
  if (MEDIA_CDN_HOSTS.test(p.hostname) || TIKTOK_MEDIA_HOST.test(p.hostname)) return false;
  if (FRAGMENT_HOSTS.test(p.hostname)) return false;
  if (CANON_PAGE_HOSTS.test(p.hostname) && p.pathname.length > 1) return true;
  if (PERMALINK_PATH.test(p.pathname)) return true;
  if (/[?&](v|video_id|vid)=[\w-]{3,}/i.test(p.search)) return true;
  return false;
}

const FRAGMENT_HOSTS = /(^|\.)(fbcdn\.net|cdninstagram\.com|fbsbx\.com|akamaized\.net|googlevideo\.com)$/i;

// tabId -> Map(url -> caughtItem)
const caught = new Map();
// tabId -> Map(baseUrlKey -> {frags: Map(start -> {url,start,end}), ...})
const fragGroups = new Map();

const RANGEY_MEDIA_CT = /^(video|audio)\//i;

// The group key ignores the query entirely: Instagram/Facebook (and most signed
// CDNs) put a per-request token + the byte range in the query but keep the
// pathname stable for one asset, and the pathname already contains an asset
// hash, so a collision between two different videos is not realistic.
function groupKey(u) {
  const p = new URL(u);
  return p.origin + p.pathname;
}

// parseFragment inspects a request/response and returns
// {key, start, end, total} when it is one byte-range slice of a larger media
// file that MacDM should assemble (Instagram/Facebook and any CDN that serves a
// progressive MP4 in sequential Range requests — the pattern Neat DM catches).
// Returns null for anything else (whole-file responses, HLS/DASH segments…).
function parseFragment(rawUrl, ct, status, contentRange, reqRange) {
  let u;
  try { u = new URL(rawUrl); } catch { return null; }

  // 1. Instagram / Facebook style: range in the query string, plain 200 back.
  const bs = u.searchParams.get("bytestart");
  const be = u.searchParams.get("byteend");
  if (bs !== null && be !== null) {
    const s = parseInt(bs, 10), e = parseInt(be, 10);
    if (Number.isFinite(s) && Number.isFinite(e) && e >= s)
      return { key: groupKey(rawUrl), start: s, end: e, total: 0 };
  }

  // 2. Generic: a 206 with a Content-Range header for a media resource, or a
  //    request that carried a Range header and came back as media. Skip HLS/DASH
  //    segments (those are assembled by the stream path, not here) — a segment
  //    URL's path ends in .ts/.m4s, or a numeric/segment name.
  const media = RANGEY_MEDIA_CT.test(ct || "");
  if (!media) return null;
  if (/\.(ts|m4s|cmfv|cmfa)$/i.test(u.pathname)) return null;

  const cr = /bytes\s+(\d+)-(\d+)\/(\d+|\*)/i.exec(contentRange || "");
  if (status === 206 && cr) {
    const s = parseInt(cr[1], 10), e = parseInt(cr[2], 10);
    const total = cr[3] === "*" ? 0 : parseInt(cr[3], 10);
    // A single 206 that covers the whole file is not a "fragment".
    if (total && s === 0 && e === total - 1) return null;
    return { key: groupKey(rawUrl), start: s, end: e, total };
  }
  const rr = /bytes=(\d+)-(\d*)/i.exec(reqRange || "");
  if (rr && rr[2] !== "") {
    const s = parseInt(rr[1], 10), e = parseInt(rr[2], 10);
    if (e > s) return { key: groupKey(rawUrl), start: s, end: e, total: 0 };
  }
  return null;
}
// requestId -> { headers } captured at send time, consumed at response time
const pendingHeaders = new Map();

// Headers forwarded to the daemon so it can replay the request the way the
// browser made it. TikTok / ByteDance CDNs 403 a request that is missing the
// fetch-metadata + client-hint headers, so mirror those too.
const RELEVANT_HEADERS = [
  "referer", "origin", "user-agent", "cookie", "authorization",
  "accept", "accept-language",
  "sec-fetch-dest", "sec-fetch-mode", "sec-fetch-site",
  "sec-ch-ua", "sec-ch-ua-mobile", "sec-ch-ua-platform",
];
// Range is captured for fragment detection but NOT forwarded as a job header
// (the daemon sets its own ranges).
const CAPTURE_HEADERS = [...RELEVANT_HEADERS, "range"];

// "extraHeaders" is a Chrome-only opt-in for seeing Cookie/Referer/etc. Firefox
// exposes those headers without it and *rejects* the unknown spec value.
const IS_FIREFOX =
  typeof navigator !== "undefined" && /firefox/i.test(navigator.userAgent || "");
const reqSpec = IS_FIREFOX ? ["requestHeaders"] : ["requestHeaders", "extraHeaders"];
const respSpec = IS_FIREFOX ? ["responseHeaders"] : ["responseHeaders", "extraHeaders"];

chrome.webRequest.onSendHeaders.addListener(
  (details) => {
    if (details.tabId < 0) return;
    const h = {};
    for (const { name, value } of details.requestHeaders || []) {
      if (CAPTURE_HEADERS.includes(name.toLowerCase())) h[name] = value;
    }
    pendingHeaders.set(details.requestId, h);
    if (pendingHeaders.size > 500) {
      pendingHeaders.delete(pendingHeaders.keys().next().value);
    }
  },
  { urls: ["<all_urls>"] },
  reqSpec
);

chrome.webRequest.onHeadersReceived.addListener(
  (details) => {
    if (details.tabId < 0) return;
    let contentType, contentLength, disposition, contentRange;
    for (const { name, value } of details.responseHeaders || []) {
      const n = name.toLowerCase();
      if (n === "content-type") contentType = value;
      else if (n === "content-length") contentLength = parseInt(value, 10);
      else if (n === "content-disposition") disposition = value;
      else if (n === "content-range") contentRange = value;
    }

    const reqHeaders = pendingHeaders.get(details.requestId) || {};
    pendingHeaders.delete(details.requestId);
    const reqRange = reqHeaders.Range || reqHeaders.range || "";
    // Never forward Range as a job header.
    const jobHeaders = {};
    for (const k in reqHeaders) if (k.toLowerCase() !== "range") jobHeaders[k] = reqHeaders[k];

    // Byte-range slice of a larger media file (Instagram/Facebook, and any CDN
    // that streams a progressive MP4 in sequential Range requests). Group the
    // slices so the popup shows one video, not 13 chunks.
    const frag = parseFragment(details.url, contentType, details.statusCode, contentRange, reqRange);
    if (frag) {
      const g = fragGroups.get(details.tabId) || new Map();
      const grp = g.get(frag.key) || {
        key: frag.key, category: "video", headers: jobHeaders,
        title: "", total: 0, frags: new Map(), ts: Date.now(),
      };
      grp.frags.set(frag.start, { url: details.url, start: frag.start, end: frag.end });
      if (frag.total) grp.total = frag.total;
      grp.ts = Date.now();
      g.set(frag.key, grp);
      fragGroups.set(details.tabId, g);
      updateBadge(details.tabId);
      return;
    }

    const hit = MacDMClassify.classify({ url: details.url, contentType, contentLength, disposition });
    if (!hit) return;

    const tabMap = caught.get(details.tabId) || new Map();
    const keyUrl = hit.kind === "http" ? hit.url : hit.url.split("#")[0];
    if (!tabMap.has(keyUrl)) {
      tabMap.set(keyUrl, {
        url: hit.url,
        kind: hit.kind,
        category: hit.category || "other",
        contentType: hit.contentType,
        size: hit.contentLength,
        disposition: disposition || "",
        headers: reqHeaders,
        ts: Date.now(),
      });
      caught.set(details.tabId, tabMap);
      updateBadge(details.tabId);
    }
  },
  { urls: ["<all_urls>"] },
  respSpec
);

chrome.tabs.onRemoved.addListener((tabId) => { caught.delete(tabId); fragGroups.delete(tabId); });
chrome.tabs.onUpdated.addListener((tabId, info) => {
  if (info.status === "loading" && info.url) {
    caught.delete(tabId);
    fragGroups.delete(tabId);
    updateBadge(tabId);
  }
});

function updateBadge(tabId) {
  const n = (caught.get(tabId)?.size || 0) + (fragGroups.get(tabId)?.size || 0);
  chrome.action.setBadgeBackgroundColor({ color: "#0a84ff" });
  chrome.action.setBadgeText({ tabId, text: n ? String(n) : "" });
}

// bestCaughtMedia returns the most useful sniffed media item for a tab: prefer a
// progressive video by size, then any video, then audio. Used when there is no
// page URL yt-dlp can resolve (or the site's yt-dlp extractor is unreliable).
function bestCaughtMedia(tabId) {
  const items = [...(caught.get(tabId)?.values() || [])];
  const rank = (it) => (it.category === "video" ? 2 : it.category === "audio" ? 1 : 0);
  let best = null;
  for (const it of items) {
    if (rank(it) === 0) continue;
    if (!best ||
        rank(it) > rank(best) ||
        (rank(it) === rank(best) && (it.size || 0) > (best.size || 0))) {
      best = it;
    }
  }
  return best;
}

// yt-dlp's extractors for these hosts are frequently broken between releases —
// a directly sniffed progressive URL is more reliable there.
const YTDLP_UNRELIABLE = /(^|\.)(tiktok\.com)$/i;

// Collapse a fragment group into a caught-item-like record for the popup.
function groupToItem(grp) {
  const frags = [...grp.frags.values()].sort((a, b) => a.start - b.start);
  const covered = frags.reduce((s, f) => s + (f.end - f.start + 1), 0);
  // If every slice was fetched from the SAME url (range travelled in the Range
  // header, server honoured it with 206), the daemon can just range-download
  // that url itself — no need to replay the browser's slices.
  const uniq = new Set(frags.map((f) => f.url));
  const sameURL = uniq.size === 1;
  return {
    fragmentGroup: true,
    url: sameURL ? frags[0].url : grp.key,
    kind: "http",
    category: "video",
    size: grp.total || covered,
    count: frags.length,
    fragments: sameURL ? [] : frags,
    sameURL,
    headers: grp.headers,
    ts: grp.ts,
  };
}

// --- native messaging ---

function sendToNative(message) {
  return new Promise((resolve) => {
    try {
      chrome.runtime.sendNativeMessage(NATIVE_HOST, message, (resp) => {
        if (chrome.runtime.lastError) {
          resolve({ ok: false, error: chrome.runtime.lastError.message });
        } else {
          resolve(resp || { ok: false, error: "empty response" });
        }
      });
    } catch (e) {
      resolve({ ok: false, error: String(e) });
    }
  });
}

async function startDownload({ url, headers, referer, title, conns, fragments }) {
  return sendToNative({
    type: "download",
    url,
    headers: headers || {},
    referer: referer || "",
    title: title || "",
    conns: conns || 0,
    fragments: fragments || [],
  });
}

// --- message API for popup / content script ---

chrome.runtime.onMessage.addListener((msg, sender, sendResponse) => {
  (async () => {
    switch (msg.type) {
      case "getCaught": {
        const tabId = msg.tabId ?? sender.tab?.id;
        const groups = [...(fragGroups.get(tabId)?.values() || [])].map(groupToItem);
        const items = [...(caught.get(tabId)?.values() || [])];
        sendResponse({ items: [...groups, ...items].sort((a, b) => b.ts - a.ts) });
        break;
      }
      case "daemonStatus": {
        const r = await sendToNative({ type: "ping" });
        sendResponse({ ok: !!r.ok });
        break;
      }
      case "getTakeover": {
        const r = await chrome.storage.local.get("takeOverDownloads");
        sendResponse({ on: !!r.takeOverDownloads });
        break;
      }
      case "setTakeover": {
        await chrome.storage.local.set({ takeOverDownloads: !!msg.on });
        sendResponse({ ok: true });
        break;
      }
      case "download": {
        sendResponse(await startDownload(msg.payload));
        break;
      }
      case "downloadPage": {
        const tab = sender.tab;
        const tabHost = (() => { try { return new URL(tab?.url || "").hostname; } catch { return ""; } })();

        // yt-dlp resolves a real permalink to the FULL muxed video+audio at best
        // quality — prefer it, EXCEPT on hosts whose yt-dlp extractor is
        // currently flaky (TikTok). There, use the progressive URL the content
        // script pulled from the page JSON (msg.ttURL) — the sniffed <video>
        // URL is a session-locked DASH stream that 403s. Replay the browser's
        // captured session headers (Cookie / ttwid / UA) with it.
        const sniffed = bestCaughtMedia(tab?.id);
        if (YTDLP_UNRELIABLE.test(tabHost)) {
          const referer = tab?.url || "https://www.tiktok.com/";
          const cookie = sniffed?.headers &&
            (sniffed.headers.Cookie || sniffed.headers.cookie);
          const base = {
            "Sec-Fetch-Dest": "video",
            "Sec-Fetch-Mode": "no-cors",
            "Sec-Fetch-Site": "same-site",
            "Accept": "*/*",
            "Referer": referer,
          };
          // 1. The progressive URL tiktok-main.js read from the page's own state
          //    (playAddr / bitrateInfo). This is what Neat DM / yt-dlp use — it
          //    downloads with just a Referer for public videos.
          if (msg.ttURL) {
            sendResponse(await startDownload({
              url: msg.ttURL, referer,
              title: msg.title || tab?.title,
              headers: Object.assign({}, cookie ? { Cookie: cookie } : {}, base),
            }));
            break;
          }
          // 2. On a real video page, hand the permalink to yt-dlp (nightly).
          const permalink = /\/@[\w.-]+\/(video|photo)\/\d+/.test(new URL(referer).pathname)
            ? referer : (isExtractableURL(msg.url) ? msg.url : null);
          if (permalink) {
            sendResponse(await startDownload({
              url: permalink, referer,
              title: msg.title || tab?.title,
            }));
            break;
          }
          // 3. Fall back to the caught <video> stream URL — needs the tiktok.com
          //    cookies (httpOnly ttwid etc.) or it 403s.
          if (cookie) {
            sendResponse(await startDownload({
              url: sniffed.url, referer,
              title: msg.title || tab?.title,
              headers: Object.assign({}, sniffed.headers, base),
            }));
            break;
          }
          sendResponse({ ok: false, error: "open the video on its own page, then try again" });
          break;
        }

        const pageURL = isExtractableURL(msg.url) ? msg.url
                      : isExtractableURL(tab?.url) ? tab.url
                      : null;
        if (pageURL) {
          sendResponse(await startDownload({
            url: pageURL,
            referer: tab?.url,
            title: msg.title || tab?.title,
          }));
          break;
        }

        // No page URL — a directly sniffed media stream is the next best thing.
        if (sniffed) {
          sendResponse(await startDownload({
            url: sniffed.url,
            headers: sniffed.headers,
            referer: tab?.url,
            title: msg.title || tab?.title,
          }));
          break;
        }

        const g = fragGroups.get(tab?.id);
        if (g && g.size) {
          // Pick the biggest group by total bytes — that is the video track.
          let best = null, bestBytes = -1;
          for (const grp of g.values()) {
            let b = 0;
            for (const f of grp.frags.values()) b += f.end - f.start + 1;
            if (b > bestBytes) { best = grp; bestBytes = b; }
          }
          if (best && best.frags.size >= 1) {
            const item = groupToItem(best);
            sendResponse(await startDownload({
              url: item.url,
              headers: item.headers,
              referer: tab?.url,
              title: msg.title || tab?.title,
              fragments: item.fragments,
            }));
            break;
          }
        }
        // Nothing usable: a bare feed URL (tiktok.com/) or an expiring CDN chunk.
        // Sending either to yt-dlp just yields "Unsupported URL" / 403.
        if (!isExtractableURL(msg.url)) {
          sendResponse({ ok: false, error: "open the video on its own page first, then use MacDM" });
          break;
        }
        sendResponse(await startDownload({
          url: msg.url,
          referer: tab?.url,
          title: msg.title || tab?.title,
        }));
        break;
      }
      default:
        sendResponse({ ok: false, error: "unknown message" });
    }
  })();
  return true; // async
});

// --- context menu ---

chrome.runtime.onInstalled.addListener(() => {
  chrome.contextMenus.removeAll(() => {
    chrome.contextMenus.create({
      id: "macdm-link",
      title: "Download with MacDM",
      contexts: ["link", "video", "audio", "image"],
    });
    chrome.contextMenus.create({
      id: "macdm-page",
      title: "Send this page to MacDM (extract video)",
      contexts: ["page"],
    });
  });
});

chrome.contextMenus.onClicked.addListener(async (info, tab) => {
  if (info.menuItemId === "macdm-link") {
    const url = info.linkUrl || info.srcUrl;
    if (url) await startDownload({ url, referer: tab?.url, title: tab?.title });
  } else if (info.menuItemId === "macdm-page") {
    await startDownload({ url: tab.url, referer: tab.url, title: tab.title });
  }
});

// --- opt-in: take over browser downloads (default OFF) ---

let takeOver = false;
chrome.storage.local.get("takeOverDownloads", (r) => { takeOver = !!r.takeOverDownloads; });
chrome.storage.onChanged.addListener((c) => {
  if (c.takeOverDownloads) takeOver = !!c.takeOverDownloads.newValue;
});
chrome.downloads.onCreated.addListener((item) => {
  if (!takeOver || !item.url || item.url.startsWith("blob:") || item.url.startsWith("data:")) return;
  chrome.downloads.cancel(item.id, () => chrome.downloads.erase({ id: item.id }));
  startDownload({ url: item.finalUrl || item.url, referer: item.referrer || "" });
});
