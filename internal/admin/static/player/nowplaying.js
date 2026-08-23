// The persistent now-playing bar.
//
// Parented to <body>, outside #player-root, because it must survive
// every client-side route change — and, with the sessionStorage
// restore in audio.js, be able to reappear after a full page load onto
// a server page.

import { el, clear, link } from "./ui.js";
import { duration, formatChip } from "./format.js";
import * as audio from "./audio.js";

let bar = null;
let refs = {};

export function mount() {
  if (bar) return;
  bar = el("div", { class: "npbar", attrs: { id: "nowplaying", role: "region", "aria-label": "Now playing" } });
  bar.hidden = true;

  refs.art = el("div", { class: "np-art" });
  refs.title = el("span", { class: "np-title" });
  refs.sub = el("span", { class: "np-sub" });
  refs.prev = iconButton("Previous", "⏮", () => audio.advance(-1));
  refs.play = iconButton("Play", "▶", () => audio.toggle());
  refs.next = iconButton("Next", "⏭", () => audio.advance(1));
  refs.elapsed = el("span", { class: "np-time", text: "0:00" });
  refs.total = el("span", { class: "np-time", text: "0:00" });

  // A styled <input type="range">, not a rebuilt div: keyboard seeking,
  // screen-reader value announcement and Windows High Contrast all come
  // free, and a div-based slider gets none of them.
  refs.seek = el("input", {
    class: "np-seek",
    attrs: { type: "range", min: "0", max: "1000", value: "0", "aria-label": "Seek" },
  });
  let scrubbing = false;
  refs.seek.addEventListener("input", () => { scrubbing = true; });
  refs.seek.addEventListener("change", () => {
    const s = audio.snapshot();
    if (s.duration > 0) audio.seek((refs.seek.value / 1000) * s.duration);
    scrubbing = false;
  });
  refs.isScrubbing = () => scrubbing;

  refs.shuffle = iconButton("Shuffle", "🔀", () => audio.setShuffle(!audio.snapshot().shuffle));
  refs.repeat = iconButton("Repeat", "🔁", () => audio.cycleRepeat());
  refs.chip = el("span", { class: "np-chip" });
  refs.notice = el("span", { class: "np-notice" });

  // Volume is desktop-only: iOS Safari ignores audio.volume outright,
  // and a slider that does nothing is worse than no slider.
  refs.volume = el("input", {
    class: "np-vol",
    attrs: { type: "range", min: "0", max: "100", value: "100", "aria-label": "Volume" },
  });
  refs.volume.addEventListener("input", () => audio.setVolume(refs.volume.value / 100));
  if (isTouch()) refs.volume.hidden = true;

  bar.append(
    refs.art,
    el("div", { class: "np-meta" }, refs.title, refs.sub, refs.notice),
    el("div", { class: "np-transport" }, refs.prev, refs.play, refs.next),
    el("div", { class: "np-scrub" }, refs.elapsed, refs.seek, refs.total),
    el("div", { class: "np-extra" }, refs.chip, refs.shuffle, refs.repeat, refs.volume),
  );
  document.body.appendChild(bar);
  document.body.classList.add("has-player-bar");

  audio.subscribe(apply);
  setInterval(tick, 500);
}

function tick() {
  const s = audio.snapshot();
  if (!s.track) return;
  refs.elapsed.textContent = duration(s.currentTime) || "0:00";
  if (s.seekable && s.duration > 0) {
    refs.total.textContent = duration(s.duration);
    if (!refs.isScrubbing()) refs.seek.value = String(Math.round((s.currentTime / s.duration) * 1000));
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

  clear(refs.art);
  if (s.albumArt) {
    refs.art.appendChild(el("img", { attrs: { src: s.albumArt, alt: "", loading: "lazy" } }));
  }
  clear(refs.title);
  const t = s.track;
  refs.title.appendChild(
    t.albumId ? link(`/album/${t.albumId}`, { text: t.title || t.path })
      : document.createTextNode(t.title || t.path));
  refs.sub.textContent = t.artist || "";
  refs.chip.textContent = formatChip(t);
  refs.play.textContent = s.playing ? "⏸" : "▶";
  refs.play.setAttribute("aria-label", s.playing ? "Pause" : "Play");
  refs.shuffle.setAttribute("aria-pressed", String(s.shuffle));
  refs.repeat.setAttribute("aria-pressed", String(s.repeat !== "off"));
  refs.repeat.textContent = s.repeat === "one" ? "🔂" : "🔁";

  // An unseekable source is one whose length the server couldn't
  // report — typically a UPnP upstream that ignored the Range request.
  // Disabling the control is the honest move: a scrubber bound to an
  // unknown duration is a control that lies.
  refs.seek.disabled = !s.seekable;
  refs.total.textContent = s.seekable ? refs.total.textContent : "—";

  const notes = [];
  if (s.degraded) notes.push("playing a converted copy");
  if (!s.seekable) notes.push("seeking unavailable for this source");
  if (s.error) notes.push(s.error);
  refs.notice.textContent = notes.join(" · ");
  refs.notice.classList.toggle("np-notice-error", !!s.error);
}

function iconButton(label, glyph, onClick) {
  return el("button", {
    class: "np-btn", text: glyph,
    attrs: { type: "button", "aria-label": label },
    on: { click: onClick },
  });
}

function isTouch() {
  return window.matchMedia && window.matchMedia("(hover: none)").matches;
}
