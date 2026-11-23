// The player bar's output under node (B634, PR 2): the SHIPPED output.js,
// format.js and, for the cases that need them, audio.js and nowplaying.js
// over a DOM model, driven scenario by scenario. Each scenario reports named
// checks; player_output_bar_test.go runs one scenario a test and fails it on
// any check that failed, or on too few checks (a scenario that stopped
// checking anything).
//
//   node bar_harness.mjs <player module directory URL> <scenario>
//
// Every race is made by holding a request (hold), never by a timer: a held
// request answers when the scenario lets it go.

const [, , dirURL, scenarioName] = process.argv;
const R = { pass: [], fail: [], log: [] };
const check = (name, cond, detail) => {
  if (cond) R.pass.push(name);
  else R.fail.push({ name, detail: detail === undefined ? "" : JSON.stringify(detail) });
};
// The clock's own timers, which a case that holds audio.js's (longTimers)
// leaves to settle.
const realTimeout = globalThis.setTimeout;
const realClear = globalThis.clearTimeout;
const settle = async () => { for (let i = 0; i < 12; i++) await new Promise((r) => realTimeout(r, 0)); };

// ---- The page the modules see (enough for import, then each case's own) ----

const listenersOf = new Map();
const winTarget = {
  addEventListener(type, fn) { (listenersOf.get(type) || listenersOf.set(type, []).get(type)).push(fn); },
  removeEventListener() {},
  dispatchEvent(e) { for (const fn of listenersOf.get(e.type) || []) fn(e); return true; },
};
globalThis.window = Object.assign(winTarget, { location: { pathname: "/", search: "", href: "http://x/", origin: "http://x" } });
globalThis.location = globalThis.window.location;
const store = new Map();
globalThis.sessionStorage = {
  getItem: (k) => (store.has(k) ? store.get(k) : null),
  setItem: (k, v) => { store.set(k, String(v)); },
  removeItem: (k) => { store.delete(k); },
};

const { createOutput, listen, installOutput, PHASE, WORDS, ROUTES, CLOCK_HEADER, OUTPUT_KEY, OP_KEY, opKey, READ_BACK_MS, REQUEST_MS, SAMPLE_RETRY_MS, QUEUE_RETRY_MS, START_WAIT_MS } =
  await import(new URL("output.js", dirURL));
const { playFor } = await import(new URL("format.js", dirURL));

// ---- Fakes ----

const E1 = "e1".repeat(16);
const E2 = "e2".repeat(16);
const T0 = Date.parse("2026-11-23T10:00:00.000Z");

function clock(start = T0) {
  const c = { t: start, mono: 1000 };
  c.now = () => c.t;
  c.perf = () => c.mono;
  c.advance = (ms) => { c.t += ms; c.mono += ms; };
  c.jump = (ms) => { c.t += ms; };
  return c;
}

function gate() {
  let open;
  const p = new Promise((r) => { open = r; });
  return { p, open };
}

// server is a fake console: each route answers from a handler, which can
// hold the answer until the scenario lets it go; every request is logged.
function server(c) {
  const log = [];
  const handlers = [];
  const s = {
    log,
    order: [],
    on(method, path, fn) { handlers.unshift({ method, path, fn }); return s; },
    posts(path) { return log.filter((e) => e.method === "POST" && (!path || e.path === path)); },
    gets(path) { return log.filter((e) => e.method === "GET" && e.path === path); },
    async fetch(url, init = {}) {
      const method = init.method || "GET";
      const [path, query] = url.split("?");
      const body = init.body ? JSON.parse(init.body) : null;
      const e = { method, path, query: new URLSearchParams(query || ""), body, at: c.now() };
      log.push(e);
      s.order.push(`fetch ${method} ${path}`);
      const h = handlers.find((x) => x.method === method && x.path === path);
      if (!h) throw new TypeError("no route");
      const res = await h.fn(e);
      if (res === "lost") throw new TypeError("the connection was lost");
      return response(res);
    },
  };
  return s;
}

function response({ status = 200, body = null, headers = {} }) {
  const lower = Object.fromEntries(Object.entries(headers).map(([k, v]) => [k.toLowerCase(), v]));
  return {
    ok: status >= 200 && status < 300,
    status,
    headers: { get: (n) => lower[n.toLowerCase()] ?? null },
    json: async () => {
      if (body === "malformed") throw new SyntaxError("not JSON");
      return body;
    },
  };
}

// pub is a publication's state, PlayerState's shape, with defaults.
function pub(o = {}) {
  return {
    epoch: E1, revision: 1, at: new Date(T0).toISOString(), running: true, why: "", offered: true, offeredWhy: "",
    transport: "stopped", starting: false, pending: "", hold: "none", release: { state: "confirmed" },
    commands: ["stop", "pause", "repeat", "replace", "append", "delete", "play", "next", "previous", "index"],
    silent: true, tone: false, generation: "0123456789abcdef.1", token: 1, length: 0, queueRev: 1, selected: 0,
    repeat: false, track: null, anchor: null, signal: null, seek: false, lastStop: { reason: "", entry: 0 },
    lastAttempt: { seq: 0, failed: false, busy: false, denied: false }, operations: [], ...o,
  };
}

const playing = (o = {}) => pub({
  transport: "playing", silent: false, length: 3, selected: 11, seek: true,
  track: { entry: 11, path: "Album/01 A.flac", named: true, title: "A", artist: "Artist", album: "Album", codec: "FLAC", bits: 24, rateHz: 96000, channels: 2, duration: 300 },
  anchor: { id: 4, occurrence: 1, entry: 11, seconds: 10, at: new Date(T0).toISOString(), advancing: true },
  commands: ["stop", "pause", "repeat", "replace", "append", "delete", "play", "next", "previous", "index", "seek"],
  ...o,
});

// browser is the web player as the switch needs it, every call recorded in
// the server's order beside the requests.
function browser(s, o = {}) {
  const b = {
    calls: [],
    queue: o.queue === undefined ? [{ path: "Album/01 A.flac", title: "A" }, { path: "Album/02 B.flac", title: "B" }] : o.queue,
    note(x) { b.calls.push(x); s.order.push(`browser.${x}`); },
    handover() {
      b.note("handover");
      return { queue: b.queue, index: o.index ?? 1, playing: o.playing ?? true, position: o.position ?? 42, pcm: o.pcm ?? true };
    },
    hasQueue() { return b.queue.length > 0; },
    suspend() { b.note("suspend"); },
    resume(h) { b.note("resume"); b.resumed = h; },
    take(h, play) { b.note(`take:${play}`); b.taken = h; },
    start() { b.note("start"); },
    mediaSession(active) { b.calls.push(`session:${active}`); },
  };
  return b;
}

let tabCounter = 0;
function env(s, c, b, storage) {
  return {
    fetch: (u, i) => s.fetch(u, i),
    storage: storage || sessionStorage,
    random: (bytes) => { tabCounter += 1; for (let i = 0; i < bytes.length; i++) bytes[i] = (tabCounter * 31 + i) & 0xff; },
    now: c.now,
    perf: c.perf,
    browser: b,
  };
}

// storageOf is a session storage of its own over m, as a copy of a tab
// gets: what one writes the other never reads (R8).
function storageOf(m) {
  return {
    getItem: (k) => (m.has(k) ? m.get(k) : null),
    setItem: (k, v) => { m.set(k, String(v)); },
    removeItem: (k) => { m.delete(k); },
  };
}

// stored is session storage's record of operations not yet resolved, as
// round 2 keeps them: the document's identity, and each operation by its
// epoch, its tab and its count (R7).
function stored(tab, seq, list) {
  const pending = {};
  for (const p of list) pending[opKey(p.epoch, p.tab, p.seq)] = { kind: "", handover: null, ...p };
  return JSON.stringify({ tab, seq, pending });
}

// pendingIn is what a storage holds of operations not yet resolved.
function pendingIn(m) {
  try { return Object.keys(JSON.parse(m.get(OP_KEY) || "{}").pending || {}); } catch { return null; }
}

// rig is one tab: a fresh session storage (unless keep, or one of its own),
// a console, a clock, a browser and the output over them, its GET now
// answering the latest state the scenario set with the clock at send.
function rig(o = {}) {
  if (!o.keep && !o.storage) store.clear();
  if (o.output) (o.storage || sessionStorage).setItem(OUTPUT_KEY, o.output);
  const c = clock();
  const s = server(c);
  const b = browser(s, o.browser || {});
  const r = { c, s, b, latest: o.latest || pub(), queueAnswer: null };
  s.on("GET", ROUTES.now, async () => ({ status: 200, body: r.latest, headers: { [CLOCK_HEADER]: new Date(c.now()).toISOString() } }));
  s.on("GET", ROUTES.queue, async () => r.queueAnswer || ({ body: { epoch: r.latest.epoch, readAt: r.latest.revision, queueRev: r.latest.queueRev, token: 1, selected: r.latest.selected, entries: [] } }));
  r.out = createOutput(env(s, c, b, o.storage));
  r.serial = 0;
  r.open = () => { r.serial += 1; r.out.streamOpen(r.serial); };
  r.frame = (st) => r.out.frame(JSON.stringify(st), r.serial);
  r.v = () => r.out.view();
  return r;
}

// started is a rig on the stream with its first frame and its clock sampled.
async function started(o = {}) {
  const r = rig(o);
  r.open();
  r.frame(r.latest);
  await settle();
  return r;
}

const answer = (record, st, status) => ({
  status: status || (record.status === "done" ? 200 : (record.status === "running" ? 202 : 409)),
  body: { record, player: JSON.stringify(st), ...(record.status === "done" || record.status === "running" ? {} : { error: record.error || record.status }) },
});

const rec = (r, o = {}) => ({ tab: r.out.identity().tab, seq: r.out.identity().seq, kind: "transport", status: "done", effect: "some", sent: 1, of: 1, last: "ok", release: "none", ...o });

// ---- Scenarios ----

const S = {};

S.TestAStaleCommandRereadsAndNeverReplays = async () => {
  const r = await started({ output: "bridge", latest: playing({ revision: 5, queueRev: 3 }) });
  const before = r.s.gets(ROUTES.queue).length;
  const newer = playing({ revision: 7, queueRev: 6, generation: "0123456789abcdef.9" });
  r.s.on("POST", ROUTES.transport, async () => { r.latest = newer; return answer(rec(r, { status: "refused", error: "stale", effect: "none", last: "conflict" }), newer); });
  await r.out.command("next");
  await settle();
  check("the stale answer's state taken", r.v().state.revision === 7, r.v().state.revision);
  check("the queue read again for the newer queueRev", r.s.gets(ROUTES.queue).length > before);
  check("the command sent once, never again", r.s.posts(ROUTES.transport).length === 1, r.s.posts(ROUTES.transport).length);
  check("the bar says the player changed", r.v().line === WORDS.stale, r.v().line);
};

S.TestAPartialReplaceIsShownAsItIs = async () => {
  const r = await started({ output: "bridge" });
  const after = pub({ revision: 9, length: 5, queueRev: 9 });
  r.s.on("POST", ROUTES.replace, async (e) => answer(rec(r, { kind: "replace", status: "changed", sent: 4, of: 12, entries: [21, 22, 23], last: "ok" }), after));
  await r.out.replace({ album: "a1" }, { start: 0, play: true });
  await settle();
  check("the partial edit shown as it is", r.v().line === "The queue changed while the console was replacing it: it holds 3 of the 10 tracks, and the player is stopped", r.v().line);
  check("nothing rolled back or sent again", r.s.log.filter((e) => e.method === "POST").length === 1, r.s.posts());
  check("the state as it is", r.v().state.revision === 9 && r.v().phase === PHASE.bridge, r.v().phase);
};

S.TestAnUnconfirmedOperationClearsOnlyOnItsRecord = async () => {
  const r = await started({ output: "bridge", latest: playing({ revision: 3 }) });
  r.s.on("POST", ROUTES.transport, async () => "lost");
  r.s.on("GET", ROUTES.operation, async () => ({ body: { epoch: E1, revision: 4, record: rec(r, { status: "running", sent: 1 }) } }));
  await r.out.command("pause");
  await settle();
  check("unconfirmed once the answer is lost", r.v().phase === PHASE.unconfirmed, r.v().phase);
  r.frame(playing({ revision: 4, transport: "paused", silent: true }));
  await settle();
  check("still unconfirmed after a change that looks like the command's", r.v().phase === PHASE.unconfirmed, r.v().phase);
  const id = r.out.identity();
  r.frame(playing({ revision: 5, transport: "paused", operations: [{ tab: id.tab, seq: id.seq, kind: "transport", command: "pause", status: "done", effect: "some", sent: 1, of: 1, last: "ok", release: "none" }] }));
  await settle();
  check("cleared on its record", r.v().phase === PHASE.bridge, r.v().phase);
  check("never sent again", r.s.posts(ROUTES.transport).length === 1, r.s.posts(ROUTES.transport).length);
};

S.TestAReloadedTabFindsItsOperation = async () => {
  store.clear();
  const old = "ab".repeat(16);
  store.set(OUTPUT_KEY, "bridge");
  store.set(OP_KEY, stored(old, 3, [{ tab: old, seq: 3, epoch: E1, role: "command" }]));
  const r = rig({ keep: true, latest: playing({ revision: 6 }) });
  r.s.on("GET", ROUTES.operation, async (e) => ({ body: { epoch: E1, revision: 6, record: { tab: e.query.get("tab"), seq: Number(e.query.get("seq")), kind: "transport", status: "done", effect: "some", sent: 1, of: 1, last: "ok", release: "none" } } }));
  check("unconfirmed at boot with an operation inherited", r.v().phase === PHASE.unconfirmed, r.v().phase);
  check("a tab identity of its own", r.out.identity().tab !== old && /^[0-9a-f]{32}$/.test(r.out.identity().tab), r.out.identity());
  r.open();
  r.frame(r.latest);
  await settle();
  const rb = r.s.gets(ROUTES.operation)[0];
  check("its operation read back by the inherited identity", rb && rb.query.get("tab") === old && rb.query.get("seq") === "3" && rb.query.get("epoch") === E1, rb && rb.query.toString());
  check("resolved by its record", r.v().phase === PHASE.bridge, r.v().phase);
};

S.TestTheSwitchToTheBridgeStopsTheBrowserFirst = async () => {
  const r = await started({});
  r.s.on("POST", ROUTES.replace, async () => answer(rec(r, { kind: "replace", status: "done" }), playing({ revision: 4 })));
  await r.out.pick("bridge");
  await settle();
  const o = r.s.order.filter((x) => x.startsWith("browser.") || x === `fetch POST ${ROUTES.replace}`);
  check("the handover read, the browser stopped, then the request", JSON.stringify(o.slice(0, 3)) === JSON.stringify(["browser.handover", "browser.suspend", `fetch POST ${ROUTES.replace}`]), o);
  check("on the bridge once done", r.v().phase === PHASE.bridge && r.v().output === "bridge", r.v().phase);
};

