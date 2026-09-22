package integrity

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// The forward sweeps delete FILES, and until now the only thing standing
// between them and a whole variants tree was "is the known set EMPTY?".
//
// That question is the wrong shape. The 2026-09-20 relocation left the
// catalog empty for a few minutes and then NOT empty: the auto-optimize
// sweeper, whose candidate query is "no fresh variant row exists", started
// re-rendering and had written 200 rows before anyone looked. A
// `bridge upscale --gc` at that moment would have walked 10,248 files,
// matched 200 of them against the catalog and unlinked the other 10,048 —
// 254 GiB — with `gcRefuseEmptyKnownSetOverPopulatedDir` satisfied, because
// 200 is not zero, and with the relocation guard silent, because every one
// of those 200 rows had its file exactly where it said.
//
// So the forward sweeps need their own denominator, and it is not the one
// the reverse sweeps use. MassDeleteRefusal asks "how much of the CATALOG
// would this delete" — the right question about rows, and useless here: a
// catalog that lost its index is small, and the deletion does not touch it
// at all. The question about files is "how much of the TREE would this
// delete, and is there more of it than the catalog could ever have
// referenced".
//
// TakeSidecarInventory answers the first half by walking once and
// classifying rather than deleting as it goes; MassOrphanRefusal decides
// on the answer. Both are shared, for the reason LocateSidecar and
// MassDeleteRefusal are: `upscale --gc`'s file walk, `analyze --gc`'s and
// `bridge doctor`'s `variants-index` check must not each carry their own
// idea of what a tree that lost its index looks like.

// SidecarInventory is one read-only classification pass over a sidecar
// tree. Files partitions into Known + Orphans; Scratch is counted apart
// because a half-written `.tmp` is the sweep's own litter, never the
// operator's data, and including it in the ratio would let a crashed run
// trip the guard on the next one.
type SidecarInventory struct {
	// Files is every candidate the walk classified — the ones Consider
	// accepted. Scratch files are NOT among them.
	Files int
	// Known and Orphans partition Files: a file whose cleaned, folded path
	// is in the known set, and one that is not.
	Known   int
	Orphans int
	// OrphanPaths holds the orphans in walk order, capped by
	// MaxOrphanPaths (all of them when that is 0). The `--gc` sweeps ask
	// for all of them — they are about to unlink exactly this list, and a
	// second walk could see a different tree. The doctor asks for a
	// handful, to name examples in its hint.
	OrphanPaths []string
	// ScratchPaths holds every scratch file, uncapped: the callers that
	// ask for them remove them unconditionally.
	ScratchPaths []string
	// Unreadable counts directories the walk could not descend into.
	// Their contents are missing from every count above, which can only
	// make the deletion set SMALLER — the known set comes from the
	// database, not from the walk — so it is reported rather than
	// refused. A report built from a partial tree should say so.
	Unreadable int
	// Truncated is true when the walk stopped at MaxEntries with more of
	// the tree unseen. A caller that deletes must not truncate; a caller
	// that reports must scope its claim to what it looked at.
	Truncated bool
}

// SidecarInventoryOptions configures one inventory pass.
type SidecarInventoryOptions struct {
	// Consider reports whether a BASENAME is a file this sweep manages.
	// nil accepts every file, which is what `upscale --gc` has always
	// done inside its variants directory (a foreign file there is an
	// orphan to it, and changing that is a separate decision).
	Consider func(name string) bool
	// Scratch reports whether a basename is the sweep's own half-written
	// temporary. nil means the family has none.
	Scratch func(name string) bool
	// MaxEntries caps how many entries the walk TRAVERSES — every
	// directory and every file it is handed, whether or not Consider
	// accepts it; 0 is unbounded. A walk that hits the cap sets
	// Truncated.
	//
	// Traversed, not classified, because the cap is a wall-clock bound
	// and a directory costs the same to walk as a file. Gated on
	// inv.Files it would bound nothing on a tree of directories or of
	// files Consider rejects — the guarantee would hold only for
	// today's callers, both of which happen to pass a nil Consider
	// (CodeRabbit on #940).
	//
	// Only a REPORTING caller may set it. A sweep that deletes on a
	// truncated inventory would be deleting on a ratio measured from part
	// of the tree, which is the confident-wrong-answer shape this package
	// keeps paying for.
	MaxEntries int
	// MaxOrphanPaths caps how many orphan paths are retained; 0 keeps all
	// of them. Orphans is counted either way.
	MaxOrphanPaths int
}

