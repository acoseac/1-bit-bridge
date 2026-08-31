// The section views. Each render* returns nothing and paints into the
// container it is handed; each is responsible for its own loading and
// error states.

import { api, coverURL, artistImageURL, collectionCoverURL, bookletURL, downloadURL,
         playlistExportURL, isAborted } from "./api.js";
import { duration, totalDuration, qualityLabel, formatChip, plural, unplayableReason,
         bytes, variantKindLabel, variantSkipLabel, timeAgo } from "./format.js";
import { el, clear, link, cover, chip, spinner, emptyState, errorState, chunkAppend, onVisible, alphabetRail, aboutBlock, detailTabs, crumbs, announce } from "./ui.js";
import * as audio from "./audio.js";
import { variantPanel, onVariantChange } from "./variants.js";

const PAGE = 60;

// The album-grid filters filterAlbums accepts. Kept as one list so a new
// axis is forwarded and preserved by every helper below at once.
//
// "source" belongs here for the same reason the other three do: it is a
// query param the server intersects into the same allow-set, so it must
// survive a sort change, a jump and a genre drill exactly as they do.
const AXIS_FILTERS = ["artist", "genre", "composer", "source"];

// The variant kinds a coverage readout reports, in the order they are
// shown. One list, and the labels come from variantKindLabel — so the
// folder rows and the folder's own summary line cannot name or order
// them differently, and a third kind reaches both at once.
const COVERAGE_KINDS = ["upscale", "optimize"];

// The top-level section each detail view hangs off, as ONE table.
//
// A crumb pointing at a path the shell does not own would fall out of
// the player and take a full page load with it — which stops playback,
// the one thing the client-side router exists to prevent. Keeping the
// hrefs here rather than inline at seven call sites means a renamed
// section is one edit, and TestCrumbRootsAreRealSections pins these
// against boot.js's own SECTIONS list so a rename that misses this
// table fails loudly instead of shipping seven dead links.
const CRUMB_ROOTS = {
  albums: { label: "Albums", href: "/albums" },
  artists: { label: "Artists", href: "/artists" },
  genres: { label: "Genres", href: "/genres" },
  composers: { label: "Composers", href: "/composers" },
  playlists: { label: "Playlists", href: "/playlists" },
  mixes: { label: "Smart Mixes", href: "/mixes" },
  folders: { label: "Folders", href: "/folders" },
};

/**
 * The crumb ancestors for a detail page: the route the reader actually
 * took, when the router recorded one, and the page's own structural
 * chain when it did not.
 *
 * The fallback is not a degraded mode — it is the right answer for a
 * pasted link, a new tab or a reload into a fresh entry, where there is
 * no route to report and the structural chain is the only true one.
 *
 * Folders deliberately do NOT use this: a folder path IS a hierarchy, it
 * is derivable without any history, and drilling down produces the same
 * trail anyway.
 */
function crumbAncestors(trail, structural) {
  return trail?.length ? trail : structural;
}

/**
 * The banner every browse grid shows while a source filter is active.
 *
 * Without it the filter is invisible: a scoped album grid looks exactly
 * like a library that is missing most of its music, and the only clue is
 * a query parameter. It carries the way out as well as the state, since
 * the toolbar selects preserve the scope by design.
 *
 * The name is resolved from the sources list rather than passed down,
 * because the scope survives navigation between four different grids and
 * threading a label through all of them would mean four chances to drop
 * it. A failed lookup still renders — with the generic wording, since
 * being unable to name the source is not a reason to hide that one is
 * applied.
 */
function sourceScopeBanner(sourceID) {
  if (!sourceID) return null;
  const box = el("div", { class: "scope-banner" });
  const label = el("span", { class: "scope-label", text: "Filtered to one source" });
  const clearURL = new URL(location.href);
  clearURL.searchParams.delete("source");
  box.append(label, link(clearURL.pathname + clearURL.search,
    { class: "scope-clear", text: "Show all sources" }));
  // Returned synchronously and named later, so the grid never waits on
  // a second request to paint. The banner already says the true thing
  // without the name; the name only makes it a better sentence.
  //
  // sourceNames, not sources: the map is memoised (the banner renders on
  // every scoped grid) and it does not share the Sources page's request
  // key, so the two cannot abort each other mid-navigation.
  api.sourceNames().then((names) => {
    const name = names.get(sourceID);
    if (name) label.textContent = `Showing ${name}`;
  }).catch(() => {
    /* the banner's job is to say a filter is on; the name is a bonus */
  });
  return box;
}

/**
 * A player route with the CURRENT source scope carried along.
 *
 * Detail pages had no scope in their URL, so clicking an album inside
 * Chord 2go landed on /album/<id> — and the sidebar reverted to Browse,
 * the rail un-narrowed, and the reader was out of the source without
 * having asked to leave. The scoped GRIDS were fixed by rewriting the
 * rail's own links; this is the same rule for the links a view builds.
 *
 * Reads the live URL rather than taking the scope as an argument, so a
 * tile builder cannot be given the wrong one — and so a builder shared by
 * a scoped and an unscoped grid needs no branch at all.
 */
function scopedRoot(root) {
  return { label: root.label, href: scopedHref(root.href) };
}

function scopedHref(path) {
  const source = new URLSearchParams(location.search).get("source");
  return source ? `${path}?source=${encodeURIComponent(source)}` : path;
}

/**
 * The crumb ancestors, rooted at the library source when one is in scope.
 *
 * "Chord 2go > Albums > Waltz for Debby" answers, at a glance, the thing
 * the source facet exists for: where this music actually lives. Without
 * it a scoped detail page is indistinguishable from an unscoped one.
 *
 * The name is looked up rather than passed down because the scope
 * survives navigation across four different views, and threading a label
 * through all of them is four chances to drop it. A failed lookup yields
 * no root rather than a placeholder: a crumb that says "Source" tells the
 * reader less than one that says nothing.
 */
async function sourceRootedCrumbs(trail, structural) {
  const items = crumbAncestors(trail, structural);
  const source = new URLSearchParams(location.search).get("source");
  if (!source) return items;
  try {
    const name = (await api.sourceNames()).get(source);
    if (!name) return items;
    return [{ label: name, href: scopedHref("/albums") }, ...items];
  } catch {
    return items;
  }
}

// ---- Albums grid ----

export async function renderAlbums(view, ctx) {
  const { params, setToolbar, scopeLabel } = ctx;
  const sort = params.get("sort") || "recent";
  const quality = params.get("quality") || "all";
  const needs = params.get("needs") || "all";
  // artist / genre / composer narrow the grid. filterAlbums has always
  // implemented all three (including intersection), but they were never
  // forwarded — so every genre and composer link landed on the FULL
  // library and the filter looked broken from the outside.
  const scope = {};
  for (const key of AXIS_FILTERS) {
    const v = params.get(key);
    if (v) scope[key] = v;
  }
  setToolbar(albumToolbar(sort, quality, needs));

  // One paging engine, not two. The grid used to carry its own copy of
  // the fetch/sentinel/chunk loop, which is why the A–Z rail and the
  // jump-reset appeared on every browse view EXCEPT the biggest one.
  await renderPagedList(view, ctx, {
    fetchPage: (offset) => api.albums({ sort, quality, needs, ...scope, offset, limit: PAGE }),
    pick: (r) => r.albums,
    make: albumTile,
    containerClass: "grid",
    countNoun: "album",
    label: scopeLabel,
    banner: sourceScopeBanner(scope.source),
    emptyTitle: "No albums here",
    emptyDetail: emptyGridDetail({ needs, quality, scoped: Object.keys(scope).length > 0 }),
  });
}

/**
 * Why the album grid is empty, in the terms of whatever narrowed it.
 *
 * The order is most-specific-first: a variant filter explains itself
 * before an axis filter does, and an axis filter before a quality one,
 * because that is the order the reader most recently touched them.
 * "Add a library root" is the only answer that means the LIBRARY is
 * empty rather than the view.
 */
function emptyGridDetail({ needs, quality, scoped }) {
  if (needs !== "all") return "Every album that can take these already has them.";
  if (scoped) return "Nothing here matches the current filter.";
  if (quality !== "all") return "Nothing in the library matches this quality filter.";
  return "Add a library root and run a scan.";
}

function albumTile(a) {
  const q = qualityLabel(a.quality);
  return link(scopedHref(`/album/${a.id}`), { class: "tile" },
    cover(coverURL(a, 500), a.title),
    el("div", { class: "tile-body" },
      el("span", { class: "tile-title", text: a.title || "Unknown album" }),
      el("span", { class: "tile-sub", text: a.albumArtist || "" }),
      el("span", { class: "tile-meta" },
        a.year ? el("span", { text: String(a.year) }) : null,
        q ? chip(q, "chip-quality") : null,
        a.routed && a.routedOnline === false ? chip("offline", "chip-warn") : null),
      variantBadge(a.variants)));
}

