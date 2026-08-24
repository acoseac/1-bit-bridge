// The section views. Each render* returns nothing and paints into the
// container it is handed; each is responsible for its own loading and
// error states.

import { api, coverURL, artistImageURL, bookletURL, downloadURL, isAborted } from "./api.js";
import { duration, totalDuration, qualityLabel, formatChip, plural, unplayableReason } from "./format.js";
import { el, clear, link, cover, chip, spinner, emptyState, errorState, chunkAppend, onVisible, alphabetRail, aboutBlock } from "./ui.js";
import * as audio from "./audio.js";

const PAGE = 60;

// The album-grid filters filterAlbums accepts. Kept as one list so a new
// axis is forwarded and preserved by every helper below at once.
const AXIS_FILTERS = ["artist", "genre", "composer"];

// ---- Albums grid ----

export async function renderAlbums(view, ctx) {
  const { params, setToolbar, scopeLabel } = ctx;
  const sort = params.get("sort") || "recent";
  const quality = params.get("quality") || "all";
  // artist / genre / composer narrow the grid. filterAlbums has always
  // implemented all three (including intersection), but they were never
  // forwarded — so every genre and composer link landed on the FULL
  // library and the filter looked broken from the outside.
  const scope = {};
  for (const key of AXIS_FILTERS) {
    const v = params.get(key);
    if (v) scope[key] = v;
  }
  setToolbar(albumToolbar(sort, quality));

  // One paging engine, not two. The grid used to carry its own copy of
  // the fetch/sentinel/chunk loop, which is why the A–Z rail and the
  // jump-reset appeared on every browse view EXCEPT the biggest one.
  await renderPagedList(view, ctx, {
    fetchPage: (offset) => api.albums({ sort, quality, ...scope, offset, limit: PAGE }),
    pick: (r) => r.albums,
    make: albumTile,
    containerClass: "grid",
    countNoun: "album",
    label: scopeLabel,
    emptyTitle: "No albums here",
    emptyDetail: Object.keys(scope).length
      ? "Nothing here matches the current filter."
      : quality === "all"
        ? "Add a library root and run a scan."
        : "Nothing in the library matches this quality filter.",
  });
}

function albumTile(a) {
  const q = qualityLabel(a.quality);
  return link(`/album/${a.id}`, { class: "tile" },
    cover(coverURL(a, 500), a.title),
    el("div", { class: "tile-body" },
      el("span", { class: "tile-title", text: a.title || "Unknown album" }),
      el("span", { class: "tile-sub", text: a.albumArtist || "" }),
      el("span", { class: "tile-meta" },
        a.year ? el("span", { text: String(a.year) }) : null,
        q ? chip(q, "chip-quality") : null,
        a.routed && a.routedOnline === false ? chip("offline", "chip-warn") : null)));
}

function albumToolbar(sort, quality) {
  const bar = el("div", { class: "toolbar" });
  bar.appendChild(select("sort", sort, [
    ["recent", "Recently Added"], ["artist", "Artist"], ["title", "Title"], ["year", "Year"],
  ]));
  bar.appendChild(select("quality", quality, [
    ["all", "All Qualities"], ["dsd", "Any DSD"], ["dsd64", "DSD64"], ["dsd128", "DSD128"],
    ["dsd256Plus", "DSD256+"], ["hiresPCM", "Hi-Res PCM"], ["cdQuality", "CD Quality"],
    ["lossy", "Lossy"],
  ]));
  return bar;
}

function select(name, value, options) {
  const s = el("select", { class: "toolbar-select", attrs: { "aria-label": name } });
  for (const [v, label] of options) {
    const o = el("option", { text: label, attrs: { value: v } });
    if (v === value) o.selected = true;
    s.appendChild(o);
  }
  s.addEventListener("change", () => {
    // Mutating the CURRENT url preserves any axis filter already on it,
    // so changing sort inside a genre does not silently widen the view
    // back to the whole library.
    const u = new URL(location.href);
    u.searchParams.set(name, s.value);
    // replaceState, not push: four sort attempts should not cost four
    // presses of Back to leave the page.
    history.replaceState(history.state, "", u);
    window.dispatchEvent(new CustomEvent("player:rerender"));
  });
  return s;
}

