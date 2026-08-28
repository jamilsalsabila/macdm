// Runs in the PAGE context (content_scripts world: "MAIN") on tiktok.com.
//
// TikTok's real progressive video URLs (playAddr / downloadAddr / bitrateInfo)
// live only on window globals — __UNIVERSAL_DATA_FOR_REHYDRATION__ /
// __$UNIVERSAL_DATA$__ / SIGI_STATE — which the normal (isolated) content script
// cannot read. The <video> element only ever gets a session-locked DASH URL that
// 403s outside the browser. This grabs the good URLs and hands them to the
// isolated script via window.postMessage. This is the "Neat Download Manager"
// approach: use the page's own declared source, not the player's stream URL.
(function () {
  "use strict";

  function videoNode() {
    for (const root of [
      window.__UNIVERSAL_DATA_FOR_REHYDRATION__,
      window["__$UNIVERSAL_DATA$__"],
    ]) {
      const scope = root && root.__DEFAULT_SCOPE__;
      const v = scope && scope["webapp.video-detail"] &&
        scope["webapp.video-detail"].itemInfo &&
        scope["webapp.video-detail"].itemInfo.itemStruct &&
        scope["webapp.video-detail"].itemInfo.itemStruct.video;
      if (v) return v;
    }
    // SIGI_STATE (older layout): ItemModule[<id>].video
    const sigi = window.SIGI_STATE;
    if (sigi && sigi.ItemModule) {
      for (const k in sigi.ItemModule) {
        if (sigi.ItemModule[k] && sigi.ItemModule[k].video) return sigi.ItemModule[k].video;
      }
    }
    return null;
  }

  // Ordered best → worst.
  function orderedURLs() {
    const v = videoNode();
    const out = [];
    const push = (u) => { if (typeof u === "string" && /^https?:\/\//.test(u) && !out.includes(u)) out.push(u); };

    if (v) {
      const bitrates = (v.bitrateInfo || []).slice().sort((a, b) => (b.Bitrate || 0) - (a.Bitrate || 0));
      for (const b of bitrates) {
        const list = (b.PlayAddr && (b.PlayAddr.UrlList || b.PlayAddr.url_list)) || [];
        list.forEach(push);
      }
      push(v.playAddr || v.play_addr);
      push(v.downloadAddr || v.download_addr);
    }
    if (out.length) return out;

    // Fallback: deep-walk whatever state we can find.
    const seen = new Set();
    const KEY = /^(playAddr|play_addr|downloadAddr|download_addr)$/;
    (function walk(o, d) {
      if (!o || typeof o !== "object" || d > 9 || seen.has(o)) return;
      seen.add(o);
      for (const k in o) {
        let val; try { val = o[k]; } catch { continue; }
        if (typeof val === "string" && KEY.test(k)) push(val);
        else if (val && typeof val === "object") {
          const list = val.UrlList || val.url_list;
          if ((k === "PlayAddr" || k === "play_addr") && Array.isArray(list)) list.forEach(push);
          walk(val, d + 1);
        }
      }
    })(window.__UNIVERSAL_DATA_FOR_REHYDRATION__ || window["__$UNIVERSAL_DATA$__"] || window.SIGI_STATE, 0);
    return out;
  }

  function announce() {
    const urls = orderedURLs();
    if (urls.length) {
      window.postMessage({ __macdm_tiktok: 1, urls: urls, page: location.href }, location.origin);
    }
  }

  announce();
  // TikTok hydrates after load and on in-app navigation; keep trying briefly.
  let n = 0;
  const iv = setInterval(() => { announce(); if (++n >= 15) clearInterval(iv); }, 700);
  addEventListener("popstate", announce);
})();
