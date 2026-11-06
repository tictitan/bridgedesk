"use strict";
/* BridgeDesk dashboard — vanilla JS, also runs inside the Android WebView. */

const App = {
  key: null,
  device: "local",
  devices: [],
  ws: null,
  rpcId: 1,
  pending: new Map(),
  subs: new Map(),      // sid -> handler
  connected: false,
};

/* ---------- helpers ---------- */
const $ = (s) => document.querySelector(s);
const el = (s) => document.querySelector(s);
function toast(msg, isErr) {
  const t = $("#toast");
  t.textContent = msg; t.className = "show" + (isErr ? " err" : "");
  clearTimeout(toast._t); toast._t = setTimeout(() => (t.className = ""), 2600);
}
function fmtSize(n) {
  if (n == null) return "";
  const u = ["B", "KB", "MB", "GB", "TB"]; let i = 0;
  while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
  return (i === 0 ? n : n.toFixed(1)) + " " + u[i];
}
function b64ToBytes(b64) {
  const bin = atob(b64); const a = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) a[i] = bin.charCodeAt(i);
  return a;
}
function bytesToB64(bytes) {
  let bin = ""; const chunk = 0x8000;
  for (let i = 0; i < bytes.length; i += chunk) bin += String.fromCharCode.apply(null, bytes.subarray(i, i + chunk));
  return btoa(bin);
}
const enc = new TextEncoder(), dec = new TextDecoder();

/* ---------- transport ---------- */
function wsURL() {
  const proto = location.protocol === "https:" ? "wss:" : "ws:";
  return `${proto}//${location.host}/ws?key=${encodeURIComponent(App.key)}`;
}
function connect() {
  const ws = new WebSocket(wsURL());
  App.ws = ws;
  ws.onopen = () => { setStatus(true); refreshDevices(); startDevicePoll(); };
  ws.onclose = () => { setStatus(false); setTimeout(connect, 1500); };
  ws.onerror = () => ws.close();
  ws.onmessage = (ev) => {
    const m = JSON.parse(ev.data);
    if (m.event) { const h = App.subs.get(m.params && m.params.sid); if (h) h(m); return; }
    if (m.id && App.pending.has(m.id)) {
      const { resolve, reject } = App.pending.get(m.id); App.pending.delete(m.id);
      if (m.error) reject(new Error(m.error)); else resolve(m.result);
    }
  };
}
function setStatus(ok) {
  App.connected = ok;
  $("#statusDot").className = "dot " + (ok ? "on" : "off");
  $("#statusTxt").textContent = ok ? "connected" : "reconnecting…";
}
function whenConnected() {
  return new Promise((resolve, reject) => {
    if (App.ws && App.ws.readyState === 1) return resolve();
    let n = 0;
    const t = setInterval(() => {
      if (App.ws && App.ws.readyState === 1) { clearInterval(t); resolve(); }
      else if (++n > 100) { clearInterval(t); reject(new Error("not connected")); }
    }, 100);
  });
}
function rpc(method, params, device) {
  return new Promise((resolve, reject) => {
    if (!App.ws || App.ws.readyState !== 1) return reject(new Error("not connected"));
    const id = App.rpcId++;
    App.pending.set(id, { resolve, reject });
    App.ws.send(JSON.stringify({ id, action: "rpc", device: device || App.device, method, params: params || {} }));
    setTimeout(() => { if (App.pending.has(id)) { App.pending.delete(id); reject(new Error("timeout")); } }, 600000);
  });
}
function subscribe(sid, handler) {
  App.subs.set(sid, handler);
  App.ws.send(JSON.stringify({ action: "subscribe", device: App.device, sid }));
}
function unsubscribe(sid) { App.subs.delete(sid); }

async function api(path, opts) {
  opts = opts || {};
  opts.headers = Object.assign({ "Authorization": "Bearer " + App.key }, opts.headers || {});
  const r = await fetch(path, opts);
  if (!r.ok) throw new Error(await r.text() || r.status);
  return r;
}

/* ---------- devices ---------- */
async function refreshDevices() {
  try {
    const r = await (await api("/api/devices")).json();
    App.devices = r.devices;
    if (r.self) App.self = r.self;
    const sig = r.devices.map((d) => d.id + (d.online ? "1" : "0") + (d.name)).join("|");
    if (sig === App._devSig) return; // unchanged — skip re-render to avoid flicker
    App._devSig = sig;
    const sel = $("#deviceSel"); const cur = App.device;
    sel.innerHTML = "";
    for (const d of r.devices) {
      const o = document.createElement("option");
      o.value = d.id;
      const icon = d.local ? "🖥 " : (d.kind === "android" ? "📱 " : (d.kind === "darwin" ? "💻 " : "💻 "));
      o.textContent = icon + d.name + (d.online ? "" : " (offline)") + (d.via ? "  ·  via " + d.via : "");
      sel.appendChild(o);
    }
    if (r.devices.find((d) => d.id === cur)) sel.value = cur; else { App.device = r.self; sel.value = r.self; }
    renderDevices();
  } catch (e) { /* ignore during reconnect */ }
}
function activeDev() { return App.devices.find((d) => d.id === App.device) || {}; }
function switchDevice(id) {
  if (App.device === id) return;
  App.device = id;
  const sel = $("#deviceSel"); if (sel) sel.value = id;
  Term.reset(); Desktop.stop();
  const active = document.querySelector(".page.active");
  if (active) go(active.dataset.page); // re-open the current tab against the new device
}
let devicePollTimer = null;
function startDevicePoll() {
  if (devicePollTimer) return;
  // Keep the fleet list fresh as machines/phones join or leave anywhere in the mesh.
  devicePollTimer = setInterval(() => { if (App.connected) refreshDevices(); }, 4000);
}

/* ---------- navigation ---------- */
const NAV = [
  ["term", "Terminal", '<path d="M4 4h16v16H4z" fill="none" stroke="currentColor" stroke-width="2"/><path d="M7 9l3 3-3 3M13 15h4" stroke="currentColor" stroke-width="2" fill="none" stroke-linecap="round"/>'],
  ["files", "Files", '<path d="M3 7a2 2 0 012-2h4l2 2h8a2 2 0 012 2v8a2 2 0 01-2 2H5a2 2 0 01-2-2z" fill="none" stroke="currentColor" stroke-width="2"/>'],
  ["desktop", "Desktop", '<rect x="3" y="4" width="18" height="12" rx="1.5" fill="none" stroke="currentColor" stroke-width="2"/><path d="M9 20h6M12 16v4" stroke="currentColor" stroke-width="2" stroke-linecap="round"/>'],
  ["clip", "Clipboard", '<rect x="6" y="4" width="12" height="17" rx="2" fill="none" stroke="currentColor" stroke-width="2"/><rect x="9" y="2.5" width="6" height="3.5" rx="1" fill="currentColor"/>'],
  ["notif", "Alerts", '<path d="M6 9a6 6 0 0112 0c0 5 2 6 2 6H4s2-1 2-6z" fill="none" stroke="currentColor" stroke-width="2" stroke-linejoin="round"/><path d="M10 19a2 2 0 004 0" fill="none" stroke="currentColor" stroke-width="2"/>'],
  ["devices", "Devices", '<rect x="3" y="5" width="12" height="9" rx="1" fill="none" stroke="currentColor" stroke-width="2"/><rect x="16" y="8" width="5" height="11" rx="1" fill="none" stroke="currentColor" stroke-width="2"/>'],
  ["settings", "Settings", '<circle cx="12" cy="12" r="3" fill="none" stroke="currentColor" stroke-width="2"/><path d="M12 2v3M12 19v3M2 12h3M19 12h3M5 5l2 2M17 17l2 2M19 5l-2 2M7 17l-2 2" stroke="currentColor" stroke-width="2" stroke-linecap="round"/>'],
];
function buildNav() {
  const nav = $("#nav");
  for (const [id, label, svg] of NAV) {
    const d = document.createElement("div");
    d.className = "tab"; d.dataset.go = id;
    d.innerHTML = `<svg viewBox="0 0 24 24">${svg}</svg><span>${label}</span>`;
    d.onclick = () => go(id);
    nav.appendChild(d);
  }
}
function go(id) {
  document.querySelectorAll("#nav .tab").forEach((t) => t.classList.toggle("active", t.dataset.go === id));
  document.querySelectorAll(".page").forEach((p) => p.classList.toggle("active", p.dataset.page === id));
  if (id === "files") Files.enter();
  if (id === "term") Term.enter();
  if (id === "devices") renderDevices();
  if (id === "notif") Notif.enter();
  if (id === "settings") Settings.load();
  if (id !== "desktop") Desktop.stop();
}

