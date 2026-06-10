// /v1/upscale/batch* endpoints (v1.3 operator-driven upscale).
// Companion to /v1/upscale: where the legacy endpoint enqueues a
// single track or a folder of tracks, the batch surface enrolls a
// folder / root / library into a tracked operator-initiated batch
// the admin Library Inspector + Jobs page render from
// `upscale_batches`.
//
// Auth: minted-token bearer, same as /v1/upscale. The endpoints
// surface a 503 `upscale_disabled` when no BatchCoordinator is
// wired (feature off OR sox precheck failed at boot).

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Shared upscale-disabled error pair surfaced by every /v1/upscale*
// handler when the feature is off (cfg.Upscale.Enabled == false or
// no BatchCoordinator wired). Same payload shape as the admin
// package's pair, but the code uses an underscore (`upscale_disabled`)
// matching the public wire convention; admin uses kebab-case for the
// admin JSON channel.
const (
	errCodeUpscaleDisabled    = "upscale_disabled"
	errMsgUpscalingNotEnabled = "upscaling is not enabled on this bridge"
)

// BatchCoordinator is the interface POST /v1/upscale/batch and
// the sibling GET / DELETE endpoints consume. Implemented by
// `transcode.Coordinator` over in internal/transcode; this
// package can't import internal/transcode directly (mirrors the
// UpscaleEnqueuer / VariantStore decoupling pattern).
//
// `Submit` enrolls a path into a new batch; `SubmitOptimize` is the
// kind="optimize" sibling (CarPlay-optimized 16-bit family-preserving
// FLAC, target auto-derived per-track); `Cancel` flips an existing
// batch's status; `ListBatches` returns recent history for the admin
// Jobs page.
type BatchCoordinator interface {
	Submit(ctx context.Context, libraryRelPath string, targetRate, targetBits int) (BatchSubmitResult, error)
	SubmitOptimize(ctx context.Context, libraryRelPath string) (BatchSubmitResult, error)
	Cancel(id uuid.UUID) error
	ListBatches(limit int) ([]BatchRow, error)
	Throughput() BatchThroughput
}

// BatchSubmitResult is the JSON shape POST /v1/upscale/batch
// returns on 202 Accepted. Mirrors transcode.SubmitResult field-
// for-field — the adapter in cmd/bridge translates between the
// two value types so internal/transcode stays free of the api
// package wire shape.
type BatchSubmitResult struct {
	BatchID            string `json:"batchID"`
	Path               string `json:"path"`
	TargetRate         int    `json:"targetRate"`
	TargetBits         int    `json:"targetBits"`
	TotalFiles         int    `json:"totalFiles"`
	AlreadyCovered     int    `json:"alreadyCovered"`
	ProjectedSizeBytes int64  `json:"projectedSizeBytes"`
	AvailableBytes     int64  `json:"availableBytes"`
	EnqueuedCount      int    `json:"enqueuedCount"`
}

// BatchRow is the per-row JSON shape returned by GET /v1/upscale/batches.
// Mirrors manifest.UpscaleBatchRow but stringifies the UUID for
// JSON-decode friendliness on the admin web side.
type BatchRow struct {
	ID             string `json:"id"`
	Path           string `json:"path"`
	TargetRate     int    `json:"targetRate"`
	TargetBits     int    `json:"targetBits"`
	Status         string `json:"status"`
	TotalFiles     int    `json:"totalFiles"`
	ProcessedFiles int    `json:"processedFiles"`
	FailedFiles    int    `json:"failedFiles"`
	SkippedFiles   int    `json:"skippedFiles,omitempty"`
	Error          string `json:"error,omitempty"`
	CreatedAt      int64  `json:"createdAt"`
	UpdatedAt      int64  `json:"updatedAt"`
}

// BatchThroughput carries the rolling-average derived values the
// admin dashboard renders. Mirrors transcode.ThroughputSnapshot.
type BatchThroughput struct {
	JobsPerHour float64 `json:"jobsPerHour"`
	EtaSeconds  float64 `json:"etaSeconds"`
	Samples     int     `json:"samples"`
}

// BatchInsufficientDiskSpace is the typed error returned by
// Coordinator.Submit when the pre-flight refuses. Mirrors
// transcode.InsufficientDiskSpaceError so this package doesn't
// have to import internal/transcode for the type alone. The
// adapter in cmd/bridge converts on Submit's return.
type BatchInsufficientDiskSpace struct {
	ProjectedBytes int64
	RequiredBytes  int64
	AvailableBytes int64
}

func (e *BatchInsufficientDiskSpace) Error() string {
	return "upscale batch: insufficient disk space"
}

// ErrBatchInsufficientDiskSpace is the sentinel for errors.Is checks.
var ErrBatchInsufficientDiskSpace = errors.New("upscale batch insufficient disk space")

func (e *BatchInsufficientDiskSpace) Unwrap() error { return ErrBatchInsufficientDiskSpace }

