package manifest

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
)

// A library root may be configured inside another: neither the config's
// validation nor the console's or bridge library add's root add refuses one
// (config.Validate checks a root for being empty alone; the adds refuse a
// root already configured and two of one folder name). The outer root is
// listed first in these tests, the order in which a first-match lookup gives
// a path under the nested root to the outer one (B651's fifth round,
// CodeRabbit on #1397).

// nestedRoots is a library root, music, holding a root of its own, music/usb,
// on a volume that can be taken away, both indexed by a scan while the volume
// was there; and a watcher over them that has not started. volume is a second
// copy of the nested root's tree, standing for the volume a mount puts back.
type nestedRoots struct {
	outer, inner, volume, gone string
	store                      *Store
	scanner                    *Scanner
	w                          *Watcher
}

// The rows the fixture indexes: the outer root's own track, the outer
// root's row for the nested root's track (its walk passes through the nested
// root, as it always has), and the nested root's own row for that track.
const (
	nestedOuterTrack   = "music/Artist/Album/01.flac"
	nestedThroughOuter = "music/usb/Band/Record/01.flac"
	nestedInnerTrack   = "usb/Band/Record/01.flac"
)

func newNestedRoots(t *testing.T, debounce time.Duration) *nestedRoots {
	t.Helper()
	dir := t.TempDir()
	n := &nestedRoots{
		outer:  filepath.Join(dir, "music"),
		volume: filepath.Join(dir, "volume"),
		gone:   filepath.Join(dir, "gone"),
	}
	n.inner = filepath.Join(n.outer, "usb")
	when := time.Now().Add(-time.Hour).Truncate(time.Second)
	for _, f := range []string{
		filepath.Join(n.outer, "Artist", "Album", "01.flac"),
		filepath.Join(n.inner, "Band", "Record", "01.flac"),
		filepath.Join(n.volume, "Band", "Record", "01.flac"),
	} {
		if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, []byte("not a real flac"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(f, when, when); err != nil {
			t.Fatal(err)
		}
	}
	store, err := OpenStore(filepath.Join(dir, "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	n.store = store
	n.scanner = NewScanner([]string{n.outer, n.inner}, store, "")
	if _, err := n.scanner.Scan(context.Background()); err != nil {
		t.Fatalf("first scan: %v", err)
	}
	for _, rel := range []string{nestedOuterTrack, nestedThroughOuter, nestedInnerTrack} {
		n.mustHold(t, rel)
	}
	w, err := NewWatcher(n.scanner, debounce)
	if err != nil {
		t.Fatal(err)
	}
	w.recheckFirst, w.recheckMax, w.rootCheckEvery = 20*time.Millisecond, 100*time.Millisecond, 20*time.Millisecond
	n.w = w
	return n
}

// takeInnerAway unmounts the nested root's volume: its mount point stays,
// bare.
func (n *nestedRoots) takeInnerAway(t *testing.T) {
	t.Helper()
	if err := os.Rename(n.inner, n.gone); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(n.inner, 0o755); err != nil {
		t.Fatal(err)
	}
}

// mountInner puts the nested root's volume back over its bare mount point.
func (n *nestedRoots) mountInner(t *testing.T) {
	t.Helper()
	if err := os.Remove(n.inner); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(n.volume, n.inner); err != nil {
		t.Fatal(err)
	}
}

// mustHold fails the test unless the store holds rel's row with no missed
// scan counted against it.
func (n *nestedRoots) mustHold(t *testing.T, rel string) {
	t.Helper()
	if got, _ := n.store.GetTrack(context.Background(), rel); got == nil {
		t.Fatalf("the store does not hold %s", rel)
	}
	mustHaveMissingCount(t, n.store, "tracks", rel, 0)
}

// TestTheWatcherWatchesNothingOfANestedRootItHolds: a root nested in another
// that reads as a volume not mounted at the start (indexed, its mount point
// bare) is held, and the walk of the root it is in watches nothing at or
// under it, as no walk watches a held root (CodeRabbit on #1397): a watch on
// the bare mount point is on a directory the mount then hides. The outer
// root is watched as ever, and the nested one again once its volume is back.
func TestTheWatcherWatchesNothingOfANestedRootItHolds(t *testing.T) {
	n := newNestedRoots(t, 50*time.Millisecond)
	n.takeInnerAway(t)
	held, resumed := make(chan string, 8), make(chan string, 8)
	n.w.afterHoldForTests = func(root string) { held <- root }
	n.w.afterResumeForTests = func(root string) { resumed <- root }
	startWatcher(t, n.w)
	if got := waitForRoot(t, held, "the watcher never held a nested root that reads as a volume not mounted"); got != n.inner {
		t.Errorf("the watcher held %q, want %q", got, n.inner)
	}
	watchesOuter := false
	for _, p := range n.w.w.WatchList() {
		p = filepath.Clean(p)
		if pathAtOrUnder(p, n.inner) {
			t.Errorf("the watcher watches %s, at or under %s, a root it holds", p, n.inner)
		}
		watchesOuter = watchesOuter || p == n.outer
	}
	if !watchesOuter {
		t.Errorf("the watcher does not watch %s, the root a held root is nested in (watches: %q)", n.outer, n.w.w.WatchList())
	}
	n.mountInner(t)
	if got := waitForRoot(t, resumed, "the watcher never watched the nested root again once its volume was back"); got != n.inner {
		t.Errorf("the watcher watched %q again, want %q", got, n.inner)
	}
	if !slices.ContainsFunc(n.w.w.WatchList(), func(p string) bool { return filepath.Clean(p) == n.inner }) {
		t.Errorf("the nested root is not watched once its volume is back (watches: %q)", n.w.w.WatchList())
	}
}

// TestANestedRootsPathsAndEventsAreItsOwn: a path under a root nested in
// another is the nested root's, the deepest root that holds it, whichever is
// listed first, for the watcher's events and for the look a dispatch makes
// (CodeRabbit and Codex on #1397). So while the nested root is held, an event
// naming it, or anything under it, arms no scan, of it or of the root it is
// in, and the dispatch's look finds the nested root not mounted; an event in
// the outer root, outside the nested one, arms the scan of its folder. The
// watcher's steps run on the test's goroutine with no Run, so nothing else
// arms or fires meanwhile; the scans are armed for an hour.
func TestANestedRootsPathsAndEventsAreItsOwn(t *testing.T) {
	n := newNestedRoots(t, time.Hour)
	t.Cleanup(func() {
		n.w.cancelAllPending()
		_ = n.w.w.Close()
	})
	roots := n.scanner.Roots()
	for _, c := range []struct {
		name, root string
		itself     bool
	}{
		{n.outer, n.outer, true},
		{filepath.Join(n.outer, "Artist"), n.outer, false},
		{n.inner, n.inner, true},
		{filepath.Join(n.inner, "Band", "Record"), n.inner, false},
	} {
		if root, itself := n.scanner.rootIn(roots, c.name); root != c.root || itself != c.itself {
			t.Errorf("rootIn(%s) = %q, itself %v; want %q, itself %v", c.name, root, itself, c.root, c.itself)
		}
	}

	n.takeInnerAway(t)
	ctx := context.Background()
	root, look, absent := n.w.absentRootOf(ctx, filepath.Join(n.inner, "Band"))
	if root != n.inner || !absent {
		t.Fatalf("the dispatch's look at a folder under the nested root found %q absent %v (%s); want %q not mounted", root, absent, look.reason, n.inner)
	}
	n.w.holdRoot(n.inner, look)
	n.w.handleEvent(ctx, fsnotify.Event{Name: n.inner, Op: fsnotify.Remove})
	n.w.handleEvent(ctx, fsnotify.Event{Name: filepath.Join(n.inner, "Band", "Record", "02.flac"), Op: fsnotify.Create})
	if got := armedUnder(n.w, n.outer); len(got) != 0 {
		t.Errorf("events at and under a held nested root armed scans of %q", got)
	}
	album := filepath.Join(n.outer, "Artist", "Album")
	n.w.handleEvent(ctx, fsnotify.Event{Name: filepath.Join(album, "02.flac"), Op: fsnotify.Create})
	if got := armedUnder(n.w, n.outer); !slices.Equal(got, []string{album}) {
		t.Errorf("an event in the outer root armed scans of %q, want %q", got, []string{album})
	}
}

// TestAScanUnderANestedRootIsItsOwn: a subtree scan of a folder under a root
// nested in another is the nested root's scan, whichever is listed first: a
// new file there is indexed under the nested root's own path, not the outer
// root's (Codex on #1397).
func TestAScanUnderANestedRootIsItsOwn(t *testing.T) {
	n := newNestedRoots(t, time.Hour)
	record := filepath.Join(n.inner, "Band", "Record")
	if err := os.WriteFile(filepath.Join(record, "02.flac"), []byte("not a real flac"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := n.scanner.ScanSubtree(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if got, _ := n.store.GetTrack(context.Background(), "usb/Band/Record/02.flac"); got == nil {
		t.Error("a scan of a folder under the nested root did not index its new file under the nested root's path")
	}
	if got, _ := n.store.GetTrack(context.Background(), "music/usb/Band/Record/02.flac"); got != nil {
		t.Error("a scan of a folder under the nested root indexed its new file under the outer root's path")
	}
}

// TestAScanOfARootSparesANestedRootThatReadsAsNotMounted: a scan of the outer
// root, a subtree scan of it (what an event at its top asks for) or the full
// scan, neither walks into a nested root that reads as a volume not mounted
// (Scanner.lookAtRoot) nor reaps the rows it holds under that root's path
// (Argon on the brief): its bare mount point is no evidence that the
// nested volume's files are gone. The nested root's own rows are spared by
// its own guards, as before.
func TestAScanOfARootSparesANestedRootThatReadsAsNotMounted(t *testing.T) {
	n := newNestedRoots(t, time.Hour)
	n.takeInnerAway(t)
	ctx := context.Background()
	if _, err := n.scanner.ScanSubtree(ctx, n.outer); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{nestedOuterTrack, nestedThroughOuter, nestedInnerTrack} {
		n.mustHold(t, rel)
	}
	if _, err := n.scanner.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{nestedOuterTrack, nestedThroughOuter, nestedInnerTrack} {
		n.mustHold(t, rel)
	}
}
