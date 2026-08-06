package api

import (
	"net/http"
	"os"
	"regexp"
	"strconv"

	"github.com/acoseac/1-bit-bridge/internal/enrich"
)

// artworkPendingRetryAfterSeconds is the value the handlers set on
// `Retry-After` when an MBID is known to the server but the cache
// file isn't on disk yet (202 response). 30s matches iOS's default
// base backoff; a longer value would starve the retry loop, a shorter
// one would hammer an enricher that's already rate-limited by MB /
// CAA / Deezer.
const artworkPendingRetryAfterSeconds = 30

// errMsgInternalError is the opaque user-facing error string returned
// from every 500 path in artwork. Real diagnostic context lands in the
// server-side log via writeErrorLog; the public response stays generic
// so a malicious caller can't probe filesystem layout.
const errMsgInternalError = "internal error"

// ArtworkDirProvider is the minimal interface api needs to serve cached
// artwork. Implemented by cmd/bridge's serveCmd (via the Enricher's
// CacheDir). Split out so api tests don't have to import internal/enrich.
type ArtworkDirProvider interface {
	ArtworkCacheDir() string
}

// mbidPattern validates that a path segment looks like a MusicBrainz
// UUID. Prevents traversal and filesystem abuse through the {mbid}
// parameter. Used by the artist-image handler, which only accepts
// MusicBrainz-derived MBIDs.
var mbidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// artworkMBIDPattern validates the {mbid} segment for /v1/artwork/{mbid}.
// Accepts either a MusicBrainz UUID (set by the enricher after a
// successful Cover Art Archive fetch) OR a local-<sha256> sentinel set
// by the scanner when it found embedded ID3 APIC art or a folder-level
// cover.jpg / folder.jpg next to the audio file. The local- branch is
// lowercase-only to match `hex.EncodeToString` output deterministically.
// Same traversal protection as the strict UUID pattern: the alphabet
// is bounded to [a-z0-9-] so no path-segment escape is possible.
var artworkMBIDPattern = regexp.MustCompile(`^([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}|local-[0-9a-f]{64})$`)

// artistImage handles GET /v1/artist-image/{mbid}.
//
// Serves the Deezer-sourced artist portrait the enricher cached under
// <artworkCacheDir>/artist-<mbid>.jpg. Same MBID validation + 404 / 400
// semantics as /v1/artwork.
func (s *Server) artistImage(w http.ResponseWriter, r *http.Request) {
	if s.artworkDirs == nil {
		writeError(w, http.StatusServiceUnavailable, "scan_in_progress",
			"artist-image service not ready")
		return
	}
	mbid := r.PathValue("mbid")
	if !mbidPattern.MatchString(mbid) {
		writeError(w, http.StatusBadRequest, "bad_request",
			"mbid must be a MusicBrainz UUID")
		return
	}
	path := enrich.ArtistImagePath(s.artworkDirs.ArtworkCacheDir(), mbid)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			// Three-way miss split (see MBIDProbe): 202 only while
			// enrichment is genuinely pending — the cold-cache first
			// scan where the portrait may land any moment. Once every
			// track carrying the MBID is enriched and there's still no
			// file, no image exists upstream (Deezer has nothing for
			// this artist): answer a TERMINAL 404 so clients stop
			// asking. Pre-fix this state answered 202 forever and iOS
			// rode a multi-minute retry ladder per imageless artist on
			// every coverage sweep.
			if s.mbidProbe != nil && s.mbidProbe.HasTrackWithArtistMBID(r.Context(), mbid) {
				if s.mbidProbe.ArtistMBIDEnrichmentPending(r.Context(), mbid) {
					w.Header().Set("Retry-After", strconv.Itoa(artworkPendingRetryAfterSeconds))
					writeError(w, http.StatusAccepted, "pending",
						"artist image enrichment pending; retry after the Retry-After window")
					return
				}
				writeError(w, http.StatusNotFound, "no_image",
					"enrichment complete; no artist image available for this MBID")
				return
			}
			writeError(w, http.StatusNotFound, "not_found",
				"artist image not cached (unknown MBID)")
			return
		}
		logger.Error("open artist image", "mbid", mbid, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", errMsgInternalError)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		logger.Error("stat artist image", "mbid", mbid, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", errMsgInternalError)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	http.ServeContent(w, r, info.Name(), info.ModTime(), f)
}

// artwork handles GET /v1/artwork/{mbid}?size=500.
//
// Serves the pre-cached JPEG the enricher fetched from Cover Art Archive.
// A miss returns 404 rather than falling through to CAA directly — we
// don't want to expose our server as a proxy and a miss simply means
// "enrichment hasn't caught up yet, try again later".
func (s *Server) artwork(w http.ResponseWriter, r *http.Request) {
	if s.artworkDirs == nil {
		writeError(w, http.StatusServiceUnavailable, "scan_in_progress",
			"artwork service not ready")
		return
	}
	mbid := r.PathValue("mbid")
	if !artworkMBIDPattern.MatchString(mbid) {
		writeError(w, http.StatusBadRequest, "bad_request",
			"mbid must be a MusicBrainz UUID or local-<sha256> hash")
		return
	}
	size, err := enrich.ParseSize(r.URL.Query().Get("size"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	path := enrich.ArtworkCachePath(s.artworkDirs.ArtworkCacheDir(), mbid, size)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			// See artistImage handler for the full rationale — the
			// same three-way split: 202 while enrichment is pending,
			// terminal 404 `no_image` once enrichment completed with
			// nothing cached (CAA/iTunes had no cover), plain 404 for
			// an MBID no track references.
			if s.mbidProbe != nil && s.mbidProbe.HasTrackWithArtworkMBID(r.Context(), mbid) {
				if s.mbidProbe.ArtworkMBIDEnrichmentPending(r.Context(), mbid) {
					w.Header().Set("Retry-After", strconv.Itoa(artworkPendingRetryAfterSeconds))
					writeError(w, http.StatusAccepted, "pending",
						"artwork enrichment pending; retry after the Retry-After window")
					return
				}
				writeError(w, http.StatusNotFound, "no_image",
					"enrichment complete; no artwork available for this MBID")
				return
			}
			writeError(w, http.StatusNotFound, "not_found",
				"artwork not cached (unknown MBID)")
			return
		}
		logger.Error("open release artwork", "mbid", mbid, "size", size, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", errMsgInternalError)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		logger.Error("stat release artwork", "mbid", mbid, "size", size, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", errMsgInternalError)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	http.ServeContent(w, r, info.Name(), info.ModTime(), f)
}
