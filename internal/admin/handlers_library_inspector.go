// Library Inspector admin endpoints (v1.3 operator-driven upscale).
// The admin web UI's "Library Inspector" page consumes these:
//
//   - GET /api/library/browse                (PR 2 — folder tree + rollup)
//   - GET /api/library/browse-projection     (PR 2 — pre-flight)
//   - POST /api/upscale/batch                (PR 4 — trigger a batch)
//   - GET /api/upscale/batches               (PR 4 — Jobs page list)
//   - DELETE /api/upscale/batches/{id}       (PR 4 — Cancel)
//   - PATCH /api/upscale/target              (PR 4 — Settings target picker)
//
// All loopback-only (enforced upstream at the listener layer). The
// Submit / Cancel / list handlers proxy through to the
// `AdminBatchCoordinator` deps closure so the admin package stays
// decoupled from internal/transcode.

package admin

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// Shared upscale-disabled error pair surfaced by every admin handler that
// gates on `deps.Coordinator.UpscaleEnabled() == false`. Three call sites
// inside handlers_library_inspector.go + two in handlers_upscale_delete.go;
// SonarCloud go:S1192 flagged the duplicates.
const (
	errCodeUpscaleDisabled    = "upscale-disabled"
	errMsgUpscalingNotEnabled = "upscaling is not enabled on this bridge"
)

// adminBatchSubmitRequest is the JSON shape POST /api/upscale/batch
// accepts. Optional `targetRate` / `targetBits` fall back to the
// scan_state-stored admin Settings.
//
// `Kind` is one of "upscale" (default when omitted, back-compat) or
// "optimize" (v1.x CarPlay-optimized variants — family-preserving
// 16/44.1 or 16/48 FLAC, target params auto-derived per-track). The
// admin Library Inspector's tile-level multi-select batch UI emits
// one POST per kind per selected folder.
type adminBatchSubmitRequest struct {
	Path       string `json:"path"`
	Kind       string `json:"kind,omitempty"`
	TargetRate int    `json:"targetRate,omitempty"`
	TargetBits int    `json:"targetBits,omitempty"`
}

// apiUpscaleBatchSubmit handles POST /api/upscale/batch.
func (s *Server) apiUpscaleBatchSubmit(w http.ResponseWriter, r *http.Request) {
	if s.deps.BatchCoordinator == nil {
		writeError(w, http.StatusServiceUnavailable, errCodeUpscaleDisabled,
			errMsgUpscalingNotEnabled)
		return
	}
	defer r.Body.Close()
	var req adminBatchSubmitRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, adminMaxBodyBytes)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad-json", err.Error())
		return
	}
	// Plumb the request context so an operator's browser
	// disconnect cancels the underlying Coordinator.Submit (which
	// in turn cancels its manifest projection walk).
	// Per Gemini high on PR #202.
	kind := strings.ToLower(strings.TrimSpace(req.Kind))
	// Normalise before the coordinator sees it — this handler used to
	// forward req.Path VERBATIM, unlike every read-side endpoint. The
	// store's prefix helpers treat a prefix that trims to empty as
	// whole-library, so `{"path": "//"}` enqueued the ENTIRE library
	// while the rollup card rendered beside it showed 0.
	//
	// normaliseBrowsePath is the same helper the projection endpoint
	// uses: it strips ALL leading slashes (not just one) and already
	// maps path.Clean's "." result back to "" — hand-rolling that here
	// is how the `Clean("") == "."` trap gets reintroduced.
	normalised, ok := normaliseBrowsePath(req.Path)
	if !ok {
		writeError(w, http.StatusBadRequest, "bad-path",
			"path must be a clean library-relative path (no traversal, no backslashes)")
		return
	}
	var (
		res AdminBatchSubmitResult
		err error
	)
	switch kind {
	case "", "upscale":
		res, err = s.deps.BatchCoordinator.Submit(r.Context(), normalised, req.TargetRate, req.TargetBits)
	case "optimize":
		// Optimize ignores caller-supplied targetRate/targetBits —
		// the coordinator auto-derives per-track via
		// TargetRateForOptimize (family-preserving 16/44.1 or 16/48).
		res, err = s.deps.BatchCoordinator.SubmitOptimize(r.Context(), normalised)
	default:
		writeError(w, http.StatusBadRequest, "invalid-kind",
			`unknown kind: `+req.Kind+` (expected "upscale" or "optimize")`)
		return
	}
	if err != nil {
		var dskErr *AdminBatchInsufficientDiskSpace
		if errors.As(err, &dskErr) {
			// 507 Insufficient Storage — well-defined HTTP semantics
			// for "the request would consume more disk than is
			// available." JSON body carries the numbers so the UI
			// can render the projection without re-running it.
			writeJSON(w, http.StatusInsufficientStorage, map[string]any{
				"error":          "insufficient_disk_space",
				"projectedBytes": dskErr.ProjectedBytes,
				"requiredBytes":  dskErr.RequiredBytes,
				"availableBytes": dskErr.AvailableBytes,
			})
			return
		}
		writeError(w, http.StatusInternalServerError, "submit-failed", err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, res)
}

// apiUpscaleBatchList handles GET /api/upscale/batches?limit=N.
func (s *Server) apiUpscaleBatchList(w http.ResponseWriter, r *http.Request) {
	if s.deps.BatchCoordinator == nil {
		writeError(w, http.StatusServiceUnavailable, errCodeUpscaleDisabled,
			errMsgUpscalingNotEnabled)
		return
	}
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}
	rows, err := s.deps.BatchCoordinator.ListBatches(limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list-failed", err.Error())
		return
	}
	tp := s.deps.BatchCoordinator.Throughput()
	writeJSON(w, http.StatusOK, map[string]any{
		"batches":    rows,
		"throughput": tp,
	})
}

