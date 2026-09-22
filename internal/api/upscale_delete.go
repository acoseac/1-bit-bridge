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
	"context"
	"errors"
	"fmt"
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
	AllVariants(ctx context.Context) ([]VariantSummary, error)
	ListVariantsByPathPrefix(ctx context.Context, prefix string) ([]VariantSummary, error)
	ListVariantsForPath(ctx context.Context, sourcePath string) ([]VariantSummary, error)
	DeleteVariant(ctx context.Context, sourcePath, variantID string) error
	// LocateVariantSidecar says where this row's file actually is, so the
	// unlink acts on the file rather than on the row's CLAIM about it.
	// Required rather than an optional capability: there is one production
	// implementation and a bridge that could not answer would silently be
	// the bug this method exists to close.
	LocateVariantSidecar(v VariantSummary) VariantSidecarLocation
	// SidecarStoreState probes the variants directory once and reports
	// both halves of its answer.
	//
	// serveVariant's reactive reap is the third of the three reapers
	// #937 named, and the only one that had no mount check: the sweep in
	// VariantWatcher.tick and the one in `upscale --gc` both refuse
	// wholesale via VariantsDirSweepBlock, while this one deleted a row
	// per PLAY.
	SidecarStoreState() VariantSidecarStoreState
}

// VariantSidecarStoreState is the variants directory's state at one
// probe — api's projection of integrity.VariantsDirBlock, translated at
// the wiring point for the reason VariantSidecarPlacement is.
type VariantSidecarStoreState struct {
	// Available reports whether a MISSING sidecar is evidence about the
	// FILE rather than about the VOLUME. False when the directory is
	// gone, unreadable, not a directory, or empty — a clean unmount
	// reverts a mountpoint to an empty local directory, which is why
	// "empty" belongs here too.
	Available bool
	// Empty is true only for the exists-is-a-directory-holds-no-entries
	// case, and is the ONE reason a caller can explain away: a request
	// that has just unlinked files from this directory is what made it
	// empty. A MISSING directory is not Empty — the two are different
	// facts and a caller acting on one must not act on the other, which
	// is the split gcCheckOutputDirBeforeReverseSweep makes one layer up
	// (#941).
	Empty bool
	// Store identifies the directory INSTANCE this probe observed. Nil
	// when there is nothing to identify (no directory configured, or the
	// probe could not stat one).
	//
	// "We emptied it" and "it unmounted" look identical to a stat: a
	// clean unmount reverts a mountpoint to an empty LOCAL directory,
	// which exists, is a directory, and holds no entries. So an
	// in-store unlink at row k proves the volume was mounted at row k
	// and says nothing about row k+1 — a whole-library delete runs long
	// enough for a mount to drop underneath it, which is why the probe
	// is per row in the first place. Comparing the instance is what
	// tells them apart: the local directory under a mountpoint is not
	// the same directory as the volume that was mounted on it
	// (CodeRabbit on #968).
	Store SidecarStoreIdentifier
}

// SidecarStoreIdentifier identifies one observation of the variants
// directory. Two of them are compared only with Same.
//
// Opaque because identity is a filesystem question — device and inode on
// POSIX, volume and file index on Windows — that os.SameFile answers
// portably and no exported type carries as a value. cmd/bridge wraps a
// FileInfo; this package only ever asks whether two observations name the
// same directory.
type SidecarStoreIdentifier interface {
	Same(other SidecarStoreIdentifier) bool
}

// VariantSidecarPlacement says where a variant row's sidecar actually is.
//
// It is api's projection of integrity.SidecarLocation's verdict — the api
// package cannot import internal/integrity (upward cycle), so cmd/bridge
// translates at the wiring point, the same pattern VariantSummary uses for
// manifest.VariantRow.
type VariantSidecarPlacement int

const (
	// VariantSidecarRecorded — the file is at the path the row records.
	// The zero value, so a caller that cannot locate degrades to today's
	// behaviour rather than to "skip everything".
	VariantSidecarRecorded VariantSidecarPlacement = iota
	// VariantSidecarRelocated — absent at the recorded path, present at
	// the canonical one with the recorded size. The tree moved; the file
	// is real and is the one this row names.
	VariantSidecarRelocated
	// VariantSidecarCopyInFlight — something is at the canonical path but
	// it is not this row's file (the size disagrees).
	VariantSidecarCopyInFlight
	// VariantSidecarAbsent — neither location holds it, or the row never
	// recorded a path at all.
	VariantSidecarAbsent
)