S.TestTheSwitchToTheBrowserStartsItOnlyOnSilence = async () => {
  const r = await started({ output: "bridge", latest: playing({ revision: 3 }) });
  const g = gate();
  const st = pub({ revision: 5, transport: "paused", silent: true });
  const handover = { tracks: [{ id: "x1", path: "Album/01 A.flac", track: { path: "Album/01 A.flac", title: "A" } }, { id: "x2", path: "Album/02 B.flac", track: { path: "Album/02 B.flac", title: "B" } }], selected: 1, position: 61, playing: true };
  r.s.on("POST", ROUTES.switchOut, async () => { await g.p; return answer(rec(r, { kind: "switch-out", status: "done", silence: 5, handover }), st); });
  const p = r.out.pick("browser");
  await settle();
  r.frame(pub({ revision: 4, transport: "paused", silent: true }));
  await settle();
  check("nothing started while the barrier has not answered", !r.b.calls.some((x) => x.startsWith("take") || x === "start" || x === "resume"), r.b.calls);
  check("the bar says it is moving", r.v().phase === PHASE.toBrowser, r.v().phase);
  g.open();
  await p;
  await settle();
  check("the browser takes the bridge's queue on the barrier's done, playing", r.b.calls.includes("take:true") && r.b.taken?.index === 1 && r.b.taken?.position === 61, [r.b.calls, r.b.taken]);
  check("this browser chosen", r.v().output === "browser" && r.v().phase === PHASE.browser, r.v().phase);
};

S.TestARefusalOrAPausesSuccessIsNotSilence = async () => {
  {
    const r = await started({ output: "bridge", latest: playing({ revision: 3 }) });
    r.s.on("POST", ROUTES.switchOut, async () => answer(rec(r, { kind: "switch-out", status: "refused", error: "check", effect: "some", last: "maintenance" }), pub({ revision: 4, silent: true, hold: "check" })));
    await r.out.pick("browser");
    await settle();
    check("a refused barrier starts nothing, whatever its publication reads", !r.b.calls.some((x) => x.startsWith("take") || x === "start" || x === "resume"), r.b.calls);
    check("the check's words", r.v().line === "The DSD check is playing on the DAC: Stop ends it" && r.v().phase === PHASE.bridge, r.v().line);
  }
  {
    const r = await started({ output: "bridge", latest: playing({ revision: 3, transport: "releasing", pending: "play" }) });
    r.s.on("POST", ROUTES.switchOut, async () => answer(rec(r, { kind: "switch-out", status: "changed", effect: "some", sent: 3, of: 3, last: "ok" }), playing({ revision: 6 })));
    await r.out.pick("browser");
    await settle();
    check("a Pause that succeeded beside a play that followed it starts nothing", !r.b.calls.some((x) => x.startsWith("take") || x === "start" || x === "resume"), r.b.calls);
    check("the bar says the bridge went on playing", r.v().line === WORDS.wentOn && r.v().output === "bridge", r.v().line);
  }
};

S.TestAnUnknownSwitchNeverStartsTheBrowser = async () => {
  {
    const r = await started({});
    const g = gate();
    r.s.on("POST", ROUTES.replace, async () => "lost");
    r.s.on("GET", ROUTES.operation, async (e) => { await g.p; return { body: { epoch: E1, revision: 5, record: { tab: e.query.get("tab"), seq: Number(e.query.get("seq")), kind: "replace", status: "unknown", effect: "unknown", sent: 2, of: 4, last: "closed", release: "none" } } }; });
    await r.out.pick("bridge");
    await settle();
    check("to the bridge, lost: unconfirmed and the browser stopped", r.v().phase === PHASE.unconfirmed && !r.b.calls.includes("resume"), [r.v().phase, r.b.calls]);
    g.open();
    await settle();
    check("to the bridge, unknown read back: the browser never resumes", !r.b.calls.includes("resume") && !r.b.calls.includes("start") && r.v().output === "bridge", [r.b.calls, r.v().output]);
  }
  {
    const r = await started({ output: "bridge", latest: playing({ revision: 3 }) });
    r.s.on("POST", ROUTES.switchOut, async () => "lost");
    await r.out.pick("browser");
    await settle();
    check("to the browser, lost: unconfirmed", r.v().phase === PHASE.unconfirmed, r.v().phase);
    // A restart: the new epoch's first frame makes the outcome unknown.
    r.out.streamError(r.serial);
    r.open();
    r.frame(pub({ epoch: E2, revision: 1 }));
    await settle();
    check("to the browser, a new epoch: nothing started, the pick to make again", !r.b.calls.some((x) => x.startsWith("take") || x === "start" || x === "resume") && r.v().phase === PHASE.bridge && r.v().line === WORDS.pickAgain, [r.b.calls, r.v().phase, r.v().line]);
  }
};

S.TestARefusalThatChangedNothingLetsTheBrowserPlayOnOnlyBesideSilence = async () => {
  {
    const r = await started({});
    r.s.on("POST", ROUTES.replace, async () => answer(rec(r, { kind: "replace", status: "refused", error: "stale", effect: "none", sent: 1, of: 4, last: "conflict" }), pub({ revision: 4, silent: true })));
    r.s.on("POST", ROUTES.switchOut, async (e) => answer(rec(r, { kind: "switch-out", status: "done", silence: 6 }), pub({ revision: 6, silent: true })));
    await r.out.pick("bridge");
    await settle();
    const so = r.s.posts(ROUTES.switchOut)[0];
    check("beside silence the browser's resume goes through the barrier, take false", so && so.body.take === false, so && so.body);
    check("and resumes on its done, at the position it read", r.b.calls.includes("resume") && r.b.resumed?.position === 42 && r.v().output === "browser", [r.b.calls, r.b.resumed]);
  }
  {
    const r = await started({});
    r.s.on("POST", ROUTES.replace, async () => answer(rec(r, { kind: "replace", status: "refused", error: "stale", effect: "none", sent: 1, of: 4, last: "conflict" }), playing({ revision: 4 })));
    await r.out.pick("bridge");
    await settle();
    check("beside a bridge that plays no barrier is sent", r.s.posts(ROUTES.switchOut).length === 0, r.s.posts());
    check("and the browser stays stopped, the bar saying so", !r.b.calls.includes("resume") && r.v().line === WORDS.bridgePlaying && r.v().output === "browser", [r.b.calls, r.v().line]);
  }
};

S.TestThePositionCarriesOverForPCM = async () => {
  {
    const r = await started({ browser: { position: 42, pcm: true, index: 1, playing: true } });
    r.s.on("POST", ROUTES.replace, async () => answer(rec(r, { kind: "replace", status: "done" }), playing({ revision: 4 })));
    await r.out.pick("bridge");
    await settle();
    const b = r.s.posts(ROUTES.replace)[0]?.body;
    check("PCM: the replace carries its whole seconds, its track and play", b && b.position === 42 && b.start === 1 && b.play === true && b.tracks.length === 2, b);
  }
  {
    const r = await started({ browser: { position: 42, pcm: false, index: 0, playing: true } });
    r.s.on("POST", ROUTES.replace, async () => answer(rec(r, { kind: "replace", status: "done" }), playing({ revision: 4 })));
    await r.out.pick("bridge");
    await settle();
    const b = r.s.posts(ROUTES.replace)[0]?.body;
    check("DSD: no position, the track plays from its start", b && b.position === undefined, b);
  }
};

// anchored is a rig on the bridge whose state is an advancing anchor at T0,
// 10 s in, its clock sampled at T0 + 2 s (no skew).
async function anchored() {
  const r = rig({ output: "bridge", latest: playing({ revision: 5 }) });
  r.c.advance(2000);
  r.open();
  r.frame(r.latest);
  await settle();
  return r;
}

S.TestTheBarsPositionHoldsThroughARepeatChange = async () => {
  const r = await anchored();
  check("sampled", r.v().sampled);
  const p0 = r.out.position();
  check("the anchor's position at the sample", Math.abs(p0 - 12) < 0.01, p0);
  r.c.advance(5000);
  r.frame(playing({ revision: 6, repeat: true }));
  await settle();
  const p = r.out.position();
  check("a repeat change moves no position", Math.abs(p - 17) < 0.01, p);
};

S.TestTheBarsPositionHoldsThroughAQueueUpdate = async () => {
  const r = await anchored();
  r.c.advance(5000);
  r.frame(playing({ revision: 6, queueRev: 6, length: 4 }));
  await settle();
  const p = r.out.position();
  check("a queue update moves no position", Math.abs(p - 17) < 0.01, p);
};

S.TestTheBarsPositionHoldsAcrossAReconnection = async () => {
  const r = await anchored();
  r.c.advance(5000);
  r.out.streamError(r.serial);
  r.open();
  r.frame(playing({ revision: 5 }));
  await settle();
  const p = r.out.position();
  check("sampled again on the new connection", r.v().sampled);
  check("a reconnection's first frame carries the anchor, the position goes on", Math.abs(p - 17) < 0.01, p);
};

S.TestANewEpochWinsAndAnOldOneIsDropped = async () => {
  const r = await started({ output: "bridge", latest: playing({ revision: 9 }) });
  r.out.streamError(r.serial);
  r.open();
  r.latest = pub({ epoch: E2, revision: 2 });
  r.frame(r.latest);
  await settle();
  check("the new epoch taken from the connection's first frame", r.v().state.epoch === E2 && r.v().state.revision === 2, r.v().state);
  r.frame(playing({ epoch: E1, revision: 30 }));
  r.s.on("POST", ROUTES.transport, async () => answer(rec(r, { status: "done" }), playing({ epoch: E1, revision: 31 })));
  await r.out.command("stop");
  await settle();
  check("an older epoch's state dropped, by frame or by answer", r.v().state.epoch === E2, r.v().state.epoch);
};

S.TestAHoldNeverWipesTheView = async () => {
  const r = await started({ output: "bridge", latest: playing({ revision: 3 }) });
  r.frame(playing({ revision: 4, hold: "check", commands: ["stop"] }));
  await settle();
  const v = r.v();
  check("the track kept through the hold", v.state && v.state.track && v.state.track.title === "A", v.state && v.state.track);
  check("only Stop enabled, the hold said", v.commands.has("stop") && !v.commands.has("play") && !v.canPick, [...v.commands]);
  check("the bar stays on the bridge", v.output === "bridge" && v.phase === PHASE.bridge, v.phase);
};

S.TestReturningEligibilityShowsTheActualState = async () => {
  const r = await started({ output: "bridge", latest: pub({ revision: 3, offered: false, offeredWhy: "device_missing", transport: "stopped" }) });
  r.frame(playing({ revision: 4 }));
  await settle();
  const v = r.v();
  check("back, it shows what plays, never a prescribed stopped", v.state.transport === "playing" && v.state.track?.title === "A", v.state.transport);
  check("and sends nothing", r.s.log.filter((e) => e.method === "POST").length === 0, r.s.posts());
};

S.TestTheBarKeepsTheViewWhileTheBridgeIsUnavailable = async () => {
  const r = await started({ output: "bridge", latest: playing({ revision: 3 }) });
  r.frame(playing({ revision: 4, offered: false, offeredWhy: "device_missing", transport: "stopped", commands: ["stop", "pause", "repeat", "replace", "append", "delete"] }));
  await settle();
  const v = r.v();
  check("the bar stays on the bridge's view", v.output === "bridge" && v.phase === PHASE.bridge && v.state.track?.title === "A", v.phase);
  check("its word for the DAC away", v.pickWhy === "The DAC is not connected", v.pickWhy);
  check("only the commands the engine takes", !v.commands.has("play") && v.commands.has("stop"), [...v.commands]);
};

S.TestTheBridgesReturnPlaysNothing = async () => {
  const r = await started({ output: "bridge", latest: pub({ revision: 3, length: 2 }) });
  r.frame(pub({ revision: 4, offered: false, offeredWhy: "device_missing", length: 2 }));
  r.frame(pub({ revision: 5, offered: true, length: 2 }));
  await settle();
  check("the DAC back, nothing is sent", r.s.log.filter((e) => e.method === "POST").length === 0, r.s.posts());
  check("and the bar shows the player stopped as published", r.v().state.transport === "stopped" && r.v().canPick, r.v().state.transport);
};

S.TestRowsFollowTheChosenOutput = async () => {
  const pcm = { path: "A/01.flac", codec: "FLAC", rateHz: 96000, play: { kind: "universal" }, bridge: { way: "pcm" } };
  const native = { path: "D/01.dsf", codec: "DSF", isDsd: true, rateHz: 2822400, play: { kind: "none" }, bridge: { way: "native" } };
  const dop = { path: "D/02.dsf", codec: "DSF", isDsd: true, rateHz: 5644800, play: { kind: "none" }, bridge: { way: "dop" } };
  const conv = { path: "D/03.dsf", codec: "DSF", isDsd: true, rateHz: 11289600, play: { kind: "none" }, bridge: { way: "converted" } };
  const routed = { path: "R/01.flac", codec: "FLAC", routed: true, play: { kind: "universal" }, bridge: { way: "none", reason: "routed" } };
  const b = (t) => playFor(t, "bridge");
  const w = (t) => playFor(t, "browser");
  check("the browser: DSD not playable, PCM plays", !w(native).playable && w(native).chip === "DSD — not playable in a browser" && w(pcm).playable, [w(native), w(pcm)]);
  check("the bridge: DSD plays and reads its way", b(native).playable && b(native).chip === "DSD64, native" && b(dop).chip === "DSD128, DoP" && b(conv).chip === "DSD256, converted to PCM", [b(native), b(dop), b(conv)]);
  check("the bridge: a routed row reads why", !b(routed).playable && b(routed).chip === "On another server: the bridge's player plays this bridge's own files", b(routed));
  check("the bridge: PCM plays with no chip", b(pcm).playable && b(pcm).chip === "", b(pcm));
};

S.TestADSDOnlyAlbumPlaysOnTheBridgeFromAnEmptyBar = async () => {
  const r = await started({ browser: { queue: [] } });
  check("offered from empty session storage", r.v().offered && r.v().output === "browser" && r.v().canPick, r.v());
  await r.out.pick("bridge");
  await settle();
  check("picked from an empty bar: the bridge's at once, nothing sent", r.v().output === "bridge" && r.s.log.filter((e) => e.method === "POST").length === 0, r.s.posts());
  r.s.on("POST", ROUTES.replace, async () => answer(rec(r, { kind: "replace", status: "done", of: 4, sent: 4 }), playing({ revision: 4, track: { entry: 11, path: "DSD/01.dsf", named: true, title: "D1", codec: "DSD", bits: 1, rateHz: 2822400, channels: 2, duration: 200 }, signal: { transport: "native", format: "DSD_U32_BE", rateHz: 88200, channels: 2, validBits: 32, state: "running" }, seek: false })));
  await r.out.replace({ album: "dsdalbum" }, { start: 0, play: true });
  await settle();
  const body = r.s.posts(ROUTES.replace)[0]?.body;
  check("the album by its id, played from its first track", body && body.album === "dsdalbum" && body.start === 0 && body.play === true && body.op.seq === 1 && body.op.epoch === E1 && body.at === r.latest.generation, body);
  check("the bar on the bridge, playing", r.v().state.transport === "playing" && r.v().phase === PHASE.bridge, r.v().state.transport);
};

