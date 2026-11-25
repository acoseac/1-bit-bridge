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
      return response(res, () => { e.reads = (e.reads || 0) + 1; });
    },
  };
  return s;
}

// response is an answer whose body's every read is counted on its request's
// log entry (reads).
function response({ status = 200, body = null, headers = {} }, read = () => {}) {
  const lower = Object.fromEntries(Object.entries(headers).map(([k, v]) => [k.toLowerCase(), v]));
  return {
    ok: status >= 200 && status < 300,
    status,
    headers: { get: (n) => lower[n.toLowerCase()] ?? null },
    json: async () => {
      read();
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
  // A page, so this browser's decoder is asked as the queue asks it.
  page();
  const pcm = { path: "A/01.flac", codec: "FLAC", rateHz: 96000, play: { kind: "universal", contentType: "audio/flac" }, bridge: { way: "pcm" } };
  const native = { path: "D/01.dsf", codec: "DSF", isDsd: true, rateHz: 2822400, play: { kind: "none" }, bridge: { way: "native" } };
  const dop = { path: "D/02.dsf", codec: "DSF", isDsd: true, rateHz: 5644800, play: { kind: "none" }, bridge: { way: "dop" } };
  const conv = { path: "D/03.dsf", codec: "DSF", isDsd: true, rateHz: 11289600, play: { kind: "none" }, bridge: { way: "converted" } };
  const routed = { path: "R/01.flac", codec: "FLAC", routed: true, play: { kind: "universal" }, bridge: { way: "none", reason: "routed" } };
  const b = (t) => playFor(t, "bridge");
  const w = (t) => playFor(t, "browser");
  check("the browser: DSD not playable, PCM plays", !w(native).playable && w(native).chip === "DSD — not playable in a browser · the bridge's player plays it (DSD64, native)" && w(pcm).playable, [w(native), w(pcm)]);
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
  // The bar starts on the bridge's phase with an empty line, so the record's
  // resolution is what the operation's end shows.
  check("its operation resolved by that record, none left pending", r.out.pending().length === 0, r.out.pending());
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
  // Eight operations of other tabs fill the stream's eight: the tab's record
  // is not among them.
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
  // the document's, as a browser's do; a pointerdown bubbles through the
  // element's ancestors to the document.
  fire(t, e = {}) {
    const ev = { type: t, target: this, ...e };
    for (const fn of [...(this.listeners[t] || [])]) fn(ev);
    if (t === "pointerdown") for (let n = this.parentNode; n; n = n.parentNode) for (const fn of [...(n.listeners?.[t] || [])]) fn(ev);
    if (t === "click" || t === "keydown" || t === "pointerdown") for (const fn of [...(globalThis.document?.listeners?.[t] || [])]) fn(ev);
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
  // A tag or a class (".track"), in document order, as far as the modules the
  // cases load ask.
  querySelector(sel) { return this.querySelectorAll(sel)[0] || null; }
  querySelectorAll(sel) {
    const want = sel.startsWith(".") ? (c) => c.classList.contains(sel.slice(1)) : (c) => c.tagName === sel.toUpperCase();
    const out = [];
    const walk = (n) => { for (const c of n.children) { if (want(c)) out.push(c); walk(c); } };
    walk(this);
    return out;
  }
  closest() { return null; }
}
function textNode(s) { const t = new El("#text"); t.own = String(s); return t; }

// The element's media state as the HTML spec moves it, as far as the cases
// need it (round 3, Q6; round 4, W2): play() on a paused element flips
// `paused` at once, and its `play` event and its promise's settlement come
// later as queued media element tasks, in the browser's order; pause() flips
// it at once and queues `pause` with the AbortError of every play still
// pending; load() pauses the element with no event, removes the queued tasks,
// settling at once the promises they would have settled (a play's resolved,
// those a pause took refused with AbortError), as the spec's load algorithm
// does, refuses the pending plays (AbortError) and resets its position and
// error; a track's end (end()) and a media error (fail()) are queued tasks too, an
// error of code 4 refusing the pending plays (NotSupportedError). A play's
// promise resolves after its event by default (playMode "resolve"), stays
// pending until the case settles it ("pend": settlePlays()), or is refused at
// once with NotAllowedError ("deny"). Every play() is recorded with the source
// it played (plays).
const domError = (name) => Object.assign(new Error(name), { name });
class AudioEl extends El {
  constructor() {
    super("audio");
    this.currentTime = 0; this.duration = NaN; this.paused = true; this.readyState = 0; this.error = null; this.ended = false;
    this.buffered = { length: 0 }; this.playbackRate = 1; this.volume = 1; this.preload = "none"; this._src = ""; this.plays = [];
    this.playMode = "resolve"; this.pending = []; this.tasks = [];
  }
  get src() { return this._src; }
  set src(v) { this._src = String(v); this.attrs.src = String(v); }
  canPlayType(ct) { return /flac|wav|mpeg|mp4|aac/.test(ct) ? "probably" : ""; }
  // task queues fn as a media element task, which a load() removes, running
  // its settle (the promises fn would have settled) as it does.
  task(fn, settle) {
    const t = { live: true, settle };
    this.tasks.push(t);
    realTimeout(() => { if (t.live) { t.live = false; fn(); } }, 0);
  }
  refusePlays(name) { const ps = this.pending; this.pending = []; for (const q of ps) q.reject(domError(name)); }
  settlePlays() { const ps = this.pending; this.pending = []; for (const q of ps) q.resolve(); }
  load() {
    for (const t of this.tasks) {
      if (t.live) t.settle?.();
      t.live = false;
    }
    this.tasks = [];
    this.currentTime = 0; this.duration = NaN; this.readyState = 0; this.ended = false; this.error = null;
    if (!this.paused) this.paused = true;
    this.refusePlays("AbortError");
  }
  play() {
    this.plays.push(this._src);
    if (this.playMode === "deny") return Promise.reject(domError("NotAllowedError"));
    if (this.error?.code === 4) return Promise.reject(domError("NotSupportedError"));
    const p = new Promise((resolve, reject) => this.pending.push({ resolve, reject }));
    if (this.paused) {
      this.paused = false;
      this.ended = false;
      this.task(() => this.fire("play"));
    }
    // "Resolve": the element can play, so the promises pending now are taken
    // and resolved by a task of their own (the spec's notify about playing);
    // a later play's are not.
    if (this.playMode === "resolve") {
      const taken = this.pending;
      this.pending = [];
      const resolve = () => { for (const q of taken) q.resolve(); };
      this.task(resolve, resolve);
    }
    return p;
  }
  pause() {
    if (this.paused) return;
    this.paused = true;
    const ps = this.pending;
    this.pending = [];
    const refuse = () => { for (const q of ps) q.reject(domError("AbortError")); };
    this.task(() => { this.fire("pause"); refuse(); }, refuse);
  }
  // browserPlay is the browser's own play (a call ending, the audio focus
  // given back): no page script holds its promise.
  browserPlay() { this.play().catch(() => {}); }
  // metadata is the browser's loadedmetadata for the source it holds.
  metadata(duration) { this.duration = duration; this.readyState = 1; this.fire("loadedmetadata"); }
  // end is the playback's end: paused, pause fired, then ended.
  end() {
    this.task(() => {
      if (Number.isFinite(this.duration)) this.currentTime = this.duration;
      this.ended = true;
      if (!this.paused) {
        this.paused = true;
        this.fire("pause");
        this.refusePlays("AbortError");
      }
      this.fire("ended");
    });
  }
  // fail is a media error of code at the source it holds, fired as error.
  fail(code) {
    this.task(() => {
      this.error = { code };
      this.fire("error");
      if (code === 4) this.refusePlays("NotSupportedError");
    });
  }
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
    audio.setStartGate(out.gateStart, out.cancelStarts);
    audio.setContinueGate?.(out.gateContinue);
  } else if (!o.unwired) {
    // A case about something else gets a gate that lets every start go on;
    // `unwired` stands for a boot that failed before it wired either gate.
    audio.setStartGate((run) => run(null));
    audio.setContinueGate?.((permit, run) => run(permit));
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
    check(`${order}: the late answer's body is never read`, !r.s.posts(ROUTES.switchOut)[0]?.reads, r.s.posts(ROUTES.switchOut)[0]?.reads);
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
  check("the lock screen's Play and Next are set", typeof session.handlers.play === "function" && typeof session.handlers.nexttrack === "function",
    [typeof session.handlers.play, typeof session.handlers.nexttrack]);
  session.handlers.play();
  session.handlers.nexttrack();
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
  // Round 4 (T2's class): that refusal ends the intent too, so a retry after
  // an error, the bridge silent since, plays nothing.
  r.frame(pub({ revision: 7, silent: true }));
  await settle();
  const plays = r.el.plays.length;
  r.el.fail(2);
  await settle();
  check("the bridge silent since, a retry after an error plays nothing", r.el.paused && r.el.plays.length === plays, [r.el.paused, r.el.plays.slice(plays)]);
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
// tracks from a row's start through the gate, its metadata in; with `pend`,
// its element leaves a play's promise pending (playMode "pend"), as a browser
// does while the source has not loaded, and no metadata has come; with `bar`,
// the shipped bar is mounted over it (barPage).
async function gatedPlaying(o = {}) {
  const r = await (o.bar ? barPage : pageRig)({ gate: true, latest: pub({ revision: 3, silent: true }), ...o });
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
  if (o.pend) r.el.playMode = "pend";
  r.audio.playQueue(o.tracks || [flac(1), flac(2), flac(3)], 0);
  await settle();
  if (o.pend) return r;
  r.el.metadata(200);
  await settle();
  return r;
}

S.TestASkipPastATrackThatCannotPlayAsksTheGate = async () => {
  // S1, Q5: a source that played, then cannot, its probe done. The error
  // comes after the metadata, its play already settled, an order that stands
  // for the paths where a play's rejection was swallowed; a source a browser
  // cannot play fails before its metadata and refuses its pending play,
  // which no skip follows (round 4, W11: that order is the follow-up's).
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
  // Round 4 (T2, W7): four playable tracks, so the last removal still leaves
  // a successor to start.
  const r = await gatedPlaying({ tracks: [flac(1), flac(2), flac(3), flac(4)] });
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
  // T2: that refusal ended what this browser meant to play, so with the
  // bridge silent again the next removal starts nothing.
  r.frame(pub({ revision: 21, silent: true }));
  await settle();
  const after = r.el.plays.length;
  r.audio.removeAt(0);
  await settle();
  check("the bridge silent again, the row removed once more: nothing plays and no barrier is asked",
    r.el.paused && r.el.plays.length === after && r.s.posts(ROUTES.switchOut).length === 2 && r.audio.snapshot().track?.path === "Album/04.flac",
    [r.el.paused, r.el.plays.slice(after), r.s.posts(ROUTES.switchOut).length, r.audio.snapshot().track?.path]);
};

S.TestARefusedContinuationEndsTheIntentToPlay = async () => {
  // T2, W6: a continuation refused at once (its permit no longer holds) and
  // one its barrier refuses later each end what this browser meant to play;
  // a held barrier that answers done still plays its continuation.
  const r = await gatedPlaying({ tracks: [flac(1), flac(2), flac(3), flac(4), flac(5)] });
  r.frame(playing({ revision: 20 }));
  await settle();
  r.el.end();
  await settle();
  r.frame(pub({ revision: 21, silent: true }));
  await settle();
  let plays = r.el.plays.length;
  r.el.fail(2);
  await settle();
  check("refused at once at a track's end: the retry after an error, the bridge silent since, plays nothing",
    r.el.plays.length === plays && r.s.posts(ROUTES.switchOut).length === 1 && !r.audio.snapshot().playing, [r.el.plays.slice(plays), r.s.posts(ROUTES.switchOut).length]);
  // The reader's Play, through the gate: playing again.
  r.audio.toggle();
  await settle();
  check("the reader's Play plays again, on a barrier", !r.el.paused && r.s.posts(ROUTES.switchOut).length === 2, [r.el.paused, r.s.posts(ROUTES.switchOut).length]);
  // A removed row's successor waits on a barrier the bridge's start
  // overtakes: refused later, at its done.
  const held = gate();
  r.hold = held;
  r.audio.removeAt(0);
  await settle();
  r.frame(playing({ revision: 30 }));
  await settle();
  held.open();
  await settle();
  r.frame(pub({ revision: 31, silent: true }));
  await settle();
  plays = r.el.plays.length;
  r.audio.removeAt(0);
  await settle();
  check("refused at its barrier's done: the next removal, the bridge silent since, starts nothing",
    r.el.plays.length === plays && r.s.posts(ROUTES.switchOut).length === 3 && r.el.paused, [r.el.plays.slice(plays), r.s.posts(ROUTES.switchOut).length]);
  // The reader's Play again, then a removal whose held barrier answers done.
  r.audio.toggle();
  await settle();
  const held2 = gate();
  r.hold = held2;
  plays = r.el.plays.length;
  r.audio.removeAt(0);
  await settle();
  held2.open();
  await settle();
  check("a held barrier that answers done: the successor plays", r.el.plays.length === plays + 1 && !r.el.paused && r.audio.snapshot().track?.path === "Album/04.flac",
    [r.el.plays.slice(plays), r.el.paused, r.audio.snapshot().track?.path]);
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
// waiting on a barrier the case holds; its error comes after the metadata,
// in TestASkipPastATrackThatCannotPlayAsksTheGate's order (round 4, W11).
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
  // still means to play what it continues. The element's own pause: the
  // bar's Play/Pause over a failed track is a start.
  const { r, held } = await skipWaiting();
  const plays = r.el.plays.length;
  r.el.pause();
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

S.TestARetryPlaysOnlyWhatThisBrowserMeantToPlay = async () => {
  // S2, Q6, Q8: the retry after a network or decode error reloads the
  // source, which pauses the element; it plays only where this browser
  // meant to play, and asks the start's permit again.
  const r = await gatedPlaying();
  // A take made stopped, as output.js makes one beside a bridge that plays
  // again: its metadata in, then a Range fetch fails.
  r.audio.take({ queue: [flac(4), flac(5), flac(6)], index: 0, position: 30, playing: true }, false);
  r.el.metadata(200);
  await settle();
  let plays = r.el.plays.length;
  r.el.fail(2);
  await settle();
  check("a take made stopped meets a network error: the retry plays nothing", r.el.paused && r.el.plays.length === plays && !r.audio.snapshot().playing, [r.el.paused, r.el.plays.slice(plays)]);
  // Playing, through the gate: the retry plays again, under its permit.
  r.audio.advance(1);
  await settle();
  r.el.metadata(200);
  plays = r.el.plays.length;
  check("Next plays the next track, through a barrier", !r.el.paused && r.audio.snapshot().track?.path === "Album/05.flac", [r.el.paused, r.audio.snapshot().track?.path]);
  r.el.fail(2);
  await settle();
  check("a playing track meets a network error: the retry reloads it, paused by the reload, and plays it again",
    !r.el.paused && r.el.plays.length === plays + 1 && /05\.flac.*_r=/.test(r.el.plays.at(-1) || ""), [r.el.paused, r.el.plays.slice(plays)]);
  // Playing, then a state that shows the bridge playing: the permit refuses.
  r.audio.advance(1);
  await settle();
  r.el.metadata(200);
  r.frame(playing({ revision: 20 }));
  await settle();
  plays = r.el.plays.length;
  r.el.fail(2);
  await settle();
  check("the bridge playing since: the retry plays nothing, said", r.el.paused && r.el.plays.length === plays && r.out.view().line === WORDS.bridgePlaying,
    [r.el.paused, r.el.plays.slice(plays), r.out.view().line]);
};

S.TestARestoredQueuesRetryPlaysNothing = async () => {
  // S2: a queue restored at page load, paused, whose metadata is in when a
  // Range fetch fails; the bridge silent, so a retry the gate let through
  // would sound beside the tab this one was copied from.
  const r = await pageRig({ gate: true, saved: { queue: [flac(1), flac(2)], index: 0, at: 40, shuffle: false, repeat: "off" }, latest: pub({ revision: 3, silent: true }) });
  r.s.on("POST", ROUTES.switchOut, async (e) => answer(recOf(e, { kind: "switch-out", silence: 4 }), pub({ revision: 4, silent: true })));
  r.el.metadata(200);
  await settle();
  check("restored paused at its position", r.el.paused && r.el.currentTime === 40 && r.el.plays.length === 0, [r.el.paused, r.el.currentTime]);
  r.el.fail(2);
  await settle();
  check("its retry plays nothing and asks nothing", r.el.paused && r.el.plays.length === 0 && !r.audio.snapshot().playing && r.s.posts().length === 0, [r.el.paused, r.el.plays, r.s.posts()]);
};

// barrierRig is a tab beside a silent bridge whose first barrier's POST
// never answers (a half-open connection) and whose later ones answer done
// in the epoch the latest state names.
async function barrierRig(o = {}) {
  const r = await started2({ latest: pub({ revision: 3, silent: true }), ...o });
  let n = 0;
  r.s.on("POST", ROUTES.switchOut, async (e) => {
    n += 1;
    if (n === 1) return new Promise(() => {});
    const ep = r.v().state?.epoch || E1;
    return answer(recOf(e, { kind: "switch-out", silence: 30 + n }), pub({ epoch: ep, revision: 30 + n, silent: true }));
  });
  r.ran = 0;
  r.out.gateStart(() => { r.ran += 1; });
  await settle();
  check("the barrier sent, unanswered", r.s.posts(ROUTES.switchOut).length === 1 && r.ran === 0, [r.s.posts(ROUTES.switchOut).length, r.ran]);
  return r;
}

S.TestABarrierLostPastItsDeadlineFreesTheNextStart = async () => {
  // S3, Q2, Q9: lost at 30 s, its record read back done: the latch goes
  // with it, nothing starts, and the next start needs a barrier of its own.
  const r = await barrierRig();
  r.s.on("GET", ROUTES.operation, async (e) => readBackOf(e, { silence: 4 }));
  r.c.advance(REQUEST_MS);
  r.out.tick();
  await settle();
  check("past 30 s its record read back done: nothing starts, and the bar asks for Play again",
    r.ran === 0 && r.s.gets(ROUTES.operation).length >= 1 && r.v().line === WORDS.startAgain && r.v().phase === PHASE.browser,
    [r.ran, r.s.gets(ROUTES.operation).length, r.v().line, r.v().phase]);
  r.out.gateStart(() => { r.ran += 1; });
  await settle();
  check("Play again sends a barrier of its own, and starts on its answer alone", r.s.posts(ROUTES.switchOut).length === 2 && r.ran === 1, [r.s.posts(ROUTES.switchOut).length, r.ran]);
};

S.TestEveryEndOfABarrierFreesTheNextStart = async () => {
  // Q9: a barrier its epoch's end made unknown, and one a newer pick
  // superseded, free the next start as its record does.
  {
    const r = await barrierRig();
    r.out.streamError(r.serial);
    r.open();
    r.frame(pub({ epoch: E2, revision: 1, silent: true }));
    await settle();
    check("a restart makes it unknown: nothing starts, and the bar asks for Play again", r.ran === 0 && r.v().line === WORDS.startAgain, [r.ran, r.v().line]);
    r.out.gateStart(() => { r.ran += 1; });
    await settle();
    check("the next start sends a barrier of its own in the new epoch, and starts on its answer", r.s.posts(ROUTES.switchOut).length === 2 && r.s.posts(ROUTES.switchOut)[1].body.op.epoch === E2 && r.ran === 1,
      [r.s.posts(ROUTES.switchOut).map((e) => e.body.op.epoch), r.ran]);
  }
  {
    const r = await barrierRig({ browser: { playing: true } });
    r.s.on("POST", ROUTES.replace, async (e) => answer(recOf(e, { kind: "replace", status: "refused", error: "stale", effect: "none", sent: 1, of: 4, last: "conflict" }), pub({ revision: 4, silent: true })));
    await r.out.pick("bridge");
    await settle();
    check("a pick of the bridge meanwhile, refused with nothing changed: the browser's resume sends a barrier of its own and resumes on its answer",
      r.s.posts(ROUTES.switchOut).length === 2 && r.b.calls.includes("resume") && r.ran === 0 && r.v().output === "browser", [r.s.posts(ROUTES.switchOut).length, r.b.calls, r.ran]);
  }
  {
    // ...and one refused before it ran.
    const r = await started2({ latest: pub({ revision: 3, silent: true }) });
    let n = 0;
    r.s.on("POST", ROUTES.switchOut, async (e) => { n += 1; return n === 1 ? refusal("operation_conflict", null) : answer(recOf(e, { kind: "switch-out", silence: 5 }), pub({ revision: 5, silent: true })); });
    let ran = 0;
    r.out.gateStart(() => { ran += 1; });
    await settle();
    check("a barrier refused before it ran starts nothing", ran === 0 && r.v().line === WORDS.conflict, [ran, r.v().line]);
    r.out.gateStart(() => { ran += 1; });
    await settle();
    check("the next start sends a barrier of its own, and starts on its answer", r.s.posts(ROUTES.switchOut).length === 2 && ran === 1, [r.s.posts(ROUTES.switchOut).length, ran]);
  }
};

S.TestAReloadKeepsANewerPicksSupersession = async () => {
  // S4, Q4, Q10: round 2's V11 case (a switch to this browser lost, then a
  // pick of the bridge from an empty browser, which ends at once and sends
  // nothing), then a reload over a copy of its session storage, and a
  // second over the first's: the switch-out read back done, its handover
  // with it, moves nothing, and neither document holds the phase for it.
  const ma = new Map();
  const a = rig({ storage: storageOf(ma), output: "bridge", latest: playing({ revision: 3 }), browser: { queue: [] } });
  a.open();
  a.frame(a.latest);
  await settle();
  a.s.on("POST", ROUTES.switchOut, async () => "lost");
  a.s.on("GET", ROUTES.operation, async () => new Promise(() => {}));
  await a.out.pick("browser");
  await settle();
  check("the switch to this browser lost: unconfirmed", a.v().phase === PHASE.unconfirmed, a.v().phase);
  await a.out.pick("bridge");
  await settle();
  check("the newer pick, at once: the bridge, nothing sent", a.v().output === "bridge" && a.v().phase === PHASE.bridge && a.s.posts().length === 1, [a.v().output, a.v().phase, a.s.posts().length]);
  const doc = (storage, held) => {
    const r = rig({ storage, latest: playing({ revision: 9 }) });
    r.s.on("GET", ROUTES.operation, async (e) => { await held.p; return readBackOf(e, { silence: 5, handover: handoverOf() }); });
    return r;
  };
  const mb = new Map(ma);
  const gb = gate();
  const b = doc(storageOf(mb), gb);
  check("the reload: the bridge, its phase not held", b.v().output === "bridge" && b.v().phase === PHASE.bridge && b.v().line !== WORDS.unconfirmed, [b.v().output, b.v().phase, b.v().line]);
  b.open();
  b.frame(b.latest);
  await settle();
  const gc = gate();
  const c = doc(storageOf(new Map(mb)), gc);
  check("a second reload, over the first's storage: the bridge, its phase not held", c.v().output === "bridge" && c.v().phase === PHASE.bridge && c.v().line !== WORDS.unconfirmed, [c.v().output, c.v().phase, c.v().line]);
  c.open();
  c.frame(c.latest);
  await settle();
  gb.open();
  gc.open();
  await settle();
  for (const [n, r] of [["the reload", b], ["the second reload", c]]) {
    check(`${n}: the switch-out read back done moves nothing: the bridge, nothing taken`,
      r.s.gets(ROUTES.operation).length >= 1 && r.v().output === "bridge" && r.v().phase === PHASE.bridge && !r.b.calls.some((x) => x.startsWith("take")),
      [r.s.gets(ROUTES.operation).length, r.v().output, r.v().phase, r.b.calls]);
  }
};

S.TestABarrierRefusedForNoPlayerLetsItsStartGoOn = async () => {
  // A barrier refused with a not-offered word (the console's player taken
  // away meanwhile): no player is offered, so the start it held goes on in
  // this browser, as a start does where none is offered, and as a refused
  // switch to the bridge's resume already does.
  const r = await started2({ latest: pub({ revision: 3, silent: true }) });
  r.s.on("POST", ROUTES.switchOut, async () => refusal("player_not_offered", null, 409, "this bridge offers no player of its own"));
  let ran = 0;
  const permits = [];
  r.out.gateStart((p) => { ran += 1; permits.push(p); });
  await settle();
  check("refused for no player: the start goes on in this browser, with no permit to ask", ran === 1 && permits[0] === null && r.v().notOffered, [ran, permits, r.v().notOffered]);
  r.out.gateStart(() => { ran += 1; });
  await settle();
  check("and the next start goes on at once, nothing sent", ran === 2 && r.s.posts(ROUTES.switchOut).length === 1, [ran, r.s.posts(ROUTES.switchOut).length]);
};

// ---- Round 4: no play the bar did not start sounds outside the gate ----
//
// The browser's own pause and resume (a call, another app's audio) are the
// element's pause() and play() called by the case, never through audio.js.

S.TestTheBrowsersOwnResumeBesideAPlayingBridgeStaysPaused = async () => {
  // T1(a): another controller starts the bridge while a call has the
  // browser paused; the call ends and the browser resumes by itself.
  const r = await gatedPlaying();
  r.frame(playing({ revision: 20 }));
  await settle();
  r.el.pause();
  await settle();
  const plays = r.el.plays.length;
  const posts = r.s.posts().length;
  r.el.browserPlay();
  await settle();
  check("the browser's own resume beside the bridge playing: the element ends paused", r.el.paused && !r.audio.snapshot().playing, [r.el.paused, r.audio.snapshot().playing]);
  check("no play of the bar's after it, nothing sent", r.el.plays.length === plays + 1 && r.s.posts().length === posts, [r.el.plays.slice(plays), r.s.posts().length - posts]);
  check("and the bar says the bridge plays", r.out.view().line === WORDS.bridgePlaying, r.out.view().line);
};

S.TestTheBrowsersOwnResumeBesideASilentBridgeGoesThroughTheGate = async () => {
  // T1(b): the same beside a silent bridge: one barrier, then one play of
  // the bar's, and the intent it sets carries the album on.
  const r = await gatedPlaying();
  r.el.pause();
  await settle();
  const plays = r.el.plays.length;
  const posts = r.s.posts(ROUTES.switchOut).length;
  r.el.browserPlay();
  await settle();
  check("beside a silent bridge the browser's own resume plays again through a barrier and one play of the bar's",
    !r.el.paused && r.s.posts(ROUTES.switchOut).length === posts + 1 && r.el.plays.length === plays + 2, [r.el.paused, r.s.posts(ROUTES.switchOut).length - posts, r.el.plays.slice(plays)]);
  r.el.end();
  await settle();
  check("its intent set: the track's end plays the next track", !r.el.paused && r.audio.snapshot().index === 1, [r.el.paused, r.audio.snapshot().index]);
};

S.TestAPlayMarkLivesOnlyUntilItsEventOrItsSettlement = async () => {
  // T1(c), W1: a play the browser refuses at once, and one made on an
  // element already playing, leave no mark a later play of the browser's
  // own could take.
  const r = await gatedPlaying();
  r.audio.toggle();
  await settle();
  r.el.playMode = "deny";
  r.audio.toggle();
  await settle();
  check("the reader's Play, refused by the browser: paused, and said", r.el.paused && !r.audio.snapshot().playing && r.audio.snapshot().error === "Press play to start.",
    [r.el.paused, r.audio.snapshot().playing, r.audio.snapshot().error]);
  r.el.playMode = "resolve";
  r.frame(playing({ revision: 20 }));
  await settle();
  r.el.browserPlay();
  await settle();
  check("a refused play left no mark: the browser's own play beside the bridge playing ends paused", r.el.paused, r.el.paused);
  r.frame(pub({ revision: 21, silent: true }));
  await settle();
  r.audio.toggle();
  await settle();
  check("the reader's Play again: playing", !r.el.paused, r.el.paused);
  r.audio.start();
  await settle();
  r.frame(playing({ revision: 22 }));
  await settle();
  r.el.pause();
  await settle();
  r.el.browserPlay();
  await settle();
  check("a play on an element already playing left no mark: the browser's own play ends paused", r.el.paused, r.el.paused);
};

S.TestTheBrowsersOwnResumeWaitingOnItsGateEndsForTheReader = async () => {
  // T1(d), W4: while the browser's own resume waits on its barrier the bar
  // shows it starting; the reader's Play/Pause cancels it, and a queue the
  // reader replaces meanwhile plays in its place.
  const r = await gatedPlaying();
  r.el.pause();
  await settle();
  const h1 = gate();
  r.hold = h1;
  r.el.browserPlay();
  await settle();
  check("the browser's own resume waits on a barrier, the element paused, shown starting", r.el.paused && r.audio.snapshot().loading && r.s.posts(ROUTES.switchOut).length === 2,
    [r.el.paused, r.audio.snapshot().loading, r.s.posts(ROUTES.switchOut).length]);
  const plays = r.el.plays.length;
  r.audio.toggle();
  await settle();
  check("the reader's Play/Pause while it waits cancels it, asking nothing", !r.audio.snapshot().loading && r.s.posts(ROUTES.switchOut).length === 2, [r.audio.snapshot().loading, r.s.posts(ROUTES.switchOut).length]);
  h1.open();
  await settle();
  check("its barrier's answer then resumes nothing", r.el.paused && r.el.plays.length === plays, [r.el.paused, r.el.plays.slice(plays)]);
  r.audio.toggle();
  await settle();
  r.el.pause();
  await settle();
  const h2 = gate();
  r.hold = h2;
  r.el.browserPlay();
  await settle();
  const before = r.el.plays.length;
  r.audio.playQueue([flac(7), flac(8)], 0);
  await settle();
  h2.open();
  await settle();
  const s = r.audio.snapshot();
  check("a queue replaced while it waits: the replacement plays, the old track never resumes",
    s.track?.path === "Album/07.flac" && !r.el.paused && r.el.plays.slice(before).every((src) => /07\.flac/.test(src)) && r.el.plays.length === before + 1, [s.track?.path, r.el.paused, r.el.plays.slice(before)]);
};

S.TestAnAdmittedResumeOfTheBrowsersOwnGoesOnAtTheTracksEnd = async () => {
  // W3: the protective pause is no cancellation: with no player offered the
  // browser's own resume is admitted at once, and once every queued event
  // has run the track's end still plays the next one.
  const r = await pageRig({ gate: true, noFrame: true });
  r.s.on("GET", ROUTES.now, async () => ({ status: 409, body: { error: "player_not_offered", message: "no player" } }));
  await r.out.sampleNow();
  await settle();
  r.audio.playQueue([flac(1), flac(2)], 0);
  await settle();
  r.el.metadata(200);
  r.el.pause();
  await settle();
  r.el.browserPlay();
  await settle();
  check("no player offered: the browser's own resume is admitted, playing", !r.el.paused && r.audio.snapshot().playing, [r.el.paused, r.audio.snapshot().playing]);
  r.el.end();
  await settle();
  check("every queued event run, the track's end plays the next track", !r.el.paused && r.audio.snapshot().index === 1, [r.el.paused, r.audio.snapshot().index]);
};

S.TestTheBrowsersOwnResumeNeverTakesAReadersPlace = async () => {
  // W5: a row the reader played waits on its barrier; the old track's own
  // resume asks with a continuation's priority and takes nothing of it.
  const r = await gatedPlaying();
  const held = gate();
  r.hold = held;
  r.audio.playQueue([flac(7), flac(8)], 0);
  await settle();
  r.el.pause();
  await settle();
  const plays = r.el.plays.length;
  r.el.browserPlay();
  await settle();
  check("the old track's own resume while the reader's row waits: paused, no play of the bar's", r.el.paused && r.el.plays.length === plays + 1, [r.el.paused, r.el.plays.slice(plays)]);
  held.open();
  await settle();
  check("the barrier's done plays the reader's row", r.audio.snapshot().track?.path === "Album/07.flac" && !r.el.paused && /07\.flac/.test(r.el.plays.at(-1) || ""), [r.audio.snapshot().track?.path, r.el.plays]);
};

S.TestARejectedPlayEndsTheIntentToPlay = async () => {
  // W8: a play the browser refuses ends what this browser meant to play,
  // wherever the bar made it: the retry after an error, a repeat-one replay.
  const r = await gatedPlaying({ tracks: [flac(1), flac(2), flac(3), flac(4), flac(5)] });
  r.el.playMode = "deny";
  r.el.fail(2);
  await settle();
  check("the retry's play refused: paused, not playing, said", r.el.paused && !r.audio.snapshot().playing && r.audio.snapshot().error === "Press play to start.",
    [r.el.paused, r.audio.snapshot().playing, r.audio.snapshot().error]);
  r.el.playMode = "resolve";
  let plays = r.el.plays.length;
  let posts = r.s.posts(ROUTES.switchOut).length;
  r.audio.removeAt(0);
  await settle();
  check("its intent gone: the removed row's successor starts nothing", r.el.plays.length === plays && r.s.posts(ROUTES.switchOut).length === posts, [r.el.plays.slice(plays), r.s.posts(ROUTES.switchOut).length - posts]);
  r.audio.toggle();
  await settle();
  r.audio.cycleRepeat();
  r.audio.cycleRepeat();
  r.el.playMode = "deny";
  r.el.end();
  await settle();
  check("a repeat-one replay refused: paused, said", r.el.paused && r.audio.snapshot().error === "Press play to start.", [r.el.paused, r.audio.snapshot().error]);
  r.el.playMode = "resolve";
  plays = r.el.plays.length;
  posts = r.s.posts(ROUTES.switchOut).length;
  r.audio.removeAt(0);
  await settle();
  check("its intent gone too: the next removal starts nothing", r.el.plays.length === plays && r.s.posts(ROUTES.switchOut).length === posts, [r.el.plays.slice(plays), r.s.posts(ROUTES.switchOut).length - posts]);
};

// notOfferedResume is a browser playing at 42 s whose pick of the bridge is
// refused as not offered, so it resumes at once, with no permit, its play
// deferred to the track's metadata.
async function notOfferedResume() {
  const r = await gatedPlaying();
  r.el.currentTime = 42;
  r.s.on("POST", ROUTES.replace, async () => refusal("player_not_offered", null, 409, "this bridge offers no player of its own"));
  await r.out.pick("bridge");
  await settle();
  check("refused as not offered: this browser the output, its resume waiting for the metadata", r.out.view().output === "browser" && r.out.view().notOffered && r.el.paused, [r.out.view().output, r.out.view().notOffered, r.el.paused]);
  return r;
}

S.TestADeferredStartWithNoPermitAsksTheGateThatHoldsWhenItRuns = async () => {
  // W9: a player offered since, playing, before the metadata: the deferred
  // play asks the gate as it is then, which refuses it.
  const r = await notOfferedResume();
  r.frame(playing({ revision: 20 }));
  await settle();
  const plays = r.el.plays.length;
  r.el.metadata(200);
  await settle();
  check("a player offered since that plays: the deferred play plays nothing", r.el.paused && r.el.plays.length === plays, [r.el.paused, r.el.plays.slice(plays)]);
  check("and the bar says so", r.out.view().line === WORDS.bridgePlaying, r.out.view().line);
};

S.TestADeferredStartWithNoPermitPlaysWhileNoPlayerIsOffered = async () => {
  // W9's other side: still no player offered at the metadata, it plays, from
  // its position.
  const r = await notOfferedResume();
  r.el.metadata(200);
  await settle();
  check("still no player offered: the deferred play plays, from its position", !r.el.paused && r.el.currentTime === 42, [r.el.paused, r.el.currentTime]);
};

// ---- Round 5: a load leaves no protective pause counted, and no gate means no start ----

// oneTask lets the tasks queued so far run, and none of those they queue.
const oneTask = () => new Promise((resolve) => realTimeout(resolve, 0));

S.TestALoadClearsTheProtectivePausesCount = async () => {
  // A load that lands between the protective pause of the browser's own play
  // and that pause's event removes the event, and the count of protective
  // pauses goes with it: the browser's next pause still ends the intent.
  const r = await pageRig({ gate: true, noFrame: true });
  r.s.on("GET", ROUTES.now, async () => ({ status: 409, body: { error: "player_not_offered", message: "no player" } }));
  await r.out.sampleNow();
  await settle();
  r.audio.playQueue([flac(1), flac(2), flac(3)], 0);
  await settle();
  r.el.metadata(200);
  r.el.pause();
  await settle();
  r.el.browserPlay();
  await oneTask();
  r.audio.removeAt(0);
  await settle();
  check("the playing row removed before the protective pause's event: its successor plays", !r.el.paused && r.audio.snapshot().track?.path === "Album/02.flac",
    [r.el.paused, r.audio.snapshot().track?.path]);
  r.el.pause();
  await settle();
  const plays = r.el.plays.length;
  r.audio.removeAt(0);
  await settle();
  check("the browser's next pause ended the intent: the next removal starts nothing",
    r.el.paused && r.el.plays.length === plays && r.audio.snapshot().track?.path === "Album/03.flac", [r.el.paused, r.el.plays.slice(plays), r.audio.snapshot().track?.path]);
};

S.TestALoadBesideASoundingBridgeLeavesTheBrowserSilent = async () => {
  // The same sequence beside the bridge's player playing: its gate refuses
  // the browser's resume at once, so the browser stays silent.
  const r = await gatedPlaying();
  r.frame(playing({ revision: 20 }));
  await settle();
  r.el.pause();
  await settle();
  r.el.browserPlay();
  await oneTask();
  r.audio.removeAt(0);
  await settle();
  r.el.pause();
  await settle();
  const plays = r.el.plays.length;
  r.audio.removeAt(0);
  await settle();
  check("beside the bridge playing the browser stays silent", r.el.paused && r.el.plays.length === plays, [r.el.paused, r.el.plays.slice(plays)]);
  check("and the bar says the bridge plays", r.out.view().line === WORDS.bridgePlaying, r.out.view().line);
};

// unwiredPage is a boot that threw after audio.init() restored a queue and
// set its source, before it wired either gate.
async function unwiredPage() {
  const r = await pageRig({ unwired: true, noFrame: true, saved: { queue: [flac(1), flac(2)], index: 0, at: 0, shuffle: false, repeat: "off" } });
  check("the restore set a source", !!r.el.getAttribute("src"), r.el.getAttribute("src"));
  return r;
}

S.TestAnUnwiredGateRefusesTheBrowsersOwnPlay = async () => {
  const r = await unwiredPage();
  r.el.browserPlay();
  await settle();
  check("the browser's own play with no gate wired: the element ends paused", r.el.paused && !r.audio.snapshot().playing, [r.el.paused, r.audio.snapshot().playing]);
};

S.TestAnUnwiredGateRefusesEveryStart = async () => {
  const r = await unwiredPage();
  check("the lock screen's Play and Next are set", typeof session.handlers.play === "function" && typeof session.handlers.nexttrack === "function",
    [typeof session.handlers.play, typeof session.handlers.nexttrack]);
  session.handlers.play();
  await settle();
  check("the lock screen's Play with no gate wired: nothing plays", r.el.paused && r.el.plays.length === 0, [r.el.paused, r.el.plays]);
  session.handlers.nexttrack();
  await settle();
  check("and its Next: nothing plays and nothing moves", r.el.paused && r.el.plays.length === 0 && r.audio.snapshot().index === 0, [r.el.paused, r.el.plays, r.audio.snapshot().index]);
};

// ---- A resume's wait, a failed track, a deferred start, a handover and the menu ----

// browserResumeWaits pauses the element as the browser does, lets the bar's
// barrier for the browser's own resume be held (r.hold) and resumes it as the
// browser does: the resume then waits on that barrier, shown starting.
async function browserResumeWaits(r) {
  r.el.pause();
  await settle();
  const held = gate();
  r.hold = held;
  r.el.browserPlay();
  await settle();
  return held;
}

S.TestAResumeWaitTheReaderCancelsStopsItsSpinner = async () => {
  const r = await gatedPlaying();
  let shown = null;
  r.audio.subscribe((s) => { shown = s; });
  for (const [how, act] of [["a row clicked", () => r.audio.playQueue([flac(7)], 0)], ["Next", () => r.audio.advance(1)]]) {
    r.frame(pub({ revision: how === "Next" ? 30 : 10, silent: true }));
    await settle();
    const held = await browserResumeWaits(r);
    check(`${how}: the browser's own resume waits on its barrier, shown starting`, r.audio.snapshot().loading && shown?.loading, [r.audio.snapshot().loading, shown?.loading]);
    r.frame(playing({ revision: how === "Next" ? 31 : 20 }));
    await settle();
    act();
    await settle();
    held.open();
    await settle();
    check(`${how} beside the bridge playing, refused, ends the resume and its spinner, the bar told`, !r.audio.snapshot().loading && shown && !shown.loading && r.el.paused,
      [r.audio.snapshot().loading, shown?.loading, r.el.paused]);
  }
};

S.TestRemovingARowThatIsNotPlayingCancelsNoResume = async () => {
  const r = await gatedPlaying({ tracks: [flac(1), flac(2), flac(3), flac(4)] });
  const resumed = async (act) => {
    const held = await browserResumeWaits(r);
    const plays = r.el.plays.length;
    act();
    held.open();
    await settle();
    return r.el.plays.length - plays;
  };
  const removed = await resumed(() => r.audio.removeAt(2));
  check("a row that is not playing removed while the browser's own resume waits: it resumes", removed === 1 && !r.el.paused && r.audio.snapshot().index === 0,
    [removed, r.el.paused, r.audio.snapshot().index]);
  const others = await resumed(() => {
    r.audio.enqueue([flac(5)]);
    r.audio.seek(10);
    r.audio.setVolume(0.5);
    r.audio.setShuffle(false);
    r.audio.cycleRepeat();
  });
  check("an append, a seek, the level, shuffle and repeat meanwhile cancel nothing either", others === 1 && !r.el.paused, [others, r.el.paused]);
};

S.TestRemovingTheOnlyRowLeavesNoTrackAndNothingPlaying = async () => {
  const r = await gatedPlaying({ tracks: [flac(1)] });
  check("playing its one track", !r.el.paused && r.audio.snapshot().index === 0, [r.el.paused, r.audio.snapshot().index]);
  r.audio.removeAt(0);
  await settle();
  const s = r.audio.snapshot();
  check("the queue empty points at no track, and nothing plays", s.queue.length === 0 && s.index === -1 && s.track === null && r.el.paused && !s.playing && !s.loading,
    [s.queue.length, s.index, s.track, r.el.paused, s.playing, s.loading]);
};

// failing is gatedPlaying whose skips past a source that cannot play are held
// until the case fires them, the probe answering that the bridge served it.
async function failing(o = {}) {
  const t = longTimers();
  globalThis.fetch = async () => ({ ok: true, status: 200 });
  const r = await gatedPlaying(o);
  return { r, t };
}

S.TestAnAlbumSkipsPastATrackTheBrowserCannotPlay = async () => {
  // A browser raises the source's error before its metadata, the bar's play
  // still pending, and refuses that play with NotSupportedError: the error's
  // skip moves on, through a barrier of its own.
  const { r, t } = await failing({ pend: true });
  r.el.fail(4);
  await settle();
  check("the source refused before its metadata: the skip scheduled, the bar showing it stopped", t.count(1200) === 1 && !r.audio.snapshot().playing,
    [t.count(1200), r.audio.snapshot().playing]);
  t.fire(1200);
  await settle();
  check("the skip plays the next track on its barrier's done", r.audio.snapshot().index === 1 && /02\.flac/.test(r.el.plays.at(-1) || "") && r.s.posts(ROUTES.switchOut).length === 2,
    [r.audio.snapshot().index, r.el.plays, r.s.posts(ROUTES.switchOut).length]);
};

S.TestTheStopAfterThreeUnplayableTracksStartsNothingMore = async () => {
  const { r, t } = await failing({ pend: true, tracks: [flac(1), flac(2), flac(3), flac(4), flac(5)] });
  for (let i = 0; i < 2; i++) {
    r.el.fail(4);
    await settle();
    t.fire(1200);
    await settle();
  }
  r.el.fail(4);
  await settle();
  const s = r.audio.snapshot();
  check("the third unplayable track in a row: stopped, and said", s.index === 2 && !s.playing && t.count(1200) === 0 && /can't play in this browser/.test(s.error),
    [s.index, s.playing, t.count(1200), s.error]);
  const plays = r.el.plays.length;
  const posts = r.s.posts(ROUTES.switchOut).length;
  r.audio.removeAt(2);
  await settle();
  check("that stop ended the intent: removing its row starts nothing", r.el.plays.length === plays && r.s.posts(ROUTES.switchOut).length === posts,
    [r.el.plays.slice(plays), r.s.posts(ROUTES.switchOut).length - posts]);
};

S.TestASkipUnderRepeatOneMovesPastTheFailedTrack = async () => {
  const { r, t } = await failing({ pend: true });
  r.audio.cycleRepeat();
  r.audio.cycleRepeat();
  check("repeat one", r.audio.snapshot().repeat === "one", r.audio.snapshot().repeat);
  r.el.fail(4);
  await settle();
  t.fire(1200);
  await settle();
  check("under repeat one the skip moves past the failed track and never replays it", r.audio.snapshot().index === 1 && /02\.flac/.test(r.el.plays.at(-1) || ""),
    [r.audio.snapshot().index, r.el.plays]);
  r.el.settlePlays();
  r.el.playMode = "resolve";
  r.el.metadata(200);
  await settle();
  const plays = r.el.plays.length;
  r.el.end();
  await settle();
  check("a track that plays still repeats at its end", r.audio.snapshot().index === 1 && r.el.plays.length === plays + 1 && /02\.flac/.test(r.el.plays.at(-1) || "") && r.el.currentTime === 0 && !r.el.paused,
    [r.audio.snapshot().index, r.el.plays.slice(plays), r.el.currentTime, r.el.paused]);
};

// handedOver is a tab on the bridge picking this browser: its switch-out
// answers done with handover, under a state that shows the bridge playing
// again where `again` says so, and every barrier after it answers done.
async function handedOver(handover, o = {}) {
  store.set(OUTPUT_KEY, "bridge");
  const r = await pageRig({ gate: true, latest: playing({ revision: 3 }) });
  r.n = 0;
  r.s.on("POST", ROUTES.switchOut, async (e) => {
    r.n += 1;
    const rev = 4 + r.n;
    const st = o.again && r.n === 1 ? playing({ revision: rev + 1 }) : pub({ revision: rev, silent: true });
    return answer(recOf(e, { kind: "switch-out", silence: rev, ...(r.n === 1 ? { handover } : {}) }), st);
  });
  await r.out.pick("browser");
  await settle();
  return r;
}

const handoverAt30 = { tracks: [{ id: "x1", path: "Album/01.flac", track: flac(1) }, { id: "x2", path: "Album/02.flac", track: flac(2) }], selected: 0, position: 30, playing: true };

S.TestAPlayingHandoverWhoseSourceFailsBeforeItsMetadataSkips = async () => {
  const t = longTimers();
  globalThis.fetch = async () => ({ ok: true, status: 200 });
  const r = await handedOver(handoverAt30);
  check("the handover taken to play at 30 s, waiting for its metadata", r.el.paused && r.audio.snapshot().index === 0, [r.el.paused, r.audio.snapshot().index]);
  r.el.fail(4);
  await settle();
  t.fire(1200);
  await settle();
  check("its source refused before the metadata: the skip plays the next track on a barrier of its own", r.audio.snapshot().index === 1 && /02\.flac/.test(r.el.plays.at(-1) || "") && r.n === 2,
    [r.audio.snapshot().index, r.el.plays, r.n]);
};

S.TestAHandoverTakenStoppedWhoseSourceFailsStartsNothing = async () => {
  const t = longTimers();
  globalThis.fetch = async () => ({ ok: true, status: 200 });
  const r = await handedOver(handoverAt30, { again: true });
  check("the bridge playing again: the handover taken stopped", r.el.paused && r.out.view().line === WORDS.bridgeAgain, [r.el.paused, r.out.view().line]);
  r.frame(pub({ revision: 9, silent: true }));
  await settle();
  r.el.fail(4);
  await settle();
  t.fire(1200);
  await settle();
  check("its source refused before the metadata: nothing skips, nothing plays", r.audio.snapshot().index === 0 && r.el.plays.length === 0 && r.n === 1,
    [r.audio.snapshot().index, r.el.plays, r.n]);
};

// gatesSeen wraps the page's two gates so a case reads what each was asked
// and what each ran or dropped, in order: a start's ("start asked", "start
// ran"), and what playback does by itself ("skip asked", "skip ran", "skip
// dropped").
function gatesSeen(r) {
  const seen = [];
  r.audio.setStartGate((run, o) => {
    seen.push("start asked");
    r.out.gateStart((p) => { seen.push("start ran"); run(p); }, o);
  }, r.out.cancelStarts);
  r.audio.setContinueGate((permit, run, drop) => {
    seen.push("skip asked");
    r.out.gateContinue(permit, (p) => { seen.push("skip ran"); run(p); }, () => { seen.push("skip dropped"); drop?.(); });
  });
  return seen;
}

S.TestPlayOverAFailedTrackStartsAgain = async () => {
  // skipWaiting's steps, the gates watched before the skip asks: Play over
  // the failed track takes the skip's place, and the barrier's answer runs
  // the reader's start, never the skip.
  const t = longTimers();
  globalThis.fetch = async () => ({ ok: true, status: 200 });
  const r = await gatedPlaying();
  const seen = gatesSeen(r);
  r.el.fail(4);
  await settle();
  const held = gate();
  r.hold = held;
  t.fire(1200);
  await settle();
  check("the failed track shown stopped, its skip waiting on a barrier of its own", !r.audio.snapshot().playing && r.s.posts(ROUTES.switchOut).length === 2 && seen.join() === "skip asked",
    [r.audio.snapshot().playing, r.s.posts(ROUTES.switchOut).length, seen]);
  r.audio.toggle();
  await settle();
  check("Play over it takes the skip's place behind the barrier, asking nothing more", r.s.posts(ROUTES.switchOut).length === 2 && seen.join() === "skip asked,start asked,skip dropped",
    [r.s.posts(ROUTES.switchOut).length, seen]);
  held.open();
  await settle();
  check("on the barrier's done the reader's start runs, the skip never", seen.join() === "skip asked,start asked,skip dropped,start ran", seen);
  check("and plays the next playable track", r.audio.snapshot().index === 1 && /02\.flac/.test(r.el.plays.at(-1) || "") && !r.el.paused,
    [r.audio.snapshot().index, r.el.plays, r.el.paused]);
};

// reloadsAfter is Play over a track whose error (2, network; 3, decode) came
// back after its retry: a start that reloads the track, its retry there again.
const reloadsAfter = (code) => async () => {
  const r = await gatedPlaying();
  r.el.fail(code);
  await settle();
  r.el.metadata(200);
  await settle();
  check(`an error of code ${code} retried at once`, /01\.flac.*_r=/.test(r.el.plays.at(-1) || "") && !r.el.paused, [r.el.plays, r.el.paused]);
  r.el.fail(code);
  await settle();
  check("a second one: it failed, said, stopped", r.audio.snapshot().error === 'Playback failed for "T1".' && !r.audio.snapshot().playing, [r.audio.snapshot().error, r.audio.snapshot().playing]);
  const plays = r.el.plays.length;
  r.audio.toggle();
  await settle();
  check("Play over it reloads the track through a barrier and plays it", r.el.plays.length === plays + 1 && /01\.flac$/.test(r.el.plays.at(-1) || "") && !r.el.paused && r.audio.snapshot().error === "",
    [r.el.plays.slice(plays), r.el.paused, r.audio.snapshot().error]);
  r.el.metadata(200);
  await settle();
  r.el.fail(code);
  await settle();
  check("its automatic retry is there again", r.el.plays.length === plays + 2 && /01\.flac.*_r=/.test(r.el.plays.at(-1) || "") && r.audio.snapshot().error === "",
    [r.el.plays.slice(plays), r.audio.snapshot().error]);
};

S.TestPlayOverATrackThatFailedForGoodReloadsIt = reloadsAfter(2);
S.TestPlayOverATrackThatFailedToDecodeForGoodReloadsIt = reloadsAfter(3);

S.TestPlayOverAFailedLastTrackStopsAtTheEnd = async () => {
  // The stop is the reader's Play's, seen before the skip's timer fires.
  const { r, t } = await failing({ tracks: [flac(1)] });
  r.el.fail(4);
  await settle();
  const plays = r.el.plays.length;
  const posts = r.s.posts(ROUTES.switchOut).length;
  check("the failed last track shown stopped, its skip scheduled, its element not paused", !r.audio.snapshot().playing && t.count(1200) === 1 && !r.el.paused,
    [r.audio.snapshot().playing, t.count(1200), r.el.paused]);
  r.audio.toggle();
  await settle();
  check("Play over it, repeat off, before its skip: through a barrier it stops at the end and plays nothing",
    r.s.posts(ROUTES.switchOut).length === posts + 1 && r.el.paused && !r.audio.snapshot().playing && r.el.plays.length === plays && r.audio.snapshot().index === 0,
    [r.s.posts(ROUTES.switchOut).length - posts, r.el.paused, r.audio.snapshot().playing, r.el.plays.slice(plays), r.audio.snapshot().index]);
  t.fire(1200);
  await settle();
  check("its skip then does nothing more", r.s.posts(ROUTES.switchOut).length === posts + 1 && r.el.plays.length === plays && r.el.paused,
    [r.s.posts(ROUTES.switchOut).length - posts, r.el.plays.slice(plays), r.el.paused]);
};

S.TestTheLockScreensPauseOverAFailedTrackStaysAPause = async () => {
  const { r, held } = await skipWaiting();
  const plays = r.el.plays.length;
  check("the lock screen's Pause is set", typeof session.handlers.pause === "function", typeof session.handlers.pause);
  session.handlers.pause();
  await settle();
  held.open();
  await settle();
  check("the lock screen's Pause while the skip waits: nothing plays, nothing moves", r.el.plays.length === plays && r.audio.snapshot().index === 0 && r.el.paused && !r.audio.snapshot().playing,
    [r.el.plays.slice(plays), r.audio.snapshot().index, r.el.paused]);
};

S.TestAPlayBeforeAHandoversMetadataIsItsOnlyStart = async () => {
  const r = await pageRig({ gate: true, noFrame: true });
  r.s.on("GET", ROUTES.now, async () => ({ status: 409, body: { error: "player_not_offered", message: "no player" } }));
  await r.out.sampleNow();
  await settle();
  r.audio.take({ queue: [flac(1), flac(2)], index: 0, position: 30, playing: true }, true, null);
  await settle();
  r.audio.toggle();
  await settle();
  check("the reader's Play before the metadata plays", !r.el.paused && r.el.plays.length === 1, [r.el.paused, r.el.plays]);
  r.el.metadata(200);
  await settle();
  check("the metadata seeks and starts nothing more", r.el.plays.length === 1 && !r.el.paused && r.el.currentTime === 30 && r.audio.snapshot().playing,
    [r.el.plays, r.el.paused, r.el.currentTime, r.audio.snapshot().playing]);
};

S.TestAPauseBeforeAHandoversMetadataCancelsItsDeferredStart = async () => {
  const r = await handedOver(handoverAt30);
  check("the handover taken, its play deferred to the metadata", r.el.paused && !!r.el.getAttribute("src"), [r.el.paused, r.el.getAttribute("src")]);
  r.audio.toggle();
  await settle();
  check("the reader's Play plays, on a barrier", !r.el.paused && r.n === 2, [r.el.paused, r.n]);
  r.audio.toggle();
  await settle();
  const plays = r.el.plays.length;
  r.el.metadata(200);
  await settle();
  check("the reader's Pause before the metadata: the deferred start plays nothing", r.el.paused && r.el.plays.length === plays && !r.audio.snapshot().playing,
    [r.el.paused, r.el.plays.slice(plays), r.audio.snapshot().playing]);
};

S.TestAHandoverWhoseSelectedEntryIsGoneStartsTheNextKeptEntry = async () => {
  const r = await handedOver({ tracks: [{ id: "a", path: "Album/01.flac", track: flac(1) }, { id: "x", path: "Gone/09.flac" }, { id: "c", path: "Album/03.flac", track: flac(3) }], selected: 1, position: 120, playing: true });
  const s = r.audio.snapshot();
  check("the next kept entry is selected", s.track?.path === "Album/03.flac" && s.queue.length === 2, [s.track?.path, s.queue.length]);
  r.el.metadata(200);
  await settle();
  check("and plays as the handover did, from its start", !r.el.paused && /03\.flac/.test(r.el.plays.at(-1) || "") && r.el.currentTime === 0, [r.el.paused, r.el.plays, r.el.currentTime]);
};

S.TestAHandoverWhoseSelectedLastEntryIsGoneIsTakenStopped = async () => {
  const r = await handedOver({ tracks: [{ id: "a", path: "Album/01.flac", track: flac(1) }, { id: "x", path: "Gone/09.flac" }], selected: 1, position: 120, playing: true });
  check("with no kept entry after it, the first kept entry is selected", r.audio.snapshot().track?.path === "Album/01.flac", r.audio.snapshot().track?.path);
  r.el.metadata(200);
  await settle();
  check("taken stopped, from its start", r.el.paused && r.el.plays.length === 0 && r.el.currentTime === 0 && !r.audio.snapshot().playing,
    [r.el.paused, r.el.plays, r.el.currentTime, r.audio.snapshot().playing]);
};

S.TestATrackThatFailsForGoodPlaysNothingWhenItsBarrierAnswers = async () => {
  const r = await pageRig({ gate: true, noFrame: true });
  r.s.on("GET", ROUTES.now, async () => ({ status: 409, body: { error: "player_not_offered", message: "no player" } }));
  await r.out.sampleNow();
  await settle();
  r.audio.playQueue([flac(1), flac(2)], 0);
  await settle();
  r.el.metadata(200);
  // The bridge's player comes up, silent: a retry of what started with no
  // player offered asks a barrier, held here.
  const held = gate();
  r.s.on("GET", ROUTES.now, async () => ({ body: pub({ epoch: E2, revision: 1, silent: true }), headers: { [CLOCK_HEADER]: new Date(r.c.now()).toISOString() } }));
  r.s.on("POST", ROUTES.switchOut, async (e) => { await held.p; return answer(recOf(e, { kind: "switch-out", silence: 2 }), pub({ epoch: E2, revision: 2, silent: true })); });
  r.out.streamOpen(1);
  r.frame(pub({ epoch: E2, revision: 1, silent: true }));
  await settle();
  r.el.fail(2);
  await settle();
  check("a network error's retry waits on a barrier", r.s.posts(ROUTES.switchOut).length === 1 && r.el.paused, [r.s.posts(ROUTES.switchOut).length, r.el.paused]);
  r.el.fail(2);
  await settle();
  const plays = r.el.plays.length;
  check("a second one meanwhile: it failed, said", r.audio.snapshot().error === 'Playback failed for "T1".', r.audio.snapshot().error);
  held.open();
  await settle();
  check("the barrier's done then plays nothing over the failed track", r.el.paused && r.el.plays.length === plays && !r.audio.snapshot().loading && !r.audio.snapshot().playing,
    [r.el.paused, r.el.plays.slice(plays), r.audio.snapshot().loading]);
};

S.TestAContinuationBehindABarrierLeavesATakenQueueAlone = async () => {
  // A queue output.js hands this browser while a skip waits on its barrier:
  // the skip, let go on, asks again whether the playback it continues is
  // still the one this browser plays, and it is not.
  const { r, held } = await skipWaiting();
  r.audio.take({ queue: [flac(7), flac(8)], index: 0, position: 0, playing: true }, true, () => true);
  await settle();
  check("the taken queue plays from its start while the skip waits", r.audio.snapshot().track?.path === "Album/07.flac" && /07\.flac/.test(r.el.plays.at(-1) || ""),
    [r.audio.snapshot().track?.path, r.el.plays]);
  held.open();
  await settle();
  const s = r.audio.snapshot();
  check("the skip's barrier answers done and moves nothing of it", s.index === 0 && s.track?.path === "Album/07.flac" && !r.el.plays.some((src) => /08\.flac/.test(src)),
    [s.index, s.track?.path, r.el.plays]);
};

S.TestAnUnwiredGateLetsNoTrackGoOnByItself = async () => {
  const r = await unwiredPage();
  r.audio.take({ queue: [flac(1), flac(2)], index: 0, position: 0, playing: true }, true);
  await settle();
  check("a queue taken to play", !r.el.paused, r.el.paused);
  const plays = r.el.plays.length;
  r.el.end();
  await settle();
  check("its end with no gate wired: no next track goes on", r.el.paused && r.audio.snapshot().index === 0 && r.el.plays.length === plays && !r.audio.snapshot().playing,
    [r.el.paused, r.audio.snapshot().index, r.el.plays.slice(plays)]);
};

S.TestTheOldestMarkGoesToTheFirstPlayEventAndNothingUnaskedSounds = async () => {
  // No player offered, the gate counting what it is asked. The browser's own
  // play is queued, then the reader's Pause and Play before any of the
  // element's events: the browser's event takes the reader's mark, so the
  // reader's own event is paused and asked as the browser's; nothing sounds
  // that the gate did not let.
  const r = await pageRig({ unwired: true, noFrame: true });
  let asks = 0;
  r.audio.setStartGate((run) => run(null));
  r.audio.setContinueGate((permit, run) => { asks += 1; run(permit); });
  r.audio.playQueue([flac(1), flac(2)], 0);
  await settle();
  r.el.metadata(200);
  r.el.pause();
  await settle();
  asks = 0;
  const plays = r.el.plays.length;
  r.el.browserPlay();
  r.audio.toggle();
  r.audio.toggle();
  await settle();
  check("playing, through one ask of the gate and one play of the bar's after the reader's", !r.el.paused && r.audio.snapshot().playing && asks === 1 && r.el.plays.length === plays + 3,
    [r.el.paused, r.audio.snapshot().playing, asks, r.el.plays.slice(plays)]);
  r.el.end();
  await settle();
  check("and the track's end plays the next one", !r.el.paused && r.audio.snapshot().index === 1, [r.el.paused, r.audio.snapshot().index]);
};

S.TestTheHarnessElementSettlesWhatALoadDrops = async () => {
  page();
  const el = document.createElement("audio");
  const outcomeOf = async (p) => {
    let out = "pending";
    p.then(() => { out = "resolved"; }, (e) => { out = e?.name || "rejected"; });
    await settle();
    return out;
  };
  const resolved = el.play();
  el.load();
  check("a play whose settlement a load overtakes resolves", (await outcomeOf(resolved)) === "resolved");
  el.playMode = "pend";
  const refused = el.play();
  el.pause();
  el.load();
  check("a play a pause took, the pause overtaken by a load, is refused with AbortError", (await outcomeOf(refused)) === "AbortError");
};

S.TestTheOutputMenuClosesAsTheFocusLeavesIt = async () => {
  store.set(OUTPUT_KEY, "bridge");
  const r = await barPage({ latest: playing({ revision: 3 }) });
  r.out.subscribe(() => {});
  await settle();
  const menu = findAll(r.body, (n) => n.attrs?.id === "np-output-menu")[0];
  const btn = r.find("Output: the bridge's player");
  const [browserPick, bridgePick] = findAll(r.body, (n) => n.tagName === "BUTTON" && n.classList.contains("np-pick"));
  const outside = r.find("Queue");
  const open = () => !menu.hidden && btn.attrs["aria-expanded"] === "true";
  const closed = () => menu.hidden && btn.attrs["aria-expanded"] === "false";
  btn.click();
  check("the menu opens", open(), [menu.hidden, btn.attrs["aria-expanded"]]);
  menu.fire("focusout", { target: bridgePick, relatedTarget: browserPick });
  check("moving between its picks leaves it open", open(), menu.hidden);
  menu.fire("focusout", { target: browserPick, relatedTarget: outside });
  check("Tab past its last pick closes it", closed(), [menu.hidden, btn.attrs["aria-expanded"]]);
  btn.click();
  menu.fire("focusout", { target: bridgePick, relatedTarget: btn });
  check("Shift+Tab back to the Output button closes it", closed(), [menu.hidden, btn.attrs["aria-expanded"]]);
  btn.click();
  let prevented = false;
  btn.fire("mousedown", { preventDefault() { prevented = true; } });
  // A browser moves the focus to the button it presses, unless the press is
  // prevented.
  if (!prevented) menu.fire("focusout", { target: bridgePick, relatedTarget: btn });
  btn.click();
  check("a click on the Output button while the menu is open closes it once", prevented && closed(), [prevented, menu.hidden, btn.attrs["aria-expanded"]]);
};

S.TestTheOutputButtonClaimsNoPopup = async () => {
  const r = await barPage({ latest: pub({ revision: 3, silent: true }) });
  r.out.subscribe(() => {});
  await settle();
  const btn = r.find("Output: this browser");
  check("the Output button found", !!btn);
  check("it claims no popup: its menu is a group of buttons", btn && !("aria-haspopup" in btn.attrs), btn?.attrs);
};

// ---- An explicit Pause, a handover's wait, the failure streak and an abort ----

S.TestAnExplicitPauseCancelsAStartWaitingForTheEpoch = async () => {
  // No frame yet: a row's start waits for the epoch. The lock screen's Pause
  // lets it go, so the first frame sends no barrier, which would pause the
  // bridge's engine, and plays nothing.
  const r = await pageRig({ gate: true, noFrame: true, saved: { queue: [flac(1), flac(2)], index: 0, at: 0, shuffle: false, repeat: "off" }, latest: pub({ revision: 2, silent: true }) });
  r.s.on("POST", ROUTES.switchOut, async (e) => answer(recOf(e, { kind: "switch-out", silence: 3 }), pub({ revision: 3, silent: true })));
  r.audio.playQueue([flac(3)], 0);
  await settle();
  check("the row's start waits for the epoch, said, nothing sent", r.out.view().line === WORDS.startWaits && r.s.posts().length === 0, [r.out.view().line, r.s.posts()]);
  check("the lock screen's Pause is set", typeof session.handlers.pause === "function", typeof session.handlers.pause);
  session.handlers.pause();
  await settle();
  r.out.streamOpen(1);
  r.frame(pub({ revision: 2, silent: true }));
  await settle();
  const s = r.audio.snapshot();
  check("the first frame then sends no barrier and nothing plays", r.s.posts(ROUTES.switchOut).length === 0 && r.el.plays.length === 0 && r.el.paused,
    [r.s.posts(ROUTES.switchOut).length, r.el.plays, r.el.paused]);
  check("the selection the browser held stays", s.queue.length === 2 && s.track?.path === "Album/01.flac" && s.index === 0, [s.queue.length, s.track?.path, s.index]);
  r.c.advance(START_WAIT_MS);
  r.out.tick();
  await settle();
  check("no wait is left to time out", r.out.view().line !== WORDS.startWaits && r.out.view().line !== WORDS.startUnanswered, r.out.view().line);
  r.audio.toggle();
  await settle();
  check("a fresh Play then plays, through a barrier of its own", r.s.posts(ROUTES.switchOut).length === 1 && !r.el.paused && r.el.plays.length === 1 && /01\.flac/.test(r.el.plays[0] || ""),
    [r.s.posts(ROUTES.switchOut).length, r.el.paused, r.el.plays]);
};

S.TestAnExplicitPauseCancelsAStartWaitingOnItsBarrier = async () => {
  // A row clicked beside a silent bridge waits on its barrier; the lock
  // screen's Pause, and then the bar's Pause on a second row, let it go: the
  // barrier's answer plays nothing, the selection stays, and Play still plays.
  const r = await gatedPlaying({ bar: true });
  for (const [how, pause] of [["the lock screen's Pause", () => session.handlers.pause()], ["the bar's Pause", () => r.find("Pause").click()]]) {
    const held = gate();
    r.hold = held;
    const posts = r.s.posts(ROUTES.switchOut).length;
    r.audio.playQueue([flac(7), flac(8)], 0);
    await settle();
    check(`${how}: the row waits on its barrier`, r.s.posts(ROUTES.switchOut).length === posts + 1 && !r.el.paused, [r.s.posts(ROUTES.switchOut).length - posts, r.el.paused]);
    const plays = r.el.plays.length;
    pause();
    await settle();
    held.open();
    await settle();
    let s = r.audio.snapshot();
    check(`${how}: the barrier's answer plays nothing, the selection stays`,
      r.el.plays.length === plays && r.el.paused && !s.playing && s.queue.length === 3 && s.track?.path === "Album/01.flac",
      [r.el.plays.slice(plays), r.el.paused, s.playing, s.queue.length, s.track?.path]);
    r.find("Play").click();
    await settle();
    s = r.audio.snapshot();
    check(`${how}: a fresh Play then plays what the browser held`, !r.el.paused && r.el.plays.length === plays + 1 && /01\.flac/.test(r.el.plays.at(-1) || "") && s.track?.path === "Album/01.flac",
      [r.el.paused, r.el.plays.slice(plays), s.track?.path]);
  }
};

S.TestAnExplicitPauseCancelsAPlayThatTookASkipsPlace = async () => {
  // Play over a failed track takes the skip's place behind its barrier; the
  // lock screen's Pause then lets it go, and the next track never plays.
  const { r, held } = await skipWaiting();
  r.audio.toggle();
  await settle();
  const plays = r.el.plays.length;
  session.handlers.pause();
  await settle();
  held.open();
  await settle();
  check("the barrier's answer plays nothing and moves nothing", r.el.plays.length === plays && r.audio.snapshot().index === 0 && r.el.paused && !r.audio.snapshot().playing,
    [r.el.plays.slice(plays), r.audio.snapshot().index, r.el.paused]);
  r.audio.toggle();
  await settle();
  check("a fresh Play then plays the next playable track", r.audio.snapshot().index === 1 && /02\.flac/.test(r.el.plays.at(-1) || "") && !r.el.paused,
    [r.audio.snapshot().index, r.el.plays.slice(plays), r.el.paused]);
};

// heldBarrier answers the next barriers when the case lets held go: done at
// revision 20 beside a silent bridge, or refused under the DSD check.
function heldBarrier(r, held, { refuse = false } = {}) {
  r.s.on("POST", ROUTES.switchOut, async (e) => {
    await held.p;
    return refuse
      ? answer(recOf(e, { kind: "switch-out", status: "refused", error: "check", effect: "none", last: "maintenance" }), pub({ revision: 20, silent: true, hold: "check" }))
      : answer(recOf(e, { kind: "switch-out", silence: 20 }), pub({ revision: 20, silent: true }));
  });
}

S.TestAPauseDuringAHandoversWaitStopsItsSpinner = async () => {
  // A playing handover at 30 s waits for its metadata, shown starting; the
  // lock screen's Pause shows it stopped at once, and its metadata then
  // plays nothing.
  const r = await handedOver(handoverAt30);
  let shown = null;
  r.audio.subscribe((s) => { shown = s; });
  check("the handover waits for its metadata, shown starting", r.el.paused && shown?.loading === true, [r.el.paused, shown?.loading]);
  session.handlers.pause();
  check("the lock screen's Pause shows it stopped at once", shown && !shown.playing && !shown.loading && !r.audio.snapshot().loading, [shown?.playing, shown?.loading]);
  await settle();
  r.el.metadata(200);
  await settle();
  check("its metadata then plays nothing, and it stays shown stopped", r.el.plays.length === 0 && r.el.paused && !shown.playing && !shown.loading,
    [r.el.plays, r.el.paused, shown.playing, shown.loading]);
};

S.TestAReaderPlaySupersedesAHandoversDeferredStart = async () => {
  // The reader's Play during a positioned handover's wait, its barrier held:
  // the metadata seeks and plays nothing, and the Play plays once admitted.
  const r = await handedOver(handoverAt30);
  const held = gate();
  heldBarrier(r, held);
  r.audio.toggle();
  await settle();
  r.el.metadata(200);
  await settle();
  check("the metadata first: the seek to 30 s and no play before the Play is admitted", r.el.currentTime === 30 && r.el.plays.length === 0 && r.el.paused,
    [r.el.currentTime, r.el.plays, r.el.paused]);
  held.open();
  await settle();
  check("admitted, the Play plays once, at 30 s", r.el.plays.length === 1 && !r.el.paused && r.el.currentTime === 30 && r.audio.snapshot().playing,
    [r.el.plays, r.el.paused, r.el.currentTime, r.audio.snapshot().playing]);
};

S.TestARefusedReaderPlayDuringAHandoversWaitShowsItStopped = async () => {
  // The same Play, its barrier refused: the seek stays, nothing plays, and
  // the bar shows the handover stopped, its spinner gone.
  const r = await handedOver(handoverAt30);
  const held = gate();
  heldBarrier(r, held, { refuse: true });
  r.audio.toggle();
  await settle();
  r.el.metadata(200);
  await settle();
  held.open();
  await settle();
  const s = r.audio.snapshot();
  check("refused: the seek to 30 s, no play, shown stopped and not starting", r.el.currentTime === 30 && r.el.plays.length === 0 && r.el.paused && !s.playing && !s.loading,
    [r.el.currentTime, r.el.plays, r.el.paused, s.playing, s.loading]);
};

// failsAndSkips is the current track failing, its source refused, and its
// skip fired, through a barrier of its own.
async function failsAndSkips(r, t) {
  r.el.fail(4);
  await settle();
  t.fire(1200);
  await settle();
}

S.TestATrackThatPlaysToItsEndBreaksTheFailureStreak = async () => {
  // Bad, good, bad, good, bad, good: each good track plays to its end, so no
  // three failures come in a row and the album reaches its sixth track.
  const { r, t } = await failing({ pend: true, tracks: [flac(1), flac(2), flac(3), flac(4), flac(5), flac(6)] });
  for (let i = 0; i < 3; i++) {
    await failsAndSkips(r, t);
    r.el.metadata(200);
    await settle();
    if (i < 2) {
      r.el.end();
      await settle();
    }
  }
  const s = r.audio.snapshot();
  check("the sixth track plays, no stop said", s.index === 5 && /06\.flac/.test(r.el.plays.at(-1) || "") && s.playing && !/next tracks/.test(s.error),
    [s.index, r.el.plays.at(-1), s.playing, s.error]);
};

S.TestThreeFailuresInARowStopOnTheThirdThoughEachHadItsPlayEvent = async () => {
  // Each unplayable track's play event comes before its error: that play
  // breaks no streak, and the third in a row stops, said.
  const { r, t } = await failing({ pend: true, tracks: [flac(1), flac(2), flac(3), flac(4), flac(5)] });
  let played = 0;
  for (let i = 0; i < 3; i++) {
    if (r.audio.snapshot().playing) played += 1;
    r.el.fail(4);
    await settle();
    if (i < 2) {
      t.fire(1200);
      await settle();
    }
  }
  const s = r.audio.snapshot();
  check("each of the three had its play event before its error", played === 3, played);
  check("the third in a row stops, said", s.index === 2 && !s.playing && t.count(1200) === 0 && /can't play in this browser/.test(s.error),
    [s.index, s.playing, t.count(1200), s.error]);
};

S.TestAFailureAfterARepeatOneReplayStartsANewStreak = async () => {
  // Repeat one: two tracks fail, the third plays to its end and is replayed,
  // and its replay's failure is the first of a new streak, its skip scheduled.
  const { r, t } = await failing({ pend: true, tracks: [flac(1), flac(2), flac(3), flac(4)] });
  r.audio.cycleRepeat();
  r.audio.cycleRepeat();
  await failsAndSkips(r, t);
  await failsAndSkips(r, t);
  check("two failures skip to the third track", r.audio.snapshot().index === 2 && /03\.flac/.test(r.el.plays.at(-1) || ""), [r.audio.snapshot().index, r.el.plays]);
  r.el.metadata(200);
  await settle();
  const plays = r.el.plays.length;
  r.el.end();
  await settle();
  check("it plays to its end and is replayed, from its start", r.el.plays.length === plays + 1 && /03\.flac/.test(r.el.plays.at(-1) || "") && r.el.currentTime === 0,
    [r.el.plays.slice(plays), r.el.currentTime]);
  r.el.fail(4);
  await settle();
  const s = r.audio.snapshot();
  check("the replay's failure is the first of a new streak: its skip scheduled, no stop said", t.count(1200) === 1 && s.error === `Can't play "T3" in this browser.`,
    [t.count(1200), s.error]);
};

S.TestAfterAnAbortTheButtonDoesWhatItShows = async () => {
  // A fetch the user aborted after playback began (code 1) fires abort and
  // no pause, so the bar still shows Pause: pressed, it pauses and asks the
  // gate nothing; stopped, it shows Play, which starts through the gate.
  const r = await gatedPlaying({ bar: true });
  let asks = 0;
  r.audio.setStartGate((run, o) => { asks += 1; r.out.gateStart(run, o); }, r.out.cancelStarts);
  r.el.error = { code: 1 };
  r.el.fire("abort");
  await settle();
  const pause = r.find("Pause");
  check("after the abort the bar still shows Pause over a playing element", !!pause && !r.el.paused, [!!pause, r.el.paused]);
  const posts = r.s.posts(ROUTES.switchOut).length;
  const plays = r.el.plays.length;
  pause?.click();
  await settle();
  check("pressed, it pauses and asks the gate nothing", r.el.paused && asks === 0 && r.s.posts(ROUTES.switchOut).length === posts && r.el.plays.length === plays,
    [r.el.paused, asks, r.s.posts(ROUTES.switchOut).length - posts, r.el.plays.slice(plays)]);
  const play = r.find("Play");
  check("stopped, it shows Play", !!play && !r.audio.snapshot().playing, [!!play, r.audio.snapshot().playing]);
  const [asked, sent, played] = [asks, r.s.posts(ROUTES.switchOut).length, r.el.plays.length];
  play?.click();
  await settle();
  check("and Play starts through the gate, a barrier of its own, the track it showed", asks === asked + 1 && r.s.posts(ROUTES.switchOut).length === sent + 1 && !r.el.paused && r.el.plays.length === played + 1 &&
    r.audio.snapshot().index === 0 && /01\.flac/.test(r.el.plays.at(-1) || ""),
    [asks - asked, r.s.posts(ROUTES.switchOut).length - sent, r.el.paused, r.el.plays.slice(played), r.audio.snapshot().index]);
};

S.TestTheBrowsersOwnPlayTakesTheSlotAPauseLetGo = async () => {
  // A row's start behind a held barrier, let go by the lock screen's Pause:
  // the browser's own play then takes that barrier, as it would take no
  // barrier at all, and resumes on its answer what the browser held.
  const r = await gatedPlaying();
  const held = gate();
  r.hold = held;
  r.audio.playQueue([flac(7), flac(8)], 0);
  await settle();
  session.handlers.pause();
  await settle();
  const plays = r.el.plays.length;
  r.el.browserPlay();
  await settle();
  check("the browser's own play waits on the barrier in flight, shown starting, nothing more sent",
    r.el.paused && r.audio.snapshot().loading && r.s.posts(ROUTES.switchOut).length === 2, [r.el.paused, r.audio.snapshot().loading, r.s.posts(ROUTES.switchOut).length]);
  held.open();
  await settle();
  check("and resumes on its answer what the browser held", !r.el.paused && r.audio.snapshot().track?.path === "Album/01.flac" && r.el.plays.length === plays + 2,
    [r.el.paused, r.audio.snapshot().track?.path, r.el.plays.slice(plays)]);
};

S.TestABarrierWhoseStartAPauseLetGoEndsQuietly = async () => {
  // A row's barrier never answers; the lock screen's Pause lets its start go,
  // and at 30 s the barrier is lost and read back done: the bar comes back to
  // this browser's phase saying nothing of a start, and Play still plays.
  const r = await gatedPlaying();
  r.s.on("GET", ROUTES.operation, async (e) => readBackOf(e, { silence: 9 }));
  r.hold = gate();
  r.audio.playQueue([flac(7), flac(8)], 0);
  await settle();
  const plays = r.el.plays.length;
  session.handlers.pause();
  await settle();
  r.c.advance(REQUEST_MS);
  r.out.tick();
  await settle();
  const v = r.out.view();
  check("lost and read back done: this browser's phase, no line of a start, nothing played",
    r.s.gets(ROUTES.operation).length >= 1 && v.phase === PHASE.browser && v.line === "" && r.el.plays.length === plays && r.audio.snapshot().track?.path === "Album/01.flac",
    [r.s.gets(ROUTES.operation).length, v.phase, v.line, r.el.plays.slice(plays), r.audio.snapshot().track?.path]);
  r.audio.toggle();
  await settle();
  check("Play then plays through a barrier of its own", !r.el.paused && r.el.plays.length === plays + 1 && r.s.posts(ROUTES.switchOut).length === 3,
    [r.el.paused, r.el.plays.slice(plays), r.s.posts(ROUTES.switchOut).length]);
};

S.TestAPlayAfterAnExplicitPauseOutlivesThatPausesEvent = async () => {
  // The bar's Pause lets the row's start go and queues the element's pause
  // event; a fresh Play before that event takes the barrier in flight; the
  // event, which the element's own pause fires too, cancels nothing of it.
  const r = await gatedPlaying();
  const held = gate();
  r.hold = held;
  r.audio.playQueue([flac(7), flac(8)], 0);
  await settle();
  const plays = r.el.plays.length;
  r.audio.toggle();
  r.audio.toggle();
  await settle();
  check("the fresh Play waits on the barrier in flight, nothing more sent", r.s.posts(ROUTES.switchOut).length === 2 && r.el.paused, [r.s.posts(ROUTES.switchOut).length, r.el.paused]);
  held.open();
  await settle();
  const s = r.audio.snapshot();
  check("the barrier's answer plays the fresh Play, what the browser held, never the row",
    !r.el.paused && r.el.plays.length === plays + 1 && /01\.flac/.test(r.el.plays.at(-1) || "") && s.track?.path === "Album/01.flac" && s.queue.length === 3,
    [r.el.paused, r.el.plays.slice(plays), s.track?.path, s.queue.length]);
  r.el.end();
  await settle();
  check("and what it means to play goes on at the track's end", !r.el.paused && r.audio.snapshot().index === 1, [r.el.paused, r.audio.snapshot().index]);
};

// ---- A resume a pause overtook, the bar's Pause shown at once, and a Stop ----

// overtakenResume is track 1 paused by the reader while a row's start waits
// on its barrier, held; the browser's own resume of track 1 queues its play
// event, and the lock screen's Pause comes before that event, so the event
// runs, the barrier answers as it ends (reply), and the Pause's own pause
// event comes last. plays counts the plays before the browser's own.
async function overtakenResume(reply) {
  const r = await gatedPlaying();
  r.audio.toggle();
  await settle();
  const held = gate();
  r.s.on("POST", ROUTES.switchOut, async (e) => {
    await held.p;
    return reply(e);
  });
  r.audio.playQueue([flac(7), flac(8)], 0);
  await settle();
  check("track 1 paused, the row's start waits on its barrier", r.el.paused && r.audio.snapshot().track?.path === "Album/01.flac" && r.s.posts(ROUTES.switchOut).length === 2,
    [r.el.paused, r.audio.snapshot().track?.path, r.s.posts(ROUTES.switchOut).length]);
  const plays = r.el.plays.length;
  r.el.browserPlay();
  session.handlers.pause();
  r.el.addEventListener("play", function once() {
    r.el.removeEventListener("play", once);
    held.open();
  });
  await settle();
  return { r, plays };
}

// overtakenResumeEnds checks what that resume leaves: track 1 not played
// again, the row the Pause let go of not played, and nothing played after
// the browser's own, track 1 selected, paused and shown stopped.
function overtakenResumeEnds(r, plays, how) {
  const after = r.el.plays.slice(plays + 1);
  const s = r.audio.snapshot();
  check(`${how}: the paused track is not played again`, !after.some((src) => /01\.flac/.test(src)), after);
  check(`${how}: the row the Pause let go of is not played`, !after.some((src) => /07\.flac/.test(src)), after);
  check(`${how}: nothing plays after the browser's own, track 1 selected, paused and shown stopped`,
    r.el.plays.length === plays + 1 && r.el.paused && s.index === 0 && s.track?.path === "Album/01.flac" && !s.playing && !s.loading,
    [r.el.plays.slice(plays), r.el.paused, s.index, s.track?.path, s.playing, s.loading]);
}

S.TestTheBrowsersOwnResumeAPauseOvertookPlaysNothingOnItsBarriersDone = async () => {
  const { r, plays } = await overtakenResume((e) => answer(recOf(e, { kind: "switch-out", silence: 20 }), pub({ revision: 20, silent: true })));
  overtakenResumeEnds(r, plays, "the barrier answered done beside a silent bridge");
};

S.TestTheBrowsersOwnResumeAPauseOvertookPlaysNothingOnANoPlayerRefusal = async () => {
  const { r, plays } = await overtakenResume(() => refusal("player_not_offered", null, 409, "this bridge offers no player of its own"));
  check("the barrier refused as no player offered", r.out.view().notOffered, r.out.view().notOffered);
  overtakenResumeEnds(r, plays, "the barrier refused as no player offered");
};

S.TestTheBrowsersOwnResumeAPauseOvertookAsksNothingWhereNoPlayerIsOffered = async () => {
  // No player offered, so the gate lets a start go on at once: the browser's
  // own resume of the paused track, its play event queued before the lock
  // screen's Pause, asks nothing when that event comes; a resume of the
  // browser's own made after the Pause still plays.
  const r = await pageRig({ gate: true, noFrame: true });
  r.s.on("GET", ROUTES.now, async () => ({ status: 409, body: { error: "player_not_offered", message: "no player" } }));
  await r.out.sampleNow();
  await settle();
  r.audio.playQueue([flac(1), flac(2)], 0);
  await settle();
  r.el.metadata(200);
  r.audio.toggle();
  await settle();
  check("no player offered, track 1 paused", r.out.view().notOffered && r.el.paused && r.audio.snapshot().track?.path === "Album/01.flac",
    [r.out.view().notOffered, r.el.paused, r.audio.snapshot().track?.path]);
  const plays = r.el.plays.length;
  r.el.browserPlay();
  session.handlers.pause();
  await settle();
  const s = r.audio.snapshot();
  check("the resume the Pause overtook asks nothing: no play after the browser's own, paused and shown stopped",
    r.el.plays.length === plays + 1 && r.el.paused && !s.playing && !s.loading && s.track?.path === "Album/01.flac",
    [r.el.plays.slice(plays), r.el.paused, s.playing, s.loading, s.track?.path]);
  r.el.browserPlay();
  await settle();
  check("a resume of the browser's own made after the Pause still plays", !r.el.paused && r.audio.snapshot().playing && r.el.plays.length === plays + 3 && /01\.flac/.test(r.el.plays.at(-1) || ""),
    [r.el.paused, r.audio.snapshot().playing, r.el.plays.slice(plays)]);
};

S.TestAfterAnAbortThePausePressedShowsPlayAtOnce = async () => {
  // A fetch the user aborted (code 1) fires abort and no pause. The bar's
  // Pause pressed pauses and shows Play at once, before the element's pause
  // event; that Play pressed before the event asks the gate once and plays
  // the track the bar showed, and what it means to play goes on at its end.
  const r = await gatedPlaying({ bar: true });
  let asks = 0;
  r.audio.setStartGate((run, o) => { asks += 1; r.out.gateStart(run, o); }, r.out.cancelStarts);
  r.el.error = { code: 1 };
  r.el.fire("abort");
  await settle();
  const posts = r.s.posts(ROUTES.switchOut).length;
  const plays = r.el.plays.length;
  r.find("Pause")?.click();
  const shown = r.find("Play") ? "Play" : (r.find("Pause") ? "Pause" : "");
  check("pressed, the Pause pauses and shows Play at once, before its pause event, asking the gate nothing",
    shown === "Play" && r.el.paused && asks === 0 && r.s.posts(ROUTES.switchOut).length === posts, [shown, r.el.paused, asks, r.s.posts(ROUTES.switchOut).length - posts]);
  r.find(shown)?.click();
  check("the control shown, pressed before that event, does what it shows: a Pause asks nothing, a Play one start",
    (shown === "Pause" && asks === 0) || (shown === "Play" && asks === 1), [shown, asks]);
  await settle();
  const s = r.audio.snapshot();
  check("one start, through a barrier of its own, plays the track the bar showed",
    asks === 1 && r.s.posts(ROUTES.switchOut).length === posts + 1 && !r.el.paused && r.el.plays.length === plays + 1 && /01\.flac/.test(r.el.plays.at(-1) || "") && s.index === 0 && s.track?.path === "Album/01.flac",
    [asks, r.s.posts(ROUTES.switchOut).length - posts, r.el.paused, r.el.plays.slice(plays), s.index, s.track?.path]);
  r.el.end();
  await settle();
  check("and what it means to play goes on at the track's end", !r.el.paused && r.audio.snapshot().index === 1 && /02\.flac/.test(r.el.plays.at(-1) || ""),
    [r.el.paused, r.audio.snapshot().index, r.el.plays.slice(plays)]);
};

S.TestAStopLetsGoOfAStartWaitingForTheEpoch = async () => {
  // No frame yet: a row's start waits for the epoch. The lock screen's Stop
  // clears the queue and lets the start go: the first frame sends no barrier
  // and plays nothing, and the queue stays empty. Play on it does nothing, and
  // a row played after it plays.
  const r = await pageRig({ gate: true, noFrame: true, saved: { queue: [flac(1), flac(2)], index: 0, at: 0, shuffle: false, repeat: "off" }, latest: pub({ revision: 2, silent: true }) });
  r.s.on("POST", ROUTES.switchOut, async (e) => answer(recOf(e, { kind: "switch-out", silence: 3 }), pub({ revision: 3, silent: true })));
  r.audio.playQueue([flac(7), flac(8)], 0);
  await settle();
  check("the row's start waits for the epoch, said, nothing sent", r.out.view().line === WORDS.startWaits && r.s.posts().length === 0, [r.out.view().line, r.s.posts()]);
  check("the lock screen's Stop is set", typeof session.handlers.stop === "function", typeof session.handlers.stop);
  session.handlers.stop();
  await settle();
  r.out.streamOpen(1);
  r.frame(pub({ revision: 2, silent: true }));
  await settle();
  let s = r.audio.snapshot();
  check("the first frame then sends no barrier and nothing plays, the queue empty",
    r.s.posts(ROUTES.switchOut).length === 0 && r.el.plays.length === 0 && r.el.paused && s.queue.length === 0 && s.index === -1 && !s.track,
    [r.s.posts(ROUTES.switchOut).length, r.el.plays, r.el.paused, s.queue.length, s.index]);
  r.c.advance(START_WAIT_MS);
  r.out.tick();
  await settle();
  check("no wait is left to time out", r.out.view().line !== WORDS.startWaits && r.out.view().line !== WORDS.startUnanswered, r.out.view().line);
  session.handlers.play();
  await settle();
  check("Play on the cleared queue does nothing", r.s.posts(ROUTES.switchOut).length === 0 && r.el.plays.length === 0 && r.audio.snapshot().index === -1,
    [r.s.posts(ROUTES.switchOut).length, r.el.plays, r.audio.snapshot().index]);
  r.audio.playQueue([flac(9)], 0);
  await settle();
  s = r.audio.snapshot();
  check("a row played after the Stop plays, through a barrier of its own",
    r.s.posts(ROUTES.switchOut).length === 1 && !r.el.paused && r.el.plays.length === 1 && /09\.flac/.test(r.el.plays[0] || "") && s.track?.path === "Album/09.flac",
    [r.s.posts(ROUTES.switchOut).length, r.el.paused, r.el.plays, s.track?.path]);
};

S.TestAStopLetsGoOfAStartWaitingOnItsBarrier = async () => {
  // A row clicked beside a silent bridge waits on its barrier, held; the lock
  // screen's Stop clears the queue and lets the start go: the barrier's answer
  // plays nothing and the queue stays empty. Play on it does nothing, and a
  // row played after it plays.
  const r = await gatedPlaying();
  const held = gate();
  r.hold = held;
  r.audio.playQueue([flac(7), flac(8)], 0);
  await settle();
  check("the row waits on its barrier", r.s.posts(ROUTES.switchOut).length === 2, r.s.posts(ROUTES.switchOut).length);
  const plays = r.el.plays.length;
  session.handlers.stop();
  await settle();
  held.open();
  await settle();
  let s = r.audio.snapshot();
  check("the barrier's answer plays nothing, the queue empty",
    r.el.plays.length === plays && r.el.paused && !s.playing && s.queue.length === 0 && s.index === -1 && !s.track,
    [r.el.plays.slice(plays), r.el.paused, s.playing, s.queue.length, s.index]);
  session.handlers.play();
  await settle();
  check("Play on the cleared queue does nothing", r.s.posts(ROUTES.switchOut).length === 2 && r.el.plays.length === plays && r.audio.snapshot().index === -1,
    [r.s.posts(ROUTES.switchOut).length, r.el.plays.slice(plays), r.audio.snapshot().index]);
  r.audio.playQueue([flac(9)], 0);
  await settle();
  s = r.audio.snapshot();
  check("a row played after the Stop plays, through a barrier of its own",
    r.s.posts(ROUTES.switchOut).length === 3 && !r.el.paused && r.el.plays.length === plays + 1 && /09\.flac/.test(r.el.plays.at(-1) || "") && s.track?.path === "Album/09.flac",
    [r.s.posts(ROUTES.switchOut).length, r.el.paused, r.el.plays.slice(plays), s.track?.path]);
};

// ---- A reader's action while a switch to this browser is outstanding ----
//
// The switch to this browser takes the bridge's queue and plays it only where
// the reader has done nothing to playback in this browser since it was sent:
// a Stop, a Pause or a row played meanwhile is what the browser keeps, and a
// row played in a document that reads an earlier one's switch back is too. A
// switch to the bridge refused, or refused with nothing changed, resumes
// nothing past the reader's Next. And a resume of the browser's own waiting on
// its barrier plays nothing a cancel or an error's retry would start.

// albums is the library the cases' album pages show, by id.
const albums = new Map();

// libraryRows renders the shipped views.js's album page over tracks, from the
// modules at base, and returns its rows' title buttons (the view on .view):
// the browser's row, which plays its track on the output chosen (playOn),
// where the bar's queue panel, hidden while this browser is chosen, drives the
// bridge. Its fetch answers the albums' details and nothing else, each answer
// parsed afresh, as the server's is: no two requests share a row.
async function libraryRows(tracks, { id = "a1", base = dirURL } = {}) {
  albums.set(id, tracks);
  globalThis.fetch = async (url) => {
    const m = /^\/api\/player\/albums\/([^?]+)/.exec(String(url));
    const t = m ? albums.get(decodeURIComponent(m[1])) : null;
    const body = t ? { album: { id: m[1], title: "Album", albumArtist: "Artist", artistId: "" }, tracks: t, variants: null, release: null } : {};
    return { ok: true, status: 200, json: async () => JSON.parse(JSON.stringify(body)) };
  };
  document.title = "Player";
  const views = await import(new URL("views.js", base));
  const view = document.createElement("main");
  await views.renderAlbum(view, { id, gen: () => 1, setToolbar: () => {}, setCrumb: () => {}, trail: [] });
  const rows = findAll(view, (n) => n.classList.contains("track-title"));
  check(`the album page lists its ${tracks.length} rows`, rows.length === tracks.length, rows.length);
  rows.view = view;
  return rows;
}

// theConsole is the console a switch to this browser meets, keeping its
// records as serve's control does: each tab's last operation alone, so that a
// read back of an older one answers operation_gone, and the eight most recent,
// which every publication it makes carries, their handovers left out (k.pub(o)
// is its latest publication, k.publish(st) a new one). routes(s) puts its
// routes on a connection, a reload's too. Its first barrier (k.setup) is lost
// while it runs, read back running at revision 38; the switch that takes the
// bridge's queue, admitted next, ends that barrier's unsent steps (cancelled)
// and runs to done, its silence 40, handing over 09.flac playing, which every
// publication from then on holds as a one-track queue, paused: its answer
// waits for k.held, or, with `lost`, is lost, and a read back of it waits for
// k.readBack. Each later barrier runs to done at a revision above the
// switch's: its answer waits for k.hold where set (once) and, with
// `lostAfter`, is lost, and a read back of it waits for k.backHold; with
// k.stuck (once) it is still running as its answer is lost, its admission
// published above the switch's silence too. A read back of a sequence in
// k.unreachable fails once, as a dropped connection does. k.revs records the
// revisions that history ran at.
function theConsole({ lost = false, lostAfter = false } = {}) {
  const k = { held: gate(), readBack: { p: Promise.resolve() }, backHold: { p: Promise.resolve() }, hold: null,
    unreachable: new Set(), setup: null, switched: null, revs: { running: 0, switched: 0, after: [], stuck: 0 } };
  const last = new Map();
  const recent = [];
  const keep = (e, rec) => {
    last.set(e.body.op.tab, rec);
    recent.push(rec);
    if (recent.length > 8) recent.shift();
    return rec;
  };
  const summaries = () => recent.map((r) => ({ ...r, handover: null }));
  const handover = { tracks: [{ id: "x9", path: "Album/09.flac", track: flac(9) }], selected: 0, position: 0, playing: true };
  let rev = 38;
  let queue = {};
  k.pub = (o = {}) => pub({ revision: rev, silent: true, ...queue, ...o, operations: summaries() });
  k.publish = (st) => { rev += 1; return { ...st, revision: rev, operations: summaries() }; };
  k.routes = (s) => {
    s.on("POST", ROUTES.switchOut, async (e) => {
      if (!k.setup) {
        k.setup = keep(e, recOf(e, { kind: "switch-out", status: "running", effect: "none", sent: 0, last: "" }));
        return "lost";
      }
      if (e.body.take) {
        Object.assign(k.setup, { status: "cancelled", sent: 0, of: 1, last: "" });
        rev = 40;
        k.revs.switched = rev;
        queue = { transport: "paused", length: 1, selected: 9, track: { entry: 9, path: "Album/09.flac", named: true, title: "T9", artist: "", album: "",
          codec: "FLAC", bits: 16, rateHz: 44100, channels: 2, duration: 200 } };
        k.switched = keep(e, recOf(e, { kind: "switch-out", silence: rev, handover }));
        const st = k.pub();
        if (lost) return "lost";
        await k.held.p;
        return answer(k.switched, st);
      }
      if (k.stuck) {
        // Still running as its answer is lost: its Pause not answered yet.
        k.stuck = false;
        rev += 1;
        k.revs.stuck = rev;
        keep(e, recOf(e, { kind: "switch-out", status: "running", effect: "none", sent: 0, last: "" }));
        return "lost";
      }
      rev += 1;
      k.revs.after.push(rev);
      const rec = keep(e, recOf(e, { kind: "switch-out", silence: rev }));
      const st = k.pub();
      const h = k.hold;
      k.hold = null;
      if (h) await h.p;
      return lostAfter ? "lost" : answer(rec, st);
    });
    s.on("GET", ROUTES.operation, async (e) => {
      const seq = Number(e.query.get("seq"));
      if (k.unreachable.delete(seq)) return "lost";
      const rec = last.get(e.query.get("tab"));
      if (!rec || rec.seq !== seq) return { status: 404, body: { error: "operation_gone", epoch: E1, revision: rev } };
      if (rec === k.setup) k.revs.running = rev;
      else await (rec === k.switched ? k.readBack : k.backHold).p;
      return { body: { epoch: E1, revision: rev, record: rec } };
    });
  };
  return k;
}

// afterTheSwitch checks the console's history past the switch: every barrier
// it admitted since ran at a revision above the switch's silence.
function afterTheSwitch(k) {
  check("the console's history: each later barrier's silence above the switch's",
    k.revs.after.length > 0 && k.revs.after.every((x) => x > k.revs.switched), k.revs);
}

// switchOutstanding is gatedPlaying beside theConsole, o the options of both:
// a row of the library played, its barrier lost while the console runs it, so
// the bar is unconfirmed with this browser chosen and track 1 playing on;
// then, unless o.pick is false, this browser picked again, which sends the
// switch that takes the bridge's queue (r.switchSeq; its promise k.p).
async function switchOutstanding(o = {}) {
  const r = await gatedPlaying(o);
  const k = theConsole(o);
  k.routes(r.s);
  (await libraryRows([flac(5), flac(6)], { id: "a5" }))[0].click();
  await settle();
  const v = r.out.view();
  check("a row's barrier lost: unconfirmed, this browser chosen, track 1 playing on",
    v.phase === PHASE.unconfirmed && v.output === "browser" && !r.el.paused && r.audio.snapshot().track?.path === "Album/01.flac",
    [v.phase, v.output, r.el.paused, r.audio.snapshot().track?.path]);
  if (o.pick === false) return { r, k };
  k.p = r.out.pick("browser");
  r.switchSeq = r.out.identity().seq;
  await settle();
  check("the switch to this browser sent to take the bridge's queue, unanswered",
    r.s.posts(ROUTES.switchOut).some((e) => e.body.take === true && e.body.op.seq === r.switchSeq) && r.out.view().phase === (o.lost ? PHASE.unconfirmed : PHASE.toBrowser),
    [r.s.posts(ROUTES.switchOut).map((e) => [e.body.take, e.body.op.seq]), r.out.view().phase]);
  check("the console's history: the setup barrier's running read back at a revision below the switch's silence",
    k.revs.running > 0 && k.revs.running < k.revs.switched, k.revs);
  return { r, k };
}

// answered lets the switch's answer go and waits for it.
async function answered(k) {
  k.held.open();
  await k.p;
  await settle();
}

// stopLeftAlone checks what the Stop left once the switch has ended: nothing
// of the handover taken or played, the queue empty at no track, the element
// paused, and this browser the output, in its own phase, nothing said.
function stopLeftAlone(r, how) {
  const s = r.audio.snapshot();
  const v = r.out.view();
  check(`${how}: nothing taken or played, the queue empty as the Stop left it, silent`,
    !r.el.plays.some((src) => /09\.flac/.test(src)) && s.queue.length === 0 && s.index === -1 && r.el.paused && !s.playing,
    [r.el.plays, s.queue.length, s.index, r.el.paused, s.playing]);
  check(`${how}: this browser the output, in its own phase, nothing said`,
    v.output === "browser" && v.phase === PHASE.browser && v.line === "", [v.output, v.phase, v.line]);
}

// pausedOnTrack1 checks that nothing of the handover was taken or played and
// that this browser's own queue is paused on track 1.
function pausedOnTrack1(r, how) {
  const s = r.audio.snapshot();
  check(`${how}: nothing taken or played, this browser's queue paused on track 1`,
    !r.el.plays.some((src) => /09\.flac/.test(src)) && s.queue.length === 3 && s.index === 0 && s.track?.path === "Album/01.flac" && r.el.paused && !s.playing,
    [r.el.plays, s.queue.length, s.index, s.track?.path, r.el.paused, s.playing]);
}

S.TestAStopWhileASwitchToThisBrowserIsOutstandingTakesNothing = async () => {
  const { r, k } = await switchOutstanding();
  session.handlers.stop();
  await settle();
  await answered(k);
  stopLeftAlone(r, "the switch answered done beside a silent bridge after the lock screen's Stop");
};

S.TestAPauseWhileASwitchToThisBrowserIsOutstandingTakesNothing = async () => {
  const { r, k } = await switchOutstanding({ bar: true });
  session.handlers.pause();
  await settle();
  await answered(k);
  pausedOnTrack1(r, "the switch answered done beside a silent bridge after the lock screen's Pause");
  const v = r.out.view();
  check("this browser the output, in its own phase, nothing said", v.output === "browser" && v.phase === PHASE.browser && v.line === "", [v.output, v.phase, v.line]);
  const posts = r.s.posts(ROUTES.switchOut).length;
  r.find("Play")?.click();
  await settle();
  check("the bar's Play then plays track 1, through a barrier of its own",
    r.s.posts(ROUTES.switchOut).length === posts + 1 && !r.el.paused && /01\.flac/.test(r.el.plays.at(-1) || "") && r.audio.snapshot().track?.path === "Album/01.flac",
    [r.s.posts(ROUTES.switchOut).length - posts, r.el.paused, r.el.plays]);
};

S.TestARowWhileASwitchToThisBrowserIsOutstandingPlaysAndTakesNothing = async () => {
  const { r, k } = await switchOutstanding();
  const rows = await libraryRows([flac(7), flac(8)], { id: "a7" });
  const posts = r.s.posts(ROUTES.switchOut).length;
  rows[1].click();
  await settle();
  const sent = r.s.posts(ROUTES.switchOut);
  check("the library's row plays through a barrier of its own, 08.flac selected and playing",
    sent.length === posts + 1 && sent.at(-1).body.take === false && !r.el.paused && r.audio.snapshot().track?.path === "Album/08.flac" && /08\.flac/.test(r.el.plays.at(-1) || ""),
    [sent.length - posts, sent.at(-1)?.body.take, r.el.paused, r.audio.snapshot().track?.path, r.el.plays]);
  await answered(k);
  const s = r.audio.snapshot();
  const v = r.out.view();
  check("the switch answered done beside a silent bridge after the row: nothing taken or played, the row's album playing on",
    !r.el.plays.some((src) => /09\.flac/.test(src)) && s.queue.length === 2 && s.index === 1 && s.track?.path === "Album/08.flac" && !r.el.paused && s.playing,
    [r.el.plays, s.queue.length, s.index, s.track?.path, r.el.paused, s.playing]);
  check("this browser the output, in its own phase, nothing said", v.output === "browser" && v.phase === PHASE.browser && v.line === "", [v.output, v.phase, v.line]);
};

S.TestTheSwitchsLateAnswerLeavesThePhaseARowsLostBarrierSet = async () => {
  // The console runs the switch to done and holds its answer, then admits the
  // row's barrier at a higher revision and runs it, its answer lost.
  const { r, k } = await switchOutstanding({ lostAfter: true });
  const rows = await libraryRows([flac(7), flac(8)], { id: "a7" });
  const posts = r.s.posts(ROUTES.switchOut).length;
  rows[0].click();
  await settle();
  check("the row's barrier lost once the console ran it: unconfirmed again, track 1 playing on",
    r.s.posts(ROUTES.switchOut).length === posts + 1 && r.out.view().phase === PHASE.unconfirmed && r.out.view().line === WORDS.unconfirmed &&
      !r.el.paused && r.audio.snapshot().track?.path === "Album/01.flac",
    [r.s.posts(ROUTES.switchOut).length - posts, r.out.view().phase, r.out.view().line, r.el.paused, r.audio.snapshot().track?.path]);
  await answered(k);
  const s = r.audio.snapshot();
  const v = r.out.view();
  check("the switch's late answer: nothing taken or played, track 1 playing on",
    !r.el.plays.some((src) => /09\.flac/.test(src)) && s.queue.length === 3 && s.track?.path === "Album/01.flac" && !r.el.paused,
    [r.el.plays, s.queue.length, s.track?.path, r.el.paused]);
  check("this browser the output, the phase and the line as the row's lost barrier left them",
    v.output === "browser" && v.phase === PHASE.unconfirmed && v.line === WORDS.unconfirmed, [v.output, v.phase, v.line]);
};

S.TestTheSwitchsAnswerAfterTheBridgePlaysAgainSaysSoAndTakesNothing = async () => {
  const { r, k } = await switchOutstanding();
  session.handlers.pause();
  await settle();
  r.frame(k.publish(playing()));
  await settle();
  await answered(k);
  pausedOnTrack1(r, "the bridge playing again before the switch's answer, after the lock screen's Pause");
  const v = r.out.view();
  check("this browser the output, in its own phase, saying the bridge plays again",
    v.output === "browser" && v.phase === PHASE.browser && v.line === WORDS.bridgeAgain, [v.output, v.phase, v.line]);
};

S.TestAPausesEventAfterThePickOfThisBrowserLetsItsHandoverPlay = async () => {
  // The Pause is the reader's action before the switch was sent, and its
  // pause event, which comes after, is no action of the reader's.
  const { r, k } = await switchOutstanding({ pick: false });
  let events = 0;
  r.el.addEventListener("pause", () => { events += 1; });
  session.handlers.pause();
  k.p = r.out.pick("browser");
  check("the lock screen's Pause, then the pick of this browser before the Pause's event: the switch sent",
    events === 0 && r.el.paused && r.s.posts(ROUTES.switchOut).some((e) => e.body.take === true), [events, r.el.paused, r.s.posts(ROUTES.switchOut).map((e) => e.body.take)]);
  await settle();
  check("the Pause's event came after the switch was sent", events === 1, events);
  await answered(k);
  const s = r.audio.snapshot();
  check("the switch answered done beside a silent bridge: the handover taken, its 09.flac playing, once",
    r.el.plays.filter((src) => /09\.flac/.test(src)).length === 1 && s.queue.length === 1 && s.track?.path === "Album/09.flac" && !r.el.paused,
    [r.el.plays, s.queue.length, s.track?.path, r.el.paused]);
};

S.TestASeekOrAnAddWhileASwitchToThisBrowserIsOutstandingLeavesItsHandoverToPlay = async () => {
  // None of these changes what plays, so the switch's answer still takes the
  // bridge's queue in place of what they left, as it did before.
  const { r, k } = await switchOutstanding({ bar: true });
  const rows = await libraryRows([flac(7)], { id: "a7" });
  findAll(rows.view, (n) => n.tagName === "BUTTON" && n.textContent === "Add to queue")[0]?.click();
  session.handlers.seekto({ seekTime: 10 });
  const vol = r.byClass("np-vol");
  vol.value = "50";
  vol.fire("input");
  r.find("Shuffle")?.click();
  r.find("Repeat off")?.click();
  const s0 = r.audio.snapshot();
  check("an album added, a seek, the level, shuffle and repeat while the switch is outstanding",
    s0.queue.length === 4 && r.el.currentTime === 10 && r.el.volume === 0.5 && s0.shuffle && s0.repeat === "all" && !r.el.paused,
    [s0.queue.length, r.el.currentTime, r.el.volume, s0.shuffle, s0.repeat, r.el.paused]);
  await answered(k);
  const s = r.audio.snapshot();
  check("the switch answered done beside a silent bridge: the handover taken, its 09.flac playing, once",
    r.el.plays.filter((src) => /09\.flac/.test(src)).length === 1 && s.queue.length === 1 && s.track?.path === "Album/09.flac" && !r.el.paused,
    [r.el.plays, s.queue.length, s.track?.path, r.el.paused]);
};

S.TestAStopBeforeTheSwitchsLostAnswerIsReadBackTakesAndStartsNothing = async () => {
  const { r } = await switchOutstanding({ lost: true });
  session.handlers.stop();
  await settle();
  const posts = r.s.posts(ROUTES.switchOut).length;
  r.c.advance(READ_BACK_MS);
  r.out.tick();
  await settle();
  const backs = r.s.gets(ROUTES.operation).map((e) => Number(e.query.get("seq")));
  check("the switch's record read back done after the Stop", backs.includes(r.switchSeq), backs);
  check("no barrier asked for its handover", r.s.posts(ROUTES.switchOut).length === posts, r.s.posts(ROUTES.switchOut).length - posts);
  stopLeftAlone(r, "read back done after the lock screen's Stop");
};

// reloaded is this tab loaded again over the same session storage: every
// shipped module a fresh instance, from a copy of their directory, as a new
// document has them; a console connection of its own, over the console the
// case's routes keep, its GET now answering st; the gate wired as boot.js
// wires it; and the stream's first frame st.
async function reloaded(routes, st) {
  const fs = await import("node:fs");
  const path = await import("node:path");
  const os = await import("node:os");
  const { fileURLToPath, pathToFileURL } = await import("node:url");
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "bar-reload-"));
  process.on("exit", () => fs.rmSync(dir, { recursive: true, force: true }));
  fs.cpSync(fileURLToPath(dirURL), dir, { recursive: true });
  const base = pathToFileURL(dir + path.sep).href;
  const p = page();
  const o = await import(new URL("output.js", base));
  const audio = await import(new URL("audio.js", base));
  audio.init();
  const c = clock();
  const s = server(c);
  s.on("GET", ROUTES.now, async () => ({ body: st, headers: { [CLOCK_HEADER]: new Date(c.now()).toISOString() } }));
  s.on("GET", ROUTES.queue, async () => ({ body: { epoch: st.epoch, readAt: st.revision, queueRev: st.queueRev, token: 1, selected: 0, entries: [] } }));
  routes(s);
  const b = { handover: audio.handover, hasQueue: audio.hasQueue, suspend: audio.suspend, resume: audio.resume, take: audio.take, start: audio.start, mediaSession: audio.setMediaSession };
  const out = o.createOutput(env(s, c, b));
  o.installOutput(out);
  audio.setStartGate(out.gateStart, out.cancelStarts);
  audio.setContinueGate(out.gateContinue);
  out.streamOpen(1);
  out.frame(JSON.stringify(st), 1);
  await settle();
  return { ...p, audio, el: p.audios[0], s, c, out, base };
}

S.TestAStopBeforeAReloadIsNotSeenAndTheMoveComesBackPaused = async () => {
  // What this change leaves as it was: the reader's generation is the
  // document's own, so the reload's read back of the switch knows nothing of
  // the Stop made before it.
  const { r, k } = await switchOutstanding();
  session.handlers.stop();
  await settle();
  const tab = r.out.identity().tab;
  const d = await reloaded(k.routes, k.pub());
  const backs = d.s.gets(ROUTES.operation).filter((e) => e.query.get("tab") === tab).map((e) => Number(e.query.get("seq")));
  check("the reload reads the switch back by its tab's identity", backs.includes(r.switchSeq), backs);
  const s = d.audio.snapshot();
  check("and takes the bridge's queue back paused: 09.flac alone at index 0, nothing played",
    s.queue.length === 1 && s.index === 0 && s.track?.path === "Album/09.flac" && d.el.plays.length === 0 && d.el.paused && !s.playing,
    [s.queue.length, s.index, s.track?.path, d.el.plays, d.el.paused, s.playing]);
  check("nothing asked of the gate, the bar saying to press Play",
    d.s.posts(ROUTES.switchOut).length === 0 && d.out.view().phase === PHASE.browser && d.out.view().line === WORDS.pressPlay,
    [d.s.posts(ROUTES.switchOut).length, d.out.view().phase, d.out.view().line]);
};

S.TestARowPlayedAfterAReloadStaysWhenTheMoveIsReadBack = async () => {
  const { r, k } = await switchOutstanding();
  k.readBack = gate();
  const d = await reloaded(k.routes, k.pub());
  const backs = d.s.gets(ROUTES.operation).map((e) => Number(e.query.get("seq")));
  check("the reload asks for the switch's record, its answer held, unconfirmed meanwhile",
    backs.includes(r.switchSeq) && d.out.view().phase === PHASE.unconfirmed, [backs, d.out.view().phase]);
  const rows = await libraryRows([flac(7), flac(8)], { id: "a7", base: d.base });
  rows[0].click();
  await settle();
  const sent = d.s.posts(ROUTES.switchOut);
  check("the library's row plays in the new document, through a barrier of its own",
    sent.length === 1 && sent[0].body.take === false && !d.el.paused && d.audio.snapshot().track?.path === "Album/07.flac" && /07\.flac/.test(d.el.plays.at(-1) || ""),
    [sent.map((e) => e.body.take), d.el.paused, d.audio.snapshot().track?.path, d.el.plays]);
  k.readBack.open();
  await settle();
  const s = d.audio.snapshot();
  const v = d.out.view();
  check("the switch's record read back done: the row stays, playing, nothing taken",
    !d.el.plays.some((src) => /09\.flac/.test(src)) && s.queue.length === 2 && s.track?.path === "Album/07.flac" && !d.el.paused && s.playing,
    [d.el.plays, s.queue.length, s.track?.path, d.el.paused, s.playing]);
  check("this browser the output, in its own phase, nothing said", v.output === "browser" && v.phase === PHASE.browser && v.line === "", [v.output, v.phase, v.line]);
};

S.TestAHandoverPlaysAndGoesOnWithNoReaderActionBetween = async () => {
  // The switch's own take moves what plays and voids nothing of its permit:
  // the play its metadata defers, and the next track at its end.
  const r = await handedOver(handoverAt30);
  check("the handover taken to play at 30 s, waiting for its metadata",
    r.el.paused && r.audio.snapshot().index === 0 && r.audio.snapshot().queue.length === 2 && r.el.plays.length === 0, [r.el.paused, r.audio.snapshot().index, r.el.plays]);
  r.el.metadata(200);
  await settle();
  check("its metadata in, it plays once, from its position", !r.el.paused && r.el.currentTime === 30 && r.el.plays.length === 1 && /01\.flac/.test(r.el.plays[0] || ""),
    [r.el.paused, r.el.currentTime, r.el.plays]);
  const n = r.n;
  r.el.end();
  await settle();
  check("its end plays the next track on the switch's permit, no barrier",
    !r.el.paused && r.audio.snapshot().index === 1 && r.el.plays.length === 2 && /02\.flac/.test(r.el.plays.at(-1) || "") && r.n === n,
    [r.el.paused, r.audio.snapshot().index, r.el.plays, r.n - n]);
};

// nextDuringASwitchIn is track 1 playing under the bar, the bridge picked:
// this browser stopped for it and the replace sent, its answer reply(e) when
// the case lets it go; before(r, e), where given, runs next, then GET now says
// no player is offered, so the bar drives this browser again, and its Next
// plays track 2 at once. plays counts the plays up to that one.
async function nextDuringASwitchIn(reply, before) {
  const r = await gatedPlaying({ bar: true });
  const held = gate();
  r.s.on("POST", ROUTES.replace, async (e) => { await held.p; return reply(e); });
  const p = r.out.pick("bridge");
  await settle();
  check("the switch to the bridge sent, this browser stopped for it",
    r.s.posts(ROUTES.replace).length === 1 && r.el.paused && r.out.view().phase === PHASE.toBridge, [r.s.posts(ROUTES.replace).length, r.el.paused, r.out.view().phase]);
  if (before) {
    before(r, r.s.posts(ROUTES.replace)[0]);
    await settle();
  }
  r.s.on("GET", ROUTES.now, async () => ({ status: 409, body: { error: "player_not_offered", message: "this bridge offers no player of its own" } }));
  await r.out.sampleNow();
  await settle();
  const next = r.find("Next");
  check("GET now says no player is offered: the bar drives this browser", r.out.view().notOffered && next && !next.disabled, [r.out.view().notOffered, next?.disabled]);
  next?.click();
  await settle();
  check("the bar's Next plays track 2 at once", !r.el.paused && r.audio.snapshot().index === 1 && /02\.flac/.test(r.el.plays.at(-1) || ""), [r.el.paused, r.audio.snapshot().index, r.el.plays]);
  const plays = r.el.plays.length;
  held.open();
  await p;
  await settle();
  return { r, plays };
}

// nextStands checks that the switch's end left the reader's Next standing:
// track 2 playing on, nothing of track 1 played after it, this browser the
// output in its own phase, saying the switch's words.
function nextStands(r, plays, how, line) {
  const after = r.el.plays.slice(plays);
  const v = r.out.view();
  check(`${how}: the Next stands, track 2 playing on, nothing of track 1 played again`,
    !after.some((src) => /01\.flac/.test(src)) && !r.el.paused && r.audio.snapshot().index === 1 && r.audio.snapshot().track?.path === "Album/02.flac",
    [after, r.el.paused, r.audio.snapshot().index]);
  check(`${how}: this browser the output, in its own phase, its words said`,
    v.output === "browser" && v.phase === PHASE.browser && v.line === line, [v.output, v.phase, v.line]);
}

S.TestANextWhileTheSwitchToTheBridgeIsRefusedForNoPlayerStands = async () => {
  const { r, plays } = await nextDuringASwitchIn(() => refusal("player_not_offered", null, 409, "this bridge offers no player of its own"));
  nextStands(r, plays, "the switch refused for no player", "this bridge offers no player of its own");
};

S.TestANextWhileTheSwitchToTheBridgeIsRefusedWithNothingChangedStands = async () => {
  // The console publishes the refusal's terminal record at revision 6, above
  // every publication before it, and the stream delivers that state first;
  // then GET now says no player is offered, and the HTTP answer carrying the
  // same state comes last, not taken again, so no player is offered still.
  const refused = (e) => recOf(e, { kind: "replace", status: "refused", error: "stale", effect: "none", sent: 1, of: 4, last: "conflict" });
  let st = null;
  const { r, plays } = await nextDuringASwitchIn((e) => answer(refused(e), st), (r, e) => {
    st = pub({ revision: 6, silent: true, operations: [refused(e)] });
    r.frame(st);
  });
  check("the refusal's state taken from the stream before its answer, no barrier sent for a resume",
    r.out.view().state?.revision === 6 && r.out.view().notOffered && r.s.posts(ROUTES.switchOut).length === 1,
    [r.out.view().state?.revision, r.out.view().notOffered, r.s.posts(ROUTES.switchOut).length]);
  nextStands(r, plays, "the switch refused with nothing changed", "The player changed since this page last read it: nothing was changed");
};

// resumeQueued is the browser pausing the element that plays and resuming it
// at once, both events still to come, its next barrier held (r.hold): the
// pause's event finds the element playing again, so what this browser meant
// to play stands, and the play's pauses the element and waits on that
// barrier, shown starting.
async function resumeQueued(r, held = gate()) {
  r.hold = held;
  r.el.pause();
  r.el.browserPlay();
  await settle();
  check("the browser's resume waits on its barrier, the element paused, shown starting",
    r.el.paused && r.audio.snapshot().loading && !r.audio.snapshot().playing, [r.el.paused, r.audio.snapshot().loading, r.audio.snapshot().playing]);
  return held;
}

S.TestCancellingAResumeWaitLeavesNothingForItsRetryToPlay = async () => {
  const r = await gatedPlaying({ bar: true });
  const held = await resumeQueued(r);
  const plays = r.el.plays.length;
  r.byClass("np-btn-play").click();
  r.el.fail(2);
  await settle();
  check("the bar's spinner pressed, then a network error: the retry reloads the track and plays nothing",
    r.el.plays.length === plays && r.el.paused && /01\.flac.*_r=/.test(r.el.src), [r.el.plays.slice(plays), r.el.paused, r.el.src]);
  held.open();
  await settle();
  check("the barrier's answer plays nothing either, the element paused",
    r.el.plays.length === plays && r.el.paused && !r.audio.snapshot().playing, [r.el.plays.slice(plays), r.el.paused, r.audio.snapshot().playing]);
};

S.TestAnErrorWhileAResumeWaitsPlaysOnlyOnTheResumesAdmission = async () => {
  const r = await gatedPlaying();
  let held = await resumeQueued(r);
  let plays = r.el.plays.length;
  r.el.fail(2);
  await settle();
  check("a network error while the resume waits on its barrier: the retry reloads the track and plays nothing",
    r.el.plays.length === plays && r.el.paused && /01\.flac.*_r=/.test(r.el.src), [r.el.plays.slice(plays), r.el.paused, r.el.src]);
  held.open();
  await settle();
  check("the resume's admission plays the reloaded track, once",
    r.el.plays.length === plays + 1 && !r.el.paused && /01\.flac.*_r=/.test(r.el.plays.at(-1) || ""), [r.el.plays.slice(plays), r.el.paused]);
  // Track 2, and a resume its barrier refuses: nothing plays at all.
  session.handlers.nexttrack();
  await settle();
  r.el.metadata(200);
  held = gate();
  heldBarrier(r, held, { refuse: true });
  await resumeQueued(r, held);
  plays = r.el.plays.length;
  r.el.fail(2);
  await settle();
  held.open();
  await settle();
  check("on track 2 a network error while its resume waits, then the barrier refused: nothing plays",
    r.audio.snapshot().index === 1 && r.el.plays.length === plays && r.el.paused && !r.audio.snapshot().playing,
    [r.audio.snapshot().index, r.el.plays.slice(plays), r.el.paused, r.audio.snapshot().playing]);
};

// ---- What a switch's end settles, and a failure during a resume's wait ----
//
// A switch to this browser settles, however it ends, only what it shows
// itself: its moving words, which no other operation sets, and its own phase,
// its move or the unconfirmed its own loss set while no other operation of the
// current pick is lost here or inherited and unresolved. And a failure past
// its retry, or a source the browser cannot use, ends a resume of the
// browser's own that waits on its barrier.

S.TestAStopAfterARowRetiresTheSwitchsMovingWordsWhenItIsAnswered = async () => {
  // As the hazard has it: this browser the output and the bar unconfirmed
  // before the pick, so the row's start is admitted as a barrier.
  const { r, k } = await switchOutstanding();
  const rows = await libraryRows([flac(7), flac(8)], { id: "a7" });
  const latch = gate();
  k.hold = latch;
  rows[0].click();
  await settle();
  const row = opKey(E1, r.out.identity().tab, r.out.identity().seq);
  check("the row's barrier sent, its answer held, the bar still moving to this browser",
    r.s.posts(ROUTES.switchOut).at(-1).body.take === false && r.out.pending().includes(row) && r.out.view().phase === PHASE.toBrowser && r.out.view().line === WORDS.toBrowser,
    [r.out.pending(), r.out.view().phase, r.out.view().line]);
  session.handlers.stop();
  await settle();
  check("the lock screen's Stop while the row's answer is held: the queue empty, the row's operation still pending",
    r.audio.snapshot().queue.length === 0 && r.out.pending().includes(row), [r.audio.snapshot().queue.length, r.out.pending()]);
  latch.open();
  await settle();
  check("the row's barrier answered after the Stop: nothing plays, this browser's phase, the switch's moving words still shown",
    r.el.paused && r.out.view().phase === PHASE.browser && r.out.view().line === WORDS.toBrowser, [r.el.paused, r.out.view().phase, r.out.view().line]);
  await answered(k);
  const s = r.audio.snapshot();
  const v = r.out.view();
  check("the switch's answer then retires its moving words: nothing said, this browser's phase",
    v.line === "" && v.phase === PHASE.browser && v.output === "browser", [v.line, v.phase, v.output]);
  check("the queue empty, nothing of the handover played, no operation pending",
    s.queue.length === 0 && !r.el.plays.some((src) => /09\.flac/.test(src)) && r.out.pending().length === 0, [s.queue.length, r.el.plays, r.out.pending()]);
  afterTheSwitch(k);
};

S.TestAnInheritedSwitchLeavesTheWarningOfARowLostSinceTheReload = async () => {
  const { r, k } = await switchOutstanding({ lostAfter: true });
  k.readBack = gate();
  const d = await reloaded(k.routes, k.pub());
  check("the reload asks for the switch's record, its answer held, unconfirmed meanwhile",
    d.s.gets(ROUTES.operation).some((e) => Number(e.query.get("seq")) === r.switchSeq) && d.out.view().phase === PHASE.unconfirmed,
    [d.s.gets(ROUTES.operation).map((e) => e.query.get("seq")), d.out.view().phase]);
  (await libraryRows([flac(7), flac(8)], { id: "a7", base: d.base }))[0].click();
  await settle();
  check("a row in the new document, its barrier's answer lost once the console ran it: unconfirmed, its words said",
    d.s.posts(ROUTES.switchOut).length === 1 && d.out.view().phase === PHASE.unconfirmed && d.out.view().line === WORDS.unconfirmed,
    [d.s.posts(ROUTES.switchOut).length, d.out.view().phase, d.out.view().line]);
  k.readBack.open();
  await settle();
  const s = d.audio.snapshot();
  check("the switch's record read back done: nothing taken, the row's warning kept as the switch settles",
    d.out.view().phase === PHASE.unconfirmed && d.out.view().line === WORDS.unconfirmed && !d.el.plays.some((src) => /09\.flac/.test(src)) &&
      s.queue.length === 3 && s.track?.path === "Album/01.flac",
    [d.out.view().phase, d.out.view().line, d.el.plays, s.queue.length, s.track?.path]);
  d.c.advance(READ_BACK_MS);
  d.out.tick();
  await settle();
  check("the row's own read back then: this browser's phase and the row's own words, nothing pending",
    d.out.view().phase === PHASE.browser && d.out.view().line === WORDS.startAgain && d.out.pending().length === 0,
    [d.out.view().phase, d.out.view().line, d.out.pending()]);
  afterTheSwitch(k);
};

S.TestAnInheritedSwitchLeavesTheWarningOfAnInheritedBarrierStillUnresolved = async () => {
  // Three documents of one tab: the first sends the switch; the second,
  // loaded again over it, plays a row whose barrier the console still runs as
  // its answer is lost; the third, loaded again over the second, inherits the
  // switch and that barrier, both of its pick, and its reader presses the lock
  // screen's Stop, which sends nothing, so no newer barrier runs at the
  // console beside the one still running.
  const { r, k } = await switchOutstanding();
  k.readBack = gate();
  const d1 = await reloaded(k.routes, k.pub());
  k.stuck = true;
  (await libraryRows([flac(5), flac(6)], { id: "a5", base: d1.base }))[0].click();
  await settle();
  const barrier = opKey(E1, d1.out.identity().tab, d1.out.identity().seq);
  check("the second document's row: its barrier lost while the console runs it, published above the switch's silence",
    d1.s.posts(ROUTES.switchOut).length === 1 && d1.out.view().phase === PHASE.unconfirmed && k.revs.stuck > k.revs.switched,
    [d1.s.posts(ROUTES.switchOut).length, d1.out.view().phase, k.revs]);
  const d2 = await reloaded(k.routes, k.pub());
  check("the third document inherits the switch and that barrier, unconfirmed, the switch's record asked for and held",
    d2.out.pending().length === 2 && d2.out.view().phase === PHASE.unconfirmed &&
      d2.s.gets(ROUTES.operation).some((e) => Number(e.query.get("seq")) === r.switchSeq),
    [d2.out.pending(), d2.out.view().phase]);
  session.handlers.stop();
  await settle();
  const s0 = d2.audio.snapshot();
  check("the third document's lock-screen Stop: no barrier asked, its queue empty and paused",
    d2.s.posts(ROUTES.switchOut).length === 0 && s0.queue.length === 0 && s0.index === -1 && d2.el.paused,
    [d2.s.posts(ROUTES.switchOut).length, s0.queue.length, s0.index, d2.el.paused]);
  check("the Stop leaves the bar unconfirmed, both inherited operations pending",
    d2.out.view().phase === PHASE.unconfirmed && d2.out.pending().length === 2, [d2.out.view().phase, d2.out.pending()]);
  k.readBack.open();
  await settle();
  const s = d2.audio.snapshot();
  const v = d2.out.view();
  check("the switch's record read back done: nothing taken or played, the inherited barrier alone pending, its warning kept",
    s.queue.length === 0 && s.index === -1 && d2.el.paused && d2.el.plays.length === 0 && JSON.stringify(d2.out.pending()) === JSON.stringify([barrier]) &&
      v.phase === PHASE.unconfirmed && v.line === WORDS.unconfirmed,
    [s.queue.length, s.index, d2.el.paused, d2.el.plays, d2.out.pending(), v.phase, v.line]);
};

S.TestAGoneSwitchLeavesTheWarningOfARowLostSinceItWasSent = async () => {
  // The console runs the switch, then the row's barrier, both answers lost:
  // once it keeps the row's record it no longer keeps the switch's.
  const { r, k } = await switchOutstanding({ lost: true, lostAfter: true });
  (await libraryRows([flac(7), flac(8)], { id: "a7" }))[0].click();
  await settle();
  const rowSeq = r.out.identity().seq;
  check("the row's barrier lost too once the console ran it: unconfirmed, its words said",
    r.s.posts(ROUTES.switchOut).at(-1).body.op.seq === rowSeq && r.out.view().phase === PHASE.unconfirmed && r.out.view().line === WORDS.unconfirmed,
    [r.out.view().phase, r.out.view().line]);
  k.backHold = gate();
  r.c.advance(READ_BACK_MS);
  r.out.tick();
  await settle();
  const backs = r.s.gets(ROUTES.operation).map((e) => Number(e.query.get("seq")));
  check("read back: the switch's record gone, the row's read back held, the row's warning kept",
    backs.includes(r.switchSeq) && backs.includes(rowSeq) && !r.out.pending().includes(opKey(E1, r.out.identity().tab, r.switchSeq)) &&
      r.out.view().phase === PHASE.unconfirmed && r.out.view().line === WORDS.unconfirmed,
    [backs, r.out.pending(), r.out.view().phase, r.out.view().line]);
  k.backHold.open();
  await settle();
  check("the row's own read back then: this browser's phase and the row's own words, nothing pending",
    r.out.view().phase === PHASE.browser && r.out.view().line === WORDS.startAgain && r.out.pending().length === 0,
    [r.out.view().phase, r.out.view().line, r.out.pending()]);
  afterTheSwitch(k);
};

S.TestASwitchsReadBackClearsItsOwnWarningBesideAnOlderPicksLostBarrier = async () => {
  // The barrier sent under the older pick is still lost here as the switch's
  // record is read back: its own read back failed, and its operation_gone
  // comes later.
  const { r, k } = await switchOutstanding({ lost: true });
  session.handlers.stop();
  await settle();
  k.unreachable.add(k.setup.seq);
  r.c.advance(READ_BACK_MS);
  r.out.tick();
  await settle();
  const setup = opKey(E1, r.out.identity().tab, k.setup.seq);
  check("the switch read back done after the Stop, the older pick's barrier still lost: the switch's own warning cleared at once",
    r.out.pending().includes(setup) && r.out.view().phase === PHASE.browser && r.out.view().line === "", [r.out.pending(), r.out.view().phase, r.out.view().line]);
  r.c.advance(READ_BACK_MS);
  r.out.tick();
  await settle();
  check("that barrier's operation_gone next: nothing pending, the bar as it was",
    r.out.pending().length === 0 && r.out.view().phase === PHASE.browser && r.out.view().line === "", [r.out.pending(), r.out.view().phase, r.out.view().line]);
};

S.TestTheSwitchsAnswerKeepsTheGatesWordsAsItSettlesThePhase = async () => {
  const { r, k } = await switchOutstanding();
  r.frame(k.publish(playing()));
  await settle();
  const rows = await libraryRows([flac(7), flac(8)], { id: "a7" });
  const posts = r.s.posts(ROUTES.switchOut).length;
  rows[0].click();
  await settle();
  check("a row clicked beside the bridge playing: refused, the gate's words said, the phase still the switch's move",
    r.s.posts(ROUTES.switchOut).length === posts && r.out.view().line === WORDS.bridgePlaying && r.out.view().phase === PHASE.toBrowser,
    [r.s.posts(ROUTES.switchOut).length - posts, r.out.view().line, r.out.view().phase]);
  await answered(k);
  const s = r.audio.snapshot();
  const v = r.out.view();
  check("the switch's answer: this browser's phase, the gate's words kept, never the switch's own",
    v.phase === PHASE.browser && v.line === WORDS.bridgePlaying && v.output === "browser", [v.phase, v.line, v.output]);
  check("nothing taken: this browser's queue on track 1",
    !r.el.plays.some((src) => /09\.flac/.test(src)) && s.queue.length === 3 && s.track?.path === "Album/01.flac", [r.el.plays, s.queue.length, s.track?.path]);
};

// failedAndPaused checks that a track that failed past its retry was played
// no more: no play attempt since plays, the element paused, the failure said.
function failedAndPaused(r, plays, how) {
  const s = r.audio.snapshot();
  check(`${how}: no play attempt, the element paused, the failure shown`,
    r.el.plays.length === plays && r.el.paused && !s.playing && s.error === 'Playback failed for "T1".', [r.el.plays.slice(plays), r.el.paused, s.playing, s.error]);
}

S.TestAFailurePastItsRetryEndsAResumeWait = async () => {
  const r = await gatedPlaying();
  const held = await resumeQueued(r);
  check("the resume's barrier pending", r.s.posts(ROUTES.switchOut).length === 2, r.s.posts(ROUTES.switchOut).length);
  const plays = r.el.plays.length;
  r.el.fail(2);
  await settle();
  r.el.fail(2);
  await settle();
  failedAndPaused(r, plays, "a network error while the resume waits, then another past its retry");
  held.open();
  await settle();
  failedAndPaused(r, plays, "the resume's admission then");
};

S.TestAFailurePastItsRetryEndsAResumeWaitAfterAStartWithNoPermit = async () => {
  const r = await pageRig({ gate: true, noFrame: true });
  r.s.on("GET", ROUTES.now, async () => ({ status: 409, body: { error: "player_not_offered", message: "no player" } }));
  await r.out.sampleNow();
  await settle();
  (await libraryRows([flac(1), flac(2), flac(3)], { id: "a1" }))[0].click();
  await settle();
  r.el.metadata(200);
  check("no player offered: the library's row plays at once, with no permit, nothing sent",
    !r.el.paused && r.s.posts().length === 0 && /01\.flac/.test(r.el.plays.at(-1) || ""), [r.el.paused, r.s.posts().length, r.el.plays]);
  // The bridge's player comes up, silent: the stream opens, then its frame.
  const latest = pub({ revision: 3, silent: true });
  r.s.on("GET", ROUTES.now, async () => ({ body: latest, headers: { [CLOCK_HEADER]: new Date(r.c.now()).toISOString() } }));
  const held = gate();
  r.s.on("POST", ROUTES.switchOut, async (e) => { await held.p; return answer(recOf(e, { kind: "switch-out", silence: 4 }), pub({ revision: 4, silent: true })); });
  r.out.streamOpen(1);
  r.frame(latest);
  await settle();
  check("the bridge's player offered since, silent, its state taken", r.out.view().offered && r.out.view().state?.revision === 3,
    [r.out.view().offered, r.out.view().state?.revision]);
  r.el.pause();
  r.el.browserPlay();
  await settle();
  check("the browser's resume waits on its barrier, held", r.s.posts(ROUTES.switchOut).length === 1 && r.el.paused && r.audio.snapshot().loading,
    [r.s.posts(ROUTES.switchOut).length, r.el.paused, r.audio.snapshot().loading]);
  const plays = r.el.plays.length;
  r.el.fail(2);
  await settle();
  r.el.fail(2);
  await settle();
  failedAndPaused(r, plays, "a network error while the resume waits, then another past its retry");
  held.open();
  await settle();
  failedAndPaused(r, plays, "the resume's admission then");
};

S.TestASourceTheBrowserCannotUsePlaysNothingOnTheResumesAdmission = async () => {
  const t = longTimers();
  globalThis.fetch = async () => ({ ok: true, status: 200 });
  const r = await gatedPlaying();
  const held = await resumeQueued(r);
  check("the resume's barrier pending", r.s.posts(ROUTES.switchOut).length === 2, r.s.posts(ROUTES.switchOut).length);
  const plays = r.el.plays.length;
  r.el.fail(4);
  await settle();
  check("a source the browser cannot use while the resume waits: its skip scheduled, nothing played",
    t.count(1200) === 1 && r.el.plays.length === plays, [t.count(1200), r.el.plays.slice(plays)]);
  held.open();
  await settle();
  check("the resume's admission then plays nothing of the failed track",
    r.el.plays.length === plays && r.el.paused && r.audio.snapshot().index === 0, [r.el.plays.slice(plays), r.el.paused, r.audio.snapshot().index]);
  t.fire(1200);
  await settle();
  check("the skip then plays the next track through a barrier of its own, as before",
    r.audio.snapshot().index === 1 && /02\.flac/.test(r.el.plays.at(-1) || "") && !r.el.paused && r.s.posts(ROUTES.switchOut).length === 3,
    [r.audio.snapshot().index, r.el.plays.slice(plays), r.el.paused, r.s.posts(ROUTES.switchOut).length]);
};

// sourceFails is gatedPlaying whose source error probe answers that the
// bridge served the track, the skip past it held until the case fires it.
async function sourceFails(o = {}) {
  const t = longTimers();
  globalThis.fetch = async () => ({ ok: true, status: 200 });
  const r = await gatedPlaying(o);
  return { r, t };
}

S.TestASourceTheBrowserCannotUseShowsNoSpinnerForTheResumeItLeaves = async () => {
  // The browser's resume and the source's failure both queued while the
  // element played: the resume's protective pause comes after the failure.
  const { r } = await sourceFails();
  r.hold = gate();
  r.el.pause();
  r.el.browserPlay();
  r.el.fail(4);
  await settle();
  check("the resume waits on its barrier, and no spinner shows over the track that failed",
    r.s.posts(ROUTES.switchOut).length === 2 && r.el.paused && !r.audio.snapshot().loading && !r.audio.snapshot().playing,
    [r.s.posts(ROUTES.switchOut).length, r.el.paused, r.audio.snapshot().loading, r.audio.snapshot().playing]);
};

S.TestTheBarsPlayOverASourceTheBrowserCannotUseIsAStartThoughAResumeWaits = async () => {
  const { r } = await sourceFails({ bar: true });
  const held = await resumeQueued(r);
  r.el.fail(4);
  await settle();
  r.find("Play")?.click();
  await settle();
  held.open();
  await settle();
  check("the bar's Play over the failed track while the resume waits: a start, which plays track 2 on the barrier's answer",
    r.audio.snapshot().index === 1 && /02\.flac/.test(r.el.plays.at(-1) || "") && !r.el.paused, [r.audio.snapshot().index, r.el.plays, r.el.paused]);
};

S.TestAResumesRefusalAfterASourceTheBrowserCannotUseStillEndsItsSkip = async () => {
  // As before this change: the resume's refusal ends what this browser meant
  // to play, so the skip's timer starts nothing.
  const { r, t } = await sourceFails();
  const held = gate();
  let n = 0;
  r.s.on("POST", ROUTES.switchOut, async (e) => {
    n += 1;
    if (n > 1) return answer(recOf(e, { kind: "switch-out", silence: 20 + n }), pub({ revision: 20 + n, silent: true }));
    await held.p;
    return answer(recOf(e, { kind: "switch-out", status: "refused", error: "check", effect: "none", last: "maintenance" }), pub({ revision: 20, silent: true, hold: "check" }));
  });
  await resumeQueued(r, held);
  check("the resume's barrier pending", r.s.posts(ROUTES.switchOut).length === 2, r.s.posts(ROUTES.switchOut).length);
  const plays = r.el.plays.length;
  r.el.fail(4);
  await settle();
  held.open();
  await settle();
  check("a source the browser cannot use, then the resume's barrier refused: nothing played, paused",
    r.el.plays.length === plays && r.el.paused, [r.el.plays.slice(plays), r.el.paused]);
  const posts = r.s.posts(ROUTES.switchOut).length;
  t.fire(1200);
  await settle();
  check("the skip's timer then asks no barrier and plays nothing of track 2, as before",
    r.s.posts(ROUTES.switchOut).length === posts && r.el.plays.length === plays && r.audio.snapshot().index === 0 && r.el.paused,
    [r.s.posts(ROUTES.switchOut).length - posts, r.el.plays.slice(plays), r.audio.snapshot().index, r.el.paused]);
};

S.TestAResumesAdmissionAfterASourceTheBrowserCannotUseLetsItsSkipGoOn = async () => {
  // The browser's own pause ended what this browser meant to play; its own
  // resume, admitted, means to play again, as before this change, so the
  // skip's timer goes on to track 2 through the gate, the failed source never
  // played.
  const { r, t } = await sourceFails();
  const held = await browserResumeWaits(r);
  check("the browser's own pause delivered, then its resume waiting on its barrier",
    r.s.posts(ROUTES.switchOut).length === 2 && r.el.paused && r.audio.snapshot().loading, [r.s.posts(ROUTES.switchOut).length, r.el.paused, r.audio.snapshot().loading]);
  const plays = r.el.plays.length;
  r.el.fail(4);
  await settle();
  held.open();
  await settle();
  check("a source the browser cannot use, then the resume admitted: nothing of the failed track played",
    r.el.plays.length === plays && r.el.paused, [r.el.plays.slice(plays), r.el.paused]);
  const posts = r.s.posts(ROUTES.switchOut).length;
  t.fire(1200);
  await settle();
  check("the skip's timer then plays track 2 through a barrier of its own, as before",
    r.s.posts(ROUTES.switchOut).length === posts + 1 && r.audio.snapshot().index === 1 && /02\.flac/.test(r.el.plays.at(-1) || "") && !r.el.paused,
    [r.s.posts(ROUTES.switchOut).length - posts, r.audio.snapshot().index, r.el.plays.slice(plays), r.el.paused]);
};

S.TestTheNextTrackAfterASkipStartedWhileAResumeWaitedRetriesItsFirstError = async () => {
  // No player offered as the resume's barrier is held: the skip starts track 2
  // at once, and the resume's late admission leaves track 2's playback alone.
  const { r, t } = await sourceFails();
  const held = await resumeQueued(r);
  r.s.on("GET", ROUTES.now, async () => ({ status: 409, body: { error: "player_not_offered", message: "no player" } }));
  await r.out.sampleNow();
  await settle();
  check("no player offered now, the resume's barrier held", r.out.view().notOffered && r.s.posts(ROUTES.switchOut).length === 2,
    [r.out.view().notOffered, r.s.posts(ROUTES.switchOut).length]);
  r.el.fail(4);
  await settle();
  t.fire(1200);
  await settle();
  check("the skip plays track 2 at once", r.audio.snapshot().index === 1 && /02\.flac/.test(r.el.plays.at(-1) || "") && !r.el.paused,
    [r.audio.snapshot().index, r.el.plays, r.el.paused]);
  r.el.metadata(200);
  held.open();
  await settle();
  const plays = r.el.plays.length;
  r.el.fail(2);
  await settle();
  check("the old barrier admitted after track 2 started: track 2's first network error retries once",
    r.el.plays.length === plays + 1 && /02\.flac.*_r=/.test(r.el.plays.at(-1) || "") && !r.el.paused, [r.el.plays.slice(plays), r.el.paused]);
};

S.TestASkipTimerBeforeAResumesAnswerPlaysNothingOfTheNextTrack = async () => {
  // The skip, asked behind the resume's barrier, takes the resume's place
  // there, which ends what this browser meant to play, as before this change.
  const { r, t } = await sourceFails();
  const held = await resumeQueued(r);
  check("the resume's barrier pending", r.s.posts(ROUTES.switchOut).length === 2, r.s.posts(ROUTES.switchOut).length);
  const plays = r.el.plays.length;
  r.el.fail(4);
  await settle();
  t.fire(1200);
  await settle();
  held.open();
  await settle();
  check("the skip's timer before the resume's barrier answers: no barrier more asked and nothing of track 2 played",
    r.s.posts(ROUTES.switchOut).length === 2 && r.el.plays.length === plays && r.audio.snapshot().index === 0 && r.el.paused,
    [r.s.posts(ROUTES.switchOut).length, r.el.plays.slice(plays), r.audio.snapshot().index, r.el.paused]);
};

S.TestAResumeHeldAcrossTheStopAfterThreeUnplayableTracksStartsNothingAfterIt = async () => {
  const { r, t } = await failing({ pend: true, tracks: [flac(1), flac(2), flac(3), flac(4), flac(5)] });
  for (let i = 0; i < 2; i++) {
    r.el.fail(4);
    await settle();
    t.fire(1200);
    await settle();
  }
  check("two tracks that cannot play skipped: track 3 playing", r.audio.snapshot().index === 2 && !r.el.paused, [r.audio.snapshot().index, r.el.paused]);
  const held = await resumeQueued(r);
  r.el.fail(4);
  await settle();
  check("the third in a row while the browser's resume waits on its barrier: stopped, said",
    t.count(1200) === 0 && /can't play in this browser/.test(r.audio.snapshot().error) && r.s.posts(ROUTES.switchOut).length === 4,
    [t.count(1200), r.audio.snapshot().error, r.s.posts(ROUTES.switchOut).length]);
  held.open();
  await settle();
  const [posts, plays] = [r.s.posts(ROUTES.switchOut).length, r.el.plays.length];
  r.audio.removeAt(2);
  await settle();
  check("the resume admitted after the stop, then the failed row removed: no successor started, nothing more played",
    r.s.posts(ROUTES.switchOut).length === posts && r.el.plays.length === plays && r.el.paused,
    [r.s.posts(ROUTES.switchOut).length - posts, r.el.plays.slice(plays), r.el.paused]);
};

// ---- The console by touch (B1021): where a DSD row plays, the album's
// controls, the phone's times ----
//
// In this browser a DSD row says it cannot play here and, by its catalog way,
// what the bridge's player does with it; the album's count line says how many
// rows the bridge's player plays and points to Output; Play, Shuffle and Add
// to queue are disabled, their reason in the count line, where no row plays
// on the output chosen, by the decision the queue plays by; and the bar keeps
// its seek and both times (a phone shows the times, read-only beside the
// rail, its seek hidden), and says how tall it renders.

// albumParts is an album page's count line and its three action buttons.
const albumParts = (view) => {
  const btn = (text) => findAll(view, (n) => n.tagName === "BUTTON" && n.textContent === text)[0] || null;
  return {
    line: findAll(view, (n) => n.tagName === "P" && /can't play/.test(n.textContent))[0] || null,
    play: btn("Play"), shuffle: btn("Shuffle"), add: btn("Add to queue"),
  };
};

// rowChips is the chip of each row of an album page, "" for a row with none.
const rowChips = (view) => findAll(view, (n) => n.classList.contains("track-why")).map((n) => n.textContent);

const viaBridge = (n, way, o = {}) => dsd(n, { bridge: way === "none" ? { way, reason: "dsd_rate_not_played" } : { way }, ...o });
const DSD_WORDS = "DSD — not playable in a browser";

S.TestADSDRowSaysWhereItPlays = async () => {
  await pageRig();
  const native = viaBridge(1, "native");
  const dop = viaBridge(2, "dop", { rateHz: 5644800 });
  const conv = viaBridge(3, "converted", { rateHz: 11289600 });
  const none = viaBridge(4, "none", { rateHz: 22579200 });
  const bare = dsd(5);
  const w = (t) => playFor(t, "browser");
  const b = (t) => playFor(t, "bridge");
  check("this browser: a native row says the bridge's player plays it, at its rate, natively",
    !w(native).playable && w(native).why && w(native).chip === `${DSD_WORDS} · the bridge's player plays it (DSD64, native)`, w(native));
  check("a DoP row says so as DoP", !w(dop).playable && w(dop).chip === `${DSD_WORDS} · the bridge's player plays it (DSD128, DoP)`, w(dop));
  check("a converted row says converted to PCM, never bit-exact",
    !w(conv).playable && w(conv).chip === `${DSD_WORDS} · the bridge's player plays it (DSD256, converted to PCM)` && !/bit-exact/i.test(w(conv).chip), w(conv));
  check("a row the bridge's player refuses keeps the browser's words alone", !w(none).playable && w(none).chip === DSD_WORDS, w(none));
  check("a row with no bridge field keeps them too", !w(bare).playable && w(bare).chip === DSD_WORDS, w(bare));
  check("the bridge's player chosen: each row's chip as before",
    b(native).playable && b(native).chip === "DSD64, native" && b(dop).chip === "DSD128, DoP" && b(conv).chip === "DSD256, converted to PCM" &&
    !b(none).playable && b(none).chip === "A DSD rate the bridge's DAC does not take" && !b(bare).playable && b(bare).chip === "The bridge's player has not said what it plays",
    [b(native), b(dop), b(conv), b(none), b(bare)]);
  const all = [native, dop, conv, none, bare];
  let rows = await libraryRows(all, { id: "w1" });
  check("this browser's album page draws each row's words", JSON.stringify(rowChips(rows.view)) === JSON.stringify(all.map((t) => w(t).chip)), rowChips(rows.view));
  store.set(OUTPUT_KEY, "bridge");
  await pageRig();
  rows = await libraryRows(all, { id: "w2" });
  check("the bridge's player's album page draws its own", JSON.stringify(rowChips(rows.view)) === JSON.stringify(all.map((t) => b(t).chip)), rowChips(rows.view));
};

S.TestTheAlbumsCountLineSaysHowManyPlayWhere = async () => {
  const mixed = [flac(1, { bridge: { way: "pcm" } }), viaBridge(2, "native"), viaBridge(3, "dop", { rateHz: 5644800 }), viaBridge(4, "none", { rateHz: 22579200 })];
  const words = "3 of 4 can't play in a browser. The bridge's player plays 2 of them: choose it under Output, which says whether it can be chosen now. Download those instead.";
  const r = await barPage({ latest: pub() });
  r.find("Output: this browser")?.click();
  await settle();
  const pick = () => findAll(r.body, (n) => n.classList.contains("np-pick") && n.textContent.startsWith("The bridge's player"))[0] || null;
  for (const [name, st] of [["the bridge's player free", null], ["held by the DSD check", pub({ revision: 2, hold: "check" })]]) {
    if (st) {
      r.frame(st);
      await settle();
    }
    const rows = await libraryRows(mixed, { id: st ? "n-held" : "n-free" });
    const { line } = albumParts(rows.view);
    check(`${name}: how many, and where they play`, line?.textContent === words, line?.textContent);
    const dl = line ? findAll(line, (n) => n.classList.contains("track-dl-note"))[0] : null;
    check(`${name}: Download offered in the words the page shows only where it shows the link`, dl?.textContent === " Download those instead.", dl?.textContent);
    const p = pick();
    check(`${name}: Output says whether it can be chosen now`,
      st ? p?.disabled === true && p.textContent.includes("The DSD check is playing on the DAC") : p?.disabled === false, [p?.disabled, p?.textContent]);
  }
  {
    const rows = await libraryRows([flac(1), dsd(2), dsd(3)], { id: "n-bare" });
    const { line } = albumParts(rows.view);
    check("rows the bridge's player has not described: no word of it, Download where its link shows",
      line?.textContent === "2 of 3 can't play in a browser. Download those instead.", line?.textContent);
  }
  store.set(OUTPUT_KEY, "bridge");
  await pageRig();
  const rows = await libraryRows(mixed, { id: "n-bridge" });
  const { line } = albumParts(rows.view);
  check("the bridge's player chosen: its count as before", line?.textContent === "1 of 4 can't play on the bridge's player.", line?.textContent);
};

S.TestAnAlbumNoRowOfWhichPlaysHereOffersNoPlayShuffleOrAdd = async () => {
  const r = await pageRig({ gate: true, latest: pub({ revision: 3, silent: true }) });
  r.s.on("POST", ROUTES.switchOut, async (e) => answer(recOf(e, { kind: "switch-out", silence: 4 }), pub({ revision: 4, silent: true })));
  const dsdOnly = [viaBridge(1, "native"), viaBridge(2, "native")];
  let rows = await libraryRows(dsdOnly, { id: "x1" });
  let p = albumParts(rows.view);
  check("no row plays in this browser: Play, Shuffle and Add to queue disabled", p.play?.disabled && p.shuffle?.disabled && p.add?.disabled, [p.play?.disabled, p.shuffle?.disabled, p.add?.disabled]);
  check("their reason in words the page shows, which each names", /^2 of 2 can't play in a browser\. The bridge's player plays them: choose it under Output/.test(p.line?.textContent || "") &&
    !!p.line?.getAttribute("id") && [p.play, p.shuffle, p.add].every((x) => x?.getAttribute("aria-describedby") === p.line.getAttribute("id")),
    [p.line?.textContent, p.line?.getAttribute("id"), p.play?.getAttribute("aria-describedby")]);
  p.play?.click();
  p.shuffle?.click();
  p.add?.click();
  await settle();
  check("a disabled Play sends no barrier and starts nothing", r.s.posts(ROUTES.switchOut).length === 0 && r.el.plays.length === 0 && r.audio.snapshot().queue.length === 0,
    [r.s.posts(ROUTES.switchOut).length, r.el.plays, r.audio.snapshot().queue.length]);
  // The same album with a fresh FLAC copy of its first row, which the queue plays.
  const copy = { kind: "none", contentType: "audio/x-dsf", variantId: "pcm-1", variantContentType: "audio/flac" };
  rows = await libraryRows([viaBridge(1, "native", { play: copy }), viaBridge(2, "native")], { id: "x2" });
  p = albumParts(rows.view);
  check("a row with a copy this browser plays is playable, by the queue's own decision",
    rows[0].getAttribute("aria-disabled") === null && rows[1].getAttribute("aria-disabled") === "true" && JSON.stringify(rowChips(rows.view)) === JSON.stringify(["", `${DSD_WORDS} · the bridge's player plays it (DSD64, native)`]),
    [rows[0].getAttribute("aria-disabled"), rowChips(rows.view)]);
  check("and its album's Play, Shuffle and Add to queue are live", !p.play?.disabled && !p.shuffle?.disabled && !p.add?.disabled && p.play?.getAttribute("aria-describedby") === null,
    [p.play?.disabled, p.shuffle?.disabled, p.add?.disabled]);
  p.play?.click();
  await settle();
  check("its Play asks the barrier and plays the copy", r.s.posts(ROUTES.switchOut).length === 1 && /variant=pcm-1/.test(r.el.plays.at(-1) || ""), [r.s.posts(ROUTES.switchOut).length, r.el.plays]);
  rows = await libraryRows([dsd(1), flac(2)], { id: "x3" });
  p = albumParts(rows.view);
  check("one PCM row beside DSD: all three live in this browser", !p.play?.disabled && !p.shuffle?.disabled && !p.add?.disabled, [p.play?.disabled, p.shuffle?.disabled, p.add?.disabled]);
  store.set(OUTPUT_KEY, "bridge");
  await pageRig();
  rows = await libraryRows(dsdOnly, { id: "x4" });
  p = albumParts(rows.view);
  check("the bridge's player chosen, which plays them: Play and Add to queue live, Shuffle off as before",
    !p.play?.disabled && !p.add?.disabled && p.shuffle?.disabled && p.shuffle?.title === WORDS.noShuffle && !p.line, [p.play?.disabled, p.add?.disabled, p.shuffle?.disabled, p.shuffle?.title, p.line?.textContent]);
  rows = await libraryRows([viaBridge(1, "none"), dsd(2)], { id: "x5" });
  p = albumParts(rows.view);
  check("rows the bridge's player does not play: all three disabled, the reason said",
    p.play?.disabled && p.shuffle?.disabled && p.add?.disabled && p.line?.textContent === "2 of 2 can't play on the bridge's player." && p.add?.getAttribute("aria-describedby") === p.line.getAttribute("id"),
    [p.play?.disabled, p.add?.disabled, p.line?.textContent]);
};

S.TestAnOutputSwitchRedrawsAnAlbumsRowsLineAndControls = async () => {
  const r = await pageRig();
  const tracks = [viaBridge(1, "native"), viaBridge(2, "none", { rateHz: 22579200 })];
  const rows = await libraryRows(tracks, { id: "o1" });
  const view = rows.view;
  const views = await import(new URL("views.js", dirURL));
  // As boot.js wires it (wireOutput): the output picked redraws the route.
  let redrawn = null;
  listen(r.out, { win: window, doc: document, onOutput: () => { redrawn = views.renderAlbum(view, { id: "o1", gen: () => 1, setToolbar: () => {}, setCrumb: () => {}, trail: [] }); } });
  let p = albumParts(view);
  check("this browser: no row plays, the controls off, the line its words",
    p.play?.disabled && p.add?.disabled && /^2 of 2 can't play in a browser\. The bridge's player plays 1 of them/.test(p.line?.textContent || ""), [p.play?.disabled, p.line?.textContent]);
  await r.out.pick("bridge");
  await settle();
  await redrawn;
  p = albumParts(view);
  check("the switch to the bridge's player redraws the page", redrawn !== null && r.out.view().output === "bridge", r.out.view().output);
  check("its rows read the bridge's player's ways", JSON.stringify(rowChips(view)) === JSON.stringify(["DSD64, native", "A DSD rate the bridge's DAC does not take"]), rowChips(view));
  check("its line and its controls follow: Play and Add to queue live", !p.play?.disabled && !p.add?.disabled && p.shuffle?.disabled && p.line?.textContent === "1 of 2 can't play on the bridge's player.",
    [p.play?.disabled, p.add?.disabled, p.line?.textContent]);
};

S.TestTheBarCarriesItsSeekAndBothTimesAndSaysHowTallItIs = async () => {
  const observed = [];
  globalThis.ResizeObserver = class { constructor(fn) { this.fn = fn; } observe(n) { observed.push({ n, fn: this.fn }); } disconnect() {} };
  const r = await pageRig();
  const ticks = [];
  globalThis.setInterval = (fn) => { ticks.push(fn); return 0; };
  const props = {};
  document.documentElement.style.setProperty = (k, v) => { props[k] = v; };
  const np = await import(new URL("nowplaying.js", dirURL));
  np.mount();
  const bar = r.body.children.find((c) => c.attrs.id === "nowplaying");
  const one = (cls) => findAll(bar, (n) => n.classList.contains(cls));
  r.audio.playQueue([flac(1)], 0);
  r.el.metadata(200);
  r.el.currentTime = 42;
  await settle();
  ticks.forEach((fn) => fn());
  const scrub = one("np-scrub")[0];
  const seek = one("np-seek")[0];
  const times = one("np-time");
  check("the bar's scrub row holds the seek and both times", !!scrub && !!seek && scrub.contains(seek) && times.length === 2 && times.every((x) => scrub.contains(x)), [!!scrub, !!seek, times.length]);
  check("the times say where the track is and how long it is", times[0]?.textContent === "0:42" && times[1]?.textContent === "3:20", times.map((x) => x.textContent));
  check("the seek live for a source that seeks", seek?.tagName === "INPUT" && seek.getAttribute("type") === "range" && !seek.disabled, seek?.disabled);
  const watch = observed.find((o) => o.n === bar);
  bar.offsetHeight = 152;
  watch?.fn([]);
  check("the bar's height as it renders is handed to what sits above it", !!watch && props["--np-bar-h"] === "152px", [!!watch, props]);
};

// The seek where it is shown, wider than 768px, lands as it always has: at
// its change alone, never at an input or a pointer's lift, in either output.
S.TestTheSeekLandsOnlyAtItsChange = async () => {
  const r = await barPage();
  r.s.on("POST", ROUTES.replace, async (e) => answer(recOf(e, { kind: "replace", sent: 1, of: 1 }), playing({ revision: 4 })));
  r.s.on("POST", ROUTES.transport, async (e) => answer(recOf(e), playing({ revision: 4 })));
  const seek = r.byClass("np-seek");
  r.audio.playQueue([flac(1)], 0);
  r.el.metadata(200);
  await settle();
  let writes = 0;
  let at = r.el.currentTime;
  Object.defineProperty(r.el, "currentTime", { get: () => at, set: (v) => { writes += 1; at = v; }, configurable: true });
  const down = () => seek.fire("pointerdown", { pointerId: 1, isPrimary: true, pointerType: "mouse" });
  const up = () => seek.fire("pointerup", { pointerId: 1, isPrimary: true, pointerType: "mouse" });
  down();
  seek.value = "500";
  seek.fire("input");
  check("this browser: an input previews and seeks nothing", writes === 0, writes);
  up();
  check("this browser: the pointer's lift seeks nothing", writes === 0, writes);
  seek.fire("change");
  check("this browser: the change lands one seek, where the value was let go", writes === 1 && at === 100, [writes, at]);
  await r.out.pick("bridge");
  await settle();
  const seeks = () => r.s.posts(ROUTES.transport).filter((e) => e.body?.command === "seek").map((e) => e.body.seconds);
  down();
  seek.value = "500";
  seek.fire("input");
  up();
  await settle();
  check("the bridge's player: an input and the pointer's lift send no seek", r.out.view().output === "bridge" && seeks().length === 0, [r.out.view().output, seeks()]);
  seek.fire("change");
  await settle();
  check("the bridge's player: the change sends one seek, where the value was let go", JSON.stringify(seeks()) === "[150]", seeks());
};

// A preview no change has ended (a drag in a window wider than 768px that a
// resize to a phone's width cut short, the seek hidden from under it) holds
// nothing of the 2px rail, a phone's progress there: at each tick the rail
// is where the track is, in either output.
S.TestTheRailFollowsTheTrackPastAPreview = async () => {
  const r = await pageRig();
  r.s.on("POST", ROUTES.replace, async (e) => answer(recOf(e, { kind: "replace", sent: 1, of: 1 }), playing({ revision: 4 })));
  const ticks = [];
  globalThis.setInterval = (fn) => { ticks.push(fn); return 0; };
  const np = await import(new URL("nowplaying.js", dirURL));
  np.mount();
  const bar = r.body.children.find((c) => c.attrs.id === "nowplaying");
  const seek = findAll(bar, (n) => n.classList.contains("np-seek"))[0];
  const rail = findAll(bar, (n) => n.classList.contains("np-rail"))[0];
  const props = {};
  rail.style = { setProperty: (k, v) => { props[k] = v; }, removeProperty: (k) => { delete props[k]; } };
  r.audio.playQueue([flac(1)], 0);
  r.el.metadata(200);
  await settle();
  seek.fire("pointerdown", { pointerId: 1, isPrimary: true, pointerType: "mouse" });
  seek.value = "900";
  seek.fire("input");
  r.el.currentTime = 50;
  ticks[0]?.();
  check("this browser: at the next tick the rail is where the track is", ticks.length === 1 && props["--np-progress"] === "25%", [ticks.length, props["--np-progress"]]);
  r.el.currentTime = 100;
  ticks[0]?.();
  check("this browser: and it follows the track as it plays", props["--np-progress"] === "50%", props["--np-progress"]);
  await r.out.pick("bridge");
  await settle();
  r.c.advance(20000);
  ticks[0]?.();
  const p = r.out.position();
  check("the bridge's player: at the next tick the rail is where its track is",
    r.out.view().output === "bridge" && p > 0 && props["--np-progress"] === `${(p / 300) * 100}%`,
    [r.out.view().output, p, props["--np-progress"]]);
};

S.TestAnAlbumAsksThisBrowserWhatTheQueuePlays = async () => {
  const r = await pageRig({ gate: true, latest: pub({ revision: 3, silent: true }) });
  r.s.on("POST", ROUTES.switchOut, async (e) => answer(recOf(e, { kind: "switch-out", silence: 4 }), pub({ revision: 4, silent: true })));
  // A row the server calls universal whose type this browser's decoder does
  // not take, and no copy it does: the queue plays nothing of it.
  const ogg = flac(1, { path: "Album/01.ogg", codec: "Vorbis", play: { kind: "universal", contentType: "audio/ogg" } });
  let rows = await libraryRows([ogg], { id: "q1" });
  let p = albumParts(rows.view);
  check("a universal row this browser cannot decode: unplayable, its album's Play, Shuffle and Add to queue disabled",
    rows[0].getAttribute("aria-disabled") === "true" && p.play?.disabled && p.shuffle?.disabled && p.add?.disabled,
    [rows[0].getAttribute("aria-disabled"), p.play?.disabled, p.shuffle?.disabled, p.add?.disabled]);
  check("its reason said in the count line and the row's chip",
    /^1 of 1 can't play in a browser\./.test(p.line?.textContent || "") && rowChips(rows.view)[0] === "May not play in this browser",
    [p.line?.textContent, rowChips(rows.view)]);
  p.play?.click();
  await settle();
  check("its Play sends no barrier and plays nothing", r.s.posts(ROUTES.switchOut).length === 0 && r.el.plays.length === 0,
    [r.s.posts(ROUTES.switchOut).length, r.el.plays]);
  rows = await libraryRows([flac(2)], { id: "q2" });
  p = albumParts(rows.view);
  check("a row this browser decodes: playable, the album's controls live",
    rows[0].getAttribute("aria-disabled") === null && !p.play?.disabled && !p.shuffle?.disabled && !p.add?.disabled && !p.line,
    [rows[0].getAttribute("aria-disabled"), p.play?.disabled, p.line?.textContent]);
  // A row with no play field: the queue plays nothing of it, and its reason
  // is an element the page renders.
  rows = await libraryRows([{ path: "Album/03.flac", title: "T3", codec: "FLAC" }, flac(4)], { id: "q3" });
  const named = rows[0].getAttribute("aria-describedby");
  const reason = findAll(rows.view, (n) => n.getAttribute?.("id") === named)[0];
  check("a row with no play field: unplayable, its reason rendered where it points",
    rows[0].getAttribute("aria-disabled") === "true" && !!named && reason?.textContent === "Not playable in a browser",
    [rows[0].getAttribute("aria-disabled"), named, reason?.textContent]);
};

S.TestAnAlbumRedrawsWhenTheOutputFallsBackToThisBrowser = async () => {
  store.set(OUTPUT_KEY, "bridge");
  const r = await pageRig({ latest: playing({ revision: 3 }) });
  const rows = await libraryRows([flac(1)], { id: "f1" });
  const view = rows.view;
  const views = await import(new URL("views.js", dirURL));
  // As boot.js wires it (wireOutput): the output's change redraws the route.
  let redraws = 0;
  let redrawn = null;
  listen(r.out, { win: window, doc: document, onOutput: () => { redraws += 1; redrawn = views.renderAlbum(view, { id: "f1", gen: () => 1, setToolbar: () => {}, setCrumb: () => {}, trail: [] }); } });
  let p = albumParts(view);
  check("the bridge's player chosen: a row it says nothing of plays nowhere, the album's Play off",
    r.out.view().output === "bridge" && !r.out.view().notOffered && p.play?.disabled === true, [r.out.view().output, p.play?.disabled]);
  // GET now answers that no player is wired: this browser is the output now,
  // the pick of the bridge kept.
  r.s.on("GET", ROUTES.now, async () => ({ status: 409, body: { error: "player_not_wired", message: "no player" } }));
  await r.out.sampleNow();
  await settle();
  await redrawn;
  p = albumParts(view);
  const row = findAll(view, (n) => n.classList.contains("track-title"))[0];
  check("no player offered: the pick kept, this browser the output", r.out.view().notOffered && r.out.view().output === "bridge", r.out.view());
  check("the album redraws once for this browser: its FLAC row live, Play, Shuffle and Add to queue enabled",
    redraws === 1 && row?.getAttribute("aria-disabled") === null && p.play?.disabled === false && p.shuffle?.disabled === false && p.add?.disabled === false,
    [redraws, row?.getAttribute("aria-disabled"), p.play?.disabled, p.shuffle?.disabled, p.add?.disabled]);
};

S.TestAConfirmedDecodeFailureDisablesTheAlbumsHopelessPlay = async () => {
  const r = await pageRig({ gate: true, latest: pub({ revision: 3, silent: true }) });
  r.s.on("POST", ROUTES.switchOut, async (e) => answer(recOf(e, { kind: "switch-out", silence: 4 }), pub({ revision: 4, silent: true })));
  const t = longTimers();
  let rows = await libraryRows([flac(1)], { id: "d1" });
  rows[0].click();
  await settle();
  check("a start from the album: one barrier, one play", r.s.posts(ROUTES.switchOut).length === 1 && r.el.plays.length === 1, [r.s.posts(ROUTES.switchOut).length, r.el.plays]);
  // The album shown again after the queue started: its rows parsed afresh,
  // none of them an object the queue holds.
  rows = await libraryRows([flac(1)], { id: "d1" });
  const view = rows.view;
  // The byte probe of the source that fails: the bridge served it.
  const read = globalThis.fetch;
  globalThis.fetch = (url, init) => /^\/api\/player\/albums\//.test(String(url)) ? read(url, init) : Promise.resolve({ ok: true, status: 206, json: async () => ({}) });
  const views = await import(new URL("views.js", dirURL));
  let redrawn = null;
  let redraws = 0;
  // As boot.js wires it: a view's rerender is the route's (cut at three, so a
  // storm of them ends the case).
  window.addEventListener("player:rerender", () => {
    redraws += 1;
    if (redraws <= 3) redrawn = views.renderAlbum(view, { id: "d1", gen: () => 1, setToolbar: () => {}, setCrumb: () => {}, trail: [] });
  });
  r.el.fail(4);
  await settle();
  await redrawn;
  await settle();
  const p = albumParts(view);
  const row = findAll(view, (n) => n.classList.contains("track-title"))[0];
  check("the decode failure confirmed: the album redraws once, the row reads why, the album's Play, Shuffle and Add to queue disabled, the count line saying so",
    redraws === 1 && row?.getAttribute("aria-disabled") === "true" && rowChips(view)[0] === "Not playable in a browser" && p.play?.disabled && p.shuffle?.disabled && p.add?.disabled &&
    /^1 of 1 can't play in a browser\./.test(p.line?.textContent || ""), [redraws, row?.getAttribute("aria-disabled"), rowChips(view), p.play?.disabled, p.line?.textContent]);
  const barriers = r.s.posts(ROUTES.switchOut).length;
  const plays = r.el.plays.length;
  p.play?.click();
  await settle();
  check("its Play, with the skip held, sends no barrier and plays nothing", t.count(1200) === 1 && r.s.posts(ROUTES.switchOut).length === barriers && r.el.plays.length === plays,
    [t.count(1200), r.s.posts(ROUTES.switchOut).length - barriers, r.el.plays.length - plays]);
  t.fire(1200);
  await settle();
  check("the skip, let go, goes on as it did: through the gate, to the queue's end, nothing played",
    r.s.posts(ROUTES.switchOut).length === barriers + 1 && r.el.plays.length === plays && !r.audio.snapshot().playing,
    [r.s.posts(ROUTES.switchOut).length - barriers, r.el.plays.length - plays, r.audio.snapshot().playing]);
};

S.TestATapOnTheRailJumpsOnlyFromTheLetterItBeganOn = async () => {
  page();
  const ui = await import(new URL("ui.js", dirURL));
  const jumps = [];
  const rail = ui.alphabetRail([{ key: "A", offset: 1, count: 3 }, { key: "M", offset: 14, count: 2 }, { key: "Z", offset: 28, count: 1 }], (b) => jumps.push(b.key));
  const letter = (k) => rail.children.find((n) => n.textContent === k);
  const empty = rail.children.filter((n) => n.hasAttribute("disabled"));
  // A touch as WebKit delivers it: its pointerdown on the letter under the
  // finger, which the rail hears, then the click: a live letter's own, and an
  // empty letter's handed by touch adjustment to the nearest live one.
  const tap = (under, clicked) => {
    rail.fire("pointerdown", { target: under, pointerType: "touch", pointerId: 7, isPrimary: true });
    clicked?.fire("click", { detail: 1 });
  };
  for (const k of ["A", "M", "Z"]) tap(letter(k), letter(k));
  check("a tap on each live letter asks for its bucket once", JSON.stringify(jumps) === '["A","M","Z"]', jumps);
  const near = (n) => { const i = rail.children.indexOf(n); return letter(i < 7 ? "A" : (i < 21 ? "M" : "Z")); };
  for (const n of empty) tap(n, near(n));
  check("a tap on each of the 24 empty letters asks for nothing, whatever live letter its click is handed",
    empty.length === 24 && jumps.length === 3, [empty.length, jumps]);
  // No suppression left over: a key on a focused live letter, right after an
  // empty tap whose click went elsewhere and after one that made no click.
  tap(letter("B"), letter("A"));
  letter("A").fire("click", { detail: 0 });
  tap(letter("N"), null);
  letter("M").fire("click", { detail: 0 });
  rail.fire("pointerdown", { target: letter("Z"), pointerType: "mouse", pointerId: 1, isPrimary: true });
  letter("Z").fire("click", { detail: 1 });
  check("a key's click right after an empty tap, and a mouse's, each ask once", JSON.stringify(jumps) === '["A","M","Z","A","M","Z"]', jumps);
  // A touch that began beside the rail, its click handed to a live letter
  // after an empty tap's was refused: the letter's, as it was.
  tap(letter("B"), letter("A"));
  letter("A").fire("click", { detail: 1 });
  check("a click no touch on the rail began, after an empty tap's, asks once", JSON.stringify(jumps.slice(6)) === '["A"]', jumps.slice(6));
};

S.TestADecodeFailureClosesTheAlbumThatStartedIt = async () => {
  const r = await pageRig({ gate: true, latest: pub({ revision: 3, silent: true }) });
  r.s.on("POST", ROUTES.switchOut, async (e) => answer(recOf(e, { kind: "switch-out", silence: 4 }), pub({ revision: 4, silent: true })));
  const t = longTimers();
  const rows = await libraryRows([flac(1)], { id: "s1" });
  const view = rows.view;
  const views = await import(new URL("views.js", dirURL));
  let redraws = 0;
  let redrawn = null;
  // As boot.js wires it (cut at three, so a storm of them ends the case).
  window.addEventListener("player:rerender", () => {
    redraws += 1;
    if (redraws <= 3) redrawn = views.renderAlbum(view, { id: "s1", gen: () => 1, setToolbar: () => {}, setCrumb: () => {}, trail: [] });
  });
  // The byte probe of the source that fails: the bridge served it.
  const read = globalThis.fetch;
  globalThis.fetch = (url, init) => /^\/api\/player\/albums\//.test(String(url)) ? read(url, init) : Promise.resolve({ ok: true, status: 206, json: async () => ({}) });
  albumParts(view).play?.click();
  await settle();
  check("the album's Play: one barrier, one play", r.s.posts(ROUTES.switchOut).length === 1 && r.el.plays.length === 1, [r.s.posts(ROUTES.switchOut).length, r.el.plays]);
  // A network error's retry first, then the decode failure the probe confirms.
  r.el.fail(2);
  await settle();
  r.el.metadata(200);
  await settle();
  r.el.fail(4);
  await settle();
  await redrawn;
  await settle();
  let p = albumParts(view);
  const row = findAll(view, (n) => n.classList.contains("track-title"))[0];
  check("the failure confirmed after a retry: the page that started it redraws once, the row reads why, the count line says so, Play, Shuffle and Add to queue disabled",
    redraws === 1 && row?.getAttribute("aria-disabled") === "true" && rowChips(view)[0] === "Not playable in a browser" &&
    /^1 of 1 can't play in a browser\./.test(p.line?.textContent || "") && p.play?.disabled === true && p.shuffle?.disabled === true && p.add?.disabled === true,
    [redraws, row?.getAttribute("aria-disabled"), rowChips(view), p.line?.textContent, p.play?.disabled, p.shuffle?.disabled, p.add?.disabled]);
  const barriers = r.s.posts(ROUTES.switchOut).length;
  const plays = r.el.plays.length;
  p.play?.click();
  await settle();
  check("its Play, with the skip held, sends no barrier and plays nothing", t.count(1200) === 1 && r.s.posts(ROUTES.switchOut).length === barriers && r.el.plays.length === plays,
    [t.count(1200), r.s.posts(ROUTES.switchOut).length - barriers, r.el.plays.length - plays]);
  r.el.fire("timeupdate");
  r.audio.setShuffle(false);
  await settle();
  check("no redraw for the snapshots after it", redraws === 1, redraws);
  window.dispatchEvent(new Event("pagehide"));
  const stored = JSON.parse(store.get("bridge-player-queue") || "null");
  check("the stored queue keeps the row's play as the catalog sent it, so a reload plays it again",
    stored?.queue?.[0]?.play?.kind === "universal", stored?.queue?.[0]?.play);
  t.fire(1200);
  await settle();
  p = albumParts(view);
  check("the skip, let go, goes on as it did: through the gate, to the queue's end, nothing played",
    r.s.posts(ROUTES.switchOut).length === barriers + 1 && r.el.plays.length === plays && !r.audio.snapshot().playing && redraws === 1,
    [r.s.posts(ROUTES.switchOut).length - barriers, r.el.plays.length - plays, r.audio.snapshot().playing, redraws]);
  check("on this page the source stays refused: the row reads why and the album's Play is disabled",
    rowChips(view)[0] === "Not playable in a browser" && p.play?.disabled === true, [rowChips(view), p.play?.disabled]);
  // A reload over the queue the page stored: the failures are the page's
  // own, so the new page asks for the same source again.
  const failedSrc = r.el.plays[0];
  const st = pub({ revision: 5, silent: true });
  const d = await reloaded((s) => s.on("POST", ROUTES.switchOut, async (e) => answer(recOf(e, { kind: "switch-out", silence: 4 }), st)), st);
  const restored = d.audio.snapshot();
  check("the reload restores the row, paused, the same source on its element",
    restored.track?.path === "Album/01.flac" && !restored.playing && d.el.src === failedSrc && d.el.plays.length === 0,
    [restored.track?.path, restored.playing, d.el.src, failedSrc, d.el.plays]);
  d.audio.toggle();
  await settle();
  check("its Play asks the barrier and plays that source again",
    d.s.posts(ROUTES.switchOut).length === 1 && d.el.plays.length === 1 && d.el.plays[0] === failedSrc,
    [d.s.posts(ROUTES.switchOut).length, d.el.plays, failedSrc]);
};

S.TestAFailedVariantIsThatSourceAlone = async () => {
  const r = await pageRig({ gate: true, latest: pub({ revision: 3, silent: true }) });
  r.s.on("POST", ROUTES.switchOut, async (e) => answer(recOf(e, { kind: "switch-out", silence: 4 }), pub({ revision: 4, silent: true })));
  longTimers();
  const viaVariant = (v) => dsd(1, { play: { kind: "none", contentType: "audio/x-dsf", variantId: v, variantContentType: "audio/flac" } });
  let rows = await libraryRows([viaVariant("A")], { id: "v1" });
  const view = rows.view;
  const views = await import(new URL("views.js", dirURL));
  let redraws = 0;
  let redrawn = null;
  window.addEventListener("player:rerender", () => {
    redraws += 1;
    if (redraws <= 3) redrawn = views.renderAlbum(view, { id: "v1", gen: () => 1, setToolbar: () => {}, setCrumb: () => {}, trail: [] });
  });
  const read = globalThis.fetch;
  globalThis.fetch = (url, init) => /^\/api\/player\/albums\//.test(String(url)) ? read(url, init) : Promise.resolve({ ok: true, status: 206, json: async () => ({}) });
  albumParts(view).play?.click();
  await settle();
  check("the DSD row plays its browser variant A", r.el.plays.length === 1 && /variant=A$/.test(r.el.src), [r.el.plays.length, r.el.src]);
  r.el.fail(4);
  await settle();
  await redrawn;
  await settle();
  let p = albumParts(view);
  check("variant A confirmed undecodable: the album's Play disabled", redraws === 1 && p.play?.disabled === true, [redraws, p.play?.disabled]);
  // A fresh answer for the same path whose variant is now B, a source of its
  // own; the queue still holds the row whose A failed.
  rows = await libraryRows([viaVariant("B")], { id: "v1" });
  p = albumParts(rows.view);
  check("a fresh answer naming variant B: the album's Play enabled", p.play?.disabled === false && rows[0].getAttribute("aria-disabled") === null,
    [p.play?.disabled, rows[0].getAttribute("aria-disabled")]);
  p.play?.click();
  await settle();
  check("pressing it loads B", r.el.plays.length === 2 && /variant=B$/.test(r.el.src), [r.el.plays.length, r.el.src]);
};

S.TestTheRailTakesEveryPointerDownForItsClick = async () => {
  const pg = page();
  const ui = await import(new URL("ui.js", dirURL));
  const jumps = [];
  const rail = ui.alphabetRail([{ key: "A", offset: 1, count: 3 }, { key: "M", offset: 14, count: 2 }, { key: "Z", offset: 28, count: 1 }], (b) => jumps.push(b.key));
  pg.body.appendChild(rail);
  const letter = (k) => rail.children.find((n) => n.textContent === k);
  const outside = document.createElement("div");
  pg.body.appendChild(outside);
  // An empty tap that makes no click; a touch that goes down outside the
  // rail; the click WebKit hands that touch's tap, on a live letter.
  letter("B").fire("pointerdown", { pointerType: "touch", pointerId: 7, isPrimary: true });
  outside.fire("pointerdown", { pointerType: "touch", pointerId: 8, isPrimary: true });
  letter("A").fire("click", { detail: 1 });
  check("an empty tap that made no click, then a click a touch outside the rail is handed: one jump", JSON.stringify(jumps) === '["A"]', jumps);
  // A touch held on an empty letter, a key's click on a live one, then the
  // touch's click handed to another live letter.
  letter("N").fire("pointerdown", { pointerType: "touch", pointerId: 9, isPrimary: true });
  letter("A").fire("click", { detail: 0 });
  letter("M").fire("click", { detail: 1 });
  check("a key's click beside a held touch jumps, and that touch's handed click does not", JSON.stringify(jumps) === '["A","A"]', jumps);
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
