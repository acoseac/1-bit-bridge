// The persistent now-playing bar.
//
// Parented to <body>, outside #player-root, because it must survive
// every client-side route change — and, with the sessionStorage
// restore in audio.js, be able to reappear after a full page load onto
// a server page.
//
// Layout is the three-zone one every media player converges on: what is
// playing (left), the controls (centre), the extras (right). The prior
// bar put transport, scrubber and toggles in ONE row of a five-column
// grid, which left the scrubber fighting the metadata for width and put
// shuffle/repeat off in a corner where nothing related them to the
// transport they modify.
//
// Icons come from the layout.html sprite rather than emoji. The emoji
// they replace were not merely inconsistent across platforms: 🔀 and 🔁
// render as COLOUR emoji, so `color: var(--accent)` never reached them
// and the active state was invisible — the reported "repeat looks the
// same whether it is on or off" was that, not a missing rule.
//
// The bar plays to one of two outputs (B634): this browser, or the
// bridge's own player, picked with the output button in its right zone.
// With the bridge's player chosen the bar shows the engine's state, as
// output.js orders it, and enables only the commands the engine takes now;
// shuffle, repeat-one, volume and mute are disabled with their words, Stop
// is shown, and a Queue button opens the engine's queue. On a box that
// offers its player the bar stays, as an empty strip, with nothing chosen,
// so the bridge's player can be picked before any track is (§7.1).

import { el, clear, link, setDisabled } from "./ui.js";
import { duration, formatChip, signalChip } from "./format.js";
import * as audio from "./audio.js";
import { currentOutput, PHASE, WORDS, PROMISE, ROUTES, whyWord, holdWord } from "./output.js";

let bar = null;
let refs = {};

/** Human name for each repeat mode, used as the button's accessible name. */
const REPEAT_LABEL = { off: "Repeat off", all: "Repeat all", one: "Repeat this track" };

/** The bridge's player repeats the whole queue or nothing (§7.5). */
const BRIDGE_REPEAT_LABEL = { off: "Repeat off", all: "Repeat all" };

/** The output the bar drives now. */
function out() {
  return currentOutput();
}

function bridgeChosen(o) {
  return !!o && o.output === "bridge" && !o.notOffered;
}