S.TestTheBarOffersItsOutputFromEmptySessionStorage = async () => {
  store.clear();
  const r = rig({ keep: true, browser: { queue: [] } });
  await r.out.sampleNow();
  await settle();
  check("GET now's answer offers the bridge's player before the stream opens", r.v().output === "browser" && r.v().offered === true && r.v().state === null, r.v());
  r.open();
  r.frame(r.latest);
  await settle();
  check("the first frame offers it from empty storage", r.v().offered && r.v().canPick && r.v().output === "browser", r.v());
  const r2 = rig({ browser: { queue: [] } });
  r2.s.on("GET", ROUTES.now, async () => ({ status: 409, body: { error: "player_not_offered", message: "no player" } }));
  await r2.out.sampleNow();
  await settle();
  check("a box that offers no player says so at once", r2.v().notOffered && !r2.v().offered, r2.v());
};

S.TestPickingTheBridgeFromAnEmptyBarSendsNothing = async () => {
  const r = await started({ browser: { queue: [] } });
  await r.out.pick("bridge");
  await settle();
  check("nothing sent", r.s.log.filter((e) => e.method === "POST").length === 0, r.s.posts());
  check("the bridge chosen and kept in session storage", r.v().output === "bridge" && store.get(OUTPUT_KEY) === "bridge");
  check("the browser untouched", !r.b.calls.includes("handover") && !r.b.calls.includes("suspend"), r.b.calls);
};

S.TestEveryBrowserStartGoesThroughTheBarrier = async () => {
  {
    const r = await started({});
    const so = gate();
    r.s.on("POST", ROUTES.replace, async () => answer(rec(r, { kind: "replace", status: "refused", error: "not_playable", effect: "none", sent: 0 }), pub({ revision: 4, silent: true })));
    r.s.on("POST", ROUTES.switchOut, async () => { await so.p; return answer(rec(r, { kind: "switch-out", status: "done", silence: 7 }), pub({ revision: 7 })); });
    await r.out.pick("bridge");
    await settle();
    check("the refusal's publication reads silent, and nothing has started", !r.b.calls.includes("resume"), r.b.calls);
    check("the barrier asked, its Pause the console's own", r.s.posts(ROUTES.switchOut).length === 1);
    so.open();
    await settle();
    check("the browser resumes on the barrier's done alone", r.b.calls.includes("resume"), r.b.calls);
  }
  {
    const r = await started({});
    r.s.on("POST", ROUTES.replace, async () => answer(rec(r, { kind: "replace", status: "refused", error: "not_playable", effect: "none", sent: 0 }), playing({ revision: 4 })));
    await r.out.pick("bridge");
    await settle();
    check("refused with the bridge playing: no barrier, nothing started", r.s.posts(ROUTES.switchOut).length === 0 && !r.b.calls.includes("resume"), [r.s.posts(), r.b.calls]);
  }
};

S.TestALateAnswerFromBeforeARestartNeverReplacesTheNewEpoch = async () => {
  {
    // The old serve's answer after the new epoch's first frame.
    const r = await started({ output: "bridge", latest: playing({ revision: 9 }) });
    const g = gate();
    r.s.on("POST", ROUTES.transport, async () => { await g.p; return answer(rec(r, { status: "done" }), playing({ epoch: E1, revision: 12 })); });
    const p = r.out.command("next");
    await settle();
    r.out.streamError(r.serial);
    r.open();
    r.frame(pub({ epoch: E2, revision: 1 }));
    await settle();
    g.open();
    await p;
    await settle();
    check("the old serve's answer after the restart's first frame never takes the bar back", r.v().state.epoch === E2, r.v().state.epoch);
  }
  {
    // The old serve's answer before the restart's first frame.
    const r = await started({ output: "bridge", latest: playing({ revision: 9 }) });
    r.s.on("POST", ROUTES.transport, async () => answer(rec(r, { status: "done" }), playing({ epoch: E1, revision: 12 })));
    await r.out.command("next");
    r.out.streamError(r.serial);
    r.open();
    r.frame(pub({ epoch: E2, revision: 1 }));
    await settle();
    check("before the first frame: the new epoch wins at the frame", r.v().state.epoch === E2, r.v().state.epoch);
  }
  {
    // The new serve's answer while the stream still reconnects: held, then
    // taken by revision once the first frame confirms its epoch.
    const r = await started({ output: "bridge", latest: playing({ revision: 9 }) });
    r.out.streamError(r.serial);
    r.s.on("GET", ROUTES.now, async () => ({ body: pub({ epoch: E2, revision: 5, length: 4 }), headers: { [CLOCK_HEADER]: new Date(r.c.now()).toISOString() } }));
    await r.out.sampleNow();
    await settle();
    check("held while the stream reconnects", r.v().state.epoch === E1 && r.v().reconnecting, r.v().state.epoch);
    r.open();
    r.frame(pub({ epoch: E2, revision: 3 }));
    await settle();
    check("taken once the first frame confirms its epoch, by revision", r.v().state.epoch === E2 && r.v().state.revision === 5, r.v().state);
  }
};

S.TestAnOperationIsConfirmedByItsRecordNeverByAChange = async () => {
  const r = await started({ output: "bridge", latest: pub({ revision: 4 }) });
  r.s.on("POST", ROUTES.transport, async () => answer(rec(r, { status: "done", command: "pause" }), pub({ revision: 5 })));
  await r.out.command("pause");
  await settle();
  check("a Pause of a stopped bridge confirmed by its answer's record under its own revision", r.v().phase === PHASE.bridge && r.v().state.revision === 5 && r.v().line === "", [r.v().phase, r.v().state.revision]);
};

S.TestTheFirstStateOfAnEpochLeadsToAQueueReadWhateverTheToken = async () => {
  for (const token of [0, 5]) {
    const r = rig({ output: "bridge", latest: pub({ revision: 1, token, queueRev: 1, length: token ? 3 : 0 }) });
    r.open();
    r.frame(r.latest);
    await settle();
    check(`the first frame reads the queue, token ${token}`, r.s.gets(ROUTES.queue).length >= 1, r.s.gets(ROUTES.queue).length);
    const n = r.s.gets(ROUTES.queue).length;
    r.latest = pub({ revision: 2, token, queueRev: 2, length: 3 });
    r.frame(r.latest);
    await settle();
    check(`and again at the next queueRev, token ${token}`, r.s.gets(ROUTES.queue).length > n, r.s.gets(ROUTES.queue).length);
  }
};

S.TestAnOlderQueueReadNeverWinsOverANewerFrame = async () => {
  const r = rig({ output: "bridge", latest: pub({ revision: 1, queueRev: 1 }) });
  const g = gate();
  let reads = 0;
  r.s.on("GET", ROUTES.queue, async () => {
    reads += 1;
    if (reads === 1) { await g.p; return { body: { epoch: E1, readAt: 1, queueRev: 1, token: 1, selected: 0, entries: [{ id: 1, path: "old" }] } }; }
    return { body: { epoch: E1, readAt: 2, queueRev: 2, token: 2, selected: 0, entries: [{ id: 2, path: "new" }] } };
  });
  r.open();
  r.frame(r.latest);
  await settle();
  r.frame(pub({ revision: 2, queueRev: 2 }));
  await settle();
  g.open();
  await settle();
  check("the older read dropped, the queue read again", r.v().queue?.readAt === 2 && r.v().queue.entries[0].path === "new", r.v().queue);
};

S.TestAReloadedTabReadsItsOperationBack = async () => {
  store.clear();
  const old = "cd".repeat(16);
  store.set(OUTPUT_KEY, "bridge");
  store.set(OP_KEY, stored(old, 2, [{ tab: old, seq: 2, epoch: E1, role: "switch-out" }]));
  // Nine operations of other tabs: the tab's record is not in the stream's eight.
  const others = Array.from({ length: 8 }, (_, i) => ({ tab: String(i).repeat(32), seq: 1, kind: "transport", status: "done", effect: "some", sent: 1, of: 1, last: "ok", release: "none" }));
  const r = rig({ keep: true, latest: pub({ revision: 20, operations: others }) });
  const handover = { tracks: [{ id: "x1", path: "Album/01 A.flac", track: { path: "Album/01 A.flac", title: "A" } }], selected: 0, position: 33, playing: true };
  r.s.on("GET", ROUTES.operation, async (e) => ({ body: { epoch: E1, revision: 20, record: { tab: old, seq: 2, kind: "switch-out", status: "done", effect: "some", sent: 1, of: 1, last: "ok", release: "none", silence: 15, handover } } }));
  r.s.on("POST", ROUTES.switchOut, async (e) => answer({ tab: r.out.identity().tab, seq: r.out.identity().seq, kind: "switch-out", status: "done", effect: "some", sent: 1, of: 1, last: "ok", release: "none", silence: 21 }, pub({ revision: 21 })));
  r.open();
  r.frame(r.latest);
  await settle();
  check("read back whole though not in the stream's eight", r.s.gets(ROUTES.operation).length >= 1 && r.b.taken?.position === 33, [r.s.gets(ROUTES.operation).length, r.b.taken]);
};

S.TestARecoveredHandoverIsHistory = async () => {
  // G8: this tab's own switch to this browser, its answer lost and its
  // record read back done. (A record another document left is R8's,
  // TestACopiedTabTakesTheMoveStoppedAndStartsNothing.)
  for (const sounding of [true, false]) {
    const r = await started({ output: "bridge", latest: playing({ revision: 3 }) });
    const handover = { tracks: [{ id: "x1", path: "Album/01 A.flac", track: { path: "Album/01 A.flac", title: "A" } }], selected: 0, position: 33, playing: true };
    r.s.on("POST", ROUTES.switchOut, async (e) => (e.body.take ? "lost"
      : answer({ tab: e.body.op.tab, seq: e.body.op.seq, kind: "switch-out", status: "done", effect: "some", sent: 1, of: 1, last: "ok", release: "none", silence: 31 }, pub({ revision: 31 }))));
    r.s.on("GET", ROUTES.operation, async (e) => ({ body: { epoch: E1, revision: 30, record: { tab: e.query.get("tab"), seq: Number(e.query.get("seq")), kind: "switch-out", status: "done", effect: "some", sent: 1, of: 1, last: "ok", release: "none", silence: 12, handover } } }));
    // Before the record is read back, the bridge plays again, or reads silent.
    r.latest = sounding ? playing({ revision: 30 }) : pub({ revision: 30, silent: true });
    r.frame(r.latest);
    await r.out.pick("browser");
    await settle();
    check(`the handover taken, stopped (${sounding ? "the bridge plays again" : "the bridge silent"})`, r.b.calls.includes("take:false") && r.b.taken?.position === 33, [r.b.calls, r.b.taken]);
    const barriers = r.s.posts(ROUTES.switchOut).filter((e) => e.body.take === false);
    if (sounding) {
      check("the bridge plays again: no barrier, nothing started, said", barriers.length === 0 && !r.b.calls.includes("start") && r.v().line === WORDS.bridgeAgain, [r.s.posts(), r.b.calls, r.v().line]);
    } else {
      check("the bridge silent: a new barrier, and the start on its done", barriers.length === 1 && r.b.calls.includes("start"), [r.s.posts(), r.b.calls]);
    }
  }
};

S.TestARefusedSwitchInReadsNothingChangedFromItsEffect = async () => {
  const r = await started({});
  r.s.on("POST", ROUTES.replace, async () => answer(rec(r, { kind: "replace", status: "refused", error: "stale", effect: "none", sent: 1, of: 4, last: "conflict" }), pub({ revision: 4, silent: true })));
  r.s.on("POST", ROUTES.switchOut, async () => answer(rec(r, { kind: "switch-out", status: "done", silence: 6 }), pub({ revision: 6 })));
  await r.out.pick("bridge");
  await settle();
  check("a stale first step sent and refused reads nothing changed: back to the browser", r.v().output === "browser" && r.v().phase === PHASE.browser, r.v().phase);
  check("which resumes through the barrier", r.b.calls.includes("resume"), r.b.calls);
};

S.TestAClockSampleIsTheServersClockAtSend = async () => {
  // M1: a fresh tab given an aged, advancing anchor (its publication 100 s
  // old, steady playback publishing nothing since), then a fresh sample.
  const r = rig({ output: "bridge", latest: playing({ revision: 5, at: new Date(T0).toISOString() }) });
  r.c.advance(100000);
  r.open();
  r.frame(r.latest);
  check("before a sample lands: the anchor's seconds, not extrapolated", Math.abs(r.out.position() - 10) < 0.01, r.out.position());
  await settle();
  const p = r.out.position();
  check("a fresh sample: the anchor advanced by its own age", Math.abs(p - 110) < 0.01, p);
};

S.TestALateClockSampleIsDropped = async () => {
  {
    const r = rig({ output: "bridge", latest: playing({ revision: 5 }) });
    const g = gate();
    let n = 0;
    r.s.on("GET", ROUTES.now, async () => { n += 1; const at = r.c.now(); const late = n <= 2; if (late) await g.p; return { body: r.latest, headers: { [CLOCK_HEADER]: new Date(late ? at + 50000 : at).toISOString() } }; });
    r.c.advance(2000);
    r.open();
    r.frame(r.latest);
    await settle();
    r.out.streamError(r.serial);
    r.open();
    r.frame(r.latest);
    g.open();
    await settle();
    check("a sample asked on an earlier connection is dropped", r.v().sampled && Math.abs(r.out.position() - 12) < 0.01, [r.v().sampled, r.out.position()]);
  }
  {
    const r = rig({ output: "bridge", latest: playing({ revision: 5 }) });
    const g = gate();
    let first = true;
    r.s.on("GET", ROUTES.now, async () => { const at = r.c.now(); const late = first; if (first) { first = false; await g.p; } return { body: r.latest, headers: { [CLOCK_HEADER]: new Date(late ? at + 50000 : at).toISOString() } }; });
    r.c.advance(2000);
    r.open();
    await settle();
    r.frame(r.latest);
    await settle();
    g.open();
    await settle();
    check("a sample asked before the epoch was taken is dropped", Math.abs(r.out.position() - 12) < 0.01, r.out.position());
  }
};

