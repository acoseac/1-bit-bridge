// Which row of a rendered track list is the one playing.
//
// Nothing on the page said. You clicked a title and the only
// acknowledgement was the dock at the very bottom of the window — so on a
// long album a click at row 14 was answered off-screen, which is the same
// "did that do anything?" the buffering work is about, one surface over.
// This puts the dock's own three states back where the click happened.
//
// Bound per rendered list and cleared at the route head, the way
// clearVariantRefresh is: a subscription that outlived its page would go
// on writing into a detached <ol> for the life of the tab.

import * as audio from "./audio.js";
import { el, spriteIcon } from "./ui.js";

let unsub = null;
let bound = [];

/**
 * Mark the row in `list` whose track is playing, and keep it marked.
 *
 * `tracks` is the array trackList rendered, in the same order. The .track
 * rows and the .track-disc headings are siblings in one <ol>, and a query
 * for .track alone skips the headings — so index i of that result is
 * tracks[i], with no per-row bookkeeping to keep true.
 *
 * Matched on PATH, not identity: playQueue CLONES the tracks it is given,
 * so the queue never holds the objects this list was built from. A path
 * appearing twice in one list (a playlist can do that) marks the first —
 * predictable, and the alternative is threading a queue index through a
 * list that may not be the queue at all.
 */
export function bindTrackMarks(list, tracks) {
  bound.push({ list, paths: tracks.map((t) => (t ? t.path : null)), at: -1, state: "", seen: false });
  // subscribe() calls back immediately with the current snapshot, which is
  // what marks a list rendered mid-playback. A second bind must not
  // subscribe again — it applies by hand instead.
  if (unsub) apply(audio.snapshot());
  else unsub = audio.subscribe(apply);
}

export function clearTrackMarks() {
  if (unsub) unsub();
  unsub = null;
  bound = [];
}

function apply(s) {
  // A view can re-render its list without a route change — the album
  // page's source filter does — leaving the previous <ol> detached with
  // this holding the only reference to it. Dropping it here is what saves
  // every caller from having to remember to unbind.
  //
  // Only once it has BEEN connected, though. trackList binds and then
  // returns the <ol> for its caller to append, so at bind time the list
  // is detached and looks exactly like a discarded one — a plain
  // isConnected filter dropped every binding on the spot, in the same
  // synchronous apply() that subscribe() fires, and nothing was ever
  // marked. A list that is built and never appended stays until the
  // route head clears it, which is bounded and the harmless direction.
  bound = bound.filter((b) => {
    b.seen = b.seen || b.list.isConnected;
    return !b.seen || b.list.isConnected;
  });
  const cur = s.track ? s.track.path : null;
  const state = !cur ? "" : s.loading ? "loading" : s.playing ? "playing" : "paused";
  for (const b of bound) {
    const at = cur === null ? -1 : b.paths.indexOf(cur);
    // apply runs on every emit — roughly once a second from timeupdate —
    // and a playlist list can hold tens of thousands of rows. The early
    // exit is what keeps this from being tens of thousands of DOM writes
    // per second for a value that changes a handful of times per track;
    // the querySelectorAll below it is O(rows) and must stay behind it.
    if (at === b.at && state === b.state) continue;
    const rows = b.list.querySelectorAll(".track");
    if (b.at >= 0 && rows[b.at]) paint(rows[b.at], "");
    if (at >= 0 && rows[at]) paint(rows[at], state);
    b.at = at;
    b.state = state;
  }
}

function paint(row, state) {
  const on = state !== "";
  row.classList.toggle("track-current", on);
  row.classList.toggle("track-waiting", state === "loading");
  row.classList.toggle("track-paused", state === "paused");
  const title = row.querySelector(".track-title");
  if (title) {
    // aria-current is what carries this to a screen reader. The mark is
    // aria-hidden and the digit it covers goes with visibility:hidden,
    // which takes that out of the accessibility tree too — the <ol> still
    // supplies the position, so nothing is lost.
    if (on) title.setAttribute("aria-current", "true");
    else title.removeAttribute("aria-current");
  }
  const mark = row.querySelector(".track-mark");
  if (!on) {
    // Removed rather than hidden. This is the half that makes the mark
    // affordable at playlist scale: one <svg><use> in the document
    // instead of one per row, each of which instantiates a shadow tree
    // from the sprite. Building it costs a handful of nodes per track
    // change, which happens a few times a minute.
    if (mark) mark.remove();
    return;
  }
  // The glyph states what pressing the row's title would now do, which is
  // the dock's convention. While waiting it is hidden behind the ring, so
  // the value it holds only matters for where the wait ends up.
  const icon = state === "playing" ? "pause" : "play";
  if (mark) {
    mark.querySelector("use")?.setAttribute("href", `#i-np-${icon}`);
    return;
  }
  const cell = row.querySelector(".track-num");
  if (!cell) return;
  cell.appendChild(el("span", { class: "track-mark", attrs: { "aria-hidden": "true" } },
    spriteIcon(icon, "track-ico")));
}