// apiUpscaleBatchCancel handles DELETE /api/upscale/batches/{id}.
func (s *Server) apiUpscaleBatchCancel(w http.ResponseWriter, r *http.Request) {
	if s.deps.BatchCoordinator == nil {
		writeError(w, http.StatusServiceUnavailable, errCodeUpscaleDisabled,
			errMsgUpscalingNotEnabled)
		return
	}
	idStr := r.PathValue("id")
	if idStr == "" {
		writeError(w, http.StatusBadRequest, "bad-request", "missing batch id")
		return
	}
	if err := s.deps.BatchCoordinator.Cancel(idStr); err != nil {
		writeError(w, http.StatusInternalServerError, "cancel-failed", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// adminUpscaleTargetRequest is the JSON shape PATCH
// /api/upscale/target accepts.
type adminUpscaleTargetRequest struct {
	TargetRate int `json:"targetRate"`
	TargetBits int `json:"targetBits"`
}

// apiUpscaleTargetGet handles GET /api/upscale/target.
func (s *Server) apiUpscaleTargetGet(w http.ResponseWriter, r *http.Request) {
	cfg := s.deps.CfgHolder.Load()
	if s.deps.Manifest == nil {
		writeError(w, http.StatusServiceUnavailable, "no-manifest",
			"manifest store is not configured")
		return
	}
	rate, bits, err := s.deps.Manifest.GetUpscaleTarget(r.Context())
	if err != nil {
		// `ErrUpscaleTargetUnset` is the legitimate "first run /
		// pre-seed" case → surface YAML bootstrap defaults so the
		// operator's view matches what `Coordinator.Submit` will
		// use. Anything else (corrupt DB row, parse failure,
		// SQLite I/O error) MUST surface as 500 so a real problem
		// doesn't masquerade as default state. Per CodeRabbit
		// major on PR #205 round 2.
		if !errors.Is(err, manifest.ErrUpscaleTargetUnset) {
			writeError(w, http.StatusInternalServerError, "read-target", err.Error())
			return
		}
		rate = cfg.Upscale.EffectiveBootstrapTargetRate()
		bits = cfg.Upscale.EffectiveBootstrapTargetBits()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"targetRate": rate,
		"targetBits": bits,
	})
}

// apiUpscaleTargetPatch handles PATCH /api/upscale/target.
func (s *Server) apiUpscaleTargetPatch(w http.ResponseWriter, r *http.Request) {
	if s.deps.Manifest == nil {
		writeError(w, http.StatusServiceUnavailable, "no-manifest",
			"manifest store is not configured")
		return
	}
	defer r.Body.Close()
	var req adminUpscaleTargetRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, adminMaxBodyBytes)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad-json", err.Error())
		return
	}
	if err := s.deps.Manifest.SetUpscaleTarget(r.Context(), req.TargetRate, req.TargetBits); err != nil {
		writeError(w, http.StatusBadRequest, "bad-target", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"targetRate": req.TargetRate,
		"targetBits": req.TargetBits,
	})
}

// pageLibraryInspector renders the Library Inspector HTML.
//
// `UpscaleStoragePath` is the absolute filesystem directory the
// long-lived transcode pool writes converted sidecars to —
// surfaced in the drawer alongside the "Free on data volume"
// row so the operator can see WHERE the projected variants will
// land before they hit "Upscale this scope." Always populated
// regardless of whether the pool itself is running; mirrors
// `/api/upscale/stats.storagePath`.
//
// `SoxAvailable` reflects whether SoX is on PATH (via the wired
// `UpscalePrecheck` closure). The tile-redesign template uses this
// to mark per-tile "Generate variants" buttons disabled with an
// inline "Install SoX" tooltip when false. Delete buttons stay
// enabled regardless — the operator can reclaim space on a
// SoX-less box. Nil-safe: when the closure is absent (test
// harness, upscale feature off at boot), defaults to false so
// the UI assumes "no variant generation" — strictly safer than
// assuming yes.
func (s *Server) pageLibraryInspector(w http.ResponseWriter, r *http.Request) {
	cfg := s.deps.CfgHolder.Load()
	soxAvailable := false
	if s.deps.UpscalePrecheck != nil {
		soxAvailable = s.deps.UpscalePrecheck() == nil
	}
	s.renderPage(w, "library_inspector", map[string]any{
		"UpscaleStoragePath": cfg.Upscale.EffectiveVariantsDir(cfg.DataDir),
		"SoxAvailable":       soxAvailable,
		// Atlas metadata layer gates: AtlasEnabled shows the About
		// panel card (bios / descriptions); BookletsEnabled adds the
		// booklet rows/chips (requires the harvest wiring — mirrors
		// the /v1/health `booklets` flag condition via Deps.BookletPath).
		// Tile artwork is data-driven regardless (covers exist via
		// CAA / local extraction without Atlas).
		"AtlasEnabled":    cfg.Atlas.Enabled,
		"BookletsEnabled": s.deps.BookletPath != nil,
	})
}

// pageJobs renders the Jobs page HTML.
func (s *Server) pageJobs(w http.ResponseWriter, r *http.Request) {
	s.renderPage(w, "jobs", nil)
}