/**
 * The tile's coverage badge.
 *
 * Only DONE and STALE are shown. An album that is merely missing copies
 * gets no badge, because most of a library is in that state on any
 * bridge that has not run a full pass — a badge on nearly every tile is
 * wallpaper, and the "needs" filter is the way to ask that question
 * anyway. What earns a mark is a state worth noticing at a glance:
 * finished, or finished-but-rotten.
 */
function variantBadge(cov) {
  if (!cov) return null;
  const wrap = el("span", { class: "tile-variants" });
  for (const [key, label] of [["upscale", "Hi-res"], ["optimize", "CarPlay"]]) {
    const c = cov[key];
    if (!c || c.eligible === 0) continue;
    if (c.stale > 0) {
      wrap.appendChild(el("span", {
        class: "tile-variant tile-variant-stale", text: label,
        attrs: { title: `${label}: ${c.stale} of ${c.covered} out of date` },
      }));
    } else if (c.covered >= c.eligible) {
      wrap.appendChild(el("span", {
        class: "tile-variant", text: label,
        attrs: { title: `${label}: all ${c.eligible} covered` },
      }));
    }
  }
  return wrap.childElementCount ? wrap : null;
}

function albumToolbar(sort, quality, needs) {
  const bar = el("div", { class: "toolbar" });
  bar.appendChild(select("sort", sort, [
    ["recent", "Recently Added"], ["artist", "Artist"], ["title", "Title"], ["year", "Year"],
  ]));
  bar.appendChild(select("quality", quality, [
    ["all", "All Qualities"], ["dsd", "Any DSD"], ["dsd64", "DSD64"], ["dsd128", "DSD128"],
    ["dsd256Plus", "DSD256+"], ["hiresPCM", "Hi-Res PCM"], ["cdQuality", "CD Quality"],
    ["lossy", "Lossy"],
  ]));
  // "Needs" is the question the Inspector's folder tree existed to
  // answer, asked of the library instead of a directory.
  bar.appendChild(select("needs", needs, [
    ["all", "Any variant state"],
    ["optimize", "Needs CarPlay"],
    ["upscale", "Needs hi-res"],
    ["stale", "Out-of-date copies"],
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

// appendDeleteAction adds a Delete button to a detail toolbar when the console
// allows deleting. It is fire-and-forget: a failure to learn the setting leaves
// the toolbar as it was, which is the safe direction for a destructive control.
async function appendDeleteAction(actions, tracks, label) {
  const paths = (tracks || []).map((t) => t.path).filter(Boolean);
  if (!paths.length) return;
  let cfg;
  try {
    cfg = await api.settings();
  } catch {
    return;
  }
  if (!cfg || !cfg.allowDelete) return;

  const btn = el("button", {
    class: "btn danger", text: "Delete…",
    on: {
      click: async () => {
        const n = paths.length;
        if (!confirm(
          `Move ${n} file${n === 1 ? "" : "s"} from "${label}" to the trash?\n\n` +
          `They stay recoverable from the Library page until the trash is emptied.`)) {
          return;
        }
        btn.disabled = true;
        btn.textContent = "Deleting…";
        try {
          const res = await api.trash(paths);
          const failed = (res.outcomes || []).filter((o) => o.status === "failed");
          btn.textContent = failed.length
            ? `${res.ok} deleted, ${failed.length} failed`
            : `${res.ok} moved to trash`;
        } catch (e) {
          btn.disabled = false;
          btn.textContent = "Delete…";
          alert(e && e.message ? e.message : String(e));
        }
      },
    },
  });
  actions.appendChild(btn);
}

export async function renderAlbum(view, { id, setToolbar, setCrumb, trail }) {
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

  // Structural, not a record of how the reader got here: an album is
  // reachable from the grid, an artist, a genre, a playlist and search,
  // so "where you came from" is a different answer every time and is
  // not in the URL. The artist chain is true from every one of those
  // routes and is the only up-link the page has — the .detail-artist
  // line below is the same destination, but it reads as a byline, not
  // as a way out.
  //
  // A provenance trail (?from=..., or a stack in history.state) was
  // considered and left out: it makes the URL unshareable and the same
  // page render differently for two readers, and Back already answers
  // the literal question.
  const albumName = a.title || "Unknown album";
  // The artist chain is the structural fallback, not the first choice:
  // an album opened from a composer or a genre belongs, for the reader,
  // to the list they were just in. The artist stays one click away as the
  // byline beside the cover either way.
  const structural = a.artistId
    ? [scopedRoot(CRUMB_ROOTS.artists),
       { label: a.albumArtist || "Unknown artist", href: scopedHref(`/artist/${a.artistId}`) }]
    : [scopedRoot(CRUMB_ROOTS.albums)];
  setCrumb(crumbs(await sourceRootedCrumbs(trail, structural), albumName));
  // The heading names the album, the way every other detail page names
  // its subject. It used to stay the generic word "Album", which left the
  // page with no heading of its own and put a category label between the
  // trail and the title beside the cover.
  setAxisTitle(albumName);

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

  // Delete is appended ASYNCHRONOUSLY and only when the operator has turned
  // it on, so a bridge without it renders exactly the toolbar it did before.
  // Deleting moves the files to the library's trash rather than unlinking
  // them, which is why the confirm can be a plain one: it is recoverable, and
  // the Library page is where the space is actually reclaimed.
  appendDeleteAction(actions, d.tracks, a.title);

  const unplayable = d.tracks.filter((t) => t.play && t.play.kind === "none").length;

  view.appendChild(el("div", { class: "detail" },
    el("div", { class: "detail-art" }, cover(art, a.title)),
    el("div", { class: "detail-head" },
      // No title here: setAxisTitle above has put it in the page heading,
      // and rendering it again beside the cover printed the album name
      // twice — the same reason renderCollectionDetail dropped its own.
      link(`/artist/${a.artistId}`, { class: "detail-artist", text: a.albumArtist || "" }),
      albumStatLine(a),
      unplayable > 0
        ? el("p", { class: "muted small", text:
            `${unplayable} of ${d.tracks.length} can't play in a browser — download those instead.` })
        : null,
      actions,
      d.booklet ? bookletLink(d.booklet) : null)));

  // Tracks, About and Variants are TABS rather than a stack. Stacked,
  // the About card and the variant panel together pushed the first
  // track most of a screen down — on a one-track album with nothing to
  // generate, entirely below the fold. Both are things the reader asks
  // for deliberately; the track list is what they came for.
  //
  // Nothing is lost by hiding the coverage: the album tile in the grid
  // carries its variant badge, and each track row carries its own
  // marks, so "does this have CarPlay copies" is still answerable from
  // the default tab.
  //
  // The variant panel's onChanged callback is the DELETE path's
  // refresh: deletion is synchronous, so its numbers are already true
  // when the response lands. Generation deliberately does not use it —
  // see variants.js.
  const variants = variantPanel(d.variants, { albumIds: [id] }, rerenderView, { plain: true });
  appendIf(view, detailTabs(`album:${id}`, [
    // An empty track list is a truthy <ol>, so detailTabs would keep the
    // tab and open on a blank panel. Say what happened instead: a row
    // deleted between the catalog build and this fetch is the realistic
    // way to get here, and "no tracks" is the useful thing to know.
    {
      id: "tracks", label: "Tracks",
      panel: d.tracks.length
        ? await albumTracksPanel(d.tracks, art)
        : emptyState("No tracks here",
            "This album's files are gone from the library — a rescan will remove it."),
    },
    { id: "about", label: "About", panel: aboutBlock(d.release, { plain: true }) },
    { id: "variants", label: "Variants", panel: variants },
  ]));

  // Generation IS asynchronous, so the numbers just rendered are a
  // snapshot of a moving target. app.js re-broadcasts the pool's
  // progress from the console's existing SSE stream; re-rendering on it
  // is what keeps the bars from sitting stale until a manual reload.
  // Only worth hooking when there is a panel to refresh — a bridge that
  // sent no summary would otherwise re-render the whole page for
  // numbers it never showed.
  if (variants) onVariantChange(rerenderView);
}

/** appendChild that tolerates a null child. */
function appendIf(parent, node) {
  if (node) parent.appendChild(node);
}

/**
 * Re-run the current view in place.
 *
 * Dispatched through the shell's rerender event rather than calling
 * renderAlbum directly: route() owns the generation counter that
 * invalidates an in-flight render, and a direct call would paint over a
 * view the user has already navigated away from.
 */
function rerenderView() {
  window.dispatchEvent(new CustomEvent("player:rerender"));
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

  // Size joins the line rather than the chip: it is a fact about the
  // files, like the track count, not a claim about their quality.
  const size = bytes(a.sizeBytes);
  if (size) parts.push(size);

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

/**
 * @param {object} [opts]
 * @param {boolean} [opts.collection] - render as an ordered COLLECTION
 *   rather than an album: no disc headings, and rows numbered by their
 *   position in the list.
 *
 *   A playlist spanning six albums is not an album. Disc headings would
 *   punctuate it with "Disc 1" wherever a member happened to come from a
 *   multi-disc release, and album track numbers would run 3, 1, 8, 2 —
 *   both true of the underlying files and both meaningless here, where
 *   the ORDER is the thing the user chose.
 */
/**
 * An album's track list, with a source filter when the album spans more
 * than one place.
 *
 * A mixed album is an ordinary shape on a hybrid bridge — the same
 * release ripped locally AND present on an upstream — and until this the
 * page gave no sign of it: five rows, one track count, one modal quality
 * chip, and nothing to say that two of them live somewhere that can go
 * offline.
 *
 * The list is NOT filtered to the source you browsed in. An album is one
 * release, and showing two of its five tracks because of how you arrived
 * would read as tracks missing rather than as a filter.
 */
async function albumTracksPanel(tracks, art) {
  const ids = albumSourceIDs(tracks);
  if (ids.length < 2) return trackList(tracks, art);

  let names;
  try {
    names = await api.sourceNames();
  } catch {
    // The split is real whether or not we can name the halves; without
    // names there is nothing useful to label them with, so fall back to
    // the plain list rather than to "Unknown source" twice.
    return trackList(tracks, art);
  }

  const box = el("div");
  let current = "";
  const paint = () => {
    clear(box);
    box.appendChild(sourceFilterBar(ids, names, current, (v) => {
      current = v;
      paint();
    }));
    const shown = current
      ? tracks.filter((t) => (t.sourceId || LOCAL_SOURCE_ID) === current)
      : tracks;
    // The per-track chip earns its cell only in the combined view: inside
    // a filtered one every row has the same answer, which the button
    // above already gives.
    box.appendChild(trackList(shown, art, current ? {} : { sourceNames: names }));
  };
  paint();
  return box;
}

/**
 * Where one track of a mixed album lives.
 *
 * Only ever rendered when the album actually spans sources: on a
 * single-source album every row would carry the same chip, which is
 * wallpaper rather than information — the same reasoning that keeps the
 * variant marks off tracks with nothing to say.
 *
 * Absence of `sourceId` means the filesystem. That is what keeps the
 * field off every row of a pure-filesystem library, where the answer is
 * never in question.
 */
function sourceChip(t, names) {
  const id = t.sourceId || LOCAL_SOURCE_ID;
  const name = names.get(id);
  return name ? chip(name, "chip-quiet") : null;
}

// The facet id of the bridge's own filesystem, mirroring
// librarycat.LocalSourceID. A track carries no sourceId when it is local,
// so this is what absence resolves to.
const LOCAL_SOURCE_ID = "local";

/**
 * The distinct sources an album's tracks come from, in a stable order.
 *
 * Derived from the TRACKS rather than from the album, because those are
 * the rows actually on screen: hydrateTracks drops anything deleted or
 * newly duplicate-suppressed since the catalog snapshot, and a chip
 * describing a row that is not there would be worse than none.
 */
function albumSourceIDs(tracks) {
  const seen = [];
  for (const t of tracks || []) {
    const id = t.sourceId || LOCAL_SOURCE_ID;
    if (!seen.includes(id)) seen.push(id);
  }
  return seen;
}

/**
 * The per-source filter shown above a mixed album's track list.
 *
 * "All" stays first and is the default, because an album is one release
 * and playing it whole is the ordinary thing to want — a page that opened
 * on a filtered subset would read as an album with missing tracks. The
 * per-source views answer the other question: which of these can this
 * bridge play on its own, and which need the upstream to be up.
 */
function sourceFilterBar(ids, names, current, onPick) {
  const bar = el("div", { class: "source-filter", attrs: { role: "group",
    "aria-label": "Filter tracks by source" } });
  const opts = [["", "All"], ...ids.map((id) => [id, names.get(id) || "Unknown source"])];
  for (const [value, label] of opts) {
    const b = el("button", {
      class: `source-filter-btn${value === current ? " active" : ""}`,
      text: label, attrs: { type: "button", "aria-pressed": String(value === current) },
      on: { click: () => onPick(value) },
    });
    bar.appendChild(b);
  }
  return bar;
}

function trackList(tracks, albumArt, opts = {}) {
  const list = el("ol", { class: "tracks" });
  let disc = null;
  tracks.forEach((t, i) => {
    if (!opts.collection && t.disc && t.disc !== disc) {
      disc = t.disc;
      list.appendChild(el("li", { class: "track-disc", text: `Disc ${disc}` }));
    }
    list.appendChild(trackRow(t, i, tracks, albumArt, opts));
  });
  return list;
}

function trackRow(t, i, all, albumArt, opts = {}) {
  const playable = !t.play || t.play.kind !== "none";
  const row = el("li", { class: `track${playable ? "" : " track-unplayable"}` });
  const num = opts.collection ? i + 1 : (t.track || i + 1);
  row.appendChild(el("span", { class: "track-num", text: String(num) }));
  const title = el("button", {
    class: "track-title", text: t.title || t.path,
    attrs: playable ? {} : { "aria-disabled": "true", "aria-describedby": "why-" + i },
  });
  if (playable) {
    title.addEventListener("click", () => audio.playQueue(all, i, { albumArt }));
  }
  row.appendChild(title);
  // The reason cell is ALWAYS appended, empty for a playable track, so
  // every row has the same number of cells. That is what lets the list
  // share one grid (see .track's subgrid rule) — with a conditional cell
  // the 7-child rows would auto-place their metadata into the 8-child
  // rows' columns and nothing would line up. The wrapper carries no chip
  // styling of its own; the chip goes inside it only when there is one.
  //
  // An empty one does NOT simply vanish under the shared grid: the reason
  // column is `auto`, so once any row carries a chip every row reserves
  // that width — which is exactly what puts the following columns on a
  // common x. It costs nothing on an album where nothing is unplayable.
  // Where the columns are not shared (the no-subgrid fallback, and
  // mobile) the stylesheet hides it, because there it would only add a
  // gap.
  row.appendChild(el("span", { class: "track-why" },
    playable ? null : el("span", {
      class: "chip chip-warn", text: unplayableReason(t), attrs: { id: "why-" + i },
    })));
  // Always appended, empty unless this album spans sources — the same
  // constant-cell rule .track-why documents above. A conditional cell
  // would put the 8-child rows' metadata in the 9-child rows' columns and
  // nothing below the title would line up.
  row.appendChild(el("span", { class: "track-src" },
    opts.sourceNames ? sourceChip(t, opts.sourceNames) : null));
  row.appendChild(el("span", { class: "track-meta", text: formatChip(t) }));
  row.appendChild(variantMarks(t));
  row.appendChild(el("span", { class: "track-size", text: bytes(t.sizeBytes) }));
  row.appendChild(el("span", { class: "track-dur", text: duration(t.duration) }));
  row.appendChild(el("a", {
    class: "track-dl", text: "Download",
    attrs: { href: downloadURL(t.path), download: "" },
  }));
  return row;
}

/**
 * The per-track variant marks.
 *
 * Two states are worth distinguishing and a third is not. A FRESH
 * variant is a copy that will actually be served; a STALE one is a copy
 * the serve path answers 410 for, so showing it as plain presence would
 * promise something that does not exist. "Absent" gets no mark at all —
 * a column of empty placeholders down a track list is noise, and the
 * album-level bar above already says how many are missing.
 *
 * A track that can never take a variant says so once, in place of the
 * marks, because a permanent impossibility is a different fact from a
 * gap someone could close.
 */
function variantMarks(t) {
  const wrap = el("span", { class: "track-variants" });
  const skip = variantSkipLabel(t.variantSkip);
  if (skip) {
    wrap.appendChild(el("span", {
      class: "track-variant-skip", text: "—", attrs: { title: skip },
    }));
    return wrap;
  }
  for (const v of t.variants || []) {
    const label = variantKindLabel(v.kind);
    wrap.appendChild(el("span", {
      class: `track-variant${v.fresh ? "" : " track-variant-stale"}`,
      text: label,
      attrs: { title: variantMarkTitle(v, label) },
    }));
  }
  return wrap;
}

/** The tooltip on a variant mark: what it is, or why it will not serve. */
function variantMarkTitle(v, label) {
  if (!v.fresh) {
    return `${label} copy is out of date — the source changed after it was made`;
  }
  const parts = [label, variantGeometry(v), bytes(v.sizeBytes)].filter(Boolean);
  return parts.length > 1 ? `${parts[0]} ${parts.slice(1).join(" · ")}` : parts[0];
}

function variantGeometry(v) {
  if (!v.rateHz) return "";
  const khz = (v.rateHz / 1000).toFixed(v.rateHz % 1000 ? 1 : 0);
  return v.bits ? `${khz}/${v.bits}` : `${khz} kHz`;
}

// ---- Artists ----

export async function renderArtists(view, ctx) {
  ctx.setToolbar(null);
  const source = ctx.params.get("source") || "";
  await renderPagedList(view, ctx, {
    banner: sourceScopeBanner(source),
    fetchPage: (offset) => api.artists({ source, offset, limit: PAGE }),
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
    ? artistImageURL(a.artistMBID, 250, a.imageVersion)
    : coverURL(a, 250);
  const tile = link(scopedHref(`/artist/${a.id}`), { class: "tile tile-round" },
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


export async function renderArtist(view, { id, gen, setToolbar, setCrumb, trail }) {
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
  // Falls back for an EMPTY name, not an absent d.artist: the lines below
  // dereference it unconditionally, so optional chaining here would title
  // the page "Unknown artist" and then throw three lines later — hiding
  // the failure without preventing it. An empty name is the reachable
  // case, and it would leave the page with no heading at all.
  const artistName = d.artist.name || "Unknown artist";
  setCrumb(crumbs(await sourceRootedCrumbs(trail, [scopedRoot(CRUMB_ROOTS.artists)]), artistName));
  setAxisTitle(artistName);
  const portrait = d.hasImage
    ? artistImageURL(d.artist.artistMBID, 500, d.artist.imageVersion)
    : null;
  view.appendChild(el("div", { class: "detail detail-artist-head" },
    el("div", { class: "detail-art detail-art-round" }, cover(portrait, d.artist.name)),
    el("div", { class: "detail-head" },
      // Named by the heading, like every other detail page.
      el("p", { class: "muted small",
        text: `${plural(d.artist.albumCount, "album")} · ${plural(d.artist.trackCount, "track")}` }))));
  // Same tab set as an album, one level up: the discography is what the
  // page is for, and a long bio plus a variant panel between the header
  // and the first cover is exactly the burial this replaced.
  //
  // No "Discography" heading inside the panel — the tab says Albums,
  // and a heading under it would be the same word twice.
  const grid = el("div", { class: "grid" });
  // An artist is where a bulk variant action actually belongs — "give
  // this whole discography CarPlay copies" is the request, and doing it
  // album by album is the tedium the Inspector's folder tree absorbed.
  const variants = variantPanel(d.variants, { artistId: id }, rerenderView, { plain: true });
  const albums = d.albums.length
    ? grid
    : emptyState("No albums here",
        "This artist's files are gone from the library — a rescan will remove them.");
  appendIf(view, detailTabs(`artist:${id}`, [
    { id: "albums", label: "Albums", panel: albums },
    { id: "about", label: "About", panel: aboutBlock(d.about, { plain: true }) },
    { id: "variants", label: "Variants", panel: variants },
  ]));
  if (variants) onVariantChange(rerenderView);

  // chunkAppend, not a bare forEach: the artist detail returns the whole
  // discography unpaginated, so a prolific artist built every tile in
  // one synchronous pass and dropped frames doing it.
  //
  // After the grid is in the document, so the chunks land in a node the
  // reader can actually see fill. Skipped when the grid was replaced by
  // an empty state and is not in the document at all.
  if (albums === grid) chunkAppend(grid, d.albums, albumTile, gen);
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
  const source = ctx.params.get("source") || "";
  await renderPagedList(view, ctx, {
    banner: sourceScopeBanner(source),
    fetchPage: (offset) => fetcher({ source, offset, limit: PAGE }),
    pick: (r) => r.entries,
    make: (e) => link(scopedHref(`/${kind}/${e.id}`), { class: "row" },
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

  // Set before the label lookup, not after: the crumb is derivable from
  // the ROUTE alone, so it can be on screen while the name is still in
  // flight — and it survives the lookup failing, which is the case
  // where a way back matters most.
  const root = kind === "genre" ? CRUMB_ROOTS.genres : CRUMB_ROOTS.composers;
  const chain = await sourceRootedCrumbs(ctx.trail, [scopedRoot(root)]);
  ctx.setCrumb(crumbs(chain));

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
  if (label) {
    setAxisTitle(label);
    // Re-set now that the name is known, so the trail ends on the page the
    // reader is on. The ancestors-only form above stays as the immediate
    // paint and as the fallback when the lookup fails.
    ctx.setCrumb(crumbs(chain, label));
  }
  return renderAlbums(view, { ...ctx, params, scopeLabel: label });
}

/**
 * Retitle the page once the real name is known.
 *
 * Re-announces as well as retitling. route() announces the SECTION name
 * ("Folders", "Genre") because that is all it knows at dispatch time, so
 * without this a screen reader hears the same word on every folder in a
 * tree while the heading says something different each time — the one
 * reader who cannot see the crumb gets the least orientation.
 */
function setAxisTitle(label) {
  const h = document.getElementById("player-title");
  if (h) h.textContent = label;
  const base = document.title.split(" — ").slice(1).join(" — ");
  document.title = base ? `${label} — ${base}` : label;
  announce(label);
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
    emptyTitle, emptyDetail, countNoun = "", label = "", banner = null } = opts;

  clear(view);
  view.appendChild(spinner());

  let offset = 0;
  let total = 0;
  let loading = false;
  let disposeSentinel = null;
  let container = null;
  const sentinel = el("div", { class: "sentinel" });

  // renderGen invalidates a fetch that was in flight when a jump reset
  // the list.
  //
  // Be precise about what this does and does not fix. The obvious
  // hazard — a stale page appending into the freshly cleared container —
  // is ALREADY prevented, because getJSON aborts the previous request
  // under the same key and every fetchPage here happens to use one. It
  // is not reproducible: four rapid jumps against a build with this
  // guard removed still rendered exactly the last bucket.
  //
  // What the counter buys is that the protection stops being
  // INCIDENTAL. renderPagedList accepts an arbitrary fetchPage and does
  // not require a keyed request, so the day one is added without a key
  // the abort quietly stops covering it. And the abort never covered
  // `loading` at all: the superseded call's `finally` clears it while
  // the jump's own fetch is still running, which lets the sentinel start
  // a third page. That part is a real ordering defect today.
  let renderGen = 0;

  // A jump is a RESET, not an append. page() appends into the existing
  // container, so jumping to "S" (offset 240) while the reader has only
  // scrolled through offset 40 would splice 240 straight after 40 — one
  // list, two discontiguous alphabets, and a sentinel that then pages on
  // from the wrong place. So: tear down the observer, drop the nodes,
  // reset the cursor, and let page() rebuild from the new offset.
  function resetTo(newOffset) {
    renderGen++;
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
    const myGen = renderGen;
    try {
      const r = await fetchPage(offset);
      if (myGen !== renderGen) return; // a jump superseded this page
      const items = pick(r) || [];
      const first = !container || jumped;
      if (first) {
        total = r.total ?? items.length;
        if (total === 0) {
          clear(view);
          // The banner belongs on the EMPTY view too, and most of all
          // there: "no albums here" with no sign of an active filter
          // reads as a broken library rather than a narrow view.
          if (banner) view.appendChild(banner);
          view.appendChild(emptyState(emptyTitle, emptyDetail));
          return;
        }
        if (!container) {
          clear(view);
          if (banner) view.appendChild(banner);
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
      if (isAborted(e) || myGen !== renderGen) return;
      clear(view);
      container = null;
      view.appendChild(errorState(e, () => { offset = 0; void page(); }));
    } finally {
      // Only the CURRENT generation may release the flag: a superseded
      // page clearing it would let the sentinel fire while the jump's
      // own fetch is still in flight.
      if (myGen === renderGen) loading = false;
    }
  }
  await page();
}

// ---- Favorites / Playlists / Mixes / Folders / Search ----

/**
 * Hearted albums and hearted tracks, as two tabs.
 *
 * This used to be two stacked lists of unclickable grey text — the
 * stored backup document printed verbatim, which is the operator's
 * question, not a listener's. Now the albums ARE albums (the same tile
 * as the grid, opening the same page) and the tracks are a playable
 * queue, because both resolve server-side through the same catalog the
 * rest of the player reads.
 *
 * Tabs rather than a stack for the reason the album page has them: a
 * long album grid pushed every hearted track off the bottom of the
 * screen, and the two are answers to different questions asked one at a
 * time.
 */
export async function renderFavorites(view, { gen, setToolbar }) {
  setToolbar(null);
  clear(view);
  view.appendChild(spinner());
  let r;
  try {
    r = await api.favorites();
  } catch (e) {
    if (isAborted(e)) return;
    clear(view);
    view.appendChild(errorState(e));
    return;
  }
  clear(view);
  const albums = r.albums || [];
  const tracks = r.tracks || [];
  const lostAlbums = r.unresolvedAlbums || 0;
  const lostTracks = r.unresolvedTracks || 0;
  // BEFORE the empty-state branch below, not after the tabs. A stored
  // document whose every entry is foreign or deleted returns early, and
  // that is exactly the state where "backed up by X, 3 months ago" does
  // the most work — it is what tells the reader the hearts are real and
  // simply belong somewhere else. (CodeRabbit on PR #763.)
  appendIf(view, collectionProvenance(r));

  // Nothing hearted at all is a different state from nothing that
  // RESOLVES here: the first is a setup hint, the second means the
  // hearts belong to another source and no amount of setup on this
  // bridge will show them.
  if (!albums.length && !tracks.length) {
    view.appendChild(r.stored && (lostAlbums || lostTracks)
      ? emptyState("Nothing hearted from this library",
          `${plural(lostAlbums + lostTracks, "favorite")} came from another bridge, ` +
          "a device's own files, or something removed since — none of them live here.")
      : emptyState("No favorites yet",
          "Hearts sync from the 1-bit app when a device backs them up to this bridge."));
    return;
  }

  const grid = el("div", { class: "grid" });
  // The provenance line is already in place above (and above the tabs,
  // not inside one: it describes the whole stored document, and
  // repeating it on both tabs would say the same thing twice while
  // implying the two halves were pushed separately).
  appendIf(view, detailTabs("favorites", [
    {
      id: "albums", label: `Albums (${albums.length})`,
      panel: albums.length
        ? withUnresolved(grid, lostAlbums, "album")
        : (lostAlbums ? unresolvedOnly(lostAlbums, "album") : null),
    },
    {
      id: "tracks", label: `Tracks (${tracks.length})`,
      panel: tracks.length
        ? favoriteTracksPanel(tracks, lostTracks)
        : (lostTracks ? unresolvedOnly(lostTracks, "track") : null),
    },
  ]));

  // After the grid is in the document, so the reader watches it fill —
  // and only when it actually got there, since a tab whose panel was
  // replaced by the unresolved notice never holds it.
  if (albums.length) chunkAppend(grid, albums, albumTile, gen);
}

/**
 * The three shapes a tab takes when some hearts belong somewhere else.
 *
 * They are said out loud rather than dropped, for the same reason the
 * collection detail reports its own unresolved members: the operator's
 * Favorites panel counts every entry, so a page that quietly shows
 * fewer disagrees with it in a way that reads as a bug.
 *
 * withUnresolved is the tab that HAS content — a note above it.
 * unresolvedOnly is the tab that has none, where the count is the whole
 * answer and needs to explain itself rather than read as "empty".
 * unresolvedNote is the line they share.
 */
function withUnresolved(node, lost, noun) {
  if (!lost) return node;
  return el("div", {}, unresolvedNote(lost, noun), node);
}

function unresolvedOnly(lost, noun) {
  return emptyState(`No hearted ${noun}s from this library`,
    `${plural(lost, noun)} came from another bridge, a device's own files, ` +
    "or something removed since.");
}

function unresolvedNote(lost, noun) {
  return el("p", { class: "muted small", text:
    `${plural(lost, noun)} not shown — from another bridge, or removed since.` });
}

/**
 * The tracks tab: transport, then the list.
 *
 * albumArt is deliberately null. The queue carries ONE cover and
 * favorites span the library, so any choice would be the wrong cover
 * for all but one track — the now-playing bar shows none rather than a
 * confident lie, exactly as it does for a playlist with no cover.
 */
function favoriteTracksPanel(tracks, lost) {
  const box = el("div", {}, collectionActions(tracks, null));
  if (lost) box.appendChild(unresolvedNote(lost, "track"));
  // Numbered by position with no disc headings: favorites are an
  // ordered set drawn from many albums, which is exactly what
  // `collection` means here.
  box.appendChild(trackList(tracks, null, { collection: true }));
  return box;
}

export async function renderPlaylists(view, ctx) {
  ctx.setToolbar(null);
  await renderPagedList(view, ctx, {
    fetchPage: () => api.playlists(),
    pick: (r) => r.collections || [],
    // Subtitle computed here rather than server-side: it is one line of
    // presentation over two fields the response already carries, and
    // baking it into the DTO would make the same string the mix grid's
    // real subtitle (a family's description) shares a field with.
    make: (c) => collectionTile({ ...c, subtitle: playlistSubtitle(c) },
      `/playlist/${c.id}`, "playlist"),
    containerClass: "grid",
    countNoun: "playlist",
    emptyTitle: "No playlists backed up",
    emptyDetail: "Playlists appear here when a paired device has playlist backup switched on.",
  });
}

export async function renderMixes(view, ctx) {
  const { setToolbar, mixesEnabled } = ctx;
  if (!mixesEnabled) {
    // The gear is the POINT of this state: the empty state used to name
    // a page in Settings and leave the reader to walk there, which is
    // the trip this whole tray exists to remove. The switch is restart-
    // required, so it says so after the save rather than pretending the
    // grid will fill in.
    setToolbar(mixesToolbar(null));
    clear(view);
    view.appendChild(emptyState("Smart mixes are off",
      "Turn them on with the gear above — they are generated from your listening history."));
    return;
  }
  // "Regenerate all" came from the retired /smartmixes page. It belongs
  // on the grid rather than on a mix: it runs the whole engine, and
  // every family's contents change at once.
  setToolbar(mixesToolbar(regenerateAllButton()));
  await renderPagedList(view, ctx, {
    fetchPage: () => api.mixes(),
    pick: (r) => r.collections || [],
    make: (c) => collectionTile(c, `/mix/${c.id}`, "smartmix"),
    containerClass: "grid",
    countNoun: "mix",
    emptyTitle: "No mixes generated yet",
    emptyDetail: "Mixes are rebuilt periodically once there is listening history to work from.",
  });
}

/**
 * The mixes grid's toolbar: whatever controls the state has, plus the
 * feature gear.
 *
 * window.BridgeFeatureTray, not an import: the tray lives in app.js,
 * which is a deferred classic script — the same one-way window handshake
 * boot.js uses in the other direction for window.__player. Guarded,
 * because a missing app.js must cost the gear and not the grid.
 *
 * The tray is returned INSIDE the toolbar node rather than placed as a
 * sibling, because setToolbar clears only #player-toolbar: a sibling
 * would survive every route change and stack a copy per visit.
 */
function mixesToolbar(controls) {
  const bar = controls || el("div", { class: "toolbar" });
  const built = window.BridgeFeatureTray?.build({
    title: "Smart mixes",
    blurb: "Auto-generated playlists — Heavy Rotation, Forgotten Favorites, " +
      "Auto Mix — rebuilt daily from what your devices have played.",
    rows: [
      {
        field: "smartPlaylistsEnabled", type: "switch", label: "Generate smart mixes",
      },
      {
        field: "analysisEnabled", type: "switch", label: "Audio analysis",
        hint: "Only the harmonic Auto Mix needs it. The history-based families " +
          "generate without it.",
      },
    ],
    link: { href: "/settings?tab=audio", text: "All audio settings →" },
  });
  if (!built) return bar;
  bar.appendChild(built.button);
  return el("div", { class: "toolbar-stack" }, bar, built.tray);
}

/**
 * The mixes grid's one operator control.
 *
 * A rebuild takes a while and changes every family, so the button
 * reports and then re-routes rather than mutating tiles in place —
 * there is no partial state worth painting.
 */
function regenerateAllButton() {
  const status = el("span", { class: "muted small", attrs: { role: "status" } });
  const btn = el("button", { class: "btn", text: "Regenerate all" });
  btn.addEventListener("click", async () => {
    btn.disabled = true;
    const was = btn.textContent;
    btn.textContent = "Regenerating…";
    status.textContent = "";
    try {
      const r = await api.regenerateMixes();
      status.textContent = r?.families != null
        ? `Rebuilt ${plural(r.families, "mix", "mixes")}.`
        : "Rebuilt.";
      await window.__player?.route?.();
    } catch (e) {
      status.textContent = e.message || "Could not regenerate.";
    } finally {
      btn.disabled = false;
      btn.textContent = was;
    }
  });
  return el("div", { class: "toolbar" }, btn, status);
}

/**
 * "12 tracks · 3d ago" for a playlist tile.
 *
 * The date is the bridge's RECEIPT time, not the client's own
 * last-modified stamp: the question a backup listing answers is "is
 * this device still syncing", and the answer has to be in the
 * bridge's own clock to be trustworthy.
 */
function playlistSubtitle(c) {
  return [plural(c.count ?? 0, "track"), timeAgo(c.updatedAt)].filter(Boolean).join(" · ");
}

/**
 * "Backed up by Arsenie's iPhone · 3d ago", or null.
 *
 * Null for a smart mix, which has no provenance to report — it is
 * generated here — so the shared detail renderer needs no flag: the
 * fields are simply absent and omitempty never put them on the wire.
 *
 * The token prefix rides the title attribute rather than the text. Two
 * devices can share a name ("iPhone") and the prefix is what the CLI
 * and every other console surface key on, so it has to stay
 * recoverable — but it is a hex string, and putting it in the line
 * would make the line about the identifier instead of the device.
 */
function collectionProvenance(c) {
  const when = timeAgo(c.updatedAt);
  if (!c.deviceName && !c.deviceTokenPrefix && !when) return null;
  const who = c.deviceName || c.deviceTokenPrefix;
  const text = who
    ? `Backed up by ${who}${when ? ` · ${when}` : ""}`
    : `Backed up ${when}`;
  const node = el("p", { class: "detail-provenance muted small", text });
  if (c.deviceTokenPrefix) node.title = c.deviceTokenPrefix;
  return node;
}

/**
 * The members that could not be turned into playable rows: a count, and
 * — when the server named them — the list behind a disclosure.
 *
 * Said out loud rather than hidden, because the count above includes
 * them and a silent discrepancy reads as a bug. Collapsed by default
 * because on a healthy playlist there is nothing here, and on an
 * unhealthy one the reader wants the number first and the names second.
 */
function unresolvedBlock(d) {
  const n = d.unresolved || 0;
  if (n <= 0) return null;
  const note = el("summary", { class: "muted small",
    text: `${plural(n, "track")} not in this library — from another bridge, or removed since.` });
  const items = d.unresolvedItems || [];
  if (!items.length) return el("p", { class: "muted small", text: note.textContent });

  const list = el("ol", { class: "unresolved-list" });
  for (const it of items) {
    list.appendChild(el("li", {},
      el("span", { class: "unresolved-pos", text: String((it.position ?? 0) + 1) }),
      el("span", { class: "unresolved-title", text: it.title || it.origin || "Unknown track" }),
      it.artist ? el("span", { class: "unresolved-artist", text: it.artist }) : null,
      chip(it.foreign ? "another source" : "missing", "chip-quiet")));
  }
  if (items.length < n) {
    list.appendChild(el("li", { class: "muted small",
      text: `…and ${n - items.length} more.` }));
  }
  return el("details", { class: "unresolved" }, note, list);
}

/**
 * A playlist or smart-mix tile.
 *
 * Artwork is a ladder, not a single source: an operator-uploaded cover
 * wins outright; otherwise a 2x2 mosaic of the collection's leading
 * DISTINCT album covers; otherwise a single cover when the collection
 * spans fewer than four albums; otherwise the empty-cover glyph. The
 * server has already dropped members with no artwork, so a quadrant is
 * never a hole.
 */
function collectionTile(c, href, scope) {
  return link(href, { class: "tile" },
    collectionArt(c, scope),
    el("div", { class: "tile-body" },
      el("span", { class: "tile-title", text: c.name || c.id }),
      el("span", { class: "tile-sub", text: c.subtitle || plural(c.count ?? 0, "track") })));
}

function collectionArt(c, scope) {
  if (c.hasCover) return cover(collectionCoverURL(scope, c.id), c.name || "");
  const refs = Array.isArray(c.covers) ? c.covers : [];
  if (refs.length >= 4) {
    const box = el("div", { class: "cover cover-mosaic" });
    for (const ref of refs.slice(0, 4)) {
      box.appendChild(el("img", {
        attrs: { src: coverURL(ref, 250), alt: "", loading: "lazy", decoding: "async" },
      }));
    }
    return box;
  }
  return cover(refs.length ? coverURL(refs[0], 500) : null, c.name || "");
}

// ---- Playlist / mix detail ----

export async function renderPlaylistDetail(view, ctx) {
  await renderCollectionDetail(view, ctx, {
    fetch: () => api.playlist(ctx.id),
    scope: "playlist",
    root: CRUMB_ROOTS.playlists,
    // Cover only — a backed-up playlist has no server-side regenerate
    // or snapshot to offer. POST /api/playlists/{id}/cover has existed
    // since the covers work landed with no caller at all: the only UI
    // ever built was the smart-mix half, on a page that is now gone.
    actions: (c) => playlistActions(c),
  });
}

export async function renderMixDetail(view, ctx) {
  await renderCollectionDetail(view, ctx, {
    fetch: () => api.mix(ctx.id),
    scope: "smartmix",
    root: CRUMB_ROOTS.mixes,
    actions: mixActions,
  });
}

async function renderCollectionDetail(view, ctx, opts) {
  ctx.setToolbar(null);
  // Above the title, where a way back belongs. It used to be appended after
  // the Play / Shuffle / Add-to-queue row, which put a navigation link in the
  // middle of an action group — and on a smart mix, which has a SECOND action
  // row below (Regenerate / Save as playlist), it wrapped to a line of its own
  // and read as a third kind of button sandwiched between the two.
  const chain = crumbAncestors(ctx.trail, [opts.root]);
  ctx.setCrumb(crumbs(chain));
  clear(view);
  view.appendChild(spinner());
  let d;
  try {
    d = await opts.fetch();
  } catch (e) {
    if (isAborted(e)) return;
    clear(view);
    view.appendChild(errorState(e));
    return;
  }
  clear(view);
  const c = d.collection;
  const tracks = d.tracks || [];
  const name = c.name || c.id;
  setAxisTitle(name);
  // Re-set with the name now that it is known; the ancestors-only form
  // above was the immediate paint while the fetch was in flight.
  ctx.setCrumb(crumbs(chain, name));

  // The single URL, for the things that can only carry one: the
  // now-playing bar's queue art and the track rows' fallback.
  const art = c.hasCover
    ? collectionCoverURL(opts.scope, c.id)
    : (c.covers?.length ? coverURL(c.covers[0], 500) : null);

  const stats = [plural(c.count ?? tracks.length, "track"), totalDuration(
    tracks.reduce((n, t) => n + (t.duration || 0), 0))].filter(Boolean).join(" · ");

  // No title here: setAxisTitle above has already put this exact string in
  // the page's <h1>, and rendering it again beside the art printed the
  // collection name twice. Albums and artists now do the same — the
  // .detail-title class this note used to carve them out for is gone, and
  // every detail page names its subject in the heading.
  const head = el("div", { class: "detail-head" },
    c.subtitle ? el("p", { class: "detail-artist", text: c.subtitle }) : null,
    el("p", { class: "detail-stats muted small", text: stats }),
    collectionProvenance(c),
    unresolvedBlock(d),
    collectionActions(tracks, art));

  if (opts.actions) head.appendChild(opts.actions(c, view, ctx));

  // The BOX gets collectionArt, not `art` — the same ladder the tile in
  // the grid uses, mosaic and all. Rendering only covers[0] here meant a
  // playlist showed four covers on the way in and one on arrival, which
  // reads as the wrong page having loaded.
  view.appendChild(el("div", { class: "detail" },
    el("div", { class: "detail-art" }, collectionArt(c, opts.scope)), head));

  if (tracks.length) {
    view.appendChild(trackList(tracks, art, { collection: true }));
  } else {
    view.appendChild(emptyState("Nothing playable here",
      "None of this collection's tracks resolve to a file in this library."));
  }
}

function collectionActions(tracks, art) {
  const playable = tracks.filter((t) => !t.play || t.play.kind !== "none");
  const row = el("div", { class: "detail-actions" });
  if (!playable.length) return row;
  row.appendChild(el("button", {
    class: "btn btn-primary", text: "Play",
    on: { click: () => audio.playQueue(tracks, 0, { albumArt: art }) },
  }));
  row.appendChild(el("button", {
    class: "btn", text: "Shuffle",
    on: { click: () => { audio.setShuffle(true); audio.playQueue(tracks, 0, { albumArt: art }); } },
  }));
  row.appendChild(el("button", {
    class: "btn", text: "Add to queue",
    on: { click: () => audio.enqueue(tracks) },
  }));
  return row;
}

/**
 * Regenerate / Save as playlist for a smart mix.
 *
 * Both call the EXISTING operator endpoints — no new backend. Feedback
 * is inline and the buttons disable while in flight, because regenerate
 * runs the whole engine and a second click would queue a second run.
 */
function mixActions(c, view, ctx) {
  const status = el("p", { class: "muted small", attrs: { role: "status" } });
  const box = el("div", { class: "detail-actions detail-actions-secondary" });

  const regen = el("button", { class: "btn btn-quiet", text: "Regenerate" });
  regen.addEventListener("click", async () => {
    regen.disabled = true;
    const was = regen.textContent;
    regen.textContent = "Regenerating…";
    status.textContent = "";
    try {
      const r = await api.regenerateMix(c.id);
      if (r?.removed) {
        status.textContent = "This mix no longer has enough to draw from and was removed.";
        return;
      }
      status.textContent = "Rebuilt.";
      await window.__player?.route?.();
    } catch (e) {
      status.textContent = e.message || "Could not regenerate.";
    } finally {
      regen.disabled = false;
      regen.textContent = was;
    }
  });
  box.appendChild(regen);

  const save = el("button", { class: "btn btn-quiet", text: "Save as playlist" });
  save.addEventListener("click", () => {
    if (box.querySelector(".save-form")) return;
    const input = el("input", {
      class: "toolbar-select save-name",
      attrs: { type: "text", value: c.name || c.id, "aria-label": "Playlist name" },
    });
    const confirm = el("button", { class: "btn btn-primary", text: "Save" });
    const form = el("form", { class: "save-form" }, input, confirm);
    form.addEventListener("submit", async (ev) => {
      ev.preventDefault();
      confirm.disabled = true;
      try {
        const r = await api.saveMixAsPlaylist(c.id, input.value.trim() || c.name);
        status.textContent = `Saved "${r?.name || input.value}" as a playlist.`;
        form.remove();
      } catch (e) {
        status.textContent = e.message || "Could not save.";
        confirm.disabled = false;
      }
    });
    box.appendChild(form);
    input.focus();
    input.select();
  });
  box.appendChild(save);
  box.appendChild(coverControl("smartmix", c, status));

  return el("div", {}, box, status);
}

/**
 * The playlist detail's secondary row: export, then a cover control.
 *
 * Export is the one thing the retired operator table did that had no
 * equivalent here, and it is the reason a backup listing exists at all
 * — a playlist you cannot get out of the bridge is not backed up in any
 * useful sense. Three formats, unchanged from that page: JSON (the
 * document verbatim), CSV (a spreadsheet), M3U8 (openable by a player
 * on the bridge host).
 *
 * location assignment, not fetch: the response carries
 * Content-Disposition, so it has to reach the browser's own download
 * path rather than a JS reader.
 */
function playlistActions(c) {
  const status = el("p", { class: "muted small", attrs: { role: "status" } });
  const box = el("div", { class: "detail-actions detail-actions-secondary" });
  box.appendChild(el("span", { class: "export-label muted small", text: "Export" }));
  for (const fmt of ["json", "csv", "m3u8"]) {
    box.appendChild(el("button", {
      class: "btn btn-quiet", text: fmt.toUpperCase(),
      on: { click: () => { location.href = playlistExportURL(c.id, fmt); } },
    }));
  }
  box.appendChild(coverControl("playlist", c, status));
  return el("div", {}, box, status);
}

/**
 * Set / replace / remove an operator-uploaded cover.
 *
 * The file input is inside its <label>, visually hidden but NOT
 * `hidden`: [hidden] is display:none !important, which drops the input
 * from the tab order, and a <label> is not natively focusable — so the
 * upload would be unreachable by keyboard. The label paints the focus
 * ring via :focus-within. (Same reasoning as the retired page's control,
 * which learned it from a review.)
 *
 * Re-routes on success rather than swapping the <img> src: the cover
 * appears on the tile and in the now-playing bar too, and the read URL
 * is unversioned, so a re-fetch is the only thing that makes every copy
 * of it agree.
 */
function coverControl(scope, c, status) {
  const box = el("span", { class: "cover-control" });

  const input = el("input", {
    class: "sr-only",
    attrs: { type: "file", accept: "image/jpeg,image/png" },
  });
  const label = el("label", { class: "btn btn-quiet" },
    input, c.hasCover ? "Replace cover…" : "Set cover…");
  input.addEventListener("change", async () => {
    const file = input.files?.[0];
    if (!file) return;
    status.textContent = "Uploading…";
    try {
      await api.uploadCover(scope, c.id, await readAsDataURL(file));
      status.textContent = "Cover saved.";
      await window.__player?.route?.();
    } catch (e) {
      status.textContent = e.message || "Upload failed.";
    } finally {
      // Cleared unconditionally, so re-picking the SAME file fires
      // `change` again — without this a failed upload could not be
      // retried with the same image.
      input.value = "";
    }
  });
  box.appendChild(label);

  if (c.hasCover) {
    const remove = el("button", { class: "btn btn-quiet", text: "Remove cover" });
    remove.addEventListener("click", async () => {
      remove.disabled = true;
      status.textContent = "Removing…";
      try {
        await api.deleteCover(scope, c.id);
        status.textContent = "Cover removed.";
        await window.__player?.route?.();
      } catch (e) {
        remove.disabled = false;
        status.textContent = e.message || "Remove failed.";
      }
    });
    box.appendChild(remove);
  }
  return box;
}

/** Read a picked image as a data: URL, which is what the endpoint takes. */
function readAsDataURL(file) {
  return new Promise((resolve, reject) => {
    const fr = new FileReader();
    fr.onload = () => {
      // readAsDataURL always yields a string, but FileReader.result is
      // typed string | ArrayBuffer | null — and String(anArrayBuffer) is
      // "[object ArrayBuffer]", which would be POSTed as an image and
      // rejected by the server with a confusing message. Fail here, where
      // the cause is nameable.
      if (typeof fr.result !== "string") {
        reject(new Error("Could not read that file."));
        return;
      }
      resolve(fr.result);
    };
    fr.onerror = () => reject(new Error("Could not read that file."));
    fr.readAsDataURL(file);
  });
}

export async function renderFolders(view, { params, setToolbar, setCrumb }) {
  setToolbar(null);
  const path = params.get("path") || "";
  // Both derived from the path, so both are painted BEFORE the fetch:
  // they are the answer to "where am I", which is exactly the question a
  // reader has while the folder is still loading, and they stay correct
  // if it fails to load at all.
  //
  // The heading takes the leaf and the crumb takes its ancestors, so the
  // page names itself the way /genre/{id} and a playlist do rather than
  // sitting under a permanent "Folders". The full path used to be a grey
  // line under a lone "← Up" link, which said where you were but gave no
  // way to any level except one step up — on Artist/Album/Disc 03 that
  // meant clicking up twice to reach the artist.
  const segments = pathSegments(path);
  const leaf = segments.length ? segments[segments.length - 1] : "";
  setCrumb(crumbs(folderAncestors(path), leaf));
  if (leaf) setAxisTitle(leaf);

  clear(view);
  view.appendChild(spinner());
  let r;
  try {
    r = await api.browse(path);
  } catch (e) {
    if (isAborted(e)) return;
    clear(view);
    view.appendChild(errorState(e));
    return;
  }
  clear(view);

  // The whole node, not this page. The rollup is the only honest source
  // for a folder's totals — summing the returned page under-counts the
  // moment a node has more children than one page, which on a
  // 647-folder root meant showing half the library.
  view.appendChild(folderSummary(r, path));

  const list = el("div", { class: "rows" });
  (r.folders || []).forEach((f) => list.appendChild(folderRow(f)));
  (r.tracks || []).forEach((t) => list.appendChild(browseTrackRow(t)));
  if (!list.childElementCount) {
    view.appendChild(emptyState("Nothing here"));
    return;
  }
  view.appendChild(list);
}

/**
 * A browse path split into its segments.
 *
 * Empty segments are dropped so a stray or doubled separator — which a
 * hand-edited URL can carry — cannot produce a blank crumb linking to a
 * path that differs from the one it was derived from.
 */
function pathSegments(path) {
  return (path || "").split("/").filter(Boolean);
}

/**
 * The ancestors of a browse path: the root, then every segment ABOVE
 * the current one, each linking to its own level.
 *
 * The leaf is excluded because it is the heading (see renderFolders).
 * At the root the whole trail is empty, so crumbs() returns null and the
 * slot collapses — "Folders › " above a heading that already says
 * Folders would be one word twice and a link to the page you are on.
 */
function folderAncestors(path) {
  const segments = pathSegments(path);
  if (!segments.length) return [];
  const trail = [CRUMB_ROOTS.folders];
  let prefix = "";
  for (const segment of segments.slice(0, -1)) {
    prefix = prefix ? `${prefix}/${segment}` : segment;
    trail.push({ label: segment, href: `/folders?path=${encodeURIComponent(prefix)}` });
  }
  return trail;
}

/**
 * The current folder's variant readout and actions.
 *
 * Folders are the one scope that IS a path prefix, so these act through
 * the folder form — which is also what makes the empty path work as
 * "the whole library", the bulk control that has no album or artist
 * equivalent.
 */
function folderSummary(r, path) {
  const total = r.subtreeTracks || 0;
  if (!total) return el("span");

  // `??`, not `||`. A missing denominator (the store failed) and a
  // genuine zero (nothing here is eligible) are different answers, and
  // `||` silently turns the second into the first — which would render
  // a folder full of finished work as a folder full of missing work.
  const summary = {
    upscale: {
      covered: r.subtreeUpscaled || 0,
      eligible: r.subtreeUpscaleEligible ?? total,
      exempt: Math.max(0, total - (r.subtreeUpscaleEligible ?? total)),
    },
    optimize: {
      covered: r.subtreeOptimized || 0,
      eligible: r.subtreeOptimizeEligible ?? total,
      exempt: Math.max(0, total - (r.subtreeOptimizeEligible ?? total)),
    },
    sourceBytes: r.subtreeSizeBytes || 0,
    variantBytes: 0,
    // The browse endpoint carries no feature/toolchain state, and
    // guessing would either hide working buttons or offer broken ones.
    // Both true means the panel renders its controls and lets the
    // endpoint answer — a 503 with a real message beats a disabled
    // button with an invented one.
    enabled: true,
    soxAvailable: true,
  };
  const heading = el("p", { class: "muted small", text: tracksAndSize(total, r.subtreeSizeBytes) });
  const wrap = el("div", {}, heading);
  // Plain, because the disclosure below supplies the frame and the
  // label — the card chrome inside it would draw a box in a box under a
  // heading that already says "Variants".
  const panel = variantPanel(summary, { path }, rerenderView, { plain: true });
  if (panel) wrap.appendChild(variantDisclosure(summary, panel));
  onVariantChange(rerenderView);
  return wrap;
}

/**
 * Whether the folder view's variant panel is expanded.
 *
 * Remembering it at all is what makes the disclosure usable: the panel
 * re-renders whenever generated variants land (onVariantChange above),
 * so without this, pressing Generate would collapse the panel that
 * reported the result.
 *
 * NOT keyed on the folder path, so walking into a subfolder keeps it
 * open rather than making the reader re-open it at every level of the
 * tree — which is how it is used, one directory at a time.
 *
 * In memory, and deliberately NOT sessionStorage (suggested on review).
 * The player's own analogue, detailTabs' tabMemory, is in memory for the
 * same reason: a fresh load should open on the default, and here the
 * default — collapsed — is the whole point of the change. Persisting it
 * would mean an operator who once expanded the panel gets a screenful
 * of it back on every load thereafter, which is the state this replaced.
 * app.js persists the SETTINGS tab across reloads, but that is a
 * navigation position: losing it lands you somewhere you were not,
 * whereas losing this only re-collapses a tool that is one click away.
 */
let folderVariantsOpen = false;

/**
 * The folder view's variant panel, collapsed behind a summary line.
 *
 * Two coverage bars, four buttons, two notes and a totals line is most
 * of a screen, and on the Folders page it sat above the listing — so
 * browsing a tree meant scrolling past the same panel at every level to
 * reach the folders, which is what the page is for.
 *
 * Nothing is lost while it is closed: the summary carries both ratios,
 * and every folder row already shows its own coverage marks. Kinds with
 * nothing eligible are left out of the summary for the same reason
 * folderRow leaves them out of a row — "0 / 0" is noise that reads like
 * a problem.
 */
function variantDisclosure(summary, panel) {
  const line = el("span", { class: "variants-summary" });
  for (const key of COVERAGE_KINDS) {
    const c = summary[key];
    if (!c || !c.eligible) continue;
    line.appendChild(coverageMark(key, c.covered, c.eligible));
  }
  const box = el("details", { class: "variants variants-disclosure" });
  box.open = folderVariantsOpen;
  box.appendChild(el("summary", { class: "variants-summary-row" },
    el("span", { class: "variants-head", text: "Variants" }), line));
  box.appendChild(panel);
  box.addEventListener("toggle", () => { folderVariantsOpen = box.open; });
  return box;
}

/**
 * One "Hi-res 3/12" coverage mark.
 *
 * Shared by the folder rows and the folder's own summary line so the two
 * can't drift in label, order or the done state — the summary is a
 * roll-up of exactly what the rows below it say.
 */
function coverageMark(kind, covered, eligible) {
  return el("span", {
    class: `row-cov${covered >= eligible ? " row-cov-done" : ""}`,
    text: `${variantKindLabel(kind)} ${covered}/${eligible}`,
    attrs: { "data-kind": kind },
  });
}

/** "12 tracks · 1.2 GB", dropping the size when there isn't one. */
function tracksAndSize(count, sizeBytes) {
  return [plural(count, "track"), bytes(sizeBytes)].filter(Boolean).join(" · ");
}

function folderRow(f) {
  const marks = el("span", { class: "row-coverage" });
  const counts = {
    upscale: [f.upscaledCount || 0, f.upscaleEligibleCount ?? f.trackCount ?? 0],
    optimize: [f.optimizedCount || 0, f.optimizeEligibleCount ?? f.trackCount ?? 0],
  };
  for (const key of COVERAGE_KINDS) {
    const [covered, eligible] = counts[key];
    // A folder with nothing eligible for a kind shows no mark for it at
    // all. "0 / 0" down a long list of folders is noise that reads like
    // a problem.
    if (eligible === 0) continue;
    marks.appendChild(coverageMark(key, covered, eligible));
  }
  return link(`/folders?path=${encodeURIComponent(f.path)}`, { class: "row" },
    el("span", { class: "row-title", text: `📁 ${f.name}` }),
    marks,
    el("span", { class: "row-meta", text: tracksAndSize(f.trackCount || 0, f.totalSizeBytes) }));
}

/**
 * A loose track in the folder tree.
 *
 * Deliberately NOT the album track row: this one has no play button
 * (browse rows carry no playability verdict), and it shows the
 * variant-eligibility skip reason, which is the question a folder view
 * is being asked.
 */
function browseTrackRow(t) {
  const marks = el("span", { class: "row-coverage" });
  const skip = variantSkipLabel(t.skipReason);
  if (skip) {
    marks.appendChild(el("span", { class: "track-variant-skip", text: "—", attrs: { title: skip } }));
  } else {
    for (const [label, has] of [["Hi-res", t.isUpscaled], ["CarPlay", t.isOptimized]]) {
      if (has) marks.appendChild(el("span", { class: "track-variant", text: label }));
    }
  }
  return el("div", { class: "row" },
    el("span", { class: "row-title", text: t.name }),
    marks,
    el("span", { class: "row-meta", text:
      [t.codec, bytes(t.sizeBytes)].filter(Boolean).join(" · ") }));
}

/**
 * A key-filtered track list.
 *
 * This exists because the Smart Mixes harmonic wheel deep-links to a
 * single Camelot code, and the only view that ever answered that was
 * the Library Inspector. Folders would be the wrong home for it —
 * harmonic key has nothing to do with directory structure — so the
 * question gets a view of its own, backed by the same
 * `/api/library/browse?camelot=` the Inspector used, which already
 * returns exactly a filtered track list and no folders.
 */
export async function renderTracks(view, { params, setToolbar, setCrumb, trail }) {
  setToolbar(null);
  const camelot = (params.get("camelot") || "").toUpperCase();
  clear(view);
  if (!/^\d+[AB]$/.test(camelot)) {
    view.appendChild(emptyState("Tracks by key",
      "Open a segment of the harmonic wheel on Smart Mixes to list the tracks in that key."));
    return;
  }
  view.appendChild(spinner());
  let r;
  try {
    r = await api.browseByKey(camelot);
  } catch (e) {
    if (isAborted(e)) return;
    clear(view);
    view.appendChild(errorState(e));
    return;
  }
  clear(view);
  // The human pitch, not the wheel code: "A minor" names the page in the
  // same vocabulary the line below it uses.
  const keyName = r.keyName || camelot;
  setCrumb(crumbs(crumbAncestors(trail, [CRUMB_ROOTS.mixes]), keyName));
  setAxisTitle(keyName);

  const tracks = r.tracks || [];
  // keyName is the human pitch ("A minor"); the code alone is jargon to
  // anyone who has not memorised the wheel.
  view.appendChild(el("p", { class: "muted small", text:
    `${plural(tracks.length, "track")} in ${r.keyName || camelot} (${r.keyFilter || camelot})` }));
  if (!tracks.length) {
    view.appendChild(emptyState("Nothing in this key",
      "No analysed track carries this key. Run an analysis pass if the library is new."));
    return;
  }
  const list = el("div", { class: "rows" });
  tracks.forEach((t) => list.appendChild(browseTrackRow(t)));
  view.appendChild(list);
}

export async function renderSearch(view, { params, setToolbar }) {
  setToolbar(null);
  const q = (params.get("q") || "").trim();
  if (q.length < 2) {
    clear(view);
    view.appendChild(emptyState("Search the library",
      "Type at least two characters. Albums and artists match on a folded key, so " +
      "\u201cbeatles\u201d finds \u201cThe Beatles\u201d."));
    return;
  }

  // Refining a query keeps the previous results on screen, dimmed, while
  // the next one is in flight. Clearing to a spinner on every commit is
  // what made typing feel like a series of separate searches: the page
  // blanked, redrew, and blanked again a word later, so nothing was ever
  // stable long enough to read. A spinner is right for the FIRST search,
  // where there is nothing to keep.
  const previous = view.querySelector(".search-results");
  if (previous) previous.setAttribute("aria-busy", "true");
  else {
    clear(view);
    view.appendChild(spinner());
  }

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
  // Everything below goes into one container, which is also the marker
  // the refinement path above looks for. It has to be a real element
  // rather than a fragment for that reason.
  const results = view.appendChild(el("div", { class: "search-results" }));

  const albums = r.albums || [];
  const artists = r.artists || [];
  const tracks = r.tracks || [];
  if (!albums.length && !artists.length && !tracks.length) {
    results.appendChild(emptyState(`Nothing matches \u201c${q}\u201d`,
      r.tracksAvailable === false
        ? "Track search is unavailable on this bridge (SQLite built without FTS5), so only " +
          "albums and artists were searched."
        : null));
    return;
  }

  if (albums.length) {
    results.appendChild(el("h3", { class: "section-head", text: "Albums" }));
    const grid = el("div", { class: "grid" });
    albums.forEach((a) => grid.appendChild(
      link(`/album/${a.id}`, { class: "tile" },
        cover(coverURL(a, 500), a.name),
        el("div", { class: "tile-body" },
          el("span", { class: "tile-title", text: a.name }),
          el("span", { class: "tile-sub", text: a.detail || "" })))));
    results.appendChild(grid);
  }

  if (artists.length) {
    results.appendChild(el("h3", { class: "section-head", text: "Artists" }));
    const list = el("div", { class: "rows" });
    artists.forEach((a) => list.appendChild(
      link(`/artist/${a.id}`, { class: "row" },
        el("span", { class: "row-title", text: a.name }),
        el("span", { class: "row-meta", text: a.detail || "" }))));
    results.appendChild(list);
  }

  if (tracks.length) {
    results.appendChild(el("h3", { class: "section-head", text: "Tracks" }));
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
    results.appendChild(list);
  } else if (r.tracksAvailable === false) {
    results.appendChild(el("p", { class: "muted small",
      text: "Track search is unavailable on this bridge (SQLite built without FTS5)." }));
  }
}
