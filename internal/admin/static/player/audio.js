// The audio element, the queue, and the OS media-session bridge.
//
// Exactly ONE <audio>, parented to <body> — never inside the view
// container, so no route change can detach it. A second hidden element
// acts as a preloader.

import { audioURL } from "./api.js";
import { resolvePlayable, formatChip, markUnplayable } from "./format.js";
import { currentOutput } from "./output.js";

const STORE_KEY = "bridge-player-queue";

/** Consecutive unplayable skips before we stop and say so. */
const SKIP_STORM_LIMIT = 3;

const listeners = new Set();

const state = {
  el: null,
  pre: null,
  queue: [],
  index: -1,
  shuffle: false,
  shuffleOrder: null,
  repeat: "off", // "off" | "all" | "one"
  playing: false,
  loading: false,
  degraded: false,
  seekable: true,
  error: "",
  skipped: 0,
  albumArt: null,
};

/** readyState: the element has enough data to advance at least a little. */
const HAVE_FUTURE_DATA = 3;
/** readyState: the element believes it can play to the end uninterrupted. */
const HAVE_ENOUGH_DATA = 4;

/** How long before the end of a track its successor is warmed. */
const PRIME_LEAD_S = 30;

// The queue entry the preloader currently holds, so a prime is issued
// once rather than re-issued on every timeupdate.
let primedFor = null;

// Bumped every time the CURRENT track changes — a load, or the queue
// being cleared. Anything that suspends and then wants to touch playback
// state captures it first and bails when it no longer matches.
//
// A counter rather than comparing the track object: playQueue clones its
// tracks, so identity catches a replaced queue and a different selection,
// but NOT the same track being re-loaded — a reader retrying the row that
// just failed would have their fresh attempt clobbered by the stale one.
let playbackGen = 0;
// Each play of the audible element: every load of it (the retry's too, never
// the preloader's) and each repeat-one replay, which loads nothing. The bar's
// seek binds a drag to it (B1021); nothing else reads it.
let plays = 0;

// How long the failure probe waits for response HEADERS. Short, because
// the source has ALREADY failed to load by the time it runs — this only
// decides which of four messages the reader gets, and a stalled probe
// would hold up the skip.
const PROBE_TIMEOUT_MS = 4000;

// The position a suspended browser keeps (B634): read from the element
// before its source was let go, which resets the element's own, so the
// saved queue keeps it until something loads again (§7.3, step 1); and
// whether it was playing, which a pick of the bridge made again while the
// first is unconfirmed hands over as it was (R3).
let heldAt = null;
let heldPlaying = false;

// The start gate (B634 round 2, R1): output.js's gateStart, set by boot.js
// before the bar mounts. Every start of the element asks it (playQueue,
// toggle's play, an advance a reader asks for, the MediaSession's handlers,
// which call those), and runs only as it says, handed a permit that a later
// play of that start asks again (V4). A pause never asks it; the reader's
// explicit Pause (the bar's, the lock screen's) and the lock screen's Stop
// let go of the starts it holds, through startCancel, set beside it
// (output.js's cancelStarts).
let startGate = null;
let startCancel = null;
// The continuation gate (round 3, S1, Q7): output.js's gateContinue, set by
// boot.js beside the start gate. What playback does by itself asks it
// (continueStart): the next track at a track's end and the retry after an
// error, with their start's permit; a skip past a track that cannot play, a
// repeat-one replay and a removed row's successor, which start the element
// anew, with none, as a start is asked.
let continueGate = null;
// The permit of the start playing now, which a continuation asks again.
let startPermit = null;
// Whether this browser means to play what it holds (round 3, S2, Q8): set by
// a load that means to play and by a play a start or a continuation lets
// go on; cleared by a load that does not, a pause of the element other than
// the one its end makes or one a play has overtaken, the reader's explicit
// Pause, a start the reader asks over a paused element (requestStart),
// suspend(), clearQueue(), the stop at the queue's end and restore(). A
// continuation, and the retry after an error, play only while it holds: the
// element cannot say it, since load() pauses it and a track's end pauses it
// before `ended`.
let intent = false;
// The marks of the bar's own plays whose `play` event has not come (round 4,
// T1, W1): a play() on a paused element queues one `play` event, which takes
// the oldest mark; one on a playing element queues none and leaves none. A
// mark goes with its play's promise once that settles, and every load takes
// them all, since the load algorithm removes the events they wait for.
const marks = [];
// The protective pauses of the browser's own plays, whose `pause` events are
// the bar's own (W3), and the one such play waiting on the gate (W4); one
// whose source the browser cannot use is marked (skip), its answer settling
// only whether that source's skip goes on, until a load or the skips' stop.
let ownPauses = 0;
let resumeWait = null;
// What the reader does to playback (a transport call, a pick's suspend or
// take) and a pause that is not the bar's own move this count; a resume of
// the browser's own goes on only where it has not moved (W4).
let actions = 0;

/** setStartGate makes gate the one way the element starts, and cancel what lets go of the starts it holds. */
export function setStartGate(gate, cancel = null) {
  startGate = gate;
  startCancel = cancel;
}

/** setContinueGate makes gate the one way playback goes on by itself. */
export function setContinueGate(gate) {
  continueGate = gate;
}

// With no gate wired (a boot that failed before it wired them), every start
// is refused, as a start the gate refuses is (round 5). A start asked over a
// paused element supersedes what this browser meant to play there, a
// positioned handover's play its metadata defers among it, which then only
// seeks; one the gate never runs shows that element stopped (startDropped).
function requestStart(run) {
  if (state.el?.paused) intent = false;
  const gen = playbackGen;
  if (startGate) startGate(run, { drop: () => startDropped(gen) });
}

// startDropped is a start of the reader's that never runs (refused, put
// aside for another, let go of): over the paused element of the playback it
// was asked in, the bar shows nothing starting, a handover's spinner gone.
function startDropped(gen) {
  if (gen !== playbackGen || !state.el.paused) return;
  state.playing = false;
  state.loading = false;
  emit();
}

