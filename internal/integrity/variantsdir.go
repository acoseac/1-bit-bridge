package integrity

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
)

// VariantsDirSweepBlockReason probes a variants output directory
// before a sweep that deletes catalog rows whose sidecar files are
// missing on disk. Shared by VariantWatcher's per-tick reverse
// sweep (variants.go) and the operator-triggered
// `bridge upscale --gc` reverse pass (cmd/bridge/upscale.go) —
// both interpret a per-row ENOENT as "the variant is gone" and
// delete the row, so both must refuse en-masse when the whole
// directory looks gone.
//
// The hazard: the variants dir may live on a network/external
// mount. When that volume is CLEANLY unmounted, the mountpoint
// reverts to an empty local directory — every sidecar stats
// ENOENT, and an unguarded sweep mass-deletes the entire
// track_variants catalog in one pass (2026-07-21 review H4/M15).
// A cleanly-unmounted mountpoint is MISSING or EMPTY; a live
// variants dir backing a non-empty catalog is neither.
//
// Returns "" when the directory is healthy for sweeping (exists,
// is a directory, holds at least one entry); otherwise a short
// human-readable reason the caller logs/prints alongside its
// refusal. Any probe failure (stat error, unreadable directory)
// also blocks: a sweep that can't see the directory can't
// distinguish "sidecar deleted" from "filesystem fault", and
// refusing costs the operator one re-run while a wrong sweep
// costs a full library re-transcode.
//
// Callers gate on row count themselves: with zero catalog rows
// there is nothing to lose and the sweep should proceed (the
// legitimately-empty state before any upscale ever ran).
func VariantsDirSweepBlockReason(dir string) string {
	info, err := os.Stat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "variants directory is missing"
	case err != nil:
		return fmt.Sprintf("cannot stat variants directory: %v", err)
	case !info.IsDir():
		return "variants path is not a directory"
	}
	empty, err := dirIsEmpty(dir)
	switch {
	case err != nil:
		return fmt.Sprintf("cannot read variants directory: %v", err)
	case empty:
		return "variants directory is empty"
	}
	return ""
}

// dirIsEmpty reports whether dir holds zero entries, reading at
// most one entry — a full os.ReadDir would materialize every
// name in a 100k-sidecar tree just to answer "any?".
func dirIsEmpty(dir string) (bool, error) {
	f, err := os.Open(dir)
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	if _, err := f.ReadDir(1); err != nil {
		if errors.Is(err, io.EOF) {
			return true, nil
		}
		return false, err
	}
	return false, nil
}
