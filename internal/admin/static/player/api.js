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
  browse: (path, cursor) =>
    getJSON(`/api/library/browse?path=${encodeURIComponent(path || "")}` +
      (cursor ? `&afterFolder=${encodeURIComponent(cursor)}` : ""), { key: "browse" }),
  playlists: () => getJSON("/api/playlists", { key: "playlists" }),
  playlistDetail: (device, id) =>
    getJSON(`/api/playlists/detail?device=${encodeURIComponent(device)}&id=${encodeURIComponent(id)}`,
      { key: "detail" }),
  favorites: () => getJSON("/api/favorites", { key: "favorites" }),
  mixes: () => getJSON("/api/smart-playlists", { key: "mixes" }),
};

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

export function artistImageURL(mbid) {
  return mbid ? `/api/library/artist-image/${encodeURIComponent(mbid)}` : null;
}

export function bookletURL(mbid) {
  return mbid ? `/api/library/booklet/${encodeURIComponent(mbid)}` : null;
}