// continueStart is how playback goes on by itself (S1): run, handed the
// permit to play under, once the continuation gate lets it, and only while
// the playback it continues is the one this browser still means to play (no
// load since, no pause), asked again when it runs, since a barrier's round
// trip lies between (Q3). A fresh continuation starts the element anew, so it
// hands the gate no permit and is asked as a start is (Q1). Until it runs the
// browser shows itself stopped; refused, it stays so, the bar saying why.
function continueStart(run, { fresh = false } = {}) {
  const gen = playbackGen;
  let ran = false;
  let settled = false;
  const go = (permit) => {
    if (settled) return;
    settled = true;
    if (gen !== playbackGen || !intent) return;
    ran = true;
    run(permit);
  };
  // Refused, or put aside for another start: the continuation ends, and
  // with it what this browser meant to play, for this playback alone, so an
  // older one's refusal clears no newer intent (round 4, T2, W6).
  const drop = () => {
    if (settled) return;
    settled = true;
    if (gen !== playbackGen) return;
    intent = false;
    state.playing = false;
    state.loading = false;
    emit();
  };
  const permit = fresh ? null : startPermit;
  if (continueGate) continueGate(permit, go, drop);
  else drop();
  if (!ran) {
    state.playing = false;
    state.loading = false;
    emit();
  }
}

// readerAction counts what the reader does to playback, and lets go of a
// resume of the browser's own that waits on the gate (W4), and of its
// spinner; the caller's own emit says so. A call of the reader's own tells the
// output too, whose switch to this browser then takes nothing (B1015); a pause
// event's (own false) tells it nothing: the reader's Pause told it as it was
// pressed, and an interruption changes nothing for the switch.
function readerAction(own = true) {
  actions += 1;
  if (own) currentOutput()?.readerActed?.();
  if (resumeWait) {
    resumeWait = null;
    state.loading = false;
  }
}

// playEl is the one way the bar plays the element (round 4, T1): it marks
// the play, and a play the browser refuses ends what this browser meant to
// play, while the playback is the same (W8).
function playEl() {
  const el = state.el;
  const at = playbackGen;
  const mark = el.paused ? {} : null;
  if (mark) marks.push(mark);
  let p;
  try { p = el.play(); } catch (e) { p = Promise.reject(e); }
  Promise.resolve(p).then(() => unmark(mark), (e) => {
    unmark(mark);
    playRefused(e, at);
  });
}

function unmark(mark) {
  const i = mark ? marks.indexOf(mark) : -1;
  if (i >= 0) marks.splice(i, 1);
}

// loadEl runs the element's load algorithm, which removes the `play` events
// the marks wait for and the `pause` events of the protective pauses.
function loadEl(el) {
  if (el === state.el) {
    marks.length = 0;
    ownPauses = 0;
    plays += 1;
  }
  el.load();
}

// playRefused: a refusal that leaves the element paused (NotAllowedError) ends
// the intent; an AbortError (a pause, the bar's protective one included, or a
// load) and a NotSupportedError, whose error skips it, change nothing (W8).
function playRefused(e, at) {
  if (at !== playbackGen || e?.name === "AbortError" || e?.name === "NotSupportedError") return;
  intent = false;
  state.playing = false;
  state.loading = false;
  if (e?.name === "NotAllowedError" && !currentOutput()?.refusalShown?.()) state.error = "Press play to start.";
  emit();
}

// ownPlay is the browser's own play of the element (round 4, T1): a call
// ending, another app giving the audio focus back. The bar did not start it,
// so the element is paused at once, before anything sounds, and the gate is
// asked as a fresh start with a continuation's priority (W5): admitted, it
// resumes through playEl where the playback is the same and the reader has
// done nothing since; refused, it stays paused, the intent gone, the bar
// saying why. Meanwhile the bar shows it starting. A play a pause has
// overtaken by the time its event comes (the element paused again: the
// reader's Pause, a stop) asks nothing; that pause's own event says the rest.
function ownPlay() {
  if (state.el.paused) return;
  ownPauses += 1;
  state.el.pause();
  const gen = playbackGen;
  const acts = actions;
  const wait = {};
  resumeWait = wait;
  state.playing = false;
  state.loading = true;
  emit();
  const go = (permit) => {
    if (resumeWait !== wait || gen !== playbackGen || acts !== actions) return;
    resumeWait = null;
    if (permit !== undefined) startPermit = permit;
    intent = true;
    if (!wait.skip) playEl();
  };
  const drop = () => {
    if (resumeWait !== wait) return;
    resumeWait = null;
    intent = false;
    state.loading = false;
    emit();
  };
  if (continueGate) continueGate(null, go, drop);
  else drop();
}

// Whether the browser's MediaSession handlers are set: cleared while the
// bridge's player is the bar's output (§7.5).
let sessionActive = true;

export function subscribe(fn) {
  listeners.add(fn);
  fn(snapshot());
  return () => listeners.delete(fn);
}

function emit() {
  const s = snapshot();
  for (const fn of listeners) fn(s);
}

export function snapshot() {
  return {
    track: state.index >= 0 ? state.queue[state.index] : null,
    play: plays,
    queue: state.queue,
    index: state.index,
    playing: state.playing,
    loading: state.loading,
    shuffle: state.shuffle,
    repeat: state.repeat,
    degraded: state.degraded,
    seekable: state.seekable,
    error: state.error,
    albumArt: state.albumArt,
    currentTime: state.el ? state.el.currentTime : 0,
    duration: state.el && Number.isFinite(state.el.duration) ? state.el.duration : 0,
    buffered: bufferedAhead(),
  };
}

/**
 * How far the browser has fetched from the playhead, in seconds.
 *
 * The end of the buffered range the playhead is INSIDE, not the end of
 * the last range. A seek leaves the earlier range behind, and reporting
 * its end would paint a buffer bar to the left of the thumb — fetched
 * bytes, drawn behind where the reader now is.
 *
 * Read live off the element rather than tracked in state, the same way
 * currentTime and duration are: TimeRanges is recomputed by the browser
 * on every fetch, and mirroring it would be a second copy to keep true.
 */
function bufferedAhead() {
  const el = state.el;
  if (!el?.buffered?.length) return 0;
  const t = el.currentTime;
  for (let i = 0; i < el.buffered.length; i++) {
    // A hair of tolerance at the near edge: straight after a seek the
    // playhead can sit a few milliseconds ahead of the range the browser
    // just opened for it, which reads as "nothing buffered" and blanks
    // the bar for a beat.
    if (el.buffered.start(i) <= t + 0.5 && el.buffered.end(i) > t) {
      return el.buffered.end(i);
    }
  }
  return 0;
}

/**
 * `loading` means ONE thing: the player intends to be producing sound
 * and is not, because it is waiting for bytes.
 *
 * Deliberately not "a fetch is in flight" — a paused element topping up
 * its buffer is not something the reader is waiting on, and a spinner
 * for it would cry wolf. Deliberately not readyState either: a track
 * that has stalled mid-play still reports whatever it managed to
 * buffer, and only the element knows it has starved.
 */