// VariantSidecarLocation is one row's answer from LocateVariantSidecar.
type VariantSidecarLocation struct {
	Placement VariantSidecarPlacement
	// Path is the file to act on: the recorded path when Recorded, the
	// canonical one when Relocated, empty for the other two. Empty is
	// what keeps os.Remove("") off Windows, where it returns
	// ERROR_INVALID_NAME rather than anything errors.Is(ErrNotExist)
	// matches.
	Path string
	// WithinStore reports whether Path lies under the directory
	// SidecarStoreState probes, which is the ONLY unlink that can
	// explain that directory being empty.
	//
	// A recorded path is absolute and need not be under the current
	// variants directory at all — that is the relocation this whole
	// lookup exists for. So a request that unlinks a file from the OLD
	// tree has not emptied the CURRENT one, and counting it would let
	// the empty-store exception run while the current volume is
	// unmounted, deleting a row whose sidecar is intact on the volume
	// that will come back (CodeRabbit on #968).
	//
	// True for Relocated by construction (the canonical path is built
	// from the probed directory), and true when no directory is
	// configured at all, where the probe answers Available and the
	// exception is never reached.
	WithinStore bool
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

// VariantDeleteRequest is the parsed, validated input to
// `RunVariantDelete`. The HTTP handler builds this from query
// parameters; the admin-console wrapper builds it from its own
// URL parsing. Exactly one of `All`, `Prefix`, `Path` is set on
// any valid request — see `(*Server).validateVariantDeleteRequest`
// for the validation surface that produces it.
type VariantDeleteRequest struct {
	// All true → delete every variant in the manifest. Mutually
	// exclusive with `Prefix` / `Path`. The HTTP handler additionally
	// requires `?confirm=true` for this shape; that gate lives in
	// the parsing layer because admin / future callers may have
	// their own confirmation surface.
	All bool
	// Prefix non-empty → delete all variants whose source path is
	// under this path prefix. Must be a cleaned relative path
	// (validateRelativePath).
	Prefix string
	// Path non-empty → delete variants for one exact source path.
	// Must be a cleaned relative path.
	Path string
	// Paths non-empty → delete variants for an explicit SET of exact
	// source paths. Entries must each be a cleaned relative path.
	//
	// This is the identity-scoped shape, and it is not
	// interchangeable with `Prefix`. An album's directory is the
	// common ancestor of its tracks and is routinely shared with
	// other albums — on the reference library 69 of 880 albums share
	// theirs, and one artist folder holds 18 albums flat — so a
	// prefix delete for such an album reclaims its neighbours'
	// sidecars too. Only an explicit set can say "this album and
	// nothing else".
	//
	// No `/v1` query parameter produces this shape; it reaches
	// RunVariantDelete only from the admin route, which expands an
	// album or artist id against the library catalog. The wire
	// contract is therefore unchanged.
	Paths []string
	// Kind narrows the deletion to one variant kind: "" (default,
	// back-compat — deletes ALL kinds matching the path scope),
	// "upscale" (only `upscaled-*` variants), or "optimize" (only
	// `optimized-*` variants). The PR feat/library-inspector-tiles
	// added the kind-aware admin DELETE so per-kind buttons in the
	// inspector drawer can clear one kind without wiping the other.
	// Validated at the parsing layer; invalid values rejected with
	// `bad_request` before reaching here. Empty preserves the
	// pre-feature behaviour so a stray external `DELETE
	// /v1/upscale/variants?prefix=…` keeps working as it did.
	Kind string
}

// VariantDeleteResponse is the wire shape returned on success.
// DeletedPaths is the set of source paths that had at least one
// variant removed — iOS uses this to reconcile its local
// `Track.bridgeVariants` without waiting for a full delta-sync
// (alongside the upscale.deleted SSE event, which is the primary
// fan-out path).
type VariantDeleteResponse struct {
	DeletedCount int      `json:"deletedCount"`
	FreedBytes   int64    `json:"freedBytes"`
	DeletedPaths []string `json:"deletedPaths"`
}

// ErrVariantDeleteUnavailable is the sentinel error `RunVariantDelete`
// returns when the server has no `VariantDeleter` wired (i.e. the
// `deleteVariants` capability is off on this bridge). The HTTP
// handler maps this to 404 variant_not_found; admin maps it to 503
// service_unavailable to match its existing "feature off" pattern.
var ErrVariantDeleteUnavailable = errors.New("variant deleter not wired")

// upscaleDelete is the http.HandlerFunc registered at
// DELETE /v1/upscale/variants. Wraps the request scope so the
// per-call ctx (used for the SQLite-row enumeration and the
// deletes) is tied to the client; on disconnect we abandon work
// in the listing phase but still publish the upscale.deleted SSE
// event for whatever we DID delete (so iOS reconciles even when
// the request gets cut short).
func (s *Server) upscaleDelete(w http.ResponseWriter, r *http.Request) {
	if s.refuseUpscaleMutationInDemoMode(w) {
		return
	}
	// Same two-gate shape as upscaleRequest, and for the same reason: the
	// deleter is wired unconditionally now, so nil-ness alone stopped
	// gating anything and this mutation answered on a bridge that
	// advertises the feature as off.
	if s.variantDeleter == nil || !s.upscaleActive() {
		writeError(w, http.StatusNotFound, "variant_not_found", errMsgUpscalingNotEnabled)
		return
	}

	// safeQuery, not r.URL.Query(): `prefix`/`path` carry library paths,
	// and iOS leaves `+` literal in the query component — the stdlib
	// form-decode turns it into a space and the delete silently no-ops
	// (200 with deletedCount 0). Same hazard the other path-bearing
	// query consumers (serveFile / list / stat / manifest) already guard.
	q := safeQuery(r)
	req, errCode, errMsg := parseVariantDeleteQuery(q)
	if errCode != "" {
		writeError(w, http.StatusBadRequest, errCode, errMsg)
		return
	}

	resp, err := s.RunVariantDelete(r.Context(), req)
	if err != nil {
		if errors.Is(err, ErrVariantDeleteUnavailable) {
			writeError(w, http.StatusNotFound, "variant_not_found", errMsgUpscalingNotEnabled)
			return
		}
		writeErrorLog(w, r, http.StatusInternalServerError, "internal",
			"the bridge couldn't enumerate variants to delete", err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// parseVariantDeleteQuery validates the three mutually-exclusive
// query-param shapes (`?confirm=true` / `?prefix=<rel>` /
// `?path=<rel>`) and returns a normalised `VariantDeleteRequest`.
// On rejection returns `(zero, code, message)` where `code` is the
// wire `error.code` the HTTP handler emits (caller maps to status).
// Exported indirectly via the admin-console wrapper which uses the
// same query-param shape; the admin's CSRF + loopback gates layer
// on top of this validation, not in place of it.
func parseVariantDeleteQuery(q map[string][]string) (req VariantDeleteRequest, errCode string, errMsg string) {
	get := func(k string) string {
		if vs := q[k]; len(vs) > 0 {
			return vs[0]
		}
		return ""
	}
	has := func(k string) bool { _, ok := q[k]; return ok }

	hasPrefix := has("prefix")
	hasPath := has("path")
	prefix := strings.TrimSpace(get("prefix"))
	pathParam := strings.TrimSpace(get("path"))
	confirm := strings.EqualFold(get("confirm"), "true")
	// Kind narrows the delete to one variant kind. Empty preserves
	// the pre-feature behaviour (deletes BOTH kinds matching the
	// path scope). Validated here so the downstream
	// RunVariantDelete sees a known-clean value.
	kind := strings.ToLower(strings.TrimSpace(get("kind")))
	switch kind {
	case "", "upscale", "optimize", "pcm":
		req.Kind = kind
	default:
		return req, "bad_request",
			`unknown kind: ` + kind + ` (expected "upscale", "optimize" or "pcm")`
	}

	// Mutually-exclusive shape — `prefix` AND `path` together is
	// ambiguous (do we widen or narrow?); reject upfront rather
	// than guess.
	if hasPrefix && hasPath {
		return req, "bad_request",
			"cannot combine `prefix` and `path` query parameters; pick one"
	}

	// Defense against accidental tooling — a stray
	// `curl -X DELETE /v1/upscale/variants` would wipe the cache.
	// The explicit confirm token forces operators / scripts to
	// opt in deliberately. Per-prefix / per-path deletes are
	// already scoped by the parameter; only the unscoped form
	// requires confirm.
	if !hasPrefix && !hasPath && !confirm {
		return req, "bad_request",
			"deleting all variants requires `?confirm=true` to be set explicitly"
	}

	if hasPrefix {
		v, ok := validateRelativePath(prefix)
		if !ok {
			return req, "bad_request",
				"`prefix` must be a clean relative path (no leading `/`, no `..`)"
		}
		req.Prefix = v
		return req, "", ""
	}
	if hasPath {
		v, ok := validateRelativePath(pathParam)
		if !ok {
			return req, "bad_request",
				"`path` must be a clean relative path (no leading `/`, no `..`)"
		}
		req.Path = v
		return req, "", ""
	}
	req.All = true
	return req, "", ""
}

// RunVariantDelete is the core delete-variants execution path,
// extracted from `upscaleDelete` so the admin console
// (`DELETE /api/upscale/variants`) can share it without
// duplicating the four-phase list/dedup-drop/unlink+delete/SSE-publish
// loop. The HTTP handler and the admin handler both parse their
// own query params, call this method, and translate the result
// (or `ErrVariantDeleteUnavailable`) into their respective response
// shapes.
//
// Returns `(VariantDeleteResponse, nil)` for the success path
// (including the empty-rows fast path → zero-counts response).
// Returns `(_, ErrVariantDeleteUnavailable)` when `variantDeleter`
// is nil — callers map to 404 (api) or 503 (admin). Any other
// error is a SQLite enumeration failure during the list phase;
// the per-row unlink / DeleteVariant errors log+continue
// internally so the response always reflects what actually
// happened.
//
// The same `upscale.deleted` SSE event is emitted regardless of
// caller — iOS reconciles via that single fan-out path no matter
// whether the user clicked Delete in the iOS app, the admin
// console, or invoked the HTTP endpoint directly.
// errShapeRequired is the exactly-one-scope guard's message, shared by
// the up-front validation and the unreachable default arm so the two
// can't drift.
var errShapeRequired = errors.New("variant delete request must set exactly one of All, Prefix, Path, Paths")

func (s *Server) RunVariantDelete(ctx context.Context, req VariantDeleteRequest) (VariantDeleteResponse, error) {
	if s.variantDeleter == nil {
		return VariantDeleteResponse{}, ErrVariantDeleteUnavailable
	}

	// Defense in depth: validate the exact-one-shape invariant the
	// parsers (api + admin) both produce. The HTTP path can only
	// reach here via `parseVariantDeleteQuery`, which sets exactly
	// one of `All` / `Prefix` / `Path`. But `RunVariantDelete` is
	// an exported method on `*Server` — a future caller (test
	// harness, internal tool) that constructs `VariantDeleteRequest`
	// directly could omit the explicit `All=true` and fall through
	// to the `default` switch arm pre-fix, silently wiping the
	// whole cache. Reject zero-value and mixed-shape requests
	// upfront so the load-bearing "every all-variants delete is an
	// explicit operator opt-in" property holds at the execution
	// boundary, not just at the HTTP boundary (CodeRabbit Major on
	// PR #220).
	shapes := 0
	for _, set := range []bool{req.All, req.Prefix != "", req.Path != "", len(req.Paths) > 0} {
		if set {
			shapes++
		}
	}
	if shapes != 1 {
		return VariantDeleteResponse{}, errShapeRequired
	}

	// Phase 1: resolve the target row set under the request ctx.
	// A client disconnect here surfaces as an error and we return —
	// no rows touched.
	var (
		rows []VariantSummary
		err  error
	)
	switch {
	case req.Path != "":
		rows, err = s.variantDeleter.ListVariantsForPath(ctx, req.Path)
	case len(req.Paths) > 0:
		// Union of the per-path listings. Deliberately reuses
		// ListVariantsForPath rather than adding a set-shaped store
		// query: phase 1 is the only part of this function that
		// differs per shape, and everything destructive below —
		// unlink, DB delete, SSE fan-out — stays the single shared
		// path an admin delete and an iOS delete both run.
		//
		// Paths are de-duplicated here rather than trusted from the
		// caller. The admin route already dedupes while expanding an
		// artist (whose albums can overlap), but RunVariantDelete is
		// exported: a duplicate would list the same sidecar twice, and
		// the second unlink would then report a spurious failure for a
		// file the first one correctly removed.
		seen := make(map[string]struct{}, len(req.Paths))
		for _, p := range req.Paths {
			if _, dup := seen[p]; dup {
				continue
			}
			seen[p] = struct{}{}
			var batch []VariantSummary
			batch, err = s.variantDeleter.ListVariantsForPath(ctx, p)
			if err != nil {
				break
			}
			rows = append(rows, batch...)
		}
	case req.Prefix != "":
		rows, err = s.variantDeleter.ListVariantsByPathPrefix(ctx, req.Prefix)
	case req.All:
		// Validated above; explicit `case req.All` (rather than
		// `default`) closes the "zero-value falls through to wipe
		// the cache" gap noted in the guard comment.
		rows, err = s.variantDeleter.AllVariants(ctx)
	default:
		// Unreachable — the guard above rejects every shape that
		// would land here. Defensive return so a future refactor
		// that loosens the guard doesn't silently re-open the
		// zero-value wipe path.
		return VariantDeleteResponse{}, errShapeRequired
	}
	if err != nil {
		return VariantDeleteResponse{}, err
	}

	// Kind narrowing: filter the listed rows by variant_id prefix
	// before any unlink / DB-delete fires. Done here rather than
	// at the Store layer because all three list shapes
	// (ListVariantsForPath / ListVariantsByPathPrefix / AllVariants)
	// return the full row set; filtering in-memory keeps the Store
	// interface unchanged. Version-agnostic prefix (matches v1
	// AND v2 sidecars) by design — see manifest.VariantKindPrefix*
	// docblock for the rationale.
	//
	// Defense in depth: parseVariantDeleteQuery rejects unknown
	// values at the HTTP boundary, but RunVariantDelete is
	// exported — a future direct caller (test harness, internal
	// tool) that passes `Kind = "junk"` would have `wantPrefix`
	// remain "" and `strings.HasPrefix(..., "")` match every row,
	// silently widening the delete back to BOTH kinds. The
	// `default` arm here closes that gap by returning an error.
	// Empty Kind preserves pre-feature behaviour (no filter,
	// delete BOTH kinds). Per CodeRabbit major on PR #276 round 3.
	var wantPrefix string
	switch req.Kind {
	case "":
		// Empty → no filter; preserves pre-feature back-compat.
	case "upscale":
		wantPrefix = "upscaled-"
	case "optimize":
		// Version-agnostic AND family-inclusive: `optimized-%` also
		// matches the DSD compact tier (`optimized-dsd-v1-…`), which is
		// the optimize KIND's own output.
		wantPrefix = "optimized-"
	case "pcm":
		wantPrefix = "pcm-"
	default:
		return VariantDeleteResponse{}, fmt.Errorf("variant delete request: unknown kind %q (expected empty, \"upscale\", \"optimize\" or \"pcm\")", req.Kind)
	}
	if wantPrefix != "" {
		filtered := rows[:0]
		for _, row := range rows {
			if strings.HasPrefix(row.VariantID, wantPrefix) {
				filtered = append(filtered, row)
			}
		}
		rows = filtered
	}

	resp := VariantDeleteResponse{DeletedPaths: []string{}}
	if len(rows) == 0 {
		return resp, nil
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
		for _, row := range rows {
			targets[row.SourcePath] = struct{}{}
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
	seenVariantIDs := map[string]struct{}{}
	// skippedUnavailable counts rows left alone because the variants
	// directory was unreachable. ONE line after the loop, never one per
	// row: the condition is the same for every row in the request, so
	// per-row logging is the M-SEARCH flood shape — thousands of
	// identical lines that make every other line unfindable.
	skippedUnavailable := 0
	// unlinkedFrom identifies the directory instance THIS request first
	// removed a file from, which is the only thing that makes that
	// directory being empty explicable rather than evidence of an
	// unmount. Nil until such an unlink happens.
	//
	// Narrower than "files removed" twice over: a recorded path can name
	// the OLD tree, and unlinking from there explains nothing about the
	// current one (hence WithinStore); and the directory that was there
	// when we unlinked need not be the one that is there now (hence the
	// identity). Recorded once, on the FIRST in-store unlink — a
	// re-probe per deleted row would put a stat on the happy path of a
	// whole-library delete, and a later instance that differs is refused
	// by the comparison anyway.
	var unlinkedFrom SidecarStoreIdentifier
	deletedVariantIDs := make([]string, 0, len(rows))
	logger := LoggerFromContext(ctx)
	for _, row := range rows {
		// Client disconnect (or admin cancel) cancels ctx; DeleteVariant
		// would then fail on every remaining row while os.Remove keeps
		// unlinking sidecars — a cascade of file-gone/row-kept zombies
		// (recoverable via `--gc`, but noisy). Stop before further
		// destructive I/O.
		if err := ctx.Err(); err != nil {
			break
		}
		// WHERE is the file? `sidecar_path` is a CLAIM, never proof
		// (#937). It is absolute, so a variants directory moved to
		// another host leaves every row naming the old one while every
		// file sits, byte-identical, at its canonical place under the
		// current one. Unlinking the recorded path alone then returns
		// ENOENT for every row, which the guard below reads as "already
		// gone" — so "delete all renditions" answered
		// `deletedCount: 10248, freedBytes: 0`, left 259 GiB on disk
		// with nothing referencing it, and handed `--gc` a tree it now
		// refuses to reclaim (orphans > rows). #937 enumerated three
		// reapers; this path deletes rows too and was not among them.
		loc := s.variantDeleter.LocateVariantSidecar(row)
		if loc.Placement == VariantSidecarCopyInFlight {
			// Something is at the canonical path but it is not this
			// row's file. Unlinking it would take a file the pool may
			// still be writing; deleting the row would strand it.
			// Neither — the same reading LookupVariant takes when it
			// answers 410 instead of reaping.
			logger.Warn("variant delete skipped; a copy is in flight at the canonical path",
				slog.String("source_path", row.SourcePath),
				slog.String("variant_id", row.VariantID),
				slog.String("recorded", row.SidecarPath),
			)
			continue
		}
		// An empty Path — a legacy row that never recorded one, or a
		// sidecar absent from both locations — has nothing to unlink.
		// Treat it as already-gone rather than calling os.Remove("") —
		// on Windows that returns a platform-specific error
		// (ERROR_INVALID_NAME) that errors.Is(os.ErrNotExist) does NOT
		// match, which would log a warning and `continue`, stranding the
		// DB row forever. Seeding removeErr with ErrNotExist flows
		// through the guard below (no warning, row still deleted) and
		// correctly frees 0 bytes (Gemini PR #518).
		removeErr := os.ErrNotExist
		if loc.Path != "" {
			removeErr = os.Remove(loc.Path)
		}
		// Nothing was unlinked. Is the volume even there? With the
		// variants directory unmounted, LocateSidecar stats both the
		// recorded and the canonical path under the same dead
		// mountpoint, answers "absent at both", and the ENOENT flows
		// through the already-gone guard as success — deleting the row
		// while its sidecar sits intact on the volume that will come
		// back. That is the same stranding this handler was fixed for
		// in #959, reached by a different cause, so it takes the same
		// answer the two sweeps give: keep the row.
		//
		// AFTER the locate and only on the already-gone path, not
		// before every row. The probe answers "is a missing sidecar
		// evidence about the FILE or about the VOLUME", which is a
		// question about a file that is missing — asked of every row it
		// skipped ones whose file LocateSidecar had just found. A
		// variants directory pointed at a fresh empty folder, with
		// every rendition still at the absolute path its row records,
		// is the ordinary shape of that: "delete all renditions"
		// unlinked nothing and left the bytes, which is the state
		// #959's lookup exists to fix.
		//
		// And EMPTY is explicable once this request has unlinked
		// something FROM THAT SAME DIRECTORY: on the legacy hash-flat
		// layout (no subdirectories) the last successful unlink is what
		// emptied it, so a probe after that would refuse the remaining
		// rows on a state this very request created —
		// gcCheckOutputDirBeforeReverseSweep's lesson (#941), which
		// took a `removed` count for exactly this reason. Missing,
		// unreadable and not-a-directory still refuse however much was
		// unlinked: this loop removes files and never directories, so
		// it cannot be what took the root.
		//
		// SAME directory, not merely "we unlinked something": a clean
		// unmount reverts a mountpoint to an empty LOCAL directory, so
		// an unlink at row k proves only that the volume was mounted at
		// row k. Without the identity compare, a mount dropping
		// mid-request lets the rows after it be deleted while their
		// sidecars sit intact on the volume that will come back — which
		// is the stranding this handler is being fixed for, re-entered
		// through its own exception (CodeRabbit on #968).
		//
		// Per row rather than once, because a whole-library delete runs
		// long enough for that to happen. The cost is one stat beside
		// the two LocateVariantSidecar already took on the same volume,
		// and only on rows that unlinked nothing.
		if errors.Is(removeErr, os.ErrNotExist) {
			st := s.variantDeleter.SidecarStoreState()
			emptiedByUs := st.Empty && unlinkedFrom != nil &&
				st.Store != nil && st.Store.Same(unlinkedFrom)
			if !st.Available && !emptiedByUs {
				skippedUnavailable++
				continue
			}
		}
		if removeErr == nil && loc.WithinStore && unlinkedFrom == nil {
			// FIRST in-store unlink only — see unlinkedFrom. A store
			// that differs by the time the exception is asked is
			// refused by the comparison, so re-probing per row would
			// buy nothing and cost a stat on the happy path.
			unlinkedFrom = s.variantDeleter.SidecarStoreState().Store
		}
		if removeErr == nil && loc.Placement == VariantSidecarRelocated {
			// AFTER the unlink succeeded, not before it is attempted:
			// logged early, a failing os.Remove produced a journal that
			// reported the unlink and then reported it failing
			// (CodeRabbit on #959). Name BOTH paths — the journal
			// otherwise records what the row claimed rather than what
			// was actually removed, which is the whole distinction this
			// branch exists to make.
			logger.Info("variant delete unlinked a relocated sidecar",
				slog.String("source_path", row.SourcePath),
				slog.String("variant_id", row.VariantID),
				slog.String("recorded", row.SidecarPath),
				slog.String("unlinked", loc.Path),
			)
		}
		if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			logger.Warn("variant unlink failed; leaving DB row in place",
				slog.String("sidecar", row.SidecarPath),
				slog.String("variant_id", row.VariantID),
				slog.Any("err", removeErr),
			)
			continue
		}
		if err := s.variantDeleter.DeleteVariant(ctx, row.SourcePath, row.VariantID); err != nil {
			logger.Warn("variant DB delete failed after unlink; sidecar gone but row remains",
				slog.String("source_path", row.SourcePath),
				slog.String("variant_id", row.VariantID),
				slog.Any("err", err),
			)
			continue
		}
		resp.DeletedCount++
		// Count only bytes ACTUALLY reclaimed — an already-gone sidecar
		// (ErrNotExist: a prior --gc / concurrent cleanup / manual wipe) is
		// reconciled in the DB but freed nothing on disk.
		if removeErr == nil {
			resp.FreedBytes += row.SizeBytes
		}
		// Only successfully-deleted variantIDs land in the SSE
		// payload (CodeRabbit Major + Gemini High on PR #209) —
		// the prior shape leaked failed-delete variantIDs into
		// the event, telling iOS clients "these variants are
		// gone" while the DB row was still alive.
		// Dedup like DeletedPaths below: N tracks sharing one variant
		// kind would otherwise repeat the same ID N times in the SSE.
		if _, seen := seenVariantIDs[row.VariantID]; !seen {
			seenVariantIDs[row.VariantID] = struct{}{}
			deletedVariantIDs = append(deletedVariantIDs, row.VariantID)
		}
		if _, seen := deletedPaths[row.SourcePath]; !seen {
			deletedPaths[row.SourcePath] = struct{}{}
			resp.DeletedPaths = append(resp.DeletedPaths, row.SourcePath)
		}
	}
	if skippedUnavailable > 0 {
		// One line carrying the count, for the reason the per-row
		// logging was not taken: every skipped row has the SAME reason,
		// so N identical lines say nothing the count does not and bury
		// everything else in the journal.
		logger.Warn("variant delete skipped rows; the variants directory is unavailable",
			slog.Int("skipped", skippedUnavailable),
			slog.Int("considered", len(rows)),
		)
	}

	// Phase 4: fan-out to SSE subscribers (iOS). Single event
	// per request, batches every path/variantID pair we
	// successfully removed. Pre-feature bridges have a nop
	// publisher; tests can stub via EventPublisher.
	if resp.DeletedCount > 0 {
		publishUpscaleDeleted(s.EventPublisher(), resp.DeletedPaths, deletedVariantIDs)
	}

	return resp, nil
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
