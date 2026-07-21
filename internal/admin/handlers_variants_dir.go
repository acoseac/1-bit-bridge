// Admin variants-dir management (v1.4 PR D2). Two surfaces:
//
//   - GET  /api/upscale/variants-dir
//     Returns the current effective variants directory + the default
//     path + used/free bytes on its volume + legacy-variant count
//     (any variants whose sidecar_path doesn't sit under the current
//     dir — operator UI surfaces "N legacy variants — [Migrate]").
//
//   - POST /api/upscale/variants-dir  body {"path": "<absolute>"}
//     Validates + writes the new path to bridge.yaml + reloads the
//     in-memory config. Subsequent upscales land at the new path;
//     existing variants are NOT moved (operator runs
//     `bridge variants move --to <path>` for that).
//
// Loopback-only (enforced upstream); 1 MiB body cap; same
// path-validation rules as `validateVariantsDir`.

package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
	"github.com/acoseac/1-bit-bridge/internal/logging"
	"github.com/acoseac/1-bit-bridge/internal/transcode"
)

// variantsDirResponse is the shape returned by both GET and POST.
// Including the snapshot in the POST response lets the UI refresh
// without a follow-up GET round-trip.
//
// `UsedByKind` splits the total `UsedBytes` figure by variant kind
// ("upscale" / "optimize") so the inspector's storage bar can render
// `Used 12 GB (upscale 8 GB · optimize 4 GB)`. Keys are pre-seeded
// to 0 so consumers can read both unconditionally. A defensive
// "unknown" bucket appears only if `track_variants` ever holds a
// row whose variant_id doesn't match either prefix (should be empty
// in practice; the bridge logs at notice level when it sees one).
//
// Legacy `Current` / `Default` / `UsedBytes` / `FreeBytes` field
// names preserved (existing JS consumers parse them) — `UsedByKind`
// is strictly additive.
type variantsDirResponse struct {
	Current     string           `json:"current"`
	Default     string           `json:"default"`
	UsedBytes   int64            `json:"usedBytes"`
	UsedByKind  map[string]int64 `json:"usedByKind"`
	FreeBytes   int64            `json:"freeBytes"`
	LegacyCount int              `json:"legacyCount"`
	LegacyBytes int64            `json:"legacyBytes"`
}

type variantsDirPatchRequest struct {
	Path string `json:"path"`
}

// apiVariantsDirGet handles GET /api/upscale/variants-dir.
func (s *Server) apiVariantsDirGet(w http.ResponseWriter, r *http.Request) {
	cfg := s.deps.CfgHolder.Load()
	current := cfg.Upscale.EffectiveVariantsDir(cfg.DataDir)
	defaultDir := transcode.OutputDirFor(cfg.DataDir)

	ctx := r.Context()
	used, free := s.probeVariantsDirUsage(ctx, current)
	legacyCount, legacyBytes := s.countLegacyVariants(ctx, current)
	usedByKind := s.probeUsedByKind(ctx)

	writeJSON(w, http.StatusOK, variantsDirResponse{
		Current:     current,
		Default:     defaultDir,
		UsedBytes:   used,
		UsedByKind:  usedByKind,
		FreeBytes:   free,
		LegacyCount: legacyCount,
		LegacyBytes: legacyBytes,
	})
}