function setLoading(on) {
  if (state.loading === !!on) return;
  state.loading = !!on;
  emit();
}

export function init() {
  if (state.el) return;
  state.el = document.createElement("audio");
  state.el.id = "bridge-audio";
  state.el.preload = "metadata";
  state.pre = document.createElement("audio");
  state.pre.preload = "none";
  for (const el of [state.el, state.pre]) {
    el.hidden = true;
    document.body.appendChild(el);
  }

  state.el.addEventListener("play", () => {
    // A play the bar did not mark is the browser's own, and asks the gate,
    // refused where none is wired.
    if (!marks.length) {
      ownPlay();
      return;
    }
    marks.shift();
    state.playing = true;
    // `play` fires the instant `paused` flips, which is BEFORE the
    // element knows whether it has anything to play — so resuming into
    // an empty buffer arrives here looking exactly like resuming into a
    // full one. readyState is what tells them apart.
    state.loading = state.el.readyState < HAVE_FUTURE_DATA;
    setSessionState("playing");
    emit();
  });
  state.el.addEventListener("pause", () => {
    // A pause, the reader's or the browser's own, ends what this browser
    // means to play; the one a track's end makes before its `ended` does not,
    // nor the bar's protective pause of the browser's own play (W3), nor one a
    // play has overtaken by the time its event comes (the element playing
    // again), which leaves that play's intent alone.
    const own = ownPauses > 0;
    if (own) ownPauses -= 1;
    else if (!state.el.ended && state.el.paused) {
      intent = false;
      readerAction(false);
    }
    state.playing = false;
    state.loading = own && resumeWait !== null && !resumeWait.skip;
    setSessionState("paused");
    persist();
    emit();
  });
  // A track's end: the next track goes on its start's permit, asked again;
  // a repeat-one replay starts the element its end paused, as a start does.
  // A track that played to its end breaks a streak of unplayable ones (a
  // play event does not: one comes before a source's error).
  state.el.addEventListener("ended", () => {
    state.skipped = 0;
    continueStart((permit) => advanceNow(1, { auto: true, permit }), { fresh: state.repeat === "one" });
  });
  state.el.addEventListener("timeupdate", throttle(() => {
    persist(); emit(); updatePositionState(); maybePrime();
  }, 900));

  // The element's own account of starving and recovering. Measured on a
  // 200 KB/s link, `waiting` lands ~1 ms after `play` on a cold source —
  // early enough to be the spinner's trigger rather than its
  // confirmation.
  state.el.addEventListener("waiting", () => setLoading(!state.el.paused));
  state.el.addEventListener("playing", () => setLoading(false));
  // `stalled` is the same wait by another name — the fetch itself has
  // gone quiet — but it ALSO fires on a healthy element whose buffer is
  // full and which has therefore stopped asking for bytes. The
  // readyState guard is the whole difference between the two.
  state.el.addEventListener("stalled", () => {
    setLoading(!state.el.paused && state.el.readyState < HAVE_FUTURE_DATA);
  });
  // `emptied` is also fired by load()'s own abort — and as a QUEUED task,
  // so it lands one tick AFTER the synchronous block that set loading and
  // called play(), and one tick BEFORE the `play` that would set it back.
  // Clearing unconditionally therefore blinks the spinner off in the gap.
  // Measured at 1 ms on a warm source, which is exactly the kind of
  // margin that stops being 1 ms on someone else's machine. It only means
  // "nothing is being waited for" when nothing intends to play.
  state.el.addEventListener("emptied", () => {
    if (state.el.paused) setLoading(false);
  });
  state.el.addEventListener("loadedmetadata", () => {
    // A non-finite duration means the source didn't report a length —
    // an upstream that ignored Range, typically. Binding a scrubber to
    // Infinity produces a control that lies, so the UI is told to show
    // elapsed only.
    state.seekable = Number.isFinite(state.el.duration) && state.el.duration > 0;
    updatePositionState();
    emit();
  });
  state.el.addEventListener("error", onError);

  restore();
  wireMediaSession();
}

function onError() {
  const err = state.el.error;
  const track = state.queue[state.index];
  if (!err || !track) return;
  switch (err.code) {
    case 1: // MEDIA_ERR_ABORTED — our own src reassignment.
      return;
    case 4: // MEDIA_ERR_SRC_NOT_SUPPORTED — see handleSourceError.
      // The probe below is a round trip, and a spinner left up across it
      // says "still coming" about a source we already know did not load;
      // nothing plays, and the bar's Play over it starts again (toggle). A
      // resume of the browser's own waiting on the gate settles only whether
      // the skip goes on, as it did: admitted, it plays nothing of the source
      // that failed; refused, it ends the intent (B1015).
      if (resumeWait) resumeWait.skip = true;
      state.playing = state.loading = false;
      emit();
      void handleSourceError(track, state.el.src, state.source);
      return;
    default: { // 2 network / 3 decode — retry once, then surface.
      if (!track._retried) {
        track._retried = true;
        // The reload pauses the element, so whether the retry plays is what
        // this browser meant before it (S2, Q8): a take made stopped, or a
        // queue restored at load, plays nothing; playback goes on as a
        // continuation, its start's permit asked again (V4, Q1).
        const wanted = intent;
        const busted = state.el.src + (state.el.src.includes("?") ? "&" : "?") + "_r=" + Date.now();
        state.el.src = busted;
        loadEl(state.el);
        if (!wanted) {
          state.playing = false;
          state.loading = false;
          emit();
          return;
        }
        // A resume of the browser's own waiting on the gate plays the source
        // reloaded once it is admitted, and nothing if it is cancelled or
        // refused (B1015).
        if (resumeWait) return;
        continueStart((permit) => {
          startPermit = permit;
          playEl();
        });
        return;
      }
      state.error = `Playback failed for "${track.title || track.path}".`;
      // Its retry's barrier may still answer, and a resume of the browser's
      // own waiting on the gate be admitted: neither plays over this (B1015).
      intent = false;
      resumeWait = null;
      state.playing = false;
      state.loading = false;
      emit();
    }
  }
}

/**
 * MEDIA_ERR_SRC_NOT_SUPPORTED does NOT mean "this browser cannot decode
 * it". The element raises the same code when the source could not be
 * fetched at all, so a missing file and an unreachable upstream both
 * arrived here and were reported as a format problem — and, worse, the
 * track was marked permanently unplayable for the session, so an upstream
 * coming back up would not fix it.
 *
 * The server already distinguishes these: 404 for a track that is not
 * there, 503 upstream_unavailable for a routed track whose server is
 * down, and a 2xx for anything it served — which leaves decoding as the
 * only remaining explanation.
 */