S.TestAFailedClockSampleExtrapolatesNothing = async () => {
  for (const kind of ["lost", "malformed", "no clock"]) {
    const r = rig({ output: "bridge", latest: playing({ revision: 5 }) });
    r.s.on("GET", ROUTES.now, async () => {
      if (kind === "lost") return "lost";
      if (kind === "malformed") return { body: "malformed", headers: { [CLOCK_HEADER]: new Date(r.c.now()).toISOString() } };
      return { body: r.latest };
    });
    r.c.advance(2000);
    r.open();
    r.frame(r.latest);
    await settle();
    r.c.advance(3000);
    check(`a ${kind} sample: the anchor's seconds, no extrapolation`, !r.v().sampled && Math.abs(r.out.position() - 10) < 0.01, [r.v().sampled, r.out.position()]);
    const asked = r.s.gets(ROUTES.now).length;
    r.c.advance(5000);
    r.out.tick();
    await settle();
    check(`a ${kind} sample: asked again within 5 s`, r.s.gets(ROUTES.now).length > asked, r.s.gets(ROUTES.now).length);
  }
};

S.TestAResetSamplesTheClockAgain = async () => {
  for (const reset of ["visible", "jump"]) {
    const r = await anchored();
    const asked = r.s.gets(ROUTES.now).length;
    const g = gate();
    r.s.on("GET", ROUTES.now, async () => { const at = r.c.now(); await g.p; return { body: r.latest, headers: { [CLOCK_HEADER]: new Date(at).toISOString() } }; });
    if (reset === "visible") r.out.resume();
    else { r.out.tick(); r.c.jump(5000); r.out.tick(); }
    await settle();
    check(`a ${reset} reset forgets the samples`, !r.v().sampled, r.v().sampled);
    check(`a ${reset} reset samples again`, r.s.gets(ROUTES.now).length > asked);
    g.open();
    await settle();
    check(`a ${reset} reset's new sample lands`, r.v().sampled);
  }
};

S.TestAClockSampleAskedBeforeAResetIsDropped = async () => {
  // P7: held across a clock jump within one connection and epoch.
  const r = rig({ output: "bridge", latest: playing({ revision: 5 }) });
  const g = gate();
  let n = 0;
  r.s.on("GET", ROUTES.now, async () => { n += 1; const at = r.c.now(); const late = n <= 2; if (late) await g.p; return { body: r.latest, headers: { [CLOCK_HEADER]: new Date(late ? at + 50000 : at).toISOString() } }; });
  r.c.advance(2000);
  r.open();
  r.frame(r.latest);
  await settle();
  r.out.tick();
  r.c.jump(5000);
  r.out.tick();
  g.open();
  await settle();
  check("only a sample asked after the reset enables extrapolation", r.v().sampled && Math.abs(r.out.position() - 17) < 0.01, [r.v().sampled, r.out.position()]);
  check("the held samples, their clock 50 s ahead, did not land", r.out.position() < 30, r.out.position());
};

S.TestClonedTabsNeverShareAnOperation = async () => {
  // M2: two documents from one session storage send identical commands.
  store.clear();
  const tabA = rig({ keep: true, output: "bridge", latest: playing({ revision: 3 }) });
  const copied = new Map(store);
  store.clear();
  for (const [k, v] of copied) store.set(k, v);
  const tabB = rig({ keep: true, output: "bridge", latest: playing({ revision: 3 }) });
  check("two documents, two tab identities", tabA.out.identity().tab !== tabB.out.identity().tab, [tabA.out.identity(), tabB.out.identity()]);
  for (const t of [tabA, tabB]) {
    t.open();
    t.frame(t.latest);
    t.s.on("POST", ROUTES.switchOut, async (e) => answer({ tab: e.body.op.tab, seq: e.body.op.seq, kind: "switch-out", status: "done", effect: "some", sent: 1, of: 1, last: "ok", release: "none", silence: 4 }, pub({ revision: 4 })));
  }
  await settle();
  await tabA.out.pick("browser");
  await tabB.out.pick("browser");
  const a = tabA.s.posts(ROUTES.switchOut)[0]?.body.op;
  const b = tabB.s.posts(ROUTES.switchOut)[0]?.body.op;
  check("identical commands under distinct identities", a && b && a.tab !== b.tab && a.seq === 1 && b.seq === 1, [a, b]);
};

S.TestAnOperationConflictTakesANewTabIdentity = async () => {
  // P2: a 409 operation_conflict, injected.
  const r = await started({ output: "bridge", latest: playing({ revision: 3 }) });
  r.s.on("POST", ROUTES.transport, async () => ({ status: 409, body: { error: "operation_conflict", message: "conflict", player: JSON.stringify(playing({ revision: 3 })) } }));
  const before = r.out.identity();
  await r.out.command("next");
  await settle();
  const after = r.out.identity();
  check("a new tab identity, its count reset", after.tab !== before.tab && after.seq === 0, [before, after]);
  check("the command not sent again", r.s.posts(ROUTES.transport).length === 1);
  check("the bar says so", r.v().line === WORDS.conflict, r.v().line);
};

S.TestARevisionZeroIsNoState = async () => {
  // M5: a publication of revision 0 (the publisher ended before its first).
  const r = rig({ output: "bridge" });
  r.open();
  r.frame(pub({ revision: 0 }));
  await settle();
  check("revision 0 is no state", r.v().state === null, r.v().state);
  r.frame(pub({ revision: 1 }));
  await settle();
  check("revision 1 is", r.v().state?.revision === 1, r.v().state);
};

// ---- The page cases (a DOM model): the shipped audio.js and nowplaying.js ----
//
// Enough of a page for the bar: elements with attributes, classes, children
// and listeners; an <audio> whose load() resets its position, as a browser's
// does, and whose play() answers a promise; navigator.mediaSession with its
// handlers. Each page case runs in its own node process, so its modules'
// state is its own.

class El {
  constructor(tag) {
    this.tagName = String(tag).toUpperCase();
    this.children = []; this.parentNode = null; this.attrs = {}; this.listeners = {};
    this.className = ""; this.dataset = {}; this.hidden = false; this.disabled = false; this.own = "";
    const self = this;
    this.style = { setProperty() {}, removeProperty() {} };
    this.classList = {
      add: (...c) => { for (const x of c) if (!self.classList.contains(x)) self.className = (self.className + " " + x).trim(); },
      remove: (...c) => { self.className = self.className.split(/\s+/).filter((x) => x && !c.includes(x)).join(" "); },
      toggle: (c, on) => { const want = on === undefined ? !self.classList.contains(c) : !!on; if (want) self.classList.add(c); else self.classList.remove(c); return want; },
      contains: (c) => self.className.split(/\s+/).includes(c),
    };
  }
  get textContent() { return this.children.length ? this.children.map((c) => c.textContent).join("") : this.own; }
  set textContent(v) { this.children = []; this.own = String(v); }
  setAttribute(k, v) { this.attrs[k] = String(v); if (k === "src") this._src = String(v); }
  getAttribute(k) { return k in this.attrs ? this.attrs[k] : null; }
  removeAttribute(k) { delete this.attrs[k]; if (k === "src") this._src = ""; }
  hasAttribute(k) { return k in this.attrs; }
  appendChild(c) { if (c.parentNode) c.parentNode.removeChild(c); this.children.push(c); c.parentNode = this; return c; }
  append(...cs) { for (const c of cs) this.appendChild(typeof c === "string" ? textNode(c) : c); }
  insertBefore(c, ref) { this.appendChild(c); return c; }
  removeChild(c) { this.children = this.children.filter((x) => x !== c); c.parentNode = null; return c; }
  remove() { if (this.parentNode) this.parentNode.removeChild(this); }
  get firstChild() { return this.children[0] || null; }
  addEventListener(t, fn) { (this.listeners[t] ||= []).push(fn); }
  removeEventListener(t, fn) { this.listeners[t] = (this.listeners[t] || []).filter((x) => x !== fn); }
  // fire runs the element's listeners, and a click and a keydown bubble to
  // the document's, as a browser's do.
  fire(t, e = {}) {
    const ev = { type: t, target: this, ...e };
    for (const fn of [...(this.listeners[t] || [])]) fn(ev);
    if (t === "click" || t === "keydown") for (const fn of [...(globalThis.document?.listeners?.[t] || [])]) fn(ev);
  }
  click() { if (!this.disabled) this.fire("click"); }
  // The focus as a browser keeps it: on one element, and moved to the body
  // when the element that holds it is disabled (the focus fixup rule).
  focus() { if (globalThis.document) globalThis.document.activeElement = this; }
  get disabled() { return !!this._disabled; }
  set disabled(v) {
    this._disabled = !!v;
    if (v && globalThis.document && globalThis.document.activeElement === this) globalThis.document.activeElement = globalThis.document.body;
  }
  contains(n) { for (let x = n; x; x = x.parentNode) if (x === this) return true; return false; }
  get isConnected() { let x = this; while (x.parentNode) x = x.parentNode; return x === globalThis.document?.body; }
  querySelector(sel) { const tag = sel.toUpperCase(); const walk = (n) => { for (const c of n.children) { if (c.tagName === tag) return c; const f = walk(c); if (f) return f; } return null; }; return walk(this); }
  closest() { return null; }
}
function textNode(s) { const t = new El("#text"); t.own = String(s); return t; }

// The element's media state as the HTML spec moves it, as far as the cases
// need it (round 3, Q6): load() pauses the element with no pause event and
// resets its position, as the media element load algorithm does; a track's
// end pauses it, firing pause and then ended (end()); every play() is
// recorded with the source it played (plays).
class AudioEl extends El {
  constructor() {
    super("audio");
    this.currentTime = 0; this.duration = NaN; this.paused = true; this.readyState = 0; this.error = null; this.ended = false;
    this.buffered = { length: 0 }; this.playbackRate = 1; this.volume = 1; this.preload = "none"; this._src = ""; this.plays = [];
  }
  get src() { return this._src; }
  set src(v) { this._src = String(v); this.attrs.src = String(v); }
  canPlayType(ct) { return /flac|wav|mpeg|mp4|aac/.test(ct) ? "probably" : ""; }
  load() { this.currentTime = 0; this.duration = NaN; this.readyState = 0; this.paused = true; this.ended = false; this.error = null; }
  play() { this.plays.push(this._src); this.ended = false; this.paused = false; this.fire("play"); return Promise.resolve(); }
  pause() { if (!this.paused) { this.paused = true; this.fire("pause"); } }
  // metadata is the browser's loadedmetadata for the source it holds.
  metadata(duration) { this.duration = duration; this.readyState = 1; this.fire("loadedmetadata"); }
  // end is the playback's end: paused, pause fired, then ended.
  end() {
    if (Number.isFinite(this.duration)) this.currentTime = this.duration;
    this.ended = true;
    if (!this.paused) { this.paused = true; this.fire("pause"); }
    this.fire("ended");
  }
  // fail is a media error of code at the source it holds, fired as error.
  fail(code) { this.error = { code }; this.fire("error"); }
}

const session = { handlers: {}, metadata: undefined, playbackState: "", setActionHandler(a, fn) { this.handlers[a] = fn; }, setPositionState() {} };

function page() {
  const body = new El("body");
  const audios = [];
  globalThis.document = {
    body,
    documentElement: new El("html"),
    visibilityState: "visible",
    activeElement: body,
    listeners: {},
    createElement: (tag) => { if (tag === "audio") { const a = new AudioEl(); audios.push(a); return a; } return new El(tag); },
    createElementNS: (ns, tag) => new El(tag),
    createTextNode: textNode,
    getElementById: () => null,
    querySelector: () => null,
    querySelectorAll: () => [],
    addEventListener(t, fn) { (this.listeners[t] ||= []).push(fn); },
  };
  // navigator is a getter on node's global (as crypto is), so it is
  // replaced by defineProperty, never assigned.
  Object.defineProperty(globalThis, "navigator", { value: { mediaSession: session }, configurable: true, writable: true });
  globalThis.MediaMetadata = class { constructor(m) { Object.assign(this, m); } };
  window.MediaMetadata = globalThis.MediaMetadata;
  globalThis.setInterval = () => 0;
  return { body, audios };
}

// pageRig is a page with the shipped audio.js (its element the page's
// first <audio>) and an output over the fake console, the browser half the
// shipped audio.js's own exports.
async function pageRig(o = {}) {
  const p = page();
  if (o.saved) store.set("bridge-player-queue", JSON.stringify(o.saved));
  const audio = await import(new URL("audio.js", dirURL));
  audio.init();
  const el = p.audios[0];
  const c = clock();
  const s = server(c);
  const latest = o.latest || pub();
  s.on("GET", ROUTES.now, async () => ({ body: latest, headers: { [CLOCK_HEADER]: new Date(c.now()).toISOString() } }));
  s.on("GET", ROUTES.queue, async () => ({ body: { epoch: latest.epoch, readAt: latest.revision, queueRev: latest.queueRev, token: 1, selected: 0, entries: [] } }));
  const b = { handover: audio.handover, hasQueue: audio.hasQueue, suspend: audio.suspend, resume: audio.resume, take: audio.take, start: audio.start, mediaSession: audio.setMediaSession };
  const out = createOutput(env(s, c, b));
  installOutput(out);
  // As boot.js wires it: every start of this browser asks the output (R1),
  // and so does what playback does by itself (round 3, S1).
  if (o.gate) {
    audio.setStartGate(out.gateStart);
    audio.setContinueGate?.(out.gateContinue);
  }
  if (!o.noFrame) {
    out.streamOpen(1);
    out.frame(JSON.stringify(latest), 1);
  }
  await settle();
  return { ...p, audio, el, s, c, out, latest, frame: (st) => out.frame(JSON.stringify(st), 1) };
}

const flac = (n, o = {}) => ({ path: `Album/0${n}.flac`, title: `T${n}`, codec: "FLAC", rateHz: 44100, bits: 16, duration: 200, play: { kind: "universal", contentType: "audio/flac" }, ...o });
const dsd = (n, o = {}) => ({ path: `DSD/0${n}.dsf`, title: `D${n}`, codec: "DSF", isDsd: true, rateHz: 2822400, duration: 200, play: { kind: "none", contentType: "audio/x-dsf" }, ...o });

// longTimers holds every timer of a second or more that the modules arm (the
// skip past a source that cannot play, its probe's deadline) until the case
// fires it; a shorter one runs as it would.
function longTimers() {
  const held = new Map();
  let id = 1e6;
  globalThis.setTimeout = (fn, ms, ...a) => {
    if (!(ms >= 1000)) return realTimeout(fn, ms, ...a);
    id += 1;
    held.set(id, { fn, ms });
    return id;
  };
  globalThis.clearTimeout = (t) => { if (!held.delete(t)) realClear(t); };
  return {
    count: (ms) => [...held.values()].filter((t) => t.ms === ms).length,
    fire(ms) { for (const [k, t] of [...held]) if (t.ms === ms) { held.delete(k); t.fn(); } },
  };
}

