package manifest

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// The console's writes into a library root that reads as a volume not mounted
// (backlog B776). Where the bare mount point a volume leaves is the service
// user's to write, a write while the volume is away leaves library content
// there: an upload's folders and files, a restored track. The scans then read
// that as the root's content, so the clean-empty guard no longer spares the
// root (emptyRootMustBeSpared), and the rows the volume's files have go
// missing and are reaped at the threshold; and the volume, once it is mounted
// again, hides what was written. An upload staged there is hidden with it,
// and its session reads as lost. So the console's writers (the upload's
// staging and its commit, the trash's restore) ask, before they write
// anything, the look the watcher holds a root by (Scanner.lookAtRoot) of the
// deepest configured root that owns each path they would write, and a request
// one of whose roots reads as absent is refused whole, nothing written, in the
// hold's words. Only rows are at stake, never a file: nothing the console
// writes deletes one.
//
// A root the index holds nothing of (a fresh install whose volume is not
// mounted yet) reads as it is, as it does for the watcher, so a write there
// lands on the bare mount point, and the mount then hides it. A root whose
// path is not there at all reads as unreachable, and a write may make its
// folder only where the index holds none of its rows (firstWriteMakesRoot):
// the first upload into a root configured before its folder was made, which a
// public serve starts over. One case stays open: backlog B776, item 7.

// ErrRootNotMounted is what a write refused under a library root that reads as
// a volume not mounted matches (errors.Is): every *RootAwayError.
var ErrRootNotMounted = errors.New("library root " + readsAsNotMounted)

// RootAwayError refuses a write under a library root that reads as a volume
// not mounted, or as one holding nothing but links to directories over the
// tracks the index holds (rootOnlyDirLinks), whose rows the scan spares as it
// spares an emptied root's: what lookAtRoot reads as absent.
type RootAwayError struct {
	// Root is the configured root, as configured.
	Root string
	look rootLook
}

func (e *RootAwayError) Error() string {
	return fmt.Sprintf("%v: %s (reads=%s)", ErrRootNotMounted, e.Root, e.look.reason)
}

func (e *RootAwayError) Unwrap() error { return ErrRootNotMounted }

// Reason is how the root read (the rootAbsent constants, or rootOnlyDirLinks),
// as the watcher's held line gives it (reads=).
func (e *RootAwayError) Reason() string { return e.look.reason }

// Message is the refusal as the console says it: the hold's own words
// (msgWatchRootHeld and heldHint), that nothing was written, and what to do.
// It names the root by its folder's name, as the app names a root's tracks,
// and holds no path: a console error body names none (admin-console.md).
func (e *RootAwayError) Message() string {
	name := filepath.Base(filepath.Clean(e.Root))
	if e.look.reason == rootOnlyDirLinks {
		return fmt.Sprintf("the library root %q %s, so nothing was written to it: a file written there "+
			"would let the scans delete the rows kept under those links. To index what a link leads to, "+
			"add that directory as a library root of its own", name, holdsOnlyDirLinks)
	}
	return fmt.Sprintf("the library root %q %s, so nothing was written to it: %s, then try again "+
		"(the scan keeps the tracks indexed under it; %s)", name, readsAsNotMounted, hintVolumeMounted, hintContainerRestart)
}

// RefuseWritesUnder looks at the configured roots that own each of paths,
// absolute paths a request would write, and answers a *RootAwayError for the
// first that reads as absent (lookAtRoot), nil when none does, each root
// looked at once. A path has two owners, which may be one root: the deepest
// root that holds it as its elements spell it, by which root a path is
// (rootIn: the containment a subtree scan takes its owning root by, as the
// volume compares names; a nested root's folder written through the path of
// the root it is nested in is the nested root's), and the deepest that holds
// it where the write lands (landingRoots.owners). rootIn resolves no link,
// while a write's MkdirAll and rename follow one in its folders, and the
// upload's and the trash's own containment checks resolve it: a link in a
// healthy root to the folder of a root whose volume is away took a write
// into that bare mount point (B776's round 2). A path under no configured
// root asks nothing: the caller's own containment refuses it. A look is a
// stat, one batch of the root's listing and, for a root holding nothing, one
// count of its rows, and where a write lands a few lstats, the other roots
// resolved only where a link is on its way, so a healthy write pays a few of
// each and a scan none: rootIn is the scans' and the watcher's, and is left as
// it is.
func (s *Scanner) RefuseWritesUnder(ctx context.Context, paths []string) error {
	roots := s.Roots()
	landing := newLandingRoots(roots)
	looked := make(map[string]bool)
	for _, p := range paths {
		owners := landing.owners(s, p)
		if root, _ := s.rootIn(roots, p); root != "" {
			owners = append([]string{root}, owners...)
		}
		for _, root := range owners {
			if looked[root] {
				continue
			}
			looked[root] = true
			if look := s.lookAtRoot(ctx, root); look.absent && !s.firstWriteMakesRoot(ctx, root, look) {
				return &RootAwayError{Root: root, look: look}
			}
		}
	}
	return nil
}

