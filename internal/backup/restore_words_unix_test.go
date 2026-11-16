//go:build unix

package backup_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/backup"
)

// TestARestoreRefusedForAnotherRestoreNamesItselfAndThatRestore (backlog
// B826's third round): a restore of snapshot A, stopped while its read of A's
// manifest does not answer under the folder's lock (a named pipe stands in for
// the mount), is given up and marks the folder as A's restore. A restore of
// snapshot B, A's neighbour, named through a link to the folder, is refused,
// ErrCheckStillRunning, naming B as it was given it and never where it
// resolves, and naming the hold that refuses it as it is named, A's restore
// (StillRunningError: A's folder name, a restore), and the marker beside the
// lock says the same to another process: its record names A's restore, and
// another process's verification of B there is refused naming it.
func TestARestoreRefusedForAnotherRestoreNamesItselfAndThatRestore(t *testing.T) {
	src := storeWithTracks(t, 5)
	snapA := snapshotOf(t, src)
	snapB := snapshotOf(t, src)
	root := filepath.Dir(snapA)
	givenB := throughALinkedFolder(t, snapB)
	b, c := newDestinationBox(t), newDestinationBox(t)
	ends := sourceWorkerEnds(t)
	pipe := newFIFOManifest(t, snapA)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	callers := joinBackupCallers(t, pipe.feedIfRead)
	done := make(chan error, 1)
	callers.start(func() {
		done <- backup.RestoreWithOptions(snapA, b.targets, backup.RestoreOptions{Context: ctx, LockWait: 20 * time.Second})
	})
	pipe.awaitReader(t)
	cancel()
	if err := requireReturnedWithin(t, done, time.Now(), backup.StopWriteBound+2*time.Second, "a restore whose read of its snapshot does not answer"); !errors.Is(err, context.Canceled) {
		t.Fatalf("premise: the stopped restore of A answered %v, want its stop's refusal", err)
	}

	blocker := "a restore of snapshot " + filepath.Base(snapA)
	err := backup.RestoreWithOptions(givenB, c.targets, backup.RestoreOptions{LockWait: 10 * time.Second})
	var held *backup.StillRunningError
	if !errors.Is(err, backup.ErrCheckStillRunning) || !errors.As(err, &held) || !held.Check.Restoring() || held.Check.Snapshot != filepath.Base(snapA) {
		t.Errorf("the restore of B beside A's restore given up answered %v, want ErrCheckStillRunning for A's restore", err)
	}
	if err == nil || !strings.Contains(err.Error(), blocker) || !strings.Contains(err.Error(), "nothing was restored") {
		t.Errorf("the refusal of B's restore does not name %q, nothing restored: %v", blocker, err)
	}
	requireNamedAsGiven(t, "the refusal of B's restore", err, givenB, snapB)
	c.requireUntouched(t, "the restore of B refused beside A's restore given up")

	// Read until it decodes as A's restore, never taken once it is there
	// (backlog B899): no hold before A's was given up in root, and B's
	// restore, refused by this process's mark, held nothing.
	awaitMarker(t, root, "A's restore", func(s backup.StuckCheck) bool {
		return s.Restoring() && s.Snapshot == filepath.Base(snapA)
	})
	if _, err := backup.VerifySnapshotAsAnotherProcessForTest(t.Context(), snapB, backup.VerifyOptions{LockWait: 10 * time.Second}); !errors.Is(err, backup.ErrCheckStillRunning) ||
		!strings.Contains(err.Error(), blocker) {
		t.Errorf("another process's verification of B beside A's restore given up answered %v, want ErrCheckStillRunning naming %q", err, blocker)
	}

	pipe.feed(t)
	awaitSourceWorkerEnd(t, ends, snapA)
	awaitClean(t)
	callers.wait(t)
}
