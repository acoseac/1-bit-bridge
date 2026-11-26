// The player's lists under node (B1021 PR 3a): the SHIPPED boot.js, which
// boots the player as a page does, and views.js's lists, over a page model,
// driven scenario by scenario. Each scenario reports named checks, as
// bar_harness.mjs's do; player_output_list_test.go runs one scenario a test.
//
//   node list_harness.mjs <player module directory URL> <scenario> [app.js boost script]
//
// The model: a session history (entries of a state and a URL; a push drops
// the entries ahead; go and back dispatch popstate after a turn); the
// window's scroll, clamped to the document's height, its scroll events
// delivered as settle() runs; each list row's box from its index (a column
// count and a row height the case sets, the list's top, less the scroll),
// the pinned edge the list's rows carry as their scroll-margin-top; an
// IntersectionObserver that answers when the sentinel is within its margin;
// requestAnimationFrame queued and run by settle(); the shell's elements by
// id; every timer of 50 ms or more held until the case advances the clock
// (the filter's pause, the place writer's 500 ms); and a fetch that answers
// the list endpoints by offset, limit, sort and q from a generated library.
// What boot.js needs to import and boot and is not a list's is stubbed: the
// audio element's media methods, navigator.mediaSession, the bridge
// player's GET now (no player offered, unless a case holds it), and the
// album and artist pages' detail answers (no tracks). A reload keeps the
// tab's history and its session storage and makes everything else anew, the
// modules from a copy of their directory, since node evaluates a module once
// per URL.

