package manifest

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// lateMountFixture is a library root below a volume's mount point (root,
// usb/music), indexed by a scan while the volume was there, and a watcher
// over it that has not started. volume is a second copy of the same tree,
// standing for the volume a mount puts at the root's path: another directory
// than the one the watcher may have watched there, as a mount's root is.
type lateMountFixture struct {
	root, volume, gone string
	store              *Store
	w                  *Watcher
}

// lateMountTrack is the one track the fixture indexes.
const lateMountTrack = "Artist/Album/01.flac"

func newLateMountFixture(t *testing.T) *lateMountFixture {
	t.Helper()
	dir := t.TempDir()
	f := &lateMountFixture{
		root:   filepath.Join(dir, "usb", "music"),
		volume: filepath.Join(dir, "volume"),
		gone:   filepath.Join(dir, "gone"),
	}
	when := time.Now().Add(-time.Hour).Truncate(time.Second)
	for _, top := range []string{f.root, f.volume} {
		album := filepath.Join(top, "Artist", "Album")
		if err := os.MkdirAll(album, 0o755); err != nil {
			t.Fatal(err)
		}
		track := filepath.Join(album, "01.flac")
		if err := os.WriteFile(track, []byte("not a real flac"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(track, when, when); err != nil {
			t.Fatal(err)
		}
	}
	store, err := OpenStore(filepath.Join(dir, "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	f.store = store
	scanner := NewScanner([]string{f.root}, store, "")
	if _, err := scanner.Scan(context.Background()); err != nil {
		t.Fatalf("first scan: %v", err)
	}
	f.mustHold(t, lateMountTrack)
	w, err := NewWatcher(scanner, 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	w.recheckFirst, w.recheckMax, w.rootCheckEvery = 20*time.Millisecond, 100*time.Millisecond, 20*time.Millisecond
	f.w = w
	return f
}

// hooks returns the roots the watcher holds and the ones it watches again,
// as it does so. Set before Run starts.
func (f *lateMountFixture) hooks() (held, resumed <-chan string) {
	h, r := make(chan string, 8), make(chan string, 8)
	f.w.afterHoldForTests = func(root string) { h <- root }
	f.w.afterResumeForTests = func(root string) { r <- root }
	return h, r
}

// unmount takes the volume away: the root's path is then gone, or, with
// bare, the bare mount point is left there, an empty directory.
func (f *lateMountFixture) unmount(t *testing.T, bare bool) {
	t.Helper()
	if err := os.Rename(f.root, f.gone); err != nil {
		t.Fatal(err)
	}
	if bare {
		if err := os.Mkdir(f.root, 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

// mount puts the volume at the root's path, in place of the bare mount
// point when one is there.
func (f *lateMountFixture) mount(t *testing.T) {
	t.Helper()
	if err := os.Remove(f.root); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.Rename(f.volume, f.root); err != nil {
		t.Fatal(err)
	}
}

// drop writes a new track into the root's album folder and waits until it is
// indexed: what the watcher does for a root it watches.
func (f *lateMountFixture) drop(t *testing.T, name, msg string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.root, "Artist", "Album", name), []byte("not a real flac"), 0o644); err != nil {
		t.Fatal(err)
	}
	rel := "Artist/Album/" + name
	watchWaitUntil(t, func() bool {
		got, _ := f.store.GetTrack(context.Background(), rel)
		return got != nil
	}, nil, msg)
}

// mustHold fails the test unless the store holds rel's row with no missed
// scan counted against it.
func (f *lateMountFixture) mustHold(t *testing.T, rel string) {
	t.Helper()
	if got, _ := f.store.GetTrack(context.Background(), rel); got == nil {
		t.Fatalf("the store does not hold %s", rel)
	}
	mustHaveMissingCount(t, f.store, "tracks", rel, 0)
}

// waitForRoot waits for a root one of the watcher's hooks sent, and fails
// with msg when the wait gives up (watchGiveUp).
func waitForRoot(t *testing.T, ch <-chan string, msg string) string {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-watchGiveUp(t, watchWaitReserve):
		t.Fatal(msg)
	}
	return ""
}

// mustSayOnce fails the test unless the watcher said it held a root once,
// at Warn, and that it watched it again once.
func mustSayOnce(t *testing.T, rec *loggingtest.Recorder) {
	t.Helper()
	if got := rec.Failures(msgWatchRootHeld); len(got) != 1 {
		t.Errorf("the watcher said it held the root %d times, want once:\n%v", len(got), got)
	}
	if got := rec.Lines(msgWatchRootResumed); len(got) != 1 {
		t.Errorf("the watcher said it watched the root again %d times, want once:\n%v", len(got), got)
	}
}

// TestTheWatcherWaitsForAnUnmountedRootAndWatchesWhatIsMountedThere pins the
// watcher's half of B651 at its start, which on a NAS reboot can come before
// the library's volume is mounted: a root that is not there, or the bare
// mount point holding nothing while the index holds the root's tracks, is not
// watched (a watch on the bare mount point is on a directory the mount then
// hides, so nothing the volume holds would ever be seen), and is looked at
// again on a bounded retry; once it reads as it did, the volume's own
// directory is watched, a scan of the root is asked for, and a file dropped
// into it reaches the manifest. The track indexed before is never touched.
func TestTheWatcherWaitsForAnUnmountedRootAndWatchesWhatIsMountedThere(t *testing.T) {
	for _, tc := range []struct {
		name string
		bare bool
	}{
		{"the root below the mount point is not there", false},
		{"the bare mount point is there and empty", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLateMountFixture(t)
			f.unmount(t, tc.bare)
			rec := loggingtest.Record(t)
			held, resumed := f.hooks()
			startWatcher(t, f.w)
			if got := waitForRoot(t, held, "the watcher never held a root that reads as a volume not mounted"); got != f.root {
				t.Errorf("the watcher held %q, want %q", got, f.root)
			}
			if watched(f.w, "music") {
				t.Errorf("the watcher watches a root it holds (watches: %q)", f.w.w.WatchList())
			}
			f.mount(t)
			if got := waitForRoot(t, resumed, "the watcher never watched the root again once its volume was there"); got != f.root {
				t.Errorf("the watcher watched %q again, want %q", got, f.root)
			}
			f.drop(t, "02.flac", "a file dropped into the root once its volume was there never reached the manifest")
			f.mustHold(t, lateMountTrack)
			mustSayOnce(t, rec)
		})
	}
}

// TestTheWatcherDispatchesNoScanForARootWhoseContentWent pins the check a
// debounced dispatch makes: when the scan an event asked for is due, the root
// it is under is looked at first, and a root that reads as a volume not
// mounted (here: its one folder moved out, so it holds nothing while the index
// holds its track) gets no scan. The watcher holds it, lets its watches go,
// and watches it again once the folder is back, with a scan that finds the
// track where it was.
func TestTheWatcherDispatchesNoScanForARootWhoseContentWent(t *testing.T) {
	f := newLateMountFixture(t)
	dispatched := make(chan struct{}, 16)
	f.w.afterDispatchHookForTests = func() {
		select {
		case dispatched <- struct{}{}:
		default:
		}
	}
	rec := loggingtest.Record(t)
	held, resumed := f.hooks()
	startWatcher(t, f.w)
	artist := filepath.Join(f.root, "Artist")
	if err := os.Rename(artist, f.gone); err != nil {
		t.Fatal(err)
	}
	select {
	case <-dispatched:
		t.Fatal("the watcher dispatched a scan of a root that reads as a volume not mounted")
	case got := <-held:
		if got != f.root {
			t.Errorf("the watcher held %q, want %q", got, f.root)
		}
	case <-watchGiveUp(t, watchWaitReserve):
		t.Fatal("the watcher neither held the root nor dispatched its scan")
	}
	if watched(f.w, "music") {
		t.Errorf("the watcher kept the watches of a root it holds (watches: %q)", f.w.w.WatchList())
	}
	if err := os.Rename(f.gone, artist); err != nil {
		t.Fatal(err)
	}
	waitForRoot(t, resumed, "the watcher never watched the root again once its folder was back")
	select {
	case <-dispatched:
	case <-watchGiveUp(t, watchWaitReserve):
		t.Fatal("the watcher asked for no scan of a root it watched again")
	}
	f.mustHold(t, lateMountTrack)
	mustSayOnce(t, rec)
}

// TestTheWatcherHoldsARootWhoseWatchesWentWithoutAWord pins the check of the
// roots' own watches: Linux drops every inotify watch on a volume as it is
// unmounted and says nothing (IN_UNMOUNT, which fsnotify swallows), so a
// root whose own watch is gone is looked at, held while it reads as a volume
// not mounted, and watched again once the volume is back. The test drops the
// watches itself, as the unmount does, and takes the volume away under no
// watch.
func TestTheWatcherHoldsARootWhoseWatchesWentWithoutAWord(t *testing.T) {
	if kqueueBackend {
		t.Skip("kqueue keeps the watched folders of an unmounted volume open, and the watcher's account of them")
	}
	f := newLateMountFixture(t)
	held, resumed := f.hooks()
	startWatcher(t, f.w)
	for _, p := range f.w.w.WatchList() {
		_ = f.w.w.Remove(p)
	}
	f.unmount(t, true)
	if got := waitForRoot(t, held, "the watcher never held a root whose watches went"); got != f.root {
		t.Errorf("the watcher held %q, want %q", got, f.root)
	}
	f.mount(t)
	waitForRoot(t, resumed, "the watcher never watched the root again once its volume was back")
	f.drop(t, "02.flac", "a file dropped into the root once its volume was back never reached the manifest")
	f.mustHold(t, lateMountTrack)
}

// TestTheWatcherHoldsARootThatWentAndWatchesWhatComesBack pins the look an
// event naming a root itself asks for: the root moved away (its own watch's
// Rename), the watcher holds it, and once another directory is there, as a
// mount puts one, that directory is watched.
func TestTheWatcherHoldsARootThatWentAndWatchesWhatComesBack(t *testing.T) {
	f := newLateMountFixture(t)
	held, resumed := f.hooks()
	startWatcher(t, f.w)
	f.unmount(t, false)
	if got := waitForRoot(t, held, "the watcher never held a root that went"); got != f.root {
		t.Errorf("the watcher held %q, want %q", got, f.root)
	}
	f.mount(t)
	waitForRoot(t, resumed, "the watcher never watched the root again once it was back")
	f.drop(t, "02.flac", "a file dropped into the root once it was back never reached the manifest")
	f.mustHold(t, lateMountTrack)
}