async function handleSourceError(track, src, source) {
  const at = playbackGen;
  const reason = await classifySourceFailure(src);
  // The probe is a round trip, so the reader can have moved on. Every
  // line below writes shared playback state — the error text, the
  // skip-storm counter, the queue position — so a stale handler must
  // return having touched none of it.
  if (at !== playbackGen) return;
  // Only a genuine decode failure is permanent. Marking the others would
  // keep a track dead for the rest of the session over a condition that
  // may have already cleared.
  if (reason === "decode") markUnplayable(source);

  const name = track.title || track.path;
  state.skipped += 1;
  if (state.skipped >= SKIP_STORM_LIMIT) {
    // Stop rather than machine-gun through a whole DSD library. Worded
    // for the cause rather than assuming the browser: a storm of 503s is
    // one upstream being down, not a library the browser cannot play.
    state.error = reason === "decode"
      ? `The next tracks can't play in this browser. Use Download on the ones you want.`
      : `The next tracks could not be loaded. Check the source is reachable.`;
    intent = false;
    resumeWait = null;
    state.playing = false;
    state.loading = false;
    emit();
    return;
  }
  state.error = {
    decode: `Can't play "${name}" in this browser.`,
    missing: `"${name}" is no longer on this bridge.`,
    offline: `"${name}" is on an upstream server that isn't reachable right now.`,
  }[reason] || `Playback failed for "${name}".`;
  emit();
  // Guarded, because 1200ms is long enough for the reader to act on the
  // error they were just shown. advance() steps from the CURRENT index,
  // so a timer that fires after they paused, picked another track, or
  // replaced the queue would skip past whatever they chose — from a
  // failure they had already dealt with.
  //
  // Identity on the track, not the index: a replaced queue can hold a
  // different object at the same position.
  setTimeout(() => {
    // Both conditions: the generation catches a different track, a
    // replaced queue and a re-load of this same one, while the intent
    // catches a plain pause (the reader's before its pause event lands),
    // which a source refused before any play event never shows as `playing`.
    // The skip starts the element anew, under repeat one too, so it asks the
    // gate as a start does, and the generation and the intent are asked again
    // once the gate lets it go on (S1, Q3).
    if (intent && at === playbackGen) {
      continueStart((permit) => advanceNow(1, { auto: true, skip: true, permit }), { fresh: true });
    }
  }, 1200);
}

/**
 * Why the element could not use the source: "decode", "missing",
 * "offline", or "error" when the question cannot be answered.
 *
 * Asks for ONE byte and abandons the response as soon as the headers are
 * in. fetch resolves on headers and streams the body lazily, so aborting
 * there costs nothing even against a server that ignored the Range —
 * which a plain GET would have turned into a second full download of the
 * file that just failed.
 */
async function classifySourceFailure(url) {
  if (!url) return "error";
  const ctrl = new AbortController();
  // A source that accepts the connection and then sends no headers would
  // leave this pending forever — and with it the error message, the skip
  // and the advance. Aborting on a deadline turns that into an ordinary
  // "error", which the caller already handles.
  const deadline = setTimeout(() => ctrl.abort(), PROBE_TIMEOUT_MS);
  try {
    const r = await fetch(url, { headers: { range: "bytes=0-0" }, signal: ctrl.signal });
    clearTimeout(deadline);
    ctrl.abort();
    if (r.ok || r.status === 206) return "decode";
    if (r.status === 404 || r.status === 410) return "missing";
    if (r.status === 503) return "offline";
    return "error";
  } catch {
    clearTimeout(deadline);
    ctrl.abort();
    return "error";
  }
}

/**
 * Replace the queue and start at `start`, shuffled where shuffle says, once
 * the start gate lets it (R1): refused, nothing changes.
 */
export function playQueue(tracks, start = 0, { albumArt = null, shuffle } = {}) {
  readerAction();
  emit();
  requestStart((permit) => playQueueNow(tracks, start, { albumArt, shuffle, permit }));
}

function playQueueNow(tracks, start, { albumArt, shuffle, permit }) {
  init();
  state.queue = tracks.filter(Boolean).map((t) => ({ ...t }));
  state.albumArt = albumArt;
  if (shuffle !== undefined) state.shuffle = !!shuffle;
  state.skipped = 0;
  state.error = "";
  reshuffle();
  const idx = clampIndex(start);
  if (idx < 0) {
    state.error = "Nothing in this selection can play in a browser.";
    // Nothing will load, so nothing is being waited for. Without this a
    // spinner from the track the reader clicked a moment ago keeps
    // turning underneath a message saying the new selection cannot play.
    state.loading = false;
    emit();
    return;
  }
  load(idx, { autoplay: true, permit });
}

/** Append without disturbing what's playing. */
export function enqueue(tracks) {
  init();
  const add = tracks.filter(Boolean).map((t) => ({ ...t }));
  state.queue = state.queue.concat(add);
  reshuffle();
  maybePrime();
  persist();
  emit();
}

export function removeAt(i) {
  if (i < 0 || i >= state.queue.length) return;
  state.queue.splice(i, 1);
  if (i < state.index) state.index -= 1;
  else if (i === state.index) {
    // The row that plays is gone (no other removal is the reader's action):
    // its successor is loaded stopped, and plays only as a start does,
    // through the gate (S1); with none, the queue points at no track.
    readerAction();
    const play = intent;
    const next = clampIndex(state.index);
    if (next < 0) {
      intent = state.playing = state.loading = false;
      state.index = -1;
      abandon(state.el);
    } else load(next, { autoplay: false });
    if (play && next >= 0) {
      intent = true;
      continueStart((permit) => playNow(permit), { fresh: true });
    }
  }
  reshuffle();
  maybePrime();
  persist();
  emit();
}

export function clearQueue() {
  playbackGen += 1;
  intent = false;
  readerAction();
  state.el?.pause();
  // A Stop lets go of the starts the gate holds, as the explicit Pause does:
  // a row waiting for the epoch or behind its barrier plays nothing.
  startCancel?.();
  state.queue = [];
  state.index = -1;
  state.playing = false;
  state.loading = false;
  abandon(state.el);
  cancelPrime();
  persist();
  emit();
}

function clampIndex(from) {
  for (let i = Math.max(0, from); i < state.queue.length; i++) {
    if (playableAt(i)) return i;
  }
  for (let i = 0; i < Math.max(0, from); i++) {
    if (playableAt(i)) return i;
  }
  return -1;
}

