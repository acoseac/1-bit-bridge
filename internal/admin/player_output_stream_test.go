package admin

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// The event stream's lifecycle as the player bar hears it (B634, PR 2,
// P8): app.js's own startEventStream and recycleEventStream, assembled out
// of the shipped app.js, over a stand-in EventSource, hand the shipped
// output.js (through its own listen) a connection serial at every open, a
// native reconnection of one EventSource included, an error as a connection
// ends, a replacement during a boosted navigation included, and each player
// frame with the serial it came on; and drop what a retired EventSource
// still dispatches. The clock's samples asked on a connection the bar has
// left are dropped whole.
const streamLifecycleHarness = `
import { readFile } from "node:fs/promises";
const input = JSON.parse(await readFile(process.argv[2], "utf8"));
const R = { pass: [], fail: [] };
const check = (name, cond, detail) => { if (cond) R.pass.push(name); else R.fail.push({ name, detail: JSON.stringify(detail === undefined ? "" : detail) }); };
const settle = async () => { for (let i = 0; i < 12; i++) await new Promise((r) => setTimeout(r, 0)); };
const gate = () => { let open; const p = new Promise((r) => { open = r; }); return { p, open }; };

// What app.js's stream code reads at the top level, and its two helpers it
// needs nothing of here (stubs).
globalThis.window = new EventTarget();
globalThis.document = { querySelectorAll: () => [], addEventListener() {}, visibilityState: "visible" };
let activeEventSource = null;
let lastEventSourceSeenAt = 0;
let playerStreamSerial = 0;
const SSE_RECONNECT_IDLE_THRESHOLD_MS = 60000;
function applyConnState() {}
function safeApply() {}

const sources = [];
globalThis.EventSource = class {
  static CLOSED = 2;
  constructor(url) { this.url = url; this.readyState = 0; this.listeners = {}; sources.push(this); }
  addEventListener(t, fn) { (this.listeners[t] ||= []).push(fn); }
  close() { this.readyState = 2; }
  open() { this.readyState = 1; if (this.onopen) this.onopen(); }
  error() { this.readyState = 0; if (this.onerror) this.onerror(); }
  message(t, data) { for (const fn of this.listeners[t] || []) fn({ data }); }
};

const { createOutput, listen, ROUTES, CLOCK_HEADER } = await import(input.output);
const E1 = "e1".repeat(16);
const T0 = Date.parse("2026-11-23T10:00:00.000Z");
let now = T0 + 2000;
let mono = 1000;
const state = (rev) => ({ epoch: E1, revision: rev, at: new Date(T0).toISOString(), running: true, offered: true, hold: "none",
  release: { state: "confirmed" }, commands: ["stop", "pause"], silent: false, transport: "playing", generation: "0123456789abcdef.1",
  queueRev: 1, track: { entry: 1, duration: 300 }, anchor: { id: 1, occurrence: 1, entry: 1, seconds: 10, at: new Date(T0).toISOString(), advancing: true }, operations: [] });
const asks = [];
const fetch = async (url) => {
  const path = url.split("?")[0];
  if (path === ROUTES.now) {
    const a = { g: gate(), at: now, late: false };
    asks.push(a);
    await a.g.p;
    return { ok: true, status: 200, headers: { get: (n) => (n === CLOCK_HEADER ? new Date(a.late ? a.at + 50000 : a.at).toISOString() : null) }, json: async () => state(5) };
  }
  return { ok: true, status: 200, headers: { get: () => null }, json: async () => ({ epoch: E1, readAt: 5, queueRev: 1, entries: [] }) };
};
const store = new Map();
const o = createOutput({
  fetch, storage: { getItem: (k) => store.get(k) ?? null, setItem: (k, v) => store.set(k, v), removeItem: (k) => store.delete(k) },
  random: (b) => { for (let i = 0; i < b.length; i++) b[i] = i; }, now: () => now, perf: () => mono, browser: null,
});
listen(o, { win: window, doc: document });
const releaseLate = async (from, to) => { for (const a of asks.slice(from, to)) { a.late = true; a.g.open(); } await settle(); };
const releaseRest = async (from) => { for (const a of asks.slice(from)) a.g.open(); await settle(); };

// A native reconnection on one EventSource: every open a connection of its own.
startEventStream();
const es1 = sources[0];
es1.open();
es1.message("player", JSON.stringify(state(5)));
await settle();
const beforeReconnect = asks.length;
es1.error();
check("an error of the connection says the bar reconnects", o.view().reconnecting === true, o.view().reconnecting);
es1.open();
es1.message("player", JSON.stringify(state(5)));
await settle();
check("the native reconnection's open asks the clock again", asks.length > beforeReconnect, asks.length);
await releaseLate(0, beforeReconnect);
check("the clock asked on the connection it left is dropped, however late it lands", o.view().sampled === false, o.view().sampled);
await releaseRest(beforeReconnect);
check("the new connection's own sample lands", o.view().sampled && Math.abs(o.position() - 12) < 0.01, [o.view().sampled, o.position()]);

// A replacement during a boosted navigation (recycleEventStream), with a
// sample asked on the old connection still out.
const held = asks.length;
o.sampleNow();
await settle();
recycleEventStream();
const es2 = sources[1];
check("the replaced connection's end is heard", o.view().reconnecting === true, o.view().reconnecting);
es1.message("player", JSON.stringify(state(99)));
await settle();
check("a frame the retired EventSource still dispatches is dropped", o.view().state.revision !== 99, o.view().state.revision);
const beforeOpen = asks.length;
es2.open();
es2.message("player", JSON.stringify(state(6)));
await settle();
check("the replacement's first frame taken", o.view().state.revision === 6 && o.view().reconnecting === false, o.view().state.revision);
await releaseLate(held, beforeOpen);
check("the sample asked on the replaced connection is dropped", o.view().sampled === false, o.view().sampled);
await releaseRest(beforeOpen);
check("the replacement's own sample lands", o.view().sampled && Math.abs(o.position() - 12) < 0.01, [o.view().sampled, o.position()]);
console.log(JSON.stringify(R));
process.exit(0);
`

// TestTheStreamsLifecycleReachesTheBar pins P8 through the shipped app.js
// and output.js: a native reconnection on one EventSource and a replacement
// during a boosted navigation each give the bar a connection of its own,
// with a late clock response of the connection it left dropped, and a frame
// a retired EventSource still dispatches dropped.
func TestTheStreamsLifecycleReachesTheBar(t *testing.T) {
	node := barNode(t)
	src := readConsoleJS(t, "static/app.js")
	script := withReachedFunctions(t, streamLifecycleHarness, src, "startEventStream", "recycleEventStream")
	for _, fn := range []string{"function startEventStream(", "function recycleEventStream(", "function playerStreamEvent(", "function playerFrame("} {
		if !strings.Contains(script, fn) {
			t.Fatalf("%s is not in app.js: the case runs nothing of the shipped stream", fn)
		}
	}
	out, err := filepath.Abs(filepath.Join("static", "player", "output.js"))
	if err != nil {
		t.Fatal(err)
	}
	raw := runScriptUnderNode(t, node, script, map[string]string{"output": fileURL(out)})
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	var r barResult
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &r); err != nil {
		t.Fatalf("node answered %s: %v", raw, err)
	}
	for _, f := range r.Fail {
		t.Errorf("%s: %s", f.Name, f.Detail)
	}
	if len(r.Fail) == 0 && len(r.Pass) < 9 {
		t.Errorf("%d checks passed of 9: the case has stopped checking something (%v)", len(r.Pass), r.Pass)
	}
}
