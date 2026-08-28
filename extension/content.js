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

  const POST_LINK = /\/(p|reel|reels|tv|watch|shorts|video|status|clip|v)\/[\w-]/i;

  function postLinkNear(video) {
    // Walk up to the post/article container, then take the first permalink.
    let el = video;
    for (let i = 0; i < 12 && el; i++, el = el.parentElement) {
      const scope = el.closest ? el.closest("article, [role='article'], [data-testid='tweet'], .x1qjc9v5, section") : null;
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
    return location.href;
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

    const done = (txt, ok) => {
      btn.textContent = txt;
      // On success keep the button disabled for 6s so a double/triple click
      // (dialog behind the browser) can't queue duplicates.
      const cool = ok ? 6000 : 800;
      setTimeout(() => { busy = false; btn.textContent = "⬇ MacDM"; }, cool);
    };
    const timeout = setTimeout(() => done("✗ no daemon", false), 12000);
    try {
      chrome.runtime.sendMessage(
        { type: "downloadPage", url, title: document.title },
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
