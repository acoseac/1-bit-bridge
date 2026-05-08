package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"

	bridgefs "github.com/acoseac/1-bit-bridge/internal/fs"
)

// UpscaleEnqueuer is the interface POST /v1/upscale uses to hand
// off a track or folder of tracks to the long-lived transcode
// worker pool. Mirrors the api package's other "feature glue"
// interfaces (ManifestProvider, MBIDProbe, VariantStore) so the
// api package doesn't import internal/transcode directly.
//
// The cmd/bridge wiring constructs an adapter that wraps a
// transcode.Pool. Nil-safe: when WithUpscaleEnqueuer wasn't
// called (or was called with nil because the feature gate / sox
// probe failed), the handler returns 503 `upscale_disabled`.
//
// `libraryRelativePath` is the wire-shape path the manifest
// emits and that iOS sends back. The adapter resolves it to an
// absolute on-disk path via `bridgefs.Resolver` (canonical
// resolution — handles single-root + multi-root layouts
// uniformly), stat()s the source for freshness fields, and
// hands a `transcode.JobSpec` to the Pool.
//
// Errors:
//   - `ErrUpscaleQueueFull` (typed): all queue slots taken;
//     handler returns 503 `queue_full`.
//   - `ErrUpscaleSourceMissing` (typed): the path resolves but
//     stat fails; handler counts as "rejected" with a clear
//     log line. Not a 5xx because a missing source is a
//     scanner-reconciliation issue, not a server fault.
//   - `ErrUpscaleIneligible` (typed): the source is DSD,
//     already at/above target rate, or already has a fresh
//     sidecar. Counted as "rejected" silently — the user
//     can't take action.
//   - any other error: treated as a server fault; the handler
//     logs and counts as rejected.
type UpscaleEnqueuer interface {
	EnqueueOne(libraryRelativePath string) error
}

// ErrUpscaleSourceMissing is returned by EnqueueOne when the
// resolved source path doesn't exist on disk. Distinguished
// from ErrUpscaleQueueFull so the handler can report it
// separately in `rejected` counts and so tests can assert
// the typed branch.
var ErrUpscaleSourceMissing = errors.New("upscale source path missing on disk")

// ErrUpscaleIneligible is returned by EnqueueOne when the
// source is DSD, already above the target rate, or otherwise
// not a candidate for upscaling. Silent rejection — the user
// has no remediation, so we don't surface it in the wire
// response beyond the rejected count.
var ErrUpscaleIneligible = errors.New("upscale source is ineligible")

// UpscaleRequest is the wire shape POST /v1/upscale accepts.
// `path` may be a track file or a folder; the handler stat()s
// to decide. Folder requests recursively enqueue every regular
// file under the folder; the enqueuer's per-track eligibility
// check filters non-PCM / already-at-target / already-cached
// candidates.
type UpscaleRequest struct {
	Path string `json:"path"`
}

// UpscaleResponse is the wire shape POST /v1/upscale returns
// on 202 Accepted.
//
//   - `enqueued`: number of jobs that successfully landed on the
//     worker pool's queue.
//   - `rejected`: number of candidates that were considered but
//     not queued (queue full, ineligible, source missing).
//   - `eligible`: total number of regular files the handler
//     considered (folder walk surface).
//   - `queueFull`: true when at least one rejection was due to
//     queue capacity (vs. eligibility / missing-source). iOS
//     surfaces this in the toast so the user knows to retry.
//
// When `enqueued == 0 && queueFull`, the handler returns
// 503 `queue_full` instead of 202 — every candidate that was
// eligible bounced.
type UpscaleResponse struct {
	Enqueued  int  `json:"enqueued"`
	Rejected  int  `json:"rejected,omitempty"`
	Eligible  int  `json:"eligible,omitempty"`
	QueueFull bool `json:"queueFull,omitempty"`
}

