// Player entry point.
//
// Loaded on EVERY admin page, deliberately: it owns the persistent
// <audio> element and the now-playing bar, which have to outlive a
// navigation away from the library. The router only mounts when the
// page is actually a player route.
//
// Load order: layout.html puts this after app.js. Deferred classic
// scripts and module scripts execute in document order, so app.js has
// already run its page dispatch and opened the single SSE stream by the
// time this runs — which is why the player reuses that stream instead
// of opening a second one.

import * as audio from "./audio.js";
import { createOutput, installOutput, currentOutput, listen } from "./output.js";
import { mount as mountBar } from "./nowplaying.js";
import { el, clear, announce } from "./ui.js";
import { wireVariantRefresh, clearVariantRefresh } from "./variants.js";
import { clearTrackMarks } from "./trackmarks.js";
import { abortReads } from "./api.js";
import {
  renderAlbums, renderAlbum, renderArtists, renderArtist,
  renderGenres, renderComposers,
  renderAxisAlbums, renderFavorites, renderPlaylists, renderPlaylistDetail,
  renderMixDetail,
  renderMixes, renderFolders, renderSearch, renderTracks,
} from "./views.js";

const SECTIONS = [
  ["albums", "Albums", "/albums"],
  ["artists", "Artists", "/artists"],
  ["favorites", "Favorites", "/favorites"],
  ["playlists", "Playlists", "/playlists"],
  ["mixes", "Smart Mixes", "/mixes"],
  ["composers", "Composers", "/composers"],
  ["genres", "Genres", "/genres"],
  ["folders", "Folders", "/folders"],
];

let seed = {};
let generation = 0;
const gen = () => generation;

function boot() {
  audio.init();
  // The bar's output (B634) before the bar, which reads it as it mounts; and
  // every start of this browser asks it first, before anything can start
  // (round 2, R1), and so does what playback does by itself (round 3, S1).
  const output = createOutput(browserOutputEnv());
  installOutput(output);
  audio.setStartGate(output.gateStart, output.cancelStarts);
  audio.setContinueGate(output.gateContinue);
  mountBar();
  wireGlobal();
  wireOutput();

  // Expose the shell-mount + route entry points so the admin boost router
  // (app.js) can re-mount the player after it swaps the shell back into
  // <main> — that is what lets audio survive a round trip out to a Server
  // page and back to the library. isPlayerPath lets the router recognise
  // which paths are ours without duplicating the route table.
  window.__player = { isPlayerPath, mountShell, route };

  if (document.getElementById("player-root")) mountShell();
}

// browserOutputEnv is what the bar's output runs over in a page: the
// browser's fetch, session storage and clocks, and audio.js's halves of the
// switch between the two outputs.
function browserOutputEnv() {
  return {
    fetch: (url, init) => fetch(url, init),
    storage: sessionStorage,
    random: (b) => crypto.getRandomValues(b),
    now: () => Date.now(),
    perf: () => performance.now(),
    browser: {
      handover: audio.handover,
      hasQueue: audio.hasQueue,
      suspend: audio.suspend,
      resume: audio.resume,
      take: audio.take,
      start: audio.start,
      mediaSession: audio.setMediaSession,
    },
  };
}

// wireOutput hands the output what reaches the page for it: app.js's
// player frames and its stream's lifecycle (a connection's serial at each
// open, its errors), a page shown again (its clock's samples taken again),
// and redraws the route in place when the output picked changes, its rows
// following the output (§7.6). Once, like wireGlobal.
function wireOutput() {
  const o = currentOutput();
  listen(o, { win: window, doc: document, onOutput: () => route() });
  // GET now on load: the clock's first sample, and whether this box offers
  // its player at all (a box that does not answers so at once: K1).
  o.sampleNow();
}

// wireGlobal registers the listeners that must live for the whole session.
// They are delegated (or re-query by id at event time), so they keep working
// across every partial-boost swap WITHOUT being re-registered — re-running
// this would stack a duplicate of each, so it is called exactly once.
function wireGlobal() {
  wireLinks();
  wireSearchShortcut();
  window.addEventListener("popstate", () => {
    // Player-internal back/forward only. The admin boost router owns
    // cross-page popstate and calls mountShell() when it swaps the shell
    // in; route() no-ops on operator paths (guard) and before the shell
    // exists (its own `if (!view) return`).
    if (isPlayerPath(location.pathname)) route();
  });
  window.addEventListener("player:rerender", () => route());
  wireVariantRefresh();
}


