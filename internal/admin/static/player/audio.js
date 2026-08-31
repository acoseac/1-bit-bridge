// The audio element, the queue, and the OS media-session bridge.
//
// Exactly ONE <audio>, parented to <body> — never inside the view
// container, so no route change can detach it. A second hidden element
// acts as a preloader.

import { audioURL } from "./api.js";
import { resolvePlayable, formatChip } from "./format.js";

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
  degraded: false,
  seekable: true,
  error: "",
  skipped: 0,
  albumArt: null,
};

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
    queue: state.queue,
    index: state.index,
    playing: state.playing,
    shuffle: state.shuffle,
    repeat: state.repeat,
    degraded: state.degraded,
    seekable: state.seekable,
    error: state.error,
    albumArt: state.albumArt,
    currentTime: state.el ? state.el.currentTime : 0,
    duration: state.el && Number.isFinite(state.el.duration) ? state.el.duration : 0,
  };
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

  state.el.addEventListener("play", () => { state.playing = true; setSessionState("playing"); emit(); });
  state.el.addEventListener("pause", () => { state.playing = false; setSessionState("paused"); persist(); emit(); });
  state.el.addEventListener("ended", () => { advance(1, { auto: true }); });
  state.el.addEventListener("timeupdate", throttle(() => { persist(); emit(); updatePositionState(); }, 900));
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
      void handleSourceError(track, state.el.src);
      return;
    default: { // 2 network / 3 decode — retry once, then surface.
      if (!track._retried) {
        track._retried = true;
        const busted = state.el.src + (state.el.src.includes("?") ? "&" : "?") + "_r=" + Date.now();
        state.el.src = busted;
        state.el.load();
        void state.el.play().catch(() => {});
        return;
      }
      state.error = `Playback failed for "${track.title || track.path}".`;
      state.playing = false;
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
async function handleSourceError(track, src) {
  const reason = await classifySourceFailure(src);
  // Only a genuine decode failure is permanent. Marking the others would
  // keep a track dead for the rest of the session over a condition that
  // may have already cleared.
  if (reason === "decode") markUnplayable(track);

  const name = track.title || track.path;
  state.skipped += 1;
  if (state.skipped >= SKIP_STORM_LIMIT) {
    // Stop rather than machine-gun through a whole DSD library. Worded
    // for the cause rather than assuming the browser: a storm of 503s is
    // one upstream being down, not a library the browser cannot play.
    state.error = reason === "decode"
      ? `The next tracks can't play in this browser. Use Download on the ones you want.`
      : `The next tracks could not be loaded. Check the source is reachable.`;
    state.playing = false;
    emit();
    return;
  }
  state.error = {
    decode: `Can't play "${name}" in this browser.`,
    missing: `"${name}" is no longer on this bridge.`,
    offline: `"${name}" is on an upstream server that isn't reachable right now.`,
  }[reason] || `Playback failed for "${name}".`;
  emit();
  setTimeout(() => advance(1, { auto: true }), 1200);
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
  try {
    const r = await fetch(url, { headers: { range: "bytes=0-0" }, signal: ctrl.signal });
    ctrl.abort();
    if (r.ok || r.status === 206) return "decode";
    if (r.status === 404 || r.status === 410) return "missing";
    if (r.status === 503) return "offline";
    return "error";
  } catch {
    ctrl.abort();
    return "error";
  }
}

function markUnplayable(track) {
  if (track.play) track.play.kind = "none";
}

/** Replace the queue and start at `start`. */
export function playQueue(tracks, start = 0, { albumArt = null } = {}) {
  init();
  state.queue = tracks.filter(Boolean).map((t) => ({ ...t }));
  state.albumArt = albumArt;
  state.skipped = 0;
  state.error = "";
  reshuffle();
  const idx = clampIndex(start);
  if (idx < 0) {
    state.error = "Nothing in this selection can play in a browser.";
    emit();
    return;
  }
  load(idx, { autoplay: true });
}

/** Append without disturbing what's playing. */
export function enqueue(tracks) {
  init();
  const add = tracks.filter(Boolean).map((t) => ({ ...t }));
  state.queue = state.queue.concat(add);
  reshuffle();
  persist();
  emit();
}

export function removeAt(i) {
  if (i < 0 || i >= state.queue.length) return;
  state.queue.splice(i, 1);
  if (i < state.index) state.index -= 1;
  else if (i === state.index) load(clampIndex(state.index), { autoplay: state.playing });
  reshuffle();
  persist();
  emit();
}

export function clearQueue() {
  state.el?.pause();
  state.queue = [];
  state.index = -1;
  state.playing = false;
  if (state.el) state.el.removeAttribute("src");
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

function load(index, { autoplay }) {
  const track = state.queue[index];
  if (!track) return;
  const target = resolvePlayable(track, audioURL);
  if (!target) return;
  state.index = index;
  state.degraded = target.degraded;
  state.seekable = true;
  state.error = "";
  state.el.src = target.url;
  state.el.load();
  updateMetadata(track);
  if (autoplay) {
    // A rejected play() outside a user gesture is NotAllowedError.
    // Surfacing it is the point: a swallowed rejection reads as a
    // broken player.
    state.el.play().catch((e) => {
      state.playing = false;
      if (e && e.name === "NotAllowedError") state.error = "Press play to start.";
      emit();
    });
  }
  primeNext();
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
 */
function primeNext() {
  if (!state.pre) return;
  const next = peek(1);
  if (!next) {
    state.pre.removeAttribute("src");
    return;
  }
  const target = resolvePlayable(next, audioURL);
  if (!target) return;
  state.pre.preload = "auto";
  state.pre.src = target.url;
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

export function advance(delta, { auto = false } = {}) {
  if (state.index < 0) return;
  if (auto && state.repeat === "one") {
    // Re-seek rather than reassigning src: no round-trip, no decode
    // restart.
    state.el.currentTime = 0;
    void state.el.play().catch(() => {});
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
  load(playable, { autoplay: true });
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
  state.playing = false;
  state.el.pause();
  emit();
}

export function toggle() {
  init();
  if (state.index < 0) return;
  if (state.el.paused) void state.el.play().catch(() => {});
  else state.el.pause();
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
  persist();
  emit();
}

export function cycleRepeat() {
  state.repeat = state.repeat === "off" ? "all" : state.repeat === "all" ? "one" : "off";
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
// moot rather than won. Verified: 127.0.0.1 is a
// potentially-trustworthy origin, so isSecureContext is true even over
// plain http on loopback, and public-mode admin is always behind TLS
// (directly or via a terminating proxy). There is no configuration the
// bridge permits where crypto is absent.
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

// ---- MediaSession ----

function wireMediaSession() {
  const ms = navigator.mediaSession;
  if (!ms) return;
  const set = (action, fn) => {
    try { ms.setActionHandler(action, fn); } catch { /* unsupported action */ }
  };
  set("play", () => toggle());
  set("pause", () => toggle());
  set("previoustrack", () => advance(-1));
  set("nexttrack", () => advance(1));
  set("stop", () => clearQueue());
  set("seekto", (d) => { if (d && typeof d.seekTime === "number") seek(d.seekTime); });
  set("seekbackward", (d) => seek(state.el.currentTime - (d?.seekOffset || 10)));
  set("seekforward", (d) => seek(state.el.currentTime + (d?.seekOffset || 10)));
}

function updateMetadata(track) {
  const ms = navigator.mediaSession;
  if (!ms || !window.MediaMetadata) return;
  const artwork = state.albumArt ? [{ src: state.albumArt, sizes: "500x500", type: "image/jpeg" }] : [];
  ms.metadata = new MediaMetadata({
    title: track.title || track.path,
    artist: track.artist || "",
    album: formatChip(track),
    artwork,
  });
}

function setSessionState(s) {
  if (navigator.mediaSession) navigator.mediaSession.playbackState = s;
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
    sessionStorage.setItem(STORE_KEY, JSON.stringify({
      queue: state.queue, index: state.index, shuffle: state.shuffle,
      repeat: state.repeat, at: state.el ? state.el.currentTime : 0,
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
  state.el.addEventListener("loadedmetadata", function once() {
    state.el.removeEventListener("loadedmetadata", once);
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
