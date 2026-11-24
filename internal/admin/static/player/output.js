// The player bar's output (B634): this browser, or the bridge's own player.
//
// The bar plays to one of two outputs. With this browser chosen it is the
// web player it has always been. With the bridge's player chosen it drives
// the player's engine through the console's control routes
// (/api/output/player/…) and follows it on the console's event stream,
// whose `player` frames app.js hands on as `bridge:player` window events.
//
// Everything here holds to the design (ops/plan-b634-console-output-design.md)
// and is pinned by internal/admin/player_output_bar_test.go, which runs this
// module under node:
//
// - Every start of the browser goes through one barrier: the console's own
//   Pause and the bridge's silence published after it (POST switch-out). The
//   browser starts on the `done` the barrier answers to this tab NOW, and on
//   nothing else: never on a refusal's publication, a Pause's answer or a
//   record read back, which is history (§3.2, G1, G8).
// - What playback does by itself goes on only through gateContinue (round 3,
//   S1): the next track at a track's end and the retry after an error on
//   their start's permit, asked again; a skip past a track that cannot play,
//   a repeat-one replay and a removed row's successor as a start is asked.
// - Every command is an operation named by this document's tab, its count and
//   the epoch it was sent in, stored before its request goes. It is resolved
//   by its record (the answer, the stream's last eight, or a read back), never
//   by a change of the engine's state, and never sent again (§2.3, G3, G5).
//   A document takes a tab identity of its own; the identity session storage
//   carried in is read only to resolve the operation it inherited (M2, P2).
// - Epochs move only forward: a new one is taken from a stream connection's
//   first frame alone; a state of another epoch is held while the stream
//   reconnects and dropped otherwise; within an epoch the higher revision
//   wins (§4.2, G2). A revision of 0 is no state (M5).
// - The queue is read against the publication that says when it last changed
//   (queueRev), at the first state of an epoch whatever the token reads, and
//   an older read never wins over a newer frame (§4.2, G4).
// - The position is the anchor's own: its seconds, advanced from its own
//   time by the offset between this browser's clock and the server's, taken
//   from GET now's X-Bridge-Clock samples alone (the least received less
//   sent of the current epoch, connection and sampling generation), never
//   from a publication's capture time, which the latest publication carries
//   however old it is (§4.4, M1, P7). Until a sample lands it shows the
//   anchor's seconds.
//
// The module has no side effect at import: boot.js builds the one instance
// over the browser's own fetch, storage, clocks and audio.js.

export const OUTPUT_KEY = "bridge-player-output";
export const OP_KEY = "bridge-player-op";
export const CLOCK_HEADER = "X-Bridge-Clock";

export const ROUTES = {
  now: "/api/output/player/now",
  queue: "/api/output/player/queue",
  operation: "/api/output/player/operation",
  replace: "/api/output/player/queue",
  append: "/api/output/player/queue/append",
  remove: "/api/output/player/queue/delete",
  transport: "/api/output/player/transport",
  switchOut: "/api/output/player/switch-out",
  status: "/api/output/player",
};

/** How often an unsampled clock is asked again, at most. */
export const SAMPLE_RETRY_MS = 5000;
/** How often GET now is read while the stream reconnects. */
export const RECONNECT_POLL_MS = 2000;
/** How often an unconfirmed operation's record is read back, at most. */
export const READ_BACK_MS = 2000;
/** A client clock that jumps by more than this against the monotonic one. */
export const CLOCK_JUMP_MS = 1000;
/** A request unanswered this long is lost: its operation unconfirmed (§7.2, R6). */
export const REQUEST_MS = 30000;
/** How long a start of this browser waits to know whether a player is offered. */
export const START_WAIT_MS = 10000;
/** How often a queue that is behind its state is read again, at most (C4). */
export const QUEUE_RETRY_MS = 2000;

/** opKey names an operation by its epoch, its tab and its count (V3). */
export function opKey(epoch, tab, seq) {
  return `${epoch}/${tab}/${seq}`;
}

/** The phases of §7.2. */
export const PHASE = {
  browser: "browser",
  toBridge: "to-bridge",
  bridge: "bridge",
  toBrowser: "to-browser",
  unconfirmed: "unconfirmed",
};

// The answers a request gets that say no player is offered here (K1): the
// bar then plays in the browser as it always has.
const NOT_OFFERED = new Set(["player_not_wired", "player_not_available", "player_not_supported", "player_not_offered"]);

// The structured refusals the console answers before a command becomes an
// operation, with no record: proof it never ran (R2, V2, V8). From the code:
// the not-offered words (controlRefusal); the body's bounds and shapes
// (decodeControlBody, checkTracks, the routes' own checks: body_too_large,
// bad_json, too_many_tracks, bad_track, bad_operation, bad_generation,
// bad_command); the catalog's and a playlist's read before the operation
// (expandTracks: catalog_unavailable, internal); csrfGuard's cross_origin;
// and the control's admission (admit: epoch_changed, replayed,
// operation_conflict, and player_not_running for a control that has
// closed). Any other answer without a record, a network failure or a body
// that is not JSON proves nothing: the operation stays unconfirmed, for its
// record to say how it ended.
const NEVER_RAN = new Set([
  ...NOT_OFFERED,
  "body_too_large", "bad_json", "too_many_tracks", "bad_track", "bad_operation", "bad_generation", "bad_command",
  "catalog_unavailable", "internal", "cross_origin",
  "epoch_changed", "replayed", "operation_conflict", "player_not_running",
]);

// ---- The bar's words ----

/** The promise the bar's help states word for word (§3.3). */
export const PROMISE = "The console never starts this browser while it knows the bridge's player plays. " +
  "Another controller can still start the bridge's player after the console's last check, and the bar then shows the bridge playing.";

export const WORDS = {
  toBridge: "Moving to the bridge's player…",
  toBrowser: "Moving to this browser…",
  unconfirmed: "The bridge's player has not said how the last command ended: this is what it is doing",
  reconnecting: "Reconnecting to the bridge…",
  bridgeToo: "The bridge's player is playing too: another controller started it",
  bridgePlaying: "The bridge's player is playing: this browser starts nothing beside it",
  bridgeAgain: "The bridge's player is playing again: a controller started it after the move, so this browser starts nothing",
  wentOn: "The bridge's player went on playing: another controller started it",
  stale: "The player changed since this page last read it",
  pickAgain: "The bridge's player has not said how the move ended: this is what it is doing; pick the output again",
  epochChanged: "The bridge has restarted since this page took its state: the command did not run",
  gone: "The bridge no longer keeps this command's record: it keeps the last command of the 64 tabs that sent one most recently, and each tab's count for the 4,096 most recent",
  conflict: "Another tab sent a command under this tab's identity: this tab takes an identity of its own, so press again",
  positionStayed: "The position stayed in the browser: the bridge's player starts the track from its start",
  noSeekDSD: "The bridge's player does not seek within DSD",
  noShuffle: "The bridge's player plays the queue in order",
  noRepeatOne: "The bridge's player repeats the whole queue or nothing",
  fixedLevel: "Fixed level: set it on the DAC or the amplifier",
  nothing: "Nothing playing",
  startWaits: "Waiting to hear from the bridge's player before this browser plays…",
  startUnanswered: "The bridge's player has not answered: press play again",
  startAgain: "This browser did not start: press Play again",
  pressPlay: "The move to this browser finished before this page loaded: press Play to start it here",
  queueBehind: "The bridge's queue is being read again: pick an entry once it shows",
};