// ---- Album detail ----

export async function renderAlbum(view, { id, setToolbar }) {
  setToolbar(null);
  clear(view);
  view.appendChild(spinner());
  let d;
  try {
    d = await api.album(id);
  } catch (e) {
    if (isAborted(e)) return;
    clear(view);
    view.appendChild(errorState(e));
    return;
  }
  clear(view);
  const a = d.album;
  const art = coverURL(a, 500);

  const actions = el("div", { class: "detail-actions" },
    el("button", {
      class: "btn btn-primary", text: "Play",
      on: { click: () => audio.playQueue(d.tracks, 0, { albumArt: art }) },
    }),
    el("button", {
      class: "btn", text: "Shuffle",
      on: {
        click: () => {
          audio.setShuffle(true);
          audio.playQueue(d.tracks, 0, { albumArt: art });
        },
      },
    }),
    el("button", {
      class: "btn", text: "Add to queue",
      on: { click: () => audio.enqueue(d.tracks) },
    }));

  const unplayable = d.tracks.filter((t) => t.play && t.play.kind === "none").length;

  // The About card belongs with the release, next to the buttons — not
  // stranded under the track list where a long album buries it off the
  // bottom of the page. It is also the one part that is often absent
  // (Atlas has to have matched the release), so the layout above it must
  // read complete without it.
  const about = aboutBlock(d.release, { title: "About this release" });

  view.appendChild(el("div", { class: "detail" },
    el("div", { class: "detail-art" }, cover(art, a.title)),
    el("div", { class: "detail-head" },
      el("h2", { class: "detail-title", text: a.title || "Unknown album" }),
      link(`/artist/${a.artistId}`, { class: "detail-artist", text: a.albumArtist || "" }),
      albumStatLine(a),
      unplayable > 0
        ? el("p", { class: "muted small", text:
            `${unplayable} of ${d.tracks.length} can't play in a browser — download those instead.` })
        : null,
      actions,
      d.booklet ? bookletLink(d.booklet) : null,
      about)));

  view.appendChild(trackList(d.tracks, art));
}

/**
 * The stat line under an album title.
 *
 * Deliberately does NOT repeat the album artist: it is the link on the
 * line directly above, and spending the first slot on a duplicate pushed
 * out the detail nobody else shows. The DTO has carried rateHz, bits and
 * discCount all along — they were simply never rendered, so the page
 * said "Hi-Res" where it could say "FLAC 96/24".
 */
function albumStatLine(a) {
  const parts = [];
  if (a.year) parts.push(String(a.year));
  if (a.discCount > 1) parts.push(plural(a.discCount, "disc"));
  parts.push(plural(a.trackCount, "track"));
  const dur = totalDuration(a.duration);
  if (dur) parts.push(dur);

  const line = el("p", { class: "detail-stats muted small", text: parts.join(" · ") });
  // Format rides as a chip rather than more grey text: it is the one
  // fact on this line a listener actually scans for.
  const fmt = albumFormatLabel(a);
  if (fmt) line.appendChild(chip(fmt, "chip-quality"));
  return line;
}

/**
 * "FLAC 96/24" for an album, falling back to the coarse quality tier.
 *
 * formatChip() in format.js answers the same question for a TRACK; an
 * album has no codec of its own, so this reads the geometry the fold
 * already voted on and only falls back to the tier ("Hi-Res", "CD") when
 * the album mixes formats or carries no geometry at all.
 */