function playableAt(i) {
  const t = state.queue[i];
  return t && resolvePlayable(t, audioURL) !== null;
}

function load(index, { autoplay, permit }) {
  const track = state.queue[index];
  if (!track) return;
  const target = resolvePlayable(track, audioURL);
  if (!target) return;
  playbackGen += 1;
  heldAt = null;
  // A resume marked for the skip past the track that failed goes with it.
  if (resumeWait?.skip) resumeWait = null;
  // A start's permit stays the playback's, through the tracks it advances
  // to, until another start replaces it.
  if (permit !== undefined) startPermit = permit;
  // A load says whether this browser means to play what it loads (S2).
  intent = !!autoplay;
  state.index = index;
  state.degraded = target.degraded;
  state.seekable = true;
  state.error = "";
  // The wait starts at the CLICK, not at the element's first `waiting`.
  // Asserting it here is what puts the spinner up on the same frame the
  // title changes — otherwise the bar spends a beat looking like it is
  // playing something silently, which is the whole complaint.
  state.loading = !!autoplay;
  state.el.src = target.url;
  state.source = target.key;
  loadEl(state.el);
  updateMetadata(track);
  if (autoplay) {
    // A rejected play() outside a user gesture is NotAllowedError.
    // Surfacing it is the point: a swallowed rejection reads as a
    // broken player.
    //
    // Generation-guarded for the same reason handleSourceError is, and
    // it is not theoretical: an INTERRUPTED play() rejects too. Measured
    // in Chrome — clicking a second track while the first is still
    // loading rejects the first with AbortError, "The play() request was
    // interrupted by a new load request". Unguarded, that handler then
    // clears the spinner and the playing flag of the track the reader
    // actually chose.
    //
    // The damage is bounded, and the bound is worth writing down rather
    // than overstating: the rejection is a MICROTASK queued by the
    // interrupting load, and the new track's `play` event is a TASK
    // queued after it, so the clear is always undone one tick later.
    // Measured on the guardless build, cleared at t=16 ms and restored
    // at t=24 ms against a first `playing` at 1,926 ms — an 8 ms flicker
    // of both flags, not the 1.9 s loss the ordering might suggest. It
    // is guarded because the ordering is the only thing making it small,
    // and because a stale NotAllowedError would otherwise write "Press
    // play to start." about a track nobody is waiting on.
    playEl();
  }
  // Whatever the preloader was warming is for a track that is no longer
  // next — and if it IS this one, its bytes are in the HTTP cache, not
  // in that element. Either way it must stop pulling.
  cancelPrime();
  persist();
  emit();
}

/**
 * Warm the next track's bytes near the end of this one.
 *
 * Not true gapless — that needs Web Audio with decoded buffers, which
 * for a hi-res library means decoding hundreds of megabytes in JS.
 * This just gets the connection and the first bytes out of the way, so
 * the gap is tens of milliseconds instead of hundreds.
 *
 * "Near the end" was a comment rather than a behaviour: this ran from
 * load(), so pressing play opened a SECOND preload:auto download at the
 * same instant, for a track that would not be wanted for another three
 * minutes, competing for the link with the one the reader was waiting
 * for. Measured against a throttled 200 KB/s bridge with 4-minute
 * 44.1/16 FLACs: at the moment the current track had 72 s buffered the
 * preloader already held 45 s of the NEXT one — roughly 40% of the link
 * spent ahead of a track nobody had asked for yet. On a LAN it is
 * invisible; over a relayed Tailscale link it is the difference between
 * playing and rebuffering, and it made the cold start the complaint was
 * about measurably longer.
 *
 * Driven from timeupdate rather than from load() for that reason, and
 * because the next track is not a fixed answer: shuffle, a queue edit
 * or a repeat change between here and the end would leave a prime
 * warming the wrong file.
 *
 * Which is why every mutator that can MOVE that answer calls this too —
 * setShuffle, cycleRepeat, enqueue, removeAt. timeupdate does not fire
 * while paused, so a reader who pauses near the end of a track and then
 * reshuffles would otherwise leave the preloader downloading a track
 * that is no longer next, with nothing to notice until playback resumes.
 * Calling it costs nothing when the answer has not moved: the primedFor
 * check returns immediately.
 */
function maybePrime() {
  if (!state.pre || !state.el) return;
  // repeat-one plays THIS track again, so the queue's next entry is not
  // what comes next.
  const next = state.repeat === "one" ? null : peek(1);
  // A prime whose track is no longer the next one is pure competition:
  // shuffle, a queue edit or a repeat change can all move that answer
  // out from under a download already in flight.
  if (primedFor && primedFor !== next) cancelPrime();
  if (!next || primedFor === next) return;
  const dur = state.el.duration;
  // A source that reported no length — an upstream that ignored Range —
  // cannot answer "how near the end are we". Fall back to the other
  // signal that the link has room to spare: the element saying it
  // expects to reach the end without stopping.
  const near = Number.isFinite(dur) && dur > 0
    ? dur - state.el.currentTime <= PRIME_LEAD_S
    : state.el.readyState >= HAVE_ENOUGH_DATA;
  if (!near) return;
  const target = resolvePlayable(next, audioURL);
  if (!target) return;
  state.pre.preload = "auto";
  state.pre.src = target.url;
  primedFor = next;
}

/** Abandon whatever the preloader is holding. */
function cancelPrime() {
  primedFor = null;
  abandon(state.pre);
}

/**
 * Stop `el` pulling bytes for the resource it is holding.
 *
 * Neither pause() nor removeAttribute does it. Measured against a
 * throttled 60 KB/s bridge on a 40 MB source, reading networkState and
 * buffered.end: playing at 1.7 s buffered / NETWORK_LOADING; three
 * seconds after pause(), 4.3 s / still LOADING; three seconds after
 * removeAttribute("src"), 6.7 s and STILL growing. Only load() takes it
 * to NETWORK_EMPTY and drops the buffer. A paused element filling its
 * buffer is not an anomaly — it is exactly what makes the preloader work.
 *
 * Shared because the two callers diverged inside a single change: the
 * preloader's cancel had the load() and clearQueue's did not, 120 lines
 * apart, so clearing the queue left the current track downloading to the
 * end in the background. One definition rather than two sites each
 * remembering. On an element with no src it is a no-op and logs nothing.
 */
function abandon(el) {
  if (!el?.getAttribute("src")) return;
  el.removeAttribute("src");
  loadEl(el);
}

