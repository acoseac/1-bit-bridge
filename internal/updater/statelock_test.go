package updater

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// The install marker's lock (backlog B613's round 3): one persistent file
// beside the marker, never the marker itself, which the early rollback, a
// migrating store open and serve's first disarm share.

// shortStateLockWait makes LockState's wait d for the rest of the test.
func shortStateLockWait(t *testing.T, d time.Duration) {
	t.Helper()
	t.Cleanup(SetStateLockWaitForTest(d))
}

// stubStateTryLock makes every try at the lock answer err for the rest of
// the test.
func stubStateTryLock(t *testing.T, err error) {
	t.Helper()
	stateLockHookMu.Lock()
	prev := stateTryLock
	stateTryLock = func(*os.File) error { return err }
	stateLockHookMu.Unlock()
	t.Cleanup(func() {
		stateLockHookMu.Lock()
		stateTryLock = prev
		stateLockHookMu.Unlock()
	})
}

func TestTheInstallMarkersLockIsAFileOfItsOwn(t *testing.T) {
	dir := t.TempDir()
	shortStateLockWait(t, 200*time.Millisecond)
	first, err := LockState(context.Background(), dir)
	if err != nil || !first.Held() {
		t.Fatalf("LockState = %+v, %v; want the lock held", first, err)
	}
	if fi, err := os.Lstat(filepath.Join(dir, stateLockFileName)); err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("the lock file: %v (%v), want a regular file beside the marker", fi, err)
	}
	// A write of the marker renames a new file over it, and the lock holds.
	if err := SaveState(dir, armedForBarrier()); err != nil {
		t.Fatal(err)
	}
	waited := 0
	t.Cleanup(SetStateLockWaitingForTest(func(string) { waited++ }))
	second, err := LockState(context.Background(), dir)
	if !errors.Is(err, ErrStateLockHeld) || second != nil {
		t.Fatalf("a second LockState while the first holds = %+v, %v; want ErrStateLockHeld after the wait", second, err)
	}
	if waited != 1 {
		t.Errorf("the second taker said it waited %d times, want once", waited)
	}
	first.Release()
	if _, err := os.Lstat(filepath.Join(dir, stateLockFileName)); err != nil {
		t.Fatalf("the lock file after its release: %v, want it left where it is", err)
	}
	third, err := LockState(context.Background(), dir)
	if err != nil || !third.Held() {
		t.Fatalf("LockState once released = %+v, %v; want the lock held", third, err)
	}
	third.Release()
	third.Release()
}

func TestTheInstallMarkersLockSortsEachAnswer(t *testing.T) {
	shortStateLockWait(t, 100*time.Millisecond)
	for _, tc := range []struct {
		name    string
		err     error
		held    bool
		refused bool
	}{
		{"held by another open", errLockedForTest(), false, true},
		{"no lock to give", syscall.ENOLCK, false, true},
		{"a filesystem that keeps none", syscall.EOPNOTSUPP, false, false},
		{"a platform with none", errors.ErrUnsupported, false, false},
		{"an I/O error", syscall.EIO, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubStateTryLock(t, tc.err)
			l, err := LockState(context.Background(), t.TempDir())
			switch {
			case tc.refused && err == nil:
				t.Fatalf("LockState = %+v, want a refusal", l)
			case tc.refused:
				var sle *StateLockError
				if !errors.As(err, &sle) {
					t.Fatalf("LockState = %v, want a *StateLockError", err)
				}
			case err != nil || l.Held() || l.Unheld() == nil:
				t.Fatalf("LockState = %+v, %v; want a hold that is not held, saying why", l, err)
			}
		})
	}
}

// errLockedForTest is what TryLock answers for a lock another open holds.
func errLockedForTest() error {
	return &os.PathError{Op: "flock", Path: stateLockFileName, Err: fsutil.ErrLocked}
}