function albumFormatLabel(a) {
  const tier = qualityLabel(a.quality);
  if (Array.isArray(a.qualities) && a.qualities.length > 1) return tier; // mixed — don't imply one
  if (!a.rateHz) return tier;
  const khz = (a.rateHz / 1000).toFixed(a.rateHz % 1000 ? 1 : 0);
  const geometry = a.bits ? `${khz}/${a.bits}` : `${khz} kHz`;
  return tier ? `${tier} · ${geometry}` : geometry;
}

function bookletLink(booklet) {
  const href = bookletURL(booklet.mbid);
  return el("p", {},
    el("a", { class: "btn btn-quiet", text: "Booklet (PDF)",
      attrs: { href, target: "_blank", rel: "noopener" } }),
    booklet.state === "pending"
      ? el("span", { class: "muted small", text: " — downloading, try again shortly" })
      : null);
}

function trackList(tracks, albumArt) {
  const list = el("ol", { class: "tracks" });
  let disc = null;
  tracks.forEach((t, i) => {
    if (t.disc && t.disc !== disc) {
      disc = t.disc;
      list.appendChild(el("li", { class: "track-disc", text: `Disc ${disc}` }));
    }
    list.appendChild(trackRow(t, i, tracks, albumArt));
  });
  return list;
}

function trackRow(t, i, all, albumArt) {
  const playable = !t.play || t.play.kind !== "none";
  const row = el("li", { class: `track${playable ? "" : " track-unplayable"}` });
  row.appendChild(el("span", { class: "track-num", text: t.track ? String(t.track) : String(i + 1) }));
  const title = el("button", {
    class: "track-title", text: t.title || t.path,
    attrs: playable ? {} : { "aria-disabled": "true", "aria-describedby": "why-" + i },
  });
  if (playable) {
    title.addEventListener("click", () => audio.playQueue(all, i, { albumArt }));
  }
  row.appendChild(title);
  if (!playable) {
    row.appendChild(el("span", { class: "chip chip-warn", text: unplayableReason(t), attrs: { id: "why-" + i } }));
  }
  row.appendChild(el("span", { class: "track-meta", text: formatChip(t) }));
  row.appendChild(el("span", { class: "track-dur", text: duration(t.duration) }));
  row.appendChild(el("a", {
    class: "track-dl", text: "Download",
    attrs: { href: downloadURL(t.path), download: "" },
  }));
  return row;
}

// ---- Artists ----

export async function renderArtists(view, ctx) {
  ctx.setToolbar(null);
  await renderPagedList(view, ctx, {
    fetchPage: (offset) => api.artists({ offset, limit: PAGE }),
    pick: (r) => r.artists,
    make: artistTile,
    containerClass: "grid grid-round",
    countNoun: "artist",
    emptyTitle: "No artists yet",
  });
}

/**
 * A round artist tile.
 *
 * The image is a three-step cascade, because most libraries have a
 * portrait for only some artists and a grid of placeholder glyphs looks
 * broken rather than sparse:
 *   1. the cached portrait, when `hasImage` says one exists — asked for
 *      at 250 px, since these are small circles and the stored file is
 *      a full-size download;
 *   2. otherwise the artist's top album cover — carried as an artwork
 *      ref on the artist row, so coverURL() works on it unchanged;
 *   3. otherwise a monogram, which `cover()` already falls back to when
 *      the src is null.
 *
 * hasImage is what keeps step 1 from firing a request per artist and
 * eating a 404 for everyone without a portrait.
 */
function artistTile(a) {
  const src = a.hasImage && a.artistMBID
    ? artistImageURL(a.artistMBID, 250)
    : coverURL(a, 250);
  const tile = link(`/artist/${a.id}`, { class: "tile tile-round" },
    cover(src, a.name),
    el("div", { class: "tile-body" },
      el("span", { class: "tile-title", text: a.name }),
      el("span", { class: "tile-sub", text: plural(a.albumCount, "album") })));
  if (!src) tile.querySelector(".cover")?.setAttribute("data-monogram", monogram(a.name));
  return tile;
}