/* ---------- Terminal ---------- */
const Term = {
  term: null, fit: null, sid: null, started: false,
  init() {
    const FitAddon = (window.FitAddon && window.FitAddon.FitAddon) || window.FitAddon;
    this.term = new window.Terminal({ fontSize: 13, fontFamily: 'ui-monospace, Consolas, monospace',
      cursorBlink: true, theme: { background: "#000000", foreground: "#e6edf3" }, scrollback: 5000 });
    this.fit = new FitAddon();
    this.term.loadAddon(this.fit);
    this.term.open($("#term"));
    this.term.onData((d) => { if (this.sid) rpc("term.write", { sid: this.sid, data: btoa(unescape(encodeURIComponent(d))) }); });
    this.term.onResize(({ cols, rows }) => { if (this.sid) rpc("term.resize", { sid: this.sid, cols, rows }); });
    new ResizeObserver(() => this.refit()).observe($("#termWrap"));
    $("#termNew").onclick = () => this.open();
    $("#termClear").onclick = () => this.term.clear();
    $("#termCAD").onclick = () => { if (this.sid) rpc("term.write", { sid: this.sid, data: btoa("\x03") }); };
  },
  refit() { try { this.fit.fit(); } catch (e) {} },
  enter() { if (!this.term) this.init(); this.refit(); if (!this.sid && !this.opening) this.open(); this.term.focus(); },
  async open() {
    if (this.opening) return; this.opening = true;
    if (this.sid) { try { await rpc("term.close", { sid: this.sid }); } catch (e) {} unsubscribe(this.sid); this.sid = null; }
    this.term.reset();
    try {
      await whenConnected();
      const shell = $("#termShell").value;
      this.refit();
      const r = await rpc("term.open", { shell, cols: this.term.cols, rows: this.term.rows });
      this.sid = r.sid;
      subscribe(this.sid, (m) => {
        if (m.event === "term.data") this.term.write(b64ToBytes(m.params.data));
        else if (m.event === "term.exit") { this.term.write(`\r\n\x1b[90m[process exited: ${m.params.code}]\x1b[0m\r\n`); unsubscribe(this.sid); this.sid = null; }
      });
    } catch (e) { this.term.write(`\r\n\x1b[31m${e.message}\x1b[0m\r\n`); }
    this.opening = false;
  },
  reset() { if (this.sid) { unsubscribe(this.sid); this.sid = null; } if (this.term) this.term.reset(); },
};

/* ---------- Files ---------- */
const Files = {
  path: "", roots: [],
  async enter() {
    try {
      this.roots = await rpc("fs.roots");
      const sel = $("#fRoots"); sel.innerHTML = '<option value="">Go to…</option>';
      for (const r of this.roots) { const o = document.createElement("option"); o.value = r.path; o.textContent = r.name; sel.appendChild(o); }
      if (!this.path) this.path = (this.roots[0] && this.roots[0].path) || "";
    } catch (e) {}
    this.list();
  },
  bind() {
    $("#fRefresh").onclick = () => this.list();
    $("#fHome").onclick = () => { this.path = (this.roots[0] && this.roots[0].path) || ""; this.list(); };
    $("#fUp").onclick = () => { this.path = parentPath(this.path); this.list(); };
    $("#fRoots").onchange = (e) => { if (e.target.value) { this.path = e.target.value; this.list(); e.target.value = ""; } };
    $("#fMkdir").onclick = async () => { const n = prompt("New folder name:"); if (!n) return; await rpc("fs.mkdir", { path: join(this.path, n) }).catch(err => toast(err.message, true)); this.list(); };
    $("#fUpload").onclick = () => $("#fFile").click();
    $("#fFile").onchange = (e) => { for (const f of e.target.files) this.upload(f); e.target.value = ""; };
    const dz = $("#dropzone");
    dz.ondragover = (e) => { e.preventDefault(); dz.classList.add("drag"); };
    dz.ondragleave = () => dz.classList.remove("drag");
    dz.ondrop = (e) => { e.preventDefault(); dz.classList.remove("drag"); for (const f of e.dataTransfer.files) this.upload(f); };
  },
  async list() {
    this.crumbs();
    const ul = $("#files"); ul.innerHTML = '<li class="hint" style="padding:14px">loading…</li>';
    try {
      const items = await rpc("fs.list", { path: this.path });
      ul.innerHTML = "";
      if (!items.length) ul.innerHTML = '<li class="hint" style="padding:14px">empty folder</li>';
      for (const it of items) ul.appendChild(this.row(it));
    } catch (e) { ul.innerHTML = `<li style="padding:14px;color:var(--err)">${e.message}</li>`; }
  },
  row(it) {
    const li = document.createElement("li");
    const icon = it.dir ? "📁" : iconFor(it.name);
    li.innerHTML = `<span class="icn">${icon}</span><span class="fn ${it.dir ? "dir" : ""}">${esc(it.name)}</span>
      <span class="sz">${it.dir ? "" : fmtSize(it.size)}</span>
      <span class="act"></span>`;
    const fn = li.querySelector(".fn");
    fn.onclick = () => { if (it.dir) { this.path = it.path; this.list(); } else this.download(it); };
    const act = li.querySelector(".act");
    if (!it.dir) act.appendChild(btn("⬇", "Download", () => this.download(it)));
    act.appendChild(btn("✎", "Rename", async () => { const n = prompt("Rename to:", it.name); if (!n || n === it.name) return; await rpc("fs.rename", { from: it.path, to: join(this.path, n) }).catch(e => toast(e.message, true)); this.list(); }));
    act.appendChild(btn("🗑", "Delete", async () => { if (!confirm("Delete " + it.name + "?")) return; await rpc("fs.delete", { path: it.path, recursive: it.dir }).catch(e => toast(e.message, true)); this.list(); }, "danger"));
    return li;
  },
  crumbs() {
    const c = $("#crumbs"); c.innerHTML = "";
    const win = (activeDev().kind === "windows");
    const parts = this.path.split(/[\\/]/).filter(Boolean);
    let acc = win ? "" : "/";
    const mk = (label, p) => { const a = document.createElement("a"); a.textContent = label; a.onclick = () => { this.path = p; this.list(); }; c.appendChild(a); };
    mk(win ? "This PC" : "/", win ? parts.slice(0, 1).join("") + "\\" : "/");
    for (let i = 0; i < parts.length; i++) {
      acc = win ? (i === 0 ? parts[0] + "\\" : acc + parts[i] + "\\") : acc + parts[i] + "/";
      const sep = document.createElement("span"); sep.textContent = win ? "\\" : "/"; c.appendChild(sep);
      mk(parts[i], win ? acc.replace(/\\$/, "") : acc);
    }
  },
  async download(it) {
    toast("Downloading " + it.name + "…");
    const url = `/api/download?device=${encodeURIComponent(App.device)}&path=${encodeURIComponent(it.path)}&key=${encodeURIComponent(App.key)}`;
    const a = document.createElement("a"); a.href = url; a.download = it.name; document.body.appendChild(a); a.click(); a.remove();
  },
  async upload(file) {
    toast("Uploading " + file.name + "…");
    try {
      await api(`/api/upload?device=${encodeURIComponent(App.device)}&dir=${encodeURIComponent(this.path)}&name=${encodeURIComponent(file.name)}`,
        { method: "POST", body: file });
      toast("Uploaded " + file.name); this.list();
    } catch (e) { toast(e.message, true); }
  },
};

