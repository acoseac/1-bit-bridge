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
// 254 GiB — with its empty-catalog refusal (now `gcRefuseEmptyCatalog`)
// satisfied, because 200 is not zero, and with the relocation guard
// silent, because every one of those 200 rows had its file exactly where
// it said.
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
	// MaxOrphanPaths (all of them when that is 0), in the CONFIGURED
	// spelling: the one the known set keys on and every report prints.
	// The `--gc` sweeps ask for all of them, since they are about to
	// unlink exactly these files and a second walk could see a different
	// tree. The doctor asks for a handful, to name examples in its hint.
	// A caller that unlinks uses OrphanWalkedPaths, never these.
	OrphanPaths []string
	// OrphanWalkedPaths holds the same orphans, in the same order, as the
	// walk visited them: under the RESOLVED root. OrphanWalkedPaths[i] is
	// the file OrphanPaths[i] names, and it is what a sweep unlinks. The
	// two differ only where the configured root, or a directory above it,
	// is a symlink, and there the difference is the point: the link can
	// be repointed between the walk and the unlinks (an operator moving
	// the variants alias to another volume while the bridge runs), and an
	// unlink through it then removes files in a tree the walk never
	// counted, past the mass-orphan refusal, whose verdict was taken over
	// the first tree (CodeRabbit on #1063).
	OrphanWalkedPaths []string
	// ScratchPaths holds every scratch file, uncapped: the callers that
	// ask for them remove them whatever the catalog says, once they are
	// older than the grace (ReclaimOrphan, OrphanGracePeriod: a scratch
	// file younger than that may be a live job's, and was removed
	// unconditionally until backlog B205). They are in the configured
	// spelling, and ScratchWalkedPaths is to them what OrphanWalkedPaths
	// is to OrphanPaths.
	ScratchPaths       []string
	ScratchWalkedPaths []string
	// Unreadable counts entries the walk could not resolve: a directory
	// it could not descend into, and a NON-REGULAR entry it could not
	// stat (a symlink, a Windows junction — so it cannot know whether
	// the target is a directory whose only reference this is). Both are
	// missing from every count above. That shrinks the list of files a
	// sweep could unlink (the known set comes from the database, not
	// from the walk), and it does NOT make a sweep's verdict safe, which
	// this docblock claimed until 2026-09-28: MassOrphanRefusal weighs
	// the whole tree, and a directory the walk could not list may hold
	// any number of orphans, so a verdict that proceeds over the part
	// the walk saw can be a refusal over the whole. So every deleting
	// sweep asks MassOrphanRefusalFor and PartialWalkRefusal, which weigh
	// the two kinds of entry differently (UnlistedDirs has why). A report
	// built from a partial tree should say so.
	//
	// It is therefore a count of ENTRIES, not of directories, and the
	// two CLI sweeps that print it say so: the message named directories
	// and their contents, which was already imprecise for an unstattable
	// link and is plainly wrong for a junction (CodeRabbit on #969).
	//
	// One unreadable directory is not counted: the `lost+found` directly
	// under the walk root that a permission error keeps this user out of
	// (IsFilesystemLostFound).
	Unreadable int
	// UnlistedDirs is how many of the Unreadable entries are DIRECTORIES
	// the walk could not list. Each may hold any number of files, so no
	// count taken over the rest of the tree bounds what it hides: a
	// verdict needs the walk to see it (PartialWalkRefusal). The other
	// Unreadable entries, a link or a junction the walk could not stat,
	// are never walked into, so each can be at most ONE candidate file,
	// itself, and MassOrphanRefusalFor weighs them exactly.
	UnlistedDirs int
	// Truncated is true when the walk stopped at MaxEntries with more of
	// the tree unseen. A caller that deletes must not truncate; a caller
	// that reports must scope its claim to what it looked at.
	Truncated bool
}

// ErrUnpairedInventory is what CheckPaired returns for an inventory whose
// listed and walked paths do not pair up one for one.
var ErrUnpairedInventory = errors.New("integrity: the inventory's listed and walked paths do not pair up")