/**
 * Initials for the no-artwork fallback. Two letters at most, from the
 * first two words — enough to tell tiles apart at a glance without
 * turning the grid into a wall of text.
 */
function monogram(name) {
  return (name || "?").trim().split(/\s+/).slice(0, 2)
    .map((w) => [...w][0] || "").join("").toUpperCase() || "?";
}


export async function renderArtist(view, { id, gen, setToolbar }) {
  setToolbar(null);
  clear(view);
  view.appendChild(spinner());
  let d;
  try {
    d = await api.artist(id);
  } catch (e) {
    if (isAborted(e)) return;
    clear(view);
    view.appendChild(errorState(e));
    return;
  }
  clear(view);
  const portrait = d.hasImage ? artistImageURL(d.artist.artistMBID, 500) : null;
  view.appendChild(el("div", { class: "detail detail-artist-head" },
    el("div", { class: "detail-art detail-art-round" }, cover(portrait, d.artist.name)),
    el("div", { class: "detail-head" },
      el("h2", { class: "detail-title", text: d.artist.name }),
      el("p", { class: "muted small",
        text: `${plural(d.artist.albumCount, "album")} · ${plural(d.artist.trackCount, "track")}` }))));
  const about = aboutBlock(d.about, { title: "About" });
  if (about) view.appendChild(about);
  const grid = el("div", { class: "grid" });
  view.appendChild(el("h3", { class: "section-head", text: "Discography" }));
  view.appendChild(grid);
  // chunkAppend, not a bare forEach: the artist detail returns the whole
  // discography unpaginated, so a prolific artist built every tile in
  // one synchronous pass and dropped frames doing it.
  chunkAppend(grid, d.albums, albumTile, gen);
}

// ---- Genres / Composers ----

export function renderGenres(view, ctx) {
  return renderAxis(view, ctx, api.genres, "genre", "No genres tagged",
    "Genre tags fill in as the library is scanned and enriched.");
}

export function renderComposers(view, ctx) {
  return renderAxis(view, ctx, api.composers, "composer", "No composers tagged",
    "Composer tags are read from the files; classical releases usually carry them.");
}

async function renderAxis(view, ctx, fetcher, kind, emptyTitle, emptyDetail) {
  ctx.setToolbar(null);
  await renderPagedList(view, ctx, {
    fetchPage: (offset) => fetcher({ offset, limit: PAGE }),
    pick: (r) => r.entries,
    make: (e) => link(`/${kind}/${e.id}`, { class: "row" },
      el("span", { class: "row-title", text: e.name }),
      el("span", { class: "row-meta",
        text: `${plural(e.albumCount, "album")} · ${plural(e.trackCount, "track")}` })),
    countNoun: kind,
    emptyTitle, emptyDetail,
  });
}

/**
 * One genre's or one composer's albums.
 *
 * `/genre/{id}` and `/composer/{id}` were registered server-side and
 * claimed by PLAYER_HEADS, but boot.js's route table had no case for
 * either — so both fell through to the album grid, unfiltered and
 * titled "Albums", with no sign of which genre had been clicked.
 *
 * Implemented as the album grid with the axis pinned rather than as a
 * separate view: the grid already has the paging, the toolbar and the
 * tile, and the only thing missing was the filter and a name for it.
 * The axis id is taken from the PATH, so it survives a sort change and
 * cannot be dropped by a toolbar rebuild.
 */
