package manifest

import (
	"context"
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// Backlog B777's review, round 2: the roots' own watches when a read or an
// Add fails at first.

// TestAFirstIdentityReadThatFailedIsTakenOnceTheFolderReads: the read of a
// root's identity before its watch is added fails, so the root is recorded
// with none, and every check returned before comparing, for good, though the
// reads answered again. Once the folder reads, its watch is added again
// around the read, as watchAgain adds one, so the identity recorded is of the
// folder that watch is on. Here the link the root is is repointed while the
// reads fail: the watch is still on the folder the link led to, and an
// identity recorded at a check without its watch added again would be the
// new folder's, against a watch on the old one, for good. A file written
// where the link leads now reaches the manifest.
func TestAFirstIdentityReadThatFailedIsTakenOnceTheFolderReads(t *testing.T) {
	f := newLinkedRootFixture(t, 50*time.Millisecond)
	ids := &identityAs{}
	ids.failWith(&fs.PathError{Op: "stat", Path: f.root, Err: syscall.EIO})
	checks := &rootChecks{}
	f.w.identityForTests = ids.read
	f.w.afterRootCheckForTests = checks.hook
	rec := loggingtest.Record(t)
	startWatcher(t, f.w)
	if ids.reads.Load() == 0 {
		t.Fatal("premise: the start read no identity")
	}
	checks.pass(t, 3)
	repoint(t, f.root, f.again)
	ids.failWith(nil)
	writeTracks(t, filepath.Join(f.again, "Artist", "Album", "02.flac"))
	waitForRow(t, f.store, "Artist/Album/02.flac",
		"a file written where the root's link leads now never reached the manifest: the identity the start could not read was never taken")
	checks.pass(t, 5)
	if got := rec.Lines(msgWatchRootIdentityTaken); len(got) != 1 {
		t.Errorf("the watcher said %d times that it took the root's identity, want once:\n%v", len(got), got)
	}
	mustHoldRows(t, f.store, lateMountTrack)
}

// TestTheWatcherTriesAgainARootWhoseOwnWatchCouldNotBeAdded (B777 item 2, and
// its review's round 2): a root that is there, whose own Add fails at the
// start and goes on failing (the watch limit spent, answered at the Add
// boundary), is not held: the hold lets a root's watches go and drops its
// scans and its events, which a healthy root must keep. It was looked at
// again, its tree walked, a scan asked for and the limit said at every check
// of the roots' own watches, for as long as the limit held. It is tried again
// on a backoff of its own, the hold's timing (recheckFirst, then twice as long
// each time, recheckMax at most), saying nothing, with one scan as it enters
// that state, which catches what changed while it was not watched, and none
// at a try. The other root's watches go on delivering, and once the limit
// lifts the root is watched again, said once, and a file dropped into it
// reaches the manifest.
func TestTheWatcherTriesAgainARootWhoseOwnWatchCouldNotBeAdded(t *testing.T) {
	dir := t.TempDir()
	one, two := filepath.Join(dir, "one"), filepath.Join(dir, "two")
	writeTracks(t, filepath.Join(one, "Artist", "Album", "01.flac"), filepath.Join(two, "Band", "Record", "01.flac"))
	store := openStoreIn(t, dir)
	scanner := NewScanner([]string{one, two}, store, "")
	if _, err := scanner.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Each check comes after the scan a check might ask for has run.
	w, err := NewWatcher(scanner, 20*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	w.recheckFirst, w.recheckMax, w.rootCheckEvery = 20*time.Millisecond, 160*time.Millisecond, 40*time.Millisecond
	var (
		mu      sync.Mutex
		tries   []time.Time
		limited atomic.Bool
	)
	limited.Store(true)
	w.addForTests = func(path string) error {
		if filepath.Clean(path) != filepath.Clean(two) || !limited.Load() {
			return nil
		}
		mu.Lock()
		tries = append(tries, time.Now())
		mu.Unlock()
		return fmt.Errorf("inotify_add_watch: %w", syscall.ENOSPC)
	}
	checks := &rootChecks{}
	w.afterRootCheckForTests = checks.hook
	held := make(chan string, 8)
	w.afterHoldForTests = func(root string) { held <- root }
	scans := countScans(w)
	rec := loggingtest.Record(t)
	startWatcher(t, w)
	checks.pass(t, 12)

	if n := scans.Load(); n != 1 {
		t.Errorf("the watcher dispatched %d scans over twelve checks while the root's own watch could not be added, want one", n)
	}
	mu.Lock()
	got := slices.Clone(tries)
	mu.Unlock()
	if len(got) == 0 || len(got) > 6 {
		t.Errorf("the root's own Add was tried %d times over twelve checks, want a try on a backoff, not one a check", len(got))
	}
	want := w.recheckFirst
	for i := 1; i < len(got); i++ {
		if gap := got[i].Sub(got[i-1]); gap < want {
			t.Errorf("try %d came %v after the last, want %v at least", i, gap, want)
		}
		want = min(2*want, w.recheckMax)
	}
	if n := len(rec.Failures(msgInitialWatchFailed)); n != 1 {
		t.Errorf("the start said %d times that the root's watch could not be added, want once", n)
	}
	if n := len(rec.Failures(msgWatchLimitReached)); n != 1 {
		t.Errorf("the watch limit was said %d times, want once: a try that fails says nothing", n)
	}
	select {
	case r := <-held:
		t.Errorf("the watcher held %s, a root that is there", r)
	default:
	}

	// The other root's watches go on delivering.
	writeTracks(t, filepath.Join(one, "Artist", "Album", "02.flac"))
	waitForRow(t, store, "one/Artist/Album/02.flac", "a file written into the root whose watch was added never reached the manifest")

	// The limit lifts.
	limited.Store(false)
	watchWaitUntil(t, func() bool { return len(rec.Lines(msgRootWatchAdded)) > 0 }, nil,
		"the root's own watch was never added again once the limit lifted")
	writeTracks(t, filepath.Join(two, "Band", "Record", "02.flac"))
	waitForRow(t, store, "two/Band/Record/02.flac", "a file written into the root once its own watch was added again never reached the manifest")
	checks.pass(t, 5)
	if n := len(rec.Lines(msgRootWatchAdded)); n != 1 {
		t.Errorf("the watcher said %d times that the root's own watch was added again, want once", n)
	}
}
