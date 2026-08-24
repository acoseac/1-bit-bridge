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
  favorites: () => getJSON("/api/favorites", { key: "favorites" }),

  // Mix actions reuse the operator endpoints unchanged — no new backend.
  regenerateMix: (slug) =>
    postJSON(`/api/smart-playlists/${encodeURIComponent(slug)}/regenerate`),
  saveMixAsPlaylist: (slug, name) =>
    postJSON(`/api/smart-playlists/${encodeURIComponent(slug)}/save-as-playlist`, { name }),
};

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
 * what the full-size detail hero wants. Unlike a cover there is no
 * content key to hang a year-long cache on — portraits live under a
 * fixed `artist-<mbid>` key the enricher overwrites in place — so the
 * server caps these at a day and revalidates on mtime.
 */
export function artistImageURL(mbid, size) {
  if (!mbid) return null;
  const base = `/api/library/artist-image/${encodeURIComponent(mbid)}`;
  return size ? `${base}?size=${size}` : base;
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