function peek(delta) {
  const order = state.shuffleOrder;
  if (order) {
    const pos = order.indexOf(state.index);
    const nextPos = pos + delta;
    if (nextPos < 0 || nextPos >= order.length) {
      return state.repeat === "all" ? state.queue[order[(nextPos + order.length) % order.length]] : null;
    }
    return state.queue[order[nextPos]];
  }
  const i = state.index + delta;
  if (i < 0 || i >= state.queue.length) {
    return state.repeat === "all" ? state.queue[(i + state.queue.length) % state.queue.length] : null;
  }
  return state.queue[i];
}

/**
 * Move delta tracks and play there: a reader's advance (Next, Previous, a
 * lock-screen control), which starts the element and asks the start gate
 * (R1). The element's own end of a track and a skip past one that cannot
 * play go on by themselves, through continueStart (S1).
 */
export function advance(delta) {
  if (state.index < 0) return;
  readerAction();
  emit();
  requestStart((permit) => advanceNow(delta, { permit }));
}

function advanceNow(delta, { auto = false, skip = false, permit } = {}) {
  if (state.index < 0) return;
  if (auto && !skip && state.repeat === "one") {
    // Re-seek rather than reassigning src: no round-trip, no decode
    // restart.
    if (permit !== undefined) startPermit = permit;
    intent = true;
    plays += 1;
    state.el.currentTime = 0;
    playEl();
    return;
  }
  const order = state.shuffleOrder;
  let nextIndex;
  if (order) {
    const pos = order.indexOf(state.index);
    let nextPos = pos + delta;
    if (nextPos < 0 || nextPos >= order.length) {
      if (state.repeat !== "all") { stopAtEnd(); return; }
      nextPos = (nextPos + order.length) % order.length;
    }
    nextIndex = order[nextPos];
  } else {
    nextIndex = state.index + delta;
    if (nextIndex < 0 || nextIndex >= state.queue.length) {
      if (state.repeat !== "all") { stopAtEnd(); return; }
      nextIndex = (nextIndex + state.queue.length) % state.queue.length;
    }
  }
  const playable = nextPlayableFrom(nextIndex, delta);
  if (playable < 0) { stopAtEnd(); return; }
  if (!auto) state.skipped = 0;
  load(playable, { autoplay: true, permit });
}

// nextPlayableFrom walks the queue from `start` in the direction of
// `delta`, returning the first index that can actually play, or -1.
//
// Written with an untouched loop counter and an explicit wrap. The
// previous form reassigned `i` inside the body — which works, since a
// `let` in the for-head carries the mutation into the update
// expression — but it also hid a latent bug: its wrap was a single
// `(i + len) % len`, and JS % keeps the sign of the dividend, so for
// |i| > len it never normalised (i = -3, len = 2 gives -1, still
// negative). Verified by running both forms over every combination of
// length, repeat mode, playable-subset and start offset: IDENTICAL
// across all 1,728 inputs `advance()` can actually produce (it clamps
// start into [-1, len]), and divergent only outside that range, where
// the old one was wrong. So this is not a behaviour change today — it
// is the same function with the unreachable corner made correct.
function nextPlayableFrom(start, delta) {
  const len = state.queue.length;
  if (len === 0) return -1;
  const step = delta >= 0 ? 1 : -1;
  // With shuffle on, walk SHUFFLE POSITIONS and map each candidate back
  // through the order. Walking raw queue indices is wrong and was the
  // pre-existing behaviour: advance() hands this a queue index taken
  // from the shuffled order, so skipping an unplayable track landed on
  // the raw-adjacent entry instead of the next shuffled one. With
  // order [0,1,3,2] at position 0 and queue index 1 unplayable, it
  // returned 2 where the shuffle-correct answer is 3.
  const order = state.shuffleOrder;
  const origin = order ? order.indexOf(start) : start;
  // order is a permutation of every index, so a valid start is always
  // present; -1 means the caller passed something out of range.
  if (order && origin < 0) return -1;
  for (let n = 0; n < len; n++) {
    const raw = origin + step * n;
    if ((raw < 0 || raw >= len) && state.repeat !== "all") return -1;
    // JS % keeps the sign of the dividend, so a negative index needs
    // the +len before the second modulo.
    const position = ((raw % len) + len) % len;
    const i = order ? order[position] : position;
    if (playableAt(i)) return i;
  }
  return -1;
}

function stopAtEnd() {
  intent = false;
  state.playing = false;
  state.loading = false;
  state.el.pause();
  emit();
}

/** Pause at once, or play through the start gate (R1, V7). */
export function toggle() {
  init();
  if (state.index < 0) return;
  if (resumeWait && !resumeWait.skip) {
    // The browser's own resume waits on the gate: Play/Pause cancels it (W4),
    // and with it what this browser meant to play, which an error's retry
    // reads (B1015). One marked for a skip is no wait the bar shows: Play is
    // over the failed track, a start.
    intent = false;
    readerAction();
    emit();
    return;
  }
  // A fetch the user aborted (code 1) fires no pause and is no failure: the
  // bar's Pause still shown pauses, and Play over it once stopped is Play.
  const failed = state.el.error && state.el.error.code !== 1;
  if (!state.el.paused && !failed) {
    pauseNow();
    return;
  }
  readerAction();
  // Over a failed element Play is a start: when it runs, a source it cannot
  // use moves on, a network or decode error reloads, its retry there again.
  requestStart((permit) => {
    const code = state.el.error?.code;
    if (code === 4) return advanceNow(1, { permit });
    if (code !== 2 && code !== 3) return playNow(permit);
    delete state.queue[state.index]?._retried;
    state.skipped = 0;
    load(state.index, { autoplay: true, permit });
  });
}

/** Pause at once: the bar's Pause and the lock screen's, which a failed track never makes a start. */
function pauseNow() {
  readerAction();
  intent = false;
  if (!state.el.paused) state.el.pause();
  // The explicit Pause lets go of a start the gate holds; the element's
  // pause event, which a browser fires a task later, never does.
  startCancel?.();
  // Shown stopped at once, before that event, so the Play the bar then shows
  // is a Play; a positioned handover's wait is paused already, and its
  // element fires no pause event to say so.
  state.playing = false;
  state.loading = false;
  emit();
}

export function seek(seconds) {
  if (!state.el || !state.seekable) return;
  state.el.currentTime = Math.max(0, seconds);
}

export function setVolume(v) {
  if (state.el) state.el.volume = Math.min(1, Math.max(0, v));
}

export function setShuffle(on) {
  state.shuffle = !!on;
  reshuffle();
  maybePrime();
  persist();
  emit();
}

const REPEAT_CYCLE = { off: "all", all: "one", one: "off" };