export function mount() {
  if (bar) return;
  bar = el("div", { class: "npbar", attrs: { id: "nowplaying", role: "region", "aria-label": "Now playing" } });
  bar.hidden = true;

  // A non-interactive progress line pinned to the bar's top edge. It is
  // the ONLY progress indication left below the phone breakpoint, where
  // the numeric scrubber row is hidden — before this, a phone got no
  // position feedback at all.
  refs.rail = el("div", { class: "np-rail", attrs: { "aria-hidden": "true" } });

  refs.art = el("div", { class: "np-art" });
  refs.title = el("span", { class: "np-title" });
  refs.sub = el("span", { class: "np-sub" });
  refs.prev = iconButton("Previous", "prev", () => onPrevious());
  refs.play = iconButton("Play", "play", () => onPlay(), "play");
  refs.next = iconButton("Next", "next", () => onNext());
  // Stop is the bridge's alone: it lets the DAC go at once, where a pause
  // lets it go after 30 s, and it ends the DSD check (§7.5).
  refs.stop = iconButton("Stop", "stop", () => out()?.command("stop"));
  refs.stop.hidden = true;
  refs.elapsed = el("span", { class: "np-time", text: "0:00" });
  refs.total = el("span", { class: "np-time np-time-total", text: "0:00" });

  // A styled <input type="range">, not a rebuilt div: keyboard seeking,
  // screen-reader value announcement and Windows High Contrast all come
  // free, and a div-based slider gets none of them.
  refs.seek = el("input", {
    class: "np-seek",
    attrs: { type: "range", min: "0", max: "1000", value: "0", "aria-label": "Seek" },
  });
  let scrubbing = false;
  refs.seek.addEventListener("input", () => { scrubbing = true; setProgress(refs.seek.value / 1000); });
  refs.seek.addEventListener("change", () => {
    const o = out()?.view();
    if (bridgeChosen(o)) {
      const d = o.state?.track?.duration || 0;
      if (d > 0) out().command("seek", { seconds: (refs.seek.value / 1000) * d });
    } else {
      const s = audio.snapshot();
      if (s.duration > 0) audio.seek((refs.seek.value / 1000) * s.duration);
    }
    scrubbing = false;
  });
  refs.isScrubbing = () => scrubbing;

  refs.shuffle = iconButton("Shuffle", "shuffle", () => {
    if (!bridgeChosen(out()?.view())) audio.setShuffle(!audio.snapshot().shuffle);
  }, "toggle");
  refs.repeat = iconButton("Repeat off", "repeat", () => onRepeat(), "toggle");
  refs.chip = el("span", { class: "np-chip" });
  // Polite, not assertive: this line carries "loading", "playing a
  // converted copy" and the playback errors, none of which should
  // interrupt what a screen reader is already saying. It is the only
  // place any of those were ever stated, and until now none of them
  // were announced at all. The bridge's lines (a hold, the DAC away, an
  // operation's outcome) are said here too.
  refs.notice = el("span", { class: "np-notice", attrs: { "aria-live": "polite" } });

  // Volume is desktop-only: iOS Safari ignores audio.volume outright,
  // and a slider that does nothing is worse than no slider.
  refs.volume = el("input", {
    class: "np-vol",
    attrs: { type: "range", min: "0", max: "100", value: "100", "aria-label": "Volume" },
  });
  refs.volume.addEventListener("input", () => {
    const v = refs.volume.value / 100;
    // The slider is the other way the level changes, so it feeds the
    // unmute restore point too — without this, dragging to 40%, then to
    // zero, then tapping unmute snapped back to full volume.
    if (v > 0) refs.lastVolume = v;
    audio.setVolume(v);
    setVolumeFill(v);
    swapIcon(refs.mute, v === 0 ? "vol-mute" : "vol");
    refs.mute.setAttribute("aria-label", v === 0 ? "Unmute" : "Mute");
  });
  // Mute is a real gap, not a nicety: the slider is hidden on touch, so
  // without it a phone has no way to silence the bar at all.
  refs.mute = iconButton("Mute", "vol", () => {
    const cur = Number(refs.volume.value) / 100;
    const next = cur === 0 ? (refs.lastVolume || 1) : 0;
    if (cur > 0) refs.lastVolume = cur;
    refs.volume.value = String(Math.round(next * 100));
    audio.setVolume(next);
    setVolumeFill(next);
    swapIcon(refs.mute, next === 0 ? "vol-mute" : "vol");
    refs.mute.setAttribute("aria-label", next === 0 ? "Unmute" : "Mute");
  }, "small");
  // Named for the phone's layout with the bridge chosen, where a Mute that
  // is disabled (the bridge's level is fixed) gives its width to the words
  // (round 2, R10).
  refs.mute.classList.add("np-mute");
  refs.lastVolume = 1;
  setVolumeFill(1);

  // The engine's queue, while the bridge's player is chosen (§7.5).
  refs.queueBtn = iconButton("Queue", "queue", () => toggleQueue(), "small");
  refs.queueBtn.hidden = true;
  refs.queueBtn.setAttribute("aria-expanded", "false");
  refs.panel = el("div", { class: "np-queue", attrs: { id: "np-queue", role: "region", "aria-label": "The bridge's queue" } });
  refs.panel.hidden = true;
  refs.queueBtn.setAttribute("aria-controls", "np-queue");
  // Escape closes the queue, its focus back on the Queue button (C8).
  refs.panel.addEventListener("keydown", (e) => {
    if (e.key === "Escape") {
      closeQueue();
      refs.queueBtn.focus();
    }
  });

  buildOutputMenu();

  bar.append(
    refs.rail,
    el("div", { class: "np-now" },
      refs.art,
      el("div", { class: "np-meta" }, refs.title, refs.sub, refs.notice)),
    el("div", { class: "np-center" },
      el("div", { class: "np-transport" }, refs.shuffle, refs.prev, refs.play, refs.next, refs.stop, refs.repeat),
      el("div", { class: "np-scrub" }, refs.elapsed, refs.seek, refs.total)),
    el("div", { class: "np-extra" }, refs.chip, refs.queueBtn, refs.output, refs.mute, refs.volume),
  );
  // The menu and the queue panel are the bar's siblings: the bar clips its
  // children.
  document.body.appendChild(bar);
  document.body.appendChild(refs.menu);
  document.body.appendChild(refs.panel);
  document.body.classList.add("has-player-bar");

  // A click outside the open menu or the open queue closes it, as Escape
  // does; a click on its own button is that button's toggle (C8).
  document.addEventListener("click", (e) => {
    const t = e.target;
    if (!refs.menu.hidden && !refs.menu.contains(t) && !refs.output.contains(t)) closeMenu();
    if (!refs.panel.hidden && !refs.panel.contains(t) && !refs.queueBtn.contains(t)) closeQueue();
  });

  audio.subscribe(() => render());
  out()?.subscribe(() => render());
  setInterval(tick, 500);
}