// mountShell wires the freshly-present player shell and renders the current
// route. Called on first load and again each time the boost router injects
// the shell into <main>. It deliberately does NOT touch document-level
// listeners (those live in wireGlobal); only element-level wiring, which is
// safe to redo because the elements are fresh on each injection.
export function mountShell() {
  const root = document.getElementById("player-root");
  if (!root) return;
  seed = readSeed();
  root.removeAttribute("data-booting");
  renderSections();
  wireSearchInput();
  route();
}

// PLAYER_HEADS is the set of first path segments the player owns — i.e. the
// routes the shell mounts and route() dispatches (or falls back to albums
// for). It MUST match the server's playerRoutes list (handlers_pages.go): the
// boost router uses isPlayerPath to decide whether a navigation stays in the
// player or fetches an operator partial, so a head registered server-side but
// missing here would be fetched as an operator page and never mount the shell.
// TestPlayerHeadsMatchServerRoutes pins the two together.
const PLAYER_HEADS = new Set([
  "albums", "artists", "favorites", "playlists", "mixes",
  "composers", "genres", "folders", "search", "tracks",
  "album", "artist", "genre", "composer", "playlist", "mix",
]);

// isPlayerPath reports whether a pathname is a player client-side route, so
// the boost router can tell a library navigation from an operator one. "/"
// is the album grid.
function isPlayerPath(pathname) {
  if (pathname === "/") return true;
  return PLAYER_HEADS.has(splitPath(pathname).head);
}

function readSeed() {
  const node = document.getElementById("player-seed");
  if (!node) return {};
  try {
    return JSON.parse(node.textContent || "{}");
  } catch {
    return {};
  }
}

function renderSections() {
  const nav = document.getElementById("player-sections");
  if (!nav) return;
  clear(nav);
  for (const [key, label, href] of SECTIONS) {
    // Smart Mixes stays listed even when the feature is off. It used to
    // be skipped, which was coherent while the off-state said "enable
    // this in Settings" — there was nothing to go there for. The page is
    // now where the switch IS, so hiding it left a reader with no way to
    // discover the feature from inside the player at all, while the
    // shell sidebar went on listing it two inches away.
    nav.appendChild(el("a", {
      class: "player-section", text: label,
      // data-base is the UNSCOPED href. applySectionScope rewrites the
      // live href from it on every route, so the scope can be added and
      // removed without the base ever being lost to a previous rewrite.
      attrs: { href, "data-base": href, "data-route": "", "data-section": key },
    }));
  }
}