import { cpSync, mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
import vm from "node:vm";

const [, , dirURL, scenarioName, boostScript] = process.argv;
const R = { pass: [], fail: [] };
const check = (name, cond, detail) => {
  if (cond) R.pass.push(name);
  else R.fail.push({ name, detail: detail === undefined ? "" : JSON.stringify(detail) });
};
const realTimeout = globalThis.setTimeout;
const realClear = globalThis.clearTimeout;
const turn = () => new Promise((r) => realTimeout(r, 0));

// ---- Selectors, as far as the modules and app.js's boost router ask ----

function simple(s) {
  const t = { tag: null, id: null, cls: [], attrs: [] };
  let rest = s;
  const tag = /^[a-zA-Z][\w-]*/.exec(rest);
  if (tag) { t.tag = tag[0].toUpperCase(); rest = rest.slice(tag[0].length); }
  while (rest) {
    let m;
    if ((m = /^#([\w-]+)/.exec(rest))) t.id = m[1];
    else if ((m = /^\.([\w-]+)/.exec(rest))) t.cls.push(m[1]);
    else if ((m = /^\[([\w-]+)(?:="([^"]*)")?\]/.exec(rest))) t.attrs.push([m[1], m[2]]);
    else throw new Error(`the model reads no selector ${s}`);
    rest = rest.slice(m[0].length);
  }
  return (n) => n instanceof El && (!t.tag || n.tagName === t.tag) && (!t.id || n.attrs.id === t.id) &&
    t.cls.every((c) => n.classList.contains(c)) && t.attrs.every(([k, v]) => (v === undefined ? k in n.attrs : n.attrs[k] === v));
}
const compiled = new Map();
function matcher(sel) {
  if (!compiled.has(sel)) {
    const alts = sel.split(",").map((a) => a.trim().split(/\s+/).map(simple));
    compiled.set(sel, (n) => alts.some((parts) => {
      if (!parts[parts.length - 1](n)) return false;
      let k = parts.length - 2;
      for (let x = n.parentNode; k >= 0 && x; x = x.parentNode) if (parts[k](x)) k--;
      return k < 0;
    }));
  }
  return compiled.get(sel);
}

// ---- Elements ----

let doc = null;
const bubbling = new Set(["click", "keydown", "input", "change", "pointerdown", "submit"]);

class El {
  constructor(tag) {
    this.tagName = String(tag).toUpperCase();
    this.children = []; this.parentNode = null; this.attrs = {}; this.listeners = {};
    this.className = ""; this.dataset = {}; this.own = ""; this.value = ""; this.selectionStart = 0;
    this.style = { setProperty() {}, removeProperty() {}, getPropertyValue: () => "" };
    const self = this;
    this.classList = {
      add: (...c) => { for (const x of c) if (!self.classList.contains(x)) self.className = `${self.className} ${x}`.trim(); },
      remove: (...c) => { self.className = self.className.split(/\s+/).filter((x) => x && !c.includes(x)).join(" "); },
      toggle: (c, on) => { const want = on === undefined ? !self.classList.contains(c) : !!on; if (want) self.classList.add(c); else self.classList.remove(c); return want; },
      contains: (c) => self.className.split(/\s+/).includes(c),
    };
  }
  get id() { return this.attrs.id || ""; }
  set id(v) { this.attrs.id = String(v); }
  get disabled() { return "disabled" in this.attrs; }
  set disabled(v) { if (v) this.attrs.disabled = ""; else delete this.attrs.disabled; }
  get hidden() { return "hidden" in this.attrs; }
  set hidden(v) { if (v) this.attrs.hidden = ""; else delete this.attrs.hidden; }
  get href() { return this.attrs.href === undefined ? "" : new URL(this.attrs.href, "http://x/").href; }
  set href(v) { this.setAttribute("href", v); }
  get target() { return this.attrs.target || ""; }
  get textContent() { return this.children.length ? this.children.map((c) => c.textContent).join("") : this.own; }
  set textContent(v) { for (const c of [...this.children]) this.removeChild(c); this.own = String(v); }
  setAttribute(k, v) {
    this.attrs[k] = String(v);
    if (k.startsWith("data-")) this.dataset[k.slice(5).replace(/-(\w)/g, (_, c) => c.toUpperCase())] = String(v);
  }
  getAttribute(k) { return k in this.attrs ? this.attrs[k] : null; }
  removeAttribute(k) { delete this.attrs[k]; }
  hasAttribute(k) { return k in this.attrs; }
  get attributes() { return Object.entries(this.attrs).map(([name, value]) => ({ name, value })); }
  appendChild(c) {
    if (c.tagName === "#FRAGMENT") { for (const x of [...c.children]) this.appendChild(x); return c; }
    if (c.parentNode) c.parentNode.removeChild(c);
    this.children.push(c);
    c.parentNode = this;
    return c;
  }
  append(...cs) { for (const c of cs) this.appendChild(typeof c === "string" ? text(c) : c); }
  insertBefore(c, ref) {
    if (!ref) return this.appendChild(c);
    if (c.parentNode) c.parentNode.removeChild(c);
    this.children.splice(this.children.indexOf(ref), 0, c);
    c.parentNode = this;
    return c;
  }
  // A focused element taken out of the document leaves the focus on the
  // body, as a browser's focus fixup does.
  removeChild(c) {
    this.children = this.children.filter((x) => x !== c);
    c.parentNode = null;
    if (doc && c.contains(doc.activeElement)) doc.activeElement = doc.body;
    return c;
  }
  remove() { if (this.parentNode) this.parentNode.removeChild(this); }
  replaceWith(n) { const p = this.parentNode; if (p) { p.insertBefore(n, this); p.removeChild(this); } }
  get firstChild() { return this.children[0] || null; }
  get childElementCount() { return this.children.length; }
  get isConnected() { let x = this; while (x.parentNode) x = x.parentNode; return !!doc && x === doc.documentElement; }
  contains(n) { for (let x = n; x; x = x.parentNode) if (x === this) return true; return false; }
  closest(sel) { const m = matcher(sel); for (let x = this; x; x = x.parentNode) if (m(x)) return x; return null; }
  querySelector(sel) { return this.querySelectorAll(sel)[0] || null; }
  querySelectorAll(sel) {
    const m = matcher(sel);
    const out = [];
    const walk = (n) => { for (const c of n.children) { if (m(c)) out.push(c); walk(c); } };
    walk(this);
    return out;
  }
  addEventListener(t, fn) { (this.listeners[t] ||= []).push(fn); }
  removeEventListener(t, fn) { this.listeners[t] = (this.listeners[t] || []).filter((x) => x !== fn); }
  // fire dispatches an event at the element, bubbling through its ancestors
  // to the document and the window where the event bubbles.
  fire(type, init = {}) {
    const ev = { type, target: this, button: 0, detail: 1, defaultPrevented: false, bubbles: bubbling.has(type), ...init };
    ev.preventDefault = () => { ev.defaultPrevented = true; };
    ev.stopPropagation = () => {};
    for (let n = this; n; n = ev.bubbles ? n.parentNode : null) for (const fn of [...(n.listeners[type] || [])]) fn(ev);
    if (ev.bubbles && doc) {
      for (const fn of [...(doc.listeners[type] || [])]) fn(ev);
      for (const fn of [...(win.listeners[type] || [])]) fn(ev);
    }
    return ev;
  }
  click() { if (!this.disabled) this.fire("click", { detail: 1 }); }
  focus() { if (doc) doc.activeElement = this; }
  blur() { if (doc && doc.activeElement === this) doc.activeElement = doc.body; }
  getBoundingClientRect() { return rectOf(this); }
  scrollIntoView() { if (isList(this)) { codeScrolls.push(["intoView", top(this)]); scrollTo(top(this) - layout.pinned); } }
  set innerHTML(h) { for (const c of [...this.children]) this.removeChild(c); if (h === "SHELL") shell(this); else if (h) this.append(mk("h1", {}, "Stats")); }
  get offsetHeight() { return 0; }
  get clientWidth() { return 0; }
  get scrollWidth() { return 0; }
  load() {}
  play() { return Promise.resolve(); }
  pause() {}
  canPlayType() { return "probably"; }
}
const text = (s) => { const t = new El("#text"); t.own = String(s); return t; };
function mk(tag, attrs = {}, ...children) {
  const n = new El(tag);
  for (const [k, v] of Object.entries(attrs)) { if (k === "class") n.className = v; else n.setAttribute(k, v); }
  n.append(...children);
  return n;
}

// ---- Layout: the list's rows in a grid of `cols`, `rowH` tall, from `listTop` ----

const layout = { cols: 5, rowH: 200, listTop: 400, tail: 300, pageH: 1600, pinned: 65, innerHeight: 800 };
let sy = 0;
let pendingScroll = false;
const codeScrolls = [];
const isList = (n) => n.isConnected && (n.classList.contains("grid") || n.classList.contains("rows")) && !!n.closest("#player-view");
const listEl = () => doc?.getElementById("player-view")?.querySelectorAll(".grid, .rows").find(isList) || null;
const listHeight = (c) => Math.ceil(c.children.length / layout.cols) * layout.rowH;
const top = (c) => layout.listTop;
const docHeight = () => { const c = listEl(); return c ? layout.listTop + listHeight(c) + layout.tail : layout.pageH; };
const maxScroll = () => Math.max(0, docHeight() - layout.innerHeight);
function scrollTo(y) {
  const v = Math.max(0, Math.min(Math.round(y), maxScroll()));
  if (v !== sy) { sy = v; pendingScroll = true; }
}
const zero = { top: 0, bottom: 0, left: 0, right: 0, width: 0, height: 0, x: 0, y: 0 };
function rectOf(n) {
  const c = n.parentNode && isList(n.parentNode) ? n.parentNode : null;
  if (c) {
    const t = layout.listTop + Math.floor(c.children.indexOf(n) / layout.cols) * layout.rowH - win.scrollY;
    return { ...zero, top: t, bottom: t + layout.rowH, y: t, height: layout.rowH, width: 100, right: 100 };
  }
  if (isList(n)) { const t = layout.listTop - win.scrollY; return { ...zero, top: t, y: t, bottom: t + listHeight(n), height: listHeight(n) }; }
  if (n.classList.contains("sentinel") && n.isConnected) {
    const c2 = listEl();
    const t = layout.listTop + (c2 ? listHeight(c2) : 0) - win.scrollY;
    return { ...zero, top: t, y: t, bottom: t + 1, height: 1 };
  }
  return { ...zero };
}

// ---- The window, its history and its location ----

const H = { entries: [], index: 0, pushes: 0, replaces: 0, gos: [] };
const loc = {
  origin: "http://x", hash: "", pathname: "/", search: "",
  get href() { return `http://x${this.pathname}${this.search}`; },
  assign(h) { R.assigned = h; },
};
function setURL(url) {
  const u = new URL(String(url), "http://x/");
  loc.pathname = u.pathname;
  loc.search = u.search;
}
const here = () => H.entries[H.index];
const history = {
  get state() { return here().state; },
  get length() { return H.entries.length; },
  scrollRestoration: "auto",
  pushState(state, _t, url) {
    H.pushes += 1;
    H.entries.splice(H.index + 1);
    H.entries.push({ state: structuredClone(state), url: new URL(String(url), loc.href).pathname + new URL(String(url), loc.href).search });
    H.index += 1;
    setURL(here().url);
  },
  replaceState(state, _t, url) {
    H.replaces += 1;
    const next = url === undefined || url === null ? here().url : new URL(String(url), loc.href).pathname + new URL(String(url), loc.href).search;
    H.entries[H.index] = { state: structuredClone(state), url: next };
    setURL(next);
  },
  go(n) {
    H.gos.push(n);
    realTimeout(() => {
      const to = H.index + n;
      if (to < 0 || to >= H.entries.length || n === 0) return;
      H.index = to;
      setURL(here().url);
      win.dispatchEvent({ type: "popstate", state: here().state });
    }, 0);
  },
  back() { this.go(-1); },
  forward() { this.go(1); },
};
const win = {
  listeners: {},
  addEventListener(t, fn) { (this.listeners[t] ||= []).push(fn); },
  removeEventListener(t, fn) { this.listeners[t] = (this.listeners[t] || []).filter((x) => x !== fn); },
  dispatchEvent(e) { for (const fn of [...(this.listeners[e.type] || [])]) fn(e); return true; },
  get scrollY() { if (sy > maxScroll()) { sy = maxScroll(); pendingScroll = true; } return sy; },
  get pageYOffset() { return this.scrollY; },
  get innerHeight() { return layout.innerHeight; },
  innerWidth: 390,
  scrollTo(a, b) { const y = typeof a === "object" ? a.top : b; codeScrolls.push(["scrollTo", y]); scrollTo(y); },
  location: loc,
  history,
};
globalThis.window = win;
globalThis.location = loc;
globalThis.history = history;
globalThis.getComputedStyle = (n) => ({ scrollMarginTop: isList(n) ? `${layout.pinned}px` : "0px", position: "static" });
const store = new Map();
globalThis.sessionStorage = {
  getItem: (k) => (store.has(k) ? store.get(k) : null),
  setItem: (k, v) => { store.set(k, String(v)); },
  removeItem: (k) => { store.delete(k); },
};
const session = { handlers: {}, metadata: undefined, playbackState: "", setActionHandler(a, fn) { this.handlers[a] = fn; }, setPositionState() {} };
Object.defineProperty(globalThis, "navigator", { value: { mediaSession: session }, configurable: true, writable: true });
globalThis.MediaMetadata = class { constructor(m) { Object.assign(this, m); } };
globalThis.setInterval = () => 0;
globalThis.clearInterval = () => {};

// ---- Frames, the observer and the clock ----

const frames = [];
globalThis.requestAnimationFrame = (f) => { frames.push(f); return frames.length; };
globalThis.cancelAnimationFrame = () => {};
const observers = new Set();
globalThis.IntersectionObserver = class {
  constructor(cb, o = {}) { this.cb = cb; this.margin = parseInt(o.rootMargin, 10) || 0; this.targets = new Map(); observers.add(this); }
  observe(t) { this.targets.set(t, undefined); }
  disconnect() { this.targets.clear(); observers.delete(this); }
  check() {
    let fired = false;
    for (const [t, last] of this.targets) {
      const now = t.isConnected && rectOf(t).top < layout.innerHeight + this.margin;
      if (now !== last) { this.targets.set(t, now); this.cb([{ isIntersecting: now, target: t }]); fired = true; }
    }
    return fired;
  }
};
let clock = 0;
let timerSeq = 1e6;
const held = new Map();
globalThis.setTimeout = (fn, ms, ...a) => {
  if (!(ms >= 50)) return realTimeout(fn, ms, ...a);
  timerSeq += 1;
  held.set(timerSeq, { fn, at: clock + ms, a });
  return timerSeq;
};
globalThis.clearTimeout = (id) => { if (!held.delete(id)) realClear(id); };
// advance moves the held clock, firing each timer due by then in its order.
async function advance(ms) {
  const end = clock + ms;
  for (;;) {
    const due = [...held.entries()].filter(([, t]) => t.at <= end).sort((x, y) => x[1].at - y[1].at)[0];
    if (!due) break;
    held.delete(due[0]);
    clock = due[1].at;
    due[1].fn(...due[1].a);
    await settle();
  }
  clock = end;
  await settle();
}
let inflight = 0;
// settle runs what the page has queued until nothing is left: tasks,
// scroll events, frames and the observer's answers.
async function settle() {
  for (let i = 0; i < 400; i++) {
    for (let k = 0; k < 4; k++) await turn();
    let did = false;
    if (pendingScroll) { pendingScroll = false; win.dispatchEvent({ type: "scroll" }); did = true; }
    if (frames.length) { for (const f of frames.splice(0)) f(0); did = true; }
    for (const o of [...observers]) if (o.check()) did = true;
    if (!did && inflight === 0) return;
  }
}

// ---- The library and the console's list endpoints ----

const LETTERS = "ABCDEFGHIJKLMNOPQRSTUVWXYZ";
const hex = (n) => n.toString(16).padStart(16, "0");
const fold = (s) => s.replace(/^the /i, "").normalize("NFD").replace(/\p{M}/gu, "").toUpperCase().replace(/[^\p{L}\p{N}]/gu, "");
const SNAP = "2026-11-26T08:00:00Z";
const lib = { albums: [], artists: [], genres: [], composers: [], playlists: [], mixes: [] };
function library(o = {}) {
  const n = { albums: 1400, artists: 400, genres: 80, composers: 80, playlists: 6, mixes: 12, ...o };
  lib.albums = Array.from({ length: n.albums }, (_, i) => ({
    id: hex(i + 1), title: `${LETTERS[i % 26]} Album ${String(i).padStart(4, "0")}`,
    albumArtist: i % 50 === 7 ? "The Brass Lanterns" : `Artist ${String(i % n.artists).padStart(3, "0")}`,
    artistId: hex(0x10000 + (i % n.artists)), year: 1950 + (i % 70), addedAt: n.albums - i, genre: i % n.genres,
  }));
  lib.artists = Array.from({ length: n.artists }, (_, j) => ({
    id: hex(0x10000 + j), name: `${LETTERS[j % 26]} Artist ${String(j).padStart(3, "0")}`, albumCount: 1, trackCount: 1,
  })).sort((x, y) => fold(x.name).localeCompare(fold(y.name)));
  lib.genres = Array.from({ length: n.genres }, (_, g) => ({ id: hex(0x20000 + g), name: `Genre ${String(g).padStart(2, "0")}`, albumCount: 3, trackCount: 1000 - g, g }));
  lib.composers = Array.from({ length: n.composers }, (_, c) => ({ id: hex(0x30000 + c), name: `${LETTERS[c % 26]} Composer ${c}`, albumCount: 2, trackCount: 2 }))
    .sort((x, y) => fold(x.name).localeCompare(fold(y.name)));
  lib.playlists = Array.from({ length: n.playlists }, (_, k) => ({ id: `p${k}`, name: `Playlist ${k}`, count: 10 }));
  lib.mixes = Array.from({ length: n.mixes }, (_, k) => ({ id: `m${k}`, name: `Mix ${k}`, count: 10 }));
}
const buckets = (rows, letter) => {
  const out = [];
  rows.forEach((r, i) => {
    const k = letter(r);
    if (out.length && out[out.length - 1].key === k) out[out.length - 1].count += 1;
    else out.push({ key: k, offset: i, count: 1 });
  });
  return out;
};
const initial = (s) => (/^[A-Z]/.test(fold(s)) ? fold(s)[0] : "#");
function page(rows, q, letter, key) {
  const offset = Number(q.get("offset") || 0);
  const limit = Math.min(Number(q.get("limit") || 60), 200);
  const body = { total: rows.length, offset, limit, snapshotAt: SNAP, [key]: rows.slice(offset, offset + limit) };
  if (offset === 0 && letter) body.buckets = buckets(rows, letter);
  return body;
}
const named = (rows, q, ...fields) => {
  const f = fold(q.get("q") || "");
  return f ? rows.filter((r) => fields.some((x) => fold(r[x]).includes(f))) : rows;
};
function answer(e) {
  const q = e.query;
  const p = e.path;
  if (e.partial) {
    const player = win.__player?.isPlayerPath(p);
    return { body: player ? "SHELL" : "OPERATOR", headers: { "X-Bridge-Active": player ? "player" : "stats", "X-Bridge-Section": player ? "player" : "server" } };
  }
  if (p === "/api/player/albums") {
    let rows = named(lib.albums, q, "title", "albumArtist");
    if (q.get("genre")) rows = rows.filter((a) => lib.genres[a.genre]?.id === q.get("genre"));
    const sort = q.get("sort") || "recent";
    if (sort === "title") rows = [...rows].sort((x, y) => fold(x.title).localeCompare(fold(y.title)));
    return { body: page(rows, q, sort === "title" ? (a) => initial(a.title) : null, "albums") };
  }
  let m;
  if ((m = /^\/api\/player\/albums\/(\w+)$/.exec(p))) {
    const a = lib.albums.find((x) => x.id === m[1]);
    return a ? { body: { album: a, tracks: [], variants: null } } : { status: 404, body: { error: "not_found" } };
  }
  if (p === "/api/player/artists") return { body: page(named(lib.artists, q, "name"), q, (a) => initial(a.name), "artists") };
  if ((m = /^\/api\/player\/artists\/(\w+)$/.exec(p))) {
    const a = lib.artists.find((x) => x.id === m[1]);
    return { body: { artist: { ...a, artistMBID: "" }, albums: lib.albums.filter((x) => x.artistId === m[1]).slice(0, 3), hasImage: false, variants: null } };
  }
  if (p === "/api/player/genres") return { body: page(named(lib.genres, q, "name"), q, null, "entries") };
  if (p === "/api/player/composers") return { body: page(named(lib.composers, q, "name"), q, (c) => initial(c.name), "entries") };
  if (p === "/api/player/playlists") return { body: { collections: named(lib.playlists, q, "name"), snapshotAt: SNAP } };
  if (p === "/api/player/mixes") return { body: { enabled: true, collections: lib.mixes, snapshotAt: SNAP } };
  if (p === "/api/playlists/deleted") return { body: { deleted: [] } };
  if (p === "/api/player/sources") return { body: { sources: [{ id: "nas", name: "NAS" }] } };
  if (p === "/api/settings") return { body: { allowDelete: false } };
  if (p.startsWith("/api/output/player")) return { status: 503, body: { error: "player_not_offered" } };
  return { status: 404, body: { error: "not_found" } };
}
const log = [];
const holds = new Map();
// hold makes the next request for path (whose query meets when, if given)
// wait until the case lets it go.
function hold(path, when = null) {
  let open;
  const p = new Promise((r) => { open = r; });
  holds.set(path, { p, when });
  return () => open();
}
const fails = new Set();
// fail makes the next request for path answer 500.
function fail(path) { fails.add(path); }
globalThis.fetch = async (url, init = {}) => {
  const u = new URL(String(url), "http://x/");
  const e = { path: u.pathname, query: u.searchParams, partial: !!init.headers?.["X-Bridge-Partial"] };
  log.push(e);
  const gate = holds.get(e.path);
  if (gate && (!gate.when || gate.when(e.query))) {
    holds.delete(e.path);
    await new Promise((resolve, reject) => {
      gate.p.then(resolve);
      init.signal?.addEventListener?.("abort", () => reject(Object.assign(new Error("aborted"), { name: "AbortError" })));
    });
  }
  inflight += 1;
  await turn();
  inflight -= 1;
  if (init.signal?.aborted) throw Object.assign(new Error("aborted"), { name: "AbortError" });
  const failed = fails.delete(e.path);
  const { status = 200, body = null, headers = {} } = failed ? { status: 500, body: { error: "internal", message: "The list could not be read." } } : answer(e);
  const lower = Object.fromEntries(Object.entries(headers).map(([k, v]) => [k.toLowerCase(), v]));
  return {
    ok: status >= 200 && status < 300, status, statusText: "", redirected: false,
    headers: { get: (n) => lower[n.toLowerCase()] ?? null },
    json: async () => body, text: async () => String(body),
  };
};
const listGets = (path) => log.filter((e) => e.path === path && !e.partial);

// ---- The page ----

// shell builds templates/player.html's mount points inside main.
function shell(main) {
  const seed = mk("script", { type: "application/json", id: "player-seed" });
  seed.textContent = "{}";
  main.append(seed,
    mk("div", { id: "player-root", class: "player", "data-booting": "1" },
      mk("div", { class: "player-nav" }, mk("nav", { id: "player-sections" })),
      mk("div", { class: "player-main" },
        mk("form", { id: "player-search-form", class: "player-search" }, mk("input", { id: "player-search-input", type: "search" })),
        mk("nav", { id: "player-crumb", class: "player-crumb" }),
        mk("h1", { id: "player-title", class: "player-title" }, "Library"),
        mk("div", { id: "player-toolbar" }),
        mk("div", { id: "player-view", class: "player-view" }))),
    mk("p", { id: "player-live" }));
}

// rig opens the page at path and boots the shipped player there, as a page
// load does: one entry, its state null (or `state`, an entry an older build
// recorded).
async function rig(path = "/albums", o = {}) {
  library(o.library);
  Object.assign(layout, o.layout || {});
  H.entries = [{ state: o.state ?? null, url: path }];
  H.index = 0;
  setURL(path);
  makePage();
  if (o.output) store.set("bridge-player-output", o.output);
  const nowHeld = o.holdNow ? hold("/api/output/player/now") : null;
  const views = await import(new URL("views.js", dirURL));
  await import(new URL("boot.js", dirURL));
  if (boostScript) vm.runInThisContext(readFileSync(boostScript, "utf8"), { filename: "app-boost.js" });
  await settle();
  return { views, nowHeld };
}

// reload loads the page again where it stands, as a browser's reload does:
// the tab's history (its entries and their states) and its session storage
// stay; the document, the window's listeners, the timers, the frames, the
// observers and every module are made anew, the modules from a copy of their
// directory.
const copies = [];
process.on("exit", () => { for (const d of copies) rmSync(d, { recursive: true, force: true }); });
async function reload() {
  await settle();
  for (const k of Object.keys(win.listeners)) delete win.listeners[k];
  held.clear();
  frames.length = 0;
  observers.clear();
  holds.clear();
  sy = 0;
  pendingScroll = false;
  setURL(here().url);
  makePage();
  const tmp = mkdtempSync(join(tmpdir(), "list-harness-"));
  copies.push(tmp);
  cpSync(fileURLToPath(dirURL), join(tmp, "player"), { recursive: true });
  const dir = pathToFileURL(join(tmp, "player") + "/");
  await import(new URL("views.js", dir));
  await import(new URL("boot.js", dir));
  await settle();
}

// makePage builds the document a player page loads into: the shell's
// header, main and the player's mount points.
function makePage() {
  const html = new El("html");
  const body = new El("body");
  html.appendChild(body);
  const main = mk("main");
  body.append(mk("header", { class: "sidebar" },
    mk("nav", { id: "primary-nav" }, mk("a", { href: "/stats", "data-tab": "stats" }, "Stats"), mk("a", { href: "/albums", "data-tab": "player" }, "Browse"))), main);
  shell(main);
  doc = {
    documentElement: html, body, activeElement: body, listeners: {}, visibilityState: "visible", title: "",
    createElement: (tag) => new El(tag),
    createElementNS: (_ns, tag) => new El(tag),
    createTextNode: text,
    createDocumentFragment: () => new El("#fragment"),
    getElementById: (id) => html.querySelectorAll(`#${id}`)[0] || null,
    querySelector: (sel) => html.querySelector(sel),
    querySelectorAll: (sel) => html.querySelectorAll(sel),
    addEventListener(t, fn) { (this.listeners[t] ||= []).push(fn); },
    removeEventListener(t, fn) { this.listeners[t] = (this.listeners[t] || []).filter((x) => x !== fn); },
  };
  Object.defineProperty(html, "scrollHeight", { get: docHeight });
  globalThis.document = doc;
}

// The rows of the list on screen, and what the reader sees of them.
const rows = () => listEl()?.children || [];
const titleOf = (n) => (n?.querySelector(".tile-title") || n?.querySelector(".row-title"))?.textContent || "";
function topRow() {
  const rs = rows();
  for (let i = 0; i < rs.length; i++) {
    const r = rectOf(rs[i]);
    if (r.bottom > layout.pinned) return { title: titleOf(rs[i]), delta: r.top - layout.pinned };
  }
  return null;
}
// loadThrough scrolls the reader to the list's foot until row n is loaded.
async function loadThrough(n) {
  for (let i = 0; i < 200 && rows().length <= n; i++) {
    scrollTo(docHeight());
    await settle();
  }
}
// readerAt scrolls the reader so the list's row n stands delta below the
// pinned edge, as a reader's fling leaves it.
async function readerAt(n, delta) {
  await loadThrough(n);
  // Again once the observer has paged on below a scroll the list's end cut.
  const y = layout.listTop + Math.floor(n / layout.cols) * layout.rowH - layout.pinned - delta;
  for (let i = 0; i < 4 && win.scrollY !== y; i++) {
    scrollTo(y);
    await settle();
  }
}
const tile = (n) => rows()[n].closest("a") || rows()[n];
const crumb = (label) => doc.getElementById("player-crumb").querySelectorAll("a").find((a) => a.textContent === label);
const field = () => doc.getElementById("player-view").querySelector(".list-filter");
const viewText = () => doc.getElementById("player-view").textContent;
const button = (words) => doc.getElementById("player-view").querySelectorAll("button").find((b) => b.textContent === words);
async function navigate(n) { n.fire("click", { detail: 1 }); await settle(); }
async function back() { history.back(); await settle(); }
async function forward() { history.forward(); await settle(); }
async function type(input, s) {
  for (const ch of s) {
    input.value += ch;
    input.selectionStart = input.value.length;
    input.fire("input");
    await advance(100);
  }
}
const title = () => doc.getElementById("player-title").textContent;
const same = (a, b) => JSON.stringify(a) === JSON.stringify(b);

// ---- Scenarios ----

const S = {};

S.TestBackReturnsTheAlbumListToItsPlace = async () => {
  await rig("/albums");
  await readerAt(150, -37);
  const before = topRow();
  check("the reader's top row is album 150, 37px under the edge", before?.title === "U Album 0150" && before.delta === -37, before);
  await navigate(tile(150));
  check("its tile opens the album", title() === "U Album 0150" && location.pathname.startsWith("/album/"), [title(), location.pathname]);
  const asked = log.length;
  const scrolls = codeScrolls.length;
  await back();
  const after = topRow();
  check("Back: row 150 is the top row again, at its delta", same(after, before), [after, before]);
  check("the window starts at the list's first row", titleOf(rows()[0]) === "A Album 0000", titleOf(rows()[0]));
  check("rows 0 to 150 are loaded", rows().length >= 151, rows().length);
  const gets = listGets("/api/player/albums").filter((e) => log.indexOf(e) >= asked);
  check("the restore asks pages of 200 from 0", gets[0]?.query.get("offset") === "0" && gets[0]?.query.get("limit") === "200", gets.map((e) => e.query.toString()));
  check("one list scroll", codeScrolls.length - scrolls === 1, codeScrolls.slice(scrolls));

  // A place at row 199 of one column, more rows behind it: the rows below it
  // are loaded before the one scroll, and it stands after the observer pages on.
  layout.cols = 1;
  await readerAt(199, -12);
  const deep = topRow();
  check("the reader's top row is album 199, 12px under the edge", deep?.title === "R Album 0199" && deep.delta === -12, deep);
  await navigate(tile(199));
  const scrolls2 = codeScrolls.length;
  await back();
  check("a place at row 199 stands at its delta once the observer has paged on", same(topRow(), deep) && rows().length > 200, [topRow(), deep, rows().length]);
  check("and its restore scrolled once", codeScrolls.length - scrolls2 === 1, codeScrolls.slice(scrolls2));

  // A place 1,200 rows in: the window is re-based at the place's page, with
  // a way back to the list's first row.
  layout.cols = 5;
  await readerAt(1200, 0);
  const far = topRow();
  check("the reader's top row is album 1200", far?.title === "E Album 1200" && far.delta === 0, far);
  await navigate(tile(1200));
  await back();
  check("a place past 1,000 rows comes back with its window re-based at its page", same(topRow(), far) && titleOf(rows()[0]) === "E Album 1200", [topRow(), far, titleOf(rows()[0])]);
  const toTop = button("Back to the top");
  check("a re-based window offers Back to the top", !!toTop && !toTop.hidden, !!toTop);
  await navigate(toTop);
  check("Back to the top reaches the list's first row", titleOf(rows()[0]) === "A Album 0000" && topRow()?.title === "A Album 0000", [titleOf(rows()[0]), topRow()]);
  const pagedFrom = log.length;
  await loadThrough(60);
  check("and the list it draws pages on past its first 60 rows as the reader scrolls to their end",
    rows().length > 60 && listGets("/api/player/albums").some((e) => log.indexOf(e) >= pagedFrom && e.query.get("offset") === "60"),
    [rows().length, listGets("/api/player/albums").filter((e) => log.indexOf(e) >= pagedFrom).map((e) => e.query.toString())]);
};

S.TestBackReturnsTheArtistAndAxisListsToTheirPlace = async () => {
  await rig("/artists");
  await readerAt(150, -20);
  const artist = topRow();
  await navigate(tile(150));
  check("an artist tile opens the artist", location.pathname.startsWith("/artist/"), location.pathname);
  await back();
  check("Back returns the artists' grid to its top row", same(topRow(), artist), [topRow(), artist]);

  layout.cols = 1;
  layout.rowH = 48;
  doc.getElementById("player-sections").querySelectorAll("a").find((a) => a.dataset.section === "genres").fire("click", { detail: 1 });
  await settle();
  await readerAt(40, -10);
  const genre = topRow();
  check("the genres' rows stand at row 40", genre?.title === "Genre 40", genre);
  await navigate(tile(40));
  await back();
  check("Back returns the genres' rows to their top row", same(topRow(), genre), [topRow(), genre]);

  layout.rowH = 300;
  doc.getElementById("player-sections").querySelectorAll("a").find((a) => a.dataset.section === "playlists").fire("click", { detail: 1 });
  await settle();
  await readerAt(3, 0);
  const playlist = topRow();
  await navigate(tile(3));
  await back();
  check("Back returns the playlists, one page, to their top row", same(topRow(), playlist) && playlist?.title === "Playlist 3", [topRow(), playlist]);
};

S.TestTheCrumbReturnsToTheEntryTheReaderCameFrom = async () => {
  await rig("/albums");
  await readerAt(150, -37);
  const before = topRow();
  await navigate(tile(150));
  const length = history.length;
  await navigate(crumb("Albums"));
  check("the album's Albums crumb goes back one entry", same(H.gos, [-1]) && history.length === length && location.pathname === "/albums", [H.gos, history.length, length]);
  check("and its list stands at its place", same(topRow(), before), [topRow(), before]);
  await forward();
  check("Forward reaches the album again", location.pathname.startsWith("/album/") && title() === "U Album 0150", [location.pathname, title()]);
  await navigate(doc.getElementById("player-view").querySelector(".detail-artist"));
  check("the album's byline opens its artist", location.pathname.startsWith("/artist/"), location.pathname);
  await navigate(crumb("Albums"));
  check("the artist's Albums crumb goes back two entries to the list at its place", same(H.gos.slice(-1), [-2]) && same(topRow(), before), [H.gos, topRow(), before]);

  // Back, a filter edited in place, Forward: the list's entry no longer holds
  // the URL the album's trail recorded, and the crumb pushes.
  await forward();
  await back();
  const f = field();
  f.focus();
  await type(f, "x");
  await advance(400);
  check("the filter edited the list's own entry", location.search === "?q=x", location.search);
  await forward();
  const pushed = H.pushes;
  const gos = H.gos.length;
  await navigate(crumb("Albums"));
  check("its crumb then pushes the URL it names, never going back to the edited list",
    H.pushes === pushed + 1 && H.gos.length === gos && location.pathname === "/albums" && location.search === "", [H.pushes - pushed, H.gos.slice(gos), location.search]);
};

S.TestACrumbOutsideThisTabsHistoryPushesAsBefore = async () => {
  await rig(`/album/${hex(151)}`);
  scrollTo(500);
  await settle();
  let length = history.length;
  await navigate(crumb("Artists"));
  check("a structural crumb pushes", history.length === length + 1 && H.gos.length === 0 && location.pathname === "/artists", [history.length, length, H.gos]);
  check("and its list starts at its top", win.scrollY === 0 && titleOf(rows()[0]) === "A Artist 000", [win.scrollY, titleOf(rows()[0])]);

  // A trail an older build recorded, no depth or key on its entries.
  history.pushState({ scrollY: 0, trail: [{ label: "Albums", href: "/albums" }] }, "", `/album/${hex(152)}`);
  win.dispatchEvent({ type: "popstate" });
  await settle();
  length = history.length;
  await navigate(crumb("Albums"));
  check("a trail entry with no depth pushes", history.length === length + 1 && H.gos.length === 0 && location.pathname === "/albums", [history.length, length, H.gos]);
  check("and its list starts at its top", win.scrollY === 0 && titleOf(rows()[0]) === "A Album 0000", [win.scrollY, titleOf(rows()[0])]);

  // A source's root crumb, which no trail entry's URL is.
  history.pushState({ scrollY: 0 }, "", "/artists?source=nas");
  win.dispatchEvent({ type: "popstate" });
  await settle();
  await navigate(tile(3));
  length = history.length;
  await navigate(crumb("NAS"));
  check("a source root that is no trail entry's URL pushes", history.length === length + 1 && H.gos.length === 0 && location.search === "?source=nas" && location.pathname === "/albums", [history.length, length, location.href]);
  check("and its list starts at its top", win.scrollY === 0, win.scrollY);
};

S.TestARailJumpIsTheWindowBackRestores = async () => {
  await rig("/albums?sort=title");
  const letter = (k) => doc.getElementById("player-view").querySelectorAll(".az-letter").find((b) => b.textContent === k);
  letter("M").fire("click", { detail: 0 });
  await settle();
  check("the jump draws the list from M", titleOf(rows()[0]).startsWith("M"), titleOf(rows()[0]));
  scrollTo(win.scrollY + 830);
  await settle();
  const before = topRow();
  const first = titleOf(rows()[0]);
  await navigate(tile(rows().indexOf(rows().find((n) => titleOf(n) === before.title))));
  await back();
  check("Back: the list's first row is M's first", titleOf(rows()[0]) === first, [titleOf(rows()[0]), first]);
  check("and the top row stands where it stood", same(topRow(), before), [topRow(), before]);
  check("the restored window keeps its rail, A among its live letters", !!letter("A") && !letter("A").disabled, !!letter("A"));
  letter("A").fire("click", { detail: 0 });
  await settle();
  check("a jump to A from the restored window draws the list from A", titleOf(rows()[0]) === "A Album 0000", titleOf(rows()[0]));
  const pagedFrom = log.length;
  await loadThrough(60);
  check("and the jump's window pages on past its first 60 rows as the reader scrolls to their end",
    rows().length > 60 && titleOf(rows()[0]) === "A Album 0000" && listGets("/api/player/albums").some((e) => log.indexOf(e) >= pagedFrom && e.query.get("offset") === "60"),
    [rows().length, listGets("/api/player/albums").filter((e) => log.indexOf(e) >= pagedFrom).map((e) => e.query.toString())]);
};

S.TestAPlaceIsRestoredOnlyOnTheURLItWasMeasuredOn = async () => {
  await rig("/albums");
  await readerAt(150, -37);
  await navigate(tile(150));
  await back();
  check("Back restored the place", topRow()?.title === "U Album 0150", topRow());
  const sort = doc.getElementById("player-toolbar").querySelectorAll("select").find((s) => s.attrs["aria-label"] === "sort");
  sort.value = "title";
  sort.fire("change");
  await settle();
  check("a sort change starts the list at its top", win.scrollY === 0 && topRow()?.title === "A Album 0000", [win.scrollY, topRow()]);
  await readerAt(100, 0);
  const f = field();
  f.focus();
  await type(f, "Album 1");
  await advance(400);
  check("a filter change starts the list at its top", topRow()?.title === titleOf(rows()[0]) && titleOf(rows()[0]).includes("Album 1"), [topRow(), titleOf(rows()[0])]);
};

S.TestAPlaceIsWrittenOnlyIntoItsOwnEntry = async () => {
  await rig("/artists");
  await readerAt(150, -20);
  const artists = topRow();
  const albumsLink = doc.getElementById("player-sections").querySelectorAll("a").find((a) => a.dataset.section === "albums");
  await navigate(albumsLink);
  await readerAt(120, -5);
  scrollTo(win.scrollY + 40);
  await settle();
  check("a write is pending as the reader goes Back", held.size > 0, held.size);
  await back();
  check("the pending write landed nowhere: the artists' entry holds their own place", here().state?.place?.href === "/artists" && same(topRow(), artists), [here().state?.place?.href, topRow(), artists]);

  // A route that is still rendering writes nothing into its entry.
  const release = hold("/api/player/albums");
  await navigate(albumsLink);
  const stamped = structuredClone(here().state);
  scrollTo(300);
  await settle();
  await advance(600);
  check("nothing is written into the entry while its list renders", same(here().state, stamped), [here().state, stamped]);
  release();
  await settle();
  check("the list renders once its answer comes", rows().length > 0, rows().length);
};

S.TestARedrawOfTheSameEntryKeepsTheListsPlace = async () => {
  const { nowHeld } = await rig("/albums", { output: "bridge", holdNow: true });
  await readerAt(150, -37);
  scrollTo(win.scrollY + 3);
  await settle();
  const before = topRow();
  check("a write is pending as the output changes", held.size > 0, held.size);
  const asked = log.length;
  nowHeld();
  await settle();
  check("the output's change redrew the route", listGets("/api/player/albums").some((e) => log.indexOf(e) >= asked), log.slice(asked).map((e) => e.path));
  check("the redraw keeps the list's top row where it stood", same(topRow(), before), [topRow(), before]);
};

S.TestBackFromAnOperatorPageReturnsTheListToItsPlace = async () => {
  if (!boostScript) {
    check("app.js's boost router is given", false, "no boost script");
    return;
  }
  await rig("/albums");
  await readerAt(150, -37);
  scrollTo(win.scrollY + 3);
  await settle();
  const before = topRow();
  check("a write is pending as the reader leaves", held.size > 0, held.size);
  await navigate(doc.getElementById("primary-nav").querySelector('a[href="/stats"]'));
  check("the operator link swapped the operator page in, its own entry pushed", location.pathname === "/stats" && !doc.getElementById("player-view") && here().state?.boost === true,
    [location.pathname, here().state]);
  await back();
  check("Back swaps the shell in and the list stands at its place", same(topRow(), before), [topRow(), before]);
};

S.TestTheFilterNarrowsTheListInPlace = async () => {
  await rig("/albums?sort=title");
  const f = field();
  check("the head holds a search field labelled for the list", f?.attrs.type === "search" && f.attrs["aria-label"] === "Filter albums" && f.attrs.placeholder === "Filter albums", f?.attrs);
  f.focus();
  const asked = log.length;
  const length = history.length;
  await type(f, "bra");
  check("three keystrokes inside the pause ask nothing", listGets("/api/player/albums").filter((e) => log.indexOf(e) >= asked).length === 0);
  await advance(400);
  const gets = listGets("/api/player/albums").filter((e) => log.indexOf(e) >= asked);
  check("the pause asks once, with q", gets.length === 1 && gets[0].query.get("q") === "bra", gets.map((e) => e.query.toString()));
  check("the same field keeps the focus and its value", field() === f && doc.activeElement === f && f.value === "bra" && f.selectionStart === 3, [field() === f, doc.activeElement === f, f.value]);
  check("q is written into the list's own entry, no new one", location.search === "?sort=title&q=bra" && history.length === length, [location.search, history.length]);
  const count = doc.getElementById("player-view").querySelector(".list-body").children[0].textContent;
  const letters = doc.getElementById("player-view").querySelectorAll(".az-letter").filter((b) => !b.disabled).map((b) => b.textContent);
  const brass = lib.albums.filter((a) => fold(a.title).includes("BRA") || fold(a.albumArtist).includes("BRA"));
  check("the count line and the rail are the filtered first page's", count === `${brass.length} albums` && same(letters, [...new Set(brass.map((a) => a.title[0]))].sort()),
    [count, letters]);
  check("the new count is announced", doc.getElementById("player-live").textContent === `${brass.length} albums`, doc.getElementById("player-live").textContent);
  f.fire("keydown", { key: "Escape" });
  await settle();
  check("Escape clears the field and the filter", f.value === "" && location.search === "?sort=title" && rows().length > 0, [f.value, location.search]);
  f.value = "zzzz";
  f.fire("keydown", { key: "Enter" });
  await settle();
  check("Enter commits at once, and a filter matching nothing says so with Clear", viewText().includes("No albums match “zzzz”") && !!button("Clear") && field() === f, viewText());
  await navigate(button("Clear"));
  check("Clear empties the field, keeps it focused and draws the list", f.value === "" && doc.activeElement === f && rows().length > 0 && location.search === "?sort=title", [f.value, location.search]);

  // A list opened on a filter that matches nothing keeps its head; a genre's
  // grid writes q alone into its URL; a redraw keeps the field focused.
  history.pushState({ scrollY: 0 }, "", "/artists?q=zzz");
  win.dispatchEvent({ type: "popstate" });
  await settle();
  check("a list opened on a filter matching nothing keeps its field", !!field() && field().value === "zzz" && viewText().includes("No artists match “zzz”"), viewText());
  history.pushState({ scrollY: 0 }, "", `/genre/${lib.genres[3].id}`);
  win.dispatchEvent({ type: "popstate" });
  await settle();
  field().focus();
  await type(field(), "0");
  await advance(400);
  check("a genre's album grid writes q alone into its URL", location.pathname === `/genre/${lib.genres[3].id}` && location.search === "?q=0", location.href);
  win.dispatchEvent(new CustomEvent("player:rerender"));
  await settle();
  check("a redraw while the field is focused keeps the focus in its field", doc.activeElement === field() && field().value === "0", [doc.activeElement?.className, field()?.value]);
  history.pushState({ scrollY: 0 }, "", "/playlists");
  win.dispatchEvent({ type: "popstate" });
  await settle();
  field().focus();
  await type(field(), "3");
  await advance(400);
  check("the playlists' filter shows the filtered collection and its count", rows().length === 1 && titleOf(rows()[0]) === "Playlist 3" && viewText().includes("1 playlist"), [rows().length, viewText()]);
};

S.TestAFilterPauseWritesNothingAfterANavigation = async () => {
  await rig("/albums");
  const f = field();
  f.focus();
  f.value = "b";
  f.fire("input");
  await settle();
  await navigate(tile(4));
  const url = location.pathname + location.search;
  const state = structuredClone(here().state);
  const asked = log.length;
  await advance(400);
  check("the album's URL and entry are as the tile left them", location.pathname + location.search === url && same(here().state, state), [location.href, here().state]);
  check("the pause asked for no list", listGets("/api/player/albums").filter((e) => log.indexOf(e) >= asked).length === 0, log.slice(asked).map((e) => e.path));
};

S.TestALateAnswerTouchesNoOperatorPage = async () => {
  if (!boostScript) {
    check("app.js's boost router is given", false, "no boost script");
    return;
  }
  // A restore whose rows come once an operator page has replaced the list.
  await rig("/albums");
  await readerAt(150, -37);
  await navigate(tile(150));
  const release = hold("/api/player/albums");
  await back();
  const stats = () => doc.getElementById("primary-nav").querySelector('a[href="/stats"]');
  await navigate(stats());
  check("an operator page replaced the list while its restore waited", location.pathname === "/stats" && !doc.getElementById("player-view") && here().state?.boost === true,
    [location.pathname, here().state]);
  let scrolls = codeScrolls.length;
  release();
  await settle();
  check("the restore's rows, come after it, scroll the operator page nowhere", codeScrolls.length === scrolls, codeScrolls.slice(scrolls));

  // A genre's albums, whose name comes once an operator page has replaced them.
  await back();
  doc.getElementById("player-sections").querySelectorAll("a").find((a) => a.dataset.section === "genres").fire("click", { detail: 1 });
  await settle();
  const named = hold("/api/player/genres");
  await navigate(tile(3));
  check("the genre's page waits for its name", location.pathname.startsWith("/genre/") && holds.size === 0, location.pathname);
  await navigate(stats());
  const titled = doc.title;
  scrolls = codeScrolls.length;
  named();
  await settle();
  check("the name, come after it, retitles the operator page with nothing", titled === "Stats" && doc.title === titled, [doc.title, titled]);
  check("and nothing scrolls the operator page after its own scroll", codeScrolls.length === scrolls, codeScrolls.slice(scrolls));
};

S.TestAListWritesNoPlaceUntilALoadsRowsStand = async () => {
  await rig("/albums");
  await readerAt(450, -20);
  const before = topRow();
  await navigate(tile(450));
  const saved = structuredClone(H.entries[H.index - 1].state.place);
  check("the list's place at row 450 is recorded", saved?.anchor === 450 && before?.title === "I Album 0450", [saved, before]);
  fail("/api/player/albums");
  await back();
  check("a restore that fails shows its error with Try again, the entry's place as it was",
    !!button("Try again") && same(here().state.place, saved), [viewText(), here().state.place]);

  // Try again, its second page held, and a row of its first page opened.
  const release = hold("/api/player/albums", (q) => q.get("offset") === "200");
  await navigate(button("Try again"));
  check("the retry's first page stands while its second is held", rows().length === 200, rows().length);
  await navigate(tile(10));
  release();
  check("leaving then keeps the place the restore was for in the list's entry", same(H.entries[H.index - 1].state.place, saved), [H.entries[H.index - 1].state.place, saved]);

  // Back, the restore failing again, a retry that stands, and a scroll.
  fail("/api/player/albums");
  await back();
  await navigate(button("Try again"));
  check("a retry that stands restores the place", same(topRow(), before), [topRow(), before]);
  scrollTo(win.scrollY + 3 * layout.rowH);
  await settle();
  await advance(600);
  check("and a scroll after it records the new place", here().state.place?.anchor === 465, here().state.place);

  // A restore superseded by a jump whose rows are held, and the list left.
  const sort = doc.getElementById("player-toolbar").querySelectorAll("select").find((s) => s.attrs["aria-label"] === "sort");
  sort.value = "title";
  sort.fire("change");
  await settle();
  await readerAt(450, 0);
  await navigate(tile(450));
  const deep = structuredClone(H.entries[H.index - 1].state.place);
  const restoring = hold("/api/player/albums", (q) => q.get("offset") === "200");
  await back();
  const atM = String([...lib.albums].sort((x, y) => fold(x.title).localeCompare(fold(y.title))).findIndex((a) => a.title.startsWith("M")));
  const jumping = hold("/api/player/albums", (q) => q.get("offset") === atM);
  doc.getElementById("player-view").querySelectorAll(".az-letter").find((b) => b.textContent === "M").fire("click", { detail: 0 });
  await settle();
  await navigate(doc.getElementById("player-sections").querySelectorAll("a").find((a) => a.dataset.section === "artists"));
  restoring();
  jumping();
  check("a restore a jump superseded, left while the jump's rows were held, keeps its place", deep?.anchor === 450 && same(H.entries[H.index - 1].state.place, deep),
    [H.entries[H.index - 1].state.place, deep]);
};

S.TestAWholeCollectionRestoresFromItsFirstRowByItsRowsID = async () => {
  await rig("/playlists", { library: { playlists: 1300 }, layout: { cols: 1, rowH: 300 } });
  const toTop = () => button("Back to the top");
  await readerAt(1200, 0);
  const far = topRow();
  check("the reader's top row is playlist 1200", far?.title === "Playlist 1200" && far.delta === 0, far);
  await navigate(tile(1200));
  const asked = log.length;
  await back();
  check("Back returns playlist 1200 to the top row, the whole collection from its first row", same(topRow(), far) && titleOf(rows()[0]) === "Playlist 0",
    [topRow(), far, titleOf(rows()[0])]);
  check("asked for once, with no way back to a top it never left",
    listGets("/api/player/playlists").filter((e) => log.indexOf(e) >= asked).length === 1 && toTop()?.hidden === true,
    [listGets("/api/player/playlists").filter((e) => log.indexOf(e) >= asked).length, toTop()?.hidden]);

  // A place an earlier build wrote over a window it re-based at 1,200.
  history.pushState({ scrollY: 0, place: { href: "/playlists", start: 1200, anchor: 1203, anchorId: "p3", delta: 0, snapshotAt: SNAP } }, "", "/playlists");
  win.dispatchEvent({ type: "popstate" });
  await settle();
  check("a place an earlier build re-based lands on its row, the window from the first row", topRow()?.title === "Playlist 3" && titleOf(rows()[0]) === "Playlist 0" && toTop()?.hidden === true,
    [topRow(), titleOf(rows()[0]), toTop()?.hidden]);
  scrollTo(win.scrollY + layout.rowH);
  await settle();
  await advance(600);
  check("and the place it records next starts its window at the first row", here().state.place?.start === 0 && here().state.place?.anchorId === "p4", here().state.place);

  // A playlist gone above the place's row: the row is found by its id.
  lib.playlists.splice(1, 1);
  history.pushState({ scrollY: 0, place: { href: "/playlists", start: 0, anchor: 10, anchorId: "p10", delta: 0, snapshotAt: SNAP } }, "", "/playlists");
  win.dispatchEvent({ type: "popstate" });
  await settle();
  check("a playlist gone above the place's row: the row is found by its id", topRow()?.title === "Playlist 10", topRow());

  // Smart Mixes, which their endpoint answers whole too.
  history.pushState({ scrollY: 0, place: { href: "/mixes", start: 1200, anchor: 1205, anchorId: "m5", delta: 0, snapshotAt: SNAP } }, "", "/mixes");
  win.dispatchEvent({ type: "popstate" });
  await settle();
  check("Smart Mixes land on the recorded mix, the window from their first row", topRow()?.title === "Mix 5" && titleOf(rows()[0]) === "Mix 0" && toTop()?.hidden === true,
    [topRow(), titleOf(rows()[0]), toTop()?.hidden]);
};

S.TestACrumbPushesWhereItsEntryNowHoldsAnotherURL = async () => {
  // A filter that matches nothing, the list at its top with no write pending.
  await rig("/albums");
  await navigate(tile(3));
  await back();
  check("Back returns the list at its top, nothing scrolled to write", win.scrollY === 0 && topRow()?.title === titleOf(rows()[0]), [win.scrollY, topRow()]);
  const f = field();
  f.focus();
  await type(f, "zzzz");
  await advance(400);
  check("a filter matching nothing moved the list's URL", location.search === "?q=zzzz" && viewText().includes("No albums match “zzzz”"), [location.search, viewText()]);
  await forward();
  let pushes = H.pushes;
  let gos = H.gos.length;
  await navigate(crumb("Albums"));
  check("the album's crumb then pushes the URL it names", H.pushes === pushes + 1 && H.gos.length === gos && location.pathname === "/albums" && location.search === "",
    [H.pushes - pushes, H.gos.slice(gos), location.href]);

  // A filter that matches, its answer held, and Forward the moment the URL moved.
  await navigate(tile(4));
  await back();
  const g = field();
  g.focus();
  const release = hold("/api/player/albums");
  await type(g, "x");
  await advance(400);
  check("the URL moved, the filter's answer still held", location.search === "?q=x" && holds.size === 0, [location.search, holds.size]);
  await forward();
  release();
  pushes = H.pushes;
  gos = H.gos.length;
  await navigate(crumb("Albums"));
  check("the album's crumb pushes then too", H.pushes === pushes + 1 && H.gos.length === gos && location.pathname === "/albums" && location.search === "",
    [H.pushes - pushes, H.gos.slice(gos), location.href]);
};

S.TestACrumbReturnsToItsEntryAfterAReload = async () => {
  await rig("/albums");
  await readerAt(150, -37);
  const before = topRow();
  await navigate(tile(150));
  const length = history.length;
  await reload();
  check("the reload draws the album again", location.pathname.startsWith("/album/") && title() === "U Album 0150", [location.pathname, title()]);
  let pushes = H.pushes;
  let gos = H.gos.length;
  await navigate(crumb("Albums"));
  check("after it the album's Albums crumb goes back one entry", same(H.gos.slice(gos), [-1]) && H.pushes === pushes && history.length === length && location.pathname === "/albums",
    [H.gos.slice(gos), H.pushes - pushes, history.length, length]);
  check("and the list stands at its place", same(topRow(), before), [topRow(), before]);
  await forward();
  check("Forward reaches the album again", location.pathname.startsWith("/album/") && title() === "U Album 0150", [location.pathname, title()]);

  // Back, the filter edited in place, Forward, a reload: the list's entry no
  // longer holds the URL the album's trail recorded, and the crumb pushes.
  await back();
  const f = field();
  f.focus();
  await type(f, "zzzz");
  await advance(400);
  check("the filter edited the list's own entry", location.search === "?q=zzzz", location.search);
  await forward();
  await reload();
  pushes = H.pushes;
  gos = H.gos.length;
  await navigate(crumb("Albums"));
  check("after a reload the crumb of an entry edited since pushes the URL it names", H.pushes === pushes + 1 && H.gos.length === gos && location.pathname === "/albums" && location.search === "",
    [H.pushes - pushes, H.gos.slice(gos), location.href]);
};

S.TestNoCallbackOfAListAFilterReplacedActs = async () => {
  // The rail drawn for the old q: a letter pressed while the new q's rows are held.
  await rig("/albums?sort=title");
  const letter = (k) => doc.getElementById("player-view").querySelectorAll(".az-letter").find((b) => b.textContent === k);
  const byTitle = (rs) => [...rs].sort((x, y) => fold(x.title).localeCompare(fold(y.title)));
  const atM = byTitle(lib.albums).findIndex((a) => a.title.startsWith("M"));
  const want = byTitle(lib.albums.filter((a) => fold(a.title).includes("ALBUM1") || fold(a.albumArtist).includes("ALBUM1")));
  const f = field();
  f.focus();
  let release = hold("/api/player/albums");
  await type(f, "Album 1");
  await advance(400);
  let asked = log.length;
  check("the old rail stands while the filter's rows are held", !!letter("M") && !letter("M").disabled, !!letter("M"));
  letter("M").fire("click", { detail: 0 });
  await settle();
  release();
  await settle();
  const after = (from) => listGets("/api/player/albums").filter((e) => log.indexOf(e) >= from).map((e) => e.query.get("offset"));
  check("its letter, pressed then, asks for nothing", !after(asked).includes(String(atM)), after(asked));
  check("and the filtered list stands from its first row", rows().length > 0 && titleOf(rows()[0]) === want[0].title && viewText().includes(`${want.length} albums`),
    [rows().length, titleOf(rows()[0]), want[0].title]);

  // A restore 1,200 rows in that failed; a filter, its rows held; the old Try again.
  f.fire("keydown", { key: "Escape" });
  await settle();
  await readerAt(1200, 0);
  await navigate(tile(1200));
  fail("/api/player/albums");
  await back();
  check("the restore failed, offering Try again", !!button("Try again"), viewText());
  const g = field();
  g.focus();
  release = hold("/api/player/albums");
  await type(g, "Album 1");
  await advance(400);
  asked = log.length;
  await navigate(button("Try again"));
  release();
  await settle();
  check("the old Try again, pressed then, asks for no window of the old place", !after(asked).some((o) => Number(o) >= 1200), after(asked));
  check("and the filtered list stands from its first row again", rows().length > 0 && titleOf(rows()[0]) === want[0].title && viewText().includes(`${want.length} albums`),
    [rows().length, titleOf(rows()[0]), want[0].title]);
};

const name = scenarioName;
if (!S[name]) {
  R.fail.push({ name: "scenario", detail: `no scenario ${name}` });
} else {
  try {
    await S[name]();
  } catch (e) {
    R.fail.push({ name: "threw", detail: String((e && e.stack) || e) });
  }
}
console.log(JSON.stringify(R));
process.exit(0);