// TakeSidecarInventory walks root once and classifies every file against
// known (build it with KnownSidecarSet, so a relocated catalog's canonical
// spellings are in it).
//
// Dot-directories below the root are pruned, the rule every sidecar walk
// in this tree follows: with a variants directory on its own volume,
// `.Trashes/<uid>/` and `.Trash-1000/` sit under the walk root, and the
// files inside are ones an operator put in the Trash to get back. The
// prune is gated on d.IsDir() because SkipDir returned for a FILE skips
// the rest of its parent directory and would end the walk early.
//
// The root is RESOLVED before the walk (resolveSidecarRoot) and paths are
// REPORTED under the configured one. Both halves are load-bearing:
// unresolved, a symlinked variants directory is one non-directory entry
// that `upscale --gc`'s nil Consider classifies as an orphan file and
// unlinks; reported resolved, every key in a KnownSidecarSet built from
// the configured dir would miss and a healthy tree would read as orphans.
// A symlinked subdirectory is skipped rather than classified, for the
// first reason one level down.
//
// A missing root is an empty inventory and no error — a bridge that never
// transcoded anything has no directory, and both sweeps have always
// treated that as nothing to do; a DANGLING root reads the same way, and
// refusing the sweep on it is the directory check's job. Any other walk
// error aborts and is returned: a tree that cannot be read is not
// evidence its files are junk, and it is the same fail-closed reading
// TreeHoldsVariantSidecars takes. A directory that cannot be DESCENDED
// into is the softer case — see SidecarInventory.Unreadable.
func TakeSidecarInventory(ctx context.Context, root string, known map[string]struct{}, opts SidecarInventoryOptions) (SidecarInventory, error) {
	var (
		inv SidecarInventory
		// traversed counts every entry the walk is handed, which is what
		// MaxEntries bounds — see SidecarInventoryOptions.MaxEntries.
		traversed int
	)
	if root == "" {
		// WalkDir("") walks the process working directory — the
		// ReapOrphans rule. Nothing to inventory is not "inventory
		// everything under the cwd".
		return inv, fmt.Errorf("integrity: no sidecar directory")
	}
	// Resolve before walking — see resolveSidecarRoot. Unresolved, a
	// symlinked variants directory is handed to the callback as one
	// non-directory entry, classified as an orphan FILE, and unlinked by
	// the forward sweep.
	//
	// ENOENT keeps the reading a missing root has always had here
	// ("nothing to inventory"), and that covers a DANGLING symlink too:
	// there is no tree to classify, and refusing the sweep is the
	// directory check's job (VariantsDirSweepBlock), not this walk's. Any
	// other error fails closed — and crucially neither branch falls back
	// to walking `root` unresolved, which is the defect itself.
	walkRoot, resolveErr := resolveSidecarRoot(root)
	if resolveErr != nil {
		if errors.Is(resolveErr, fs.ErrNotExist) {
			return inv, nil
		}
		return SidecarInventory{}, fmt.Errorf("integrity: resolve sidecar directory %q: %w", root, resolveErr)
	}
	// Paths are REPORTED under the configured root, not the resolved one.
	// KnownSidecarSet keys on the recorded sidecar_path and on
	// CanonicalSidecarPath(variantsDir, …) — both in the configured
	// spelling — so emitting resolved paths would miss every one of them
	// and classify an entire healthy tree as orphans. Cheap because
	// WalkDir builds each path by joining onto walkRoot, so the prefix is
	// exact; a no-op when nothing was a symlink.
	reportPath := func(p string) string {
		return root + strings.TrimPrefix(p, walkRoot)
	}
	walkErr := filepath.WalkDir(walkRoot, func(path string, d fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			// The root itself missing is "nothing to do"; a directory
			// below it that cannot be read is counted and stepped over,
			// because its absence from the counts can only shrink what a
			// caller goes on to delete.
			if errors.Is(walkErr, fs.ErrNotExist) {
				if path == walkRoot {
					return filepath.SkipDir
				}
				return nil
			}
			if d != nil && d.IsDir() {
				inv.Unreadable++
				return nil
			}
			return walkErr
		}
		// The budget is spent BEFORE any classification, so a tree of
		// directories or of files Consider rejects costs the same as one
		// of candidates. inv.Files stays the count of CLASSIFIED files, so
		// Known, Orphans and the mass-orphan ratio are unchanged.
		if opts.MaxEntries > 0 {
			if traversed >= opts.MaxEntries {
				inv.Truncated = true
				return errInventoryBudget
			}
			traversed++
		}
		if d.IsDir() {
			if path != walkRoot && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		// A symlink that points at a DIRECTORY is not a candidate file.
		// WalkDir does not descend into it (it Lstats), so unresolved it
		// would arrive here as one non-directory entry and — under a nil
		// Consider — be unlinked as an orphan, taking an album an
		// operator parked on another volume with it. Skipped rather than
		// followed: descending would raise the cycle question, and the
		// files under it are not this tree's to reclaim. A symlink to a
		// regular FILE still counts, the #207 broken-link rule
		// TreeHoldsVariantSidecars follows.
		if d.Type()&fs.ModeSymlink != 0 {
			if info, statErr := os.Stat(path); statErr == nil && info.IsDir() {
				return nil
			}
		}
		name := d.Name()
		if opts.Scratch != nil && opts.Scratch(name) {
			inv.ScratchPaths = append(inv.ScratchPaths, reportPath(path))
			return nil
		}
		if opts.Consider != nil && !opts.Consider(name) {
			return nil
		}
		inv.Files++
		reported := reportPath(path)
		if _, ok := known[strings.ToLower(filepath.Clean(reported))]; ok {
			inv.Known++
			return nil
		}
		inv.Orphans++
		if opts.MaxOrphanPaths == 0 || len(inv.OrphanPaths) < opts.MaxOrphanPaths {
			inv.OrphanPaths = append(inv.OrphanPaths, reported)
		}
		return nil
	})
	switch {
	case errors.Is(walkErr, errInventoryBudget):
		return inv, nil
	case walkErr != nil:
		return SidecarInventory{}, walkErr
	}
	return inv, nil
}