// BatchRequest is the request body shape for POST /v1/upscale/batch.
// `targetRate` / `targetBits` are optional — when omitted the
// coordinator falls back to the DB-stored admin Settings.
//
// `Kind` is one of "upscale" (default when omitted) or "optimize"
// (CarPlay-optimized; targetRate/targetBits are ignored — the
// coordinator picks family-preserving 16/44.1 or 16/48 per-track).
type BatchRequest struct {
	Path       string `json:"path"`
	Kind       string `json:"kind,omitempty"`
	TargetRate int    `json:"targetRate,omitempty"`
	TargetBits int    `json:"targetBits,omitempty"`
}

// WithBatchCoordinator attaches the coordinator. nil disables the
// new batch surface (handler returns 503 `upscale_disabled`).
// Called once during cmd/bridge wiring.
func (s *Server) WithBatchCoordinator(c BatchCoordinator) *Server {
	s.batchCoordinator = c
	return s
}

// upscaleBatchSubmit handles POST /v1/upscale/batch.
func (s *Server) upscaleBatchSubmit(w http.ResponseWriter, r *http.Request) {
	if s.batchCoordinator == nil {
		writeError(w, http.StatusServiceUnavailable, errCodeUpscaleDisabled,
			errMsgUpscalingNotEnabled)
		return
	}
	defer r.Body.Close()
	var req BatchRequest
	// Same 64 KiB cap rationale as upscaleRequest — tiny shape, capped
	// like every other body-bearing public handler.
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, upscaleMaxBodyBytes)).Decode(&req); err != nil {
		writeErrorLog(w, r, http.StatusBadRequest, "bad_request",
			"request body must be JSON", err)
		return
	}
	// Same path-normalisation contract as POST /v1/upscale: forward-
	// slash, no traversal. Empty string is a legitimate "whole-library"
	// scope and accepted.
	libraryRel := strings.ReplaceAll(strings.TrimSpace(req.Path), `\`, "/")
	libraryRel = strings.TrimPrefix(libraryRel, "/")
	if libraryRel != "" {
		cleaned := path.Clean(libraryRel)
		if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
			writeError(w, http.StatusBadRequest, "bad_request",
				"path contains traversal segments")
			return
		}
		libraryRel = cleaned
		if libraryRel == "." {
			libraryRel = ""
		}
	}
	// Coordinator.Submit resolves the target / outputDir from
	// scan_state + dataDir; the handler doesn't need to know those
	// values. Pass zero for target so the coordinator falls back.
	//
	// Kind dispatch mirrors POST /v1/upscale's behaviour: empty or
	// "upscale" → Submit (upscale variants); "optimize" → SubmitOptimize
	// (CarPlay-optimized variants, target params auto-derived per-track).
	kind := strings.ToLower(strings.TrimSpace(req.Kind))
	var (
		res BatchSubmitResult
		err error
	)
	switch kind {
	case "", "upscale":
		res, err = s.batchCoordinator.Submit(r.Context(), libraryRel, req.TargetRate, req.TargetBits)
	case "optimize":
		res, err = s.batchCoordinator.SubmitOptimize(r.Context(), libraryRel)
	default:
		writeError(w, http.StatusBadRequest, "bad_request",
			`unknown kind: `+req.Kind+` (expected "upscale" or "optimize")`)
		return
	}
	if err != nil {
		var dskErr *BatchInsufficientDiskSpace
		if errors.As(err, &dskErr) {
			// 507 Insufficient Storage — well-defined HTTP semantics
			// for "the request would consume more disk than is
			// available." Carries the numbers in the JSON body so
			// the admin UI can render the projection without
			// re-running it.
			writeJSON(w, http.StatusInsufficientStorage, map[string]any{
				"error":          "insufficient_disk_space",
				"projectedBytes": dskErr.ProjectedBytes,
				"requiredBytes":  dskErr.RequiredBytes,
				"availableBytes": dskErr.AvailableBytes,
			})
			return
		}
		writeErrorLog(w, r, http.StatusInternalServerError, "internal",
			"submit batch failed", err)
		return
	}
	writeJSON(w, http.StatusAccepted, res)
}

// upscaleBatchList handles GET /v1/upscale/batches?limit=N.
func (s *Server) upscaleBatchList(w http.ResponseWriter, r *http.Request) {
	if s.batchCoordinator == nil {
		writeError(w, http.StatusServiceUnavailable, errCodeUpscaleDisabled,
			errMsgUpscalingNotEnabled)
		return
	}
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		// Loose parse — invalid value falls back to default.
		var n int
		if _, err := fmt.Sscan(v, &n); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}
	rows, err := s.batchCoordinator.ListBatches(limit)
	if err != nil {
		writeErrorLog(w, r, http.StatusInternalServerError, "internal",
			"list batches failed", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"batches":     rows,
		"generatedAt": time.Now().UTC(),
	})
}

// upscaleBatchCancel handles DELETE /v1/upscale/batches/{id}.
func (s *Server) upscaleBatchCancel(w http.ResponseWriter, r *http.Request) {
	if s.batchCoordinator == nil {
		writeError(w, http.StatusServiceUnavailable, errCodeUpscaleDisabled,
			errMsgUpscalingNotEnabled)
		return
	}
	idStr := r.PathValue("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request",
			"path parameter must be a valid UUID")
		return
	}
	if err := s.batchCoordinator.Cancel(id); err != nil {
		writeErrorLog(w, r, http.StatusInternalServerError, "internal",
			"cancel batch failed", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
