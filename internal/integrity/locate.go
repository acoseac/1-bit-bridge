package integrity

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/transcode"
)

// A recorded sidecar path is a claim about where the file WAS, never
// proof that it is gone.
//
// `track_variants.sidecar_path` is absolute. Copy the database to a host
// where the variants directory has a different path — or move the
// directory and update the config — and every row's recorded path reads
// ENOENT while every file sits, byte-identical, exactly where the
// source-mirrored layout puts it under the CURRENT directory. Field
// report 2026-09-20: 10,248 rows (259.7 GiB of renditions) reaped at
// first boot on a new host, silently, and 200 of the files then
// overwritten by the auto-optimize sweeper before anyone noticed.
//
// So every consumer that turns "the recorded path is missing" into a
// deletion asks THIS file first: is the file at its canonical place under
// the current directory, with the size the row recorded? If so the row is
// RELOCATED and the right action is to adopt the path — an UPDATE of
// `sidecar_path`, no `indexed_at` bump, nothing on the wire — not to reap
// the row and re-render a file that exists. The forward sweeps (the ones
// that unlink FILES no row references) consult the same function from the
// other side: their known set carries the canonical path beside the
// recorded one, so a moved tree's files are never orphans.

// SidecarVerdict classifies one track_variants row against the disk.
type SidecarVerdict uint8

const (
	// SidecarPresent — the recorded path exists. Nothing to do.
	SidecarPresent SidecarVerdict = iota
	// SidecarRelocated — the recorded path is gone, and the canonical
	// path under the current variants dir holds a file of the recorded
	// size. Adopt: rewrite sidecar_path to Canonical.
	SidecarRelocated
	// SidecarMissing — neither the recorded nor the canonical path holds
	// a file. The one verdict a reaper may delete on.
	SidecarMissing
	// SidecarMismatched — the recorded path is gone and the canonical
	// path holds a file of a DIFFERENT size: a copy still in flight, or a
	// rendering this row never described. Keep the row untouched — the
	// next sweep may find the copy complete, and a reap here would have
	// the sweeper render over a file someone is still writing.
	SidecarMismatched
	// SidecarUnknown — a stat failed with something other than ENOENT
	// (EACCES, EIO, a wedged mount). Keep the row; Err carries the cause.
	SidecarUnknown
)

// String names the verdict for logs and test failures.
func (v SidecarVerdict) String() string {
	switch v {
	case SidecarPresent:
		return "present"
	case SidecarRelocated:
		return "relocated"
	case SidecarMissing:
		return "missing"
	case SidecarMismatched:
		return "mismatched"
	case SidecarUnknown:
		return "unknown"
	}
	return fmt.Sprintf("SidecarVerdict(%d)", uint8(v))
}

// SidecarLocation is LocateSidecar's answer: the verdict, the canonical
// path it probed (empty when there was nothing to probe — no variants
// dir, a row with no source identity, or a recorded path that already IS
// the canonical one), and the stat error behind SidecarUnknown.
type SidecarLocation struct {
	Verdict   SidecarVerdict
	Canonical string
	Err       error
}

// CanonicalSidecarPath is where row's file belongs under variantsDir —
// transcode.VariantSidecarPath, the layout the pool writes and `bridge
// variants move` produces. Empty when the row carries no source identity
// (a lister that projects only paths) or no directory is known, so a
// caller building a known set can range over rows and skip the empties.
func CanonicalSidecarPath(variantsDir string, row VariantSnapshot) string {
	if variantsDir == "" || row.SourcePath == "" || row.VariantID == "" {
		return ""
	}
	return transcode.VariantSidecarPath(variantsDir, row.SourcePath, row.VariantID)
}

// LocateSidecar stats the recorded path and, on ENOENT, the canonical
// one. Pure classification: it writes nothing, so the watcher, the CLI
// reverse sweep and the serve-side lookup can share it and act on the
// verdict in their own way (adopt-and-continue, adopt-and-print,
// adopt-and-serve).
//
// os.Stat, not Lstat, on both probes: a symlink to a missing target is
// missing from the serving path's point of view, which is the point of
// view a reaper must take (Gemini on PR #207, pinned by the CLI's
// broken-link test). The size compare is exact — `size_bytes` is what the
// writer stat'd after the atomic rename, and a sidecar is never modified
// in place — so a partial copy can never be adopted.
func LocateSidecar(variantsDir string, row VariantSnapshot) SidecarLocation {
	return locateRecordedFile(row.SidecarPath, CanonicalSidecarPath(variantsDir, row), row.SizeBytes)
}

