package backup_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// readStuckMarker reads the marker beside the lock of the backups folder at
// root as a StuckCheck, failing the test where it does not read.
func readStuckMarker(t *testing.T, root string) backup.StuckCheck {
	t.Helper()
	var s backup.StuckCheck
	if err := json.Unmarshal(readBytes(t, filepath.Join(root, backup.StuckMarkerName)), &s); err != nil {
		t.Fatalf("the marker does not read: %v", err)
	}
	return s
}

// waitReserve is what a capped wait leaves of the test binary's deadline for
// the cleanups that run after it (waitWithin).
const waitReserve = 10 * time.Second

// waitWithin is how long a test's wait for an event may take: limit, the
// wait's own maximum, capped by the test binary's deadline less waitReserve,
// so that a missed event fails its case while the cleanups after it still have
// time to run, never at the package's deadline; with no deadline (-timeout 0)
// the maximum alone applies (backlog B871).
func waitWithin(t *testing.T, limit time.Duration) time.Duration {
	t.Helper()
	if deadline, ok := t.Deadline(); ok {
		if left := time.Until(deadline) - waitReserve; left < limit {
			return max(left, 0)
		}
	}
	return limit
}

// awaitEvent waits for event on events, failing the test past its wait: 30
// s, capped by the test binary's deadline (waitWithin).
func awaitEvent(t *testing.T, events <-chan string, want ...string) string {
	t.Helper()
	deadline := time.After(waitWithin(t, 30*time.Second))
	for {
		select {
		case ev := <-events:
			for _, w := range want {
				if ev == w {
					return ev
				}
			}
		case <-deadline:
			t.Fatalf("none of %v came", want)
		}
	}
}

// TestTheStaleMarkersRemovalNeverTakesAwayARestoresMarker is backlog B826's
// second round (Codex on its brief, finding 3): a restore holds the folder's
// lock, beside a marker a checker whose process ended left there, and is
// stopped and given up, which marks the folder as a restore's. Given up
// while its removal of the stale marker does not answer, its marker's write
// waits for that removal to end, so the removal, once it answers, takes the
// stale marker away and never the restore's, written after it; given up
// before the removal began, the removal is skipped, the restore's marker
// standing. Either way another process then reads the folder as held by a
// given-up restore while the worker still holds the lock, and the worker,
// seeing the give-up, reads nothing more and lets the lock go, its marker
// with it. Its controls write the marker at once, which a removal answering
// after it takes away, and remove the stale marker whatever the give-up.
func TestTheStaleMarkersRemovalNeverTakesAwayARestoresMarker(t *testing.T) {
	for _, when := range []string{"during the removal", "before the removal began"} {
		t.Run(when, func(t *testing.T) {
			src := storeWithTracks(t, 5)
			snap := snapshotOf(t, src)
			other := snapshotOf(t, src)
			root := filepath.Dir(snap)
			marker := writeStuckMarker(t, root, "a check whose process ended")
			b := newDestinationBox(t)
			ends := sourceWorkerEnds(t)

			removal, held, cleared := newParking(), newParking(), newParking()
			var removing, holding, clearedOnce atomic.Bool
			t.Cleanup(backup.SetRemoveStaleMarkerForTest(func(p string) error {
				if when == "during the removal" && p == marker && removing.CompareAndSwap(false, true) {
					removal.park()
				}
				return os.Remove(p)
			}))
			events := make(chan string, 8)
			t.Cleanup(backup.SetOnMarkerWriteForTest(func(event, m string) {
				if m == marker {
					select {
					case events <- event:
					default:
					}
				}
			}))
			t.Cleanup(backup.SetOnSourceStepForTest(func(dir, step string) {
				switch {
				case dir != snap:
				case step == "held" && when == "before the removal began" && holding.CompareAndSwap(false, true):
					held.park()
				case step == "cleared" && clearedOnce.CompareAndSwap(false, true):
					cleared.park()
				}
			}))
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			callers := joinBackupCallers(t, func() { removal.release(); held.release(); cleared.release() })
			done := make(chan error, 1)
			callers.start(func() {
				done <- backup.RestoreWithOptions(snap, b.targets, backup.RestoreOptions{Context: ctx, LockWait: 20 * time.Second})
			})
			if when == "during the removal" {
				awaitEntered(t, removal.entered, "the stale marker's removal")
			} else {
				awaitEntered(t, held.entered, "the worker with the lock, before its removal")
			}
			cancel()
			err := requireReturnedWithin(t, done, time.Now(), backup.StopWriteBound+2*time.Second, "a restore stopped with the lock held")
			if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "nothing was restored") {
				t.Fatalf("the stopped restore answered %v, want its stop's refusal, nothing restored", err)
			}
			if when == "during the removal" {
				// The restore's marker waits for the removal, or (its control)
				// is written.
				awaitEvent(t, events, "waits", "writes")
				removal.release()
			} else {
				awaitEvent(t, events, "writes")
				held.release()
			}
			awaitEntered(t, cleared.entered, "the worker past its removal")
			if when == "during the removal" {
				awaitEvent(t, events, "writes")
			}
			if !pathExists(t, marker) {
				t.Fatal("the stale marker's removal took the restore's marker away")
			}
			if s := readStuckMarker(t, root); !s.Restoring() || s.Snapshot != filepath.Base(snap) {
				t.Errorf("the marker beside the lock reads %+v, want the restore of %s", s, filepath.Base(snap))
			}
			words := "a restore of snapshot " + filepath.Base(snap)
			if _, err := backup.VerifySnapshotAsAnotherProcessForTest(t.Context(), other, backup.VerifyOptions{LockWait: 10 * time.Second}); !errors.Is(err, backup.ErrCheckStillRunning) ||
				!strings.Contains(err.Error(), words) {
				t.Errorf("another process's verification beside the restore given up answered %v, want ErrCheckStillRunning naming %q", err, words)
			}

			cleared.release()
			awaitSourceWorkerEnd(t, ends, snap)
			awaitClean(t)
			if pathExists(t, marker) {
				t.Error("the restore's marker outlived its lock")
			}
			if held, err := backupsLockHeld(root); err != nil || held {
				t.Errorf("the folder's lock once the restore's worker let go: held %v, %v; want it free", held, err)
			}
			b.requireUntouched(t, "the restore given up with the lock held")
			callers.wait(t)
		})
	}
}