export function cycleRepeat() {
  // off -> all -> one -> off. A table, not a chained ternary: the cycle is
  // data, and the fall-through keeps the old ladder's answer for a persisted
  // value this build does not know.
  //
  // Object.hasOwn, NOT `REPEAT_CYCLE[state.repeat] ?? "off"`: a plain lookup
  // reads INHERITED properties, so a persisted "__proto__" / "constructor" /
  // "toString" returns an object or a function — truthy, so `??` never fires —
  // and state.repeat stops being a string. The chained ternary this replaced
  // answered "off" for all four. (CodeRabbit on #949.)
  state.repeat = Object.hasOwn(REPEAT_CYCLE, state.repeat)
    ? REPEAT_CYCLE[state.repeat]
    : "off";
  maybePrime();
  persist();
  emit();
}

/**
 * Shuffle is a PERMUTATION generated once when it turns on, not a
 * random pick per next(). The random-per-next form is the classic bug
 * where one track plays three times in a row and turning shuffle off
 * loses your place.
 */
// randomBelow returns a uniformly-distributed integer in [0, n).
//
// Uses crypto.getRandomValues rather than Math.random. A shuffle order
// is a listening preference and not a security decision, so this is not
// a threat-model change — it is that the CSPRNG is available
// unconditionally here and costs nothing, which makes the argument
// moot rather than won. crypto.getRandomValues is no secure-context
// API: it is there on the console the bridge opens on the home network
// (adminLANAddress, backlog B989), plain http on a private address that
// is no secure context, as on loopback and behind TLS. What that page
// lacks is crypto.subtle, which this does not use.
//
// Rejection sampling rather than a plain modulo: 2^32 is not a multiple
// of most n, so `value % n` would bias the low indices. The discarded
// range is under one part in 2^32/n, so the loop effectively never
// repeats.
function randomBelow(n) {
  if (n <= 1) return 0;
  const limit = Math.floor(0x100000000 / n) * n;
  const buf = new Uint32Array(1);
  let v;
  do {
    crypto.getRandomValues(buf);
    v = buf[0];
  } while (v >= limit);
  return v % n;
}

function reshuffle() {
  if (!state.shuffle || state.queue.length === 0) {
    state.shuffleOrder = null;
    return;
  }
  const order = state.queue.map((_, i) => i);
  for (let i = order.length - 1; i > 0; i--) {
    const j = randomBelow(i + 1);
    [order[i], order[j]] = [order[j], order[i]];
  }
  if (state.index >= 0) {
    // Keep the current track first so turning shuffle on doesn't jump.
    const at = order.indexOf(state.index);
    if (at > 0) [order[0], order[at]] = [order[at], order[0]];
  }
  state.shuffleOrder = order;
}

// ---- The bar's output (B634) ----
//
// The switch to the bridge's player and back (§7.3, §7.4). output.js
// decides; these are the browser's halves of it.

/**
 * The browser's handover: its queue as listed (never a shuffle order), its
 * current track, whether it plays and its position in whole seconds, read
 * from the player's own state BEFORE the source is let go (G11), since
 * letting go resets the element's position. A browser already suspended for
 * the bridge (a pick of the bridge made again while the first is
 * unconfirmed, R3) hands over what it held when it stopped.
 */
export function handover() {
  const track = state.index >= 0 ? state.queue[state.index] : null;
  const held = heldAt !== null;
  return {
    queue: state.queue.map((t) => ({ ...t })),
    index: state.index,
    playing: held ? heldPlaying : state.playing,
    position: held ? heldAt : (state.el ? Math.floor(state.el.currentTime || 0) : 0),
    pcm: !!track && !track.isDsd,
  };
}

/** Whether the browser holds a queue to hand over. */
export function hasQueue() {
  return state.queue.length > 0;
}

/**
 * Stop the browser for the bridge: everything that was pending against the
 * track it played is now stale (a failure probe, a play rejection, the
 * restored position's seek: M3, P6), its source is let go, and the saved
 * queue keeps the position the handover read, not the element's reset one.
 */
export function suspend(h) {
  playbackGen += 1;
  intent = false;
  readerAction();
  heldAt = h && h.position > 0 ? h.position : 0;
  heldPlaying = !!h?.playing;
  state.el?.pause();
  state.playing = false;
  state.loading = false;
  abandon(state.el);
  cancelPrime();
  persist();
  emit();
}

/**
 * Load the track at index and, for a position, seek the element there
 * before it plays, so nothing of the track's start is heard (§7.4). A play
 * the metadata defers asks the start's permit again just before it (V4):
 * a newer pick, or a state showing the bridge playing since, plays nothing.
 */
function loadAt(index, { autoplay, at, permit }) {
  if (!(at > 0)) {
    load(index, { autoplay, permit });
    return;
  }
  load(index, { autoplay: false, permit });
  intent = !!autoplay;
  const mine = playbackGen;
  state.loading = !!autoplay;
  state.el.addEventListener("loadedmetadata", function once() {
    state.el.removeEventListener("loadedmetadata", once);
    if (mine !== playbackGen) return;
    if (Number.isFinite(state.el.duration)) state.el.currentTime = Math.min(at, state.el.duration);
    // One start a playback: none once the reader's Play plays it, or a pause.
    if (!autoplay || !intent || !state.el.paused) return;
    if (permit && !permit()) {
      // Refused, it ends what this browser meant to play (round 4, T2).
      intent = false;
      state.playing = false;
      state.loading = false;
      emit();
      return;
    }
    // A start let go on where no player was offered holds no permit: its
    // deferred play asks the gate as it is now, so a player offered since
    // that plays refuses it (round 4, W9).
    if (permit === null) {
      continueStart((p) => {
        startPermit = p;
        playEl();
      }, { fresh: true });
      return;
    }
    playEl();
  });
  emit();
}

/**
 * The browser's resume of its own queue at the position it read before it
 * stopped: called only on a barrier's done answered now (§3.2, G1), with
 * its permit.
 */
export function resume(h, permit) {
  init();
  if (!h || h.index < 0 || !state.queue[h.index]) return;
  loadAt(h.index, { autoplay: !!h.playing, at: h.pcm ? h.position : 0, permit });
}

/**
 * The browser takes the bridge's queue (a switch-out's handover): its
 * tracks, the selected one current and, for PCM, its position; it plays
 * only where play says, which output.js decides on a barrier's done, with
 * its permit. A selected track this browser cannot play stays shown,
 * paused, with its reason, and Next moves to one it plays (§7.4).
 */