// locateRecordedFile is the classification itself, over a recorded path,
// the canonical path it belongs at, and the size the row claims.
//
// ONE body, because LocateSidecar and LocateWaveform make the same
// decision about two different tables and there is no reading on which
// they should ever answer differently: both are "a recorded path is a
// claim about where the file was". Two copies would be two places to fix
// the day a verdict moves, and the one that drifts is the one deciding
// whether a file is adopted or stranded. (The variants half already
// keeps `MassDeleteRefusal` this way, for the same reason.)
//
// `canonical == ""` means the caller had nothing to probe — no
// directory, or a row with no source identity — and `canonical ==
// recorded` means the row already points where the file belongs, i.e.
// the ordinary "the operator deleted this one" case. Both are Missing:
// there is nowhere else to look.
//
// os.Stat, not Lstat, on both probes: a symlink to a missing target is
// missing from the serving path's point of view, which is the point of
// view a reaper must take (Gemini on PR #207, pinned by the CLI's
// broken-link test). The size compare is exact — the writer stat'd it
// after the atomic rename, and neither a variant nor a waveform is
// modified in place — so a partial copy can never be adopted.
func locateRecordedFile(recorded, canonical string, wantSize int64) SidecarLocation {
	_, err := os.Stat(recorded)
	if err == nil {
		return SidecarLocation{Verdict: SidecarPresent}
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return SidecarLocation{Verdict: SidecarUnknown, Err: err}
	}
	if canonical == "" || canonical == recorded {
		return SidecarLocation{Verdict: SidecarMissing}
	}
	info, err := os.Stat(canonical)
	switch {
	case err == nil && info.Mode().IsRegular() && info.Size() == wantSize:
		return SidecarLocation{Verdict: SidecarRelocated, Canonical: canonical}
	case err == nil:
		return SidecarLocation{Verdict: SidecarMismatched, Canonical: canonical}
	case errors.Is(err, fs.ErrNotExist):
		return SidecarLocation{Verdict: SidecarMissing, Canonical: canonical}
	default:
		return SidecarLocation{Verdict: SidecarUnknown, Canonical: canonical, Err: err}
	}
}

// variantSidecarNameRe recognises a bridge sidecar by its basename in
// EITHER on-disk layout — the source-mirrored `<src>.<variantID>.flac`
// and the legacy hash-flat `<hash>-<variantID>.flac` — by requiring a
// well-formed variant id somewhere before the `.flac`, introduced by a
// dot or a dash. Deliberately looser than the scanner's anchored
// `isVariantSidecarName`: that one must never mistake a track for a
// sidecar, this one must never mistake a sidecar for junk, and the two
// errors point in opposite directions. Built on the scanner's own
// VariantIDPattern so both see the same family list.
var variantSidecarNameRe = regexp.MustCompile(`(?:^|[.-])` + manifest.VariantIDPattern + `\.flac$`)

// looksLikeVariantSidecar reports whether a basename is a bridge
// sidecar in either layout.
func looksLikeVariantSidecar(name string) bool {
	return variantSidecarNameRe.MatchString(name)
}

// errFoundSidecar stops the walk in TreeHoldsVariantSidecars at the
// first hit; it never escapes the function.
var errFoundSidecar = errors.New("integrity: sidecar found")

// resolveSidecarRoot is the one place a sidecar walk turns a configured
// directory into the path filepath.WalkDir may be handed.
//
// WalkDir Lstats its root and follows no link, so a variants directory
// that is itself a symlink (`/srv/variants -> /mnt/vol/…`, the ordinary
// mountpoint alias) arrives at the callback as ONE non-directory entry
// and the walk ends there. The two consumers read that differently and
// both read it wrong: TreeHoldsVariantSidecars sees a tree that "holds
// nothing", and TakeSidecarInventory — whose `upscale --gc` caller
// passes a nil Consider, so every non-directory is a candidate —
// classifies the variants directory ITSELF as an orphan file and hands
// it to the forward sweep to unlink. One orphan is below the
// mass-orphan floor of ten, so no guard can see it.
//
// ONE body, because the two have to agree about what the tree IS before
// they can disagree about what is in it: #937 resolved the root in the
// read-only guard and #940's new shared walker — the one that DELETES —
// did not get it.
//
// The error is returned raw. "Missing" means opposite things to the two
// callers — fail closed for a guard, "a bridge that never transcoded
// anything" for an inventory — so the classification stays with the
// caller. What neither may do is fall back to the unresolved path: that
// walks the symlink raw and is the defect.
func resolveSidecarRoot(dir string) (string, error) {
	return filepath.EvalSymlinks(dir)
}