// CheckPaired returns ErrUnpairedInventory, with the counts, unless every
// listed path has its walked path beside it: OrphanWalkedPaths as long as
// OrphanPaths, and ScratchWalkedPaths as long as ScratchPaths.
// TakeSidecarInventory appends to both halves of a pair together, so an
// inventory it returned always passes; one built or trimmed by hand may
// not. Each deleting sweep asks before it unlinks anything and refuses an
// unpaired inventory whole (Gemini on #1063). Both alternatives are
// worse: indexing past the shorter list panics, which in `bridge serve` is
// the background sweep's goroutine taking the process down, and unlinking
// by the listed spelling where a walked path is missing is the defect the
// walked paths exist to close. A length is all it can check: two lists of
// one length in different orders would pass, and nothing builds such a
// pair.
func (inv SidecarInventory) CheckPaired() error {
	if len(inv.OrphanWalkedPaths) != len(inv.OrphanPaths) {
		return fmt.Errorf("%w: %d orphan path(s), %d walked", ErrUnpairedInventory,
			len(inv.OrphanPaths), len(inv.OrphanWalkedPaths))
	}
	if len(inv.ScratchWalkedPaths) != len(inv.ScratchPaths) {
		return fmt.Errorf("%w: %d scratch path(s), %d walked", ErrUnpairedInventory,
			len(inv.ScratchPaths), len(inv.ScratchWalkedPaths))
	}
	return nil
}

