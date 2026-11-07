package updater

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// Backlog B660's second round, in the updater: whether a serve holds the data
// dir's lock, the hold of a command's migration while the install awaits its
// confirmation after a start that did not end cleanly, and serve's own
// permission past it.

// holdServeLock holds dir's server.lock from another open of this process,
// as another process's serve would, until the test ends or release.
func holdServeLock(t *testing.T, dir string) (release func()) {
	t.Helper()
	f, err := fsutil.OpenLockFile(dir, ServeLockFileName)
	if err != nil {
		t.Fatal(err)
	}
	if err := fsutil.TryLock(f); err != nil {
		_ = f.Close()
		t.Skipf("no lock can be taken on %s here: %v", ServeLockFileName, err)
	}
	done := false
	release = func() {
		if !done {
			done = true
			_ = fsutil.Unlock(f)
			_ = f.Close()
		}
	}
	t.Cleanup(release)
	return release
}

// TestServeHoldsDataDirReadsTheLockAndThisProcesssHolds: a serve holds the
// data dir's lock where the lock is held by another open, unless this process
// holds it as a command; a hold this process noted as a serve's answers
// without the lock file; no lock file, and a lock that is free, are none.
func TestServeHoldsDataDirReadsTheLockAndThisProcesssHolds(t *testing.T) {
	dir := t.TempDir()
	if ServeHoldsDataDir(dir) {
		t.Fatal("a data dir with no lock file reads as held by a serve")
	}
	if _, err := os.Stat(filepath.Join(dir, ServeLockFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the question made the lock file (%v): it must make nothing", err)
	}
	release := holdServeLock(t, dir)
	if !ServeHoldsDataDir(dir) {
		t.Error("a data dir whose lock another open holds does not read as held by a serve")
	}
	unnote := NoteDataDirLock(dir, false)
	if ServeHoldsDataDir(dir) {
		t.Error("the lock this process holds as a command reads as a serve's")
	}
	unnote()
	unnote()
	if !ServeHoldsDataDir(dir) {
		t.Error("the lock still held, once the command's note is gone, does not read as held")
	}
	release()
	if ServeHoldsDataDir(dir) {
		t.Error("a free lock reads as held by a serve")
	}
	unnoteServe := NoteDataDirLock(dir+string(filepath.Separator), true)
	if !ServeHoldsDataDir(dir) {
		t.Error("a hold this process noted as a serve's, under another spelling of the data dir, does not read as one")
	}
	unnoteServe()
	if ServeHoldsDataDir(dir) {
		t.Error("a serve's note let go still reads as held")
	}
}

// TestMigrationHeldAsksTheStartsBeforeNow: a command's migration is held
// only where the install of its version awaits its confirmation, the crash
// rollback's checks at the limit would put the backup back, and a start
// before now did not end cleanly: the count less the start a serve running
// now counts.
func TestMigrationHeldAsksTheStartsBeforeNow(t *testing.T) {
	migrated, unswapped := probationMarker(2), probationMarker(2)
	migrated.StoreMigratedBy = "0.2.4"
	unswapped.SwapStarted = false
	for _, tc := range []struct {
		name         string
		st           State
		current      string
		serveRunning bool
		held         bool
	}{
		{"no install", State{}, "0.2.4", false, false},
		{"another version's install", probationMarker(2), "0.2.9", false, false},
		{"no start counted", probationMarker(0), "0.2.4", false, false},
		{"one start counted, no serve running", probationMarker(1), "0.2.4", false, true},
		{"one start counted, a serve's own", probationMarker(1), "0.2.4", true, false},
		{"two counted, one a serve's", probationMarker(2), "0.2.4", true, true},
		{"a migration begun already", migrated, "0.2.4", false, false},
		{"an install that never reached its swap", unswapped, "0.2.4", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			why := MigrationHeld(tc.st, tc.current, tc.serveRunning)
			if (why != "") != tc.held {
				t.Fatalf("MigrationHeld = %q, want held: %v", why, tc.held)
			}
			if tc.held && (!strings.Contains(why, "awaits its confirmation") || !strings.Contains(why, "0.2.3")) {
				t.Errorf("MigrationHeld = %q, want it to name the probation and the version it keeps", why)
			}
		})
	}
}

