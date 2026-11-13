//go:build linux

package doctor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestACountBelowARootThatAnswersIsNotReadAsALowOne is B650's case of a
// folder below a root that does not answer, with the library watcher on
// (Codex's second note and Carbon's first on the brief): the root's own probe
// answers, so library-roots passes it, and the count of its directories
// meets a mount that does not answer. The count is bounded as the probe is,
// and the watch line says it is not checked and names the root, never a count
// it did not finish as a healthy one. Red on main, whose countDirs walked on
// the run's goroutine: the run never returned.
func TestACountBelowARootThatAnswersIsNotReadAsALowOne(t *testing.T) {
	if _, err := readInotifyLimit(); err != nil {
		t.Skipf("this host's inotify limit does not read, and the check stops there: %v", err)
	}
	root := libraryRoot(t, "music")
	s := newStallingRoots(nil, nil)
	p := probesOver(t, s, testBound)
	p.countWatches = func(r string, stopAt int) (watchCount, error) {
		if r == root {
			<-s.release
		}
		return countRootWatches(r, stopAt)
	}
	run := p.BeginRun()
	d := Deps{RootProbes: run, LibraryRoots: []string{root}, LibraryWatchEnabled: true}
	c, _ := within(t, testDeadline, "inotify-watch-limit", func() Check { return checkWatchLimit(context.Background(), d) })
	want := fmt.Sprintf("not checked: the folders under library root %s were not all counted within %s: a mount under it may be stalled", root, testBound)
	if c.Status != Warn || !strings.HasPrefix(c.Summary, want) || c.Hint != stalledCountAdvice {
		t.Errorf("inotify-watch-limit = %+v, want a warn saying %q", c, want)
	}
	if roots := checkLibraryRoots(context.Background(), d); roots != ok(checkNameLibraryRoots, "1 root(s) reachable") {
		t.Errorf("library-roots = %+v, want the root, which answers, reachable", roots)
	}
	second, _ := within(t, testDeadline, "inotify-watch-limit", func() Check {
		return checkWatchLimit(context.Background(), Deps{RootProbes: p.BeginRun(), LibraryRoots: []string{root}, LibraryWatchEnabled: true})
	})
	if !strings.Contains(second.Summary, "are still not all counted") {
		t.Errorf("a later run's inotify-watch-limit = %+v, want the count still out", second)
	}
}

// TestAHealthyLibrarysWatchLineIsAsItWas is B650's guard for the inotify
// line: over roots that answer, it is what main printed for this fixture, word
// for word (the same test is green on main's tree), the count summed over the
// roots each counted on its own.
func TestAHealthyLibrarysWatchLineIsAsItWas(t *testing.T) {
	limit, err := readInotifyLimit()
	if err != nil {
		t.Skipf("this host's inotify limit does not read: %v", err)
	}
	a, b := libraryRoot(t, "a"), libraryRoot(t, "b")
	if err := os.Mkdir(filepath.Join(b, "Album"), 0o755); err != nil {
		t.Fatal(err)
	}
	want := ok("inotify-watch-limit", fmt.Sprintf("3 directories vs limit %d", limit))
	if c := checkWatchLimit(context.Background(), Deps{LibraryRoots: []string{a, b}, LibraryWatchEnabled: true}); c != want {
		t.Errorf("inotify-watch-limit = %+v, want %+v", c, want)
	}
}
