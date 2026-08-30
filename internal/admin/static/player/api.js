// Fetch helpers for the player's read surface.
//
// Deliberately does NOT reuse app.js's `API`: that object is a classic
// script global, and reaching for it from a module would make the
// player's load order depend on app.js having run. The one thing worth
// sharing — escapeHTML and the About-card helpers, which carry a
// security review — is exposed on window.__bridge and used explicitly.

const inflight = new Map();

/** GET JSON, cancelling any earlier request under the same key. */
export async function getJSON(url, { key } = {}) {
  if (key && inflight.has(key)) {
    inflight.get(key).abort();
  }
  const ctrl = new AbortController();
  if (key) inflight.set(key, ctrl);
  try {
    const r = await fetch(url, {
      headers: { accept: "application/json" },
      signal: ctrl.signal,
    });
    if (!r.ok) throw await errorFrom(r);
    return await r.json();
  } finally {
    if (key && inflight.get(key) === ctrl) inflight.delete(key);
  }
}

async function errorFrom(r) {
  let msg = `${r.status} ${r.statusText}`;
  try {
    const j = await r.json();
    if (j && (j.message || j.error)) msg = j.message || j.error;
  } catch {
    /* a non-JSON body is not worth a second failure */
  }
  const e = new Error(msg);
  e.status = r.status;
  return e;
}

/**
 * Cancel every in-flight read.
 *
 * getJSON's per-key abort only covers a SECOND request under the SAME
 * key. That is enough for paging inside one view and no help at all
 * across views: leaving /favorites for /albums leaves the favorites
 * fetch running under its own key, and when it lands the view clears
 * itself and paints favorites over the album grid — the URL and the
 * heading say Albums while the body says something else. Reproduced in
 * a browser by delaying one read past a navigation, not theorised.
 *
 * route() calls this up front, beside the crumb and variant-refresh
 * teardown that are there for the same reason: a view the reader has
 * left must not be able to act. Every render already returns on
 * isAborted, so no individual view needs a guard of its own — which is
 * the point, since eleven of them would have had to remember.
 *
 * Deliberately scoped to READS. postJSON / deleteJSON carry no signal
 * and never enter this map, so a generate or a delete stays alive
 * across a navigation — which is what an operator who pressed the
 * button and walked away expects.
 */
export function abortReads() {
  for (const ctrl of inflight.values()) ctrl.abort();
  inflight.clear();
}

/** True for an AbortError, which is a cancellation, not a failure. */
export function isAborted(err) {
  return err && (err.name === "AbortError" || err.code === 20);
}

export const api = {
  albums: (params) => getJSON(`/api/player/albums?${qs(params)}`, { key: "albums" }),
  album: (id) => getJSON(`/api/player/albums/${encodeURIComponent(id)}`, { key: "detail" }),
  artists: (params) => getJSON(`/api/player/artists?${qs(params)}`, { key: "artists" }),
  artist: (id) => getJSON(`/api/player/artists/${encodeURIComponent(id)}`, { key: "detail" }),
  genres: (params) => getJSON(`/api/player/genres?${qs(params)}`, { key: "axis" }),
  composers: (params) => getJSON(`/api/player/composers?${qs(params)}`, { key: "axis" }),
  stats: () => getJSON("/api/player/stats"),
  // The player's own two-tier search: albums and artists from the
  // cached catalog, tracks from FTS5. Not /api/library/search, which is
  // the Inspector's track+folder view and knows nothing about albums.
  search: (q) => getJSON(`/api/player/search?q=${encodeURIComponent(q)}&limit=12`, { key: "search" }),
  // The harmonic-key view. Same endpoint as browse, different question:
  // it answers with a flat track list and no folders, because a key is
  // not a place.
  browseByKey: (camelot) =>
    getJSON(`/api/library/browse?camelot=${encodeURIComponent(camelot)}`, { key: "browse" }),
  browse: (path, cursor) =>
    getJSON(`/api/library/browse?path=${encodeURIComponent(path || "")}` +
      (cursor ? `&afterFolder=${encodeURIComponent(cursor)}` : ""), { key: "browse" }),
  // The player's own collection endpoints, not the operator ones:
  // /api/playlists and /api/smart-playlists are summaries-only by
  // design, so they carry no cover refs and nothing playable.
  playlists: () => getJSON("/api/player/playlists", { key: "playlists" }),
  playlist: (id) => getJSON(`/api/player/playlists/${encodeURIComponent(id)}`, { key: "detail" }),
  mixes: () => getJSON("/api/player/mixes", { key: "mixes" }),
  mix: (slug) => getJSON(`/api/player/mixes/${encodeURIComponent(slug)}`, { key: "detail" }),
  // The PLAYER's favorites, not /api/favorites: that one is the stored
  // backup document verbatim (the operator's Data page reads it), which
  // carries no artwork, no album ids and no playability. This one is
  // joined against the catalog so the tabs can show albums as albums
  // and tracks as a queue.
  favorites: () => getJSON("/api/player/favorites", { key: "favorites" }),

  // Delete reuses the operator trash endpoints unchanged. The settings read
  // is what tells the toolbar whether to offer the control at all.
  settings: () => getJSON("/api/settings", { key: "settings" }),
  trash: (paths) => postJSON("/api/library/trash", { paths }),

  // Mix actions reuse the operator endpoints unchanged — no new backend.
  // They came with the retired /smartmixes page; the player is now the
  // only surface that offers them.
  regenerateMixes: () => postJSON("/api/smart-playlists/regenerate"),
  regenerateMix: (slug) =>
    postJSON(`/api/smart-playlists/${encodeURIComponent(slug)}/regenerate`),
  saveMixAsPlaylist: (slug, name) =>
    postJSON(`/api/smart-playlists/${encodeURIComponent(slug)}/save-as-playlist`, { name }),

  // Operator-uploaded collection covers. The two scopes are different
  // endpoints rather than one parameterised route, so the mapping lives
  // here in one place instead of at every call site.
  //
  // POST /api/playlists/{id}/cover has existed since the covers work
  // landed and had NO caller: the only UI ever built was the smart-mix
  // half, on the page that is now gone. A playlist could not be given a
  // cover from anywhere in the console.
  uploadCover: (scope, key, dataURL) =>
    postJSON(coverAdminURL(scope, key), { image: dataURL }),
  deleteCover: (scope, key) => deleteJSON(coverAdminURL(scope, key)),
};

