// Operator-driven variant batches.
//
//   - POST /api/upscale/batch                trigger a batch
//   - GET /api/upscale/batches               Jobs page list
//   - DELETE /api/upscale/batches/{id}       cancel
//   - GET|PATCH /api/upscale/target          Settings target picker
//   - POST /api/upscale/failures/retry       clear the failure debounce
//
// These were written for the Library Inspector, which owned the only UI
// that could reach them. That page is gone; the consumers are now the
// player's album, artist and folder views, plus the Roots page's
// transcoded-cache panel. The endpoints themselves did not change —
// only who calls them, and with which scope (see variantScope).
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
	"net/url"
	"strconv"
	"strings"

	"github.com/acoseac/1-bit-bridge/internal/logging"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// Shared upscale-disabled error pair surfaced by every admin handler that
// gates on `deps.Coordinator.UpscaleEnabled() == false`. Three call sites
// inside handlers_upscale_batch.go + two in handlers_upscale_delete.go;
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
//
// The body names a SCOPE — see variantScope for why the folder form and
// the identity forms are not interchangeable. `path` (including "" for
// the whole library) is the folder form and stays the default, so every
// pre-existing caller is unchanged; `albumIds` / `artistId` /
// `trackPaths` are the identity forms and are mutually exclusive with it
// and with each other.
type adminBatchSubmitRequest struct {
	scopeRequest
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
	if kind != "" && kind != "upscale" && kind != "optimize" {
		writeError(w, http.StatusBadRequest, "invalid-kind",
			`unknown kind: `+req.Kind+` (expected "upscale" or "optimize")`)
		return
	}
	scope, scopeErr := s.resolveVariantScope(r, req.scopeRequest)
	if scopeErr != nil {
		writeError(w, scopeErr.Status, scopeErr.Code, scopeErr.Message)
		return
	}
	var (
		res AdminBatchSubmitResult
		err error
	)
	switch {
	case kind == "optimize" && scope.ByIdentity:
		res, err = s.deps.BatchCoordinator.SubmitOptimizePaths(r.Context(), scope.Label, scope.Paths)
	case kind == "optimize":
		// Optimize ignores caller-supplied targetRate/targetBits —
		// the coordinator auto-derives per-track via
		// TargetRateForOptimize (family-preserving 16/44.1 or 16/48).
		res, err = s.deps.BatchCoordinator.SubmitOptimize(r.Context(), scope.Prefix)
	case scope.ByIdentity:
		res, err = s.deps.BatchCoordinator.SubmitPaths(r.Context(), scope.Label, scope.Paths, req.TargetRate, req.TargetBits)
	default:
		res, err = s.deps.BatchCoordinator.Submit(r.Context(), scope.Prefix, req.TargetRate, req.TargetBits)
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
	// The album grid's coverage snapshot keys on the target, since an
	// album's eligibility depends on it. Dropping it here means the
	// operator who just moved the target sees the bars they moved it
	// for, rather than the previous target's answer for up to a TTL.
	s.InvalidateAlbumCoverage()
	writeJSON(w, http.StatusOK, map[string]any{
		"targetRate": req.TargetRate,
		"targetBits": req.TargetBits,
	})
}

// pageJobs renders the Jobs page HTML.
func (s *Server) pageJobs(w http.ResponseWriter, r *http.Request) {
	s.renderPage(w, r, "jobs", nil)
}

// variantFailureRetryRequest scopes the retry to a subtree ("" = whole
// library), matching the batch-submit and enrichment-retry shapes.
type variantFailureRetryRequest struct {
	Path string `json:"path"`
}

type variantFailureRetryResponse struct {
	Cleared int64 `json:"cleared"`
}

// apiVariantFailureRetry handles POST /api/upscale/failures/retry.
//
// The transcode-failure debounce (migration v39) sidelines a source after
// repeated failures on the same file version, which is what stops the batch
// walks and the auto-optimize sweeper looping on work that cannot succeed.
// That suppression clears itself two ways — the file changes, or the TTL
// expires — but an operator who has just fixed the underlying cause (installed
// a codec, freed disk, replaced a mount) should not have to wait 30 days or
// touch every file's mtime. This is that escape hatch.
//
// No rate guard, unlike the enrichment retry: this touches only local columns
// and starts no upstream traffic, so the worst case of a double-click is one
// redundant UPDATE. The re-attempt itself is still bounded by the debounce —
// a genuinely broken file simply earns its strikes again.
func (s *Server) apiVariantFailureRetry(w http.ResponseWriter, r *http.Request) {
	if s.deps.Manifest == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "manifest store not wired")
		return
	}
	defer r.Body.Close()
	var req variantFailureRetryRequest
	// decodeOptionalJSONBody writes its own 400 on malformed input, and
	// tolerates an absent body — a bare POST means "whole library".
	if !decodeOptionalJSONBody(w, r, &req) {
		return
	}
	normalised, ok := normaliseBrowsePath(req.Path)
	if !ok {
		writeError(w, http.StatusBadRequest, "bad-path", "path escapes the library root")
		return
	}
	n, err := s.deps.Manifest.ClearVariantFailuresUnderPrefix(r.Context(), normalised)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	// Sub-component logger, and the operator-supplied path is scrubbed of
	// control characters first.
	//
	// slog's TextHandler already quotes and escapes values (verified: a
	// newline renders as a literal \n, so a forged `level=ERROR` line is not
	// possible), so this is not closing an exploitable hole. It is worth
	// doing anyway because this log is READ BACK AS TEXT by
	// /api/logs/export, whose parser keys on a strict `time= level=` prefix
	// — belt-and-braces for the one consumer that parses it, and it keeps
	// the taint flow from reaching a sink at all.
	logging.Component("admin.upscale-failures").Info("transcode-failure suppressions cleared",
		"path", scrubForLog(normalised), "rows", n)
	writeJSON(w, http.StatusOK, variantFailureRetryResponse{Cleared: n})
}

// redirectRetiredInspector keeps the Library Inspector's URLs working.
//
// The page is gone — the player's album, artist and folder views cover
// what it did — but its links were bookmarkable and one of them was a
// deep link from the Smart Mixes harmonic wheel. A 404 would read as a
// broken console rather than as a moved feature.
//
// Each of its three URL shapes has an exact successor, so the query
// survives the move:
//
//	?camelot=8A  → /tracks   a key is a list of tracks, not a place
//	?path=Album  → /folders  the same tree, same scoping
//	(bare)       → /folders
//
// 301 rather than 302: this is permanent, and a permanent redirect lets
// a browser stop asking.
func redirectRetiredInspector(w http.ResponseWriter, r *http.Request) {
	q := safeQuery(r)
	target := "/folders"
	switch {
	case q.Get("camelot") != "":
		target = "/tracks?camelot=" + url.QueryEscape(q.Get("camelot"))
	case q.Get("path") != "":
		// normaliseBrowsePath, not the raw value: this builds a URL the
		// browser will follow, and the folder view resolves it against
		// the same rules every other path-bearing endpoint uses.
		if p, ok := normaliseBrowsePath(q.Get("path")); ok && p != "" {
			target = "/folders?path=" + url.QueryEscape(p)
		}
	}
	http.Redirect(w, r, target, http.StatusMovedPermanently)
}