export function take(h, play, permit) {
  init();
  playbackGen += 1;
  intent = false;
  readerAction();
  heldAt = null;
  state.queue = (h?.queue || []).filter(Boolean).map((t) => ({ ...t }));
  state.skipped = 0;
  state.error = "";
  reshuffle();
  const idx = h && h.index >= 0 && h.index < state.queue.length ? h.index : clampIndex(0);
  const track = state.queue[idx];
  if (!track) {
    state.index = -1;
    persist();
    emit();
    return;
  }
  if (!resolvePlayable(track, audioURL)) {
    state.index = idx;
    state.playing = false;
    state.loading = false;
    abandon(state.el);
    state.error = `"${track.title || track.path}" is not playable in this browser.`;
    updateMetadata(track);
    persist();
    emit();
    return;
  }
  // The position where the handover has one: PCM's, which the element seeks
  // to before it plays; a DSD track's FLAC sidecar is seekable here too.
  loadAt(idx, { autoplay: !!play, at: h.position > 0 ? h.position : 0, permit });
}

/**
 * Start what the browser holds: on a barrier's done (G8), with its permit,
 * or for a toggle's play the start gate let through (R1).
 */
export function start(permit) {
  init();
  playNow(permit);
}

function playNow(permit) {
  if (state.index < 0 || !state.queue[state.index]) return;
  intent = true;
  if (!state.el.getAttribute("src")) {
    loadAt(state.index, { autoplay: true, at: heldAt || 0, permit });
    return;
  }
  if (permit !== undefined) startPermit = permit;
  playEl();
}

/**
 * The MediaSession while an output is chosen: the browser's handlers,
 * metadata and playback state with this browser, and none of them with the
 * bridge's player, whose sound is not this page's (§7.5).
 */
export function setMediaSession(active) {
  sessionActive = !!active;
  const ms = navigator.mediaSession;
  if (!ms) return;
  if (!sessionActive) {
    for (const action of SESSION_ACTIONS) {
      try { ms.setActionHandler(action, null); } catch { /* unsupported action */ }
    }
    ms.metadata = null;
    ms.playbackState = "none";
    return;
  }
  wireMediaSession();
  const track = state.index >= 0 ? state.queue[state.index] : null;
  if (track) updateMetadata(track);
  ms.playbackState = state.playing ? "playing" : "paused";
}

// ---- MediaSession ----

/** Every action the browser sets a handler for. */
const SESSION_ACTIONS = ["play", "pause", "previoustrack", "nexttrack", "stop", "seekto", "seekbackward", "seekforward"];

function wireMediaSession() {
  const ms = navigator.mediaSession;
  if (!ms || !sessionActive) return;
  const set = (action, fn) => {
    try { ms.setActionHandler(action, fn); } catch { /* unsupported action */ }
  };
  set("play", () => toggle());
  set("pause", () => pauseNow());
  set("previoustrack", () => advance(-1));
  set("nexttrack", () => advance(1));
  set("stop", () => clearQueue());
  set("seekto", (d) => { if (d && typeof d.seekTime === "number") seek(d.seekTime); });
  set("seekbackward", (d) => seek(state.el.currentTime - (d?.seekOffset || 10)));
  set("seekforward", (d) => seek(state.el.currentTime + (d?.seekOffset || 10)));
}

function updateMetadata(track) {
  const ms = navigator.mediaSession;
  if (!ms || !window.MediaMetadata || !sessionActive) return;
  const artwork = state.albumArt ? [{ src: state.albumArt, sizes: "500x500", type: "image/jpeg" }] : [];
  ms.metadata = new MediaMetadata({
    title: track.title || track.path,
    artist: track.artist || "",
    album: formatChip(track),
    artwork,
  });
}

function setSessionState(s) {
  if (navigator.mediaSession && sessionActive) navigator.mediaSession.playbackState = s;
}

// Throttled to ~1 Hz: spamming setPositionState makes some OS widgets
// jitter, and nothing needs finer resolution than a second.
function updatePositionState() {
  const ms = navigator.mediaSession;
  if (!ms || !ms.setPositionState || !state.seekable) return;
  try {
    ms.setPositionState({
      duration: state.el.duration,
      playbackRate: state.el.playbackRate,
      position: Math.min(state.el.currentTime, state.el.duration),
    });
  } catch { /* a mid-load duration change can throw; harmless */ }
}

// ---- Persistence ----
//
// The queue survives a navigation to a server page (Stats, Settings,
// Server), which is a full page load. It restores PAUSED with a resume
// control: play() outside a user gesture rejects, and a silent
// rejection reads as a broken player.

function persist() {
  try {
    // A suspended browser keeps the position its handover read: its
    // element's was reset when the source was let go (§7.3, step 1).
    const at = heldAt !== null ? heldAt : (state.el ? state.el.currentTime : 0);
    sessionStorage.setItem(STORE_KEY, JSON.stringify({
      queue: state.queue, index: state.index, shuffle: state.shuffle,
      repeat: state.repeat, at,
      albumArt: state.albumArt,
    }));
  } catch { /* private mode / quota — playback still works */ }
}

function restore() {
  let saved;
  try {
    saved = JSON.parse(sessionStorage.getItem(STORE_KEY) || "null");
  } catch { return; }
  if (!saved || !Array.isArray(saved.queue) || saved.queue.length === 0) return;
  // Restored paused: nothing here means to play until the reader's Play.
  intent = false;
  state.queue = saved.queue;
  state.shuffle = !!saved.shuffle;
  state.repeat = saved.repeat || "off";
  state.albumArt = saved.albumArt || null;
  reshuffle();
  const idx = Math.min(Math.max(0, Math.trunc(saved.index) || 0), state.queue.length - 1);
  const track = state.queue[idx];
  const target = track && resolvePlayable(track, audioURL);
  if (!target) return;
  state.index = idx;
  state.el.src = target.url;
  state.source = target.key;
  // Generation-guarded, as every continuation of playback is: a switch of
  // the bar's output, or any later load, makes this seek stale, and it must
  // not seek another track to this one's position (P6).
  const mine = playbackGen;
  state.el.addEventListener("loadedmetadata", function once() {
    state.el.removeEventListener("loadedmetadata", once);
    if (mine !== playbackGen) return;
    if (saved.at > 0 && Number.isFinite(state.el.duration)) state.el.currentTime = saved.at;
  });
  updateMetadata(track);
  emit();
}

window.addEventListener("pagehide", persist);

function throttle(fn, ms) {
  let last = 0;
  return (...args) => {
    const now = Date.now();
    if (now - last < ms) return;
    last = now;
    fn(...args);
  };
}