// apiVariantsDirPatch handles POST /api/upscale/variants-dir.
//
// The request body's `path` MUST be absolute AND must not resolve
// under any library root — same constraints `validateVariantsDir`
// enforces at config-load time. Empty string is treated as "revert
// to the dataDir default" (the same semantics the YAML field has).
func (s *Server) apiVariantsDirPatch(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	var req variantsDirPatchRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, adminMaxBodyBytes)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad-json", err.Error())
		return
	}
	req.Path = strings.TrimSpace(req.Path)

	// Validation: empty = revert-to-default (valid); non-empty must
	// be absolute AND not under any library root. Validation reads
	// the live config snapshot WITHOUT holding any lock — CfgHolder
	// is atomic-pointer-backed (PR #234) so the read is race-free.
	// The holder's write lock is taken (inside CfgHolder.Update) ONLY
	// for the clone+save+publish sequence below, narrowing the
	// contended region from "validation + disk save + DB probes"
	// down to "save + publish." CodeRabbit Major on PR #245.
	if req.Path != "" {
		if !filepath.IsAbs(req.Path) {
			writeError(w, http.StatusBadRequest, "not-absolute",
				"variants directory must be an absolute path")
			return
		}
		cfg := s.deps.CfgHolder.Load()
		if err := assertNotUnderLibraryRoots(req.Path, cfg.LibraryRoots); err != nil {
			writeError(w, http.StatusBadRequest, "under-library-root", err.Error())
			return
		}
	}

	// Mutate + persist via CfgHolder.Update: the clone-from-live →
	// re-validate → Save → Store sequence runs under the holder's
	// single write lock, shared with every other config writer
	// (settings PATCH, roots add/remove, UPnP server CRUD), so a
	// concurrent writer committing between the validation read above
	// and here is both SEEN (the clone is taken fresh inside the
	// lock) and PRESERVED (its fields ride along in the clone) —
	// 2026-07-21 review finding M13. Save() does atomic temp-file +
	// rename. The disk-snapshot + DB-count probes below run OUTSIDE
	// the lock so a slow filesystem stat can't serialise unrelated
	// admin work.
	err := s.deps.CfgHolder.Update(s.deps.CfgPath, func(next *config.Config) error {
		// Re-validate against THIS locked snapshot's roots: a concurrent apiRootsAdd
		// could have committed a root containing req.Path between the pre-check above
		// and here, and config.Load's validateVariantsDir would then reject the
		// persisted value at next boot (the "UI said fine / boot rejects" hazard).
		// Mirror the pre-check's empty-path carve-out.
		if req.Path != "" {
			if err := assertNotUnderLibraryRoots(req.Path, next.LibraryRoots); err != nil {
				return &cfgAbort{status: http.StatusBadRequest, code: "under-library-root", msg: err.Error()}
			}
		}
		next.Upscale.VariantsDir = req.Path
		return nil
	})
	if err != nil {
		writeCfgUpdateErr(w, err)
		return
	}

	// Return the refreshed snapshot. Disk + DB probes run unlocked —
	// they're read-only and a stale-by-a-few-ms snapshot is fine. The
	// holder's fresh Load IS the clone Update just stored, so
	// subsequent reads (including the transcode pool's next OutputDir
	// computation) see the new value.
	applied := s.deps.CfgHolder.Load()
	current := applied.Upscale.EffectiveVariantsDir(applied.DataDir)
	defaultDir := transcode.OutputDirFor(applied.DataDir)
	ctx := r.Context()
	used, free := s.probeVariantsDirUsage(ctx, current)
	legacyCount, legacyBytes := s.countLegacyVariants(ctx, current)
	usedByKind := s.probeUsedByKind(ctx)
	writeJSON(w, http.StatusOK, variantsDirResponse{
		Current:     current,
		Default:     defaultDir,
		UsedBytes:   used,
		UsedByKind:  usedByKind,
		FreeBytes:   free,
		LegacyCount: legacyCount,
		LegacyBytes: legacyBytes,
	})
}

// probeUsedByKind returns the per-kind byte breakdown of cached
// variants ({"upscale": X, "optimize": Y}) via the manifest store's
// `CountVariantsByKind` aggregate. Always returns BOTH keys pre-
// seeded to 0 even on probe failure so the inspector's storage-bar
// JS reads `usedByKind.upscale` / `usedByKind.optimize` without
// nil-map / missing-key gymnastics.
//
// A non-zero "unknown" bucket (variant_id matching neither
// `upscaled-%` nor `optimized-%`) gets logged at notice level so
// operators see drift in production; the UI ignores the bucket.
func (s *Server) probeUsedByKind(ctx context.Context) map[string]int64 {
	// These bucket keys MUST stay in lockstep with the SQL CASE labels in
	// manifest.CountVariantsByKind ("upscale" / "optimize" / "unknown") —
	// that query is the producer of these names; drift here silently zeroes
	// a dashboard tile. (Deliberately NOT tied to the transcode.JobKind enum:
	// the labels are SQL-derived from the variant_id prefix, not the
	// enqueue-time kind, so the coupling that matters is to the store.)
	out := map[string]int64{
		"upscale":  0,
		"optimize": 0,
	}
	// Degrade to zeros when the manifest store isn't wired (e.g. upscaling
	// disabled) rather than panicking — mirrors every sibling admin
	// handler's `s.deps.Manifest == nil` guard (apiDevicesList et al.).
	if s.deps.Manifest == nil {
		return out
	}
	got, err := s.deps.Manifest.CountVariantsByKind(ctx)
	if err != nil {
		return out
	}
	if v, ok := got["upscale"]; ok {
		out["upscale"] = v
	}
	if v, ok := got["optimize"]; ok {
		out["optimize"] = v
	}
	if unk, ok := got["unknown"]; ok && unk > 0 {
		logging.Component("admin.variants-dir").Warn(
			"unknown variant kind bucket non-empty",
			"bytes", unk,
		)
	}
	return out
}