/** Why the bridge's player cannot be picked, from the state (§7.1). */
const WHY = {
  off: "No DAC was bound when the bridge started, or its player is off",
  starting: "The bridge's player is starting",
  start_failed: "The bridge's player did not start",
  closed: "The bridge's player has stopped",
  output_unknown: "The player has not heard yet whether its DAC is there",
  output_unavailable: "The DAC is not connected",
  device_missing: "The DAC is not connected",
  device_ambiguous: "More than one DAC matches the one chosen",
  identity_transient: "The DAC is being plugged in or out",
  disabled: "The bridge's player is turned off",
  binding_changed: "Another DAC is chosen: it plays once the bridge restarts",
};

/** The line a hold says (§4.3, §8). */
const HOLD = {
  check: "The DSD check is playing on the DAC: Stop ends it",
  maintenance: "The player is held for maintenance",
  configuring: "The player is saving its DSD choice",
};

/** What a refused operation says, by the server's word (§5.3, §7.4). */
const REFUSED = {
  stale: "The player changed since this page last read it: nothing was changed",
  check: "The DSD check is playing on the DAC: Stop ends it",
  maintenance: "The player is held for maintenance",
  configuring: "The player is saving its DSD choice: nothing starts until it has",
  tone: "The test tone is playing on the DAC",
  release_unconfirmed: "The DAC has not confirmed it is free: pick this browser again once it has",
  output_unknown: "The player has not heard yet whether its DAC is there",
  output_unavailable: "The DAC is not available",
  not_playable: "No track of this plays on the bridge's player",
  queue_full: "The queue has no room for these tracks",
  empty_queue: "The queue is empty",
  entry_not_found: "The queue no longer holds that entry",
  seek_unsupported: "The bridge's player does not seek within DSD",
  seek_unavailable: "There is no track to seek in",
  seek_out_of_range: "That position is past the track's end",
  player_not_running: "The bridge's player does not run",
  edits_refused: "The player takes no edits now: its IDs could not be reserved durably",
  not_found: "The library holds no such album or playlist",
};

/** whyWord is the sentence for a state's word for why the bridge is not offered. */
export function whyWord(word) {
  if (!word) return "";
  return WHY[word] || `The DAC is not available (${word})`;
}

/** holdWord is the line a hold says, "" for none. */
export function holdWord(hold) {
  return HOLD[hold] || "";
}

/** refusedWord is what a refused operation's word says. */
export function refusedWord(word) {
  if (!word) return "The bridge's player refused the command";
  return REFUSED[word] || `The bridge's player refused the command (${word})`;
}

/** What a transport command is called in a line. */
const COMMAND_WORDS = {
  play: "Play", pause: "Pause", stop: "Stop", next: "Next", previous: "Previous",
  index: "Playing that entry", seek: "The seek", repeat: "Repeat",
};

/**
 * recordLine is what an operation's final record says, "" for done: its
 * word (a player that does not run with why, an engine's own word where the
 * table has none), how far a cancelled one got, its command's name.
 */
export function recordLine(rec) {
  if (!rec || rec.status === "done" || rec.status === "running") return "";
  let words;
  if (rec.status === "cancelled") {
    words = `It stopped after ${rec.sent} of ${rec.of} steps: a newer command came, or the page that sent it left`;
  } else if (rec.status === "unknown") {
    words = WORDS.pickAgain;
  } else if (rec.status === "changed") {
    words = "Another controller changed the queue or the transport while it ran: this is what is there";
  } else if (rec.error === "player_not_running") {
    words = `${refusedWord(rec.error)}: ${whyWord(rec.reason) || "it is not running"}`;
  } else if (rec.error === "player_refused") {
    words = refusedWord(rec.last);
  } else {
    words = refusedWord(rec.error);
  }
  const name = rec.kind === "transport" ? COMMAND_WORDS[rec.command] : "";
  return name ? `${name}: ${words}` : words;
}

/** leftOutLine says what a replace or an append left out, "" for nothing. */
export function leftOutLine(rec, total) {
  const out = rec?.leftOut || [];
  if (!out.length) return "";
  const words = new Map();
  const names = [];
  for (const t of out) {
    words.set(t.word, (words.get(t.word) || 0) + 1);
    if (names.length < 3) names.push(t.path.split("/").pop());
  }
  const parts = [...words].map(([w, n]) => `${n} ${LEFT_OUT[w] || `left out (${w})`}`);
  const more = out.length > names.length ? ", …" : "";
  return `${out.length} of ${total || out.length} tracks are left out: ${parts.join(", ")} (${names.join(", ")}${more})`;
}

const LEFT_OUT = {
  routed: "on another server",
  not_in_library: "not in this library",
  unknown_container: "a WavPack or Monkey's Audio file",
  lossy_refused: "lossy",
  sacd_virtual_track: "an SACD image's track",
  dsd_rate_not_played: "a DSD rate the DAC does not take",
  metadata_too_large: "tagged past what the player keeps",
  reference_too_long: "named past what the player keeps",
  not_a_container: "not a file the player plays",
};

/** windowLine says what a windowed replace queued, "" where nothing was cut. */
export function windowLine(rec) {
  const w = rec?.window;
  if (!w) return "";
  const n = w.last - w.first + 1;
  return `The bridge's player holds ${n.toLocaleString("en")} tracks: tracks ${w.first.toLocaleString("en")} to ${w.last.toLocaleString("en")} of these ${w.total.toLocaleString("en")} are queued`;
}

/**
 * releaseLine says a replace without play whose release passed its
 * deadline: the queue is in place, the DAC not confirmed free (G10).
 */
export function releaseLine(rec) {
  return rec?.release === "unconfirmed" ? "The queue is in place, and the DAC has not confirmed it is free" : "";
}

/** roomLine says what an append that did not fit found room for. */
export function roomLine(rec) {
  const room = rec?.room;
  if (!room) return "";
  if (room.entries >= room.wanted) {
    return `The queue's tags have room for ${room.metadata} more bytes, fewer than these ${room.wanted} tracks need`;
  }
  return `The queue has room for ${room.entries} more tracks; this has ${room.wanted}`;
}

/**
 * plannedOf is the tracks an edit meant to queue, from its record: an
 * append's steps, a replace's less its DeleteAll and, with play, its PlayID.
 */
export function plannedOf(rec, play) {
  if (rec?.kind === "append") return rec.of || 0;
  return Math.max(0, (rec?.of || 0) - 1 - (play ? 1 : 0));
}