function wireLinks() {
  document.addEventListener("click", (e) => {
    if (e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
    // Any link's click, a boosted operator page's included (app.js pushes
    // its entry after a fetch, never through pushRoute): the page's place,
    // while its DOM and its entry are still current.
    if (e.target.closest("a[href]")) savePlace();
    const a = e.target.closest("a[data-route]");
    if (!a) return;
    // A download or an explicit target is not a navigation, so it is not
    // ours to intercept — the same reasoning as the modifier keys above,
    // expressed by the markup instead of by the reader's hand. Inert as
    // written: every such anchor the player builds (the booklet's
    // target=_blank, a track's download, the Atlas attribution) is a plain
    // el("a") with no data-route, so this handler never sees one. It is
    // here because link() is the ergonomic helper for building an anchor,
    // and a download button reaching for it would otherwise be swallowed
    // by the router with nothing failing.
    if (a.hasAttribute("download") || (a.target && a.target !== "_self")) return;
    const url = new URL(a.href, location.origin);
    if (url.origin !== location.origin) return;
    e.preventDefault();
    if (url.href === location.href) return;
    // A crumb whose page is an ancestor in this tab's history goes back to
    // it, its place restored there (B1021); popstate routes it.
    const back = a.closest("#player-crumb") ? crumbBack(url.pathname + url.search) : 0;
    if (back) {
      savePlace();
      history.go(back);
      return;
    }
    pushRoute(url.pathname + url.search);
    route();
  });

  // Operator links (#primary-nav / .subnav) used to open in a new tab
  // while something was playing, because leaving the player was a full
  // page load that stopped playback. The admin boost router now swaps
  // those pages in place instead, keeping <audio> alive — so that
  // new-tab workaround is gone. When boost is disabled (?boost=0) they
  // fall through to an ordinary full load, the same as any other link.
}

// wireSearch owns the search field: a debounced filter-as-you-type that
// keeps the URL in step, plus the "/" shortcut to focus it.
//
// Navigation uses replaceState while typing and pushState only on the
// first entry into /search — otherwise every keystroke would leave a
// history entry and Back would walk the user backwards through their
// own query one character at a time.
// wireSearchInput binds the element-level listeners on the search field.
// It runs once per shell mount because the input/form are fresh elements on
// each injection — re-binding them is correct and can't stack (the old
// elements are gone). Split out from the "/" shortcut, which is document-level
// and must NOT be re-bound.
function wireSearchInput() {
  const form = document.getElementById("player-search-form");
  const input = document.getElementById("player-search-input");
  if (!form || !input) return;

  if (location.pathname === "/search") {
    input.value = new URLSearchParams(location.search).get("q") || "";
  }

  let timer = null;
  // Where the reader was when the pending keystroke was typed. A debounce
  // outlives a navigation: type two letters, click an album inside the
  // window, and the timer still fires — commit then sees the album route
  // as `entering` and pushes the reader straight back out to /search,
  // roughly 300 ms after they arrived somewhere they chose deliberately.
  // Reproduced in a browser; it predates the 250 ms → 400 ms change,
  // which only widened the window.
  //
  // Comparing the full href rather than clearing the timer from route()
  // covers every way the location can change — a link click, popstate,
  // navigate(), a boost swap out of the player — without enumerating
  // them, and it leaves a `player:rerender` (a variant job landing) alone,
  // which route() would otherwise cancel out from under someone mid-word.
  let armedAt = "";
  // typed distinguishes the debounce firing from an explicit Enter or
  // Escape. Only the first is suppressed below: an explicit action gets
  // an explicit answer, even when the answer is "type another letter".
  const commit = ({ typed = false } = {}) => {
    if (typed && location.href !== armedAt) return; // the reader moved on
    const q = input.value.trim();
    const entering = location.pathname !== "/search";
    // One letter is not a search — renderSearch requires two — so
    // committing it swapped whatever the reader was looking at for a
    // "type at least two characters" panel, on the way to a query they
    // were still in the middle of. Once ON /search it does commit, so
    // deleting back down to one letter still keeps the box, the URL and
    // the results describing the same thing.
    if (typed && entering && q.length < 2) return;
    const url = q ? `/search?q=${encodeURIComponent(q)}` : "/search";
    if (entering) pushRoute(url);
    else history.replaceState(history.state || {}, "", url);
    route();
  };

  input.addEventListener("input", () => {
    clearTimeout(timer);
    // 400 ms, up from 250. The debounce restarts on every keystroke, so
    // what it really measures is how long a PAUSE has to be before the
    // reader is treated as finished — and 250 ms is inside the ordinary
    // rhythm of typing a word, which is why searching felt like it fired
    // per letter. 400 ms sits past the gaps within a word and still
    // under the beat between words. Enter commits immediately for anyone
    // who does not want to wait at all.
    //
    // api.js aborts the in-flight request per keystroke, so a slow
    // response can't overwrite a newer one.
    armedAt = location.href;
    timer = setTimeout(() => commit({ typed: true }), 400);
  });
  form.addEventListener("submit", (e) => {
    e.preventDefault();
    clearTimeout(timer);
    commit();
  });
  input.addEventListener("keydown", (e) => {
    if (e.key !== "Escape") return;
    input.value = "";
    clearTimeout(timer);
    input.blur();
    navigate("/albums");
  });
}

// wireSearchShortcut binds the document-level "/" focus shortcut. It is
// registered once (in wireGlobal) and re-queries the input by id at event
// time, so it keeps focusing the CURRENT field after the shell is re-mounted
// rather than a stale, detached element from a previous injection.
function wireSearchShortcut() {
  document.addEventListener("keydown", (e) => {
    if (e.key !== "/" || e.metaKey || e.ctrlKey || e.altKey) return;
    const t = e.target;
    if (t && (t.tagName === "INPUT" || t.tagName === "TEXTAREA" || t.isContentEditable)) return;
    const input = document.getElementById("player-search-input");
    if (!input) return; // not on a player page right now
    e.preventDefault();
    input.focus();
    input.select();
  });
}

/**
 * The route the reader actually took to reach a page, kept per history
 * entry so the breadcrumb can say where they came FROM rather than only
 * where a page structurally hangs.
 *
 * The structural answer is right for a folder and wrong for an album:
 * an album reached from a composer hangs off its artist, so a purely
 * structural trail told a reader who had just clicked through
 * "Composers › Lewis Allen" that they were somewhere else entirely. It
 * was reported twice.
 *
 * Kept in history.state rather than in the URL deliberately. An album
 * URL stays canonical — it is the album, for everyone — while the state
 * rides the entry through reload, Back and Forward. A pasted link or a
 * new tab has no state, which is exactly when the structural fallback is
 * the honest answer.
 *
 * Recording it in the ROUTER means every entry point is covered at once,
 * including ones added later: nothing at a link site has to opt in.
 */
const maxTrail = 4;

/**
 * The page being left, as a crumb entry.
 *
 * The label is read from the heading at navigation time, which is what
 * the reader just saw — including a title a view set asynchronously. A
 * click landing before that resolves gets the section name, which is
 * imprecise but never wrong.
 */
function currentCrumbEntry() {
  const h = document.getElementById("player-title");
  const label = (h?.textContent || "").trim();
  // Its entry's depth and key, which a crumb back to it reads (B1021).
  const s = history.state || {};
  return label ? { label, href: location.pathname + location.search, depth: s.depth, key: s.key } : null;
}

/**
 * Read the trail off the current history entry.
 *
 * Validated on the way in, not merely type-checked: history.state
 * survives reloads and outlives the code that wrote it, so a stale entry
 * from an older (or newer) version of this file can arrive in any shape.
 * The href test rejects "//evil.com" as well as absolute URLs — a
 * protocol-relative href passes a naive startsWith("/") and would put an
 * off-origin link in the breadcrumb.
 */
function readTrail() {
  const raw = history.state?.trail;
  if (!Array.isArray(raw)) return null;
  const out = raw.filter((e) =>
    e && typeof e.label === "string" && e.label !== "" &&
    typeof e.href === "string" && e.href.startsWith("/") && !e.href.startsWith("//"))
    // An entry's depth and key are kept only as an integer and a string: one
    // recorded before them is a crumb that pushes.
    .map(({ label, href, depth, key }) => (Number.isInteger(depth) && typeof key === "string" ? { label, href, depth, key } : { label, href }));
  return out.length ? out : null;
}

/**
 * The top-level lists, which START a trail rather than extending one.
 *
 * Without this a breadcrumb becomes a browsing log: leaving an artist for
 * the Albums section produced "Artists › Abdullah Ibrahim › Albums › …",
 * which is a true history and a useless trail. Arriving at a top-level
 * list is a fresh start, so it resets — and the cycle check below cannot
 * cover this, because jumping to a SIBLING section is not a loop.
 *
 * "/" is the album grid, and /search is a top-level destination too even
 * though it has no section link of its own. Compared on the path, so a
 * query (/search?q=…, /folders?path=…) still matches.
 */
const trailRoots = new Set(["/", "/search", ...SECTIONS.map(([, , href]) => href)]);

/**
 * The trail to store on the entry we are about to push.
 *
 * Ancestors of the current page, plus the current page itself. A target
 * already in the trail truncates it back to before that point, so
 * stepping back up a chain (album → its artist byline → that album
 * again) cannot grow one link per lap. maxTrail is a backstop only —
 * with the root reset above, real trails are two or three deep.
 */
function trailFor(targetHref) {
  if (trailRoots.has(targetHref.split("?")[0])) return [];
  const here = currentCrumbEntry();
  let next = readTrail() || [];
  if (here) next = [...next, here];
  const loop = next.findIndex((e) => e.href === targetHref);
  if (loop >= 0) next = next.slice(0, loop);
  return next.slice(-maxTrail);
}

/**
 * pushState with the route trail attached. Every push goes through here,
 * after the place of the page being left is written into its own entry, and
 * stamps the new entry one deeper than the current one, with a key of its
 * own (B1021).
 */
function pushRoute(href) {
  savePlace();
  history.pushState({ scrollY: 0, trail: trailFor(href), depth: depthOf(history.state) + 1, key: newKey() }, "", href);
}

const depthOf = (s) => (Number.isInteger(s?.depth) ? s.depth : 0);
// Unique across this tab's loads: history.state outlives a reload.
const newKey = () => Array.from(crypto.getRandomValues(new Uint32Array(2)), (n) => n.toString(36)).join("");

/**
 * The URL each entry of this tab's history last held, by its key, recorded
 * where it moves: as the router routes the entry, and as a list's filter
 * writes its URL in place. Kept in the tab's session storage, which a reload
 * keeps and no other tab shares, since a descendant's trail is a copy taken
 * as it was pushed and the ancestor's own state cannot be read before going
 * back to it; the latest ENTRY_URLS_KEPT entries. Where the record is not to
 * be had, a crumb pushes.
 */
const ENTRY_URLS = "bridge-player-entries";
const ENTRY_URLS_KEPT = 200;

function recordedURLs() {
  try {
    const v = JSON.parse(sessionStorage.getItem(ENTRY_URLS) || "[]");
    return Array.isArray(v) ? v.filter((e) => Array.isArray(e) && typeof e[0] === "string" && typeof e[1] === "string") : [];
  } catch {
    return [];
  }
}

function recordURL(key) {
  if (typeof key !== "string") return;
  try {
    const kept = recordedURLs().filter(([k]) => k !== key);
    kept.push([key, location.pathname + location.search]);
    sessionStorage.setItem(ENTRY_URLS, JSON.stringify(kept.slice(-ENTRY_URLS_KEPT)));
  } catch {
    // Not recorded: no URL an entry held before may stand for it, so every
    // crumb pushes until the next record.
    try { sessionStorage.removeItem(ENTRY_URLS); } catch { /* nothing to trust */ }
  }
}

/**
 * How far back (a negative step) the crumb's page is in this tab's history,
 * or 0 where the crumb pushes: a trail entry of this entry's with that href,
 * a depth below this one's, and an entry recorded as still holding the href
 * (a filter edited in place there since, by Back and Forward, has moved it).
 */
function crumbBack(href) {
  const here = depthOf(history.state);
  const t = readTrail()?.find((e) => e.href === href);
  return t && t.depth < here && recordedURLs().find(([k]) => k === t.key)?.[1] === href ? t.depth - here : 0;
}

// The page the place writer writes for (B1021): its entry's key, its view,
// its list's measure, and whether it has rendered (nothing is written into
// its entry before).
let shown = { key: "", view: null, list: null, ready: false };
let saveTimer = 0;

/**
 * Write the page's place into its own entry: a list's (views.js's measure)
 * or a page's scroll offset. Only into the entry the page was routed for,
 * while its view is in the document and once it has rendered; just before
 * every navigation from it, at pagehide, at a list's jump, and at most once
 * per 500 ms while it scrolls. A write WebKit refuses past its rate of
 * history writes is dropped, never thrown at the navigation after it.
 */
function savePlace() {
  clearTimeout(saveTimer);
  saveTimer = 0;
  const s = history.state;
  if (!shown.ready || s?.key !== shown.key || !shown.view?.isConnected) return;
  try {
    if (shown.list) {
      const place = shown.list();
      if (place) history.replaceState({ ...s, place }, "");
    } else if (Math.abs((s.scrollY || 0) - window.scrollY) >= 40) {
      history.replaceState({ ...s, scrollY: window.scrollY }, "");
    }
  } catch {
    /* refused past WebKit's rate of history writes: the next one lands */
  }
}

export function navigate(href) {
  pushRoute(href);
  route();
}

// splitPath turns a pathname into { path, head, rest } — trailing
// slashes trimmed, leading slash dropped, split at the first remaining
// separator. "/album/abc" → head "album", rest "abc".
function splitPath(pathname) {
  let end = pathname.length;
  while (end > 1 && pathname[end - 1] === "/") end--;
  const path = end > 0 ? pathname.slice(0, end) : "/";
  const body = path.startsWith("/") ? path.slice(1) : path;
  const slash = body.indexOf("/");
  return slash === -1
    ? { path, head: body, rest: "" }
    : { path, head: body.slice(0, slash), rest: body.slice(slash + 1) };
}

/**
 * Where the section strip has to be scrolled for the active entry to be
 * visible, or null when it should not move at all.
 *
 * Below 769px the strip is a horizontally-scrolling row, and it opens at
 * scrollLeft 0 — so on the sections furthest right the marked entry sat
 * off-screen and the strip looked like it had no active state. It was
 * true of Folders before Sources existed; Sources only made the strip one
 * entry longer.
 *
 * Two things it deliberately does NOT do:
 *
 *   - It does not move a strip whose active entry is already visible.
 *     route() runs on every filter change too, so an unconditional
 *     re-centre would yank the strip back from wherever the reader had
 *     scrolled it, every time they touched a sort dropdown.
 *   - It returns a scroll offset rather than calling scrollIntoView.
 *     That method walks ANCESTORS, so on a page whose vertical position
 *     the boost router is separately restoring — under its own
 *     generation guard, because a stale offset landing last is a real
 *     defect there — it is a second writer to the same scroll state.
 *     Assigning the container's own scrollLeft cannot touch anything
 *     else.
 *
 * Pure, and separated from the DOM read for that reason: the geometry is
 * the part worth pinning, and it is testable without a browser.
 */
export function sectionScrollLeft({ navLeft, navWidth, scrollLeft, maxScroll, itemLeft, itemWidth }) {
  // Not scrollable: the desktop rail is a vertical column, where every
  // entry is visible and there is nothing to correct.
  if (!(maxScroll > 0)) return null;
  const itemStart = itemLeft - navLeft;
  if (itemStart >= 0 && itemStart + itemWidth <= navWidth) return null;
  // The leading margin is floored at zero so an entry WIDER than the strip
  // lands on its start rather than its middle: a negative margin would
  // scroll past the beginning of the label and show the reader the inside
  // of a word. No shipped label is that wide, but the clamp costs nothing
  // and the alternative is only correct by accident of the label set.
  const margin = Math.max(0, (navWidth - itemWidth) / 2);
  return Math.round(Math.max(0, Math.min(scrollLeft + itemStart - margin, maxScroll)));
}

/** Apply sectionScrollLeft to the live strip. */
function revealActiveSection() {
  const nav = document.getElementById("player-sections");
  // aria-current, not the .active class: that attribute is the SEMANTIC
  // marker for "you are here", so this keeps working for any future row
  // in the rail that is not a .player-section — and route() already sets
  // the two together, so it is not a second thing to remember.
  const active = nav?.querySelector('[aria-current="page"]');
  if (!active) return;
  const navBox = nav.getBoundingClientRect();
  const itemBox = active.getBoundingClientRect();
  const next = sectionScrollLeft({
    navLeft: navBox.left, navWidth: nav.clientWidth,
    scrollLeft: nav.scrollLeft, maxScroll: nav.scrollWidth - nav.clientWidth,
    itemLeft: itemBox.left, itemWidth: itemBox.width,
  });
  if (next !== null) nav.scrollLeft = next;
}

// The sections a source scope actually narrows.
//
// The four axes below all take `source=` on their endpoint. The rest do
// not, and each for its own reason: Smart Mixes are generated from the
// whole library's pool, Folders is filesystem-only by construction (its
// query excludes routed rows outright), and Favorites and Playlists are
// documents that can span every source at once — a playlist is not "on"
// an upstream.
//
// Listing them unscoped inside a scoped context is what the reader
// reported as "it reverts back to the library view": the rail offered a
// way out of the scope that looked like a way around it.
const SOURCE_SCOPED_SECTIONS = new Set(["albums", "artists", "composers", "genres"]);

/**
 * Carry the current source scope across the section rail, and hide the
 * sections it cannot narrow.
 *
 * The links are static hrefs, so before this, clicking Artists inside
 * Chord 2go dropped `?source=` and landed on the whole library — the
 * scope survived a sort change (the toolbar mutates the live URL) but not
 * a section change, which is a difference no reader could be expected to
 * predict.
 *
 * Rewriting the href rather than intercepting the click keeps
 * middle-click and open-in-new-tab honest: the address the anchor shows
 * is the address it goes to.
 */
function applySectionScope() {
  const source = new URLSearchParams(location.search).get("source") || "";
  const scoped = source !== "";
  const qs = scoped ? `?source=${encodeURIComponent(source)}` : "";
  for (const a of document.querySelectorAll(".player-section")) {
    const base = a.dataset.base;
    if (!base) continue;
    const keep = SOURCE_SCOPED_SECTIONS.has(a.dataset.section);
    a.href = keep ? base + qs : base;
    // Never hide the section the reader is ON, even when the scope cannot
    // narrow it — a hand-typed /playlists?source=… would otherwise leave
    // the rail with no active entry and no explanation.
    a.hidden = scoped && !keep && !a.classList.contains("active");
  }
}

function route() {
  const view = document.getElementById("player-view");
  const titleEl = document.getElementById("player-title");
  if (!view) return;

  // The page being left, while its DOM and its entry are current: an output
  // change and a redraw route the same entry without pushRoute.
  savePlace();
  generation += 1;
  // An entry the router did not push (a first load, a boosted one) is
  // stamped, keeping all it holds.
  let s = history.state || {};
  if (!Number.isInteger(s.depth) || typeof s.key !== "string") {
    s = { ...s, depth: depthOf(s), key: typeof s.key === "string" ? s.key : newKey() };
    history.replaceState(s, "");
  }
  recordURL(s.key);
  const here = shown = { key: s.key, view, list: null, ready: false };
  // Whether the page is still this route's: its entry the one shown and its
  // view in the document. A boosted navigation to an operator page pushes its
  // own entry and replaces <main> with no route, so the generation alone
  // cannot tell.
  const owns = () => history.state?.key === s.key && view.isConnected;
  // Split the path with string ops rather than a regex. Both patterns
  // this replaced were flagged for super-linear backtracking, and while
  // a pathname is short enough that it never mattered, the manual form
  // is linear and easier to read than /^\/([^/]*)\/?(.*)$/ was.
  const { path, head, rest } = splitPath(location.pathname);
  const params = new URLSearchParams(location.search);
  const section = path === "/" ? "albums" : head;

  for (const a of document.querySelectorAll(".player-section")) {
    const active = a.dataset.section === section ||
      (section === "album" && a.dataset.section === "albums") ||
      (section === "artist" && a.dataset.section === "artists");
    a.classList.toggle("active", active);
    if (active) a.setAttribute("aria-current", "page");
    else a.removeAttribute("aria-current");
  }
  // Scope BEFORE reveal: applySectionScope hides the sections a source
  // cannot narrow, which changes the strip's scroll geometry — measuring
  // first would scroll to where the active entry used to be.
  applySectionScope();
  revealActiveSection();
  updateSidebarNav(section);

  const setToolbar = (node) => {
    const bar = document.getElementById("player-toolbar");
    if (!bar) return;
    clear(bar);
    if (node) bar.appendChild(node);
  };
  // The breadcrumb is cleared HERE, up front, rather than by each view the
  // way the toolbar is. Only three views ever set one, so leaving it to the
  // views would mean the other eleven had to remember to clear it — and the
  // failure mode of forgetting is a stale "← Smart Mixes" sitting above an
  // unrelated page, pointing somewhere the reader never was.
  const setCrumb = (node) => {
    const bar = document.getElementById("player-crumb");
    if (!bar) return;
    clear(bar);
    if (node) bar.appendChild(node);
  };
  setCrumb(null);
  // Same reasoning as the crumb: cleared up front, so a view that
  // installed a variant-refresh hook cannot have it fire after the user
  // has navigated somewhere else. Only two views ever set one.
  clearVariantRefresh();
  // And once more for the now-playing row marks: the lists this route is
  // about to replace are the only thing holding those bindings, and a
  // subscription that outlived them would write into detached nodes for
  // the life of the tab. trackList re-binds as it renders.
  clearTrackMarks();
  // And the same reasoning once more, for the reads themselves: a fetch
  // the previous view started must not be able to paint. getJSON's
  // per-key abort cannot do this — two views with different keys race,
  // and the loser wins. See abortReads in api.js.
  abortReads();
  // And the settings snapshot the feature trays share. An operator page
  // drops it in its page init, which the player never runs for its own
  // navigation, so a player tray showed a switch as it was when the page
  // loaded, whatever Settings, the CLI or another tab had done since, beside
  // a view that reads the server fresh. One fetch per route that mounts a
  // tray. Guarded like every other reach into app.js.
  window.BridgeFeatureTray?.invalidate?.();
  const ctx = {
    params, gen, setToolbar, setCrumb, id: rest, trail: readTrail(),
    // A list's place (views.js's renderPagedList): its measure as it begins,
    // whether its rows stand (each of its loads decides, a failed one and
    // one under way leaving it unready), a write at its jump, and its entry's
    // URL moved by its filter.
    listing: (measure) => { here.list = measure; },
    listed: (on) => { here.ready = on; },
    savePlace,
    owns,
    moved: () => recordURL(s.key),
  };

  const routes = {
    albums: ["Albums", () => renderAlbums(view, ctx)],
    album: ["Album", () => renderAlbum(view, ctx)],
    artists: ["Artists", () => renderArtists(view, ctx)],
    artist: ["Artist", () => renderArtist(view, ctx)],
    genres: ["Genres", () => renderGenres(view, ctx)],
    genre: ["Genre", () => renderAxisAlbums(view, ctx, "genre")],
    composers: ["Composers", () => renderComposers(view, ctx)],
    composer: ["Composer", () => renderAxisAlbums(view, ctx, "composer")],
    favorites: ["Favorites", () => renderFavorites(view, ctx)],
    playlists: ["Playlists", () => renderPlaylists(view, ctx)],
    playlist: ["Playlist", () => renderPlaylistDetail(view, ctx)],
    mixes: ["Smart Mixes", () => renderMixes(view, ctx)],
    mix: ["Mix", () => renderMixDetail(view, ctx)],
    folders: ["Folders", () => renderFolders(view, ctx)],
    search: ["Search", () => renderSearch(view, ctx)],
    tracks: ["Tracks", () => renderTracks(view, ctx)],
  };
    // routes[section] || routes.albums is a silent fallback: a head that
  // PLAYER_HEADS claims but this table forgets renders the album grid
  // under the wrong title instead of failing. TestPlayerRoutesTableCovers
  // EveryPlayerHead is what keeps the two in step.
  const [title, render] = routes[section] || routes.albums;

  titleEl.textContent = title;
  document.title = `${title} — ${seed.libraryName || "Library"}`;
  // Client-side navigation is silent for assistive tech: without moving
  // focus and announcing, a keyboard user pressing Back gets no signal
  // that anything changed.
  //
  // Except when the reader is TYPING, which is a navigation they did not
  // ask for: filter-as-you-type routes to /search on every pause, so the
  // unconditional focus() yanked the caret out of the field mid-word and
  // every keystroke after it went nowhere. Reproduced at a 320 ms
  // cadence — a perfectly ordinary typing speed against the debounce —
  // where "abdullah" reached the box as "a".
  //
  // The live region still fires: announcing costs nothing and moving
  // focus is the only part that was wrong. So for a list's name filter
  // (B1021), whose list keeps the focus in it as it redraws.
  const typing = document.activeElement;
  if (typing !== document.getElementById("player-search-input") && !typing?.classList?.contains("list-filter")) {
    titleEl.focus({ preventScroll: true });
  }
  announce(title);

  // Generation-guarded, like chunkAppend: a render that has been
  // superseded must not apply ITS route's scroll offset to the page
  // that replaced it. abortReads() makes an abandoned render resolve
  // (its fetch rejects, the view returns on isAborted), so this `.then`
  // now runs reliably for routes the reader has left — and the abort
  // can land AFTER the new route has already scrolled, which makes the
  // stale offset the LAST writer rather than a flicker. Measured:
  // scrollTo saw [{top: 0}, {top: 640}] and the reader ended up 640px
  // down a page they had just opened.
  //
  // A list is its page's one scroll writer, to its place or its top
  // (views.js's renderPagedList), and its loads say when it is ready: this
  // restore is for every other page, and only while it is this route's (an
  // operator page boosted in meanwhile moved no generation).
  const myGen = gen();
  void Promise.resolve(render()).then(() => {
    if (myGen !== gen() || !owns() || here.list) return;
    window.scrollTo({ top: s.scrollY || 0 });
    here.ready = true;
  });
}

// updateSidebarNav highlights the sidebar entry that owns this player
// route.
//
// The server does the same thing on a cold load (pageData.PlayerNav →
// layout.html), but most navigation here never reaches the server: the
// router swaps views in place, so nothing else would move the highlight
// off Smart mixes when the reader clicks through to an album.
//
// Every player route carries data-tab="player", so the entry keyed on a
// SECTION has to win — otherwise Browse would match everything and
// Smart mixes would never light. Sections with no entry of their own
// fall back to Browse, which is the entry without a data-player-section.
//
// Kept in step with playerNavEntry (handlers_pages.go) by
// TestPlayerNavEntriesMatchTheLayout, which walks playerRoutes, playerNavEntry
// and the layout's data-player-section attributes. The `detailOwner` map below
// is the one pair it does not read — see the note there.
function updateSidebarNav(section) {
  const nav = document.getElementById("primary-nav");
  if (!nav) return;
  // A detail route belongs to its grid's entry: /mix/x is Smart mixes and
  // /playlist/x is Playlists, not Browse.
  //
  // NOT pinned against the server's playerNavEntry: that test walks the
  // layout's declared sections, and this map is a client-side detail-route
  // mapping with no server counterpart to compare against while the sidebar
  // declares no player sections at all (Playlists and Smart mixes were removed
  // as duplicates of Browse's section rail). The cost of drift is a nav entry
  // that fails to light on a detail route.
  const detailOwner = { mix: "mixes", playlist: "playlists" };
  const owner = detailOwner[section] || section;
  // Compared as a value, not interpolated into a selector. `section` is a
  // path segment straight off location.pathname, so a URL carrying a quote
  // or a bracket would build a malformed selector and querySelector throws
  // a DOMException — taking route() down with it and leaving the page
  // unrendered. CSS.escape would also fix that; not building the selector
  // at all is one fewer thing to remember.
  const links = [...nav.querySelectorAll("a")];
  // A source scope wins over Browse. Both would otherwise light — every
  // player route renders the player section — and two "you are here"
  // marks tell the reader nothing. Same rule the server applies through
  // pageData.SourceCurrent, restated here for the navigations it never
  // sees. Compared as a VALUE, like `section` above: a source id off the
  // query string must not be interpolated into a selector.
  const source = new URLSearchParams(location.search).get("source") || "";
  const match = (source !== "" && links.find((a) => a.dataset.sourceId === source)) ||
    links.find((a) => a.dataset.playerSection === owner) ||
    links.find((a) => a.dataset.tab === "player" && !a.dataset.playerSection) ||
    null;
  for (const a of links) {
    if (a === match) a.setAttribute("aria-current", "page");
    else a.removeAttribute("aria-current");
  }
}

// Coalesced (B1021): every 40px of a fling wrote history, past the rate
// WebKit allows.
window.addEventListener("scroll", () => {
  if (document.getElementById("player-root") && !saveTimer) saveTimer = setTimeout(savePlace, 500);
}, { passive: true });
window.addEventListener("pagehide", savePlace);

// Entry point, LAST.
//
// boot() reaches most of this module, so every `const` and `let` it can
// touch has to be initialised before it runs — and a top-level `boot()`
// near the top of the file puts every declaration below it in the
// temporal dead zone. That is not a theoretical hazard here: it has
// silently emptied the rail twice, once for the sources TTL and once for
// SOURCE_SCOPED_SECTIONS, and both times the only symptom was a
// ReferenceError in the console and a page that rendered nothing.
// Function declarations hoist, so calling it from the bottom changes
// nothing except which half of the file is safe to declare in.
//
// TestPlayerBootCallIsLast keeps it here.
boot();