// ---- The output button and its menu (§7.1) ----

function buildOutputMenu() {
  refs.output = iconButton("Output: this browser", "output", () => toggleMenu(), "small");
  refs.output.setAttribute("aria-haspopup", "true");
  refs.output.setAttribute("aria-expanded", "false");
  refs.output.setAttribute("aria-controls", "np-output-menu");
  refs.menu = el("div", { class: "np-menu", attrs: { id: "np-output-menu", role: "group", "aria-label": "Output" } });
  refs.menu.hidden = true;
  refs.pickBrowser = el("button", {
    class: "np-pick", text: "This browser",
    attrs: { type: "button", "aria-pressed": "true" },
    on: { click: () => pickOutput("browser") },
  });
  refs.pickBridgeName = el("span", { class: "np-pick-name", text: "The bridge's player" });
  refs.pickBridgeLine = el("span", { class: "np-pick-line" });
  refs.pickBridge = el("button", {
    class: "np-pick",
    attrs: { type: "button", "aria-pressed": "false" },
    on: { click: () => pickOutput("bridge") },
  }, refs.pickBridgeName, refs.pickBridgeLine);
  refs.menuHelp = el("p", { class: "np-menu-help", text: PROMISE });
  refs.menu.append(refs.pickBrowser, refs.pickBridge, refs.menuHelp);
  refs.menu.addEventListener("keydown", (e) => {
    if (e.key === "Escape") {
      closeMenu();
      refs.output.focus();
    }
  });
}

function toggleMenu() {
  if (refs.menu.hidden) openMenu();
  else closeMenu();
}

function openMenu() {
  refs.menu.hidden = false;
  refs.output.setAttribute("aria-expanded", "true");
  readDACName();
  render();
  (refs.pickBridge.disabled ? refs.pickBrowser : refs.pickBridge).focus();
}

function closeMenu() {
  refs.menu.hidden = true;
  refs.output.setAttribute("aria-expanded", "false");
}

// The DAC's name, from the Player page's status, read when the menu opens.
let dacName = "";
async function readDACName() {
  try {
    const r = await fetch(ROUTES.status, { headers: { accept: "application/json" } });
    if (!r.ok) return;
    const s = await r.json();
    dacName = s?.running?.dac?.name || "";
    render();
  } catch { /* the menu says the player's state without the name */ }
}

function pickOutput(which) {
  closeMenu();
  out()?.pick(which);
  refs.output.focus();
}

function paintOutputButton(o) {
  const bridge = bridgeChosen(o);
  refs.output.hidden = !o || !o.offered;
  refs.output.setAttribute("aria-label", bridge ? "Output: the bridge's player" : "Output: this browser");
  refs.output.title = bridge ? "Output: the bridge's player" : "Output: this browser";
  refs.output.classList.toggle("np-btn-active", bridge);
  refs.pickBrowser.setAttribute("aria-pressed", String(!bridge));
  refs.pickBridge.setAttribute("aria-pressed", String(bridge));
  refs.pickBridgeName.textContent = dacName ? `The bridge's player: ${dacName}` : "The bridge's player";
  refs.pickBridgeLine.textContent = bridgeLine(o);
  const off = !o || (!o.canPick && !bridge);
  // A pick that goes disabled while it holds the focus would hand the focus
  // to the page, out of the open menu and its Escape: it goes to the other
  // pick first (C9).
  if (off && document.activeElement === refs.pickBridge) refs.pickBrowser.focus();
  setDisabled(refs.pickBridge, off);
}

