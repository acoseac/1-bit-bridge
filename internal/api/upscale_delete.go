// Package api — handler for DELETE /v1/upscale/variants.
//
// Three query-param shapes:
//
//	DELETE /v1/upscale/variants?confirm=true        → every variant
//	DELETE /v1/upscale/variants?prefix=<rel-path>   → variants under a path prefix
//	DELETE /v1/upscale/variants?path=<rel-path>     → variants for one exact source path
//
// Bearer-token authed via s.authed(...) wrapper at route registration.
// Additive — no ProtocolVersion bump. Pre-feature bridges return 404
// without the X-Bridge-Protocol header; iOS classifies that as
// `.notSupported` and hides any client UI behind the
// `deleteVariants` capability flag.
//
// Response shape: 200 OK with {deletedCount, freedBytes, deletedPaths}.
// Errors:
//   - 400 bad_request: unscoped delete without `confirm=true`; malformed
//     `prefix` / `path`; both `prefix` AND `path` set.
//   - 404 variant_not_found: feature unavailable on this bridge (no
//     variant store wired). Same shape every other variant-not-found
//     surfaces.
//   - 500 internal: SQLite read error during the list step; partial
//     deletes are NOT rolled back (each (unlink, DeleteVariant) pair is
//     idempotent — the `--gc` reverse sweep + integrity watcher reap
//     any zombie state).
package api

import (
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path"
	"strings"
)

// VariantSummary is the api-package-local view of a track_variants
// row. Carries only the columns the delete handler reads — the
// `SidecarPath` (for `os.Remove`), the composite key, and
// `SizeBytes` (for the freedBytes response field). Mirrors how
// `VariantRecord` projects the columns the download handler needs.
type VariantSummary struct {
	SourcePath  string
	VariantID   string
	SidecarPath string
	SizeBytes   int64
}

// VariantDeleter is the interface the DELETE /v1/upscale/variants
// handler uses to enumerate and remove variant rows. cmd/bridge
// wires a thin adapter around manifest.Store; tests stub directly.
// Nil-safe — when nil the handler returns 404 variant_not_found
// (same shape pre-v1.2 bridges produce for the unsupported case).
//
// Three list shapes mirror the three query-param shapes: empty
// prefix to AllVariants, non-empty prefix to ListVariantsByPathPrefix,
// exact path to ListVariantsForPath. The handler picks based on
// query-param parsing; this interface stays narrow.
type VariantDeleter interface {
	AllVariants() ([]VariantSummary, error)
	ListVariantsByPathPrefix(prefix string) ([]VariantSummary, error)
	ListVariantsForPath(sourcePath string) ([]VariantSummary, error)
	DeleteVariant(sourcePath, variantID string) error
}

// InflightDropper is the interface the delete handler uses to
// pre-cancel any transcode-pool dedup entries for source paths
// about to be deleted. Without this, a worker mid-job for a
// matching path would race the handler — its UpsertVariant would
// land AFTER our DeleteVariant, leaving a zombie row that the
// `--gc` reverse pass / integrity watcher reaps later. Calling
// DropInflight clears the dedup so a subsequent re-submit (if
// any) doesn't no-op against a stale slot. Workers ALREADY in
// progress are NOT cancelled (no per-job cancellation primitive);
// the unlink race is documented honestly.
//
// Nil-safe — when no pool is wired the handler skips the drop.
type InflightDropper interface {
	DropInflight(matches func(sourcePath string) bool) int
}

// WithVariantDeleter attaches the deleter adapter and enables the
// `deleteVariants` capability flag in /v1/health.features. nil is
// the default (test harnesses, pre-feature bridges) — the handler
// surfaces 404 and the feature flag stays out of the advertised
// set.
func (s *Server) WithVariantDeleter(d VariantDeleter) *Server {
	s.variantDeleter = d
	return s
}

// WithInflightDropper attaches the transcode pool's dedup-drop
// primitive. Optional — bridges without an upscale pool wired
// (test harnesses, pure-manifest builds) skip the dedup-drop
// step entirely.
func (s *Server) WithInflightDropper(d InflightDropper) *Server {
	s.inflightDropper = d
	return s
}

