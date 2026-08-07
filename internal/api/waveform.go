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

	// Spectrum is the `1BSP` file-provenance curve served by
	// /v1/spectrum, or nil when the row carries none. Bytes rather than a
	// path because it is ~80 bytes and lives on the analysis row itself —
	// there is no sidecar to open.
	Spectrum []byte
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
// **The response is NOT content-addressed.** The only request key is
// `?path=`, so the URL is stable while the body is not: re-analysis
// (an edited source, a schema bump) rewrites the sidecar under the same
// URL. The waveform's content tag exists — iOS learns it from the
// manifest and keys its own disk cache on it — but it is not part of
// the request, so the tag can only be served BACK as an ETag, never
// used to route. Hence `Cache-Control: private, no-cache`: cache it,
// but revalidate, and let the ETag turn the revalidation into a 304.
//
// Deferred: adding `&tag=<waveformTag>` to the request would make the
// URL genuinely content-addressed and re-earn `immutable`. That is a
// wire-shape change (new query parameter documented in PROTOCOL.md +
// the iOS `docs/BridgeProtocol.md` mirror + the client's fetch), so it
// belongs in a Mirror-PR pair, not here.
// analysisSourceMTimeToleranceNS is how far a source's mtime may drift before
// a cached analysis is considered stale — the same 2 s serveVariant uses
// (covers FAT32 / SMB granularity; a real edit jumps far more).
const analysisSourceMTimeToleranceNS int64 = 2_000_000_000

// lookupAnalysisForRequest performs the request handling /v1/waveform and
// /v1/spectrum share: feature gate, `?path=` presence, the traversal-guarded
// resolve whose canonical `os.FileInfo` the freshness check needs, and the row
// lookup.
//
// Shared rather than copied because the two endpoints describe the SAME source
// file — they must agree on which row a path resolves to and on what "the
// source drifted" means, and two copies of that is a divergence waiting to
// happen. `kind` supplies the error-code prefix ("waveform" / "spectrum") so
// each endpoint keeps its own vocabulary.
//
// Returns (nil, nil, false) when it has already written a response. The
// PAYLOAD check stays with the caller: what counts as "present" differs (a
// sidecar path vs. bytes on the row), and it is deliberately made BEFORE the
// freshness gate so a row with no payload reads 404 rather than 410.
func (s *Server) lookupAnalysisForRequest(w http.ResponseWriter, r *http.Request, kind string) (*AnalysisRecord, os.FileInfo, bool) {
	if s.analysisStore == nil {
		writeError(w, http.StatusNotFound, kind+"_not_found", "audio analysis is not enabled on this bridge")
		return nil, nil, false
	}
	clientPath := safeQuery(r).Get("path")
	if clientPath == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "missing path parameter")
		return nil, nil, false
	}
	// Validate the SOURCE path (traversal guard + the canonical stat the
	// freshness check uses). Neither payload is at a user-controlled path.
	_, info, err := s.resolver.ResolveChecked(clientPath)
	if ok := writeResolveError(w, r, err); ok {
		return nil, nil, false
	}
	if info.IsDir() {
		writeError(w, http.StatusBadRequest, "bad_request", "path is a directory")
		return nil, nil, false
	}
	rec, err := s.analysisStore.LookupAnalysis(r.Context(), clientPath)
	if err != nil {
		writeErrorLog(w, r, http.StatusInternalServerError, "internal",
			"the bridge couldn't look up this "+kind, err)
		return nil, nil, false
	}
	return rec, info, true
}

// analysisSourceDrifted reports whether the source has changed since the
// analysis was computed. A cached measurement of different bytes is worse than
// none — it is evidence about a file that no longer exists.
func analysisSourceDrifted(rec *AnalysisRecord, info os.FileInfo) bool {
	delta := rec.SourceMTimeNS - info.ModTime().UnixNano()
	if delta < 0 {
		delta = -delta
	}
	return delta > analysisSourceMTimeToleranceNS || rec.SourceSize != info.Size()
}

func (s *Server) waveform(w http.ResponseWriter, r *http.Request) {
	rec, info, ok := s.lookupAnalysisForRequest(w, r, "waveform")
	if !ok {
		return
	}
	if rec == nil || rec.WaveformPath == "" {
		writeError(w, http.StatusNotFound, "waveform_not_found", "no waveform for this track yet")
		return
	}
	if analysisSourceDrifted(rec, info) {
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
		// `no-cache` means "store it, but revalidate before reuse" —
		// NOT "don't store". Paired with the ETag, a client that
		// already holds this waveform pays one conditional request and
		// gets a 304 (http.ServeContent handles If-None-Match below).
		//
		// It must not be `immutable`: nothing in the URL identifies the
		// body (see the handler docblock), and a conforming client
		// never revalidates an immutable response — so a re-analysed
		// waveform would be pinned stale for the full max-age, and the
		// ETag one line above would be dead code.
		w.Header().Set("ETag", `"`+rec.WaveformTag+`"`)
		w.Header().Set("Cache-Control", "private, no-cache")
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