/** What the bridge's player is doing, or why it cannot be picked (§7.1). */
function bridgeLine(o) {
  if (!o?.state) return "";
  if (!o.canPick && o.output !== "bridge") return o.pickWhy;
  const st = o.state;
  const title = st.track?.title || st.track?.path || "";
  if (st.transport === "playing") return title ? `playing «${title}»` : "playing";
  if (st.transport === "paused") return title ? `paused on «${title}»` : "paused";
  return "stopped";
}

// ---- The controls ----

function onPlay() {
  const o = out()?.view();
  if (!bridgeChosen(o)) {
    audio.toggle();
    return;
  }
  const playing = o.state?.transport === "playing";
  out().command(playing ? "pause" : "play");
}

function onPrevious() {
  if (bridgeChosen(out()?.view())) out().command("previous");
  else audio.advance(-1);
}

function onNext() {
  if (bridgeChosen(out()?.view())) out().command("next");
  else audio.advance(1);
}

function onRepeat() {
  const o = out()?.view();
  if (bridgeChosen(o)) out().command("repeat", { on: !o.state?.repeat });
  else audio.cycleRepeat();
}

// ---- The queue panel (§7.5) ----

function toggleQueue() {
  refs.panel.hidden = !refs.panel.hidden;
  refs.queueBtn.setAttribute("aria-expanded", String(!refs.panel.hidden));
  render();
}

function closeQueue() {
  refs.panel.hidden = true;
  refs.queueBtn.setAttribute("aria-expanded", "false");
}

let panelKey = "";
function paintPanel(o) {
  const show = bridgeChosen(o);
  refs.queueBtn.hidden = !show;
  if (!show) {
    refs.panel.hidden = true;
    refs.queueBtn.setAttribute("aria-expanded", "false");
    return;
  }
  if (refs.panel.hidden) return;
  const q = o.queue;
  const st = o.state;
  const heard = st?.track?.entry || 0;
  // A row's index names the queue this panel drew: while that is behind the
  // state's last change of the queue, a pick by position is disabled (and
  // the output refuses it), and the panel redraws as soon as queueRev
  // moves, before the queue is read again (R9, V12).
  const behind = !!o.queueBehind;
  const key = JSON.stringify([q?.readAt, st?.queueRev, behind, st?.selected, heard, [...o.commands]]);
  if (key === panelKey) return;
  panelKey = key;
  clear(refs.panel);
  if (!q || !q.entries?.length) {
    refs.panel.appendChild(el("p", { class: "np-queue-empty", text: "The bridge's queue is empty" }));
    return;
  }
  const list = el("ol", { class: "np-queue-list" });
  q.entries.forEach((e, i) => {
    const name = e.track?.title || e.path || e.word || `Entry ${e.id}`;
    const row = el("li", { class: `np-queue-row${e.id === heard || (!heard && e.id === st?.selected) ? " np-queue-current" : ""}` });
    const play = el("button", {
      class: "np-queue-title", text: name,
      attrs: { type: "button", "aria-current": e.id === heard ? "true" : null, title: behind ? WORDS.queueBehind : null },
      on: { click: () => out()?.command("index", { index: i }) },
    });
    setDisabled(play, !o.commands.has("index") || behind);
    row.appendChild(play);
    if (e.word) row.appendChild(el("span", { class: "chip chip-warn", text: queueWord(e.word) }));
    else if (st?.transport === "stopped" && e.id === st.lastStop?.entry && STOP_WORDS[st.lastStop.reason]) {
      row.appendChild(el("span", { class: "chip chip-warn", text: "Stopped here" }));
    }
    const del = el("button", {
      class: "np-queue-remove", text: "Remove",
      attrs: { type: "button", "aria-label": `Remove ${name} from the bridge's queue` },
      on: { click: () => out()?.remove(e.id) },
    });
    setDisabled(del, !o.commands.has("delete"));
    row.appendChild(del);
    list.appendChild(row);
  });
  refs.panel.appendChild(list);
}

function queueWord(word) {
  return {
    track_gone: "No longer in the library",
    not_in_library: "Not in this library",
    missing_file: "The file is missing",
  }[word] || word.replaceAll("_", " ");
}

// ---- Painting ----