export async function renderAxisAlbums(view, ctx, kind) {
  const id = ctx.id;
  if (!id) return renderAlbums(view, ctx);
  const params = new URLSearchParams(ctx.params);
  params.set(kind, id);

  // The label lookup is a second round trip, so the route can change
  // under it. api.genres/api.composers share the "axis" key and so abort
  // each OTHER, but navigating to a DIFFERENT section does not — the
  // fetch completes and setAxisTitle would then stamp a genre name onto
  // whatever page the reader is now looking at. The generation counter
  // is what route() bumps on every navigation, so comparing it is the
  // reliable test rather than relying on which requests happen to share
  // a key.
  const myGen = ctx.gen();
  let label = "";
  try {
    const r = await (kind === "genre" ? api.genres({ limit: 200 }) : api.composers({ limit: 200 }));
    if (ctx.gen() !== myGen) return;
    label = (r.entries || []).find((e) => e.id === id)?.name || "";
  } catch (e) {
    if (isAborted(e) || ctx.gen() !== myGen) return;
    // A missing label costs a heading, never the albums — the grid below
    // is driven by the id, not the name.
    label = "";
  }
  if (label) setAxisTitle(label);
  return renderAlbums(view, { ...ctx, params, scopeLabel: label });
}

/** Retitle the page once the axis name is known. */
function setAxisTitle(label) {
  const h = document.getElementById("player-title");
  if (h) h.textContent = label;
  const base = document.title.split(" — ").slice(1).join(" — ");
  document.title = base ? `${label} — ${base}` : label;
}

/**
 * The paged-list engine behind every browse view.
 *
 * Replaces a single `{limit: 200}` fetch that silently truncated at the
 * server's own cap — a library with more than 200 artists showed the
 * first 200 with no count line and no hint that the rest existed.
 *
 * `fetchPage(offset)` returns the raw response; `pick` extracts the
 * items; `make` builds one node. `containerClass` chooses rows or a
 * grid, which is the only difference between the artist tiles and the
 * genre list.
 */
async function renderPagedList(view, ctx, opts) {
  const { gen } = ctx;
  const { fetchPage, pick, make, containerClass = "rows",
    emptyTitle, emptyDetail, countNoun = "", label = "" } = opts;

  clear(view);
  view.appendChild(spinner());

  let offset = 0;
  let total = 0;
  let loading = false;
  let disposeSentinel = null;
  let container = null;
  const sentinel = el("div", { class: "sentinel" });

  // A jump is a RESET, not an append. page() appends into the existing
  // container, so jumping to "S" (offset 240) while the reader has only
  // scrolled through offset 40 would splice 240 straight after 40 — one
  // list, two discontiguous alphabets, and a sentinel that then pages on
  // from the wrong place. So: tear down the observer, drop the nodes,
  // reset the cursor, and let page() rebuild from the new offset.
  function resetTo(newOffset) {
    if (disposeSentinel) { disposeSentinel(); disposeSentinel = null; }
    sentinel.remove();
    offset = newOffset;
    loading = false;
    if (container) clear(container);
    void page({ jumped: true });
  }

  async function page({ jumped = false } = {}) {
    if (loading) return;
    loading = true;
    try {
      const r = await fetchPage(offset);
      const items = pick(r) || [];
      const first = !container || jumped;
      if (first) {
        total = r.total ?? items.length;
        if (total === 0) {
          clear(view);
          view.appendChild(emptyState(emptyTitle, emptyDetail));
          return;
        }
        if (!container) {
          clear(view);
          if (countNoun) {
            view.appendChild(el("p", { class: "muted small",
              text: label ? `${plural(total, countNoun)} in ${label}` : plural(total, countNoun) }));
          }
          const rail = alphabetRail(r.buckets, (b) => resetTo(b.offset));
          if (rail) view.appendChild(rail);
          container = el("div", { class: containerClass });
          view.appendChild(container);
        }
        view.appendChild(sentinel);
        disposeSentinel = onVisible(sentinel, () => {
          if (offset < total) void page();
        });
        if (jumped) container.scrollIntoView({ block: "start", behavior: "auto" });
      }
      chunkAppend(container, items, make, gen);
      offset += items.length;
      // A page that returns nothing would otherwise leave the sentinel
      // armed and spin: stop on a short page as well as a full one.
      if (items.length === 0 || offset >= total) {
        if (disposeSentinel) { disposeSentinel(); disposeSentinel = null; }
        sentinel.remove();
      }
    } catch (e) {
      if (isAborted(e)) return;
      clear(view);
      container = null;
      view.appendChild(errorState(e, () => { offset = 0; void page(); }));
    } finally {
      loading = false;
    }
  }
  await page();
}

