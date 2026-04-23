package api

import (
	"net/http"
	"os"
	"regexp"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/enrich"
)

// ArtworkDirProvider is the minimal interface api needs to serve cached
// artwork. Implemented by cmd/bridge's serveCmd (via the Enricher's
// CacheDir). Split out so api tests don't have to import internal/enrich.
type ArtworkDirProvider interface {
	ArtworkCacheDir() string
}

// mbidPattern validates that a path segment looks like a MusicBrainz
// UUID. Prevents traversal and filesystem abuse through the {mbid}
// parameter.
var mbidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

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
	if !mbidPattern.MatchString(mbid) {
		writeError(w, http.StatusBadRequest, "bad_request",
			"mbid must be a MusicBrainz UUID")
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
			writeError(w, http.StatusNotFound, "not_found",
				"artwork not cached (enricher may not have reached this album yet)")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	http.ServeContent(w, r, info.Name(), info.ModTime(), f)
	_ = time.Duration(0) // silence unused-import in future edits
}