// TestAMigratingOpenIsHeldWhileTheInstallAwaitsItsConfirmation: a store open
// with a step pending and no permission refuses, writing nothing, while the
// install awaits its confirmation after a start that did not end cleanly,
// with the install marker's lock or without it; serve's own open, with its
// admission's permission, records its migration.
func TestAMigratingOpenIsHeldWhileTheInstallAwaitsItsConfirmation(t *testing.T) {
	t.Run("a command's", func(t *testing.T) {
		dir := t.TempDir()
		mustSave(t, dir, probationMarker(1))
		before, _ := os.ReadFile(StatePath(dir))
		if _, err := DisarmBeforeMigrating(dir, "0.2.4"); !errors.Is(err, ErrMigrationHeld) {
			t.Fatalf("a command's migrating open = %v, want it held", err)
		}
		if after, _ := os.ReadFile(StatePath(dir)); !bytes.Equal(before, after) {
			t.Error("the held open wrote the marker")
		}
	})
	t.Run("a command's, without the marker's lock", func(t *testing.T) {
		dir := t.TempDir()
		mustSave(t, dir, probationMarker(1))
		t.Cleanup(SetStateTryLockForTest(func(*os.File) error { return syscall.EIO }))
		if _, err := DisarmBeforeMigrating(dir, "0.2.4"); !errors.Is(err, ErrMigrationHeld) {
			t.Fatalf("a command's migrating open without the lock = %v, want it held", err)
		}
	})
	t.Run("serve's own", func(t *testing.T) {
		dir := t.TempDir()
		mustSave(t, dir, probationMarker(1))
		admission, err := AdmitBoot(t.Context(), dir, "0.2.4", nil)
		if err != nil {
			t.Fatal(err)
		}
		res, err := DisarmBeforeMigratingAdmitted(dir, "0.2.4", admission.Permit)
		if err != nil || !res.StoreRecorded {
			t.Fatalf("serve's migrating open = %+v, %v; want its migration recorded", res, err)
		}
	})
}

// TestThePermitAdmitsOnlyItsOwnStart: the admission's permission is for its
// data dir and its version alone, and the zero value admits nothing.
func TestThePermitAdmitsOnlyItsOwnStart(t *testing.T) {
	dir, other := t.TempDir(), t.TempDir()
	mustSave(t, dir, probationMarker(1))
	mustSave(t, other, probationMarker(1))
	admission, err := AdmitBoot(t.Context(), dir, "0.2.4", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DisarmBeforeMigratingAdmitted(other, "0.2.4", admission.Permit); !errors.Is(err, ErrMigrationHeld) {
		t.Errorf("another data dir's open with this start's permission = %v, want it held", err)
	}
	for _, p := range []MigrationPermit{{}, {dataDir: dir}, {dataDir: dir, version: "0.2.9"}} {
		if p.admits(dir, "0.2.4") {
			t.Errorf("the permission %+v admits a start of 0.2.4 in its data dir", p)
		}
	}
	if !admission.Permit.admits(dir+string(filepath.Separator), "v0.2.4") {
		t.Error("the admission's permission does not admit its own start under another spelling of its data dir and version")
	}
}

// TestCrashRollbackAtLimitAsksTheMarkerAtTheLimit: the crash rollback's
// checks of the marker as they would answer at the limit, whatever the count
// now.
func TestCrashRollbackAtLimitAsksTheMarkerAtTheLimit(t *testing.T) {
	if ok, why := CrashRollbackAtLimit(probationMarker(0), "0.2.4"); !ok {
		t.Errorf("an install with no start counted, at the limit = %q, want it taken", why)
	}
	migrated := probationMarker(0)
	migrated.StoreMigratedBy = "0.2.4"
	if ok, why := CrashRollbackAtLimit(migrated, "0.2.4"); ok || !strings.Contains(why, "began a migration") {
		t.Errorf("a migrated store at the limit = %v, %q; want the migration's refusal", ok, why)
	}
	if ok, _ := CrashRollbackAtLimit(probationMarker(5), "0.2.4"); !ok {
		t.Error("a count past the limit is not taken as it is")
	}
}
