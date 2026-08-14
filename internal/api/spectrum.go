package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"time"
)

// GET /v1/spectrum?path=… — the whole-track frequency spectrum measured by
// `bridge analyze`, as the 84-byte `1BSP` curve.
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
	// Feature gate, path validation and the row lookup are shared with
	// /v1/waveform — the two describe the same source file, so they must
	// agree on which row a path resolves to and on what "drifted" means.
	rec, info, ok := s.lookupAnalysisForRequest(w, r, "spectrum")
	if !ok {
		return
	}
	if rec == nil || len(rec.Spectrum) == 0 {
		writeError(w, http.StatusNotFound, "spectrum_not_found",
			"no spectrum for this track yet")
		return
	}
	// A curve measured from different bytes is worse than no curve: it is
	// evidence about a file that no longer exists.
	if analysisSourceDrifted(rec, info) {
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
