package dlna

import (
	"net/http"
	"strings"
)

// ArtworkPathPrefix is the URL prefix the artwork handler is mounted under
// — `/dlna/artwork/{key}`, beside FilePathPrefix. Trailing slash so the key
// is the single path component after it.
const ArtworkPathPrefix = "/dlna/artwork/"

// ArtworkSource serves one cached release cover by its `/v1/artwork/{key}`
// key — the MusicBrainz UUID, the scanner's `local-<sha256>`, or the 16-hex
// `artworkVersion` alias — writing the same bytes, headers and miss shapes
// the bearer-authed v1 route writes. The api server implements it
// (`api.(*Server).ServeArtwork`); this package never imports api, so the
// wiring layer hands the implementation in through ServerConfig.Artwork, the
// same way it hands in the UPnP proxy.
//
// The key is the SAME key the iOS client stores as an album's `artworkHash`
// (`artworkVersion ?? artworkMBID`), so an app emitting `albumArtURI` for a
// renderer composes `<base>` + ArtworkPathPrefix + that key and lands on the
// bytes it already caches from `/v1/artwork/{key}`.
type ArtworkSource interface {
	ServeArtwork(w http.ResponseWriter, r *http.Request, key string)
}

// ArtworkHandler returns the handler mounted at ArtworkPathPrefix. It
// carries `/dlna/file/`'s exact posture and nothing more:
//
//   - **Unauthenticated on the LAN by design.** Renderers and control points
//     cannot speak the bearer scheme; the LAN-only bind is the gate — and the
//     listener never starts in public deployment mode (ShouldEnableDLNA), so
//     the demo bridge and every public bridge have no such route at all.
//   - **Opaque, non-enumerable key.** A cover is addressable only by a key a
//     caller already holds from the CDS (or, for the app, from the manifest).
//     No listing, no wildcard, no path walk: a nested path or an empty key is
//     a plain 404, and the key grammar itself is enforced by the source (400
//     on anything that is not a UUID / local-hash / alias).
//   - **Read-only, GET/HEAD only.** Anything else is 405. HEAD is answered
//     by the source's http.ServeContent without a body — libavformat-style
//     probes test reachability that way before the GET.
//
// The bytes, the size ladder, the 202 `pending` / 404 `no_image` /
// 404 `not_found` split and the cache headers are the source's, not this
// wrapper's: extract, don't fork.
func ArtworkHandler(src ArtworkSource) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "GET or HEAD only", http.StatusMethodNotAllowed)
			return
		}
		key, ok := artworkKeyFromPath(r.URL.Path)
		if !ok {
			http.NotFound(w, r)
			return
		}
		src.ServeArtwork(w, r, key)
	}
}

// artworkKeyFromPath extracts the single key segment after
// ArtworkPathPrefix. Unlike extractTrackID it does NOT take the first
// segment of a longer path: a key with anything after it is not a key this
// route knows, and answering the truncated form would make two URLs name one
// cover.
func artworkKeyFromPath(urlPath string) (string, bool) {
	if !strings.HasPrefix(urlPath, ArtworkPathPrefix) {
		return "", false
	}
	key := urlPath[len(ArtworkPathPrefix):]
	if key == "" || strings.Contains(key, "/") {
		return "", false
	}
	return key, true
}

// ArtworkURLFor composes the absolute `<upnp:albumArtURI>` for a key against
// the per-request server URL (scheme + host + port, no trailing slash) the
// CDS already uses for `<res>` — so the cover URL is reachable from whichever
// interface the control point used to reach us, like the file URL beside it.
// Empty key → empty URL, so callers can assign the result unconditionally.
func ArtworkURLFor(serverURL, key string) string {
	if key == "" {
		return ""
	}
	return serverURL + ArtworkPathPrefix + key
}