// TestADisarmWithoutTheLockWritesNothing: a disarm that cannot take the
// install marker's lock (held by another open past the wait, no lock to
// give, a failure) records nothing and removes nothing, refusing where an
// install is armed and its record due, and goes on as before where nothing
// is armed; on a filesystem that keeps no locks, where no process can hold
// one, it goes on and records.
func TestADisarmWithoutTheLockWritesNothing(t *testing.T) {
	shortStateLockWait(t, 100*time.Millisecond)
	for _, tc := range []struct {
		name string
		err  error
		// records says the disarm goes on and records over an armed install.
		records bool
	}{
		{"held by another open", errLockedForTest(), false},
		{"no lock to give", syscall.ENOLCK, false},
		{"an I/O error", syscall.EIO, false},
		{"a filesystem that keeps none", syscall.EOPNOTSUPP, true},
	} {
		for _, d := range theDisarms {
			t.Run(tc.name+"/"+d.name, func(t *testing.T) {
				stubStateTryLock(t, tc.err)
				dir := t.TempDir()
				if err := SaveState(dir, armedForBarrier()); err != nil {
					t.Fatal(err)
				}
				before, err := os.ReadFile(StatePath(dir))
				if err != nil {
					t.Fatal(err)
				}
				res, err := d.run(dir, "0.2.3")
				after, _ := os.ReadFile(StatePath(dir))
				switch {
				case tc.records && (err != nil || !res.Recorded || res.NoLocks == nil):
					t.Errorf("an armed install where no lock can be had = %+v, %v; want it recorded, saying no lock was had", res, err)
				case !tc.records && err == nil:
					t.Errorf("an armed install without the lock = %+v, want a refusal", res)
				case !tc.records && string(after) != string(before):
					t.Errorf("a disarm without the lock wrote the marker: %s", after)
				}

				// Nothing armed: the start goes on as before, lock or none.
				clear := t.TempDir()
				if res, err := d.run(clear, "0.2.3"); err != nil || res.Recorded {
					t.Errorf("no install armed, without the lock = %+v, %v; want it to go on", res, err)
				}
			})
		}
	}
}

// TestAMigrationRefusesWhileARollbackOfItsVersionIsUnderWay: the early
// rollback's record of an install of this version, its backup perhaps
// already back, stops a migration of this version's: the binary on disk
// may be the version before it, which must not find its store migrated
// past it. The backup's own version, and any other, migrates; serve's
// admission records that this version went on past its settings, which
// ends the rollback for good, and a migration may run after it.
func TestAMigrationRefusesWhileARollbackOfItsVersionIsUnderWay(t *testing.T) {
	record := RollbackRecord(armedForBarrier(), "0.2.3", "0.2.3 refused this bridge's settings: line 4: x", "a digest")
	for _, tc := range []struct {
		name    string
		current string
		refused bool
	}{
		{"the version rolled back", "0.2.3", true},
		{"the version rolled back, spelled with a v", "v0.2.3", true},
		{"the backup's version", "0.2.1", false},
		{"another version", "0.2.9", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := SaveState(dir, record); err != nil {
				t.Fatal(err)
			}
			if got := MigrationVetoed(record, tc.current); got != tc.refused {
				t.Errorf("MigrationVetoed = %v, want %v", got, tc.refused)
			}
			before, _ := os.ReadFile(StatePath(dir))
			_, err := DisarmBeforeMigrating(dir, tc.current)
			if tc.refused != errors.Is(err, ErrMigrationVetoed) {
				t.Fatalf("DisarmBeforeMigrating = %v, want refused %v", err, tc.refused)
			}
			if after, _ := os.ReadFile(StatePath(dir)); tc.refused && string(after) != string(before) {
				t.Errorf("a refused migration wrote the marker: %s", after)
			}
		})
	}
	t.Run("after serve's admission", func(t *testing.T) {
		dir := t.TempDir()
		if err := SaveState(dir, record); err != nil {
			t.Fatal(err)
		}
		res, err := AdmitBoot(context.Background(), dir, "0.2.3", nil)
		if err != nil || !res.Recorded {
			t.Fatalf("AdmitBoot over the record = %+v, %v; want this version recorded", res, err)
		}
		st, err := LoadState(dir)
		if err != nil {
			t.Fatal(err)
		}
		if MigrationVetoed(st, "0.2.3") {
			t.Errorf("the record after the admission %+v still reads as a rollback under way", st)
		}
		if _, err := DisarmBeforeMigrating(dir, "0.2.3"); err != nil {
			t.Errorf("a migration after the admission = %v, want it to go on", err)
		}
	})
	if MigrationVetoed(State{Status: "installing", TargetVersion: "0.2.3", SwapStarted: true}, "0.2.3") {
		t.Error("an install marker reads as a rollback under way")
	}
	if MigrationVetoed(State{RejectedVersion: "0.2.3"}, "0.2.3") {
		t.Error("a hold alone reads as a rollback under way")
	}
}