func (s *Server) upscaleRequest(w http.ResponseWriter, r *http.Request) {
	if s.upscaleEnqueuer == nil {
		// Feature off (config flag false) OR sox precheck
		// failed at startup. Both surface as the same wire code
		// — operator privacy, and iOS only needs to know "no
		// variant chrome here".
		writeError(w, http.StatusServiceUnavailable, "upscale_disabled", "upscaling is not enabled on this bridge")
		return
	}
	defer r.Body.Close()
	var req UpscaleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "request body must be JSON: "+err.Error())
		return
	}
	// Normalise to forward-slash form before any path math. The
	// wire protocol explicitly requires forward slashes, but a
	// Windows iOS client (none today, defensive) or a hand-
	// crafted curl from a Windows admin could submit
	// backslashes that would survive the resolver's tolerance
	// only to land at `path.Join` below in unexpected shapes.
	// Cheap defensive normalisation (Gemini medium on PR #109).
	libraryRel := strings.ReplaceAll(strings.TrimSpace(req.Path), `\`, "/")
	if libraryRel == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "path is required")
		return
	}
	abs, info, err := s.resolver.ResolveChecked(libraryRel)
	if err != nil {
		// Same error mapping as serveFile — keeps the 400/404
		// surface consistent across all path-validating
		// endpoints.
		switch {
		case errors.Is(err, bridgefs.ErrBadPath):
			writeError(w, http.StatusBadRequest, "bad_request", "invalid path")
		case errors.Is(err, bridgefs.ErrNotFound), errors.Is(err, bridgefs.ErrUnknownRoot):
			writeError(w, http.StatusNotFound, "not_found", "path does not exist")
		default:
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
		}
		return
	}

	// Single file vs folder.
	candidates := []string{}
	if info.IsDir() {
		walkErr := filepath.WalkDir(abs, func(p string, d os.DirEntry, walkErr error) error {
			if walkErr != nil {
				// Log the path that failed so the operator has
				// something to act on. Returning the error
				// terminates the walk; under WalkDir's
				// fs.SkipDir convention `return nil` would skip
				// this entry's children but continue siblings,
				// but a permission failure on a parent
				// directory indicates a configuration problem
				// the user should know about — surface, don't
				// silently truncate the upscale set.
				//
				// Log the library-relative form (NOT `p`, which
				// is the resolved absolute filesystem path on
				// the host). docs/privacy.html commits to
				// "absolute filesystem paths are not logged" —
				// this branch is the only call site in v0.1.2
				// that would have leaked one. Falls back to the
				// requested `libraryRel` if `Rel` errors (root
				// failure) so the operator still gets a useful
				// pointer.
				relForLog := libraryRel
				if rel, relErr := filepath.Rel(abs, p); relErr == nil && rel != "." {
					relForLog = path.Join(libraryRel, filepath.ToSlash(rel))
				}
				logger.Error("upscale folder walk", "path", relForLog, "err", walkErr)
				return walkErr
			}
			if d.IsDir() {
				return nil
			}
			// Compute the file's library-relative form by
			// joining `libraryRel` with the file's path
			// relative to the requested folder. Forward-
			// slash everything for wire consistency.
			rel, relErr := filepath.Rel(abs, p)
			if relErr != nil {
				return nil // skip; should never trigger under WalkDir
			}
			candidates = append(candidates, path.Join(libraryRel, filepath.ToSlash(rel)))
			return nil
		})
		if walkErr != nil {
			writeError(w, http.StatusInternalServerError, "internal", "walk folder: "+walkErr.Error())
			return
		}
	} else {
		candidates = append(candidates, libraryRel)
	}

	enqueued := 0
	rejected := 0
	queueFull := false
	for _, c := range candidates {
		err := s.upscaleEnqueuer.EnqueueOne(c)
		switch {
		case err == nil:
			enqueued++
		case errors.Is(err, ErrUpscaleQueueFull):
			rejected++
			queueFull = true
		case errors.Is(err, ErrUpscaleSourceMissing), errors.Is(err, ErrUpscaleIneligible):
			// Silent rejection — user has no remediation
			// surface for these. Counted in `rejected` so
			// the response stays honest about how many
			// candidates the handler actually moved.
			rejected++
		default:
			logger.Error("upscale: enqueue failed", "path", c, "err", err)
			rejected++
		}
	}

	if enqueued == 0 && queueFull {
		// Every candidate that was eligible bounced —
		// surface the most useful 503 short-code instead of
		// a misleading 202 with `enqueued: 0`.
		writeError(w, http.StatusServiceUnavailable, "queue_full", "transcode worker queue is full; wait for current conversions to finish")
		return
	}

	writeJSON(w, http.StatusAccepted, UpscaleResponse{
		Enqueued:  enqueued,
		Rejected:  rejected,
		Eligible:  len(candidates),
		QueueFull: queueFull,
	})
}
