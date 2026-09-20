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
//
// The one-line form every caller that only prints the reason
// uses. A caller that DELETED FILES ITSELF before probing needs
// to tell the empty case apart from the rest — see
// VariantsDirSweepBlock, which this delegates to so the two
// cannot answer differently.
func VariantsDirSweepBlockReason(dir string) string {
	return VariantsDirSweepBlock(dir).Reason
}

// VariantsDirBlock is the probe's full answer.
//
// Empty is broken out on its own because it is the ONE reason a
// caller can explain away: a sweep that just unlinked files from
// this directory made it empty, and that is not evidence of an
// unmounted volume. Nothing else on this list is explicable that
// way — see gcCheckOutputDirBeforeReverseSweep, the only caller
// that asks.
type VariantsDirBlock struct {
	// Reason is "" when the directory is healthy for sweeping;
	// otherwise the operator-facing phrase.
	Reason string
	// Empty is true only for the exists-is-a-directory-holds-no-
	// entries case. A MISSING directory is not Empty: the two are
	// different facts and a caller acting on one must not act on
	// the other.
	Empty bool
}

// VariantsDirSweepBlock probes dir once and reports both halves.
func VariantsDirSweepBlock(dir string) VariantsDirBlock {
	info, err := os.Stat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return VariantsDirBlock{Reason: "variants directory is missing"}
	case err != nil:
		return VariantsDirBlock{Reason: fmt.Sprintf("cannot stat variants directory: %v", err)}
	case !info.IsDir():
		return VariantsDirBlock{Reason: "variants path is not a directory"}
	}
	empty, err := dirIsEmpty(dir)
	switch {
	case err != nil:
		return VariantsDirBlock{Reason: fmt.Sprintf("cannot read variants directory: %v", err)}
	case empty:
		return VariantsDirBlock{Reason: "variants directory is empty", Empty: true}
	}
	return VariantsDirBlock{}
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