/* ---------- Desktop ---------- */
const Desktop = {
  running: false, gen: 0, hash: "", mon: 0, fit: true,
  frames: 0, same: 0, bytes: 0, lat: 0, lastT: 0,
  pts: new Map(), touch: null, lpT: null, textQ: "", textT: null, kbdPrev: "",
  presets: { fast: { q: 40, w: 960, fps: 10 }, balanced: { q: 60, w: 1280, fps: 15 }, sharp: { q: 80, w: 1920, fps: 20 } },
  pcKeys: [
    ["", "Keys…"], ["ctrl+alt+delete", "Ctrl+Alt+Del"], ["win", "Windows key"], ["alt+tab", "Alt+Tab"],
    ["ctrl+shift+escape", "Task Manager"], ["win+d", "Show desktop"], ["win+e", "File Explorer"], ["win+r", "Run…"],
    ["alt+f4", "Close window (Alt+F4)"], ["win+up", "Maximize window"], ["win+shift+s", "Snipping tool"],
    ["printscreen", "Print Screen"], ["ctrl+c", "Copy (Ctrl+C)"], ["ctrl+v", "Paste (Ctrl+V)"], ["ctrl+z", "Undo (Ctrl+Z)"],
    ["ctrl+a", "Select all"], ["escape", "Esc"], ["f5", "F5 / refresh"], ["f11", "F11"],
    ["volumeup", "Volume up"], ["volumedown", "Volume down"], ["volumemute", "Mute"], ["mediaplaypause", "Play / pause"],
    ["@lock", "Lock the PC"],
  ],
  phoneKeys: [
    ["", "Buttons…"], ["home", "Home"], ["back", "Back"], ["recents", "Recent apps"],
    ["notifications", "Notifications"], ["quicksettings", "Quick settings"], ["power", "Power menu"], ["lock", "Lock screen"],
  ],
  isPhone() { return activeDev().kind === "android"; },
  ctrlOn() { return $("#dCtrl").checked; },
  settings() {
    const p = $("#dPreset").value;
    if (this.presets[p]) return this.presets[p];
    return { q: +$("#dQual").value, w: +$("#dWidth").value, fps: +$("#dFpsCap").value };
  },
  save() {
    try { localStorage.setItem("bd.desk", JSON.stringify({ preset: $("#dPreset").value, q: $("#dQual").value, w: $("#dWidth").value, fps: $("#dFpsCap").value, fit: this.fit })); } catch (e) {}
  },
  restore() {
    try {
      const s = JSON.parse(localStorage.getItem("bd.desk") || "null"); if (!s) return;
      if (s.preset) $("#dPreset").value = s.preset;
      if (s.q) $("#dQual").value = s.q; if (s.w != null) $("#dWidth").value = s.w; if (s.fps) $("#dFpsCap").value = s.fps;
      if (s.fit === false) this.fit = false;
    } catch (e) {}
  },
  mouse(p) { return rpc("input.mouse", Object.assign({ monitor: this.mon }, p)).catch(() => {}); },
  key(k) { this.flushText(); return rpc("input.key", { key: k, action: "press" }).catch((e) => toast(e.message, true)); },
  typeText(t) { if (!t) return; this.textQ += t; if (!this.textT) this.textT = setTimeout(() => this.flushText(), 15); },
  flushText() {
    clearTimeout(this.textT); this.textT = null;
    const t = this.textQ; this.textQ = ""; if (!t) return;
    if (this.isPhone()) { rpc("input.text", { text: t }).catch(() => {}); return; }
    rpc("input.text", { text: t }).catch((e) => toast(e.message, true));
  },
  fillKeys() {
    const sel = $("#dKeys"); sel.innerHTML = "";
    for (const [v, label] of (this.isPhone() ? this.phoneKeys : this.pcKeys)) { const o = document.createElement("option"); o.value = v; o.textContent = label; sel.appendChild(o); }
  },
  applyFit() {
    $("#screenWrap").classList.toggle("actual", !this.fit);
    $("#dFit").title = this.fit ? "Showing: fit to window — click for actual size" : "Showing: actual size — click to fit";
  },
  bind() {
    this.restore();
    const syncPreset = () => { $("#dCustom").hidden = $("#dPreset").value !== "custom"; this.hash = ""; this.save(); };
    $("#dPreset").onchange = syncPreset; syncPreset();
    for (const id of ["#dQual", "#dWidth", "#dFpsCap"]) $(id).onchange = () => { this.hash = ""; this.save(); };
    $("#dStart").onclick = () => this.start();
    $("#dStop").onclick = () => this.stop();
    $("#dMon").onchange = (e) => { this.mon = +e.target.value; this.hash = ""; };
    $("#dCtrl").onchange = () => $("#screenWrap").classList.toggle("noctl", !this.ctrlOn());
    $("#dKeys").onchange = async (e) => {
      const v = e.target.value; e.target.value = ""; if (!v) return;
      if (v === "@lock") { rpc("session.lock", {}).then(() => toast("PC locked")).catch((er) => toast(er.message, true)); return; }
      this.key(v);
    };
    $("#dType").onclick = () => {
      const t = prompt("Text to type on the device (new lines press Enter):"); if (t) { this.typeText(t); this.flushText(); }
    };
    $("#dPaste").onclick = async () => {
      let t = null;
      try { t = await navigator.clipboard.readText(); } catch (e) {}
      if (t == null || t === "") t = prompt("Paste the text to send to the device's clipboard:");
      if (!t) return;
      try {
        await rpc("clipboard.set", { text: t });
        if (!this.isPhone()) { await rpc("input.key", { key: "ctrl+v", action: "press" }); toast("Pasted on the device"); }
        else toast("Copied to the phone's clipboard — long-press a field to paste");
      } catch (e) { toast(e.message, true); }
    };
    $("#dCopy").onclick = async () => {
      try {
        const r = await rpc("clipboard.get", {}); const t = (r && r.text != null) ? r.text : (typeof r === "string" ? r : "");
        try { await navigator.clipboard.writeText(t); toast("Device clipboard copied here"); }
        catch (e) { prompt("Device clipboard (copy it from here):", t); }
      } catch (e) { toast(e.message, true); }
    };
    $("#dShot").onclick = () => {
      const img = $("#screen"); if (!img.src || img.hidden) { toast("Start the screen first", true); return; }
      const a = document.createElement("a"); a.href = img.src;
      a.download = "screenshot-" + (activeDev().name || "device").replace(/[^\w.-]+/g, "_") + "-" + new Date().toISOString().replace(/[:.]/g, "-").slice(0, 19) + ".jpg";
      document.body.appendChild(a); a.click(); a.remove();
    };
    $("#dFit").onclick = () => { this.fit = !this.fit; this.applyFit(); this.save(); };
    this.applyFit();
    $("#dFull").onclick = () => {
      const on = !document.body.classList.contains("desk-full");
      document.body.classList.toggle("desk-full", on);
      try {
        if (on && document.documentElement.requestFullscreen) document.documentElement.requestFullscreen().catch(() => {});
        if (!on && document.fullscreenElement) document.exitFullscreen().catch(() => {});
      } catch (e) {}
    };
    document.addEventListener("fullscreenchange", () => { if (!document.fullscreenElement) document.body.classList.remove("desk-full"); });

    // ---- on-screen keyboard (phones/tablets): a hidden textarea, diffed on every input ----
    const kbd = $("#dKbdInput"); const SENT = "                ";
    const resetKbd = () => { kbd.value = SENT; this.kbdPrev = SENT; try { kbd.setSelectionRange(SENT.length, SENT.length); } catch (e) {} };
    $("#dKbd").onclick = () => { if (!this.running) { toast("Start the screen first", true); return; } resetKbd(); kbd.focus(); };
    kbd.addEventListener("input", () => {
      const prev = this.kbdPrev, cur = kbd.value;
      let i = 0; while (i < prev.length && i < cur.length && prev[i] === cur[i]) i++;
      const removed = prev.length - i, added = cur.slice(i);
      for (let k = 0; k < removed; k++) this.key(this.isPhone() ? "del" : "backspace");
      const parts = added.split("\n");
      parts.forEach((part, idx) => { if (part) this.typeText(part); if (idx < parts.length - 1) this.key("enter"); });
      this.kbdPrev = cur;
      if (cur.length < 4 || cur.length > 200 || cur.includes("\n")) resetKbd();
    });
    kbd.addEventListener("keydown", (e) => {
      const named = { ArrowUp: "up", ArrowDown: "down", ArrowLeft: "left", ArrowRight: "right", Escape: "escape", Tab: "tab" }[e.key];
      if (named && !this.isPhone()) { e.preventDefault(); this.key(named); }
      e.stopPropagation();
    });

    // ---- pointer: mouse, pen and touch ----
    const img = $("#screen");
    const norm = (e) => { const r = img.getBoundingClientRect(); return { x: Math.min(1, Math.max(0, (e.clientX - r.left) / r.width)), y: Math.min(1, Math.max(0, (e.clientY - r.top) / r.height)) }; };
    const avg = () => { let x = 0, y = 0; for (const p of this.pts.values()) { x += p.x; y += p.y; } const n = this.pts.size || 1; return { x: x / n, y: y / n }; };
    let moveQ = null, moveT = null;
    const queueMove = (p) => { moveQ = p; if (moveT) return; moveT = setTimeout(() => { moveT = null; if (moveQ) { this.mouse({ action: "move", x: moveQ.x, y: moveQ.y }); moveQ = null; } }, 30); };

    img.addEventListener("pointerdown", (e) => {
      if (!this.ctrlOn()) return;
      e.preventDefault(); try { img.setPointerCapture(e.pointerId); } catch (er) {}
      this.pts.set(e.pointerId, { x: e.clientX, y: e.clientY });
      const p = norm(e);
      if (e.pointerType === "touch") {
        if (this.pts.size >= 2) {
          clearTimeout(this.lpT);
          if (this.touch && this.touch.mode === "drag" && !this.isPhone()) this.mouse({ action: "up", button: "left", x: p.x, y: p.y });
          const a = avg(); this.touch = { mode: "scroll", x: a.x, y: a.y };
          return;
        }
        this.touch = { mode: "tap", start: p, cx: e.clientX, cy: e.clientY, t: Date.now() };
        clearTimeout(this.lpT);
        this.lpT = setTimeout(() => {
          if (!this.touch || this.touch.mode !== "tap") return;
          this.touch.mode = "done";
          const s = this.touch.start;
          if (this.isPhone()) this.mouse({ action: "longpress", x: s.x, y: s.y });
          else this.mouse({ action: "click", button: "right", x: s.x, y: s.y });
          if (navigator.vibrate) try { navigator.vibrate(15); } catch (er) {}
        }, 550);
        return;
      }
      this._downAt = { x: p.x, y: p.y, t: Date.now() };
      if (!this.isPhone()) this.mouse({ action: "down", button: btnName(e.button), x: p.x, y: p.y });
    });
    img.addEventListener("pointermove", (e) => {
      if (!this.ctrlOn()) return;
      if (e.pointerType === "touch") {
        if (!this.pts.has(e.pointerId) || !this.touch) return;
        this.pts.set(e.pointerId, { x: e.clientX, y: e.clientY });
        const t = this.touch;
        if (t.mode === "scroll") {
          const a = avg(); const dy = a.y - t.y, dx = a.x - t.x;
          if (Math.abs(dy) >= 24) { const c = norm({ clientX: a.x, clientY: a.y }); this.mouse({ action: "scroll", dy: dy > 0 ? -3 : 3, x: c.x, y: c.y }); t.y = a.y; }
          if (Math.abs(dx) >= 40 && !this.isPhone()) { this.mouse({ action: "hscroll", dx: dx > 0 ? -3 : 3 }); t.x = a.x; }
          return;
        }
        if (t.mode === "tap" && Math.hypot(e.clientX - t.cx, e.clientY - t.cy) > 10) {
          clearTimeout(this.lpT); t.mode = "drag";
          if (!this.isPhone()) this.mouse({ action: "down", button: "left", x: t.start.x, y: t.start.y });
        }
        if (t.mode === "drag" && !this.isPhone()) queueMove(norm(e));
        return;
      }
      if (this.isPhone()) return;
      queueMove(norm(e));
    });
    const endPointer = (e, cancelled) => {
      if (!this.ctrlOn()) { this.pts.delete(e.pointerId); return; }
      const p = norm(e);
      if (e.pointerType === "touch") {
        this.pts.delete(e.pointerId);
        const t = this.touch; if (!t) return;
        if (t.mode === "scroll") { if (this.pts.size === 0) this.touch = null; return; }
        clearTimeout(this.lpT);
        if (!cancelled) {
          if (t.mode === "tap") this.mouse({ action: this.isPhone() ? "tap" : "click", button: "left", x: t.start.x, y: t.start.y });
          else if (t.mode === "drag") {
            if (this.isPhone()) this.mouse({ action: "swipe", x: t.start.x, y: t.start.y, x2: p.x, y2: p.y, ms: Math.min(600, Math.max(120, Date.now() - t.t)) });
            else { if (moveT) { clearTimeout(moveT); moveT = null; } this.mouse({ action: "up", button: "left", x: p.x, y: p.y }); }
          }
        } else if (t.mode === "drag" && !this.isPhone()) this.mouse({ action: "up", button: "left", x: p.x, y: p.y });
        this.touch = null;
        return;
      }
      this.pts.delete(e.pointerId);
      const s = this._downAt; if (!s) return; this._downAt = null;
      if (this.isPhone()) {
        if (cancelled) return;
        const moved = Math.hypot(p.x - s.x, p.y - s.y);
        if (moved < 0.03) this.mouse({ action: Date.now() - s.t > 550 ? "longpress" : "tap", x: p.x, y: p.y });
        else this.mouse({ action: "swipe", x: s.x, y: s.y, x2: p.x, y2: p.y, ms: Math.min(600, Math.max(120, Date.now() - s.t)) });
      } else {
        if (moveT) { clearTimeout(moveT); moveT = null; }
        this.mouse({ action: "up", button: btnName(e.button), x: p.x, y: p.y });
      }
    };
    img.addEventListener("pointerup", (e) => endPointer(e, false));
    img.addEventListener("pointercancel", (e) => endPointer(e, true));
    img.addEventListener("contextmenu", (e) => e.preventDefault());
    img.addEventListener("dragstart", (e) => e.preventDefault());
    img.addEventListener("wheel", (e) => {
      if (!this.ctrlOn()) return; e.preventDefault();
      const p = norm(e);
      if (e.shiftKey || Math.abs(e.deltaX) > Math.abs(e.deltaY)) { if (!this.isPhone()) this.mouse({ action: "hscroll", dx: Math.sign(e.deltaX || e.deltaY) * 3, x: p.x, y: p.y }); return; }
      this.mouse({ action: "scroll", dy: Math.sign(e.deltaY) * 3, x: p.x, y: p.y });
    }, { passive: false });

    // ---- physical keyboard ----
    window.addEventListener("keydown", (e) => {
      if (!this.running || !this.ctrlOn() || e.isComposing) return;
      const pg = $(".page[data-page=desktop]"); if (!pg.classList.contains("active")) return;
      const tg = e.target; if (tg && tg !== document.body && tg !== img && /^(INPUT|TEXTAREA|SELECT)$/.test(tg.tagName)) return;
      const altGr = e.getModifierState && e.getModifierState("AltGraph");
      const printable = e.key.length === 1 && (altGr || !(e.ctrlKey || e.metaKey || e.altKey));
      if (this.isPhone()) {
        if (printable) { e.preventDefault(); this.typeText(e.key); return; }
        const named = { Backspace: "del", Enter: "enter", Escape: "back", Tab: "tab", Home: "home" }[e.key];
        if (named) { e.preventDefault(); this.key(named); }
        return;
      }
      if (printable) { e.preventDefault(); this.typeText(e.key); return; }
      const combo = keyCombo(e);
      if (combo) { e.preventDefault(); this.key(combo); }
    });
    document.addEventListener("visibilitychange", () => { this.hash = ""; });
  },
  async loadMonitors() {
    const sel = $("#dMon"); sel.hidden = true; sel.innerHTML = ""; this.mon = 0;
    if (this.isPhone()) return;
    let mons = [];
    try { mons = await rpc("screen.monitors", {}) || []; } catch (e) { return; }
    if (mons.length < 2) return;
    mons.forEach((m, i) => { const o = document.createElement("option"); o.value = i; o.textContent = "Screen " + (i + 1) + (m.primary ? " (main)" : "") + " · " + m.w + "×" + m.h; sel.appendChild(o); });
    const all = document.createElement("option"); all.value = -1; all.textContent = "All screens"; sel.appendChild(all);
    sel.value = "0"; sel.hidden = false;
  },
  async start() {
    const d = activeDev();
    if (!(d.caps || []).includes("screen")) { toast("This device can't share its screen", true); return; }
    this.fillKeys();
    $("#dKbd").hidden = false; $("#dPaste").hidden = false;
    $("#screenWrap").classList.toggle("noctl", !this.ctrlOn());
    if (this.running) return;
    this.running = true; this.gen++; this.hash = "";
    $("#screen").hidden = false; $("#screenOff").hidden = true;
    this.frames = 0; this.same = 0; this.bytes = 0; this.lastT = performance.now();
    $("#dFps").textContent = "connecting…";
    await this.loadMonitors();
    this.loop(this.gen);
  },
  stop() {
    this.running = false; this.gen++; this.hash = ""; this.touch = null; this.pts.clear();
    const s = $("#screen"); if (s) { s.hidden = true; }
    const off = $("#screenOff"); if (off) off.hidden = false;
    const f = $("#dFps"); if (f) f.textContent = "";
    document.body.classList.remove("desk-full");
  },
  async loop(gen) {
    let fails = 0;
    const live = () => this.running && this.gen === gen;
    while (live()) {
      if (document.hidden) { await sleep(500); continue; }
      const st = this.settings(); const t0 = performance.now();
      try {
        const r = await rpc("screen.capture", { quality: st.q, maxWidth: st.w, monitor: this.mon, since: this.hash });
        if (!live()) break;
        this.lat = Math.round(performance.now() - t0);
        if (r.same) { this.same++; }
        else {
          const img = $("#screen"); img.src = "data:" + (r.mime || "image/jpeg") + ";base64," + r.data;
          this.hash = r.hash || ""; this.frames++; this.bytes += r.data.length * 0.75;
          this._dim = r.w + "×" + r.h;
        }
        fails = 0;
        const now = performance.now();
        if (now - this.lastT > 1000) {
          const secs = (now - this.lastT) / 1000;
          const fps = Math.round(this.frames / secs), kbs = Math.round(this.bytes / 1024 / secs);
          $("#dFps").textContent = (this.frames === 0 && this.same > 0 ? "no changes" : fps + " fps") + " · " + kbs + " KB/s · " + this.lat + " ms" + (this._dim ? " · " + this._dim : "");
          this.frames = 0; this.same = 0; this.bytes = 0; this.lastT = now;
        }
      } catch (e) {
        fails++;
        // The phone may still be showing its "start capturing?" prompt — keep trying a while.
        if (/grant|not available|prompt|projection/i.test(e.message) && fails < 60) {
          $("#dFps").textContent = "waiting for screen permission on the device…";
          await sleep(1000); continue;
        }
        if (/secure desktop|locked|BitBlt/i.test(e.message) && fails < 600) {
          $("#dFps").textContent = "screen unavailable (locked or a UAC prompt) — retrying…";
          await sleep(1000); continue;
        }
        if (fails < 3) { await sleep(500); continue; }
        toast(e.message, true); this.stop(); break;
      }
      const wait = 1000 / Math.max(1, st.fps) - (performance.now() - t0);
      await sleep(Math.max(10, wait));
    }
  },
};
function sleep(ms) { return new Promise((r) => setTimeout(r, ms)); }

