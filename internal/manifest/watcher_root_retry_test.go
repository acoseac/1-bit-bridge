package manifest

import (
	"io/fs"
	"path/filepath"
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