// upscaleDeleteResponse is the wire shape returned on success.
// DeletedPaths is the set of source paths that had at least one
// variant removed — iOS uses this to reconcile its local
// `Track.bridgeVariants` without waiting for a full delta-sync
// (alongside the upscale.deleted SSE event, which is the primary
// fan-out path).
type upscaleDeleteResponse struct {
	DeletedCount int      `json:"deletedCount"`
	FreedBytes   int64    `json:"freedBytes"`
	DeletedPaths []string `json:"deletedPaths"`
}

// upscaleDelete is the http.HandlerFunc registered at
// DELETE /v1/upscale/variants. Wraps the request scope so the
// per-call ctx (used for the SQLite-row enumeration and the
// deletes) is tied to the client; on disconnect we abandon work
// in the listing phase but still publish the upscale.deleted SSE
// event for whatever we DID delete (so iOS reconciles even when
// the request gets cut short).
func (s *Server) upscaleDelete(w http.ResponseWriter, r *http.Request) {
	if s.variantDeleter == nil {
		writeError(w, http.StatusNotFound, "variant_not_found", "upscaling is not enabled on this bridge")
		return
	}

	q := r.URL.Query()
	hasPrefix := q.Has("prefix")
	hasPath := q.Has("path")
	prefix := strings.TrimSpace(q.Get("prefix"))
	pathParam := strings.TrimSpace(q.Get("path"))
	confirm := strings.EqualFold(q.Get("confirm"), "true")

	// Mutually-exclusive shape — `prefix` AND `path` together is
	// ambiguous (do we widen or narrow?); reject upfront rather
	// than guess.
	if hasPrefix && hasPath {
		writeError(w, http.StatusBadRequest, "bad_request",
			"cannot combine `prefix` and `path` query parameters; pick one")
		return
	}

	// Defense against accidental tooling — a stray
	// `curl -X DELETE /v1/upscale/variants` would wipe the cache.
	// The explicit confirm token forces operators / scripts to
	// opt in deliberately. Per-prefix / per-path deletes are
	// already scoped by the parameter; only the unscoped form
	// requires confirm.
	if !hasPrefix && !hasPath && !confirm {
		writeError(w, http.StatusBadRequest, "bad_request",
			"deleting all variants requires `?confirm=true` to be set explicitly")
		return
	}

	if hasPrefix {
		if v, ok := validateRelativePath(prefix); ok {
			prefix = v
		} else {
			writeError(w, http.StatusBadRequest, "bad_request",
				"`prefix` must be a clean relative path (no leading `/`, no `..`)")
			return
		}
	}
	if hasPath {
		if v, ok := validateRelativePath(pathParam); ok {
			pathParam = v
		} else {
			writeError(w, http.StatusBadRequest, "bad_request",
				"`path` must be a clean relative path (no leading `/`, no `..`)")
			return
		}
	}

	// Phase 1: resolve the target row set under the request ctx.
	// A client disconnect here surfaces as an error and we return —
	// no rows touched.
	var (
		rows []VariantSummary
		err  error
	)
	switch {
	case hasPath:
		rows, err = s.variantDeleter.ListVariantsForPath(pathParam)
	case hasPrefix:
		rows, err = s.variantDeleter.ListVariantsByPathPrefix(prefix)
	default:
		rows, err = s.variantDeleter.AllVariants()
	}
	if err != nil {
		writeErrorLog(w, r, http.StatusInternalServerError, "internal",
			"the bridge couldn't enumerate variants to delete", err)
		return
	}

	resp := upscaleDeleteResponse{DeletedPaths: []string{}}
	if len(rows) == 0 {
		writeJSON(w, http.StatusOK, resp)
		return
	}

	// Phase 2: pre-cancel matching transcode-pool dedup entries
	// so a re-submit for the same source during this request
	// doesn't no-op against a stale dedup slot. Pool's
	// DropInflight is read-then-write under p.mu; the predicate
	// closure is allocation-light (no DB, no I/O) per the
	// interface contract. Document the race shape: a worker
	// already running for a matching path WILL complete and
	// write its sidecar AFTER our delete; the integrity watcher
	// / `--gc` reap that zombie within at most one tick.
	if s.inflightDropper != nil {
		// Build a set of source paths under the lock — predicate
		// uses O(1) map lookup. For all-variants the set is
		// every distinct source path in `rows`; predicate
		// returns true unconditionally only if we want to drop
		// the entire dedup map (the all-variants case), which
		// is structurally equivalent to building the same set.
		// Keep the shape uniform so future predicate evolutions
		// don't fork.
		targets := make(map[string]struct{}, len(rows))
		for _, r := range rows {
			targets[r.SourcePath] = struct{}{}
		}
		_ = s.inflightDropper.DropInflight(func(sourcePath string) bool {
			_, hit := targets[sourcePath]
			return hit
		})
	}

	// Phase 3: unlink THEN DB delete, per the unlink-before-row
	// invariant in CLAUDE.md (avoids the "stats flash 0 while
	// files exist" window — `os.Remove` then `DeleteVariant`
	// means partial failure leaves the DB row alive,
	// re-deletable on a future call rather than a zombie file
	// behind a missing row).
	//
	// Per-file errors log and continue — idempotent semantics
	// throughout; iOS reconciles on the SSE we emit at the end
	// with whatever DID disappear.
	deletedPaths := map[string]struct{}{}
	logger := LoggerFromContext(r.Context())
	for _, row := range rows {
		if err := os.Remove(row.SidecarPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			logger.Warn("variant unlink failed; leaving DB row in place",
				slog.String("sidecar", row.SidecarPath),
				slog.String("variant_id", row.VariantID),
				slog.Any("err", err),
			)
			continue
		}
		if err := s.variantDeleter.DeleteVariant(row.SourcePath, row.VariantID); err != nil {
			logger.Warn("variant DB delete failed after unlink; sidecar gone but row remains",
				slog.String("source_path", row.SourcePath),
				slog.String("variant_id", row.VariantID),
				slog.Any("err", err),
			)
			continue
		}
		resp.DeletedCount++
		resp.FreedBytes += row.SizeBytes
		if _, seen := deletedPaths[row.SourcePath]; !seen {
			deletedPaths[row.SourcePath] = struct{}{}
			resp.DeletedPaths = append(resp.DeletedPaths, row.SourcePath)
		}
	}

	// Phase 4: fan-out to SSE subscribers (iOS). Single event
	// per request, batches every path/variantID pair we
	// successfully removed. Pre-feature bridges have a nop
	// publisher; tests can stub via EventPublisher.
	if resp.DeletedCount > 0 {
		variantIDs := make([]string, 0, len(rows))
		for _, row := range rows {
			variantIDs = append(variantIDs, row.VariantID)
		}
		publishUpscaleDeleted(s.EventPublisher(), resp.DeletedPaths, variantIDs)
	}

	writeJSON(w, http.StatusOK, resp)
}

// validateRelativePath enforces the project-wide "no leading slash,
// no `..`, no empty" contract on operator-supplied path strings.
// Returns the cleaned form (forward slashes, no trailing slash)
// and `true` on accept; the zero value + `false` on reject. Same
// shape every other path-validating handler uses (artwork MBID
// validation is structurally identical though it operates on a
// different alphabet).
func validateRelativePath(p string) (string, bool) {
	if p == "" {
		return "", false
	}
	if strings.HasPrefix(p, "/") {
		return "", false
	}
	if strings.Contains(p, `\`) {
		// Wire shape is forward-slash everywhere; reject
		// Windows-style separators to keep `unicode_lower`
		// comparisons in the store deterministic.
		return "", false
	}
	cleaned := path.Clean(p)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") || cleaned != p {
		// path.Clean elides `.`/`..` only when they're
		// safe to elide — if the cleaned form differs from
		// the input, the caller passed something non-canonical
		// (double-slashes, embedded `..`, trailing `.`).
		// Reject rather than silently accept a transformed
		// path; the operator should send a canonical string.
		return "", false
	}
	return cleaned, true
}