/* ---------- Notifications ---------- */
const Notif = {
  items: [], subbed: null,
  bind() {
    $("#nRefresh").onclick = () => this.load();
    $("#nMirror").onchange = async (e) => {
      try { await api("/api/settings", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ mirrorNotif: e.target.checked }) }); toast(e.target.checked ? "PC toasts on" : "PC toasts off"); }
      catch (err) { toast(err.message, true); }
    };
  },
  async enter() {
    // reflect the PC-toast setting
    try { const s = await (await api("/api/settings")).json(); $("#nMirror").checked = !!s.mirrorNotif; } catch (e) {}
    this.subscribeLive();
    this.load();
  },
  isPhone() { const d = activeDev(); return (d.caps || []).includes("notifications") || d.kind === "android"; },
  subscribeLive() {
    if (this.subbed) { unsubscribe("notif"); this.subbed = null; }
    if (!this.isPhone()) return;
    subscribe("notif", (m) => {
      if (m.event === "notif.posted") { const n = m.params; this.items = this.items.filter(x => x.key !== n.key); this.items.unshift(n); this.render(); }
      else if (m.event === "notif.removed") { this.items = this.items.filter(x => x.key !== m.params.key); this.render(); }
    });
    this.subbed = App.device;
  },
  async load() {
    const host = $("#notifList");
    if (!this.isPhone()) {
      // show cross-device history the hub kept
      try { const r = await (await api("/api/notifications")).json(); this.items = r.notifications || []; } catch (e) { this.items = []; }
      $("#nHint").textContent = "Select your phone as the active device to control its notifications live.";
      this.render();
      return;
    }
    $("#nHint").textContent = activeDev().name;
    host.innerHTML = '<div class="hint">loading…</div>';
    try {
      const r = await rpc("notif.list");
      this.items = r.notifications || [];
      this.render();
    } catch (e) {
      host.innerHTML = `<div class="card"><p class="hint">${esc(e.message)}</p>
        <p class="hint">On the phone: BridgeDesk → <b>Phone access</b> → <b>Mirror notifications to PC</b> → enable in Android settings.</p></div>`;
    }
  },
  render() {
    const host = $("#notifList");
    if (!this.items.length) {
      host.innerHTML = `<div class="notif-empty">
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.6"><path d="M6 9a6 6 0 0112 0c0 5 2 6 2 6H4s2-1 2-6z" stroke-linejoin="round"/><path d="M10 19a2 2 0 004 0"/></svg>
        <div>You're all caught up.</div>
        <div class="hint">Notifications from your phone show up here.</div></div>`;
      return;
    }
    host.innerHTML = "";
    for (const n of this.items) {
      const card = document.createElement("div"); card.className = "notif";
      const app = n.app || n.pkg || "App";
      const color = avatarColor(app);
      const initial = (app.trim()[0] || "?").toUpperCase();
      const actions = (n.actions || []).map(a => (typeof a === "string") ? { title: a, reply: false } : a);
      let acts = "";
      actions.forEach((a, i) => {
        const primary = /approve|yes|allow|confirm|it'?s me/i.test(a.title);
        acts += `<button class="notif-act ${primary ? "primary" : ""} ${a.reply ? "is-reply" : ""}" data-k="${esc(n.key)}" data-a="${i}" data-reply="${a.reply ? 1 : 0}">${a.reply ? "↩ " : ""}${esc(a.title)}</button>`;
      });
      const openable = n.canOpen !== false;
      card.innerHTML = `
        <div class="notif-av" style="background:${color}">${esc(initial)}</div>
        <div class="notif-main">
          <div class="notif-head"><span class="notif-app">${esc(app)}</span><span class="notif-time">${relTime(n.time)}</span></div>
          <div class="notif-body ${openable ? "openable" : ""}" ${openable ? `data-open="${esc(n.key)}" title="Open on phone"` : ""}>
            ${n.title ? `<div class="notif-title">${esc(n.title)}</div>` : ""}
            ${n.text ? `<div class="notif-text">${esc(n.text)}</div>` : ""}
          </div>
          ${acts ? `<div class="notif-actions">${acts}</div>` : ""}
          <div class="notif-reply" hidden><input type="text" class="notif-reply-in" placeholder="Type a reply…"><button class="notif-act primary notif-reply-send">Send</button><button class="notif-act notif-reply-cancel">Cancel</button></div>
        </div>
        <button class="notif-dismiss" data-dismiss="${esc(n.key)}" title="Dismiss">✕</button>`;

      // tap body to open the app on the phone
      const body = card.querySelector("[data-open]");
      if (body) body.onclick = async () => { try { await rpc("notif.open", { key: body.dataset.open }); toast("Opened on phone"); } catch (e) { toast(e.message, true); } };

      const replyBox = card.querySelector(".notif-reply");
      const replyIn = card.querySelector(".notif-reply-in");
      card.querySelectorAll("button[data-a]").forEach(b => b.onclick = async () => {
        if (b.dataset.reply === "1") {
          // show the inline reply field, remember which action to fire
          replyBox.hidden = false; replyIn.focus(); replyBox.dataset.k = b.dataset.k; replyBox.dataset.a = b.dataset.a;
          return;
        }
        try { await rpc("notif.action", { key: b.dataset.k, action: +b.dataset.a }); toast("Sent: " + b.textContent.trim()); }
        catch (e) { toast(e.message, true); }
      });
      const sendReply = async () => {
        const text = replyIn.value.trim(); if (!text) return;
        try { await rpc("notif.reply", { key: replyBox.dataset.k, action: +replyBox.dataset.a, text }); toast("Reply sent"); replyIn.value = ""; replyBox.hidden = true; }
        catch (e) { toast(e.message, true); }
      };
      card.querySelector(".notif-reply-send").onclick = sendReply;
      card.querySelector(".notif-reply-cancel").onclick = () => { replyBox.hidden = true; replyIn.value = ""; };
      replyIn.onkeydown = (e) => { if (e.key === "Enter") sendReply(); };

      const dz = card.querySelector("button[data-dismiss]");
      dz.onclick = async () => { try { await rpc("notif.dismiss", { key: dz.dataset.dismiss }); this.items = this.items.filter(x => x.key !== dz.dataset.dismiss); this.render(); } catch (e) { toast(e.message, true); } };
      host.appendChild(card);
    }
  },
};

