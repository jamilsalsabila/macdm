// Content script: a single floating "⬇ MacDM" button that appears over the
// video you are hovering, IDM-style. Clicking it sends the best URL for that
// video to MacDM's extractor.
//
// One button is shared across the page (not one per <video>) so a feed of
// thumbnail previews does not sprout dozens of buttons.

// Runs in every frame — many streaming sites put the player in an <iframe>, so
// a top-frame-only button would never appear over the video. Each frame draws
// its own button positioned within that frame.
(function () {
  const MIN_W = 240;
  const MIN_H = 135;

  const btn = document.createElement("button");
  btn.textContent = "⬇ MacDM";
  Object.assign(btn.style, {
    position: "absolute",
    zIndex: "2147483647",
    font: "600 12px -apple-system, system-ui, sans-serif",
    color: "#fff",
    background: "rgba(10,132,255,0.92)",
    border: "0",
    borderRadius: "6px",
    padding: "5px 9px",
    cursor: "pointer",
    boxShadow: "0 1px 6px rgba(0,0,0,0.35)",
    opacity: "0",
    pointerEvents: "none",
    transition: "opacity .12s",
  });
  let attached = false;
  let currentVideo = null;
  let hideTimer = null;
  let busy = false;

  function ensureAttached() {
    if (!attached && document.body) {
      document.body.appendChild(btn);
      attached = true;
    }
  }

  function hasRealPath(u) {
    try {
      const p = new URL(u);
      return p.pathname.length > 1 || p.search.length > 1;
    } catch { return false; }
  }

  const POST_LINK = /\/(p|reel|reels|tv|watch|shorts|video|photo|status|clip|v)\/[\w-]/i;
  const POST_CONTAINERS =
    "article, [role='article'], [data-testid='tweet']," +
    "[data-e2e='recommend-list-item-container'], [data-e2e='video-detail']," +
    "[class*='DivItemContainer'], [class*='DivVideoFeed'], .x1qjc9v5, section";

  function postLinkNear(video) {
    // Walk up to the post/article container, then take the first permalink.
    let el = video;
    for (let i = 0; i < 14 && el; i++, el = el.parentElement) {
      const scope = el.closest ? el.closest(POST_CONTAINERS) : null;
      const root = scope || el;
      for (const a of root.querySelectorAll ? root.querySelectorAll("a[href]") : []) {
        if (POST_LINK.test(a.getAttribute("href") || "")) return a.href;
      }
      if (scope) break;
    }
    return null;
  }

  function bestURL(video) {
    const inFrame = window.top !== window;
    const host = location.hostname;
    const social = /(instagram\.com|facebook\.com|twitter\.com|x\.com|tiktok\.com|threads\.net)$/i.test(host);

    // Already on a permalink page → use it.
    if (POST_LINK.test(location.pathname)) return location.href;

    // Social feed: the post permalink lives in the DOM near the video.
    if (social) {
      const p = postLinkNear(video);
      if (p) return p;
    }

    const canon =
      document.querySelector('link[rel="canonical"]')?.href ||
      document.querySelector('meta[property="og:url"]')?.content;
    if (canon && hasRealPath(canon) &&
        (POST_LINK.test(new URL(canon).pathname) || /embed-|[?&]v=/.test(canon))) {
      return canon;
    }

    // Embed page / player frame with a real path.
    if (hasRealPath(location.href) &&
        (POST_LINK.test(location.pathname) || /watch\?|[?&]v=|embed-|\.html$/.test(location.href) ||
         (inFrame && !social))) {
      return location.href;
    }

    // A thumbnail/link on a listing.
    let el = video;
    for (let i = 0; i < 8 && el; i++, el = el.parentElement) {
      const a = el.closest && el.closest("a[href]");
      if (a && POST_LINK.test(a.getAttribute("href") || "")) return a.href;
    }

    if (inFrame && document.referrer && hasRealPath(document.referrer)) return document.referrer;

    // On a social feed with no permalink found, a bare origin (tiktok.com/,
    // instagram.com/) is useless to the extractor — signal "can't" instead.
    if (social && !hasRealPath(location.href)) return null;
    return location.href;
  }

  // TikTok's <video> plays a session-locked DASH URL (chain_token / btag) that
  // 403s outside the browser. The page embeds the real progressive URL in a
  // JSON <script> — that's what Neat DM / yt-dlp use. Read it straight from the
  // DOM (works in the content-script world; no page-context access needed).
  function deepFindURL(obj, keys, depth) {
    if (!obj || typeof obj !== "object" || depth > 9) return null;
    const good = (s) =>
      typeof s === "string" && /^https?:\/\/[^ ]+\/video\//.test(s) && !/chain_token/.test(s);
    for (const k of keys) {
      const v = obj[k];
      if (good(v)) return v;
      if (v && typeof v === "object") {
        const list = v.UrlList || v.url_list;
        if (Array.isArray(list)) {
          const hit = list.find(good) || [...list].reverse().find((s) => /^https?:/.test(s));
          if (hit) return hit;
        }
      }
    }
    for (const v of Array.isArray(obj) ? obj : Object.values(obj)) {
      const r = deepFindURL(v, keys, depth + 1);
      if (r) return r;
    }
    return null;
  }

  // tiktok-main.js runs in the PAGE context and posts the real progressive URLs
  // (playAddr / downloadAddr / bitrateInfo) here — the isolated content script
  // can't reach those window globals itself.
  let tiktokURLs = [];
  window.addEventListener("message", (e) => {
    if (e.source === window && e.data && e.data.__macdm_tiktok && Array.isArray(e.data.urls)) {
      tiktokURLs = e.data.urls;
    }
  });

  function tiktokVideoURL(video) {
    // 1. THIS video's own src. On a video page it's www.tiktok.com/aweme/v1/play/
    //    ?item_id=… which mints a fresh CDN redirect every request (no expiry)
    //    and is unambiguously the hovered clip.
    const src = video?.currentSrc || video?.src || "";
    if (/^https?:/.test(src) && /aweme\/v1\/play|\/video\/tos\//.test(src)) return src;

    // 2. The permalink for the hovered video — yt-dlp resolves it to the right
    //    clip even on the feed.
    const p = postLinkNear(video);
    if (p && /\/@[\w.-]+\/(video|photo)\/\d+/.test(p)) return p;

    // 3. Whatever tiktok-main.js read from the page state (single-video pages).
    const fromPage = tiktokURLs.find((u) => !/chain_token|[?&]tk=/.test(u)) || tiktokURLs[0];
    if (fromPage) return fromPage;

    // 4. Legacy readable <script> layouts.
    for (const id of ["__UNIVERSAL_DATA_FOR_REHYDRATION__", "SIGI_STATE"]) {
      const tag = document.getElementById(id);
      if (!tag) continue;
      let data;
      try { data = JSON.parse(tag.textContent); } catch { continue; }
      const u = deepFindURL(data, ["playAddr", "play_addr", "downloadAddr", "download_addr", "PlayAddr", "bitrateInfo"], 0);
      if (u) return u;
    }
    return null;
  }

  function place(video) {
    const r = video.getBoundingClientRect();
    if (r.width < MIN_W || r.height < MIN_H) {
      btn.style.opacity = "0";
      btn.style.pointerEvents = "none";
      return false;
    }
    btn.style.top = window.scrollY + r.top + 10 + "px";
    btn.style.left = window.scrollX + r.left + 10 + "px";
    btn.style.opacity = "1";
    btn.style.pointerEvents = "auto";
    return true;
  }

  function showFor(v) {
    ensureAttached();
    clearTimeout(hideTimer);
    currentVideo = v;
    if (!busy) btn.textContent = "⬇ MacDM";
    place(v);
  }
  function scheduleHide() {
    hideTimer = setTimeout(() => {
      btn.style.opacity = "0";
      btn.style.pointerEvents = "none";
    }, 600);
  }

  // Instagram / YouTube / TikTok stack transparent click-catchers ON TOP of the
  // <video>, so mouseenter never reaches the element. Track the pointer at the
  // document level instead and hit-test the whole element stack for a video (or
  // a box that overlaps one).
  let lastMove = 0;
  function videoAtPoint(x, y) {
    const stack = document.elementsFromPoint(x, y);
    for (const el of stack) {
      if (el === btn) continue;
      if (el.tagName === "VIDEO") return el;
    }
    // No video directly in the stack — check whether the hovered box contains
    // one (overlay exactly covering the player).
    for (const el of stack.slice(0, 4)) {
      const v = el.querySelector && el.querySelector("video");
      if (v) {
        const r = v.getBoundingClientRect();
        if (x >= r.left && x <= r.right && y >= r.top && y <= r.bottom) return v;
      }
    }
    return null;
  }
  window.addEventListener("pointermove", (e) => {
    const now = Date.now();
    if (now - lastMove < 120) return;
    lastMove = now;
    const v = videoAtPoint(e.clientX, e.clientY);
    if (v) showFor(v);
    else if (currentVideo && btn.style.opacity === "1") {
      // still hovering the button?
      const br = btn.getBoundingClientRect();
      const onBtn = e.clientX >= br.left && e.clientX <= br.right &&
                    e.clientY >= br.top && e.clientY <= br.bottom;
      if (!onBtn) scheduleHide();
    }
  }, { passive: true, capture: true });

  btn.addEventListener("mouseenter", () => clearTimeout(hideTimer));
  btn.addEventListener("mouseleave", scheduleHide);
  btn.addEventListener("click", (e) => {
    e.preventDefault();
    e.stopPropagation();
    if (busy || !currentVideo) return;
    busy = true;
    btn.textContent = "sending…";
    const url = bestURL(currentVideo);
    const ttURL = /(^|\.)tiktok\.com$/i.test(location.hostname) ? tiktokVideoURL(currentVideo) : null;

    const done = (txt, ok) => {
      btn.textContent = txt;
      // On success keep the button disabled for 6s so a double/triple click
      // (dialog behind the browser) can't queue duplicates.
      const cool = ok ? 6000 : 2500;
      setTimeout(() => { busy = false; btn.textContent = "⬇ MacDM"; }, cool);
    };

    if (!url && !ttURL) { done("✗ open the video first", false); return; }

    const timeout = setTimeout(() => done("✗ no daemon", false), 12000);
    try {
      chrome.runtime.sendMessage(
        { type: "downloadPage", url: url || location.href, ttURL, title: document.title },
        (resp) => {
          clearTimeout(timeout);
          if (chrome.runtime.lastError) { done("✗ " + chrome.runtime.lastError.message, false); return; }
          const ok = !!(resp && resp.ok);
          done(ok ? "✓ queued" : "✗ " + ((resp && resp.error) || "failed"), ok);
        }
      );
    } catch (err) {
      clearTimeout(timeout);
      done("✗ " + err, false);
    }
  });

  window.addEventListener("scroll", () => {
    if (currentVideo && btn.style.opacity === "1") place(currentVideo);
  }, { passive: true });
})();