S.TestTheSwitchReadsTheBrowsersHandoverBeforeItLetsGoOfItsSource = async () => {
  const r = await pageRig();
  r.s.on("POST", ROUTES.replace, async () => answer({ tab: r.out.identity().tab, seq: 1, kind: "replace", status: "done", effect: "some", sent: 4, of: 4, last: "ok", release: "none" }, playing({ revision: 4 })));
  r.audio.playQueue([flac(1), flac(2), flac(3)], 1);
  r.el.metadata(200);
  r.el.currentTime = 42.7;
  r.audio.setShuffle(true);
  await r.out.pick("bridge");
  await settle();
  const b = r.s.posts(ROUTES.replace)[0]?.body;
  check("the position read before the source was let go", b && b.position === 42, b);
  check("the queue as listed, never a shuffle order, from its current track", b && JSON.stringify(b.tracks) === JSON.stringify(["Album/01.flac", "Album/02.flac", "Album/03.flac"]) && b.start === 1, b);
  check("the source let go", !r.el.getAttribute("src") && r.el.currentTime === 0, [r.el.getAttribute("src"), r.el.currentTime]);
  const saved = JSON.parse(store.get("bridge-player-queue") || "{}");
  check("the saved queue keeps the position read, not the element's reset one", saved.at === 42, saved.at);
};

S.TestTheSwitchInvalidatesTheBrowsersPendingFailureProbe = async () => {
  const g = gate();
  globalThis.fetch = async () => { await g.p; return { ok: true, status: 200 }; };
  globalThis.AbortController = class { constructor() { this.signal = {}; } abort() {} };
  const r = await pageRig();
  r.s.on("POST", ROUTES.replace, async () => answer({ tab: r.out.identity().tab, seq: 1, kind: "replace", status: "done", effect: "some", sent: 3, of: 3, last: "ok", release: "none" }, playing({ revision: 4 })));
  const tracks = [flac(1), flac(2)];
  r.audio.playQueue(tracks, 0);
  r.el.error = { code: 4 };
  r.el.fire("error");
  await settle();
  await r.out.pick("bridge");
  await settle();
  g.open();
  await settle();
  const s = r.audio.snapshot();
  check("the probe that finished after the switch marked nothing unplayable", s.queue[0].play.kind === "universal", s.queue[0].play);
  check("and wrote no error", s.error === "", s.error);
};

S.TestTheSwitchCancelsTheRestoredPosition = async () => {
  const r = await pageRig({ saved: { queue: [flac(1), flac(2)], index: 0, at: 77, shuffle: false, repeat: "off" } });
  r.s.on("POST", ROUTES.switchOut, async () => answer({ tab: r.out.identity().tab, seq: r.out.identity().seq, kind: "switch-out", status: "done", effect: "some", sent: 1, of: 1, last: "ok", release: "none", silence: 4,
    handover: { tracks: [{ id: "x9", path: "Album/09.flac", track: flac(9) }], selected: 0, position: 0, playing: false } }, pub({ revision: 4 })));
  r.s.on("POST", ROUTES.replace, async () => answer({ tab: r.out.identity().tab, seq: r.out.identity().seq, kind: "replace", status: "done", effect: "some", sent: 3, of: 3, last: "ok", release: "none" }, playing({ revision: 3 })));
  // The restored track's metadata parked; the bar moves to the bridge and back.
  await r.out.pick("bridge");
  await settle();
  await r.out.pick("browser");
  await settle();
  r.el.metadata(300);
  await settle();
  check("the handover's track from its start, never at the restored track's position", r.el.currentTime === 0, r.el.currentTime);
};

S.TestTheMediaSessionIsClearedWhileTheBridgeIsChosen = async () => {
  const r = await pageRig();
  r.audio.playQueue([flac(1)], 0);
  check("the browser's handlers set", typeof session.handlers.play === "function");
  await r.out.pick("bridge");
  await settle();
  const cleared = ["play", "pause", "previoustrack", "nexttrack", "stop", "seekto", "seekbackward", "seekforward"].every((a) => session.handlers[a] === null);
  check("every handler cleared with the bridge chosen", cleared, Object.keys(session.handlers).filter((a) => session.handlers[a] !== null));
  check("its metadata cleared and its state none", session.metadata === null && session.playbackState === "none", [session.metadata, session.playbackState]);
};

S.TestADSDTrackSwitchedToTheBrowser = async () => {
  const r = await pageRig({ latest: playing({ revision: 3 }) });
  store.set(OUTPUT_KEY, "bridge");
  r.audio.take({ queue: [dsd(1), flac(2)], index: 0, position: 30, playing: true }, true);
  await settle();
  let s = r.audio.snapshot();
  check("a DSD track this browser cannot play stays, paused, with its reason", s.track?.path === "DSD/01.dsf" && !s.playing && /not playable in this browser/.test(s.error), s);
  r.audio.advance(1);
  s = r.audio.snapshot();
  check("Next moves to a track the browser plays", s.track?.path === "Album/02.flac", s.track);
  r.audio.take({ queue: [dsd(3, { play: { kind: "none", contentType: "audio/x-dsf", variantId: "pcm-1", variantContentType: "audio/flac" } })], index: 0, position: 30, playing: true }, true);
  r.el.metadata(200);
  await settle();
  s = r.audio.snapshot();
  check("a DSD track with a fresh FLAC sidecar plays it, said to be a converted copy, from the position", s.degraded && r.el.currentTime === 30, [s.degraded, r.el.currentTime]);
};

// barPage is a page with the shipped bar mounted over it.
async function barPage(o = {}) {
  const r = await pageRig(o);
  const np = await import(new URL("nowplaying.js", dirURL));
  np.mount();
  const bar = r.body.children.find((c) => c.attrs.id === "nowplaying");
  const find = (label) => {
    const walk = (n) => { for (const c of n.children) { if (c.tagName === "BUTTON" && c.attrs["aria-label"] === label) return c; const f = walk(c); if (f) return f; } return null; };
    return walk(bar);
  };
  const byClass = (cls) => { const walk = (n) => { for (const c of n.children) { if (c.classList?.contains(cls)) return c; const f = walk(c); if (f) return f; } return null; }; return walk(bar); };
  return { ...r, bar, find, byClass };
}

S.TestTheBarShowsItsEmptyStripWhereThePlayerIsOffered = async () => {
  const r = await barPage();
  r.out.subscribe(() => {});
  await settle();
  check("offered with nothing chosen: the bar shows", r.bar.hidden === false, r.bar.hidden);
  check("an empty strip saying so", r.byClass("np-title").textContent === WORDS.nothing, r.byClass("np-title").textContent);
  check("with its output button", r.byClass("np-extra").children.some((c) => c.attrs["aria-label"] === "Output: this browser" && !c.hidden));
};

S.TestTheBarDisablesWhatTheEngineDoesNotDo = async () => {
  const st = playing({ revision: 3, commands: ["stop", "pause", "repeat", "replace", "append", "delete"], seek: false });
  store.set(OUTPUT_KEY, "bridge");
  const r = await barPage({ latest: st });
  await settle();
  const off = (label) => r.find(label)?.disabled === true;
  check("pause enabled while the engine takes it", r.find("Pause") && !r.find("Pause").disabled, r.find("Pause")?.disabled);
  check("next and previous disabled where the engine does not take them", off("Next") && off("Previous"));
  check("stop shown and enabled", r.find("Stop") && !r.find("Stop").hidden && !r.find("Stop").disabled);
  check("shuffle disabled, saying why", off("Shuffle") && r.find("Shuffle").title === WORDS.noShuffle, r.find("Shuffle")?.title);
  check("repeat two states, saying repeat-one is not offered", r.find("Repeat off") && r.find("Repeat off").title.includes(WORDS.noRepeatOne), r.find("Repeat off")?.title);
  const vol = r.byClass("np-vol");
  check("volume and mute disabled, the level fixed", vol.disabled === true && vol.title === WORDS.fixedLevel && off("Mute"), [vol.disabled, vol.title]);
  check("the seek disabled where none is offered", r.byClass("np-seek").disabled === true);
};

// ---- Round 2: the bar never plays in both places, never sticks moving ----

const findAll = (root, pred) => { const out = []; const walk = (n) => { for (const c of n.children || []) { if (pred(c)) out.push(c); walk(c); } }; walk(root); return out; };
const refusal = (error, st, status = 409, message) => ({ status, body: { error, message: message || `refused: ${error}`, ...(st ? { player: JSON.stringify(st) } : {}) } });
const recOf = (e, o = {}) => ({ tab: e.body.op.tab, seq: e.body.op.seq, kind: "transport", status: "done", effect: "some", sent: 1, of: 1, last: "ok", release: "none", ...o });
const handoverOf = (o = {}) => ({ tracks: [{ id: "x1", path: "Album/01 A.flac", track: { path: "Album/01 A.flac", title: "A" } }, { id: "x2", path: "Album/02 B.flac", track: { path: "Album/02 B.flac", title: "B" } }], selected: 1, position: 61, playing: true, ...o });
const readBackOf = (e, o = {}) => ({ body: { epoch: E1, revision: 9, record: { tab: e.query.get("tab"), seq: Number(e.query.get("seq")), kind: "switch-out", status: "done", effect: "some", sent: 1, of: 1, last: "ok", release: "none", ...o } } });
const started2 = (o) => started(o);

S.TestARefusalBeforeAdmissionEndsTheSwitchToTheBridge = async () => {
  // R2: a structured refusal with no record ends the switch, back on this
  // browser with the refusal's line.
  for (const k of [
    { name: "epoch_changed", ans: () => refusal("epoch_changed", null), line: WORDS.epochChanged },
    { name: "operation_conflict", ans: () => refusal("operation_conflict", null), line: WORDS.conflict },
    { name: "body_too_large", ans: () => refusal("body_too_large", null, 413, "the request's body is larger than the 8 MiB the player's control takes"), line: "the request's body is larger than the 8 MiB the player's control takes" },
  ]) {
    const r = await started2({ browser: { playing: false } });
    const before = r.out.identity().tab;
    r.s.on("POST", ROUTES.replace, async () => k.ans());
    await r.out.pick("bridge");
    await settle();
    check(`${k.name}: the switch ended on this browser, its line said`, r.v().output === "browser" && r.v().phase === PHASE.browser && r.v().line === k.line, [r.v().output, r.v().phase, r.v().line]);
    check(`${k.name}: a browser that was not playing resumes nothing`, !r.b.calls.includes("resume") && r.s.posts(ROUTES.switchOut).length === 0, [r.b.calls, r.s.posts()]);
    if (k.name === "operation_conflict") check("operation_conflict: a new identity, its count from 0", r.out.identity().tab !== before && r.out.identity().seq === 0, r.out.identity());
  }
  {
    // A browser that was playing resumes, through the gate: a barrier's done.
    const r = await started2({ browser: { playing: true } });
    r.s.on("POST", ROUTES.replace, async () => refusal("bad_json", pub({ revision: 4, silent: true }), 400, "one JSON object, and nothing after it"));
    r.s.on("POST", ROUTES.switchOut, async (e) => answer(recOf(e, { kind: "switch-out", silence: 5 }), pub({ revision: 5 })));
    await r.out.pick("bridge");
    await settle();
    check("a playing browser resumes only through the barrier, take false", r.s.posts(ROUTES.switchOut)[0]?.body.take === false && r.b.calls.includes("resume") && r.v().output === "browser", [r.s.posts(), r.b.calls]);
  }
  {
    // No player offered: this browser is the output, and resumes at once.
    const r = await started2({ browser: { playing: true } });
    r.s.on("POST", ROUTES.replace, async () => refusal("player_not_offered", null, 409, "this bridge offers no player of its own"));
    await r.out.pick("bridge");
    await settle();
    check("not offered: the browser's, no barrier, resumed", r.v().output === "browser" && r.v().notOffered && r.b.calls.includes("resume") && r.s.posts(ROUTES.switchOut).length === 0, [r.v().notOffered, r.b.calls, r.s.posts()]);
  }
  for (const k of [{ name: "an unclassified 5xx", ans: () => refusal("internal_failure", null, 500) }, { name: "a network error", ans: () => "lost" }]) {
    // V2: neither proves the command never ran: unconfirmed, for its record.
    const r = await started2({ browser: { playing: true } });
    r.s.on("POST", ROUTES.replace, async () => k.ans());
    r.s.on("GET", ROUTES.operation, async (e) => readBackOf(e, { kind: "replace", status: "running" }));
    await r.out.pick("bridge");
    await settle();
    check(`${k.name}: unconfirmed on the bridge, read back, no barrier, nothing resumed`, r.v().output === "bridge" && r.v().phase === PHASE.unconfirmed && r.s.gets(ROUTES.operation).length >= 1 &&
      r.s.posts(ROUTES.switchOut).length === 0 && !r.b.calls.includes("resume"), [r.v().output, r.v().phase, r.s.gets(ROUTES.operation).length, r.s.posts(), r.b.calls]);
  }
};

S.TestARefusalBeforeAdmissionEndsTheSwitchToThisBrowserAndTheBarrier = async () => {
  for (const k of [
    { name: "epoch_changed", ans: () => refusal("epoch_changed", null), line: WORDS.epochChanged },
    { name: "operation_conflict", ans: () => refusal("operation_conflict", null), line: WORDS.conflict },
    { name: "bad_json", ans: () => refusal("bad_json", null, 400, "a JSON body is required"), line: "a JSON body is required" },
  ]) {
    const r = await started2({ output: "bridge", latest: playing({ revision: 3 }) });
    r.s.on("POST", ROUTES.switchOut, async () => k.ans());
    await r.out.pick("browser");
    await settle();
    check(`switch-out, ${k.name}: back on the bridge, its line said, nothing taken`, r.v().output === "bridge" && r.v().phase === PHASE.bridge && r.v().line === k.line && !r.b.calls.some((x) => x.startsWith("take")), [r.v().phase, r.v().line, r.b.calls]);
  }
  {
    const r = await started2({ output: "bridge", latest: playing({ revision: 3 }) });
    r.s.on("POST", ROUTES.switchOut, async () => refusal("player_not_offered", null, 409, "no player"));
    await r.out.pick("browser");
    await settle();
    check("switch-out, not offered: this browser the output, nothing taken or started", r.v().output === "browser" && r.v().phase === PHASE.browser && !r.b.calls.some((x) => x.startsWith("take") || x === "start" || x === "resume"), [r.v().output, r.b.calls]);
  }
  {
    const r = await started2({ output: "bridge", latest: playing({ revision: 3 }) });
    r.s.on("POST", ROUTES.switchOut, async () => refusal("internal_failure", null, 500));
    r.s.on("GET", ROUTES.operation, async (e) => readBackOf(e, { status: "running" }));
    await r.out.pick("browser");
    await settle();
    check("switch-out, an unclassified 5xx: unconfirmed, read back", r.v().phase === PHASE.unconfirmed && r.s.gets(ROUTES.operation).length >= 1, r.v().phase);
  }
  {
    // A barrier refused before it ran starts nothing, its line said.
    const r = await started2({ latest: pub({ revision: 3, silent: true }) });
    r.s.on("POST", ROUTES.switchOut, async () => refusal("epoch_changed", null));
    let ran = 0;
    r.out.gateStart(() => { ran += 1; });
    await settle();
    check("a barrier refused epoch_changed: nothing starts, said", ran === 0 && r.v().line === WORDS.epochChanged && r.v().phase === PHASE.browser, [ran, r.v().line]);
  }
};

