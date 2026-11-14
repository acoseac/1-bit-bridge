//go:build darwin

package doctor

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// TestACountBelowARootThatAnswersIsNotReadAsASmallOne is B650's case of a
// folder below a root that does not answer, with the library watcher on, for
// the kqueue budget: the root's own probe answers, and the count of what the
// watcher would hold open under it does not finish. The line says it is not
// checked and names the root, never a count it did not finish as a small one.
func TestACountBelowARootThatAnswersIsNotReadAsASmallOne(t *testing.T) {
	standInWatchFileLimits(t, manifest.WatchFDLimits{Process: 4096}, nil)
	root := libraryRoot(t, "music")
	s := newStallingRoots(nil, nil)
	p := probesOver(t, s, testBound)
	p.countWatches = func(r string, stopAt int) (watchCount, error) {
		if r == root {
			<-s.release
		}
		return countRootWatches(r, stopAt)
	}
	d := Deps{RootProbes: p.BeginRun(), LibraryRoots: []string{root}, LibraryWatchEnabled: true}
	c, _ := within(t, testDeadline, "watcher-fd-budget", func() Check { return checkWatchLimit(context.Background(), d) })
	want := fmt.Sprintf("not checked: the folders and entries under library root %s were not all counted within %s: a mount under it may be stalled", root, testBound)
	if c.Name != checkNameWatcherFDBudget || c.Status != Warn || !strings.HasPrefix(c.Summary, want) || c.Hint != stalledCountAdvice {
		t.Errorf("watcher-fd-budget = %+v, want a warn saying %q", c, want)
	}
}
