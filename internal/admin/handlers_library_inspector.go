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

	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/transcode"
)

// adminBatchSubmitRequest is the JSON shape POST /api/upscale/batch
// accepts. Optional `targetRate` / `targetBits` fall back to the
// scan_state-stored admin Settings.
type adminBatchSubmitRequest struct {
	Path       string `json:"path"`
	TargetRate int    `json:"targetRate,omitempty"`
	TargetBits int    `json:"targetBits,omitempty"`
}

// apiUpscaleBatchSubmit handles POST /api/upscale/batch.
func (s *Server) apiUpscaleBatchSubmit(w http.ResponseWriter, r *http.Request) {
	if s.deps.BatchCoordinator == nil {
		writeError(w, http.StatusServiceUnavailable, "upscale-disabled",
			"upscaling is not enabled on this bridge")
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
	res, err := s.deps.BatchCoordinator.Submit(r.Context(), req.Path, req.TargetRate, req.TargetBits)
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
		writeError(w, http.StatusServiceUnavailable, "upscale-disabled",
			"upscaling is not enabled on this bridge")
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
		writeError(w, http.StatusServiceUnavailable, "upscale-disabled",
			"upscaling is not enabled on this bridge")
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
		rate = s.deps.Cfg.Upscale.EffectiveBootstrapTargetRate()
		bits = s.deps.Cfg.Upscale.EffectiveBootstrapTargetBits()
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
func (s *Server) pageLibraryInspector(w http.ResponseWriter, r *http.Request) {
	s.renderPage(w, "library_inspector", map[string]any{
		"UpscaleStoragePath": transcode.OutputDirFor(s.deps.Cfg.DataDir),
	})
}

// pageJobs renders the Jobs page HTML.
func (s *Server) pageJobs(w http.ResponseWriter, r *http.Request) {
	s.renderPage(w, "jobs", nil)
}
