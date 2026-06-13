package api

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"os"
)

// AnalysisStore is the optional interface GET /v1/waveform uses to look
// up a track's waveform sidecar metadata. Nil-safe — when
// `s.analysisStore` is nil (analysis feature off / not wired) the
// handler returns 404, which iOS treats identically to a pre-feature
// bridge that doesn't register the route at all.
//
// **Freshness happens in the api**, not here — same rationale as
// VariantStore: the api owns the canonical `bridgefs.Resolver` stat and
// asks the store only for "do you have a row, where's the sidecar,
// what's its recorded source provenance". internal/manifest.Provider
// satisfies this via a thin LookupAnalysis wrapper at the cmd/bridge
// wiring point.
type AnalysisStore interface {
	LookupAnalysis(ctx context.Context, sourcePath string) (*AnalysisRecord, error)
}

// AnalysisRecord is the minimum metadata the waveform handler needs:
// where the sidecar lives, its content tag (served as the ETag / used
// by iOS as the immutable-cache key), and the source freshness fields.
// SourcePath is the CANONICAL row value (case-preserved); the lookup
// resolves case-insensitively.
type AnalysisRecord struct {
	SourcePath    string
	WaveformPath  string
	WaveformTag   string
	SourceMTimeNS int64
	SourceSize    int64
}

// waveform: GET /v1/waveform?path=<rel>
//
// Serves the offline-computed peak-envelope sidecar for a track so iOS
// can render a scrubber waveform. The source path is validated through
// the same `bridgefs.Resolver` traversal guard as /v1/download (so a
// malformed `?path=` is rejected before any lookup) AND its canonical
// `os.FileInfo` drives the freshness check. A drifted source yields 410
// (iOS drops the waveform until re-analysis catches up); a genuinely
// missing sidecar also yields 410 vs a permission/I-O fault's 5xx.
//
// The response is content-addressed by tag: iOS fetches with the tag it
// learned from the manifest, and a regenerated waveform gets a new tag,
// so `Cache-Control: immutable` is safe and the client downloads each
// distinct waveform exactly once.
func (s *Server) waveform(w http.ResponseWriter, r *http.Request) {
	if s.analysisStore == nil {
		writeError(w, http.StatusNotFound, "waveform_not_found", "audio analysis is not enabled on this bridge")
		return
	}
	q := safeQuery(r)
	clientPath := q.Get("path")
	if clientPath == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "missing path parameter")
		return
	}

	// Validate the SOURCE path (traversal guard + canonical stat the
	// freshness check uses). The sidecar itself lives under the
	// bridge's own data dir, not a user-controlled path.
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
			"the bridge couldn't look up this waveform", err)
		return
	}
	if rec == nil || rec.WaveformPath == "" {
		writeError(w, http.StatusNotFound, "waveform_not_found", "no waveform for this track yet")
		return
	}

	// Freshness gate — same mtime tolerance as serveVariant (2 s covers
	// FAT32 / SMB granularity; real edits jump far more).
	const mtimeToleranceNS int64 = 2_000_000_000
	mtimeDelta := rec.SourceMTimeNS - info.ModTime().UnixNano()
	if mtimeDelta < 0 {
		mtimeDelta = -mtimeDelta
	}
	if mtimeDelta > mtimeToleranceNS || rec.SourceSize != info.Size() {
		writeError(w, http.StatusGone, "waveform_stale",
			"waveform is out of date relative to source")
		return
	}

	f, err := os.Open(rec.WaveformPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// Sidecar gone (manual wipe, partial --gc) — 410 so iOS
			// drops it; the next `bridge analyze` regenerates.
			writeError(w, http.StatusGone, "waveform_missing_on_disk",
				"waveform sidecar is missing on disk")
			return
		}
		writeErrorLog(w, r, http.StatusInternalServerError, "internal",
			"the bridge couldn't open this waveform", err)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		writeErrorLog(w, r, http.StatusInternalServerError, "internal",
			"the bridge couldn't stat this waveform", err)
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	if rec.WaveformTag != "" {
		w.Header().Set("ETag", `"`+rec.WaveformTag+`"`)
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	}
	http.ServeContent(w, r, fi.Name(), fi.ModTime(), f)
}

// AnalysisStatsProvider is the interface GET /v1/analysis/stats reads.
// Mirrors UpscaleStatsProvider. Nil-safe: when unwired the handler
// returns the zero-value AnalysisStats (`enabled=false`), which iOS
// renders as "feature off".
type AnalysisStatsProvider interface {
	AnalysisStatsSnapshot(ctx context.Context) (AnalysisStats, error)
}

// AnalysisStats is the wire shape GET /v1/analysis/stats returns —
// field-for-field compatible with the admin tile, same as UpscaleStats.
//   - Enabled mirrors LIVE runtime state (pool != nil), not the
//     persisted config flag.
//   - Pool is omitted when the feature is off.
//   - SoxAvailable is omitted when no precheck closure was wired.
type AnalysisStats struct {
	Enabled         bool               `json:"enabled"`
	SoxAvailable    *bool              `json:"soxAvailable,omitempty"`
	Pool            *AnalysisPoolStats `json:"pool,omitempty"`
	CachedWaveforms int                `json:"cachedWaveforms"`
	CachedBytes     int64              `json:"cachedBytes"`
}

// AnalysisPoolStats mirrors `analyze.PoolStats` field-for-field but
// lives here so the api package compiles without importing
// internal/analyze. The wiring closure in cmd/bridge translates.
type AnalysisPoolStats struct {
	Workers  int    `json:"workers"`
	QueueCap int    `json:"queueCap"`
	QueueLen int    `json:"queueLen"`
	Inflight int    `json:"inflight"`
	Enqueued uint64 `json:"enqueued"`
	Done     uint64 `json:"done"`
	Failed   uint64 `json:"failed"`
}

// analysisStats: GET /v1/analysis/stats — authenticated read-only
// snapshot of the analysis feature's runtime + on-disk state. Cheap
// (single SQL COUNT + a mutex-protected pool snapshot + a TTL-cached
// sox precheck). Mirrors upscaleStats.
func (s *Server) analysisStats(w http.ResponseWriter, r *http.Request) {
	var resp AnalysisStats
	if s.analysisStatsProvider != nil {
		snap, err := s.analysisStatsProvider.AnalysisStatsSnapshot(r.Context())
		if err != nil {
			writeErrorLog(w, r, http.StatusServiceUnavailable, "stats_unavailable",
				"analysis stats are temporarily unavailable", err)
			return
		}
		resp = snap
	}
	writeJSON(w, http.StatusOK, resp)
}
