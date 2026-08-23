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
import {
  renderAlbums, renderAlbum, renderArtists, renderArtist,
  renderGenres, renderComposers, renderFavorites, renderPlaylists,
  renderMixes, renderFolders, renderSearch,
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

  const root = document.getElementById("player-root");
  if (!root) return; // a server page — audio and the bar are all we owe it
  seed = readSeed();
  root.removeAttribute("data-booting");

  renderSections();
  wireLinks();
  wireSearchShortcut();
  window.addEventListener("popstate", () => route());
  window.addEventListener("player:rerender", () => route());
  route();
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

  // Operator links leave the player, which is a full page load and
  // therefore stops playback. While something is actually playing,
  // send them to a new tab instead — the queue survives either way via
  // sessionStorage, but not interrupting is better than restoring.
  document.addEventListener("click", (e) => {
    const a = e.target.closest("#primary-nav a, .subnav a");
    if (!a || e.defaultPrevented || e.metaKey || e.ctrlKey || e.button !== 0) return;
    if (!audio.snapshot().playing) return;
    const url = new URL(a.href, location.origin);
    if (url.pathname === "/" || url.pathname.startsWith("/album")) return;
    e.preventDefault();
    window.open(url.href, "_blank", "noopener");
  });
}

function wireSearchShortcut() {
  document.addEventListener("keydown", (e) => {
    if (e.key !== "/" || e.metaKey || e.ctrlKey || e.altKey) return;
    const t = e.target;
    if (t && (t.tagName === "INPUT" || t.tagName === "TEXTAREA" || t.isContentEditable)) return;
    e.preventDefault();
    navigate("/search");
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
  const ctx = { params, gen, setToolbar, id: rest, mixesEnabled: !!seed.mixesEnabled };

  const routes = {
    albums: ["Albums", () => renderAlbums(view, ctx)],
    album: ["Album", () => renderAlbum(view, ctx)],
    artists: ["Artists", () => renderArtists(view, ctx)],
    artist: ["Artist", () => renderArtist(view, ctx)],
    genres: ["Genres", () => renderGenres(view, ctx)],
    composers: ["Composers", () => renderComposers(view, ctx)],
    favorites: ["Favorites", () => renderFavorites(view, ctx)],
    playlists: ["Playlists", () => renderPlaylists(view, ctx)],
    mixes: ["Smart Mixes", () => renderMixes(view, ctx)],
    folders: ["Folders", () => renderFolders(view, ctx)],
    search: ["Search", () => renderSearch(view, ctx)],
  };
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