S.TestOnlyTheNewestPickMovesTheOutput = async () => {
  {
    // The hazard: the bridge picked, its answer slow; this browser picked,
    // done; the first pick's late done moves nothing.
    const r = await started2({ browser: { playing: true } });
    const g = gate();
    r.s.on("POST", ROUTES.replace, async (e) => { await g.p; return answer(recOf(e, { kind: "replace", sent: 4, of: 4 }), pub({ revision: 6 })); });
    r.s.on("POST", ROUTES.switchOut, async (e) => answer(recOf(e, { kind: "switch-out", silence: 5, handover: handoverOf() }), pub({ revision: 5 })));
    const first = r.out.pick("bridge");
    await settle();
    await r.out.pick("browser");
    await settle();
    check("the newer pick, done: this browser takes the queue, playing", r.b.calls.includes("take:true") && r.v().output === "browser", [r.b.calls, r.v().output]);
    const n = r.b.calls.length;
    g.open();
    await first;
    await settle();
    check("the older pick's late done moves neither the output, the phase nor the browser", r.v().output === "browser" && r.v().phase === PHASE.browser && r.b.calls.length === n, [r.v().output, r.v().phase, r.b.calls.slice(n)]);
  }
  {
    // lost(): the older pick's answer lost while the newer one is out.
    const r = await started2({ browser: { playing: true } });
    const g1 = gate();
    const g2 = gate();
    r.s.on("POST", ROUTES.replace, async () => { await g1.p; return "lost"; });
    r.s.on("POST", ROUTES.switchOut, async (e) => { await g2.p; return answer(recOf(e, { kind: "switch-out", silence: 5, handover: handoverOf() }), pub({ revision: 5 })); });
    r.s.on("GET", ROUTES.operation, async (e) => readBackOf(e, { kind: "replace", status: "running" }));
    const first = r.out.pick("bridge");
    await settle();
    const second = r.out.pick("browser");
    await settle();
    g1.open();
    await first;
    await settle();
    check("the older pick's lost answer leaves the newer pick's phase", r.v().phase === PHASE.toBrowser, r.v().phase);
    g2.open();
    await second;
    await settle();
    check("the newer pick ends it", r.v().output === "browser" && r.b.calls.includes("take:true"), [r.v().output, r.b.calls]);
  }
  {
    // unknown: the older pick's record read back gone, after the newer one.
    const r = await started2({ browser: { playing: true } });
    const g = gate();
    r.s.on("POST", ROUTES.replace, async () => "lost");
    r.s.on("GET", ROUTES.operation, async () => { await g.p; return { status: 404, body: { error: "operation_gone", epoch: E1, revision: 5 } }; });
    r.s.on("POST", ROUTES.switchOut, async (e) => answer(recOf(e, { kind: "switch-out", silence: 5, handover: handoverOf() }), pub({ revision: 5 })));
    await r.out.pick("bridge");
    await settle();
    await r.out.pick("browser");
    await settle();
    g.open();
    await settle();
    check("the older pick's record gone: this browser stays the output", r.v().output === "browser" && r.v().phase === PHASE.browser, [r.v().output, r.v().phase]);
  }
  {
    // A refusal before admission of the older pick: no resume, no line.
    const r = await started2({ browser: { playing: true } });
    const g = gate();
    r.s.on("POST", ROUTES.replace, async () => { await g.p; return refusal("epoch_changed", null); });
    r.s.on("POST", ROUTES.switchOut, async (e) => answer(recOf(e, { kind: "switch-out", silence: 5, handover: handoverOf() }), pub({ revision: 5 })));
    const first = r.out.pick("bridge");
    await settle();
    await r.out.pick("browser");
    await settle();
    const n = r.b.calls.length;
    g.open();
    await first;
    await settle();
    check("the older pick refused: nothing resumed, no barrier, the newer pick's line kept", r.b.calls.length === n && r.s.posts(ROUTES.switchOut).length === 1 && r.v().line === "", [r.b.calls.slice(n), r.s.posts(), r.v().line]);
  }
  {
    // V11: a switch-in that ends at once (nothing in the browser) is a
    // pick: the older switch-out's record read back moves nothing.
    const r = await started2({ output: "bridge", latest: playing({ revision: 3 }), browser: { queue: [] } });
    const g = gate();
    r.s.on("POST", ROUTES.switchOut, async () => "lost");
    r.s.on("GET", ROUTES.operation, async (e) => { await g.p; return readBackOf(e, { silence: 5, handover: handoverOf() }); });
    await r.out.pick("browser");
    await settle();
    check("the switch-out lost: unconfirmed", r.v().phase === PHASE.unconfirmed, r.v().phase);
    await r.out.pick("bridge");
    await settle();
    g.open();
    await settle();
    check("the newer pick, at once: the bridge stays the output, nothing taken", r.v().output === "bridge" && r.v().phase === PHASE.bridge && !r.b.calls.some((x) => x.startsWith("take")), [r.v().output, r.v().phase, r.b.calls]);
  }
};

S.TestAStartReadsTheLatestStateAccepted = async () => {
  {
    const r = await started2({ output: "bridge", latest: playing({ revision: 3 }) });
    const g = gate();
    r.s.on("POST", ROUTES.switchOut, async (e) => { await g.p; return answer(recOf(e, { kind: "switch-out", silence: 5, handover: handoverOf() }), pub({ revision: 5 })); });
    const p = r.out.pick("browser");
    await settle();
    r.frame(playing({ revision: 6 }));
    await settle();
    g.open();
    await p;
    await settle();
    check("switch-out done, the bridge playing again since: the queue taken stopped, said", r.b.calls.includes("take:false") && !r.b.calls.includes("take:true") && r.v().line === WORDS.bridgeAgain && r.v().output === "browser", [r.b.calls, r.v().line]);
  }
  {
    const r = await started2({ latest: pub({ revision: 3, silent: true }) });
    const g = gate();
    r.s.on("POST", ROUTES.switchOut, async (e) => { await g.p; return answer(recOf(e, { kind: "switch-out", silence: 5 }), pub({ revision: 5 })); });
    let ran = 0;
    r.out.gateStart(() => { ran += 1; });
    await settle();
    r.frame(playing({ revision: 6 }));
    await settle();
    g.open();
    await settle();
    check("a barrier's done, the bridge playing again since: nothing starts", ran === 0 && r.v().line === WORDS.bridgeAgain, [ran, r.v().line]);
  }
  {
    const r = await started2({ browser: { playing: true } });
    const g = gate();
    r.s.on("POST", ROUTES.replace, async (e) => { await g.p; return answer(recOf(e, { kind: "replace", status: "refused", error: "stale", effect: "none", sent: 1, of: 4, last: "conflict" }), pub({ revision: 4, silent: true })); });
    const p = r.out.pick("bridge");
    await settle();
    r.frame(playing({ revision: 6 }));
    await settle();
    g.open();
    await p;
    await settle();
    check("a refused switch-in, the bridge playing in the latest state: no barrier, nothing resumed", r.s.posts(ROUTES.switchOut).length === 0 && !r.b.calls.includes("resume") && r.v().line === WORDS.bridgePlaying, [r.s.posts(), r.b.calls, r.v().line]);
  }
  {
    // ...and a hold in the latest state, which its answer's did not show:
    // not silent, though nothing sounds, so no barrier either.
    const r = await started2({ browser: { playing: true } });
    const g = gate();
    r.s.on("POST", ROUTES.replace, async (e) => { await g.p; return answer(recOf(e, { kind: "replace", status: "refused", error: "stale", effect: "none", sent: 1, of: 4, last: "conflict" }), pub({ revision: 4, silent: true })); });
    r.s.on("POST", ROUTES.switchOut, async (e) => answer(recOf(e, { kind: "switch-out", status: "refused", error: "check", effect: "none" }), pub({ revision: 7, silent: false, hold: "check" })));
    const p = r.out.pick("bridge");
    await settle();
    r.frame(pub({ revision: 6, silent: false, hold: "check" }));
    await settle();
    g.open();
    await p;
    await settle();
    check("a refused switch-in, a hold in the latest state: no barrier, the hold's words", r.s.posts(ROUTES.switchOut).length === 0 && !r.b.calls.includes("resume") && r.v().line === "The DSD check is playing on the DAC: Stop ends it", [r.s.posts(), r.v().line]);
  }
};

S.TestAStreamRecordWithoutItsWholeRecordIsReadBack = async () => {
  {
    const r = await started2({ output: "bridge", latest: playing({ revision: 3 }) });
    const g = gate();
    let asked = 0;
    r.s.on("POST", ROUTES.switchOut, async () => "lost");
    r.s.on("GET", ROUTES.operation, async (e) => {
      asked += 1;
      if (asked === 1) return readBackOf(e, { status: "running" });
      await g.p;
      return readBackOf(e, { silence: 6, handover: handoverOf({ position: 33, playing: false }) });
    });
    await r.out.pick("browser");
    await settle();
    const id = r.out.identity();
    r.frame(pub({ revision: 7, silent: true, operations: [{ tab: id.tab, seq: id.seq, kind: "switch-out", status: "done", effect: "some", sent: 1, of: 1, last: "ok", release: "none", silence: 6 }] }));
    await settle();
    check("a switch-out's record on the stream, its handover left out: nothing taken, read back", !r.b.calls.some((x) => x.startsWith("take")) && asked >= 2 && r.v().phase === PHASE.unconfirmed, [r.b.calls, asked, r.v().phase]);
    g.open();
    await settle();
    check("the record read back whole completes it, its handover taken stopped", r.b.calls.includes("take:false") && r.b.taken?.position === 33 && r.v().output === "browser", [r.b.calls, r.b.taken]);
  }
  {
    const r = await started2({ output: "bridge", latest: playing({ revision: 3 }) });
    let asked = 0;
    r.s.on("POST", ROUTES.replace, async () => "lost");
    r.s.on("GET", ROUTES.operation, async (e) => {
      asked += 1;
      if (asked === 1) return readBackOf(e, { kind: "replace", status: "running" });
      return readBackOf(e, { kind: "replace", sent: 3, of: 3, leftOut: [{ path: "R/01 X.flac", word: "routed" }] });
    });
    await r.out.replace({ tracks: ["A/01.flac", "R/01 X.flac"] }, { start: 0, play: true });
    await settle();
    const id = r.out.identity();
    r.frame(playing({ revision: 8, operations: [{ tab: id.tab, seq: id.seq, kind: "replace", status: "done", effect: "some", sent: 3, of: 3, last: "ok", release: "none" }] }));
    await settle();
    check("a replace's record on the stream, what it left out left out: read back whole, and said", asked >= 2 && /left out/.test(r.v().line), [asked, r.v().line]);
  }
};

S.TestARequestUnansweredPast30SecondsIsUnconfirmed = async () => {
  for (const order of ["the late answer first", "the read back first"]) {
    const r = await started2({ output: "bridge", latest: playing({ revision: 3 }) });
    const ga = gate();
    const gr = gate();
    r.s.on("POST", ROUTES.switchOut, async (e) => { await ga.p; return answer(recOf(e, { kind: "switch-out", silence: 5, handover: handoverOf() }), pub({ revision: 5 })); });
    r.s.on("GET", ROUTES.operation, async (e) => { await gr.p; return readBackOf(e, { silence: 5, handover: handoverOf({ playing: false }) }); });
    const p = r.out.pick("browser");
    await settle();
    r.c.advance(REQUEST_MS);
    r.out.tick();
    await settle();
    check(`${order}: past 30 s, unconfirmed and read back`, r.v().phase === PHASE.unconfirmed && r.s.gets(ROUTES.operation).length === 1, [r.v().phase, r.s.gets(ROUTES.operation).length]);
    if (order === "the late answer first") {
      ga.open();
      await p;
      await settle();
      check("the late done, its answer not read: nothing taken, still unconfirmed", !r.b.calls.some((x) => x.startsWith("take")) && r.v().phase === PHASE.unconfirmed, [r.b.calls, r.v().phase]);
      gr.open();
      await settle();
    } else {
      gr.open();
      await settle();
      ga.open();
      await p;
      await settle();
    }
    check(`${order}: the record read back resolves it, once, stopped`, r.b.calls.filter((x) => x.startsWith("take")).length === 1 && r.b.calls.includes("take:false"), r.b.calls);
  }
};

S.TestEveryUnresolvedOperationIsKeptUntilItResolves = async () => {
  {
    // V3: a conflict while an older operation is unresolved, then the
    // records of both identities.
    const r = await started2({ output: "bridge", latest: playing({ revision: 3 }) });
    let n = 0;
    r.s.on("POST", ROUTES.transport, async () => { n += 1; return n === 2 ? refusal("operation_conflict", null) : "lost"; });
    r.s.on("GET", ROUTES.operation, async (e) => readBackOf(e, { kind: "transport", status: "running" }));
    const t1 = r.out.identity().tab;
    await r.out.command("next");
    await r.out.command("pause");
    await settle();
    const t2 = r.out.identity().tab;
    await r.out.command("play");
    await settle();
    check("a new identity, the older identity's operation kept beside the new one's", t2 !== t1 && pendingIn(store)?.length === 2, [t1, t2, pendingIn(store)]);
    r.frame(playing({ revision: 4, operations: [
      { tab: t2, seq: 1, kind: "transport", command: "play", status: "done", effect: "some", sent: 1, of: 1, last: "ok", release: "none" },
      { tab: t1, seq: 1, kind: "transport", command: "next", status: "refused", error: "stale", effect: "none", sent: 1, of: 1, last: "conflict", release: "none" },
    ] }));
    await settle();
    check("each resolved by its own record, the older identity's too", pendingIn(store)?.length === 0 && r.v().line === WORDS.stale && r.v().phase === PHASE.bridge, [pendingIn(store), r.v().line, r.v().phase]);
  }
  {
    // A reload, then another: each reads the inherited operation back
    // until it resolves.
    store.clear();
    const old = "ab".repeat(16);
    store.set(OUTPUT_KEY, "bridge");
    store.set(OP_KEY, stored(old, 3, [{ tab: old, seq: 3, epoch: E1, role: "command" }]));
    for (const doc of ["the first reload", "the second"]) {
      const r = rig({ keep: true, latest: playing({ revision: 6 }) });
      r.s.on("GET", ROUTES.operation, async (e) => readBackOf(e, { kind: "transport", status: "running" }));
      r.open();
      r.frame(r.latest);
      await settle();
      const rb = r.s.gets(ROUTES.operation)[0];
      check(`${doc} reads the inherited operation back`, rb && rb.query.get("tab") === old && rb.query.get("seq") === "3", rb && rb.query.toString());
    }
  }
};

