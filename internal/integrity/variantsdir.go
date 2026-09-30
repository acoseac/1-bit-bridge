package integrity

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
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
	// Info is the directory as the probe found it, set only when
	// Reason is "": the observation a sweep judged healthy, for a
	// later check (variantsDirChanged) of whether the path still names
	// that directory. A clean unmount leaves the path naming the local
	// directory under the mountpoint, which is another directory, and
	// the comparison is what sees it (backlog B203).
	Info os.FileInfo
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
	opened, empty, err := dirIsEmpty(dir)
	switch {
	case err != nil:
		return VariantsDirBlock{Reason: fmt.Sprintf("cannot read variants directory: %v", err)}
	case empty:
		return VariantsDirBlock{Reason: "variants directory is empty", Empty: true}
	}
	return VariantsDirBlock{Info: opened}
}

// variantsDirChanged reports why dir no longer names the directory start
// observed, or "" while it does. VariantWatcher.tick asks it after the
// probe that began the tick (VariantsDirSweepBlock, whose Info is start):
// as each row reads as missing, and once more before it deletes anything.
//
// The probe at the start of a tick proves the volume was mounted then and
// says nothing about the rows classified after it: a clean unmount during
// the tick reverts the mountpoint to a local directory, every later row
// reads as a rendition that is gone, and the relocation check walks that
// directory and finds no sidecars (backlog B203: 39 rows of 40 deleted).
// Asking again whether the directory LOOKS unmounted is not enough, since
// the local directory need not be empty; its identity is what an unmount
// changes. os.SameFile is the comparison the variant delete handler makes
// for the same reason (cmd/bridge's sidecarStoreID): device and inode on
// POSIX, volume and file index on Windows, each read from an open handle
// (fsutil.DirIdentity says why never from os.Stat). A nil start is never
// the same.
//
// "" for an empty dir: the watcher probes nothing then (a nil or empty
// provider disables the mount-loss guard), so there is nothing to compare.
func variantsDirChanged(dir string, start os.FileInfo) string {
	if dir == "" {
		return ""
	}
	now, err := fsutil.DirIdentity(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "the variants directory went missing during the sweep"
	case err != nil:
		return fmt.Sprintf("cannot open variants directory during the sweep: %v", err)
	case !os.SameFile(start, now):
		return "the variants directory is no longer the directory this sweep began on"
	}
	return ""
}

// dirIsEmpty reports whether dir holds zero entries, reading at
// most one entry — a full os.ReadDir would materialize every
// name in a 100k-sidecar tree just to answer "any?". It also
// returns the directory's stat from the handle it read, the
// identity VariantsDirBlock.Info carries: from the handle so the
// identity is of the directory whose entries were read, and read
// at the call on Windows too (fsutil.DirIdentity).
func dirIsEmpty(dir string) (opened os.FileInfo, empty bool, err error) {
	f, err := fsutil.OpenDir(dir)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = f.Close() }()
	opened, err = f.Stat()
	if err != nil {
		return nil, false, err
	}
	if _, err := f.ReadDir(1); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, true, nil
		}
		return nil, false, err
	}
	return opened, false, nil
}