/**
 * partialLine says how far an edit another controller interrupted got
 * (§2.7): the console's entries the queue holds, of the tracks it meant to
 * queue, and what the player is doing. Never rolled back: a retry is the
 * reader's, from the state the bar shows.
 */
export function partialLine(rec, st, total) {
  const held = (rec?.entries || []).length;
  const of = total > 0 ? total : held;
  const doing = st?.transport === "playing" ? "playing" : (st?.transport === "paused" ? "paused" : "stopped");
  const what = rec?.kind === "append" ? "adding to" : "replacing";
  return `The queue changed while the console was ${what} it: it holds ${held} of the ${of} tracks, and the player is ${doing}`;
}

// The one instance the page runs, which boot.js installs and the bar and the
// views read.
let installed = null;

/** installOutput makes o the page's output. */
export function installOutput(o) {
  installed = o;
}

/** currentOutput is the page's output, null before boot.js installs one. */
export function currentOutput() {
  return installed;
}

/** The output chosen now: "bridge" or "browser". */
export function chosenOutput() {
  const v = installed?.view();
  return v && v.output === "bridge" && !v.notOffered ? "bridge" : "browser";
}

/**
 * listen hands o what reaches a page for it: app.js's player frames
 * (bridge:player, with the serial of the connection they came on) and its
 * stream's lifecycle (bridge:player-stream: each open with its connection's
 * serial, each error), a page shown again (visibilitychange to visible,
 * pageshow from the cache), which forgets the clock's samples; and calls
 * onOutput when the output picked changes, which redraws the route in place.
 */
export function listen(o, { win, doc, onOutput }) {
  win.addEventListener("bridge:player", (e) => o.frame(e.detail?.data, e.detail?.serial));
  win.addEventListener("bridge:player-stream", (e) => {
    if (e.detail?.kind === "open") o.streamOpen(e.detail.serial);
    else if (e.detail?.kind === "error") o.streamError(e.detail.serial);
  });
  doc.addEventListener("visibilitychange", () => {
    if (doc.visibilityState === "visible") o.resume();
  });
  win.addEventListener("pageshow", (e) => {
    if (e.persisted) o.resume();
  });
  let last = o.view().output;
  o.subscribe((v) => {
    if (v.output === last) return;
    last = v.output;
    onOutput?.(v.output);
  });
}

/**
 * createOutput is the bar's output over env: fetch, storage (session),
 * random (fills a Uint8Array), now (the wall clock, ms), perf (a monotonic
 * clock, ms), setTimeout/clearTimeout, and browser, the web player as the
 * switch needs it (handover, suspend, resume, take, hasQueue, mediaSession).
 */