/* ---------- Clipboard ---------- */
function bindClipboard() {
  $("#clipPull").onclick = async () => { try { const r = await rpc("clipboard.get"); $("#clipText").value = r.text || ""; toast("Pulled clipboard"); } catch (e) { toast(e.message, true); } };
  $("#clipPush").onclick = async () => { try { await rpc("clipboard.set", { text: $("#clipText").value }); toast("Pushed to device clipboard"); } catch (e) { toast(e.message, true); } };
}

/* ---------- Devices page ---------- */
function renderDevices() {
  const host = $("#devList"); if (!host) return;
  host.innerHTML = "";
  for (const d of App.devices) {
    const card = document.createElement("div"); card.className = "card";
    const caps = (d.caps || []).map((c) => `<span class="chip">${c}</span>`).join("");
    card.innerHTML = `<div class="dev">
      <div style="font-size:26px">${d.local ? "🖥" : (d.kind === "android" ? "📱" : "💻")}</div>
      <div class="meta"><div class="nm">${esc(d.name)} ${d.local ? '<span class="chip">this device</span>' : ""} ${d.online ? '<span class="chip" style="color:var(--ok)">online</span>' : '<span class="chip" style="color:var(--err)">offline</span>'}</div>
        <div class="sub">${esc(d.os || d.kind)} · ${esc(d.platform || "")}</div><div>${caps}</div></div>
      <button>Use</button></div>`;
    card.querySelector("button").onclick = () => { switchDevice(d.id); toast("Active: " + d.name); };
    host.appendChild(card);
  }
  // link info + PC-to-PC links
  Settings.fillLink();
  Links.bind();
  Links.render();
}