// TreeHoldsVariantSidecars reports whether at least one file under dir
// is a bridge sidecar (looksLikeVariantSidecar), pruning dot-directories
// the way the orphan sweep does — a `.Trashes/` full of sidecars an
// operator threw away is not a tree that still holds them. Stops at the
// first hit, so on the tree it exists for (a relocated library, every
// file a sidecar) it reads one directory entry; only a tree with NO
// sidecars is walked whole, and that walk is what proves the negative.
//
// Symlinks count when they resolve to a regular file, and the ROOT is
// resolved before the walk. filepath.WalkDir follows neither: a variants
// directory that is itself a symlink (`/srv/variants -> /mnt/vol/…`, the
// ordinary mountpoint alias) would otherwise walk as one non-directory
// entry and "hold nothing", and a tree of symlinked sidecars the same —
// both bypassing the guard on exactly the deployments an operator has
// arranged by hand (Gemini on #937). A live link IS a sidecar here
// because the serving path opens through it (the #207 broken-link rule);
// a dangling one is not.
//
// Any walk error other than the sentinel counts as "unknown" and is
// returned so the caller can fail closed: a directory it cannot read is
// not evidence the sidecars are gone.
func TreeHoldsVariantSidecars(dir string) (bool, error) {
	if dir == "" {
		// Refused before resolveSidecarRoot: filepath.EvalSymlinks("") is
		// ".", so an unguarded "" would walk the working directory.
		return false, errors.New("integrity: no variants directory")
	}
	root, err := resolveSidecarRoot(dir)
	if err != nil {
		return false, err
	}
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			if path != root && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !looksLikeVariantSidecar(d.Name()) {
			return nil
		}
		if d.Type().IsRegular() {
			return errFoundSidecar
		}
		if d.Type()&fs.ModeSymlink != 0 {
			if info, statErr := os.Stat(path); statErr == nil && info.Mode().IsRegular() {
				return errFoundSidecar
			}
		}
		return nil
	})
	switch {
	case errors.Is(err, errFoundSidecar):
		return true, nil
	case err != nil:
		return false, err
	}
	return false, nil
}

// massDeleteFloor is the smallest deletion the relocation guard will
// ever refuse. Below it the percentage is meaningless — one deleted
// sidecar in a four-row catalog is 25% — and the cost of being wrong is
// one rendition, not a library. The guard exists for the whole-tree
// move.
const massDeleteFloor = 10

// MassDeleteRefusal decides whether a sweep that found `missing` of
// `total` rows reapable should be refused, and says why. It is the ONE
// decision both reapers make — the watcher (variants.go) and `bridge
// upscale --gc` (cmd/bridge) — so they cannot disagree about what a
// relocation in progress looks like.
//
// Refuses when all three hold: the deletion clears massDeleteFloor, it
// exceeds maxDeletePercent of the catalog (100 disables; 0 refuses any
// mass deletion), and the variants directory still holds sidecar files —
// the signature of a tree that is there but not where the rows say. A
// tree that holds no sidecars is a library whose files really went, and
// the sweep proceeds as it always has; a tree that cannot be read is
// treated as holding them (fail closed, with the error in the reason).
//
// The reason is empty on "proceed". It names the numbers, so a WARN built
// from it tells the operator what was seen rather than that something
// was refused.
func MassDeleteRefusal(variantsDir string, missing, total, maxDeletePercent int) string {
	if missing < massDeleteFloor || total <= 0 {
		return ""
	}
	if missing*100 <= maxDeletePercent*total {
		return ""
	}
	holds, err := TreeHoldsVariantSidecars(variantsDir)
	if err != nil {
		return fmt.Sprintf("%d of %d rows (%d%%) have no sidecar at either location, over the %d%% threshold, and the variants directory could not be read (%v)",
			missing, total, missing*100/total, maxDeletePercent, err)
	}
	if !holds {
		return ""
	}
	return fmt.Sprintf("%d of %d rows (%d%%) have no sidecar at either location, over the %d%% threshold, while the variants directory still holds sidecar files",
		missing, total, missing*100/total, maxDeletePercent)
}