/** Drive both the scrubber fill and the mobile rail from one value. */
function setProgress(frac) {
  const pct = `${Math.max(0, Math.min(1, frac || 0)) * 100}%`;
  refs.seek.style.setProperty("--np-progress", pct);
  refs.rail.style.setProperty("--np-progress", pct);
}

/**
 * How much of the track the browser has actually fetched, painted behind
 * the played fill on the same two surfaces.
 *
 * The rail half is not decoration: below the phone breakpoint the whole
 * numeric scrubber row is hidden, so the rail is the only place a phone
 * can see the download at all.
 *
 * The property is --np-loaded and not --np-buffered on purpose: the
 * COLOUR of this band is --np-buffer, and two names one character apart
 * inside the same gradient declaration is a swap nobody would see in
 * review. Positions are --np-progress / --np-loaded; colours are
 * --np-track / --np-buffer.
 */
function setBuffered(frac) {
  const pct = `${Math.max(0, Math.min(1, frac || 0)) * 100}%`;
  refs.seek.style.setProperty("--np-loaded", pct);
  refs.rail.style.setProperty("--np-loaded", pct);
}

function setVolumeFill(frac) {
  refs.volume.style.setProperty("--np-progress", `${Math.max(0, Math.min(1, frac || 0)) * 100}%`);
}

function tick() {
  const o = out()?.view();
  out()?.tick();
  if (bridgeChosen(o)) {
    tickBridge(o);
    return;
  }
  const s = audio.snapshot();
  if (!s.track) return;
  refs.elapsed.textContent = duration(s.currentTime) || "0:00";
  if (s.seekable && s.duration > 0) {
    refs.total.textContent = duration(s.duration);
    setBuffered(s.buffered / s.duration);
    if (!refs.isScrubbing()) {
      refs.seek.value = String(Math.round((s.currentTime / s.duration) * 1000));
      setProgress(s.currentTime / s.duration);
    }
  } else if (s.seekable) {
    // Loaded but the length is not in yet — the window this whole
    // change is about. The previous track's total was still sitting
    // there, which is a wrong number rather than a missing one, and its
    // thumb was still parked wherever that track had got to.
    refs.total.textContent = "0:00";
    setBuffered(0);
    if (!refs.isScrubbing()) {
      refs.seek.value = "0";
      setProgress(0);
    }
  }
}

/** The bridge's position: the anchor's own, as output.js extrapolates it (§4.4). */
function tickBridge(o) {
  const d = o.state?.track?.duration || 0;
  const p = out().position();
  refs.elapsed.textContent = duration(p ?? 0) || "0:00";
  refs.total.textContent = d > 0 ? duration(d) : "—";
  setBuffered(0);
  if (!refs.isScrubbing()) {
    const frac = d > 0 && p !== null ? p / d : 0;
    refs.seek.value = String(Math.round(frac * 1000));
    setProgress(frac);
  }
}

function render() {
  if (!bar) return;
  const o = out()?.view() || null;
  if (bridgeChosen(o)) renderBridge(o);
  else renderBrowser(audio.snapshot(), o);
  if (!refs.menu.hidden) paintOutputButton(o);
}