/* ---------- Add-device pairing ---------- */
const Pair = {
  bind() {
    const open = () => this.open();
    const btn = $("#addDeviceBtn"); if (btn) btn.onclick = open;
    $("#pairClose").onclick = () => $("#pairModal").hidden = true;
    $("#pairModal").onclick = (e) => { if (e.target.id === "pairModal") $("#pairModal").hidden = true; };
    $("#pairAddrSel").onchange = () => this.render();
    $("#pairCopy").onclick = () => { const l = this.link(); if (navigator.clipboard) navigator.clipboard.writeText(l); toast("Pairing link copied"); };
  },
  async open() {
    try { this.s = await (await api("/api/settings")).json(); } catch (e) { toast(e.message, true); return; }
    const sel = $("#pairAddrSel"); sel.innerHTML = "";
    const addrs = this.s.addresses || [];
    for (const ip of addrs) { const o = document.createElement("option"); o.value = ip; o.textContent = ip; sel.appendChild(o); }
    sel.style.display = addrs.length > 1 ? "" : "none";
    $("#pairModal").hidden = false;
    this.render();
  },
  host() { const sel = $("#pairAddrSel"); return (sel && sel.value) || this.s.primaryHost || (this.s.addresses || [])[0] || ""; },
  link() { return `bd://${this.host()}:${this.s.lanPort}?key=${encodeURIComponent(this.s.accessKey)}&fp=${this.s.fingerprint}`; },
  render() {
    const s = this.s;
    $("#pairAddr").textContent = this.host() || "—";
    $("#pairPort").textContent = s.lanPort;
    $("#pairKey").textContent = s.accessKey;
    const warn = $("#pairWarn");
    if (!s.listenLan) { warn.style.display = "block"; warn.textContent = "⚠ Network sharing is paused — enable “Listen on local network” in Settings first."; }
    else if (!(s.addresses || []).length) { warn.style.display = "block"; warn.textContent = "⚠ No network address — connect this PC to Wi‑Fi/Ethernet."; }
    else warn.style.display = "none";
    const host = $("#qr"); host.innerHTML = "";
    if (!this.host()) return;
    try {
      const qr = window.qrcode(0, "M");
      qr.addData(this.link());
      qr.make();
      host.innerHTML = qr.createImgTag(5, 8);
    } catch (e) { host.innerHTML = '<span style="color:#b00">QR error</span>'; }
  },
};

/* ---------- Linked computers (PC-to-PC) ---------- */
const Links = {
  bound: false,
  bind() {
    if (this.bound) return; this.bound = true;
    const add = $("#linkAdd"); if (!add) return;
    add.onclick = async () => {
      const host = $("#linkHost").value.trim();
      const port = +($("#linkPort").value || 47810);
      const key = $("#linkKey").value.trim();
      if (!host || !key) { $("#linkMsg").textContent = "Enter the other PC's address and access key."; return; }
      $("#linkMsg").textContent = "Connecting…"; add.disabled = true;
      try {
        const r = await (await api("/api/links", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ host, port, key }) })).json();
        if (r.error) { $("#linkMsg").textContent = "⚠ " + r.error; }
        else { $("#linkMsg").textContent = "✓ Added. It will appear below and in the device switcher once it links."; $("#linkHost").value = ""; $("#linkKey").value = ""; setTimeout(() => { this.render(); refreshDevices(); }, 1200); }
      } catch (e) { $("#linkMsg").textContent = "⚠ " + e.message; }
      add.disabled = false;
      this.render();
    };
  },
  async render() {
    const host = $("#linkList"); if (!host) return;
    let links = [];
    try { links = (await (await api("/api/links")).json()).links || []; } catch (e) { return; }
    if (!links.length) { host.innerHTML = ""; return; }
    const online = {}; App.devices.forEach(d => { if (d.addr) online[d.addr] = d.online; });
    host.innerHTML = '<div class="hint" style="margin:6px 0">Linked computers:</div>';
    for (const l of links) {
      const row = document.createElement("div"); row.className = "row"; row.style.justifyContent = "space-between";
      row.innerHTML = `<div class="mono" style="font-size:13px">${esc(l.url)}</div>`;
      const rm = document.createElement("button"); rm.className = "icon danger"; rm.textContent = "Remove"; rm.title = "Unlink";
      rm.onclick = async () => { try { await api("/api/links?url=" + encodeURIComponent(l.url), { method: "DELETE" }); this.render(); refreshDevices(); } catch (e) { toast(e.message, true); } };
      row.appendChild(rm);
      host.appendChild(row);
    }
  },
};

