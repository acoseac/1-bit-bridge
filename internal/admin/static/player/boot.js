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
import { mount as mountBar } from "./nowplaying.js";
import { el, clear, announce } from "./ui.js";
import { wireVariantRefresh, clearVariantRefresh } from "./variants.js";
import { api, abortReads } from "./api.js";
import {
  renderAlbums, renderAlbum, renderArtists, renderArtist,
  renderGenres, renderComposers,
  renderAxisAlbums, renderFavorites, renderPlaylists, renderPlaylistDetail,
  renderMixDetail,
  renderMixes, renderFolders, renderSearch, renderTracks, renderSources,
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

// Sources is listed only on a bridge that HAS more than one, which is
// why it is not in SECTIONS. Unlike Smart Mixes — whose page is where
// its own switch lives, so hiding it would hide the feature — a facet
// over a single filesystem library offers one choice and explains
// nothing. The seed carries the answer so no request is needed to
// decide whether to draw it.
const SOURCES_SECTION = ["sources", "Sources", "/sources"];

// How long a painted source rail is trusted before the next navigation
// re-reads it.
//
// The rail carries LIVENESS, so it cannot be painted once and left: an
// upstream that drops mid-session would keep its green dot for as long as
// the tab stayed open. It equally cannot re-read on every route() call —
// that is one request per navigation, the cost the banner's name lookup
// was memoised to avoid. A TTL bounds it to at most one read per window
// however much the reader clicks.
const SOURCE_NAV_TTL_MS = 30_000;
let sourceNavReadAt = 0;

let seed = {};
let generation = 0;
const gen = () => generation;

boot();

function boot() {
  audio.init();
  mountBar();
  wireGlobal();

  // Expose the shell-mount + route entry points so the admin boost router
  // (app.js) can re-mount the player after it swaps the shell back into
  // <main> — that is what lets audio survive a round trip out to a Server
  // page and back to the library. isPlayerPath lets the router recognise
  // which paths are ours without duplicating the route table.
  window.__player = { isPlayerPath, mountShell, route };

  if (document.getElementById("player-root")) mountShell();
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
  // Forced: a fresh shell has an empty group, and the mount is the one
  // moment the reader is guaranteed to be looking at the rail.
  void refreshSourceNav(true);
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
  "composers", "genres", "folders", "search", "tracks", "sources",
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
  const sections = seed.sourcesEnabled ? [...SECTIONS, SOURCES_SECTION] : SECTIONS;
  for (const [key, label, href] of sections) {
    // Smart Mixes stays listed even when the feature is off. It used to
    // be skipped, which was coherent while the off-state said "enable
    // this in Settings" — there was nothing to go there for. The page is
    // now where the switch IS, so hiding it left a reader with no way to
    // discover the feature from inside the player at all, while the
    // shell sidebar went on listing it two inches away.
    nav.appendChild(el("a", {
      class: "player-section", text: label,
      attrs: { href, "data-route": "", "data-section": key },
    }));
  }
  // The sources group is painted asynchronously into this container, so
  // renderSections stays synchronous and the rail's own sections are
  // never waiting on a network read to appear.
  nav.appendChild(el("div", { class: "player-source-group", attrs: { id: "player-source-group" } }));
}

/**
 * Paint each library source into the rail, with a dot for whether it is
 * reachable right now.
 *
 * This is where the status actually earns its keep: on the Sources page
 * it answers a question the reader went looking for, and in the rail it
 * answers one they did not have to ask. A source is a place their music
 * lives, so it belongs beside the other ways into the library rather than
 * one click behind them.
 *
 * Failures leave whatever is already painted. A rail that briefly shows a
 * stale dot is better than one that empties itself because a single read
 * timed out — and the next navigation past the TTL retries.
 */
async function refreshSourceNav(force) {
  if (!seed.sourcesEnabled) return;
  const box = document.getElementById("player-source-group");
  if (!box) return;
  const now = Date.now();
  if (!force && box.childElementCount > 0 && now - sourceNavReadAt < SOURCE_NAV_TTL_MS) return;
  sourceNavReadAt = now;
  let sources;
  try {
    sources = (await api.sources()).sources || [];
  } catch {
    return;
  }
  const box2 = document.getElementById("player-source-group");
  if (!box2) return; // the shell was swapped out while the read was in flight
  clear(box2);
  if (!sources.length) return;
  box2.appendChild(el("div", { class: "player-nav-group", text: "Sources" }));
  for (const s of sources) {
    box2.appendChild(sourceNavRow(s));
  }
  markActiveSource();
}

function sourceNavRow(s) {
  const row = el("a", {
    class: "player-source",
    attrs: { href: `/albums?source=${encodeURIComponent(s.id)}`, "data-route": "", "data-source": s.id },
  }, el("span", { class: "player-source-name", text: s.name }));
  if (s.kind !== "upnp") return row;

  // Three states, and the dot alone carries none of them: offline is a
  // RING rather than a filled disc, so the difference survives greyscale
  // and a red/green-colourblind reader, and the word itself is in the
  // accessible name rather than only in a colour.
  const [cls, label] = s.online === true
    ? ["source-status-online", "Online"]
    : s.online === false
      ? ["source-status-offline", "Offline"]
      : ["source-status-unknown", "Status unknown"];
  // The status class goes on a WRAPPER, exactly as it does on the Sources
  // page, not on the row. It sets `color`, and the dot reads that through
  // currentcolor — put it on the row and the source's NAME turns red too,
  // which reads as an error rather than a status and fights the rail's own
  // "you are here" ink.
  row.appendChild(el("span", { class: `source-status ${cls}` },
    el("span", { class: "source-dot", attrs: { "aria-hidden": "true" } }),
    // Visible on the page, where there is room for it; here it is in the
    // accessible name only, so the rail stays one line per source.
    el("span", { class: "visually-hidden", text: `, ${label}` })));
  row.setAttribute("title", `${s.name} — ${label}`);
  return row;
}

/** Mark the source row matching the current ?source=, if any. */
function markActiveSource() {
  const want = new URLSearchParams(location.search).get("source") || "";
  for (const a of document.querySelectorAll(".player-source")) {
    const active = want !== "" && a.dataset.source === want;
    a.classList.toggle("active", active);
    // aria-current, set alongside .active exactly as the section rows do
    // — it is what the strip's reveal and assistive tech both read.
    if (active) a.setAttribute("aria-current", "page");
    else a.removeAttribute("aria-current");
  }
}

/**
 * One delegated click handler for every internal link.
 *
 * Modifier-clicks and middle-clicks fall through to the browser
 * untouched — intercepting them would break "open in a new tab", which
 * is the one browser affordance a library grid most needs.
 */
function wireLinks() {
  document.addEventListener("click", (e) => {
    if (e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
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
  return label ? { label, href: location.pathname + location.search } : null;
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
    typeof e.href === "string" && e.href.startsWith("/") && !e.href.startsWith("//"));
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
const trailRoots = new Set(["/", "/search", SOURCES_SECTION[2], ...SECTIONS.map(([, , href]) => href)]);

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

/** pushState with the route trail attached. Every push goes through here. */
function pushRoute(href) {
  history.pushState({ scrollY: 0, trail: trailFor(href) }, "", href);
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

function route() {
  const view = document.getElementById("player-view");
  const titleEl = document.getElementById("player-title");
  if (!view) return;

  generation += 1;
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
  markActiveSource();
  // TTL-guarded, so navigating around the library does not re-read the
  // source list on every hop while a dropped upstream still turns its dot
  // red within the window.
  void refreshSourceNav(false);
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
  // And the same reasoning once more, for the reads themselves: a fetch
  // the previous view started must not be able to paint. getJSON's
  // per-key abort cannot do this — two views with different keys race,
  // and the loser wins. See abortReads in api.js.
  abortReads();
  const ctx = { params, gen, setToolbar, setCrumb, id: rest,
    mixesEnabled: !!seed.mixesEnabled, trail: readTrail() };

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
    sources: ["Sources", () => renderSources(view, ctx)],
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
  // focus is the only part that was wrong.
  if (document.activeElement !== document.getElementById("player-search-input")) {
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
  const myGen = gen();
  const state = history.state || {};
  void Promise.resolve(render()).then(() => {
    if (myGen !== gen()) return;
    window.scrollTo({ top: state.scrollY || 0 });
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
// TestSidebarPlayerNavAgreesWithBoot.
function updateSidebarNav(section) {
  const nav = document.getElementById("primary-nav");
  if (!nav) return;
  // A detail route belongs to its grid's entry: /mix/x is Smart mixes and
  // /playlist/x is Playlists, not Browse. Kept in step with the server's
  // playerNavEntry by TestPlayerNavEntriesMatchTheLayout.
  const detailOwner = { mix: "mixes", playlist: "playlists" };
  const owner = detailOwner[section] || section;
  // Compared as a value, not interpolated into a selector. `section` is a
  // path segment straight off location.pathname, so a URL carrying a quote
  // or a bracket would build a malformed selector and querySelector throws
  // a DOMException — taking route() down with it and leaving the page
  // unrendered. CSS.escape would also fix that; not building the selector
  // at all is one fewer thing to remember.
  const links = [...nav.querySelectorAll("a")];
  const match = links.find((a) => a.dataset.playerSection === owner) ||
    links.find((a) => a.dataset.tab === "player" && !a.dataset.playerSection) ||
    null;
  for (const a of links) {
    if (a === match) a.setAttribute("aria-current", "page");
    else a.removeAttribute("aria-current");
  }
}

window.addEventListener("scroll", () => {
  if (!document.getElementById("player-root")) return;
  const s = history.state || {};
  if (Math.abs((s.scrollY || 0) - window.scrollY) < 40) return;
  history.replaceState({ ...s, scrollY: window.scrollY }, "");
}, { passive: true });