/**
 * The admin cover endpoint for a scope, which is NOT the read URL —
 * collectionCoverURL below serves the bytes, this mutates them.
 */
function coverAdminURL(scope, key) {
  const id = encodeURIComponent(key);
  return scope === "smartmix"
    ? `/api/smart-playlists/${id}/cover`
    : `/api/playlists/${id}/cover`;
}

/**
 * POST with a JSON body.
 *
 * Always sends Content-Type: application/json, per the house convention
 * — note csrfGuard does NOT 415 a bodyless POST (it gates the
 * Content-Type check on ContentLength != 0 and lets empty bodies
 * through), so the header is convention here rather than a requirement.
 * It becomes a requirement the moment a body IS sent, which
 * save-as-playlist does.
 */
async function postJSON(url, body) {
  const res = await fetch(url, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify(body ?? {}),
  });
  if (!res.ok) {
    let detail = "";
    try {
      detail = (await res.json())?.message || "";
    } catch { /* a non-JSON error body is not worth a second failure */ }
    throw new Error(detail || `Request failed (${res.status})`);
  }
  return res.status === 204 ? null : res.json();
}

/** DELETE with no body, same error shape as postJSON. */
async function deleteJSON(url) {
  const res = await fetch(url, { method: "DELETE" });
  if (!res.ok) {
    let detail = "";
    try {
      detail = (await res.json())?.message || "";
    } catch { /* a non-JSON error body is not worth a second failure */ }
    throw new Error(detail || `Request failed (${res.status})`);
  }
  return res.status === 204 ? null : res.json();
}

/** Cover URL for a playlist or smart-mix operator upload. */
export function collectionCoverURL(scope, key) {
  return `/api/library/collection-cover/${encodeURIComponent(scope)}/${encodeURIComponent(key)}`;
}

function qs(params = {}) {
  const u = new URLSearchParams();
  for (const [k, v] of Object.entries(params)) {
    if (v !== undefined && v !== null && v !== "") u.set(k, String(v));
  }
  return u.toString();
}

/**
 * Audio/download URLs for one track.
 *
 * encodeURIComponent, NOT URLSearchParams — and this is load-bearing.
 * URLSearchParams serialises to application/x-www-form-urlencoded,
 * which encodes a SPACE as "+". The server reads path parameters
 * through safeQuery, which treats "+" as a LITERAL plus so that a file
 * actually named "A+B.flac" resolves. The two conventions are
 * incompatible: form-encode a path with spaces and every one of them
 * arrives as a plus, and the file is not found.
 *
 * encodeURIComponent emits %20 for a space and %2B for a plus, so both
 * survive. Verified end-to-end against a library containing both.
 */
export function audioURL(path, variantID) {
  const v = variantID ? `&variant=${encodeURIComponent(variantID)}` : "";
  return `/api/player/audio?path=${encodeURIComponent(path)}${v}`;
}