/* ---------- Shared storage (SMB/NFS) ---------- */
const Shares = {
  html() {
    return `<div class="card">
      <h3>Shared storage (SMB / NFS)</h3>
      <p class="hint">Share a folder from this PC so other machines in the fleet can mount it as a network drive, or mount a share from elsewhere. Creating/mounting usually needs administrator rights.</p>
      <div class="row" style="justify-content:space-between"><b style="font-size:13px">Shares on your fleet</b><button id="shScan" class="icon" title="Rescan">⟳ Detect</button></div>
      <div id="shFleet"><div class="hint" style="padding:2px 0">Scanning…</div></div>
      <hr style="border:none;border-top:1px solid var(--border);margin:12px 0">
      <div class="row"><b style="font-size:13px">Share a folder</b></div>
      <div class="row">
        <input type="text" id="shPath" placeholder="folder path e.g. C:\\Shared" style="flex:2;min-width:160px">
        <input type="text" id="shName" placeholder="share name" style="flex:1;min-width:110px">
        <label class="hint"><input type="checkbox" id="shRO"> read-only</label>
        <button id="shCreate" class="primary">Share</button>
      </div>
      <div id="shList" style="margin:6px 0"></div>
      <div class="row" style="margin-top:12px"><b style="font-size:13px">Mount a share</b></div>
      <div class="row">
        <input type="text" id="shSrc" placeholder="\\\\PC\\share  or  host:/export" style="flex:2;min-width:180px">
        <input type="text" id="shAs" placeholder="Z:  or  /mnt/x" style="flex:1;min-width:90px">
        <button id="shMount" class="primary">Mount</button>
      </div>
      <div class="row"><input type="text" id="shUser" placeholder="user (optional)" style="flex:1"><input type="password" id="shPass" placeholder="password (optional)" style="flex:1"></div>
      <div id="shMsg" class="hint"></div>
    </div>`;
  },
  bind() {
    const C = $("#shCreate"); if (!C) return;
    $("#shScan").onclick = () => this.detect();
    this.detect();
    C.onclick = async () => {
      const path = $("#shPath").value.trim(), name = $("#shName").value.trim();
      if (!path || !name) { $("#shMsg").textContent = "Enter a folder and a share name."; return; }
      $("#shMsg").textContent = "Creating share…";
      try { const r = await rpc("share.create", { path, name, readonly: $("#shRO").checked });
        $("#shMsg").innerHTML = `✓ Shared as <span class="mono">${esc(r.unc || name)}</span> — mount that on another machine.`;
        $("#shPath").value = ""; $("#shName").value = ""; this.list();
      } catch (e) { $("#shMsg").textContent = "⚠ " + e.message; }
    };
    $("#shMount").onclick = async () => {
      const source = $("#shSrc").value.trim(), mountAs = $("#shAs").value.trim();
      if (!source || !mountAs) { $("#shMsg").textContent = "Enter a share path and a drive/mount point."; return; }
      $("#shMsg").textContent = "Mounting…";
      try { const r = await rpc("share.mount", { source, mountAs, user: $("#shUser").value.trim(), pass: $("#shPass").value });
        $("#shMsg").innerHTML = `✓ Mounted <span class="mono">${esc(source)}</span> at <span class="mono">${esc(r.mounted || mountAs)}</span>`;
        $("#shSrc").value = ""; $("#shAs").value = ""; $("#shUser").value = ""; $("#shPass").value = "";
      } catch (e) { $("#shMsg").textContent = "⚠ " + e.message; }
    };
    this.list();
  },
  async detect() {
    const host = $("#shFleet"); if (!host) return;
    host.innerHTML = '<div class="hint" style="padding:2px 0">Scanning the fleet…</div>';
    let data;
    try { data = await (await api("/api/fleet-shares")).json(); } catch (e) { host.innerHTML = `<div class="hint">${esc(e.message)}</div>`; return; }
    // only show shares offered by OTHER machines (mounting your own share locally is pointless)
    const shares = (data.shares || []).filter(s => s.deviceId !== data.self);
    if (!shares.length) { host.innerHTML = '<div class="hint" style="padding:2px 0">No shares detected on other machines. Share a folder below (on a Windows machine) and it\'ll appear here on the others.</div>'; return; }
    host.innerHTML = "";
    for (const s of shares) {
      const row = document.createElement("div"); row.className = "row"; row.style.cssText = "justify-content:space-between;border-bottom:1px solid var(--border);padding:7px 0";
      row.innerHTML = `<div style="min-width:0">
        <div><span class="mono" style="color:var(--accent)">${esc(s.unc || s.name)}</span></div>
        <div class="hint">on ${esc(s.device)} · ${esc(s.path || s.name)}</div></div>`;
      const btn = document.createElement("button"); btn.className = "primary"; btn.textContent = "Mount here";
      btn.onclick = async () => {
        btn.disabled = true; btn.textContent = "Mounting…";
        try { const r = await rpc("share.mount", { source: s.unc, mountAs: "auto", user: s.user || "", pass: s.pass || "" }, "local");
          $("#shMsg").innerHTML = `✓ Mounted <span class="mono">${esc(s.unc)}</span> at <span class="mono">${esc(r.mounted || "")}</span> on this PC`;
          btn.textContent = "Mounted ✓";
        } catch (e) { $("#shMsg").textContent = "⚠ " + e.message; btn.disabled = false; btn.textContent = "Mount here"; }
      };
      row.appendChild(btn);
      host.appendChild(row);
    }
  },
  async list() {
    const host = $("#shList"); if (!host) return;
    let shares = [];
    try { shares = await rpc("share.list"); } catch (e) { return; }
    if (!shares || !shares.length) { host.innerHTML = '<div class="hint" style="padding:2px 0">No shares on this PC yet.</div>'; return; }
    host.innerHTML = shares.map(s => `<div class="row" style="justify-content:space-between;border-bottom:1px solid var(--border);padding:6px 0">
      <div><span class="mono" style="color:var(--accent)">${esc(s.unc || s.name)}</span> <span class="hint">→ ${esc(s.path)}</span></div>
      <button class="icon danger" data-rm="${esc(s.name)}">Unshare</button></div>`).join("");
    host.querySelectorAll("button[data-rm]").forEach(b => b.onclick = async () => {
      try { await rpc("share.remove", { name: b.dataset.rm }); this.list(); } catch (e) { toast(e.message, true); }
    });
  },
};

/* ---------- Settings ---------- */
const Settings = {
  async load() {
    let s; try { s = await (await api("/api/settings")).json(); } catch (e) { return; }
    this.data = s;
    const body = $("#settingsBody");
    body.innerHTML = `
      <h2 class="ph">Settings — this PC</h2>
      <div class="card">
        <h3>Device</h3>
        <div class="row"><label style="flex:1">Name <input type="text" id="setName" value="${esc(s.name)}" style="width:100%"></label></div>
        <div class="row"><button id="setNameSave">Save name</button></div>
      </div>
      <div class="card">
        <h3>Capabilities</h3>
        ${tgl("allowShell", "Shell &amp; terminal", "Run PowerShell/cmd and interactive terminals", s.allowShell)}
        ${tgl("allowFiles", "File access &amp; sharing", "Browse, transfer, and the WebDAV network drive", s.allowFiles)}
        ${tgl("allowScreen", "Screen &amp; remote control", "Share the screen and accept mouse/keyboard", s.allowScreen)}
        ${tgl("allowMcp", "Claude (MCP)", "Let Claude drive this hub over the local MCP endpoint", s.allowMcp)}
        ${tgl("mirrorNotif", "Mirror phone notifications", "Pop a desktop notification on this PC when a linked phone gets one", s.mirrorNotif)}
      </div>
      <div class="card">
        <h3>Network</h3>
        ${tgl("listenLan", "Listen on local network", "Accept connections from phones/PCs on your Wi‑Fi", s.listenLan)}
        ${tgl("discovery", "LAN discovery", "Let devices find this PC automatically", s.discovery)}
        ${tgl("autostart", "Start with Windows", "Launch BridgeDesk in the background at login", s.autostart)}
        <div class="kv" style="margin-top:10px">
          <div class="k">WebDAV / network drive</div><div class="code">${esc(s.davUrl)}</div>
        </div>
      </div>
      <div class="card">
        <h3>Security</h3>
        <div class="kv">
          <div class="k">Access key</div><div class="code" id="setKey">${esc(s.accessKey)}</div>
          <div class="k">Certificate fingerprint</div><div class="mono" style="font-size:12px">${esc(prettyFp(s.fingerprint))}</div>
          <div class="k">Privilege</div><div>${s.elevated ? '<span class="chip" style="color:var(--ok)">Administrator</span>' : '<span class="chip">Standard user</span>'}</div>
        </div>
        ${(s.os === "windows" && !s.elevated) ? '<p class="hint" style="margin-top:8px">For full system access (protected folders, all processes), right-click the BridgeDesk tray icon → <b>Restart as administrator</b>.</p>' : ''}
        <div class="row" style="margin-top:10px"><button class="danger" id="setRotate">Rotate access key</button>
          <span class="hint">Rotating disconnects all currently linked devices.</span></div>
      </div>`;
    body.insertAdjacentHTML("beforeend", Shares.html());
    body.querySelectorAll(".sw").forEach((sw) => sw.onclick = () => this.toggle(sw));
    $("#setNameSave").onclick = () => this.save({ name: $("#setName").value });
    Shares.bind();
    $("#setRotate").onclick = async () => { if (!confirm("Rotate the access key? You'll re-enter it on every device.")) return; await this.save({ rotateKey: true }); toast("Key rotated — reconnecting"); setTimeout(() => location.reload(), 800); };
  },
  async toggle(sw) {
    const k = sw.dataset.k; const on = !sw.classList.contains("on");
    sw.classList.toggle("on", on);
    await this.save({ [k]: on });
  },
  async save(patch) {
    try { this.data = await (await api("/api/settings", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(patch) })).json(); toast("Saved"); refreshDevices(); }
    catch (e) { toast(e.message, true); }
  },
  async fillLink() {
    let s = this.data; if (!s) { try { s = await (await api("/api/settings")).json(); this.data = s; } catch (e) { return; } }
    if ($("#devKey")) $("#devKey").textContent = s.accessKey;
    if ($("#devFp")) $("#devFp").textContent = prettyFp(s.fingerprint);
    if ($("#devPort")) $("#devPort").textContent = s.lanPort;
    if ($("#davUrl")) $("#davUrl").textContent = s.davUrl;
    const addrs = (s.addresses || []);
    const urls = $("#lanUrls");
    if (urls) {
      urls.innerHTML = addrs.length
        ? addrs.map(ip => `https://${ip}:${s.lanPort}/`).join("<br>")
        : '<span style="color:var(--warn)">no network address found</span>';
    }
    const noLan = $("#noLan");
    if (noLan) {
      if (!s.listenLan) { noLan.style.display = "block"; noLan.textContent = "⚠ Network sharing is paused — turn on “Listen on local network” in Settings so other devices can connect."; }
      else if (!addrs.length) { noLan.style.display = "block"; noLan.textContent = "⚠ This PC has no LAN address yet (not connected to Wi‑Fi/Ethernet?)."; }
      else noLan.style.display = "none";
    }
    const link = $("#devLink");
    if (link) {
      const host = s.primaryHost || (addrs[0] || "");
      const bd = `bd://${host}:${s.lanPort}?key=${encodeURIComponent(s.accessKey)}&fp=${s.fingerprint}`;
      link.textContent = bd;
      link.onclick = () => { navigator.clipboard && navigator.clipboard.writeText(bd); toast("Link copied — paste it into the phone's address box"); };
    }
  },
};

