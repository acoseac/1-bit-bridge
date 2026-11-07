package manifest

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
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
// lands on the bare mount point, and the mount then hides it. One case stays
// open: backlog B776, item 7.

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

// RefuseWritesUnder looks at the deepest configured root that owns each of
// paths, absolute paths a request would write, by which root a path is
// (rootIn: the containment a subtree scan takes its owning root by, as the
// volume compares names; a nested root's folder written through the path of
// the root it is nested in is the nested root's), each root once, and answers
// a *RootAwayError for the first that reads as absent (lookAtRoot), nil when
// none does. A path under no configured root asks nothing: the caller's own
// containment refuses it. A look is a stat, one batch of the root's listing
// and, for a root holding nothing, one count of its rows, so a healthy write
// pays a few of them and a scan none.
func (s *Scanner) RefuseWritesUnder(ctx context.Context, paths []string) error {
	roots := s.Roots()
	looked := make(map[string]bool)
	for _, p := range paths {
		root, _ := s.rootIn(roots, p)
		if root == "" || looked[root] {
			continue
		}
		looked[root] = true
		if look := s.lookAtRoot(ctx, root); look.absent {
			return &RootAwayError{Root: root, look: look}
		}
	}
	return nil
}

// WriteGuard is RefuseWritesUnder on ctx, in the form the console's writers
// take it (upload.WithRootGuard, trash.WithRootGuard).
func (s *Scanner) WriteGuard(ctx context.Context) func(paths []string) error {
	return func(paths []string) error { return s.RefuseWritesUnder(ctx, paths) }
}