/**
 * A one-playlist export download, in json / csv / m3u8.
 *
 * The OPERATOR endpoint, unchanged: /api/playlists/export already wrote
 * all three formats (including the M3U8 whose every interpolated field
 * is newline-flattened against playlist-line injection), and a second
 * writer under /api/player would be a second place for that to be got
 * wrong. Its `device` parameter is optional now — the read is id-scoped
 * — so nothing but the id is needed here.
 *
 * A URL rather than a fetch: the response is a file download with
 * Content-Disposition, so it has to reach the browser's own download
 * path. The caller assigns location.
 */
export function playlistExportURL(id, format) {
  return `/api/playlists/export?id=${encodeURIComponent(id)}&format=${encodeURIComponent(format)}`;
}

export function downloadURL(path) {
  return `/api/player/download?path=${encodeURIComponent(path)}`;
}

/**
 * Cover URL for an album.
 *
 * Prefers artworkVersion — it is a CONTENT key, so the response is
 * immutable and the browser can cache it for a year. The bare MBID
 * falls back to a one-day cache because a premium refetch can change
 * those bytes under the same id.
 */
export function coverURL(album, size = 500) {
  if (!album || !album.artworkMBID) return null;
  const v = album.artworkVersion ? `&v=${encodeURIComponent(album.artworkVersion)}` : "";
  return `/api/library/artwork/${encodeURIComponent(album.artworkMBID)}?size=${size}${v}`;
}

/**
 * Portrait URL for an artist.
 *
 * `size` is optional and only worth passing for small boxes (the round
 * artist tiles). Omitting it serves the stored file untouched, which is
 * what the full-size detail hero wants.
 *
 * `version` is the portrait's content token (`imageVersion` on the DTO).
 * Portraits live under a fixed `artist-<mbid>` key the enricher
 * overwrites in place, so there is no content key IN the id the way
 * `local-<sha256>` covers have one — the token supplies it, and the
 * server verifies it against the file before answering immutable.
 * Without it the response caps at a day and revalidates on mtime, which
 * measured as 49 conditional requests on one load of /artists.
 *
 * Pass it wherever you have it. Omitting it is correct-but-slow, never
 * wrong.
 */
export function artistImageURL(mbid, size, version) {
  if (!mbid) return null;
  const q = [];
  if (size) q.push(`size=${size}`);
  if (version) q.push(`v=${encodeURIComponent(version)}`);
  const base = `/api/library/artist-image/${encodeURIComponent(mbid)}`;
  return q.length ? `${base}?${q.join("&")}` : base;
}

export function bookletURL(mbid) {
  return mbid ? `/api/library/booklet/${encodeURIComponent(mbid)}` : null;
}

/**
 * Variant generation and deletion, scoped by IDENTITY.
 *
 * The id forms exist because an album is not a folder: its directory is
 * the common ancestor of its tracks and is routinely shared with other
 * albums, so a path-scoped submit would enqueue the neighbours and a
 * path-scoped delete would reclaim their sidecars. Sending the catalog
 * id lets the server expand it against the same snapshot this page was
 * rendered from — and keeps an artist with thousands of tracks to one
 * short id on the wire.
 */
export function generateVariants(scope, kind) {
  return postJSON("/api/upscale/batch", { ...scope, kind });
}

/**
 * Delete is a DELETE with query parameters and no body — the shape the
 * endpoint has always had, and the reason the scope travels as an id
 * rather than a path list here too.
 */
export async function deleteVariants(scope, kind) {
  const params = new URLSearchParams();
  if (scope.albumIds) scope.albumIds.forEach((id) => params.append("albumId", id));
  if (scope.artistId) params.set("artistId", scope.artistId);
  if (scope.path !== undefined) {
    // A folder scope is a prefix. An EMPTY prefix is not "this folder"
    // — it is every variant in the manifest, which the endpoint only
    // performs behind an explicit confirm parameter. Refusing here
    // means a caller cannot reach that by passing a path it forgot to
    // fill in; clearing the whole cache is a deliberate act with its
    // own control, on the Roots page.
    if (!scope.path) throw new Error("Refusing to delete every variant from a folder scope.");
    params.set("prefix", scope.path);
  }
  if (kind) params.set("kind", kind);
  const res = await fetch(`/api/upscale/variants?${params.toString()}`, { method: "DELETE" });
  if (!res.ok) {
    let detail = "";
    try {
      detail = (await res.json())?.message || "";
    } catch { /* a non-JSON error body is not worth a second failure */ }
    throw new Error(detail || `Request failed (${res.status})`);
  }
  return res.json();
}
