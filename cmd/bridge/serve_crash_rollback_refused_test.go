package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/supervision"
	"github.com/acoseac/1-bit-bridge/internal/updater"
	"github.com/acoseac/1-bit-bridge/internal/version"
)

// Backlog B760's first residual: a start whose settings the install's version
// refuses reached the early rollback alone, which leaves an install that a
// start of this version opened the data dir under. So an install whose count
// asked for the crash rollback exited 2 at every start for as long as its
// settings stayed refused, and its crash rollback was never asked; and a crash
// rollback's own record whose rename had not landed was left the same way.

// withSettingsThisVersionRefuses writes r's settings as this version refuses
// them: "load" with a leading zero, which the load refuses, and "serve" with
// two library roots of one folder name, which serve refuses past the load
// (serveConfigRefusal). The backup's settings check answers as a backup of a
// version that takes them does (runAsBridgeCheckEnv's "takes"), unless the
// test sets it otherwise after.
func (r *refusedInstall) withSettingsThisVersionRefuses(t *testing.T, refusal string) {
	t.Helper()
	switch refusal {
	case "load":
		r.writeSettings(t, "dataDir: "+r.dataDir+"\n", "scanIntervalSec: 010\n")
	case "serve":
		a, b := filepath.Join(r.dir, "a", "Music"), filepath.Join(r.dir, "b", "Music")
		for _, d := range []string{a, b} {
			if err := os.MkdirAll(d, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		body := fmt.Sprintf("libraryRoots:\n  - %s\n  - %s\ndataDir: %s\nlistenAddress: 127.0.0.1:0\nadminAddress: 127.0.0.1:0\ndisableHttp3: true\n",
			a, b, r.dataDir)
		if err := os.WriteFile(r.cfg, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("no refusal %q", refusal)
	}
	t.Setenv(runAsBridgeCheckEnv, "takes")
}

// crashedThreeTimes runs updater.ProbationFailedStarts starts of r's install
// that crash after their admission, which leaves the count at the crash
// rollback's limit and this version recorded as having opened the data dir, as
// production's admission writes them.
func (r *refusedInstall) crashedThreeTimes(t *testing.T) {
	t.Helper()
	for start := 1; start <= updater.ProbationFailedStarts; start++ {
		r.startThatPanics(t)
	}
	if st := markerOf(t, r.dataDir); st.FailedStarts != updater.ProbationFailedStarts || st.DataDirOpenedBy != version.ServerVersion {
		t.Fatalf("the marker after %d starts that crashed = %+v, want the count at the limit and this version's record of the data dir",
			updater.ProbationFailedStarts, st)
	}
}

// crashRollbackRecordedNotRenamed is crashedThreeTimes, then a start whose
// crash rollback records itself and whose rename answers nil with nothing
// renamed: the rollback's record durable and this version's binary in place,
// as a process stopped between the two leaves them, with no timing.
func (r *refusedInstall) crashRollbackRecordedNotRenamed(t *testing.T) {
	t.Helper()
	r.crashedThreeTimes(t)
	binaries := watch(t, r.bin, r.bak)
	if code, stderr := r.serve(t, serveOpts{restoreBackup: func(string, string) error { return nil }}); code != supervision.RestartExitCode {
		t.Fatalf("the start after the three = %d, want %d at its crash rollback; stderr=%s", code, supervision.RestartExitCode, stderr)
	}
	binaries.requireUnchanged(t)
	if st := markerOf(t, r.dataDir); st.Status != "" || !st.RollbackStarted || st.FailedStarts != updater.ProbationFailedStarts ||
		st.RejectedVersion != version.ServerVersion || st.MigrationVeto != version.ServerVersion {
		t.Fatalf("the marker after a crash rollback whose rename did not land = %+v, want its record, the count kept", st)
	}
}

// markerAtTheRename is a restore that records the install marker in r's data
// dir as the backup is put back, into at, then puts it back.
func markerAtTheRename(t *testing.T, r *refusedInstall, at *updater.State) func(dst, ext string) error {
	return func(dst, ext string) error {
		// On serve's goroutine: t.Error, never t.Fatal.
		st, err := updater.LoadState(r.dataDir)
		if err != nil {
			t.Error(err)
		}
		*at = st
		return updater.RollbackBinary(dst, ext)
	}
}

// requireTheCrashRollbacksPutBack requires a start that put the backup back by
// the crash rollback: its record durable before the rename (atRename: this
// version held back whatever the settings, its migrations vetoed, the count
// kept), the backup's version at the binary's path and no backup beside it,
// nothing of the start in the data dir, and the settings as they were.
func requireTheCrashRollbacksPutBack(t *testing.T, r *refusedInstall, atRename updater.State, settings watchedFiles, stderr string) {
	t.Helper()
	if atRename.Status != "" || !atRename.RollbackStarted || atRename.RejectedVersion != version.ServerVersion ||
		atRename.MigrationVeto != version.ServerVersion || atRename.RejectedConfigDigest != "" ||
		atRename.FailedStarts != updater.ProbationFailedStarts || !strings.Contains(atRename.RejectedReason, "did not end cleanly") {
		t.Errorf("the marker as the backup was put back = %+v, want the crash rollback's record", atRename)
	}
	if got := reportedVersion(t, r.bin); !strings.Contains(got, previousVersion) {
		t.Errorf("the binary reports %q after the rollback, want %s", got, previousVersion)
	}
	if _, err := os.Lstat(r.bak); !os.IsNotExist(err) {
		t.Errorf("the backup is still there (lstat %v): it was not put back", err)
	}
	requireNoStoreOpened(t, r.dataDir)
	settings.requireUnchanged(t)
	if said := updaterLines(stderr); !strings.Contains(said, "did not end cleanly, so the install is rolled back") {
		t.Errorf("the start did not say it rolled the install back; its updater lines: %q", said)
	}
}

// TestARefusedStartPutsTheBackupBackWhereItsCountAsksForTheCrashRollback:
// three starts of this version crashed after their admission, which recorded
// that it opened the data dir, and the next start refuses its settings: at
// the load, past it, and over an install older than the early rollback's
// window, of which the crash rollback asks no recency. The early rollback
// leaves such an install, and the crash rollback puts the backup back, its
// checks all asked, the backup's settings check of the settings as they are
// now, before anything is written, exit 75. Until B760 the start exited 2,
// and so did every start after it while the settings stayed refused.
func TestARefusedStartPutsTheBackupBackWhereItsCountAsksForTheCrashRollback(t *testing.T) {
	for _, tc := range []struct {
		name, refusal string
		old           bool
	}{
		{"refused at the load", "load", false},
		{"refused past the load", "serve", false},
		{"an install past the early rollback's window", "load", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRefusedInstall(t, "")
			r.crashedThreeTimes(t)
			if tc.old {
				st := markerOf(t, r.dataDir)
				st.AttemptedAt = time.Now().UTC().Add(-updater.RecencyWindow() - time.Hour)
				if err := updater.SaveState(r.dataDir, st); err != nil {
					t.Fatal(err)
				}
			}
			r.withSettingsThisVersionRefuses(t, tc.refusal)
			settings := watch(t, r.cfg)
			var atRename updater.State
			code, stderr := r.serve(t, serveOpts{restoreBackup: markerAtTheRename(t, r, &atRename)})
			if code != supervision.RestartExitCode {
				t.Fatalf("the start that refuses its settings = %d, want %d: %d starts before it did not end cleanly; stderr=%s",
					code, supervision.RestartExitCode, updater.ProbationFailedStarts, stderr)
			}
			requireTheCrashRollbacksPutBack(t, r, atRename, settings, stderr)
		})
	}
}

// TestARefusedStartFinishesACrashRollbackWhoseRenameDidNotLand: a crash
// rollback's record is durable before its rename, and a start stopped between
// the two leaves the record with its count, this version's binary in place.
// The next start, whose settings this version refuses, finishes the rollback:
// the early rollback leaves the record (a start of this version opened the
// data dir under it), and the crash rollback asks all its checks again, the
// backup's settings check of the settings as they are now. Until B760 it
// exited 2, the record left with the backup not back.
func TestARefusedStartFinishesACrashRollbackWhoseRenameDidNotLand(t *testing.T) {
	r := newRefusedInstall(t, "")
	r.crashRollbackRecordedNotRenamed(t)
	r.withSettingsThisVersionRefuses(t, "load")
	settings := watch(t, r.cfg)
	var atRename updater.State
	code, stderr := r.serve(t, serveOpts{restoreBackup: markerAtTheRename(t, r, &atRename)})
	if code != supervision.RestartExitCode {
		t.Fatalf("the start that refuses its settings over the record = %d, want %d; stderr=%s", code, supervision.RestartExitCode, stderr)
	}
	requireTheCrashRollbacksPutBack(t, r, atRename, settings, stderr)
}

// TestARefusedStartWhoseCrashRollbackRefusesKeepsTheInstall: where the crash
// rollback a start that refuses its settings asks refuses (the backup refuses
// the settings as they are now, over an install marker and over a record
// whose rename did not land; a migration of the store began), the start says
// why in the crash rollback's words and records it, as a start whose settings
// load does, and then refuses its settings as before, exit 2, the binaries and
// the rest of the marker as they were. Until B760 it said the early rollback's
// reason alone.
func TestARefusedStartWhoseCrashRollbackRefusesKeepsTheInstall(t *testing.T) {
	for _, tc := range []struct {
		name           string
		lay            func(r *refusedInstall, t *testing.T)
		check          string
		said, recorded string
	}{
		{"the backup refuses the settings too", (*refusedInstall).crashedThreeTimes, "refused",
			"refuses this bridge's settings", "refuses this bridge's settings, so it would not start on them: line 3"},
		{"the backup refuses them under a record whose rename did not land", (*refusedInstall).crashRollbackRecordedNotRenamed, "refused",
			"refuses this bridge's settings", "refuses this bridge's settings, so it would not start on them: line 3"},
		{"a migration of the store began", func(r *refusedInstall, t *testing.T) {
			r.crashedThreeTimes(t)
			st := markerOf(t, r.dataDir)
			st.StoreMigratedBy = version.ServerVersion
			if err := updater.SaveState(r.dataDir, st); err != nil {
				t.Fatal(err)
			}
		}, "takes", "began a migration", "began a migration"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRefusedInstall(t, "")
			tc.lay(r, t)
			r.withSettingsThisVersionRefuses(t, "load")
			t.Setenv(runAsBridgeCheckEnv, tc.check)
			before := markerOf(t, r.dataDir)
			binaries := watch(t, r.bin, r.bak)
			code, stderr := r.serve(t, serveOpts{})
			if code != 2 {
				t.Fatalf("the start that refuses its settings = %d, want 2, its refusal as before; stderr=%s", code, stderr)
			}
			binaries.requireUnchanged(t)
			requireRefusal(t, r, stderr, tc.said, tc.recorded)
			if said := updaterLines(stderr); !strings.Contains(said, "refuses its settings, as it did before") || strings.Contains(said, "goes on") {
				t.Errorf("the start that refuses its settings said it goes on, or not what it does; its updater lines: %q", said)
			}
			after := markerOf(t, r.dataDir)
			after.RollbackRefused = before.RollbackRefused
			if after != before {
				t.Errorf("the marker after the refused start = %+v, want it as it was but for the refusal recorded, %+v", after, before)
			}
			requireNoStoreOpened(t, r.dataDir)
		})
	}
}

// TestARefusedStartBelowTheCrashRollbacksLimitLeavesTheInstall: with fewer
// starts that did not end cleanly than the crash rollback's limit, a start
// that refuses its settings leaves the install as the early rollback always
// has: nothing written, the early rollback's reason said, nothing of the
// crash rollback.
func TestARefusedStartBelowTheCrashRollbacksLimitLeavesTheInstall(t *testing.T) {
	r := newRefusedInstall(t, "")
	for start := 1; start < updater.ProbationFailedStarts; start++ {
		r.startThatPanics(t)
	}
	r.withSettingsThisVersionRefuses(t, "load")
	files := watch(t, r.bin, r.bak, updater.StatePath(r.dataDir))
	code, stderr := r.serve(t, serveOpts{})
	if code != 2 {
		t.Fatalf("the start that refuses its settings below the limit = %d, want 2; stderr=%s", code, stderr)
	}
	files.requireUnchanged(t)
	if said := updaterLines(stderr); !strings.Contains(said, "opened the data dir") || strings.Contains(said, "did not end cleanly") {
		t.Errorf("the start below the limit did not give the early rollback's reason alone; its updater lines: %q", said)
	}
}