/* ---------- small DOM utils ---------- */
function tgl(k, label, sub, on) {
  return `<div class="toggle"><div class="lbl">${label}<small>${sub}</small></div><div class="sw ${on ? "on" : ""}" data-k="${k}"></div></div>`;
}
function btn(txt, title, fn, cls) { const b = document.createElement("button"); b.className = "icon " + (cls || ""); b.textContent = txt; b.title = title; b.onclick = (e) => { e.stopPropagation(); fn(); }; return b; }
function esc(s) { return (s == null ? "" : String(s)).replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c])); }
function parentPath(p) { if (!p) return p; const win = p.includes("\\"); const parts = p.split(/[\\/]/).filter(Boolean); if (win && parts.length <= 1) return parts[0] + "\\"; parts.pop(); return win ? parts.join("\\") + (parts.length === 1 ? "\\" : "") : "/" + parts.join("/"); }
function join(dir, name) { const win = dir.includes("\\") || /^[A-Za-z]:$/.test(dir); const sep = win ? "\\" : "/"; return dir.replace(/[\\/]+$/, "") + sep + name; }
function iconFor(name) { const e = (name.split(".").pop() || "").toLowerCase();
  if (["png", "jpg", "jpeg", "gif", "webp", "bmp", "svg"].includes(e)) return "🖼";
  if (["mp4", "mkv", "mov", "avi", "webm"].includes(e)) return "🎬";
  if (["mp3", "wav", "flac", "ogg", "m4a"].includes(e)) return "🎵";
  if (["zip", "rar", "7z", "tar", "gz"].includes(e)) return "🗜";
  if (["pdf"].includes(e)) return "📕";
  if (["doc", "docx", "txt", "md", "rtf"].includes(e)) return "📄";
  if (["xls", "xlsx", "csv"].includes(e)) return "📊";
  if (["exe", "msi", "apk", "bat", "ps1", "sh"].includes(e)) return "⚙️";
  return "📄"; }
function btnName(b) { return b === 2 ? "right" : b === 1 ? "middle" : "left"; }
function keyCombo(e) {
  const mods = []; if (e.ctrlKey) mods.push("ctrl"); if (e.altKey) mods.push("alt"); if (e.shiftKey) mods.push("shift"); if (e.metaKey) mods.push("win");
  let k = e.key;
  const map = { " ": "space", "+": "plus", "ArrowUp": "up", "ArrowDown": "down", "ArrowLeft": "left", "ArrowRight": "right", "Escape": "escape", "Enter": "enter", "Backspace": "backspace", "Delete": "delete", "Tab": "tab", "Home": "home", "End": "end", "PageUp": "pageup", "PageDown": "pagedown", "Insert": "insert", "CapsLock": "capslock", "PrintScreen": "printscreen", "ContextMenu": "apps", "Pause": "pause", "NumLock": "numlock", "ScrollLock": "scrolllock", "AudioVolumeUp": "volumeup", "AudioVolumeDown": "volumedown", "AudioVolumeMute": "volumemute", "MediaPlayPause": "mediaplaypause" };
  if (map[k]) k = map[k]; else if (k.length === 1) {
    k = k.toLowerCase();
    // With Ctrl/Alt held, use the physical key so shortcuts work whatever the layout (e.g. Ctrl+Shift+1).
    const m = /^(Key([A-Z])|Digit(\d))$/.exec(e.code || ""); if (m && mods.length) k = (m[2] || m[3]).toLowerCase();
  } else if (/^F\d+$/.test(k)) k = k.toLowerCase(); else return null;
  if (["ctrl", "alt", "shift", "win"].includes(k)) return null;
  return mods.concat(k).join("+");
}
function prettyFp(fp) { if (!fp) return ""; return (fp.match(/.{1,4}/g) || []).slice(0, 8).join(" "); }
function avatarColor(s) {
  let h = 0; for (let i = 0; i < s.length; i++) h = (h * 31 + s.charCodeAt(i)) % 360;
  return `hsl(${h}, 42%, 42%)`;
}
function relTime(ms) {
  if (!ms) return "";
  const d = Date.now() - ms;
  if (d < 60000) return "just now";
  if (d < 3600000) return Math.floor(d / 60000) + "m ago";
  if (d < 86400000) return Math.floor(d / 3600000) + "h ago";
  return new Date(ms).toLocaleDateString();
}

/* ---------- login & boot ---------- */
async function tryLogin(key) {
  App.key = key;
  const r = await fetch("/api/devices", { headers: { "Authorization": "Bearer " + key } });
  if (!r.ok) throw new Error("Wrong access key");
  return true;
}
function showApp() {
  $("#login").style.display = "none";
  buildNav(); Files.bind(); Desktop.bind(); bindClipboard(); Notif.bind(); Pair.bind();
  $("#deviceSel").onchange = (e) => switchDevice(e.target.value);
  connect();
  go("term");
}
async function boot() {
  // Android WebView injects the key; browser uses ?key= or localStorage.
  const urlKey = new URLSearchParams(location.search).get("key");
  const injected = window.BRIDGEDESK_KEY;
  const stored = localStorage.getItem("bd_key");
  const auto = injected || urlKey || stored;
  if (auto) {
    try { await tryLogin(auto); localStorage.setItem("bd_key", auto); showApp(); return; } catch (e) {}
  }
  const doLogin = async () => {
    const k = $("#loginKey").value.trim(); if (!k) return;
    $("#loginErr").textContent = "";
    try { await tryLogin(k); localStorage.setItem("bd_key", k); showApp(); }
    catch (e) { $("#loginErr").textContent = e.message; }
  };
  $("#loginBtn").onclick = doLogin;
  $("#loginKey").onkeydown = (e) => { if (e.key === "Enter") doLogin(); };
  try { const h = await (await fetch("/api/hello")).json(); $("#loginSub").textContent = `Connect to ${h.name} (${h.os}). Enter the access key shown on that PC.`; } catch (e) {}
}
boot();
