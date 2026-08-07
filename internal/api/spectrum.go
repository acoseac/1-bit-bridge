package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"time"
)

// GET /v1/spectrum?path=… — the whole-track frequency spectrum measured by
// `bridge analyze`, as the ~80-byte `1BSP` curve.
//
// A `boundedRoute` sibling of /v1/waveform, and deliberately shaped like it:
// same auth, same path validation, same source-freshness gate, same
// ETag + `no-cache` revalidation. The one structural difference is that there
// is no sidecar to open — the curve is small enough to live on the
// `track_analysis` row, so this serves bytes already in hand.
//
// **A 404 here does NOT mean "this file has no content up there".** It means
// the bridge has no measurement: analysis hasn't reached the track, the track
// was too short to average, or the bridge predates the feature. The
// distinction matters because the client renders this as evidence about a
// file's provenance, and "no data" must never render as "no bandwidth".
func (s *Server) spectrum(w http.ResponseWriter, r *http.Request) {
	if s.analysisStore == nil {
		writeError(w, http.StatusNotFound, "spectrum_not_found",
			"audio analysis is not enabled on this bridge")
		return
	}
	q := safeQuery(r)
	clientPath := q.Get("path")
	if clientPath == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "missing path parameter")
		return
	}

	// Validate the SOURCE path (traversal guard + the canonical stat the
	// freshness check needs). The curve itself never touches the filesystem.
	_, info, err := s.resolver.ResolveChecked(clientPath)
	if ok := writeResolveError(w, r, err); ok {
		return
	}
	if info.IsDir() {
		writeError(w, http.StatusBadRequest, "bad_request", "path is a directory")
		return
	}

	rec, err := s.analysisStore.LookupAnalysis(r.Context(), clientPath)
	if err != nil {
		writeErrorLog(w, r, http.StatusInternalServerError, "internal",
			"the bridge couldn't look up this spectrum", err)
		return
	}
	if rec == nil || len(rec.Spectrum) == 0 {
		writeError(w, http.StatusNotFound, "spectrum_not_found",
			"no spectrum for this track yet")
		return
	}

	// Freshness gate — identical tolerance to /v1/waveform (2 s covers
	// FAT32 / SMB granularity; a real edit jumps far more). A spectrum
	// measured from different bytes is worse than no spectrum: it is
	// evidence about a file that no longer exists.
	const mtimeToleranceNS int64 = 2_000_000_000
	mtimeDelta := rec.SourceMTimeNS - info.ModTime().UnixNano()
	if mtimeDelta < 0 {
		mtimeDelta = -mtimeDelta
	}
	if mtimeDelta > mtimeToleranceNS || rec.SourceSize != info.Size() {
		writeError(w, http.StatusGone, "spectrum_stale",
			"spectrum is out of date relative to source")
		return
	}

	// ETag over the CURVE's own bytes rather than the waveform tag. The two
	// are produced by one analysis today, so either would revalidate
	// correctly — but the waveform tag describes different bytes, and a
	// future change that regenerates one without the other would silently
	// serve a stale curve behind a fresh-looking tag.
	sum := sha256.Sum256(rec.Spectrum)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("ETag", `"`+hex.EncodeToString(sum[:8])+`"`)
	// `no-cache` is "store it, but revalidate before reuse" — NOT "don't
	// store". With the ETag, a client that already holds this curve pays one
	// conditional request and gets a 304.
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeContent(w, r, "", time.Unix(0, rec.SourceMTimeNS), bytes.NewReader(rec.Spectrum))
}
