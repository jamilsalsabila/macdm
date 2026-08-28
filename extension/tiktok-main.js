// Runs in the PAGE context (content_scripts world: "MAIN") on tiktok.com.
//
// TikTok's real progressive video URLs (playAddr / downloadAddr / bitrateInfo)
// live only on window globals — __UNIVERSAL_DATA_FOR_REHYDRATION__ /
// __$UNIVERSAL_DATA$__ / SIGI_STATE — which the isolated content script cannot
// read. The <video> element only ever gets a session-locked DASH URL that 403s.
// This grabs the good URLs for the *currently open* clip (the feed reuses one
// URL and scrolls the address bar through /@user/video/<id>) and posts them to
// the isolated script. This is the "Neat Download Manager" approach.
(function () {
  "use strict";

  function currentId() {
    const m = location.pathname.match(/\/(?:video|photo)\/(\d+)/);
    return m ? m[1] : null;
  }

  // Find the video object for a specific item id anywhere in the state tree.
  function videoForId(id) {
    const roots = [
      window.SIGI_STATE,
      window.__UNIVERSAL_DATA_FOR_REHYDRATION__,
      window["__$UNIVERSAL_DATA$__"],
    ].filter(Boolean);

    // SIGI_STATE.ItemModule[id].video — the direct hit when present.
    for (const r of roots) {
      if (r.ItemModule && r.ItemModule[id] && r.ItemModule[id].video) return r.ItemModule[id].video;
    }
    // Otherwise walk for an object { id: "<id>", video: {...} }.
    const seen = new Set();
    let hit = null;
    (function walk(o, d) {
      if (hit || !o || typeof o !== "object" || d > 10 || seen.has(o)) return;
      seen.add(o);
      if (o.video && (o.id === id || o.awemeId === id || o.aweme_id === id)) { hit = o.video; return; }
      for (const k in o) {
        let v; try { v = o[k]; } catch { continue; }
        if (v && typeof v === "object") walk(v, d + 1);
      }
    })(roots[0], 0);
    if (hit) return hit;

    // Last resort: the single-video-page shape.
    for (const r of roots) {
      const v = r.__DEFAULT_SCOPE__ &&
        r.__DEFAULT_SCOPE__["webapp.video-detail"] &&
        r.__DEFAULT_SCOPE__["webapp.video-detail"].itemInfo &&
        r.__DEFAULT_SCOPE__["webapp.video-detail"].itemInfo.itemStruct &&
        r.__DEFAULT_SCOPE__["webapp.video-detail"].itemInfo.itemStruct.video;
      if (v) return v;
    }
    return null;
  }

  function urlsFor(v) {
    const out = [];
    const push = (u) => {
      if (typeof u === "string" && /^https?:\/\//.test(u) && !out.includes(u)) out.push(u);
    };
    if (v) {
      (v.bitrateInfo || []).slice()
        .sort((a, b) => (b.Bitrate || 0) - (a.Bitrate || 0))
        .forEach((b) => ((b.PlayAddr && (b.PlayAddr.UrlList || b.PlayAddr.url_list)) || []).forEach(push));
      push(v.playAddr || v.play_addr);
      push(v.downloadAddr || v.download_addr);
    }
    return out;
  }

  let lastKey = "";
  function announce() {
    const id = currentId();
    const urls = urlsFor(videoForId(id));
    const key = id + "|" + urls.join(",");
    if (!urls.length || key === lastKey) return;
    lastKey = key;
    window.postMessage({ __macdm_tiktok: 1, id: id, urls: urls, page: location.href }, location.origin);
  }

  // TikTok hydrates late and swaps the clip on scroll without a full navigation.
  setInterval(announce, 600);
  addEventListener("popstate", announce);
  announce();
})();