// errInventoryBudget stops the walk at MaxEntries; it never escapes
// TakeSidecarInventory.
var errInventoryBudget = errors.New("integrity: inventory budget reached")

// MassOrphanLowerBound reports whether the two terms of MassOrphanRefusal
// that SURVIVE A PARTIAL WALK hold: the deletion clears the floor, and
// there are more unreferenced files than the catalog has rows in total.
//
// Both are monotone in the walk. `orphans` can only grow as more of the
// tree is seen, and `rows` is the whole catalog either way, so a prefix
// that satisfies them proves the completed walk does too. The RATIO term
// is not, so a caller that walked under a budget may state THIS and may
// not state what MassOrphanRefusal as a whole will decide (CodeRabbit on
// #940).
//
// The referenced files are NOT bounded by the row count, which is what
// makes the divergence reachable at the DEFAULT threshold rather than an
// exotic one. KnownSidecarSet folds its keys to lower case while this
// walk counts FILES, so on a case-sensitive filesystem any number of
// case-variant sidecars collapse onto one row's key and are all counted
// Known — the deliberate false-keep that fold is documented as costing.
// An earlier draft of this docblock reasoned from `known <= 2*rows` and
// concluded the two terms could only disagree above 34%; swept without
// that assumption they disagree from 3% up (CodeRabbit on #940). The
// measurement was of a constrained space and was written down as a
// general bound, which is the trap this repo records as "a negative
// result is about the thing you measured".
//
// Exported so `bridge doctor`'s bounded probe has one place to ask the
// question rather than a second copy of the floor.
func MassOrphanLowerBound(orphans, rows int) bool {
	return orphans >= massOrphanFloor && orphans > rows
}

// massOrphanFloor is the smallest number of unreferenced files the
// forward guard will ever refuse, the twin of massDeleteFloor and set to
// the same ten for the same reason: below it the ratio says nothing, and
// the cost of being wrong is a handful of sidecars rather than a library.
const massOrphanFloor = 10

// MassOrphanRefusal decides whether a FORWARD sweep that found `orphans`
// unreferenced files among `files` candidates, against a catalog of `rows`
// rows, should be refused, and says why. Empty means proceed.
//
// It is the ONE decision the file-deleting sweeps make, as
// MassDeleteRefusal is for the row-deleting ones, and it asks a different
// question on purpose. Three terms, all of which must hold:
//
//   - `orphans >= massOrphanFloor`. Ten is the smallest "mass".
//
//   - `orphans > rows`. This is the term that knows what a lost index
//     looks like. A healthy catalog references about one file per row, so
//     a tree holding MORE unreferenced files than the catalog has rows AT
//     ALL is a tree the catalog has stopped describing. The ordinary
//     reasons for a big `--gc` do not have this shape: a naming-scheme
//     change leaves one old file per current row, and an interrupted bulk
//     delete leaves one per deleted row against the rows that remain.
//
//   - `orphans*100 > maxOrphanPercent*files`. The same knob, with the
//     same meaning at both ends, as the reverse guard —
//     `integrity.variantSweepMaxDeletePercent`: 100 disables (orphans can
//     never exceed files), 0 refuses any mass orphan removal. One number
//     an operator sets once should not mean "protect my rows" and leave
//     the files, which are the half that cannot be re-adopted, unguarded.
//
// Deliberately NOT gated on TreeHoldsVariantSidecars, unlike its reverse
// twin: that probe exists to tell a relocation from a real deletion, and
// here the files ARE the evidence — they are in hand, counted, and about
// to be unlinked.
//
// The reason names the numbers so the caller's refusal tells the operator
// what was seen rather than that something was refused.
func MassOrphanRefusal(orphans, files, rows, maxOrphanPercent int) string {
	if files <= 0 || !MassOrphanLowerBound(orphans, rows) {
		return ""
	}
	if orphans*100 <= maxOrphanPercent*files {
		return ""
	}
	return fmt.Sprintf("%d of %d file(s) on disk (%d%%) are referenced by no row, and that is more than the %d row(s) the catalog holds in total, over the %d%% threshold",
		orphans, files, orphans*100/files, rows, maxOrphanPercent)
}