// TestARestoresMarkStandsUntilItsUnlockEnds is backlog B826's second round
// (Codex on its brief, finding 4): a restore's let-go of the folder's lock is
// bounded, and one whose unlock stops answering is marked in this process,
// naming the restore and its step, until the unlock, the lock file's close and
// this process's holder of the folder are let go; the restore answers within
// its bound meanwhile. Both ways a restore lets the lock go: refused as the
// lock came (its context ended as its worker handed the lock over) and at its
// end once restored. Its control lets the lock go unmarked: another restore
// then waits its lock wait out, ErrBackupsBusy.
func TestARestoresMarkStandsUntilItsUnlockEnds(t *testing.T) {
	for _, how := range []string{"refused as the lock came", "at its end once restored"} {
		t.Run(how, func(t *testing.T) {
			snap := snapshotOf(t, storeWithTracks(t, 5))
			root := filepath.Dir(snap)
			b, c := newDestinationBox(t), newDestinationBox(t)
			const bound = 300 * time.Millisecond
			t.Cleanup(backup.SetReleaseBoundForTest(bound))
			unlock, read := newParking(), newParking()
			var unlocking, reading atomic.Bool
			t.Cleanup(backup.SetOnLockReleaseForTest(func(dir string) {
				if dir == root && unlocking.CompareAndSwap(false, true) {
					unlock.park()
				}
			}))
			if how == "refused as the lock came" {
				t.Cleanup(backup.SetOnSourceStepForTest(func(dir, step string) {
					if dir == snap && step == "read" && reading.CompareAndSwap(false, true) {
						read.park()
					}
				}))
			}
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			callers := joinBackupCallers(t, func() { read.release(); unlock.release() })
			done := make(chan error, 1)
			callers.start(func() {
				done <- backup.RestoreWithOptions(snap, b.targets, backup.RestoreOptions{Context: ctx, LockWait: 20 * time.Second})
			})
			start := time.Now()
			if how == "refused as the lock came" {
				awaitEntered(t, read.entered, "the worker with the lock, about to hand it over")
				cancel()
				start = time.Now()
				read.release()
			}
			awaitEntered(t, unlock.entered, "the restore's unlock")
			err := requireReturnedWithin(t, done, start, bound+backup.StopWriteBound+2*time.Second, "a restore whose unlock does not answer")
			switch how {
			case "refused as the lock came":
				if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "nothing was restored") {
					t.Errorf("the restore stopped as the lock came answered %v, want its stop's refusal, nothing restored", err)
				}
				b.requireUntouched(t, "the restore stopped as the lock came")
			default:
				if err != nil {
					t.Fatalf("the restore answered %v", err)
				}
				b.requireRestored(t, snap)
			}

			s, held := backup.StuckCheckHere(root)
			if !held || !s.Restoring() || s.Snapshot != filepath.Base(snap) || s.Step != "letting go of the snapshots' lock" {
				t.Errorf("while the restore's unlock does not answer the folder reads %+v, %v; want the restore of %s, letting go of the lock",
					s, held, filepath.Base(snap))
			}
			words := "a restore of snapshot " + filepath.Base(snap)
			asked := time.Now()
			if err := backup.RestoreWithOptions(snap, c.targets, backup.RestoreOptions{LockWait: 10 * time.Second}); !errors.Is(err, backup.ErrCheckStillRunning) ||
				!strings.Contains(err.Error(), words) {
				t.Errorf("another restore while the unlock does not answer answered %v, want ErrCheckStillRunning naming %q", err, words)
			}
			if took := time.Since(asked); took > 5*time.Second {
				t.Errorf("another restore waited %v to be refused", took)
			}
			if pr, err := backup.Prune(root, 1); err != nil || !pr.Skipped || !errors.Is(pr.SkippedFor, backup.ErrCheckStillRunning) {
				t.Errorf("a prune while the unlock does not answer: %+v, %v; want it skipped for ErrCheckStillRunning", pr, err)
			}

			unlock.release()
			awaitClean(t)
			if s, held := backup.StuckCheckHere(root); held {
				t.Errorf("the folder is still marked %+v once the unlock ended", s)
			}
			if held, err := backupsLockHeld(root); err != nil || held {
				t.Errorf("the folder's lock once the unlock ended: held %v, %v; want it free", held, err)
			}
			if err := backup.RestoreWithOptions(snap, c.targets, backup.RestoreOptions{}); err != nil {
				t.Fatalf("a restore once the unlock ended answered %v", err)
			}
			c.requireRestored(t, snap)
			callers.wait(t)
		})
	}
}