function renderBrowser(s, o) {
  bar.dataset.output = "browser";
  showBridgeControls(false);
  // On a box that offers its player the bar stays with nothing selected, a
  // slim strip with the output button (§7.1); elsewhere it hides as it
  // always has.
  const strip = !s.track && !!o?.offered;
  bar.hidden = !s.track && !strip;
  bar.classList.toggle("npbar-empty", !s.track);
  if (bar.hidden) {
    document.body.classList.remove("has-player-bar");
    return;
  }
  document.body.classList.add("has-player-bar");
  paintOutputButton(o);
  if (!s.track) {
    paintEmpty();
    paintLine(o?.line || "", false);
    return;
  }

  paintTrack(s);

  swapIcon(refs.play, s.playing ? "pause" : "play");
  refs.play.setAttribute("aria-label", s.playing ? "Pause" : "Play");
  // The button keeps the NAME of the action it performs — it still
  // pauses, and a reader who has decided against the wait needs that to
  // be true. Only its paint says "waiting", which is what the sighted
  // half of this was missing: a pause glyph over a 0:00 timer is a
  // player claiming to be playing something silently.
  refs.play.classList.toggle("np-btn-busy", s.loading);

  // Shuffle is genuinely binary, so aria-pressed is the right role.
  refs.shuffle.setAttribute("aria-pressed", String(s.shuffle));

  // Repeat is NOT binary — off / all / one — and aria-pressed cannot say
  // so: it would announce "Repeat, pressed" for both live modes, which is
  // exactly the ambiguity being fixed. It carries no aria-pressed at all;
  // its accessible NAME states the current mode, and because focus stays
  // on the button across a click, a screen reader announces the new name.
  // data-repeat drives the paint, and repeat-one gets a DIFFERENT glyph so
  // the three modes are never distinguished by colour alone.
  refs.repeat.dataset.repeat = s.repeat;
  refs.repeat.setAttribute("aria-label", REPEAT_LABEL[s.repeat] || REPEAT_LABEL.off);
  refs.repeat.title = REPEAT_LABEL[s.repeat] || REPEAT_LABEL.off;
  swapIcon(refs.repeat, s.repeat === "one" ? "repeat-one" : "repeat");

  // An unseekable source is one whose length the server couldn't
  // report — typically a UPnP upstream that ignored the Range request.
  // Disabling the control is the honest move: a scrubber bound to an
  // unknown duration is a control that lies.
  setDisabled(refs.seek, !s.seekable);
  refs.seek.title = "";
  if (!s.seekable) {
    refs.total.textContent = "—";
    setProgress(0);
  }

  paintNotice(s, o?.line || "");
}

/** The bridge's player as the engine publishes it (§7.2, §7.5). */
function renderBridge(o) {
  bar.dataset.output = "bridge";
  bar.hidden = false;
  document.body.classList.add("has-player-bar");
  showBridgeControls(true);
  const st = o.state;
  const moving = o.phase === PHASE.toBridge || o.phase === PHASE.toBrowser;
  const can = (c) => !moving && o.commands.has(c);
  const t = st?.track || null;
  bar.classList.toggle("npbar-empty", !t);
  paintBridgeTrack(o);

  const playing = st?.transport === "playing";
  swapIcon(refs.play, playing ? "pause" : "play");
  refs.play.setAttribute("aria-label", playing ? "Pause" : "Play");
  refs.play.classList.toggle("np-btn-busy", !!st?.starting);
  setDisabled(refs.play, !can(playing ? "pause" : "play"));
  setDisabled(refs.prev, !can("previous"));
  setDisabled(refs.next, !can("next"));
  setDisabled(refs.stop, !can("stop"));

  const mode = st?.repeat ? "all" : "off";
  refs.repeat.dataset.repeat = mode;
  refs.repeat.setAttribute("aria-label", BRIDGE_REPEAT_LABEL[mode]);
  refs.repeat.title = `${BRIDGE_REPEAT_LABEL[mode]}: ${WORDS.noRepeatOne}`;
  swapIcon(refs.repeat, "repeat");
  setDisabled(refs.repeat, !can("repeat"));

  refs.shuffle.setAttribute("aria-pressed", "false");
  refs.shuffle.title = WORDS.noShuffle;
  setDisabled(refs.shuffle, true);

  const dsd = t ? (t.codec === "DSD" || t.bits === 1) : false;
  setDisabled(refs.seek, !(st?.seek && can("seek")));
  refs.seek.title = dsd ? WORDS.noSeekDSD : "";

  paintPanel(o);
  paintOutputButton(o);
  paintLine(bridgeNotice(o), false);
  tickBridge(o);
}

/** Volume, mute and shuffle are the browser's; Stop and the queue the bridge's. */
function showBridgeControls(bridge) {
  refs.stop.hidden = !bridge;
  refs.volume.title = bridge ? WORDS.fixedLevel : "";
  refs.mute.title = bridge ? WORDS.fixedLevel : (refs.mute.getAttribute("aria-label") || "Mute");
  setDisabled(refs.volume, bridge);
  setDisabled(refs.mute, bridge);
  if (!bridge) {
    refs.shuffle.title = "Shuffle";
    setDisabled(refs.shuffle, false);
    setDisabled(refs.prev, false);
    setDisabled(refs.next, false);
    setDisabled(refs.play, false);
    setDisabled(refs.repeat, false);
    refs.queueBtn.hidden = true;
    refs.panel.hidden = true;
  }
}

