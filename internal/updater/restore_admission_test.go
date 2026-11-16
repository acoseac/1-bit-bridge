package updater

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"syscall"
	"testing"
	"time"
)

// theRestores are a restore of the store's two asks of the install marker,
// each under its hold (HoldMarkerForRestore): its admission, before it
// writes anything, which records nothing, and its record, just before its
// first write to the restored files.
var theRestores = []struct {
	name    string
	run     func(dataDir, current string) error
	records bool
}{
	{"the restore's admission", func(dataDir, current string) error {
		h, err := HoldMarkerForRestore(context.Background(), dataDir, current)
		h.Release()
		return err
	}, false},
	{"the restore's record", func(dataDir, current string) error {
		h, err := HoldMarkerForRestore(context.Background(), dataDir, current)
		if err != nil {
			return err
		}
		defer h.Release()
		_, err = h.Record()
		return err
	}, true},
}

// TestARestoreByTheVersionAnInstallReplacesIsRefused: over an armed install
// of 0.2.3 over 0.2.1, a restore of the store run as 0.2.1, however spelled,
// is refused (ErrRestoreByPreviousVersion) with the marker as it was, by its
// admission and by its record alike: a migration of 0.2.1's needs no record,
// but a restore may put 0.2.3's store in place, and a rollback of the install
// would then put 0.2.1 back over it with nothing recorded to stop it. The
// install's version and any other are admitted, the record recording them; a
// box with no install armed admits 0.2.1; and a store's open run as 0.2.1
// still migrates, writing nothing, as a rollback to it does.
func TestARestoreByTheVersionAnInstallReplacesIsRefused(t *testing.T) {
	for _, r := range theRestores {
		for _, tc := range []struct {
			current string
			refused bool
		}{
			{"0.2.1", true},
			{"v0.2.1", true},
			{"0.2.3", false},
			{"0.2.9", false},
		} {
			t.Run(r.name+"/"+tc.current, func(t *testing.T) {
				dir := t.TempDir()
				if err := SaveState(dir, armedForBarrier()); err != nil {
					t.Fatal(err)
				}
				before, err := os.ReadFile(StatePath(dir))
				if err != nil {
					t.Fatal(err)
				}
				err = r.run(dir, tc.current)
				after, _ := os.ReadFile(StatePath(dir))
				switch {
				case tc.refused && !errors.Is(err, ErrRestoreByPreviousVersion):
					t.Fatalf("a restore run as %s over an install of 0.2.3 over 0.2.1 = %v, want ErrRestoreByPreviousVersion", tc.current, err)
				case tc.refused && !bytes.Equal(before, after):
					t.Errorf("the refused restore wrote the marker: %s", after)
				case !tc.refused && err != nil:
					t.Fatalf("a restore run as %s = %v, want it admitted", tc.current, err)
				case !tc.refused && !r.records && !bytes.Equal(before, after):
					t.Errorf("the restore's admission wrote the marker: %s", after)
				case !tc.refused && r.records:
					st, err := LoadState(dir)
					if err != nil || st.StoreMigratedBy != tc.current || st.DataDirOpenedBy != tc.current {
						t.Errorf("the marker after the restore's record = %+v, %v; want %s recorded", st, err, tc.current)
					}
				}
			})
		}
		t.Run(r.name+"/no install armed", func(t *testing.T) {
			if err := r.run(t.TempDir(), "0.2.1"); err != nil {
				t.Errorf("a restore run as 0.2.1 with no install armed = %v, want it admitted", err)
			}
		})
	}
	t.Run("a store's open run as the version replaced", func(t *testing.T) {
		dir := t.TempDir()
		if err := SaveState(dir, armedForBarrier()); err != nil {
			t.Fatal(err)
		}
		before, _ := os.ReadFile(StatePath(dir))
		if res, err := DisarmBeforeMigrating(dir, "0.2.1"); err != nil || res.StoreRecorded || res.Recorded {
			t.Errorf("a migrating open run as 0.2.1 = %+v, %v; want it admitted, nothing recorded", res, err)
		}
		if after, _ := os.ReadFile(StatePath(dir)); !bytes.Equal(before, after) {
			t.Errorf("the migrating open wrote the marker: %s", after)
		}
	})
}

