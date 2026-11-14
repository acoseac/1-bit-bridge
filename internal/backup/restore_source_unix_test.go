//go:build unix

package backup_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/backup"
)

// fifoManifest is a named pipe where a snapshot keeps its manifest, the
// manifest's bytes kept aside: a read of the manifest waits for the pipe's
// writer, a mount that stopped answering's stand-in, until the test feeds it
// those bytes and closes it.
type fifoManifest struct {
	path string
	data []byte
	mu   sync.Mutex
	w    *os.File
	fed  bool
}

// newFIFOManifest puts the named pipe in place of the manifest of the
// snapshot in dir. The test's end feeds a reader still waiting on it, and puts
// the manifest back.
func newFIFOManifest(t *testing.T, dir string) *fifoManifest {
	t.Helper()
	f := &fifoManifest{path: filepath.Join(dir, backup.ManifestFile)}
	f.data = readBytes(t, f.path)
	if err := os.Remove(f.path); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(f.path, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		f.feedIfRead()
		f.putBack(t)
	})
	return f
}

// awaitReader waits until a reader has the pipe open, the read of the
// manifest under way: an open of the pipe's write end that does not wait
// succeeds only then, and is kept for the feed.
func (f *fifoManifest) awaitReader(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		w, err := os.OpenFile(f.path, os.O_WRONLY|syscall.O_NONBLOCK, 0)
		if err == nil {
			f.mu.Lock()
			f.w = w
			f.mu.Unlock()
			return
		}
		if !errors.Is(err, syscall.ENXIO) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("nothing ever read the snapshot's manifest")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// feed writes the manifest's bytes to the reader and closes the pipe: the
// read answers.
func (f *fifoManifest) feed(t *testing.T) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.w == nil || f.fed {
		t.Fatal("feed of a pipe nothing reads")
	}
	f.fed = true
	if _, err := f.w.Write(f.data); err != nil {
		t.Error(err)
	}
	_ = f.w.Close()
}

// feedIfRead feeds a reader the test left waiting, and one that opened the
// pipe since, so no read outlives the test.
func (f *fifoManifest) feedIfRead() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.w == nil {
		if w, err := os.OpenFile(f.path, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			f.w = w
		}
	}
	if f.w != nil && !f.fed {
		f.fed = true
		_, _ = f.w.Write(f.data)
		_ = f.w.Close()
	}
}

// putBack puts the manifest back where the pipe was.
func (f *fifoManifest) putBack(t *testing.T) {
	t.Helper()
	if err := os.Remove(f.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Error(err)
	}
	if err := os.WriteFile(f.path, f.data, 0o600); err != nil {
		t.Error(err)
	}
}

// TestAGivenUpRestoreThatHoldsTheLockMarksTheFolderUntilItLetsGo is round
// 2's red for item 1 (Codex's P2 on #1452): a restore whose read of the
// snapshot's manifest stops answering once it holds the folder's lock (a named
// pipe stands in for the mount) is refused at its stop, and its worker, given
// up, holds the lock and this process's holder of the folder until the read
// ends. The folder is marked while it does, naming the restore: another
// restore of it, a prune of it and another process's verification there are
// each refused at once, ErrCheckStillRunning in a restore's words, never left
// to wait their lock wait out as busy. Once the read answers the worker lets
// the lock go, its marker and its mark with it, and the folder takes a prune
// and a restore. Red on round 1, where the worker given up kept the lock with
// no mark: each of them waited, and answered ErrBackupsBusy.
func TestAGivenUpRestoreThatHoldsTheLockMarksTheFolderUntilItLetsGo(t *testing.T) {
	src := storeWithTracks(t, 5)
	snap := snapshotOf(t, src)
	other := snapshotOf(t, src)
	root := filepath.Dir(snap)
	name := filepath.Base(snap)
	b, c := newDestinationBox(t), newDestinationBox(t)
	ends := sourceWorkerEnds(t)
	pipe := newFIFOManifest(t, snap)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	callers := joinBackupCallers(t, pipe.feedIfRead)
	done := make(chan error, 1)
	callers.start(func() {
		done <- backup.RestoreWithOptions(snap, b.targets, backup.RestoreOptions{Context: ctx, LockWait: 20 * time.Second})
	})
	pipe.awaitReader(t)
	cancel()
	err := requireReturnedWithin(t, done, time.Now(), backup.StopWriteBound+2*time.Second, "a restore whose read of its snapshot does not answer")
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "nothing was restored") {
		t.Fatalf("the stopped restore answered %v, want its stop's refusal, nothing restored", err)
	}
	b.requireUntouched(t, "the restore given up")

	words := "a restore of snapshot " + name
	asked := time.Now()
	if err := backup.RestoreWithOptions(snap, c.targets, backup.RestoreOptions{LockWait: 10 * time.Second}); !errors.Is(err, backup.ErrCheckStillRunning) || !strings.Contains(err.Error(), words) {
		t.Errorf("another restore beside the restore given up answered %v, want ErrCheckStillRunning naming %q", err, words)
	}
	if pr, err := backup.Prune(root, 1); err != nil || !pr.Skipped || !errors.Is(pr.SkippedFor, backup.ErrCheckStillRunning) ||
		!strings.Contains(pr.SkippedFor.Error(), words) {
		t.Errorf("a prune beside the restore given up: %+v, %v; want it skipped for ErrCheckStillRunning naming %q", pr, err, words)
	}
	if took := time.Since(asked); took > 5*time.Second {
		t.Errorf("the restore and the prune beside the restore given up waited %v to be refused", took)
	}
	// Once the marker reads whole (backlog B899): no hold before the
	// restore's was given up in root, so the marker read is the restore's,
	// which its words say. In this process the restore's mark refuses the
	// verification first (verifyAndThen asks this process's marks before any
	// filesystem); another process's read of the marker is
	// TestACheckThatDidNotEndInAnotherProcessHoldsTheFolderUntilItEnds's.
	awaitMarker(t, root, "the restore given up", func(s backup.StuckCheck) bool {
		return s.Restoring() && s.Snapshot == name
	})
	if _, err := backup.VerifySnapshotAsAnotherProcessForTest(t.Context(), other, backup.VerifyOptions{LockWait: 10 * time.Second}); !errors.Is(err, backup.ErrCheckStillRunning) ||
		!strings.Contains(err.Error(), words) {
		t.Errorf("another process's verification beside the restore given up answered %v, want ErrCheckStillRunning naming %q", err, words)
	}
	c.requireUntouched(t, "the restore refused beside the restore given up")

	pipe.feed(t)
	awaitSourceWorkerEnd(t, ends, snap)
	awaitClean(t)
	if s, held := backup.StuckCheckHere(root); held {
		t.Errorf("the folder is still marked %+v once the restore's worker let go", s)
	}
	if pathExists(t, filepath.Join(root, backup.StuckMarkerName)) {
		t.Error("the restore's marker outlived its lock")
	}
	if held, err := backupsLockHeld(root); err != nil || held {
		t.Errorf("the folder's lock once the restore's worker let go: held %v, %v; want it free", held, err)
	}
	pipe.putBack(t)
	if err := backup.RestoreWithOptions(snap, c.targets, backup.RestoreOptions{}); err != nil {
		t.Fatalf("a restore once the worker let go answered %v", err)
	}
	c.requireRestored(t, snap)
	callers.wait(t)
}