// ---- Favorites / Playlists / Mixes / Folders / Search ----

export async function renderFavorites(view, { setToolbar }) {
  setToolbar(null);
  clear(view);
  view.appendChild(spinner());
  try {
    const r = await api.favorites();
    clear(view);
    const tracks = r.tracks || [];
    const albums = r.albums || [];
    if (!tracks.length && !albums.length) {
      view.appendChild(emptyState("No favorites yet",
        "Hearts sync from the 1-bit app when a device backs them up to this bridge."));
      return;
    }
    if (albums.length) {
      view.appendChild(el("h3", { class: "section-head", text: `Albums (${albums.length})` }));
      const list = el("div", { class: "rows" });
      albums.forEach((a) => list.appendChild(el("div", { class: "row" },
        el("span", { class: "row-title", text: a.album || "Unknown album" }),
        el("span", { class: "row-meta", text: a.albumArtist || "" }))));
      view.appendChild(list);
    }
    if (tracks.length) {
      view.appendChild(el("h3", { class: "section-head", text: `Tracks (${tracks.length})` }));
      const list = el("div", { class: "rows" });
      tracks.forEach((t) => list.appendChild(el("div", { class: "row" },
        el("span", { class: "row-title", text: t.title || t.path || "" }),
        el("span", { class: "row-meta", text: t.artist || "" }))));
      view.appendChild(list);
    }
  } catch (e) {
    if (isAborted(e)) return;
    clear(view);
    view.appendChild(errorState(e));
  }
}

export async function renderPlaylists(view, ctx) {
  ctx.setToolbar(null);
  await renderPagedList(view, ctx, {
    // Neither of these endpoints paginates, so offset is unused and the
    // engine stops after one short page. Routed through it anyway for
    // the generation guard: these two used to pass `() => 0` as gen(),
    // which makes chunkAppend's stale-render check inert — a fast
    // navigation away mid-render could interleave rows into the next
    // view.
    fetchPage: () => api.playlists(),
    pick: (r) => r.playlists || r || [],
    make: (p) => el("div", { class: "row" },
      el("span", { class: "row-title", text: p.name || p.id }),
      // trackCount, NOT itemCount: /api/playlists emits trackCount (a SQL
      // COUNT over playlist_items) and /api/smart-playlists emits
      // itemCount. This row was written against the mixes row below and
      // inherited the wrong key, so every playlist read "0 tracks".
      el("span", { class: "row-meta", text: plural(p.trackCount ?? 0, "track") })),
    emptyTitle: "No playlists backed up",
    emptyDetail: "Playlists appear here when a paired device has playlist backup switched on.",
  });
}

export async function renderMixes(view, ctx) {
  const { setToolbar, mixesEnabled } = ctx;
  setToolbar(null);
  if (!mixesEnabled) {
    clear(view);
    view.appendChild(emptyState("Smart mixes are off",
      "Enable them in Settings → Audio; they are generated from your listening history."));
    return;
  }
  await renderPagedList(view, ctx, {
    fetchPage: () => api.mixes(),
    pick: (r) => r.playlists || r || [],
    make: (m) => el("div", { class: "row" },
      el("span", { class: "row-title", text: m.title || m.slug }),
      el("span", { class: "row-meta", text: plural(m.itemCount ?? 0, "track") })),
    emptyTitle: "No mixes generated yet",
    emptyDetail: "Mixes are rebuilt periodically once there is listening history to work from.",
  });
}

