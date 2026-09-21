package admin

import (
	"net/http"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// The console's answer to "which of my files are broken".
//
// A source the decoders refuse writes no `track_analysis` row, so before
// migration v46 it was re-offered on every sweep and re-failed identically,
// forever, with nothing anywhere naming it: the field report was 30 truncated
// FLACs producing 1,385 WARN lines in 7 days, and the only way to find them
// was to read the journal. The debounce stops the retries; this pair of
// routes is what turns the stopping into something an operator can act on —
// the list to replace the files from, and the way back once they have.
//
// Both sit behind the console's existing posture (loopback-only in CLI mode,
// session auth in public mode) like every other route in this table. Neither
// is a managed control: this is the operator's OWN library data, the same
// reasoning apiDeletedPlaylistsList records, not an action a control plane
// owns the way a restart or a library-root change is.

// unreadableTrackRow is the admin-wire DTO for one refused source. Distinct
// from manifest.AdminUnreadableTrack per the wire-type rule.
type unreadableTrackRow struct {
	Path string `json:"path"`
	// Reason is the decoder's own message — for the case this exists for,
	// "sox: decoded 51.5s of 357.2s probed — source appears truncated".
	// Already redacted of absolute paths where it was produced
	// (analyze.decodeFramesWith), which is what makes it safe to render on
	// a page that promises library-relative paths.
	Reason string `json:"reason"`
	// Strikes is how many consecutive times this file version has been
	// refused; Suppressed says whether that has crossed the threshold and
	// the bridge has stopped trying. A listed-but-not-suppressed row is a
	// file on its way there, which is worth showing — waiting for the third
	// sweep to mention it would hide a fresh import's breakage for hours.
	Strikes    int  `json:"strikes"`
	Suppressed bool `json:"suppressed"`
	// FirstSeenAt / LastSeenAt are RFC3339, both scoped to THIS file
	// version: replacing the file restarts both. Strings rather than
	// time.Time for the reason deletedPlaylistRow uses them — it keeps the
	// value-time-with-omitempty trap off this DTO entirely.
	FirstSeenAt string `json:"firstSeenAt"`
	LastSeenAt  string `json:"lastSeenAt"`
	SizeBytes   int64  `json:"sizeBytes"`
}

// unreadableTracksResponse heads the list with the threshold, so the page can
// explain "refused N times running" without a second copy of the number
// living in JavaScript.
type unreadableTracksResponse struct {
	Tracks    []unreadableTrackRow `json:"tracks"`
	Threshold int                  `json:"threshold"`
	// Suppressed is how many of Tracks have stopped being retried. Computed
	// from the rows being returned, not a second query — the panel must
	// describe the list it is showing (the #942 rule).
	Suppressed int `json:"suppressed"`
}

type unreadableRetryResponse struct {
	Cleared  int64 `json:"cleared"`
	Swept    bool  `json:"swept"`
	Analysis bool  `json:"analysisActive"`
}

// apiUnreadableTracksList handles GET /api/analysis/unreadable — every track
// carrying a current decode verdict, most-recently-failed first.
//
// Unbounded on purpose, for apiDeletedPlaylistsList's reason: the set is one
// row per file the decoders refused, which is small by construction, and a
// cap would hide exactly the rows a bad bulk import just created — the case
// this list exists for.
func (s *Server) apiUnreadableTracksList(w http.ResponseWriter, r *http.Request) {
	out := unreadableTracksResponse{
		Tracks:    []unreadableTrackRow{},
		Threshold: manifest.AnalysisFailureThreshold(),
	}
	if s.deps.Manifest == nil {
		writeJSON(w, http.StatusOK, out)
		return
	}
	rows, err := s.deps.Manifest.ListUnreadableTracksForAdmin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	out.Tracks = make([]unreadableTrackRow, 0, len(rows))
	for _, t := range rows {
		if t.Suppressed {
			out.Suppressed++
		}
		out.Tracks = append(out.Tracks, unreadableTrackRow{
			Path:        t.Path,
			Reason:      t.Reason,
			Strikes:     t.Strikes,
			Suppressed:  t.Suppressed,
			FirstSeenAt: nsToRFC3339(t.FirstSeenAt),
			LastSeenAt:  nsToRFC3339(t.LastSeenAt),
			SizeBytes:   t.SizeBytes,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// apiUnreadableTracksRetry handles POST /api/analysis/unreadable/retry —
// clear every recorded verdict and ask for a sweep.
//
// Whole-library only, deliberately. The CLI's `--retry-failed` takes
// `--filter` because a scripted run may want a scope; a button labelled
// "Retry all" has one meaning, and offering a per-row retry would invite the
// operator to re-decode one truncated file at a time to watch it fail again.
// Replacing the file needs no button at all — it changes (size, mtime_ns) and
// the version gate re-opens it on the next scan.
//
// The sweep nudge is best-effort and reported rather than required: on a
// bridge with analysis off there is nothing to nudge, and the clear is still
// the right thing to have done — the next enabled sweep or `bridge analyze`
// run picks the sources up. Saying which happened beats a 503 that leaves the
// operator unsure whether the markers went.
func (s *Server) apiUnreadableTracksRetry(w http.ResponseWriter, r *http.Request) {
	if s.deps.Manifest == nil {
		writeError(w, http.StatusServiceUnavailable, "manifest_unavailable",
			"no manifest store on this bridge")
		return
	}
	n, err := s.deps.Manifest.ClearAllAnalysisFailures(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	// The Jobs card's coverage snapshot is TTL-cached for 30 s and carries
	// `unreadableExcluded`, so without this the card goes on subtracting a
	// set the list no longer shows — the panel empty, the line above it still
	// naming N refused tracks and pointing at it. Same invalidate-on-mutation
	// shape as invalidateDatabaseStats after a compaction. (CodeRabbit on #947.)
	s.invalidateAnalysisCoverage()
	out := unreadableRetryResponse{Cleared: n}
	if trigger := s.deps.TriggerAnalysisSweep; trigger != nil {
		out.Analysis = true
		out.Swept = trigger()
	}
	logger.Info("analysis: recorded decode failures cleared by operator",
		"cleared", n, "sweepQueued", out.Swept)
	writeJSON(w, http.StatusOK, out)
}
