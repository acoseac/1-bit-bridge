package integrity

import (
	"errors"
	"fmt"
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
// reverts to a local directory — every sidecar stats ENOENT, and an
// unguarded sweep mass-deletes the entire track_variants catalog in
// one pass (2026-07-21 review H4/M15).
//
// Returns "" when the directory is healthy for sweeping: it exists, is
// a directory, and holds a rendition (a file looksLikeVariantSidecar
// names, found by scanForRenditions, the walk TreeHoldsVariantSidecars
// makes) or a link to a directory, which the walk cannot see behind.
// Otherwise a short human-readable reason the caller logs/prints
// alongside its refusal. Any probe failure (stat error, a directory it
// cannot read) also blocks: a sweep that can't see the directory can't
// distinguish "sidecar deleted" from "filesystem fault", and refusing
// costs the operator one re-run while a wrong sweep costs a full
// library re-transcode.
//
// RENDITIONS, not entries (backlog B223). This called a directory
// healthy when it held any entry at all until 2026-09-29, and the local
// directory an unmount leaves need not be empty: anything written there
// while the volume is away (a Finder .DS_Store, a README, the folders a
// render makes before sox writes, which a render that fails leaves)
// made it "healthy", and the watcher's next tick read every row as a
// rendition that is gone and deleted all forty (measured). What such a
// directory holds is what an unmounted volume looks like, whatever
// else is in it, so it is Empty here, as an empty one is. So is a tree
// whose every rendition was deleted by hand, which the directory alone
// cannot tell apart: `bridge upscale --gc --allow-mass-delete` is the
// operator's way past it, and the background sweep has none.
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

// SidecarDirBlock is the probe's full answer, for a variants directory
// (VariantsDirSweepBlock) or the waveform directory (WaveformDirSweepBlock).
//
// Empty is broken out on its own because it is the ONE reason a
// caller can explain away: a sweep that just unlinked renditions from
// this directory made it hold none, and that is not evidence of an
// unmounted volume. Nothing else on this list is explicable that
// way — see gcCheckOutputDirBeforeReverseSweep and the variant delete
// handler, the callers that ask.
type SidecarDirBlock struct {
	// Reason is "" when the directory is healthy for sweeping;
	// otherwise the operator-facing phrase.
	Reason string
	// Empty is true only for the exists-is-a-directory-holds-no-
	// rendition case, whether it holds nothing at all ("is empty") or
	// only entries that are not renditions ("holds no rendition"). A
	// MISSING directory is not Empty: the two are different facts and a
	// caller acting on one must not act on the other.
	Empty bool
	// Missing is true only when nothing is at the path (the stat's
	// fs.ErrNotExist), which a render's guard words apart from Empty
	// (cmd/bridge's renditionOutputGuard, backlog B727).
	Missing bool
	// Info is the directory whose entries the probe read, from the
	// handle it read them with, so it and Empty (or a healthy
	// Reason) are about one directory: set when Reason is "" or Empty
	// is true, nil otherwise. A later check compares it with the
	// directory at the path then (VariantsDirChanged; the variant
	// delete handler's SidecarStoreState), since a clean unmount
	// leaves the path naming the local directory under the
	// mountpoint, which is another directory (backlog B203).
	Info os.FileInfo
}

// VariantsDirSweepBlock probes dir once and reports both halves.
func VariantsDirSweepBlock(dir string) SidecarDirBlock {
	return dirSweepBlock(dir, variantSidecars)
}

// WaveformDirSweepBlock is VariantsDirSweepBlock for the waveform directory:
// the same probe, by the same rules, finding a curve where the variants
// probe finds a rendition. A waveform directory that is missing, or holds no
// curve while the catalog records curves, is what a volume that is away
// looks like, and what a directory emptied by hand looks like too, which the
// directory alone cannot tell apart; the regeneration guard refuses both
// (JudgeWaveformCatalog, backlog B652), so the catalog's last curve gone
// reads as refused, never as one curve to render again.
func WaveformDirSweepBlock(dir string) SidecarDirBlock {
	return dirSweepBlock(dir, waveformSidecars)
}

// dirSweepBlock is the probe, for fam's directory: VariantsDirSweepBlock's
// docblock is its rules.
func dirSweepBlock(dir string, fam sidecarFamily) SidecarDirBlock {
	info, err := os.Stat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return SidecarDirBlock{Reason: fam.dir + " is missing", Missing: true}
	case err != nil:
		return SidecarDirBlock{Reason: fmt.Sprintf("cannot stat %s: %v", fam.dir, err)}
	case !info.IsDir():
		return SidecarDirBlock{Reason: fam.path + " is not a directory"}
	}
	scan, err := scanForSidecars(dir, fam)
	switch {
	case scan.rendition:
		return SidecarDirBlock{Info: scan.root}
	case err != nil:
		return SidecarDirBlock{Reason: fmt.Sprintf("cannot read %s: %v", fam.dir, err)}
	case scan.dirLink:
		// Renditions may sit behind the link, where the scan does not
		// look: no evidence the volume is gone, the reading every entry
		// got before B223.
		return SidecarDirBlock{Info: scan.root}
	case !scan.entries:
		return SidecarDirBlock{Reason: fam.dir + " is empty", Empty: true, Info: scan.root}
	}
	return SidecarDirBlock{Reason: fam.dir + " holds no " + fam.file, Empty: true, Info: scan.root}
}

// VariantsDirReplaced is the answer VariantsDirChanged gives when the path
// still names a directory and that directory is not the one start was
// taken from. A directory that went missing, or that cannot be opened,
// is a different answer: the sweep's health check already words those.
const VariantsDirReplaced = "the variants directory is no longer the directory this sweep began on"

// VariantsDirChanged reports why dir no longer names the directory start
// observed, or "" while it does. VariantWatcher.tick asks it after the
// probe that began the tick (VariantsDirSweepBlock, whose Info is start):
// as each row reads as missing, and once more before it deletes anything.
// upscale --gc asks it as each row reads missing, before the forward sweep
// unlinks and before each unlink, and again in
// gcCheckOutputDirBeforeReverseSweep before that sweep deletes a row.
// analyze --gc asks it before it unlinks and before each unlink.
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
func VariantsDirChanged(dir string, start os.FileInfo) string {
	return dirChanged(dir, start, variantSidecars, "sweep")
}

// WaveformDirChanged is VariantsDirChanged for the waveform directory, in its
// words: `analyze --gc` asks it before it unlinks and before each unlink.
func WaveformDirChanged(dir string, start os.FileInfo) string {
	return dirChanged(dir, start, waveformSidecars, "sweep")
}

// dirChanged is the identity check, for fam's directory, worded for the
// activity that asks it ("sweep", or the regeneration guard's "check"):
// VariantsDirChanged's docblock is its rules, and fsutil.DirStillIs the
// comparison itself, which the analysis pool's waveform-only repair makes
// too (analyze imports no integrity).
func dirChanged(dir string, start os.FileInfo, fam sidecarFamily, activity string) string {
	if dir == "" {
		return ""
	}
	err := fsutil.DirStillIs(dir, start)
	switch {
	case err == nil:
		return ""
	case errors.Is(err, fs.ErrNotExist):
		return "the " + fam.dir + " went missing during the " + activity
	case errors.Is(err, fsutil.ErrDirReplaced):
		return "the " + fam.dir + " is no longer the directory this " + activity + " began on"
	}
	return fmt.Sprintf("cannot open %s during the %s: %v", fam.dir, activity, err)
}