S.TestACopiedTabTakesTheMoveStoppedAndStartsNothing = async () => {
  const ma = new Map();
  const a = rig({ storage: storageOf(ma), output: "bridge", latest: playing({ revision: 3 }) });
  a.open();
  a.frame(a.latest);
  await settle();
  const ga = gate();
  let recordA = null;
  a.s.on("POST", ROUTES.switchOut, async (e) => {
    recordA = recOf(e, { kind: "switch-out", status: "running", effect: "none", sent: 0, last: "" });
    await ga.p;
    recordA = { ...recordA, status: "done", effect: "some", sent: 1, last: "ok", silence: 5, handover: handoverOf() };
    return answer(recordA, pub({ revision: 5 }));
  });
  const pa = a.out.pick("browser");
  await settle();
  // A copy of the tab: its session storage, A's operation in it.
  const b = rig({ storage: storageOf(new Map(ma)), latest: playing({ revision: 3 }) });
  b.s.on("GET", ROUTES.operation, async () => ({ body: { epoch: E1, revision: 5, record: recordA } }));
  b.open();
  b.frame(b.latest);
  await settle();
  check("the copy reads the operation back, its record still running", b.s.gets(ROUTES.operation).length >= 1 && b.v().phase === PHASE.unconfirmed, [b.s.gets(ROUTES.operation).length, b.v().phase]);
  ga.open();
  await pa;
  await settle();
  check("the tab that sent it takes the queue, playing", a.b.calls.includes("take:true"), a.b.calls);
  b.frame(pub({ revision: 5, silent: true }));
  b.c.advance(READ_BACK_MS);
  b.out.tick();
  await settle();
  check("the copy takes what it moved, stopped", b.v().output === "browser" && b.b.calls.includes("take:false"), [b.v().output, b.b.calls]);
  check("and starts nothing: no barrier, no start, no resume, no take to play", b.s.posts().length === 0 && !b.b.calls.some((x) => x === "start" || x === "resume" || x === "take:true"), [b.s.posts(), b.b.calls]);
  check("and says to press Play", b.v().line === WORDS.pressPlay, b.v().line);
};

S.TestACopiedTabsRecoveredSwitchInStartsNothing = async () => {
  const ma = new Map();
  const a = rig({ storage: storageOf(ma), latest: pub({ revision: 3, silent: true }), browser: { playing: true } });
  a.open();
  a.frame(a.latest);
  await settle();
  const ga = gate();
  let recordA = null;
  a.s.on("POST", ROUTES.replace, async (e) => {
    recordA = recOf(e, { kind: "replace", status: "running", effect: "none", sent: 0, of: 4, last: "" });
    await ga.p;
    recordA = { ...recordA, status: "refused", error: "stale", effect: "none", sent: 1, last: "conflict" };
    return answer(recordA, pub({ revision: 4, silent: true }));
  });
  a.s.on("POST", ROUTES.switchOut, async (e) => answer(recOf(e, { kind: "switch-out", silence: 6 }), pub({ revision: 6 })));
  const pa = a.out.pick("bridge");
  await settle();
  const b = rig({ storage: storageOf(new Map(ma)), latest: pub({ revision: 3, silent: true }) });
  b.s.on("GET", ROUTES.operation, async () => ({ body: { epoch: E1, revision: 4, record: recordA } }));
  b.open();
  b.frame(b.latest);
  await settle();
  ga.open();
  await pa;
  await settle();
  check("the tab that sent it resumes, through its own barrier", a.b.calls.includes("resume"), a.b.calls);
  b.c.advance(READ_BACK_MS);
  b.out.tick();
  await settle();
  check("the copy: this browser the output", b.v().output === "browser" && b.v().phase === PHASE.browser, [b.v().output, b.v().phase]);
  check("the copy plays nothing: no barrier, no resume, no take to play, no start", b.s.posts().length === 0 && !b.b.calls.some((x) => x === "resume" || x === "start" || x === "take:true"), [b.s.posts(), b.b.calls]);
};

S.TestARecoveredSwitchOutThatDidNotFinishSaysHowItEnded = async () => {
  for (const k of [
    { status: "changed", line: WORDS.wentOn },
    { status: "refused", error: "check", line: "The DSD check is playing on the DAC: Stop ends it" },
    { status: "unknown", line: WORDS.pickAgain },
  ]) {
    store.clear();
    const old = "fa".repeat(16);
    store.set(OUTPUT_KEY, "bridge");
    store.set(OP_KEY, stored(old, 1, [{ tab: old, seq: 1, epoch: E1, role: "switch-out" }]));
    const r = rig({ keep: true, latest: playing({ revision: 9 }) });
    r.s.on("GET", ROUTES.operation, async (e) => readBackOf(e, { status: k.status, error: k.error || "" }));
    r.open();
    r.frame(r.latest);
    await settle();
    check(`a switch-out read back ${k.status}: on the bridge, its words said`, r.v().output === "bridge" && r.v().phase === PHASE.bridge && r.v().line === k.line && !r.b.calls.some((x) => x.startsWith("take")), [r.v().phase, r.v().line]);
  }
};

S.TestAFailedQueueReadIsAskedAgain = async () => {
  const r = rig({ output: "bridge", latest: playing({ revision: 3, queueRev: 3, length: 1 }) });
  let reads = 0;
  r.s.on("GET", ROUTES.queue, async () => { reads += 1; if (reads === 1) return "lost"; return { body: { epoch: E1, readAt: 3, queueRev: 3, token: 1, selected: 11, entries: [{ id: 11, path: "Album/01 A.flac" }] } }; });
  r.open();
  r.frame(r.latest);
  await settle();
  check("the first read failed: no queue", r.v().queue === null && reads === 1, [r.v().queue, reads]);
  r.c.advance(QUEUE_RETRY_MS);
  r.out.tick();
  await settle();
  check("asked again with no new state: the queue read", reads === 2 && r.v().queue?.entries?.length === 1, [reads, r.v().queue]);
};

// ---- Round 2's page cases: the shipped audio.js, nowplaying.js and the gate ----

S.TestEveryStartOfThisBrowserGoesThroughTheGate = async () => {
  const r = await barPage({ gate: true, saved: { queue: [flac(1), flac(2)], index: 0, at: 0, shuffle: false, repeat: "off" }, latest: playing({ revision: 3 }) });
  r.out.subscribe(() => {});
  await settle();
  const quiet = () => r.el.paused && r.s.posts().length === 0;
  r.audio.playQueue([flac(3)], 0);
  await settle();
  check("a row's click beside the bridge playing: nothing plays, nothing sent, the queue kept, said", quiet() && r.audio.snapshot().queue[0].path === "Album/01.flac" && r.out.view().line === WORDS.bridgePlaying, [r.el.paused, r.s.posts(), r.out.view().line]);
  r.find("Play").click();
  await settle();
  check("Play beside the bridge playing: nothing plays", quiet(), [r.el.paused, r.s.posts()]);
  r.find("Next").click();
  await settle();
  check("Next beside the bridge playing: nothing plays, nothing moves", quiet() && r.audio.snapshot().index === 0, [r.el.paused, r.audio.snapshot().index]);
  session.handlers.play?.();
  session.handlers.nexttrack?.();
  await settle();
  check("a lock-screen Play or Next beside the bridge playing: nothing plays", quiet(), [r.el.paused, r.s.posts()]);
  // The bridge stops: Play runs the barrier, and plays on its done alone.
  r.frame(pub({ revision: 4, silent: true }));
  const g = gate();
  r.s.on("POST", ROUTES.switchOut, async (e) => { await g.p; return answer({ tab: e.body.op.tab, seq: e.body.op.seq, kind: "switch-out", status: "done", effect: "some", sent: 1, of: 1, last: "ok", release: "none", silence: 5 }, pub({ revision: 5 })); });
  r.find("Play").click();
  await settle();
  const so = r.s.posts(ROUTES.switchOut)[0];
  check("beside a bridge that does not sound, Play asks the barrier, take false, and waits", so && so.body.take === false && r.el.paused, [so && so.body, r.el.paused]);
  g.open();
  await settle();
  check("and plays on its done", !r.el.paused, r.el.paused);
  const posts = r.s.posts().length;
  r.find("Pause").click();
  check("Pause never waits: paused at once, nothing sent", r.el.paused && r.s.posts().length === posts, [r.el.paused, r.s.posts().length]);
};

S.TestAStartWaitsToKnowWhetherAPlayerIsOffered = async () => {
  // A box that offers no player sends no frame: GET now's typed refusal says so.
  const r = await pageRig({ gate: true, noFrame: true });
  let calls = 0;
  const g = gate();
  r.s.on("GET", ROUTES.now, async () => { calls += 1; if (calls === 1) return "lost"; await g.p; return { status: 409, body: { error: "player_not_offered", message: "no player" } }; });
  await r.out.sampleNow();
  await settle();
  r.audio.playQueue([flac(1)], 0);
  await settle();
  check("not known yet: the start waits, and says so", r.el.paused && r.out.view().line === WORDS.startWaits, [r.el.paused, r.out.view().line]);
  r.c.advance(SAMPLE_RETRY_MS);
  r.out.tick();
  await settle();
  check("a failed first GET now is asked again", calls === 2, calls);
  g.open();
  await settle();
  check("no player offered: the start goes on in this browser, nothing sent", !r.el.paused && r.s.posts().length === 0, [r.el.paused, r.s.posts()]);
};

S.TestAStartBeforeTheFirstFrameWaitsForTheEpoch = async () => {
  const r = await pageRig({ gate: true, noFrame: true, latest: pub({ revision: 2, silent: true }) });
  await r.out.sampleNow();
  await settle();
  r.s.on("POST", ROUTES.switchOut, async (e) => answer({ tab: e.body.op.tab, seq: e.body.op.seq, kind: "switch-out", status: "done", effect: "some", sent: 1, of: 1, last: "ok", release: "none", silence: 3 }, pub({ revision: 3 })));
  r.audio.playQueue([flac(1)], 0);
  await settle();
  r.c.advance(START_WAIT_MS);
  r.out.tick();
  await settle();
  check("a start that never learns the epoch: dropped, said", r.el.paused && r.s.posts().length === 0 && r.out.view().line === WORDS.startUnanswered, [r.el.paused, r.out.view().line]);
  r.audio.playQueue([flac(1)], 0);
  await settle();
  check("offered, no epoch yet: the start waits, nothing sent", r.el.paused && r.s.posts().length === 0 && r.out.view().line === WORDS.startWaits, [r.el.paused, r.s.posts()]);
  r.out.streamOpen(1);
  r.frame(pub({ revision: 2, silent: true }));
  await settle();
  check("the first frame: the barrier, then the start on its done", r.s.posts(ROUTES.switchOut)[0]?.body.take === false && !r.el.paused, [r.s.posts(), r.el.paused]);
};

S.TestADeferredStartAsksItsPermitAgain = async () => {
  store.set(OUTPUT_KEY, "bridge");
  const r = await pageRig({ gate: true, latest: playing({ revision: 3 }) });
  r.s.on("POST", ROUTES.switchOut, async (e) => answer({ tab: e.body.op.tab, seq: e.body.op.seq, kind: "switch-out", status: "done", effect: "some", sent: 1, of: 1, last: "ok", release: "none", silence: 5,
    handover: { tracks: [{ id: "x1", path: "Album/01.flac", track: flac(1) }], selected: 0, position: 30, playing: true } }, pub({ revision: 5 })));
  await r.out.pick("browser");
  await settle();
  check("the barrier's done: the queue taken, its play waiting for the metadata", r.el.paused && !!r.el.getAttribute("src"), [r.el.paused, r.el.getAttribute("src")]);
  r.frame(playing({ revision: 6 }));
  await settle();
  r.el.metadata(200);
  await settle();
  check("the bridge playing since: the deferred play plays nothing", r.el.paused, r.el.paused);
};

S.TestADeferredStartPlaysWhileItsPermitHolds = async () => {
  store.set(OUTPUT_KEY, "bridge");
  const r = await pageRig({ gate: true, latest: playing({ revision: 3 }) });
  r.s.on("POST", ROUTES.switchOut, async (e) => answer({ tab: e.body.op.tab, seq: e.body.op.seq, kind: "switch-out", status: "done", effect: "some", sent: 1, of: 1, last: "ok", release: "none", silence: 5,
    handover: { tracks: [{ id: "x1", path: "Album/01.flac", track: flac(1) }], selected: 0, position: 30, playing: true } }, pub({ revision: 5 })));
  await r.out.pick("browser");
  await settle();
  check("the barrier's done: its play waiting for the metadata", r.el.paused, r.el.paused);
  r.el.metadata(200);
  await settle();
  check("nothing newer: it plays, from its position", !r.el.paused && r.el.currentTime === 30, [r.el.paused, r.el.currentTime]);
};

S.TestAPositionalPickWaitsForTheQueueItNames = async () => {
  store.set(OUTPUT_KEY, "bridge");
  const r = await barPage({ latest: playing({ revision: 3, queueRev: 3, length: 2 }) });
  r.out.subscribe(() => {});
  let reads = 0;
  const g = gate();
  r.s.on("GET", ROUTES.queue, async () => {
    reads += 1;
    if (reads === 1) return { body: { epoch: E1, readAt: 4, queueRev: 4, token: 2, selected: 11, entries: [{ id: 11, path: "Album/01 A.flac" }, { id: 12, path: "Album/02 B.flac" }] } };
    await g.p;
    return { body: { epoch: E1, readAt: 5, queueRev: 5, token: 3, selected: 11, entries: [{ id: 21, path: "Album/09 New.flac" }, { id: 11, path: "Album/01 A.flac" }, { id: 12, path: "Album/02 B.flac" }] } };
  });
  r.frame(playing({ revision: 4, queueRev: 4, length: 2 }));
  await settle();
  r.find("Queue").click();
  await settle();
  const rows = () => findAll(r.body, (n) => n.tagName === "BUTTON" && n.classList.contains("np-queue-title"));
  check("the queue drawn: its rows enabled", rows().length === 2 && rows().every((b) => !b.disabled), rows().map((b) => b.disabled));
  r.frame(playing({ revision: 5, queueRev: 5, length: 3 }));
  await settle();
  check("behind the state's queueRev: every row disabled", rows().length === 2 && rows().every((b) => b.disabled), rows().map((b) => b.disabled));
  rows()[0].fire("click");
  await settle();
  check("a pick by position sends nothing, and says why", r.s.posts(ROUTES.transport).length === 0 && r.out.view().line === WORDS.queueBehind, [r.s.posts(), r.out.view().line]);
  g.open();
  await settle();
  check("the queue read: the rows enabled again", rows().length === 3 && rows().every((b) => !b.disabled), rows().map((b) => b.disabled));
};