export async function renderFolders(view, { params, setToolbar }) {
  setToolbar(null);
  const path = params.get("path") || "";
  clear(view);
  view.appendChild(spinner());
  try {
    const r = await api.browse(path);
    clear(view);
    if (path) {
      const up = path.includes("/") ? path.slice(0, path.lastIndexOf("/")) : "";
      view.appendChild(link(`/folders?path=${encodeURIComponent(up)}`, { class: "btn btn-quiet", text: "← Up" }));
      view.appendChild(el("p", { class: "muted small", text: path }));
    }
    const list = el("div", { class: "rows" });
    (r.folders || []).forEach((f) => list.appendChild(
      link(`/folders?path=${encodeURIComponent(f.path)}`, { class: "row" },
        el("span", { class: "row-title", text: `📁 ${f.name}` }),
        el("span", { class: "row-meta", text: `${f.trackCount} tracks` }))));
    (r.tracks || []).forEach((t) => list.appendChild(el("div", { class: "row" },
      el("span", { class: "row-title", text: t.name }),
      el("span", { class: "row-meta", text: t.codec || "" }))));
    if (!list.childElementCount) {
      view.appendChild(emptyState("Nothing here"));
      return;
    }
    view.appendChild(list);
  } catch (e) {
    if (isAborted(e)) return;
    clear(view);
    view.appendChild(errorState(e));
  }
}

export async function renderSearch(view, { params, setToolbar }) {
  setToolbar(null);
  const q = (params.get("q") || "").trim();
  clear(view);
  if (q.length < 2) {
    view.appendChild(emptyState("Search the library",
      "Type at least two characters. Albums and artists match on a folded key, so " +
      "\u201cbeatles\u201d finds \u201cThe Beatles\u201d."));
    return;
  }
  view.appendChild(spinner());
  let r;
  try {
    r = await api.search(q);
  } catch (e) {
    if (isAborted(e)) return;
    clear(view);
    view.appendChild(errorState(e));
    return;
  }
  clear(view);

  const albums = r.albums || [];
  const artists = r.artists || [];
  const tracks = r.tracks || [];
  if (!albums.length && !artists.length && !tracks.length) {
    view.appendChild(emptyState(`Nothing matches \u201c${q}\u201d`,
      r.tracksAvailable === false
        ? "Track search is unavailable on this bridge (SQLite built without FTS5), so only " +
          "albums and artists were searched."
        : null));
    return;
  }

  if (albums.length) {
    view.appendChild(el("h3", { class: "section-head", text: "Albums" }));
    const grid = el("div", { class: "grid" });
    albums.forEach((a) => grid.appendChild(
      link(`/album/${a.id}`, { class: "tile" },
        cover(coverURL(a, 500), a.name),
        el("div", { class: "tile-body" },
          el("span", { class: "tile-title", text: a.name }),
          el("span", { class: "tile-sub", text: a.detail || "" })))));
    view.appendChild(grid);
  }

  if (artists.length) {
    view.appendChild(el("h3", { class: "section-head", text: "Artists" }));
    const list = el("div", { class: "rows" });
    artists.forEach((a) => list.appendChild(
      link(`/artist/${a.id}`, { class: "row" },
        el("span", { class: "row-title", text: a.name }),
        el("span", { class: "row-meta", text: a.detail || "" }))));
    view.appendChild(list);
  }

  if (tracks.length) {
    view.appendChild(el("h3", { class: "section-head", text: "Tracks" }));
    const list = el("div", { class: "rows" });
    tracks.forEach((t) => {
      const label = el("span", { class: "row-title", text: t.title || t.path });
      const meta = el("span", { class: "row-meta",
        text: [t.artist, t.album].filter(Boolean).join(" \u00b7 ") });
      // A track hit links to its ALBUM: that is where it can be played,
      // and the album view is what gives it context.
      list.appendChild(t.albumId
        ? link(`/album/${t.albumId}`, { class: "row" }, label, meta)
        : el("div", { class: "row" }, label, meta));
    });
    view.appendChild(list);
  } else if (r.tracksAvailable === false) {
    view.appendChild(el("p", { class: "muted small",
      text: "Track search is unavailable on this bridge (SQLite built without FTS5)." }));
  }
}