// firstWriteMakesRoot reports whether a write may make the folder of a root
// that reads as unreachable (B776's round 2): only where nothing at all is at
// its path (os.Lstat answers that it does not exist, not a permission refused,
// an I/O error or a link to nothing) and the index's count of its rows answers
// none. That is a root configured before its folder was made, which a public
// serve starts over (a loopback one refuses an inaccessible root at its start),
// or one whose folder went while it held nothing. A root the index holds rows
// under is refused (a removed mount point, a renamed folder: made anew, its
// folder would hold the upload alone, and the scans would reap its rows), and
// so is one whose count fails. Only the write's admission asks this: what the
// root reads as for the scans and the watcher (lookAtRoot) is unchanged.
func (s *Scanner) firstWriteMakesRoot(ctx context.Context, root string, look rootLook) bool {
	if look.reason != rootAbsentUnreachable {
		return false
	}
	if _, err := os.Lstat(filepath.Clean(root)); !errors.Is(err, fs.ErrNotExist) {
		return false
	}
	n, err := s.store.CountTracksUnderRoot(ctx, root, len(s.Roots()) > 1)
	return err == nil && n == 0
}

// landingRoots is the configured roots in the form where a write lands is
// compared with, each resolved as that is (fsutil.EvalSymlinksOrClean): its
// links resolved, and on Windows its junctions, which a write follows alike,
// so a write through a link is found under a root configured through one; and
// a root whose path is not there resolved through its nearest folder that is,
// its missing names kept, so a write that would make that root's folder is
// found under it. Kept as it was configured, such a root was compared in
// another form than the write wherever a link or another spelling leads to its
// parent (backlog B794): a root nested in a library root configured through a
// link, and on CI's hosts every root a test makes, under macOS's temporary
// folder, reached through /var, a link to /private/var, or a Windows runner's,
// named by its short name. The configured roots a form stands for keep their
// names for the look, which counts the rows by them. The forms are made only
// once a path's folder is found to resolve elsewhere, so a write with no link
// on its way waits on no other root's volume (a network mount that hangs); the
// folders a request names are resolved once each.
type landingRoots struct {
	configured []string
	resolved   bool
	forms      []string
	roots      map[string][]string
	dirs       map[string]string
}

// newLandingRoots is roots, to be compared with where a write lands.
func newLandingRoots(roots []string) *landingRoots {
	return &landingRoots{configured: roots, dirs: make(map[string]string)}
}

// resolve makes the forms, once.
func (l *landingRoots) resolve(s *Scanner) {
	if l.resolved {
		return
	}
	l.resolved = true
	l.roots = make(map[string][]string)
	resolveRoot := fsutil.EvalSymlinksOrClean
	if h := s.resolveRootHook; h != nil {
		resolveRoot = h
	}
	for _, r := range l.configured {
		abs, err := filepath.Abs(r)
		if err != nil {
			continue
		}
		form := resolveRoot(abs)
		if _, seen := l.roots[form]; !seen {
			l.forms = append(l.forms, form)
		}
		l.roots[form] = append(l.roots[form], r)
	}
}

// owners is the configured roots that hold p where a write to it lands: its
// folder resolved through its nearest ancestor that exists
// (fsutil.EvalSymlinksOrClean), its own name kept, since a rename replaces a
// link at that name rather than follow it; the deepest form that holds that,
// compared element by element as which root a path is compares
// (deepestRootOf), and nil for none. Where the folder resolves to itself,
// nothing on the way is a link, the write lands where p's elements say, and
// its owner is rootIn's, which is the scans' (nestedRootsUnder compares the
// roots as configured too): nil.
func (l *landingRoots) owners(s *Scanner, p string) []string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return nil
	}
	dir := filepath.Dir(abs)
	resolved, ok := l.dirs[dir]
	if !ok {
		resolved = fsutil.EvalSymlinksOrClean(dir)
		l.dirs[dir] = resolved
	}
	if resolved == dir {
		return nil
	}
	l.resolve(s)
	form, _, _ := s.deepestRootOf(l.forms, filepath.Join(resolved, filepath.Base(abs)))
	return l.roots[form]
}

// WriteGuard is RefuseWritesUnder on ctx, in the form the console's writers
// take it (upload.WithRootGuard, trash.WithRootGuard).
func (s *Scanner) WriteGuard(ctx context.Context) func(paths []string) error {
	return func(paths []string) error { return s.RefuseWritesUnder(ctx, paths) }
}
