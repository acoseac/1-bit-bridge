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

import { el, clear, link } from "./ui.js";
import { duration, formatChip } from "./format.js";
import * as audio from "./audio.js";

let bar = null;
let refs = {};

/** Human name for each repeat mode, used as the button's accessible name. */
const REPEAT_LABEL = { off: "Repeat off", all: "Repeat all", one: "Repeat this track" };

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
  refs.prev = iconButton("Previous", "prev", () => audio.advance(-1));
  refs.play = iconButton("Play", "play", () => audio.toggle(), "play");
  refs.next = iconButton("Next", "next", () => audio.advance(1));
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
    const s = audio.snapshot();
    if (s.duration > 0) audio.seek((refs.seek.value / 1000) * s.duration);
    scrubbing = false;
  });
  refs.isScrubbing = () => scrubbing;

  refs.shuffle = iconButton("Shuffle", "shuffle", () => audio.setShuffle(!audio.snapshot().shuffle), "toggle");
  refs.repeat = iconButton("Repeat off", "repeat", () => audio.cycleRepeat(), "toggle");
  refs.chip = el("span", { class: "np-chip" });
  // Polite, not assertive: this line carries "loading", "playing a
  // converted copy" and the playback errors, none of which should
  // interrupt what a screen reader is already saying. It is the only
  // place any of those were ever stated, and until now none of them
  // were announced at all.
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
  refs.lastVolume = 1;
  setVolumeFill(1);

  bar.append(
    refs.rail,
    el("div", { class: "np-now" },
      refs.art,
      el("div", { class: "np-meta" }, refs.title, refs.sub, refs.notice)),
    el("div", { class: "np-center" },
      el("div", { class: "np-transport" }, refs.shuffle, refs.prev, refs.play, refs.next, refs.repeat),
      el("div", { class: "np-scrub" }, refs.elapsed, refs.seek, refs.total)),
    el("div", { class: "np-extra" }, refs.chip, refs.mute, refs.volume),
  );
  document.body.appendChild(bar);
  document.body.classList.add("has-player-bar");

  audio.subscribe(apply);
  setInterval(tick, 500);
}

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

function apply(s) {
  if (!bar) return;
  bar.hidden = !s.track;
  if (!s.track) {
    document.body.classList.remove("has-player-bar");
    return;
  }
  document.body.classList.add("has-player-bar");

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
  refs.seek.disabled = !s.seekable;
  if (!s.seekable) {
    refs.total.textContent = "—";
    setProgress(0);
  }

  paintNotice(s);
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
 * waiting, playing something converted, unseekable, or failed.
 */
function paintNotice(s) {
  const notes = [];
  // Two words for two different waits, because the reader can tell them
  // apart and the distinction is what makes the line worth reading: at
  // zero it is a track that has not started, anywhere else it is one
  // that has stopped.
  if (s.loading) notes.push(s.currentTime > 0 ? "Buffering…" : "Loading…");
  if (s.degraded) notes.push("playing a converted copy");
  if (!s.seekable) notes.push("seeking unavailable for this source");
  if (s.error) notes.push(s.error);
  const notice = notes.join(" · ");
  // Compared before writing. apply() runs on every throttled timeupdate,
  // and assigning textContent replaces the text node whether or not the
  // string differs — which an aria-live region reads as a fresh
  // announcement, so an unguarded write would say "Loading…" once a
  // second for as long as the wait lasted.
  if (refs.notice.textContent !== notice) refs.notice.textContent = notice;
  refs.notice.classList.toggle("np-notice-error", !!s.error);
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