/** The line the bridge's mode says: the phase, the line, a hold, the DAC away. */
function bridgeNotice(o) {
  const st = o.state;
  if (o.phase === PHASE.toBridge) return WORDS.toBridge;
  if (o.phase === PHASE.toBrowser) return WORDS.toBrowser;
  const notes = [];
  if (o.phase === PHASE.unconfirmed) notes.push(WORDS.unconfirmed);
  if (o.reconnecting) notes.push(WORDS.reconnecting);
  if (o.line && !notes.includes(o.line)) notes.push(o.line);
  if (st) {
    const hold = holdWord(st.hold);
    if (hold && !notes.includes(hold)) notes.push(hold);
    if (st.release?.state === "unconfirmed") {
      notes.push(st.release.since ? `The DAC has not confirmed it is free (since ${clockTime(st.release.since)})` : "The DAC has not confirmed it is free");
    }
    if (!st.running && st.why) notes.push(whyWord(st.why));
    else if (!st.offered && st.offeredWhy) notes.push(whyWord(st.offeredWhy));
    if (st.tone) notes.push("The test tone is playing on the DAC");
    if (st.lastAttempt?.seq > 0 && st.lastAttempt.failed && st.transport === "stopped") {
      notes.push(st.lastAttempt.busy ? "The DAC is busy: another program plays to it"
        : (st.lastAttempt.denied ? "The bridge may not open the DAC: its doctor says why" : "The last play did not start"));
    } else if (st.transport === "stopped" && STOP_WORDS[st.lastStop?.reason]) {
      notes.push(STOP_WORDS[st.lastStop.reason]);
    }
  }
  return notes.join(" · ");
}

/** Why playback last stopped, where the bar has something to say of it. */
const STOP_WORDS = {
  xrun: "Playback stopped: the DAC ran out of samples",
  disconnected: "Playback stopped: the DAC went away",
  device_error: "Playback stopped: the DAC reported an error",
  decode_failed: "Playback stopped: a track could not be decoded",
  backend_failed: "Playback stopped: the player failed",
  output_unavailable: "Playback stopped: the DAC is not available",
  removed_entry_audible: "Playback stopped: the entry playing was removed",
};

/** HH:MM of an RFC 3339 time, in this browser's zone. */
function clockTime(iso) {
  const t = new Date(iso);
  return Number.isNaN(t.getTime()) ? iso : t.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
}

/** The engine's track, named by its library path and row, never a controller's metadata. */
function paintBridgeTrack(o) {
  const st = o.state;
  const t = st?.track || null;
  clear(refs.art);
  clear(refs.title);
  if (!t) {
    refs.title.appendChild(document.createTextNode(st?.length > 0 ? "Stopped" : WORDS.nothing));
    refs.sub.textContent = st?.length > 0 ? `${st.length} in the bridge's queue` : "";
    refs.chip.textContent = "";
    return;
  }
  const entry = o.queue?.entries?.find((e) => e.id === t.entry);
  const albumId = entry?.track?.albumId;
  const name = t.named ? (t.title || t.path) : (t.path || "A track this library does not name");
  refs.title.appendChild(albumId ? link(`/album/${albumId}`, { text: name }) : document.createTextNode(name));
  refs.sub.textContent = [t.artist, t.album].filter(Boolean).join(" — ");
  refs.chip.textContent = signalChip(t, st.signal);
  // What the DAC is handed, for a reader who asks: its format, its
  // channels and the sink's state.
  const sig = st.signal;
  refs.chip.title = sig ? [sig.format, sig.channels ? `${sig.channels} ch` : "", sig.state].filter(Boolean).join(" · ") : "";
}

function paintEmpty() {
  clear(refs.art);
  clear(refs.title);
  refs.title.appendChild(document.createTextNode(WORDS.nothing));
  refs.sub.textContent = "";
  refs.chip.textContent = "";
}