// TestARestoreByTheVersionAnInstallReplacesIsRefusedWithoutTheLock: a
// restore refuses where the install marker's lock cannot be held
// (ErrMarkerLockUnavailable), whatever the marker holds and where there is
// none, writing nothing: held by another open past the wait, no lock to
// give, a failure, and a filesystem that keeps none. Until B653 PR 2a's third
// round a disarm that could not take the lock went on where it had nothing
// to write, an absent marker among them, and an installer that held the lock
// could arm its install once the restore went on.
func TestARestoreByTheVersionAnInstallReplacesIsRefusedWithoutTheLock(t *testing.T) {
	shortStateLockWait(t, 100*time.Millisecond)
	markers := []struct {
		name  string
		state *State
	}{
		{"no marker", nil},
		{"an install over this version", &State{Status: "installing", TargetVersion: "0.2.3", PreviousVersion: "0.2.1",
			AttemptedAt: time.Now().UTC().Add(-time.Minute), SwapStarted: true}},
		{"an install of this version", &State{Status: "installing", TargetVersion: "0.2.1", PreviousVersion: "0.2.0",
			AttemptedAt: time.Now().UTC().Add(-time.Minute), SwapStarted: true}},
	}
	check := func(t *testing.T, dir string, run func(dataDir, current string) error) {
		t.Helper()
		before, beforeErr := os.ReadFile(StatePath(dir))
		err := run(dir, "0.2.1")
		if !errors.Is(err, ErrMarkerLockUnavailable) {
			t.Fatalf("a restore without the marker's lock held = %v, want ErrMarkerLockUnavailable", err)
		}
		after, afterErr := os.ReadFile(StatePath(dir))
		if !bytes.Equal(before, after) || errors.Is(beforeErr, fs.ErrNotExist) != errors.Is(afterErr, fs.ErrNotExist) {
			t.Errorf("the refused restore changed the marker: %q (%v), was %q (%v)", after, afterErr, before, beforeErr)
		}
	}
	for _, m := range markers {
		for _, r := range theRestores {
			t.Run("held by another open/"+m.name+"/"+r.name, func(t *testing.T) {
				dir := t.TempDir()
				if m.state != nil {
					if err := SaveState(dir, *m.state); err != nil {
						t.Fatal(err)
					}
				}
				holder, err := LockState(context.Background(), dir)
				if err != nil || !holder.Held() {
					t.Fatalf("hold the marker's lock: %+v, %v", holder, err)
				}
				defer holder.Release()
				check(t, dir, r.run)
			})
		}
	}
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"no lock to give", syscall.ENOLCK},
		{"an I/O error", syscall.EIO},
		{"a filesystem that keeps none", syscall.EOPNOTSUPP},
	} {
		for _, m := range markers {
			for _, r := range theRestores {
				t.Run(tc.name+"/"+m.name+"/"+r.name, func(t *testing.T) {
					stubStateTryLock(t, tc.err)
					dir := t.TempDir()
					if m.state != nil {
						if err := SaveState(dir, *m.state); err != nil {
							t.Fatal(err)
						}
					}
					check(t, dir, r.run)
				})
			}
		}
	}
}

// TestARestoreHoldsTheMarkersLockUntilItLetsItGo: while a restore holds the
// install marker's lock, another taker (an install, a migration) cannot take
// it; the record under the hold takes it no second time, so it answers within
// a wait shorter than any taker's; and once the hold is let go, twice or
// more, the lock is free.
func TestARestoreHoldsTheMarkersLockUntilItLetsItGo(t *testing.T) {
	shortStateLockWait(t, 200*time.Millisecond)
	dir := t.TempDir()
	if err := SaveState(dir, State{Status: "installing", TargetVersion: "0.2.3", PreviousVersion: "0.2.1",
		AttemptedAt: time.Now().UTC().Add(-time.Minute), SwapStarted: true}); err != nil {
		t.Fatal(err)
	}
	h, err := HoldMarkerForRestore(context.Background(), dir, "0.2.3")
	if err != nil {
		t.Fatal(err)
	}
	if other, err := LockState(context.Background(), dir); !errors.Is(err, ErrStateLockHeld) {
		other.Release()
		t.Fatalf("another taker while the restore holds the marker's lock = %v, want ErrStateLockHeld", err)
	}
	if res, err := h.Record(); err != nil || !res.StoreRecorded {
		t.Fatalf("the record under the hold = %+v, %v; want it recorded without the lock taken again", res, err)
	}
	h.Release()
	h.Release()
	other, err := LockState(context.Background(), dir)
	if err != nil || !other.Held() {
		t.Fatalf("a taker once the restore let the lock go = %+v, %v; want it held", other, err)
	}
	other.Release()
}
