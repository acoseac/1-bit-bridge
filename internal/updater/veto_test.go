package updater

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// The migration veto (backlog B613's round 4): the early rollback's record
// names the version it rolls back, whose store migrations are refused from
// then on, past the housekeeping of the version put back that finishes the
// record, until that version's own start gets past its settings or a new
// install replaces the marker.

// finishedRollback is the marker the version put back leaves once its
// housekeeping has finished the early rollback of 0.2.3 over 0.2.1: the
// install cleared, the hold and the veto kept.
func finishedRollback(t *testing.T, dir string) State {
	t.Helper()
	record := RollbackRecord(armedForBarrier(), "0.2.3", "0.2.3 refused this bridge's settings: line 4: x", "a digest")
	if err := SaveState(dir, record); err != nil {
		t.Fatal(err)
	}
	if err := ClearInstall(dir, record); err != nil {
		t.Fatal(err)
	}
	st, err := LoadState(dir)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// TestTheMigrationVetoOutlivesTheRollbacksHousekeeping: the early rollback's
// record vetoes the store migrations of the version it rolls back, and the
// veto stays through what the version put back does at its start (its
// admission, its own migrations, the housekeeping's clear of the install) and
// through a refusal recorded since. A command of the version rolled back,
// still running from its own image, then refuses to migrate, holding the
// marker's lock or not; the version put back, and any other, migrates.
func TestTheMigrationVetoOutlivesTheRollbacksHousekeeping(t *testing.T) {
	record := RollbackRecord(armedForBarrier(), "0.2.3", "0.2.3 refused this bridge's settings: line 4: x", "a digest")
	if record.MigrationVeto != "0.2.3" {
		t.Fatalf("the early rollback's record = %+v, want it to veto 0.2.3's migrations", record)
	}
	dir := t.TempDir()
	if err := SaveState(dir, record); err != nil {
		t.Fatal(err)
	}
	// The version put back starts: its admission and its own migration.
	if _, err := AdmitBoot(context.Background(), dir, "0.2.1", nil); err != nil {
		t.Fatalf("the admission of the version put back = %v", err)
	}
	if _, err := DisarmBeforeMigrating(dir, "0.2.1"); err != nil {
		t.Fatalf("a migration of the version put back = %v, want it to go on", err)
	}
	// Its housekeeping finishes the record, and a refusal is recorded since.
	st, err := LoadState(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := ClearInstall(dir, st); err != nil {
		t.Fatal(err)
	}
	if err := recordRejection(dir, "0.2.4", "0.2.4 refuses this bridge's settings: line 9: y", "another digest"); err != nil {
		t.Fatal(err)
	}
	if st, err = LoadState(dir); err != nil {
		t.Fatal(err)
	}
	if InstallArmed(st) || st.MigrationVeto != "0.2.3" {
		t.Fatalf("the marker after the version put back started = %+v, want no install armed and 0.2.3's migrations vetoed", st)
	}
	before, err := os.ReadFile(StatePath(dir))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		lock error
	}{
		{"holding the marker's lock", nil},
		{"on a filesystem that keeps none", syscall.EOPNOTSUPP},
		{"without the marker's lock", syscall.EIO},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.lock != nil {
				stubStateTryLock(t, tc.lock)
			}
			if _, err := DisarmBeforeMigrating(dir, "0.2.3"); !errors.Is(err, ErrMigrationVetoed) {
				t.Errorf("a migration of the version rolled back = %v, want it vetoed", err)
			}
			if after, _ := os.ReadFile(StatePath(dir)); !bytes.Equal(after, before) {
				t.Errorf("a vetoed migration wrote the marker: %s", after)
			}
		})
	}
	if _, err := DisarmBeforeMigrating(dir, "v0.2.3"); !errors.Is(err, ErrMigrationVetoed) {
		t.Errorf("a migration of the version rolled back, spelled with a v = %v, want it vetoed", err)
	}
	for _, v := range []string{"0.2.1", "0.2.9"} {
		if _, err := DisarmBeforeMigrating(dir, v); err != nil {
			t.Errorf("a migration of %s = %v, want it to go on", v, err)
		}
	}
}