export function createOutput(env) {
  const listeners = new Set();
  // The order (§4.2).
  let epoch = "";
  let rev = 0;
  let state = null;
  let conn = 0;
  let connFirst = false;
  let reconnecting = false;
  let held = [];
  // The queue (G4).
  let queue = null;
  let queueReading = false;
  let queueAgain = false;
  let queueAnswered = 0;
  // The clock (M1, P7).
  let samplingGen = 0;
  let offset = null;
  let sampleInFlight = false;
  let lastSampleAsk = -Infinity;
  let drift = null;
  // Whether this box offers its player: null not known, true, false (K1),
  // from GET now's answer, which a box that offers none answers with its
  // typed refusal and no frame ever follows, or from a frame (V6).
  let offered = null;
  // The identity (M2, P2) and the operations not yet resolved, each keyed by
  // its epoch, its tab and its count (V3): this document's own (ops), and
  // those an earlier document left, which session storage carried in
  // (inherited, V9).
  let tab = "";
  let seq = 0;
  const ops = new Map();
  const inherited = new Map();
  let readBackAt = -Infinity;
  // The output and the phase (§7.2), and the count of the picks made here:
  // only a switch sent under the newest pick moves the output (R3).
  let output = readOutput();
  let phase = output === "bridge" ? PHASE.bridge : PHASE.browser;
  let line = "";
  let lastPoll = -Infinity;
  let pickGen = 0;
  // A start of this browser waiting to know whether a player is offered, and
  // the newest start the barrier in flight runs (R1, V7).
  let waiting = null;
  let startPending = null;
  let lastQueueAsk = -Infinity;

  const now = () => env.now();

  function emit() {
    const v = view();
    listeners.forEach((fn) => {
      try { fn(v); } catch { /* a listener's fault is its own */ }
    });
  }

  function readOutput() {
    try {
      return env.storage.getItem(OUTPUT_KEY) === "bridge" ? "bridge" : "browser";
    } catch {
      return "browser";
    }
  }

  function setOutput(o) {
    output = o;
    try { env.storage.setItem(OUTPUT_KEY, o); } catch { /* private mode */ }
    env.browser?.mediaSession?.(o !== "bridge");
  }

  // ---- The identity ----

  function newTab() {
    const b = new Uint8Array(16);
    env.random(b);
    return [...b].map((x) => x.toString(16).padStart(2, "0")).join("");
  }

  // storeIdentity keeps this document's identity and every operation not yet
  // resolved, its own and the ones it inherited, each by its key, so a
  // reload, or a second one, still reads each back (R7); and whether a newer
  // pick has superseded it (stale), so no reload lets it move the output
  // (S4).
  function storeIdentity() {
    const pending = {};
    for (const m of [inherited, ops]) {
      for (const [k, e] of m) {
        pending[k] = { epoch: e.epoch, tab: e.tab, seq: e.seq, role: e.role, kind: e.kind || "", handover: e.handover || null, stale: !ours(e) };
      }
    }
    try {
      env.storage.setItem(OP_KEY, JSON.stringify({ tab, seq, pending }));
    } catch { /* private mode: the operation still runs, a reload cannot read it back */ }
  }

  function bootIdentity() {
    let stored = null;
    try {
      stored = JSON.parse(env.storage.getItem(OP_KEY) || "null");
    } catch { stored = null; }
    const pending = stored && typeof stored.pending === "object" && stored.pending ? stored.pending : {};
    for (const p of Object.values(pending)) {
      if (!p || typeof p.tab !== "string" || !/^[0-9a-f]{32}$/.test(p.tab) || !(p.seq > 0) || typeof p.epoch !== "string") continue;
      // One a newer pick had superseded keeps a pick no pick here takes:
      // read back, it moves nothing (S4).
      const e = { key: opKey(p.epoch, p.tab, p.seq), epoch: p.epoch, tab: p.tab, seq: p.seq, role: p.role, kind: p.kind || "",
        handover: p.handover || null, inherited: true, pick: p.stale === true ? -1 : 0 };
      inherited.set(e.key, e);
    }
    // A document's own commands go under an identity of its own: two tabs
    // started from one session storage never share an operation (M2). The
    // operations it inherited stay stored until each resolves (R7); only
    // those no newer pick superseded hold the phase (Q4).
    tab = newTab();
    seq = 0;
    storeIdentity();
    if ([...inherited.values()].some(ours)) {
      phase = PHASE.unconfirmed;
      line = WORDS.unconfirmed;
    }
  }

  // freshIdentity is a new identity after operation_conflict, its count from
  // 0; the operations not yet resolved are kept, each by its own key (R7, V3).
  function freshIdentity() {
    tab = newTab();
    seq = 0;
    storeIdentity();
  }

  // ---- The order (§4.2) ----

  function parse(bytes) {
    if (bytes && typeof bytes === "object") return bytes;
    try { return JSON.parse(bytes); } catch { return null; }
  }

  // offer orders a state from source: "frame" (with its connection's
  // serial), "answer", "now" or "readback". It answers whether it was taken.
  function offer(st, source, serial) {
    if (!st || typeof st !== "object" || !st.epoch || !(st.revision > 0)) return false;
    if (source === "frame") {
      if (serial !== conn) return false;
      if (!connFirst) {
        connFirst = true;
        reconnecting = false;
        if (st.epoch !== epoch) {
          takeEpoch(st.epoch);
          const mine = held.filter((h) => h.st.epoch === st.epoch);
          held = [];
          mine.sort((a, b) => a.st.revision - b.st.revision);
          for (const h of mine) take(h.st);
        } else {
          held = [];
        }
      }
    }
    if (st.epoch !== epoch) {
      // Another epoch is never taken by itself: held while the stream
      // reconnects, until its next first frame confirms it; dropped
      // otherwise (G2, J10).
      if (reconnecting || !connFirst) held.push({ st });
      return false;
    }
    return take(st);
  }

  function takeEpoch(e) {
    epoch = e;
    rev = 0;
    state = null;
    queue = null;
    queueAnswered = 0;
    resetSamples();
    // An operation of another epoch never ran here, or ran in a serve that
    // has ended: its outcome is not known (§2.3).
    for (const m of [ops, inherited]) {
      for (const [k, op] of [...m]) {
        if (op.epoch !== e) {
          m.delete(k);
          storeIdentity();
          settleUnknown(op);
        }
      }
    }
  }

  function take(st) {
    if (st.revision <= rev) return false;
    rev = st.revision;
    const before = state;
    state = st;
    offered = true;
    if (!queue || st.queueRev > queue.readAt) readQueue();
    for (const rec of st.operations || []) noteRecord(st, rec);
    if ([...inherited.values()].some((e) => e.epoch === st.epoch)) readBack();
    flushWaiting();
    if (output === "browser" && phase === PHASE.browser) {
      // While this browser plays, a publication showing the bridge playing
      // puts §3.3's line on the bar; the console stops neither.
      const about = [WORDS.bridgeToo, WORDS.bridgePlaying, WORDS.bridgeAgain];
      if (bridgeSounds(st)) {
        if (!about.includes(line)) line = WORDS.bridgeToo;
      } else if (about.includes(line)) {
        line = "";
      }
    } else if (output === "bridge" && line === WORDS.reconnecting) {
      line = "";
    }
    void before;
    emit();
    return true;
  }

  // ---- The queue (G4) ----

  // readQueue reads the queue, one read at a time. An answer is taken only
  // where its readAt is at or above the latest queueRev the bar holds and
  // above its queue's: an older read never wins over a newer frame, and is
  // made again. It reads again only for a state that came while it read or
  // an answer that moved on, so a console that answers the same read twice
  // costs one read more, never a loop.
  async function readQueue() {
    if (queueReading) {
      queueAgain = true;
      return;
    }
    queueReading = true;
    lastQueueAsk = env.perf();
    const askedIn = epoch;
    let q = null;
    try {
      const r = await env.fetch(ROUTES.queue, { headers: { accept: "application/json" } });
      if (r.ok) q = await r.json();
    } catch { q = null; }
    queueReading = false;
    let moved = false;
    if (q && q.epoch === epoch && askedIn === epoch && q.readAt > 0) {
      moved = q.readAt > queueAnswered;
      queueAnswered = Math.max(queueAnswered, q.readAt);
      if ((!queue || q.readAt > queue.readAt) && (!state || q.readAt >= state.queueRev)) {
        queue = q;
        emit();
      }
    }
    const again = queueAgain;
    queueAgain = false;
    if (state && (again || moved) && (!queue || state.queueRev > queue.readAt)) readQueue();
  }

  // ---- The clock (M1, P7) ----

  // resetSamples forgets the samples and samples again: a response asked
  // before it, whatever it says, is dropped by its generation (P7).
  function resetSamples() {
    samplingGen += 1;
    offset = null;
    sampleNow();
  }

  async function sampleNow() {
    if (offered === false) return;
    sampleInFlight = true;
    lastSampleAsk = env.perf();
    const ask = { conn, gen: samplingGen, epoch };
    let r;
    try {
      r = await env.fetch(ROUTES.now, { headers: { accept: "application/json" } });
    } catch {
      sampleInFlight = false;
      return;
    }
    const received = now();
    sampleInFlight = false;
    let body = null;
    try { body = await r.json(); } catch { body = null; }
    // A sample belongs to the connection, the epoch and the sampling
    // generation it was asked under: a later one drops it whole, its clock
    // and its state both (P7, P8).
    if (ask.conn !== conn || ask.gen !== samplingGen || ask.epoch !== epoch) return;
    if (!r.ok) {
      // GET now's typed refusal is what says no player is offered here: a
      // box without one sends no frame at all (V6). A start waiting to know
      // goes on in this browser at once.
      if (body && NOT_OFFERED.has(body.error)) {
        offered = false;
        flushWaiting();
        emit();
      }
      return;
    }
    // GET now answered: this box offers its player, which the empty bar
    // shows at once, before the stream's first frame (K1, §7.1).
    if (offered !== true && body && typeof body === "object") {
      offered = true;
      emit();
    }
    const sent = Date.parse(r.headers?.get?.(CLOCK_HEADER) || "");
    offer(body, "now");
    if (Number.isFinite(sent) && body && body.epoch === epoch) {
      const o = received - sent;
      offset = offset === null ? o : Math.min(offset, o);
      emit();
    }
  }

  function position() {
    const an = state?.anchor;
    if (!an) return null;
    let s = an.seconds;
    // Extrapolated only while the anchor names the occurrence heard, from
    // its own time, by a sampled offset.
    const ours = !state.track || an.entry === state.track.entry;
    if (ours && an.advancing && offset !== null && an.at) {
      const at = Date.parse(an.at);
      if (Number.isFinite(at)) s += Math.max(0, now() - (at + offset)) / 1000;
    }
    const d = state?.track?.duration;
    if (d > 0 && s > d) s = d;
    return s;
  }

  // ---- The stream's lifecycle (P8) ----

  function streamOpen(serial) {
    if (!(serial > conn)) return;
    conn = serial;
    connFirst = false;
    resetSamples();
  }

  function streamError(serial) {
    if (serial !== conn) return;
    reconnecting = true;
    connFirst = false;
    if (output === "bridge") line = WORDS.reconnecting;
    emit();
  }

  function frame(bytes, serial) {
    const st = parse(bytes);
    if (offer(st, "frame", serial)) return;
    emit();
  }

  // ---- Operations (§2.3; round 2: R2, R5, R6, R7, V1, V2, V3, V10) ----

  // current says whether entry is still this document's own operation, in
  // flight or unconfirmed, and its answer still one to read: not resolved,
  // not dropped with its epoch, not past the bar's 30 s (V1, V10).
  function current(entry) {
    return ops.get(entry.key) === entry && !entry.expired;
  }

  // live says whether entry, its own or inherited, is not yet resolved.
  function live(entry) {
    return (entry.inherited ? inherited : ops).get(entry.key) === entry;
  }

  // ours says whether entry may still move the output, the phase or the
  // browser: only an operation sent under the newest pick (R3, V11).
  function ours(entry) {
    return entry.pick === pickGen;
  }

  // drop removes entry, resolved, from what is kept and stored (R7).
  function drop(entry) {
    (entry.inherited ? inherited : ops).delete(entry.key);
    storeIdentity();
  }

  async function send(route, body, op) {
    if (!epoch) {
      // No epoch, so no operation can be named: refused here, before it
      // was sent, and ended as a refusal is (R2).
      const words = "The bridge's player has not been read yet";
      line = words;
      op.refused?.({ error: "no_epoch" }, words, true);
      emit();
      return { refused: "no_epoch" };
    }
    seq += 1;
    const entry = { ...op, key: opKey(epoch, tab, seq), seq, epoch, tab, pick: pickGen, inFlight: true, sentAt: env.perf(), records: [] };
    ops.set(entry.key, entry);
    storeIdentity();
    let ans = null;
    try {
      const r = await env.fetch(route, {
        method: "POST",
        headers: { "content-type": "application/json", accept: "application/json" },
        body: JSON.stringify({ op: { tab: entry.tab, seq: entry.seq, epoch: entry.epoch }, ...body }),
      });
      // An answer past the bar's 30 s, or to an operation its epoch's end
      // settled, is not read: its record resolves it (V1, V10).
      if (!current(entry)) return { stale: true };
      ans = await r.json();
    } catch {
      ans = null;
    }
    if (!current(entry)) return { stale: true };
    entry.inFlight = false;
    if (!ans || typeof ans !== "object") {
      lost(entry);
      return { lost: true };
    }
    const st = ans.player ? parse(ans.player) : null;
    // The state the answer carries, ordered as the rest; a held answer's
    // record is processed whatever the state's revision (J10).
    if (st) offer(st, "answer");
    if (!ans.record) {
      if (!NEVER_RAN.has(ans.error)) {
        // An answer with no record that proves nothing: the command may have
        // run, so its record says how it ended (V2).
        lost(entry);
        return { lost: true, error: ans.error || "" };
      }
      // Refused before it became an operation: it never ran (R2).
      ops.delete(entry.key);
      let words;
      if (ans.error === "operation_conflict") {
        freshIdentity();
        words = WORDS.conflict;
      } else if (ans.error === "epoch_changed") {
        words = WORDS.epochChanged;
      } else if (NOT_OFFERED.has(ans.error)) {
        offered = false;
        words = ans.message || "";
      } else {
        words = ans.message || refusedWord(ans.error);
      }
      storeIdentity();
      const mine = ours(entry);
      if (mine) line = words;
      entry.refused?.(ans, words, mine);
      emit();
      return { refused: ans.error };
    }
    const rec = ans.record;
    if (rec.status === "running") {
      lost(entry);
      return { running: true, record: rec };
    }
    finish(entry, rec, "answer");
    return { record: rec };
  }

  function lost(entry) {
    if (!live(entry)) return;
    entry.lost = true;
    entry.inFlight = false;
    if (ours(entry)) {
      phase = PHASE.unconfirmed;
      line = WORDS.unconfirmed;
    }
    for (const r of entry.records) {
      if (r.status !== "running") {
        settleFromStream(entry, r);
        return;
      }
    }
    readBack();
    emit();
  }

  // noteRecord takes a record from the stream's eight, named by the
  // publication's epoch and its own tab and count (V3): an operation in
  // flight keeps it until its own answer decides; one whose answer was lost,
  // and one an earlier document left, are decided by it.
  function noteRecord(st, rec) {
    if (!rec || rec.status === "running") return;
    const key = opKey(st.epoch, rec.tab, rec.seq);
    const entry = ops.get(key) || inherited.get(key);
    if (!entry) return;
    if (entry.inFlight) {
      entry.records.push(rec);
      return;
    }
    if (entry.inherited || entry.lost) settleFromStream(entry, rec);
  }

  // A published record carries no handover and no tracks left out (the
  // control's recentRecords): an operation that needs them is completed by
  // its record read back whole, and by nothing else (R5).
  function needsWhole(entry) {
    return entry.role === "switch-out" || entry.role === "switch-in" || (entry.role === "edit" && entry.kind !== "delete");
  }

  function settleFromStream(entry, rec) {
    if (needsWhole(entry)) {
      readBack(true);
      return;
    }
    if (entry.inherited) finishInherited(entry, rec);
    else finish(entry, rec, "stream");
  }

  async function readBack(force) {
    if (!force && now() - readBackAt < READ_BACK_MS) return;
    readBackAt = now();
    const targets = [];
    for (const e of ops.values()) if (e.lost && e.epoch === epoch) targets.push(e);
    for (const e of inherited.values()) if (e.epoch === epoch) targets.push(e);
    for (const e of targets) {
      let ans = null;
      try {
        const r = await env.fetch(`${ROUTES.operation}?epoch=${encodeURIComponent(e.epoch)}&tab=${encodeURIComponent(e.tab)}&seq=${e.seq}`,
          { headers: { accept: "application/json" } });
        ans = await r.json();
      } catch { continue; }
      if (!ans || !live(e)) continue;
      if (ans.error === "operation_gone" || ans.error === "epoch_changed") {
        drop(e);
        settleUnknown(e, ans.error === "operation_gone" ? WORDS.gone : WORDS.epochChanged);
        continue;
      }
      const rec = ans.record;
      if (!rec || rec.status === "running") continue;
      if (e.inherited) finishInherited(e, rec);
      else finish(e, rec, "readback");
    }
    emit();
  }

  function finish(entry, rec, how) {
    if (ops.get(entry.key) !== entry) return;
    ops.delete(entry.key);
    storeIdentity();
    // The state the done reads is the latest accepted, never an answer
    // the order refused (R4).
    entry.done?.(rec, how, ours(entry));
    emit();
  }

  // settleUnknown ends an operation whose record is not kept, or of a serve
  // that has ended: its caller has dropped it.
  function settleUnknown(entry, words) {
    if (entry.inherited) {
      if (ours(entry)) {
        phase = output === "bridge" ? PHASE.bridge : PHASE.browser;
        line = words || WORDS.pickAgain;
      }
    } else {
      entry.unknown?.(words, ours(entry));
    }
    emit();
  }

  // finishInherited ends an operation an earlier document left, its record
  // read back here (a reload, or a copy of this tab, whose session storage
  // carried it in): what it moved is taken, stopped, and nothing starts, the
  // reader's Play going through the gate; its record's words say how it
  // ended (R8, V5, V9, C1).
  function finishInherited(e, rec) {
    if (inherited.get(e.key) !== e) return;
    drop(e);
    if (!ours(e)) {
      emit();
      return;
    }
    if (e.role === "switch-out") {
      if (rec.status === "done") {
        const h = handoverOf(rec);
        setOutput("browser");
        phase = PHASE.browser;
        if (h) env.browser?.take?.(h, false);
        line = state && bridgeSounds(state) ? WORDS.bridgeAgain : (h?.playing ? WORDS.pressPlay : "");
      } else {
        phase = PHASE.bridge;
        line = switchOutWord(rec);
      }
      emit();
      return;
    }
    if (e.role === "switch-in") {
      resolveSwitchIn(rec, e.handover, { recovered: true });
      return;
    }
    phase = output === "bridge" ? PHASE.bridge : PHASE.browser;
    line = recordLine(rec);
    emit();
  }

  // ---- The gate: every start of this browser (R1, R4, V4, V6, V7) ----

  // gateStart is how this browser starts, whatever asked (a row, Play, Next,
  // a lock-screen control, a resume after a refused move): run, given a
  // permit its later plays ask again (V4), only where no player is offered,
  // or on a barrier's done answered now beside a bridge that does not
  // sound. Before GET now or a frame has said whether a player is offered,
  // and with one offered before its epoch is known, the start waits; a
  // bridge that sounds refuses it, with its line. A pause never comes here.
  // A continuation (round 3, S1) is asked as a start is, and never displaces
  // a start the reader asked for, which supersedes it. Whatever asks may hand
  // a drop, which every way that never runs it calls, once: a refusal, a
  // start that replaced it, a pick, the wait's end (round 4, W6).
  function gateStart(run, { continuation = false, drop } = {}) {
    if (offered === false) {
      run(null);
      return;
    }
    if (offered !== true || !epoch || !state) {
      if (continuation && waiting && !waiting.continuation) {
        drop?.();
        return;
      }
      const replaced = waiting;
      waiting = { run, at: env.perf(), continuation, drop };
      line = WORDS.startWaits;
      dropOf(replaced);
      emit();
      return;
    }
    decideStart(run, continuation, drop);
  }

  // dropOf calls the drop a request handed, at most once.
  function dropOf(holder) {
    const d = holder?.drop;
    if (holder) holder.drop = null;
    d?.();
  }

  // gateContinue is how playback this browser was let start goes on by
  // itself (round 3, S1, Q1, Q7: audio.js's continueStart): the permit its
  // start was given is asked again, and the bar says why where it no longer
  // holds; with no permit to ask (a start let go on where no player was
  // offered, or a continuation that starts the element anew) it is asked as
  // a start is.
  function gateContinue(permit, run, drop) {
    if (typeof permit === "function") {
      if (permit()) {
        run(permit);
        return;
      }
      if (output === "browser" && bridgeSounds(state)) line = WORDS.bridgePlaying;
      drop?.();
      emit();
      return;
    }
    gateStart(run, { continuation: true, drop });
  }

  function flushWaiting() {
    if (!waiting) return;
    if (offered !== false && (offered !== true || !epoch || !state)) return;
    const w = waiting;
    waiting = null;
    if (line === WORDS.startWaits) line = "";
    if (offered === false) w.run(null);
    else decideStart(w.run, w.continuation, w.drop);
  }

  function decideStart(run, continuation = false, drop) {
    if (output !== "browser") {
      drop?.();
      return;
    }
    if (bridgeSounds(state)) {
      line = WORDS.bridgePlaying;
      drop?.();
      emit();
      return;
    }
    // One barrier at a time: a start asked while one is out replaces the
    // start it runs; a continuation replaces only another continuation.
    if (startPending) {
      if (!continuation || startPending.continuation) {
        const replaced = { drop: startPending.drop };
        startPending.run = run;
        startPending.continuation = continuation;
        startPending.drop = drop;
        dropOf(replaced);
      } else {
        drop?.();
      }
      return;
    }
    const pending = { run, continuation, drop };
    startPending = pending;
    // The latch lives as long as its barrier's operation: whatever resolves
    // it lets go of the latch first, and nothing else does, the request's
    // promise least of all, which a half-open connection never settles (S3,
    // Q9).
    const release = () => {
      if (startPending === pending) startPending = null;
    };
    void barrier((permit) => pending.run(permit), release, () => dropOf(pending));
  }

  // permitFor is a start's permit: true while no newer pick came, this
  // browser is the output, and no state newer than the barrier's silence,
  // or of another epoch, shows the bridge sounding (R3, R4, V4).
  function permitFor(silence, gen) {
    const ep = epoch;
    return () => {
      if (gen !== pickGen || output !== "browser") return false;
      if (!state) return true;
      if (state.epoch !== ep || state.revision > silence) return !bridgeSounds(state);
      return true;
    };
  }

  // ---- The barrier (§3.2) ----

  // barrier runs the switch-out with take false, and calls start with its
  // permit on its done answered now, and on nothing else; a newer pick, or a
  // state newer than its silence that shows the bridge playing, starts
  // nothing (R3, R4). Every end calls release first (S3, Q9), and every end
  // that starts nothing calls drop (round 4, W6).
  function barrier(start, release, drop) {
    return send(ROUTES.switchOut, { take: false }, {
      role: "barrier",
      done(rec, how, mine) {
        release();
        if (!mine) {
          drop();
          return;
        }
        // Its done, answered now, with the revision of the publication that
        // showed the bridge silent: that alone starts the browser.
        if (rec.status === "done" && how === "answer" && rec.silence > 0) {
          const permit = permitFor(rec.silence, pickGen);
          if (!permit()) {
            line = WORDS.bridgeAgain;
            drop();
            return;
          }
          start(permit);
          return;
        }
        // A record read back or published after its answer was lost: it
        // starts nothing, and Play needs a barrier of its own (Q2).
        phase = PHASE.browser;
        line = rec.status === "done" ? WORDS.startAgain : barrierWord(rec);
        drop();
      },
      unknown(words, mine) {
        release();
        if (mine) {
          phase = PHASE.browser;
          line = words || WORDS.startAgain;
        }
        drop();
      },
      // Refused before it ran: nothing starts, the refusal's line said (R2);
      // but refused for no player, none is offered, and the start goes on in
      // this browser as a start does where none is (round 3).
      refused(ans, words, mine) {
        release();
        if (!mine) {
          drop();
          return;
        }
        phase = PHASE.browser;
        if (NOT_OFFERED.has(ans.error)) start(null);
        else drop();
      },
    });
  }

  function barrierWord(rec) {
    if (rec.status === "changed") return WORDS.bridgePlaying;
    if (rec.status === "refused") return refusedWord(rec.error);
    return WORDS.pickAgain;
  }

  // ---- The switch to the bridge (§7.3) ----

  function switchIn() {
    const b = env.browser;
    if (!b?.hasQueue?.()) {
      // Nothing in the browser: the bridge's at once, nothing sent.
      setOutput("bridge");
      phase = PHASE.bridge;
      line = "";
      emit();
      return Promise.resolve({ nothing: true });
    }
    // The browser's handover is read first, before its source is let go
    // (G11); then the browser stops, and only then the request goes.
    const h = b.handover();
    b.suspend(h);
    phase = PHASE.toBridge;
    line = WORDS.toBridge;
    setOutput("bridge");
    emit();
    const tracks = h.queue.map((t) => t.path);
    const body = { at: state?.generation || "", tracks, start: h.index, play: !!h.playing };
    if (h.position > 0 && h.pcm) body.position = h.position;
    return send(ROUTES.replace, body, {
      role: "switch-in",
      kind: "replace",
      handover: h,
      done(rec, how, mine) { if (mine) resolveSwitchIn(rec, h); },
      unknown(words, mine) { if (mine) { phase = PHASE.bridge; line = words || WORDS.pickAgain; } },
      // Refused before it ran: back to this browser, its line said, which
      // resumes only through the gate, and only if it was playing (R2).
      refused(ans, words, mine) {
        if (!mine) return;
        setOutput("browser");
        phase = PHASE.browser;
        line = words;
        if (h.playing) gateStart((permit) => env.browser?.resume?.(h, permit));
      },
    });
  }

  function resolveSwitchIn(rec, h, { recovered = false } = {}) {
    if (rec.status === "done") {
      setOutput("bridge");
      phase = PHASE.bridge;
      line = leftOutLine(rec, h?.queue?.length) || (h && h.position > 0 && !h.playing ? WORDS.positionStayed : "") ||
        (rec.seek ? `${seekWord(rec.seek)}` : "") || windowLine(rec) || releaseLine(rec);
      emit();
      return;
    }
    if (rec.effect === "none") {
      // Refused with nothing changed (G9: the engine's answer decides, never
      // the count sent): back to the browser, which resumes only through
      // the gate, only beside a bridge that reads silent in the latest state
      // accepted (G1, R4), and never for a document that only read the
      // record back (R8, V5).
      setOutput("browser");
      phase = PHASE.browser;
      const why = rec.status === "refused" ? refusedWord(rec.error) : "";
      const st = state;
      if (st && !st.silent) {
        line = st.hold && st.hold !== "none" ? holdWord(st.hold) : WORDS.bridgePlaying;
        emit();
        return;
      }
      if (recovered) {
        line = [why, h?.playing ? "press Play to start this browser" : ""].filter(Boolean).join(": ");
        emit();
        return;
      }
      line = why;
      emit();
      if (h?.playing) gateStart((permit) => env.browser?.resume?.(h, permit));
      return;
    }
    // Changed, a later step refused, or cancelled: the switch happened in
    // part, so the bar is on the bridge and the browser stays stopped.
    setOutput("bridge");
    phase = PHASE.bridge;
    line = rec.status === "changed" ? partialLine(rec, state, plannedOf(rec, h?.playing)) : (rec.status === "refused" ? refusedWord(rec.error) : WORDS.pickAgain);
    emit();
  }

  function seekWord(seek) {
    return {
      dsd_or_unknown_length: "The track plays from its start: the bridge's player does not seek within DSD, or the track's length is not known",
      another_entry_plays: "The track plays from its start: another entry played before the seek",
      the_state_kept_changing: "The track plays from its start: the player kept changing",
    }[seek] || `The track plays from its start (${seek})`;
  }

  // ---- The switch to this browser (§7.4) ----

  function switchOut() {
    phase = PHASE.toBrowser;
    line = WORDS.toBrowser;
    emit();
    return send(ROUTES.switchOut, { take: true }, {
      role: "switch-out",
      done(rec, how, mine) {
        if (!mine) return;
        if (rec.status === "done" && how === "answer" && rec.silence > 0) {
          const h = handoverOf(rec);
          setOutput("browser");
          phase = PHASE.browser;
          const permit = permitFor(rec.silence, pickGen);
          // A state accepted since the silence that shows the bridge playing
          // again: the queue taken stopped, and said (R4).
          if (!permit()) {
            line = WORDS.bridgeAgain;
            if (h) env.browser?.take?.(h, false);
            return;
          }
          line = "";
          if (h) env.browser?.take?.(h, !!h.playing, permit);
          return;
        }
        if (rec.status === "done") {
          // Read back whole after its answer was lost: history (G8).
          recoverSwitchOut(rec);
          return;
        }
        phase = PHASE.bridge;
        line = switchOutWord(rec);
      },
      unknown(words, mine) { if (mine) { phase = PHASE.bridge; line = words || WORDS.pickAgain; } },
      // Refused before it ran: back on the bridge, its line said; where no
      // player is offered, this browser is the output, nothing started (R2).
      refused(ans, words, mine) {
        if (!mine) return;
        if (NOT_OFFERED.has(ans.error)) {
          setOutput("browser");
          phase = PHASE.browser;
          return;
        }
        phase = PHASE.bridge;
      },
    });
  }

  function switchOutWord(rec) {
    if (rec.status === "changed") return WORDS.wentOn;
    if (rec.status === "refused") return refusedWord(rec.error);
    return WORDS.pickAgain;
  }

  // recoverSwitchOut is this document's own switch-out, its answer lost and
  // its record read back done: what it moved is taken, and only a barrier
  // answered now, beside a bridge that does not sound, starts it (G8).
  function recoverSwitchOut(rec) {
    const h = handoverOf(rec);
    setOutput("browser");
    phase = PHASE.browser;
    if (h) env.browser?.take?.(h, false);
    if (state && bridgeSounds(state)) {
      line = WORDS.bridgeAgain;
      return;
    }
    line = "";
    if (h?.playing) gateStart((permit) => env.browser?.start?.(permit));
  }

  // handoverOf keeps the entries with a track: a selected one with none gives
  // way to the next kept, from its start, and with none after it is stopped.
  function handoverOf(rec) {
    const ho = rec?.handover;
    if (!ho || !Array.isArray(ho.tracks)) return null;
    const queue = [];
    let index = -1;
    let gone = false;
    ho.tracks.forEach((t, i) => {
      if (i === ho.selected) gone = !t?.track;
      if (!t?.track) return;
      if (i === ho.selected || (gone && index < 0)) index = queue.length;
      queue.push(t.track);
    });
    const after = !gone || index >= 0;
    return { queue, index: after ? index : (queue.length ? 0 : -1), position: gone ? 0 : ho.position || 0, playing: !!ho.playing && after };
  }

  // ---- The picks and the commands ----

  function canPick() {
    const st = state;
    return !!(st && st.offered && st.hold === "none" && st.release?.state === "confirmed");
  }

  // pick moves the output, a pick at a time: each takes the next pick
  // generation, so only the newest switch moves the output, the phase or
  // the browser (R3). A re-pick from unconfirmed supersedes. A start this
  // browser had waiting, or behind a barrier, is the older pick's: the pick
  // lets go of it (Q9).
  function pick(which) {
    if (which === "bridge") {
      if (output === "bridge" && phase !== PHASE.unconfirmed) return Promise.resolve({ already: true });
      if (!canPick()) return Promise.resolve({ refused: "not_offered" });
      newPick();
      return switchIn();
    }
    if (output === "browser" && phase === PHASE.browser) return Promise.resolve({ already: true });
    newPick();
    return switchOut();
  }

  function newPick() {
    pickGen += 1;
    const dropped = [waiting, startPending];
    waiting = null;
    startPending = null;
    for (const h of dropped) dropOf(h);
    // Stored at once: a pick that sends nothing still supersedes, across a
    // reload too (S4, Q10).
    storeIdentity();
  }

  // queueBehind says whether the queue the bar drew is older than the state's
  // last change of it: a positional command would then pick from a queue
  // the tab never drew (R9, V12).
  function queueBehind() {
    return !queue || !state || queue.readAt < state.queueRev;
  }

  function command(cmd, args = {}) {
    if (cmd === "index" && queueBehind()) {
      line = WORDS.queueBehind;
      emit();
      return Promise.resolve({ refused: "queue_behind" });
    }
    const body = { command: cmd };
    if (cmd !== "pause" && cmd !== "stop") body.at = state?.generation || "";
    if (cmd === "index") body.index = args.index;
    if (cmd === "seek") body.seconds = Math.max(0, Math.floor(args.seconds || 0));
    if (cmd === "repeat") body.on = !!args.on;
    return send(ROUTES.transport, body, {
      role: "command",
      done(rec, how, mine) {
        if (!mine) return;
        line = rec.error === "stale" ? WORDS.stale : recordLine(rec);
        if (phase === PHASE.unconfirmed) phase = PHASE.bridge;
      },
      unknown(words, mine) { if (mine) { phase = PHASE.bridge; line = words || ""; } },
    });
  }

  function edit(route, kind, scope, extra = {}) {
    const body = { at: state?.generation || "", ...scope, ...extra };
    return send(route, body, {
      role: "edit",
      kind,
      done(rec, how, mine) {
        if (!mine) return;
        if (phase === PHASE.unconfirmed) phase = PHASE.bridge;
        if (rec.status === "done") line = leftOutLine(rec, scope.tracks?.length) || windowLine(rec) || releaseLine(rec);
        else if (rec.status === "changed") line = partialLine(rec, state, plannedOf(rec, extra.play));
        else if (rec.error === "stale") line = WORDS.stale;
        else if (rec.error === "queue_full") line = roomLine(rec) || refusedWord(rec.error);
        else line = recordLine(rec);
      },
      unknown(words, mine) { if (mine) { phase = PHASE.bridge; line = words || ""; } },
    });
  }

  // replace plays scope ({album}, {playlist} or {tracks}) on the bridge's
  // player from start, its position, playing.
  function replace(scope, { start = 0, position = 0, play = true } = {}) {
    const extra = { start, play };
    if (position > 0) extra.position = Math.floor(position);
    return edit(ROUTES.replace, "replace", scope, extra);
  }

  function append(scope) { return edit(ROUTES.append, "append", scope); }

  function remove(entry) { return edit(ROUTES.remove, "delete", { entry }); }

  // ---- The ticks and the resets (M1, P7, R6, V6, C4) ----

  function tick() {
    const d = env.now() - env.perf();
    if (drift === null) drift = d;
    else if (Math.abs(d - drift) > CLOCK_JUMP_MS) {
      drift = d;
      resetSamples();
    }
    // GET now is asked again within 5 s while it has not said whether a
    // player is offered, or while the clock is unsampled (V6).
    if (offered !== false && offset === null && !sampleInFlight && env.perf() - lastSampleAsk >= SAMPLE_RETRY_MS) sampleNow();
    if (reconnecting && output === "bridge" && env.perf() - lastPoll >= RECONNECT_POLL_MS) {
      lastPoll = env.perf();
      sampleNow();
    }
    // A request unanswered past the bar's 30 s is lost: unconfirmed, its
    // record to say how it ended, its answer not read if it comes (R6, V1).
    for (const e of [...ops.values()]) {
      if (e.inFlight && env.perf() - e.sentAt >= REQUEST_MS) {
        e.expired = true;
        lost(e);
      }
    }
    // A queue that is behind its state, its read failed or overtaken, is
    // read again (C4).
    if (state && !queueReading && queueBehind() && env.perf() - lastQueueAsk >= QUEUE_RETRY_MS) readQueue();
    if (waiting && env.perf() - waiting.at >= START_WAIT_MS) {
      const w = waiting;
      waiting = null;
      line = WORDS.startUnanswered;
      dropOf(w);
      emit();
    }
    if ([...ops.values()].some((e) => e.lost) || [...inherited.values()].some((e) => e.epoch === epoch)) readBack();
  }

  // resume is a page shown again (visibilitychange to visible, pageshow
  // from the cache): the samples are forgotten and taken again (M1).
  function resume() {
    resetSamples();
  }

  // ---- The view ----

  function bridgeSounds(st) {
    if (!st || !st.running) return false;
    return st.transport === "playing" || (st.transport === "releasing" && st.pending === "play") || st.starting;
  }

  function view() {
    return {
      output,
      phase,
      offered: offered === true,
      notOffered: offered === false,
      state,
      queue,
      queueBehind: queueBehind(),
      line,
      reconnecting,
      sampled: offset !== null,
      canPick: canPick(),
      pickWhy: state ? (!state.offered ? whyWord(state.offeredWhy) : (state.hold !== "none" ? holdWord(state.hold) : (state.release?.state !== "confirmed" ? "The DAC has not confirmed it is free" : ""))) : "",
      unconfirmed: phase === PHASE.unconfirmed,
      commands: new Set(state?.commands || []),
    };
  }

  bootIdentity();
  if (output === "bridge") env.browser?.mediaSession?.(false);

  return {
    view,
    subscribe(fn) { listeners.add(fn); fn(view()); return () => listeners.delete(fn); },
    frame,
    streamOpen,
    streamError,
    sampleNow,
    pick,
    command,
    replace,
    append,
    remove,
    tick,
    resume,
    position,
    gateStart,
    gateContinue,
    // Whether the bar's line says the bridge's player plays, which a play
    // the browser refuses then says nothing beside (round 4, W8).
    refusalShown: () => [WORDS.bridgePlaying, WORDS.bridgeAgain, WORDS.bridgeToo].includes(line),
    identity: () => ({ tab, seq }),
    pending: () => [...inherited.keys(), ...ops.keys()],
  };
}