// OrphanIsRendition reports whether the i-th orphan the inventory listed
// is a rendition by name, the rule the mount-loss probe counts by
// (looksLikeVariantSidecar): a file whose removal by a forward sweep can
// leave a variants directory holding no rendition. `upscale --gc`'s
// forward sweep counts the ones it unlinks, which its reverse guard asks
// before it reads such a directory as one this run emptied (backlog
// B223): a run that removed only files that are not renditions, a
// .DS_Store say, found the directory holding none already, which is what
// an unmounted volume looks like.
func (inv SidecarInventory) OrphanIsRendition(i int) bool {
	return looksLikeVariantSidecar(filepath.Base(inv.OrphanPaths[i]))
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
// into is the softer case — see SidecarInventory.Unreadable and
// UnlistedDirs — except the filesystem's own `lost+found` at the top of
// the walk root, which is not counted at all (IsFilesystemLostFound).
func TakeSidecarInventory(ctx context.Context, root string, known map[string]struct{}, opts SidecarInventoryOptions) (SidecarInventory, error) {
	var (
		inv SidecarInventory
		// traversed counts every entry the walk is handed, which is what
		// MaxEntries bounds — see SidecarInventoryOptions.MaxEntries.
		traversed int
	)
	if root == "" {
		// Refused BEFORE resolveSidecarRoot, which is what makes this
		// load-bearing: filepath.EvalSymlinks("") answers "." (measured
		// with go1.26.6 on macOS, Linux and Windows), so an unguarded ""
		// would inventory the working directory and hand it to a sweep that
		// unlinks. WalkDir("") alone only reports ENOENT. Nothing to
		// inventory is not "inventory everything under the cwd".
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
	// exact; a no-op when nothing was a symlink. Each listed file keeps
	// its walked path too, and that is the one a sweep unlinks (see
	// SidecarInventory.OrphanWalkedPaths).
	reportPath := func(p string) string {
		return root + strings.TrimPrefix(p, walkRoot)
	}
	walkErr := filepath.WalkDir(walkRoot, func(path string, d fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			// The root itself missing is "nothing to do"; a directory
			// below it that cannot be read is counted and stepped over.
			// Its absence from the counts can only shrink the LIST a
			// caller goes on to delete from, not make the caller's
			// verdict sound, so it is counted in UnlistedDirs, which
			// PartialWalkRefusal reads.
			if errors.Is(walkErr, fs.ErrNotExist) {
				if path == walkRoot {
					return filepath.SkipDir
				}
				return nil
			}
			if d != nil && d.IsDir() {
				if IsFilesystemLostFound(walkRoot, path, d, walkErr) {
					return nil
				}
				inv.Unreadable++
				inv.UnlistedDirs++
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
		// A link that points at a DIRECTORY is not a candidate file.
		// WalkDir does not descend into it (it Lstats), so unresolved it
		// would arrive here as one non-directory entry and — under a nil
		// Consider — be unlinked as an orphan, taking an album an
		// operator parked on another volume with it. Skipped rather than
		// followed: descending would raise the cycle question, and the
		// files under it are not this tree's to reclaim. A link to a
		// regular FILE still counts, the #207 broken-link rule
		// TreeHoldsVariantSidecars follows.
		//
		// The test is "not a REGULAR file", not "is a symlink", because
		// a Windows directory JUNCTION (`mklink /J`,
		// IO_REPARSE_TAG_MOUNT_POINT) is neither. Since Go 1.23's
		// winsymlink change, os.Lstat gives a name-surrogate reparse
		// point ModeIrregular and withholds ModeDir — so a junction has
		// IsDir() false and no ModeSymlink bit, fell through every arm
		// here, and was classified as an orphan file. A junction is the
		// ORDINARY way to park an album on another volume on Windows: a
		// real symlink needs a privilege a service account usually does
		// not have. One of them is below the mass-orphan floor of ten,
		// so no guard could see it, and os.Remove takes the junction
		// while its target's files stay behind with nothing pointing at
		// them.
		switch classifyWalkEntry(d.Type(), func() (fs.FileInfo, error) { return os.Stat(path) }) {
		case walkEntrySkip:
			return nil
		case walkEntryUnreadable:
			inv.Unreadable++
			return nil
		}
		name := d.Name()
		if opts.Scratch != nil && opts.Scratch(name) {
			inv.ScratchPaths = append(inv.ScratchPaths, reportPath(path))
			inv.ScratchWalkedPaths = append(inv.ScratchWalkedPaths, path)
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
			inv.OrphanWalkedPaths = append(inv.OrphanWalkedPaths, path)
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

// walkEntryVerdict is what the inventory does with one non-directory
// entry the walk handed it.
type walkEntryVerdict uint8

const (
	// walkEntryClassify — an ordinary candidate: Scratch, Consider and
	// the known-set lookup decide from here.
	walkEntryClassify walkEntryVerdict = iota
	// walkEntrySkip — a link whose target is a DIRECTORY. Not a file,
	// and unlinking it would take a subtree's only reference.
	walkEntrySkip
	// walkEntryUnreadable — the target could not be determined. "Not
	// there" and "could not find out" are different questions and only
	// the first is junk.
	walkEntryUnreadable
)

// classifyWalkEntry decides what a non-directory walk entry is, from its
// Lstat MODE and a stat of its target.
//
// A plain file answers immediately and the stat is never taken — this
// runs once per entry on a tree that can hold 100k of them, and
// WalkDir's own Lstat already said what it is.
//
// Everything else is stat'd, and the test is "not a REGULAR file"
// rather than "is a symlink" because the shapes that matter are not all
// symlinks. A Windows directory JUNCTION (`mklink /J`,
// IO_REPARSE_TAG_MOUNT_POINT) is the live one: since Go 1.23's
// winsymlink change, os.Lstat gives a name-surrogate reparse point
// ModeIrregular and withholds ModeDir, so a junction reports IsDir()
// false with no ModeSymlink bit. It fell through every arm and was
// classified as an orphan FILE — and a junction is the ordinary way to
// park an album on another volume there, because a real symlink needs a
// privilege a service account usually does not have. ONE of them is
// below the mass-orphan floor of ten, so no guard could see it, and
// os.Remove takes the junction while its target's files stay behind
// with nothing pointing at them.
//
// A DANGLING link classifies (ErrNotExist): it is junk in this tree and
// reclaiming it is the sweep's job. A permission or I/O error is not
// evidence about the target at all, and classifying it would let the
// forward sweep remove the only reference to a subtree (CodeRabbit on
// #959). The other non-regular POSIX kinds — a FIFO, a socket, a device
// node — stat to themselves, so they classify exactly as before; only a
// DIRECTORY target changes the answer.
//
// Taken as a function of (mode, stat) rather than inline because the
// Windows shape cannot be constructed on any other platform, and a
// guard that only runs on one CI leg looks exactly like one that passed.
func classifyWalkEntry(mode fs.FileMode, stat func() (fs.FileInfo, error)) walkEntryVerdict {
	if mode.IsRegular() {
		return walkEntryClassify
	}
	info, err := stat()
	switch {
	case err == nil && info.IsDir():
		return walkEntrySkip
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return walkEntryUnreadable
	}
	return walkEntryClassify
}

// IsFilesystemLostFound reports whether a directory the walk could not
// list is the `lost+found` of the filesystem mounted AT the walk root:
// named exactly that, directly under the (resolved) walk root, and kept
// from this user by a PERMISSION error.
//
// Exported because three walks read that directory, and they must read it
// the same way (2026-09-28): TakeSidecarInventory does not count it,
// TreeHoldsVariantSidecars (the reverse guard's probe) takes it as no
// evidence either way, and `bridge artwork --gc` steps over it without a
// word, on a cache directory that is such a volume's mount root. Only the
// first read it until then; the probe answered its permission error, and
// the artwork GC stopped there with exit 1.
//
// mke2fs creates that directory at the root of every ext2/3/4 filesystem,
// owned by root with mode 0700, and fsck puts the inodes it recovers
// there under names like `#12345`. So a variants directory that IS an
// ext4 volume's mount point, the ordinary way to give renditions a disk of
// their own, holds one the bridge's service user can never list. Counted
// as an unlisted directory it made every tick of the background sweep
// refuse, forever, and every CLI `--gc` on such a host a partial walk; and
// `bridge doctor` warned about it on every run. Nothing the bridge writes
// can be in it: the sweeps' layout mirrors library-relative paths, so a
// `<variantsDir>/lost+found` the bridge wrote is one a single-root library
// with a top-level folder of that name, or (multi-root) a library root
// whose basename it is, made the bridge create, as its own user, which
// can list it; such a directory is walked as before. The one way sidecars
// could sit in a directory meeting all three terms is a render run as
// ROOT for a library folder named `lost+found` (root can write into the
// volume's 0700 one, or create its own under a restrictive umask), and
// that is left as the residual it is.
//
// The artwork cache holds nothing of that name either: its layout is flat
// `<key>-<size>.jpg` and `artist-*.jpg` files and one `thumbs/` directory.
//
// Only a permission error: an I/O error on it is a fault like any other,
// and a lost+found deeper in the tree (a volume mounted INSIDE the variants
// directory) still counts, because nothing about the walk root vouches for
// it. Directories only, which the caller has checked: d is the non-nil
// entry the walk handed over with err. walkRoot is compared cleaned,
// because filepath.WalkDir joins, and so cleans, every path below its root.
func IsFilesystemLostFound(walkRoot, path string, d fs.DirEntry, err error) bool {
	return d.Name() == "lost+found" &&
		filepath.Dir(path) == filepath.Clean(walkRoot) &&
		errors.Is(err, fs.ErrPermission)
}

// MassOrphanRefusalFor is MassOrphanRefusal over an inventory, with the
// entries its walk could not stat weighed as what they could be.
//
// Such an entry (Unreadable − UnlistedDirs: a link or a junction whose
// target the walk could not stat) is never walked into, so it is either
// nothing to this sweep, one file a row references, or one orphan. A file
// a row references adds to the files alone, which can only lower the
// ratio. An orphan adds to both counts, which can only raise the refusal:
// the floor and `orphans > rows` grow, and 100·(o+1) > pct·(f+1) follows
// from 100·o > pct·f whenever pct ≤ 100. So counting all k of them as
// orphans (o+k of f+k) refuses exactly when SOME reading of them would,
// and proceeds only when none would. That is the whole of what these
// entries can hide; a directory the walk could not list can hide any
// number of files, which is PartialWalkRefusal's case.
//
// The reason says when the count includes them, because its numbers are
// then a worst case rather than what the walk saw.
func MassOrphanRefusalFor(inv SidecarInventory, rows, maxOrphanPercent int) string {
	unstatted := inv.Unreadable - inv.UnlistedDirs
	if unstatted < 0 {
		// Only an inventory built by hand can say this; it has no bounded
		// unknowns to weigh.
		unstatted = 0
	}
	reason := MassOrphanRefusal(inv.Orphans+unstatted, inv.Files+unstatted, rows, maxOrphanPercent)
	if reason != "" && unstatted > 0 {
		reason += fmt.Sprintf(", counting the %d entr(y/ies) the walk could not stat as unreferenced files", unstatted)
	}
	return reason
}

// EmptyCatalogOrphans is the empty-catalog refusal's one rule: how many
// files a forward sweep whose known set is EMPTY would read as orphans in
// inv, and how many of those are entries the walk could not stat. A sweep
// refuses, without an operator's say-so, whenever orphans is not zero;
// both are zero when the known set holds anything, since the rule is only
// about an empty one. The background sweep (emptyCatalogRefusal) and
// `upscale --gc` and `analyze --gc` (cmd/bridge's gcRefuseEmptyCatalog)
// all decide by it, so they cannot disagree about what an empty catalog
// puts at risk.
//
// With no row naming a sidecar, every file the walk classified (inv.Files:
// the files the sweep's Consider takes) is an orphan, and each entry the
// walk could not stat is weighed as one more, as MassOrphanRefusalFor
// weighs it: the sweep refuses when some reading of the tree holds a file
// it would remove, below the mass-orphan floor too. Nothing else counts,
// because nothing else is a file the sweep would remove as an orphan: a
// directory, empty or not, a file its Consider rejects, anything under a
// pruned dot-directory, the filesystem's lost+found, and a scratch file,
// which the sweep that has any removes whatever the catalog says (once it
// is older than the grace, OrphanGracePeriod), so an empty catalog puts
// none at risk. What the Consider takes is each sweep's
// own: the background sweep takes `.flac` files and `analyze --gc`
// waveforms, while `upscale --gc` takes EVERY file, so a `.DS_Store` in the
// variants directory is a file it would remove and still refuses there.
//
// A directory the walk could not LIST is not weighed: it may hold sidecars
// or nothing, and the walk reached no file to count. A verdict over part
// of the tree is PartialWalkRefusal's.
//
// Until 2026-09-28 the background sweep, and until 2026-09-29 the CLI
// sweeps, asked instead whether the directory held any entry at all
// (VariantsDirSweepBlockReason's one-entry read), so a directory holding
// only empty folders, a .DS_Store or the filesystem's lost+found refused a
// sweep that had nothing to remove (backlog B42, B65).
func EmptyCatalogOrphans(inv SidecarInventory, known int) (orphans, unstatted int) {
	if known > 0 {
		return 0, 0
	}
	unstatted = inv.Unreadable - inv.UnlistedDirs
	if unstatted < 0 {
		// Only an inventory built by hand can say this, as for
		// MassOrphanRefusalFor.
		unstatted = 0
	}
	return inv.Files + unstatted, unstatted
}

// PartialWalkRefusal decides whether a forward sweep must refuse to take
// its mass-orphan verdict from an inventory whose walk could not list part
// of the tree, and says why. Empty means proceed.
//
// A directory the walk could not list may hold any number of orphans, so
// a verdict that proceeds over the rest can be a refusal over the whole:
// 20 live files and 15 stranded in view against 20 rows pass
// MassOrphanRefusal, while the 1,000 stranded behind the directory make the
// whole tree refuse, and both `--gc` sweeps unlinked the 15 until
// 2026-09-28. Every deleting sweep asks this after MassOrphanRefusalFor,
// whose advice is the more urgent when the part the walk saw already
// refuses (its floor and `orphans > rows` only grow as more is seen).
//
// Nothing to refuse when the verdict cannot refuse at all: at a threshold
// of 100 (the knob that disables the mass-orphan guard) no count, however
// much the directory hides, makes MassOrphanRefusal refuse. Whether a
// caller may WAIVE the refusal is the caller's: the background sweep never
// does, and the CLI sweeps take `--allow-partial-walk`, or pass on it when
// `--allow-mass-orphans` has already set the verdict aside.
func PartialWalkRefusal(inv SidecarInventory, rows, maxOrphanPercent int) string {
	if inv.UnlistedDirs == 0 || maxOrphanPercent >= 100 {
		return ""
	}
	return fmt.Sprintf("the walk could not list %d director(y/ies), so its %d orphan(s) of %d file(s) against %d row(s) describe part of the tree",
		inv.UnlistedDirs, inv.Orphans, inv.Files, rows)
}

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
