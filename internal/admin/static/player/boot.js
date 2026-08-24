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
    if (key === "mixes" && !seed.mixesEnabled) continue;
    nav.appendChild(el("a", {
      class: "player-section", text: label,
      attrs: { href, "data-route": "", "data-section": key },
    }));
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
    const url = new URL(a.href, location.origin);
    if (url.origin !== location.origin) return;
    e.preventDefault();
    if (url.href === location.href) return;
    history.pushState({ scrollY: 0 }, "", url);
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
  const commit = () => {
    const q = input.value.trim();
    const entering = location.pathname !== "/search";
    const url = q ? `/search?q=${encodeURIComponent(q)}` : "/search";
    if (entering) history.pushState({ scrollY: 0 }, "", url);
    else history.replaceState(history.state || {}, "", url);
    route();
  };

  input.addEventListener("input", () => {
    clearTimeout(timer);
    // 250 ms: long enough that a fast typist issues one request per
    // pause, short enough to feel live. api.js aborts the in-flight
    // request per keystroke, so a slow response can't overwrite a
    // newer one.
    timer = setTimeout(commit, 250);
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

export function navigate(href) {
  history.pushState({ scrollY: 0 }, "", href);
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
  const ctx = { params, gen, setToolbar, setCrumb, id: rest, mixesEnabled: !!seed.mixesEnabled };

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
  titleEl.focus({ preventScroll: true });
  announce(title);

  const state = history.state || {};
  void Promise.resolve(render()).then(() => {
    window.scrollTo({ top: state.scrollY || 0 });
  });
}

window.addEventListener("scroll", () => {
  if (!document.getElementById("player-root")) return;
  const s = history.state || {};
  if (Math.abs((s.scrollY || 0) - window.scrollY) < 40) return;
  history.replaceState({ ...s, scrollY: window.scrollY }, "");
}, { passive: true });