// TestOnlyTheVetoedVersionsOwnStartOrAReinstallLiftsTheVeto: serve's
// admission of the vetoed version, a start that got past its settings, lifts
// the veto, recording over an armed install in the same write, under the
// install marker's lock (one it cannot take refuses and writes nothing); the
// version put back and any other leave it; and a new install's marker, a
// fresh one, carries none.
func TestOnlyTheVetoedVersionsOwnStartOrAReinstallLiftsTheVeto(t *testing.T) {
	t.Run("the version put back, and another", func(t *testing.T) {
		dir := t.TempDir()
		finishedRollback(t, dir)
		before, _ := os.ReadFile(StatePath(dir))
		for _, v := range []string{"0.2.1", "0.2.9"} {
			if res, err := AdmitBoot(context.Background(), dir, v, nil); err != nil || res.VetoLifted {
				t.Errorf("the admission of %s = %+v, %v; want it to leave the veto", v, res, err)
			}
		}
		if after, _ := os.ReadFile(StatePath(dir)); !bytes.Equal(after, before) {
			t.Errorf("another version's admission wrote the marker: %s", after)
		}
	})
	t.Run("the vetoed version's admission", func(t *testing.T) {
		dir := t.TempDir()
		finished := finishedRollback(t, dir)
		res, err := AdmitBoot(context.Background(), dir, "0.2.3", nil)
		if err != nil || !res.VetoLifted {
			t.Fatalf("the vetoed version's admission = %+v, %v; want the veto lifted", res, err)
		}
		st, err := LoadState(dir)
		if err != nil {
			t.Fatal(err)
		}
		want := finished
		want.MigrationVeto = ""
		if st != want {
			t.Errorf("the marker after the admission = %+v, want %+v: the veto alone lifted", st, want)
		}
		if _, err := DisarmBeforeMigrating(dir, "0.2.3"); err != nil {
			t.Errorf("a migration after the admission = %v, want it to go on", err)
		}
	})
	t.Run("over the rollback's record, the backup not back", func(t *testing.T) {
		dir := t.TempDir()
		record := RollbackRecord(armedForBarrier(), "0.2.3", "0.2.3 refused this bridge's settings: line 4: x", "a digest")
		if err := SaveState(dir, record); err != nil {
			t.Fatal(err)
		}
		res, err := AdmitBoot(context.Background(), dir, "0.2.3", nil)
		if err != nil || !res.Recorded || !res.VetoLifted {
			t.Fatalf("the admission over the record = %+v, %v; want this version recorded and the veto lifted", res, err)
		}
		if st, err := LoadState(dir); err != nil || st.DataDirOpenedBy != "0.2.3" || st.MigrationVeto != "" {
			t.Errorf("the marker after the admission = %+v (%v), want 0.2.3 recorded and no veto", st, err)
		}
	})
	t.Run("without the marker's lock", func(t *testing.T) {
		shortStateLockWait(t, 100*time.Millisecond)
		for _, lockErr := range []error{errLockedForTest(), syscall.ENOLCK, syscall.EIO} {
			dir := t.TempDir()
			finishedRollback(t, dir)
			before, _ := os.ReadFile(StatePath(dir))
			restore := SetStateTryLockForTest(func(*os.File) error { return lockErr })
			res, err := AdmitBoot(context.Background(), dir, "0.2.3", nil)
			restore()
			if err == nil || res.VetoLifted {
				t.Errorf("the vetoed version's admission without the lock (%v) = %+v, %v; want a refusal", lockErr, res, err)
			}
			if after, _ := os.ReadFile(StatePath(dir)); !bytes.Equal(after, before) {
				t.Errorf("an admission without the lock (%v) wrote the marker: %s", lockErr, after)
			}
		}
	})
	t.Run("on a filesystem that keeps no locks", func(t *testing.T) {
		stubStateTryLock(t, syscall.EOPNOTSUPP)
		dir := t.TempDir()
		finishedRollback(t, dir)
		if res, err := AdmitBoot(context.Background(), dir, "0.2.3", nil); err != nil || !res.VetoLifted || res.NoLocks == nil {
			t.Errorf("the admission where no lock can be had = %+v, %v; want the veto lifted, saying no lock was had", res, err)
		}
	})
	t.Run("a reinstall", func(t *testing.T) {
		fix := newInstallFixture(t, "0.2.3")
		dir := t.TempDir()
		finishedRollback(t, dir)
		live := filepath.Join(dir, "bridge")
		if err := os.WriteFile(live, []byte("bridge-binary-0.2.1"), 0o755); err != nil {
			t.Fatal(err)
		}
		upd := New(Options{RepoOverride: "fake/repo", Client: NewClient("fake/repo", time.Second).WithBaseURL(fix.server.URL)})
		upd.mu.Lock()
		upd.status.CurrentVersion, upd.status.LatestVersion, upd.status.UpdateAvailable = "0.2.1", "0.2.3", true
		upd.mu.Unlock()
		if _, err := upd.Install(context.Background(), InstallOptions{DataDir: dir, BinaryPath: live, Force: true, Verifier: noopVerifier}); err != nil {
			t.Fatalf("Install = %v", err)
		}
		if st, err := LoadState(dir); err != nil || st.MigrationVeto != "" || st.Status != "installing" {
			t.Errorf("the marker after a reinstall = %+v (%v), want a fresh install marker with no veto", st, err)
		}
	})
}

// TestADisarmStoppedInItsWaitReturnsTheStop: serve's admission, stopped
// while it waits for the install marker's lock, returns the stop and writes
// nothing, never the answer of a disarm that could not take the lock, which
// for no install armed would have gone on as if the start could.
func TestADisarmStoppedInItsWaitReturnsTheStop(t *testing.T) {
	for _, tc := range []struct {
		name   string
		marker *State
	}{
		{"no install armed", nil},
		{"an armed install", func() *State { st := armedForBarrier(); return &st }()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.marker != nil {
				if err := SaveState(dir, *tc.marker); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := os.ReadFile(StatePath(dir))
			shortStateLockWait(t, 2*time.Second)
			holder, err := LockState(context.Background(), dir)
			if err != nil || !holder.Held() {
				t.Fatalf("hold the lock: %+v, %v", holder, err)
			}
			t.Cleanup(holder.Release)
			waits := make(chan struct{}, 1)
			t.Cleanup(SetStateLockWaitingForTest(func(string) {
				select {
				case waits <- struct{}{}:
				default:
				}
			}))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			type admitted struct {
				res DisarmResult
				err error
			}
			got := make(chan admitted, 1)
			go func() {
				res, err := AdmitBoot(ctx, dir, "0.2.3", nil)
				got <- admitted{res, err}
			}()
			select {
			case <-waits:
			case <-time.After(20 * time.Second):
				t.Fatal("the admission never waited for the held lock")
			}
			cancel()
			a := <-got
			if !errors.Is(a.err, context.Canceled) || a.res != (DisarmResult{}) {
				t.Errorf("the admission stopped in its wait = %+v, %v; want the stop alone", a.res, a.err)
			}
			if after, _ := os.ReadFile(StatePath(dir)); !bytes.Equal(after, before) {
				t.Errorf("a stopped admission wrote the marker: %s", after)
			}
		})
	}
}