S.TestTheMenuAndTheQueueCloseOnEscapeAndOutsideClicks = async () => {
  store.set(OUTPUT_KEY, "bridge");
  const r = await barPage({ latest: playing({ revision: 3 }) });
  r.out.subscribe(() => {});
  await settle();
  const panel = findAll(r.body, (n) => n.attrs?.id === "np-queue")[0];
  const menu = findAll(r.body, (n) => n.attrs?.id === "np-output-menu")[0];
  const qbtn = r.find("Queue");
  qbtn.click();
  check("the queue opens", panel && !panel.hidden, panel?.hidden);
  panel.fire("keydown", { key: "Escape" });
  check("Escape closes the queue, its focus on the Queue button", panel.hidden && document.activeElement === qbtn, [panel.hidden, document.activeElement?.attrs?.["aria-label"]]);
  qbtn.click();
  panel.fire("click");
  check("a click inside the queue leaves it open", !panel.hidden, panel.hidden);
  r.body.fire("click");
  check("a click outside closes the queue", panel.hidden, panel.hidden);
  r.find("Output: the bridge's player").click();
  check("the menu opens", !menu.hidden, menu.hidden);
  r.body.fire("click");
  check("a click outside closes the menu", menu.hidden, menu.hidden);
};

S.TestAPickThatGoesDisabledHandsItsFocusToTheOtherPick = async () => {
  const r = await barPage({ latest: pub({ revision: 3, silent: true }) });
  r.out.subscribe(() => {});
  await settle();
  r.find("Output: this browser").click();
  await settle();
  const [browserPick, bridgePick] = findAll(r.body, (n) => n.tagName === "BUTTON" && n.classList.contains("np-pick"));
  check("the menu opens on the bridge's pick", document.activeElement === bridgePick, document.activeElement?.className);
  r.frame(pub({ revision: 4, silent: true, hold: "check" }));
  await settle();
  check("its pick disabled, the focus on the other pick, inside the menu", bridgePick.disabled && document.activeElement === browserPick, [bridgePick.disabled, document.activeElement?.className]);
};

// ---- Round 3: no start outside the gate, no latch that outlives its barrier ----

// gatedPlaying is a page with the gate wired as boot.js wires it, the bridge
// silent, every barrier answering done at a revision of its own (r.hold
// holds the next one until the case lets it go), and this browser playing
// tracks from a row's start through the gate, its metadata in.
async function gatedPlaying(o = {}) {
  const r = await pageRig({ gate: true, latest: pub({ revision: 3, silent: true }), ...o });
  let n = 0;
  r.hold = null;
  r.s.on("POST", ROUTES.switchOut, async (e) => {
    n += 1;
    const rev = 3 + n;
    const g = r.hold;
    r.hold = null;
    if (g) await g.p;
    return answer(recOf(e, { kind: "switch-out", silence: rev }), pub({ revision: rev, silent: true }));
  });
  r.audio.playQueue(o.tracks || [flac(1), flac(2), flac(3)], 0);
  await settle();
  r.el.metadata(200);
  await settle();
  return r;
}

S.TestASkipPastATrackThatCannotPlayAsksTheGate = async () => {
  // S1, Q5: a source that played, then cannot, its probe done.
  const t = longTimers();
  globalThis.fetch = async () => ({ ok: true, status: 200 });
  const r = await gatedPlaying();
  r.el.fail(4);
  await settle();
  check("a source that cannot play: its probe done, the skip scheduled", t.count(1200) === 1, t.count(1200));
  t.fire(1200);
  await settle();
  check("beside a silent bridge the skip goes through a barrier of its own, and plays the next track on its done",
    r.s.posts(ROUTES.switchOut).length === 2 && !r.el.paused && r.audio.snapshot().index === 1 && /02\.flac/.test(r.el.plays.at(-1) || ""),
    [r.s.posts(ROUTES.switchOut).length, r.el.paused, r.audio.snapshot().index, r.el.plays]);
  r.el.metadata(200);
  r.el.fail(4);
  await settle();
  r.frame(playing({ revision: 20 }));
  await settle();
  const plays = r.el.plays.length;
  t.fire(1200);
  await settle();
  check("the bridge playing since: the skip plays nothing and moves nothing",
    r.el.plays.length === plays && r.audio.snapshot().index === 1 && r.s.posts(ROUTES.switchOut).length === 2,
    [r.el.plays.slice(plays), r.audio.snapshot().index, r.s.posts(ROUTES.switchOut).length]);
  check("and the bar says so, this browser stopped", r.out.view().line === WORDS.bridgePlaying && !r.audio.snapshot().playing, [r.out.view().line, r.audio.snapshot().playing]);
};

S.TestATracksEndAsksItsStartsPermitAgain = async () => {
  // S1: the next track at a track's end goes on its start's permit, asked again.
  const r = await gatedPlaying();
  const posts = r.s.posts().length;
  r.el.end();
  await settle();
  check("a track's end with nothing newer: the next track plays at once on its start's permit, no barrier",
    !r.el.paused && r.audio.snapshot().index === 1 && r.s.posts().length === posts, [r.el.paused, r.audio.snapshot().index, r.s.posts().length - posts]);
  r.el.metadata(200);
  r.frame(playing({ revision: 20 }));
  await settle();
  const plays = r.el.plays.length;
  r.el.end();
  await settle();
  check("a track's end beside the bridge playing since: the next track plays nothing",
    r.el.paused && r.el.plays.length === plays && r.audio.snapshot().index === 1 && r.s.posts().length === posts, [r.el.paused, r.el.plays.slice(plays), r.audio.snapshot().index]);
  check("and the bar says so, this browser stopped", r.out.view().line === WORDS.bridgePlaying && !r.audio.snapshot().playing, [r.out.view().line, r.audio.snapshot().playing]);
};

S.TestARepeatOneReplayAsksTheGate = async () => {
  // S1, Q5: the replay starts the element its end paused, as a start does.
  const r = await gatedPlaying();
  r.audio.cycleRepeat();
  r.audio.cycleRepeat();
  check("repeat one", r.audio.snapshot().repeat === "one", r.audio.snapshot().repeat);
  r.el.end();
  await settle();
  check("its end beside a silent bridge: the replay goes through a barrier of its own and plays on its done, from the start",
    r.s.posts(ROUTES.switchOut).length === 2 && !r.el.paused && r.el.currentTime === 0 && r.audio.snapshot().index === 0, [r.s.posts(ROUTES.switchOut).length, r.el.paused, r.el.currentTime]);
  r.frame(playing({ revision: 20 }));
  await settle();
  const plays = r.el.plays.length;
  r.el.end();
  await settle();
  check("its end beside the bridge playing since: the replay plays nothing", r.el.paused && r.el.plays.length === plays && r.s.posts(ROUTES.switchOut).length === 2, [r.el.paused, r.el.plays.slice(plays)]);
  check("and the bar says so", r.out.view().line === WORDS.bridgePlaying, r.out.view().line);
};

S.TestARemovedRowsSuccessorAsksTheGate = async () => {
  // S1, Q5: the row that plays removed, its successor starts as a start does.
  const r = await gatedPlaying();
  r.audio.removeAt(0);
  await settle();
  check("the playing row removed beside a silent bridge: its successor plays on a barrier of its own",
    r.s.posts(ROUTES.switchOut).length === 2 && !r.el.paused && r.audio.snapshot().track?.path === "Album/02.flac" && /02\.flac/.test(r.el.plays.at(-1) || ""),
    [r.s.posts(ROUTES.switchOut).length, r.el.paused, r.audio.snapshot().track?.path, r.el.plays]);
  r.frame(playing({ revision: 20 }));
  await settle();
  const plays = r.el.plays.length;
  r.audio.removeAt(0);
  await settle();
  check("the playing row removed beside the bridge playing since: its successor is loaded and stays paused",
    r.el.paused && r.el.plays.length === plays && r.audio.snapshot().track?.path === "Album/03.flac" && /03\.flac/.test(r.el.src),
    [r.el.paused, r.el.plays.slice(plays), r.audio.snapshot().track?.path, r.el.src]);
  check("and the bar says so, this browser stopped", r.out.view().line === WORDS.bridgePlaying && !r.audio.snapshot().playing, [r.out.view().line, r.audio.snapshot().playing]);
};

S.TestPlaybackLetGoWithNoPlayerOfferedAsksTheGateToGoOn = async () => {
  // Q1: a start let go on where no player was offered holds no permit, and
  // a null permit is no pass: what follows asks the gate.
  const r = await pageRig({ gate: true, noFrame: true });
  r.s.on("GET", ROUTES.now, async () => ({ status: 409, body: { error: "player_not_offered", message: "no player" } }));
  await r.out.sampleNow();
  await settle();
  r.audio.playQueue([flac(1), flac(2), flac(3)], 0);
  await settle();
  r.el.metadata(200);
  check("no player offered: the start goes on at once, nothing sent", !r.el.paused && r.s.posts().length === 0, [r.el.paused, r.s.posts()]);
  r.el.end();
  await settle();
  check("and so does its end", !r.el.paused && r.audio.snapshot().index === 1 && r.s.posts().length === 0, [r.el.paused, r.audio.snapshot().index, r.s.posts()]);
  r.el.metadata(200);
  // The bridge's player comes up, playing for another controller: a new
  // epoch's first frame.
  r.out.streamOpen(1);
  r.frame(playing({ epoch: E2, revision: 1 }));
  await settle();
  const plays = r.el.plays.length;
  r.el.end();
  await settle();
  check("a player offered since, playing: the next track plays nothing",
    r.el.paused && r.el.plays.length === plays && r.audio.snapshot().index === 1 && r.s.posts().length === 0, [r.el.paused, r.el.plays.slice(plays), r.audio.snapshot().index]);
  check("and the bar says so", r.out.view().line === WORDS.bridgePlaying, r.out.view().line);
};

// skipWaiting is gatedPlaying's first track failing, its skip fired and
// waiting on a barrier the case holds.
async function skipWaiting() {
  const t = longTimers();
  globalThis.fetch = async () => ({ ok: true, status: 200 });
  const r = await gatedPlaying();
  r.el.fail(4);
  await settle();
  const held = gate();
  r.hold = held;
  t.fire(1200);
  await settle();
  check("the skip waits on a barrier of its own", r.s.posts(ROUTES.switchOut).length === 2 && r.audio.snapshot().index === 0, [r.s.posts(ROUTES.switchOut).length, r.audio.snapshot().index]);
  return { r, held };
}

S.TestAContinuationBehindABarrierStopsForAPause = async () => {
  // Q3: the continuation asks again, when it runs, whether this browser
  // still means to play what it continues.
  const { r, held } = await skipWaiting();
  const plays = r.el.plays.length;
  r.audio.toggle();
  await settle();
  held.open();
  await settle();
  check("paused before the barrier's answer: the skip neither plays nor moves",
    r.el.plays.length === plays && r.audio.snapshot().index === 0 && !r.audio.snapshot().playing, [r.el.plays.slice(plays), r.audio.snapshot().index, r.audio.snapshot().playing]);
};

S.TestAContinuationBehindABarrierStopsForTheBrowsersOwnPause = async () => {
  // Q3, Q8: the browser's own pause (a headset unplugged, another tab's
  // audio) ends what this browser means to play, as the reader's does.
  const { r, held } = await skipWaiting();
  const plays = r.el.plays.length;
  r.el.pause();
  await settle();
  held.open();
  await settle();
  check("paused by the browser before the barrier's answer: the skip neither plays nor moves",
    r.el.plays.length === plays && r.audio.snapshot().index === 0 && r.el.paused, [r.el.plays.slice(plays), r.audio.snapshot().index, r.el.paused]);
};

S.TestAContinuationNeverDisplacesAStartTheReaderAsked = async () => {
  // S1: a row the reader played waits on its barrier; the old track's end
  // (a repeat-one replay, asked as a start is) does not take its place.
  const r = await gatedPlaying();
  r.audio.cycleRepeat();
  r.audio.cycleRepeat();
  const held = gate();
  r.hold = held;
  r.audio.playQueue([flac(7), flac(8)], 0);
  await settle();
  check("the reader's row waits on its barrier", r.s.posts(ROUTES.switchOut).length === 2 && r.audio.snapshot().track?.path === "Album/01.flac", [r.s.posts(ROUTES.switchOut).length, r.audio.snapshot().track?.path]);
  const plays = r.el.plays.length;
  r.el.end();
  await settle();
  check("the old track's end meanwhile replays nothing and sends nothing", r.el.plays.length === plays && r.el.paused && r.s.posts(ROUTES.switchOut).length === 2, [r.el.plays.slice(plays), r.el.paused, r.s.posts(ROUTES.switchOut).length]);
  held.open();
  await settle();
  check("on the barrier's done the reader's row plays", r.audio.snapshot().track?.path === "Album/07.flac" && /07\.flac/.test(r.el.plays.at(-1) || "") && !r.el.paused, [r.audio.snapshot().track?.path, r.el.plays]);
};

S.TestAContinuationBehindABarrierLeavesAReplacedQueueAlone = async () => {
  // Q3: a queue the reader replaced meanwhile is the start the barrier
  // runs, and the skip moves nothing of it.
  const { r, held } = await skipWaiting();
  r.audio.playQueue([flac(7), flac(8)], 0);
  await settle();
  check("the replacement waits on the same barrier, nothing sent again", r.s.posts(ROUTES.switchOut).length === 2 && r.audio.snapshot().track?.path === "Album/01.flac", [r.s.posts(ROUTES.switchOut).length, r.audio.snapshot().track?.path]);
  held.open();
  await settle();
  const s = r.audio.snapshot();
  check("on the barrier's done the replacement plays from its start, and the skip moves nothing",
    s.queue.length === 2 && s.track?.path === "Album/07.flac" && s.index === 0 && /07\.flac/.test(r.el.plays.at(-1) || "") && !r.el.paused, [s.queue.length, s.track?.path, s.index, r.el.plays]);
};

const name = scenarioName;
if (!S[name]) {
  R.fail.push({ name: "scenario", detail: `no scenario ${name}` });
} else {
  try {
    await S[name]();
  } catch (e) {
    R.fail.push({ name: "threw", detail: String(e && e.stack || e) });
  }
}
console.log(JSON.stringify(R));
process.exit(0);
