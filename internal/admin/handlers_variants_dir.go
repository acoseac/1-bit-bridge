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

	"github.com/acoseac/1-bit-bridge/internal/transcode"
)

// variantsDirResponse is the shape returned by both GET and POST.
// Including the snapshot in the POST response lets the UI refresh
// without a follow-up GET round-trip.
type variantsDirResponse struct {
	Current     string `json:"current"`
	Default     string `json:"default"`
	UsedBytes   int64  `json:"usedBytes"`
	FreeBytes   int64  `json:"freeBytes"`
	LegacyCount int    `json:"legacyCount"`
	LegacyBytes int64  `json:"legacyBytes"`
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

	writeJSON(w, http.StatusOK, variantsDirResponse{
		Current:     current,
		Default:     defaultDir,
		UsedBytes:   used,
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
	// be absolute AND not under any library root.
	s.mu.Lock()
	defer s.mu.Unlock()
	cfg := s.deps.CfgHolder.Load()
	if req.Path != "" {
		if !filepath.IsAbs(req.Path) {
			writeError(w, http.StatusBadRequest, "not-absolute",
				"variants directory must be an absolute path")
			return
		}
		if err := assertNotUnderLibraryRoots(req.Path, cfg.LibraryRoots); err != nil {
			writeError(w, http.StatusBadRequest, "under-library-root", err.Error())
			return
		}
	}

	// Mutate config + persist. Save() does atomic temp-file + rename.
	next := *cfg // shallow copy is fine; UpscaleConfig is all value types.
	next.Upscale.VariantsDir = req.Path
	if err := next.Save(s.deps.CfgPath); err != nil {
		writeError(w, http.StatusInternalServerError, "save-config", err.Error())
		return
	}
	// Reload via the holder so subsequent reads (including the
	// transcode pool's next OutputDir computation) see the new value.
	s.deps.CfgHolder.Store(&next)

	// Return the refreshed snapshot.
	current := next.Upscale.EffectiveVariantsDir(next.DataDir)
	defaultDir := transcode.OutputDirFor(next.DataDir)
	ctx := r.Context()
	used, free := s.probeVariantsDirUsage(ctx, current)
	legacyCount, legacyBytes := s.countLegacyVariants(ctx, current)
	writeJSON(w, http.StatusOK, variantsDirResponse{
		Current:     current,
		Default:     defaultDir,
		UsedBytes:   used,
		FreeBytes:   free,
		LegacyCount: legacyCount,
		LegacyBytes: legacyBytes,
	})
}

// assertNotUnderLibraryRoots mirrors `config.validateVariantsDir`'s
// containment check. Duplicated here (rather than exported from
// config) so the admin handler can produce its own error
// formatting without a config-package round-trip. Both paths MUST
// stay in lockstep — a test in `internal/admin` pins the contract
// via a sibling-path acceptance + under-root rejection pair.
func assertNotUnderLibraryRoots(candidate string, roots []string) error {
	cleaned := filepath.Clean(candidate)
	for _, root := range roots {
		if root == "" {
			continue
		}
		cleanedRoot := filepath.Clean(root)
		rel, err := filepath.Rel(cleanedRoot, cleaned)
		if err != nil {
			continue
		}
		if rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return errors.New("variants directory must not be under any library root (variants would tangle with source files)")
		}
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
	_, used, err := s.deps.Manifest.CountVariants(ctx)
	if err != nil {
		used = 0
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
// AND run while holding `s.mu` in apiVariantsDirPatch (Gemini medium
// on PR D2). The SQL path is bounded to a single index range scan +
// aggregate.
func (s *Server) countLegacyVariants(ctx context.Context, currentDir string) (int, int64) {
	if currentDir == "" {
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