/** The cover, the title, the artist and the format chip. */
function paintTrack(s) {
  const t = s.track;
  clear(refs.art);
  if (s.albumArt) {
    const img = el("img", { attrs: { src: s.albumArt, alt: "", loading: "lazy" } });
    // The cover is the largest target in the bar and it pointed nowhere,
    // while the title beside it linked to the album. Same destination.
    if (t.albumId) {
      const a = link(`/album/${t.albumId}`, { class: "np-art-link" });
      a.setAttribute("aria-label", `Go to ${t.album || "album"}`);
      a.appendChild(img);
      refs.art.appendChild(a);
    } else {
      refs.art.appendChild(img);
    }
  }
  clear(refs.title);
  refs.title.appendChild(
    t.albumId ? link(`/album/${t.albumId}`, { text: t.title || t.path })
      : document.createTextNode(t.title || t.path));
  refs.sub.textContent = t.artist || "";
  refs.chip.textContent = formatChip(t);
}

/**
 * The one line that says what is true about this playback right now:
 * waiting, playing something converted, unseekable, or failed, and the
 * output's own line (the bridge playing too, an outcome) beside them.
 */
function paintNotice(s, outputLine) {
  const notes = [];
  // Two words for two different waits, because the reader can tell them
  // apart and the distinction is what makes the line worth reading: at
  // zero it is a track that has not started, anywhere else it is one
  // that has stopped.
  if (s.loading) notes.push(s.currentTime > 0 ? "Buffering…" : "Loading…");
  if (s.degraded) notes.push("playing a converted copy");
  if (!s.seekable) notes.push("seeking unavailable for this source");
  if (s.error) notes.push(s.error);
  if (outputLine) notes.push(outputLine);
  paintLine(notes.join(" · "), !!s.error);
}

function paintLine(notice, isError) {
  // Compared before writing. apply() runs on every throttled timeupdate,
  // and assigning textContent replaces the text node whether or not the
  // string differs — which an aria-live region reads as a fresh
  // announcement, so an unguarded write would say "Loading…" once a
  // second for as long as the wait lasted.
  if (refs.notice.textContent !== notice) refs.notice.textContent = notice;
  refs.notice.classList.toggle("np-notice-error", isError);
}

/**
 * Repoint an existing button's <use> at another sprite symbol.
 *
 * Cheaper and less destructive than rebuilding the icon: the button keeps
 * its identity, so focus survives a play/pause round trip.
 */
function swapIcon(btn, name) {
  const use = btn.querySelector("use");
  if (use) use.setAttribute("href", `#i-np-${name}`);
}

/**
 * A transport button: one sprite symbol, one accessible name, one variant.
 *
 * The variant is applied through a classList call with a literal per
 * branch rather than a class string threaded in as an argument. That is
 * not stylistic: TestPlayerEmittedClassesAreStyled scrapes class literals
 * out of this module to prove every class it emits has a rule somewhere,
 * and a class arriving as a VARIABLE is invisible to it. Three of the four
 * buttons here carry a variant, so passing them as a parameter would have
 * quietly dropped them from that check.
 *
 * That scan does NOT strip JavaScript comments (its stylesheet half does),
 * so prose here must never quote a class-literal expression verbatim — the
 * quoted form is scraped as if it were real, and an ellipsis inside one
 * became a class named "…" that no stylesheet could ever define. Same trap
 * layout.html documents for element names inside template comments.
 */
function iconButton(label, icon, onClick, variant) {
  const svg = document.createElementNS("http://www.w3.org/2000/svg", "svg");
  // createElement cannot make SVG, so this one is set through the DOM and
  // is likewise outside the scrape above; np-ico is styled in player.css
  // alongside the button rules that reference it.
  svg.setAttribute("class", "np-ico");
  svg.setAttribute("viewBox", "0 0 24 24");
  svg.setAttribute("aria-hidden", "true");
  svg.setAttribute("focusable", "false");
  const use = document.createElementNS("http://www.w3.org/2000/svg", "use");
  use.setAttribute("href", `#i-np-${icon}`);
  svg.appendChild(use);

  const btn = el("button", {
    class: "np-btn",
    attrs: { type: "button", "aria-label": label, title: label },
    on: { click: onClick },
  });
  if (variant === "play") btn.classList.add("np-btn-play");
  else if (variant === "toggle") btn.classList.add("np-btn-toggle");
  else if (variant === "small") btn.classList.add("np-btn-sm");
  btn.appendChild(svg);
  return btn;
}