// TestAStuckMarkSaysWhatHoldsTheFolder is backlog B826's second round (Codex
// on its brief, finding 2): a hold given up says what it is, a verification's
// snapshot, a prune or a restore of its snapshot, in its error
// (StillRunningError, which errors.Is reads as ErrCheckStillRunning) and in
// the marker another process reads, whose restore travels in a field of its
// own; a marker written before restores were marked holds none and reads as it
// always did, and HeldByRestore tells a restore's refusal apart.
func TestAStuckMarkSaysWhatHoldsTheFolder(t *testing.T) {
	since := time.Date(2026, 11, 12, 6, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name      string
		marker    string
		what      string
		restoring bool
		pruning   bool
	}{
		{"a verification, as written before restores were marked", `{"snapshot":"S","step":"its check","since":"2026-11-12T06:00:00Z"}`, "snapshot S", false, false},
		{"a prune", `{"snapshot":"","step":"its removals","since":"2026-11-12T06:00:00Z"}`, "a prune", false, true},
		{"a restore", `{"snapshot":"S","step":"its read of the snapshot it restores","since":"2026-11-12T06:00:00Z","restore":true}`, "a restore of snapshot S", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var s backup.StuckCheck
			if err := json.Unmarshal([]byte(tc.marker), &s); err != nil {
				t.Fatal(err)
			}
			if s.Restoring() != tc.restoring || s.Pruning() != tc.pruning || s.What() != tc.what || !s.Since.Equal(since) {
				t.Errorf("%s reads %+v: restoring %v, pruning %v, what %q", tc.marker, s, s.Restoring(), s.Pruning(), s.What())
			}
			again, err := json.Marshal(s)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(again), `"restore"`) != tc.restoring {
				t.Errorf("%+v is written %s", s, again)
			}

			// Another process's marker, read while that process holds the lock.
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, backup.StuckMarkerName), []byte(tc.marker), 0o644); err != nil {
				t.Fatal(err)
			}
			f, err := fsutil.OpenLockFile(root, backup.LockFileName)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			if err := fsutil.TryLock(f); err != nil {
				t.Fatal(err)
			}
			defer fsutil.Unlock(f)
			got, held := backup.StuckCheckOf(root, backup.ProbeByDoctor)
			if !held || got.Restoring() != tc.restoring || got.What() != tc.what {
				t.Errorf("StuckCheckOf = %+v, %v; want %q", got, held, tc.what)
			}
			err = fmt.Errorf("refused: %w", &backup.StillRunningError{Check: got})
			if !errors.Is(err, backup.ErrCheckStillRunning) || !strings.Contains(err.Error(), "("+tc.what+", given up in "+got.Step+" at 2026-11-12T06:00:00Z)") ||
				!strings.Contains(err.Error(), backup.ErrCheckStillRunning.Error()) || backup.HeldByRestore(err) != tc.restoring {
				t.Errorf("the refusal reads %q, restore %v", err, backup.HeldByRestore(err))
			}
		})
	}
	if backup.HeldByRestore(backup.ErrCheckStillRunning) || backup.HeldByRestore(nil) {
		t.Error("HeldByRestore took a refusal that names no restore for a restore's")
	}
}
