function fmtSize(n) {
  if (!n) return "";
  const u = ["B", "KB", "MB", "GB"];
  let i = 0;
  while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
  return `${n.toFixed(i ? 1 : 0)} ${u[i]}`;
}

async function activeTab() {
  const [tab] = await chrome.tabs.query({ active: true, currentWindow: true });
  return tab;
}

function send(type, extra = {}) {
  return new Promise((res) => {
    let done = false;
    const t = setTimeout(() => { if (!done) { done = true; res(null); } }, 12000);
    try {
      chrome.runtime.sendMessage({ type, ...extra }, (r) => {
        if (done) return;
        done = true;
        clearTimeout(t);
        res(chrome.runtime.lastError ? null : r);
      });
    } catch {
      if (!done) { done = true; clearTimeout(t); res(null); }
    }
  });
}

async function refresh() {
  const tab = await activeTab();

  const st = await send("daemonStatus");
  document.getElementById("dot").className = "dot " + (st && st.ok ? "up" : "down");
  document.getElementById("stat").textContent = st && st.ok ? "daemon connected" : "daemon offline";

  const { items = [] } = await send("getCaught", { tabId: tab.id });
  const list = document.getElementById("list");
  if (!items.length) {
    list.innerHTML = '<div class="empty">Nothing caught on this tab yet.<br>Play a video or start a download.</div>';
    return;
  }
  list.innerHTML = "";
  const order = ["video", "audio", "archive", "document", "other"];
  const groups = {};
  for (const it of items) (groups[it.category || "other"] ||= []).push(it);

  for (const cat of order) {
    if (!groups[cat]) continue;
    const h = document.createElement("div");
    h.className = "group";
    h.textContent = cat;
    list.appendChild(h);
    for (const it of groups[cat]) {
      const div = document.createElement("div");
      div.className = "item";
      const label = it.fragmentGroup
        ? (it.sameURL
            ? `video${it.size ? " · " + fmtSize(it.size) : ""}`
            : `video · ${it.count} fragments${it.size ? " · " + fmtSize(it.size) : ""}`)
        : `${it.kind}${it.size ? " · " + fmtSize(it.size) : ""}`;
      // Built with textContent, never innerHTML: it.url comes off the network.
      // Chrome percent-encodes < and > in a URL's path/query so today's input is
      // inert, but this popup can reach the native host — it must not depend on
      // another layer's escaping to stay safe.
      const k = document.createElement("div");
      k.className = "k";
      k.textContent = label;
      const u = document.createElement("div");
      u.className = "u";
      u.textContent = it.url;
      const dlBtn = document.createElement("button");
      dlBtn.textContent = "Download";
      div.append(k, u, dlBtn);
      dlBtn.addEventListener("click", async (e) => {
        e.target.textContent = "queuing…";
        const payload = { url: it.url, headers: it.headers, referer: tab.url, title: tab.title };
        if (it.fragmentGroup) payload.fragments = it.fragments;
        const r = await send("download", { payload });
        e.target.textContent = r && r.ok ? "✓ queued" : "✗ " + ((r && r.error) || "failed");
      });
      list.appendChild(div);
    }
  }
}

const takeoverBox = document.getElementById("takeover");
send("getTakeover").then((r) => { takeoverBox.checked = !!(r && r.on); });
takeoverBox.addEventListener("change", () => send("setTakeover", { on: takeoverBox.checked }));

document.getElementById("pageDl").addEventListener("click", async (e) => {
  const tab = await activeTab();
  e.target.textContent = "sending…";
  const r = await send("download", {
    payload: { url: tab.url, referer: tab.url, title: tab.title },
  });
  e.target.textContent = r && r.ok ? "✓ queued" : "✗ " + ((r && r.error) || "failed");
  setTimeout(() => (e.target.textContent = "Extract video from this page"), 2500);
});

refresh();
setInterval(refresh, 2000);