// assertNotUnderLibraryRoots mirrors `config.validateVariantsDir`'s
// containment check, INCLUDING its symlink resolution — a lexical-only
// check here would let the admin accept a symlinked path that
// `config.Load`'s validation rejects on the next boot (bridge fails to
// start over a value the UI said was fine). The symlink resolution is the
// shared `fsutil.EvalSymlinksOrClean` (single canonical copy, also used by
// config + the `bridge variants move` CLI) so all three stay in lockstep;
// only the admin-specific error formatting lives here. A test in
// `internal/admin` pins the contract via a sibling-path acceptance +
// under-root rejection pair plus a symlink-resolution case.
func assertNotUnderLibraryRoots(candidate string, roots []string) error {
	if fsutil.IsUnderAny(candidate, roots) != "" {
		return errors.New("variants directory must not be under any library root (variants would tangle with source files)")
	}
	return nil
}

// probeVariantsDirUsage returns (usedBytes, freeBytes) for the
// volume hosting `dir`. The `used` figure is the cumulative size
// of variant sidecars (via `Manifest.CountVariants`), not the whole
// volume. Free comes from the wired `AvailableDiskSpace` closure.
//
// Tolerates errors silently — a probe failure surfaces as zeros in
// the UI rather than a 500. The variants_dir CHANGE path is the
// load-bearing operation; if the operator can't see disk numbers
// they can still type a path AND save it.
func (s *Server) probeVariantsDirUsage(ctx context.Context, dir string) (int64, int64) {
	var used int64
	// Guard the manifest deref — nil when upscaling is disabled. The free-
	// space probe below is independent of the manifest, so it still runs.
	if s.deps.Manifest != nil {
		if _, u, err := s.deps.Manifest.CountVariants(ctx); err == nil {
			used = u
		}
	}
	var free int64
	if s.deps.AvailableDiskSpace != nil {
		if v, ferr := s.deps.AvailableDiskSpace(dir); ferr == nil {
			free = v
		}
	}
	return used, free
}

// countLegacyVariants returns (count, bytes) of variants whose
// `sidecar_path` is NOT a child of the current variants directory.
// On a fresh install with no legacy data, returns (0, 0).
//
// Used by the UI to surface "Migrate legacy variants (N)" only
// when there's actually something to migrate.
//
// Implementation routes through `Manifest.CountVariantsNotUnderPrefix`,
// a single SQL aggregate. Pre-fix this helper fetched every variant
// into Go-side memory and iterated — inefficient at 50k+ variants
// AND run while holding apiVariantsDirPatch's config-write lock
// (Gemini medium on PR D2). The SQL path is bounded to a single index
// range scan + aggregate.
func (s *Server) countLegacyVariants(ctx context.Context, currentDir string) (int, int64) {
	if currentDir == "" || s.deps.Manifest == nil {
		return 0, 0
	}
	// Pattern: descendants of `currentDir` start with `currentDir + sep`.
	// `NOT LIKE` against that prefix excludes them; everything else is
	// legacy. Trailing separator prevents false matches on a sibling
	// directory with the same name prefix (e.g. /a/transcoded vs
	// /a/transcoded2).
	prefix := filepath.Clean(currentDir) + string(filepath.Separator)
	count, bytes, err := s.deps.Manifest.CountVariantsNotUnderPrefix(ctx, prefix)
	if err != nil {
		return 0, 0
	}
	return count, bytes
}
