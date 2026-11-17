package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/doctor"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/supervision"
	"github.com/acoseac/1-bit-bridge/internal/version"
)

// standJournal stands an unfinished database restore's journal in dataDir,
// as B653 PR 2b's restore leaves one (this PR's checks are tested with one
// the test writes).
func standJournal(t *testing.T, dataDir string) {
	t.Helper()
	if err := os.WriteFile(manifest.RestoreJournalPath(manifest.DefaultDBPath(dataDir)), []byte(`{"format":1}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// journalWords are the words of an unfinished restore's refusal.
const journalWords = "an unfinished restore of the library database stands (bridge.db-restore.json)"

// TestTheFullRestoreRefusesWhileADatabaseRestoreIsUnfinished (T27, the
// command's half): bridge restore over a data dir whose database restore did
// not finish exits 1 in the journal's words, the database as it was.
func TestTheFullRestoreRefusesWhileADatabaseRestoreIsUnfinished(t *testing.T) {
	cfgPath, dataDir, snap := seedRestorableInstall(t, closedAdminAddr(t))
	standJournal(t, dataDir)
	db := manifest.DefaultDBPath(dataDir)
	before, err := os.ReadFile(db)
	if err != nil {
		t.Fatal(err)
	}
	code, _, stderr := runRestoreYes(t, cfgPath, snap)
	if code != 1 || !strings.Contains(stderr, journalWords) || !strings.Contains(stderr, "nothing was restored") {
		t.Fatalf("bridge restore over an unfinished database restore = %d, want 1 in the journal's words\nstderr:\n%s", code, stderr)
	}
	if after, _ := os.ReadFile(db); !bytes.Equal(before, after) {
		t.Error("the refused restore wrote the database")
	}
}

// TestRestoreFindsAnUnfinishedRestoreBeforeItsSnapshot (T37): with
// a journal standing and the snapshot's folder gone, bridge restore exits 1
// in the journal's words, not 2 for a snapshot it does not recognise: it
// asks the journal before it looks at any snapshot, and its look's worker
// never runs. Until B653 PR 2a the look came first (CheckRestoreSource), so
// a resume would have needed the snapshot.
func TestRestoreFindsAnUnfinishedRestoreBeforeItsSnapshot(t *testing.T) {
	cfgPath, dataDir, snap := seedRestorableInstall(t, closedAdminAddr(t))
	standJournal(t, dataDir)
	if err := os.RemoveAll(snap); err != nil {
		t.Fatal(err)
	}
	var looked atomic.Int32
	t.Cleanup(backup.SetOnSourceWorkerEndForTest(func(string) { looked.Add(1) }))
	var stdout, stderr strings.Builder
	code := restoreCmd(context.Background(), []string{"--config", cfgPath, snap}, strings.NewReader(""), &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), journalWords) || strings.Contains(stderr.String(), "not a recognized snapshot") {
		t.Fatalf("bridge restore over an unfinished restore whose snapshot is gone = %d, want 1 in the journal's words\nstderr:\n%s", code, stderr.String())
	}
	if n := looked.Load(); n != 0 {
		t.Errorf("the snapshot's look ran %d times before the journal refused the restore", n)
	}
}

// TestTheRollbacksRefuseWhileADatabaseRestoreIsUnfinished (T28): serve's crash
// rollback at its count's limit, its early rollback over settings this
// version refuses, and the crash rollback a refused start hands an install
// to, each leave the install as it is while an unfinished database restore's
// journal stands: no marker write of theirs, no rename, the backup where it
// was, and the start's line saying the rollback waits for the restore. The
// crash rollback's start then meets the journal at its store's open and exits
// 1; the refused starts exit with their settings' refusal, never 75.
func TestTheRollbacksRefuseWhileADatabaseRestoreIsUnfinished(t *testing.T) {
	requireLeft := func(t *testing.T, r *refusedInstall, binaries watchedFiles, stderr string) {
		t.Helper()
		requireInstallKept(t, binaries, r)
		if lines := updaterLines(stderr); !strings.Contains(lines, "the rollback waits for the unfinished restore") {
			t.Errorf("the start did not say the rollback waits for the unfinished restore; its updater lines: %q", lines)
		}
		st := markerOf(t, r.dataDir)
		if st.RollbackRefused != "" || st.RollbackStarted || st.RejectedVersion != "" || st.MigrationVeto != "" {
			t.Errorf("the marker after a start over an unfinished restore = %+v, want no write of the rollback's", st)
		}
	}

	t.Run("the crash rollback at its limit", func(t *testing.T) {
		r := newRefusedInstall(t, "")
		r.crashedThreeTimes(t)
		standJournal(t, r.dataDir)
		binaries := watch(t, r.bin, r.bak)
		code, stderr := r.serve(t, serveOpts{})
		if code != 1 || !strings.Contains(stderr, journalWords) {
			t.Fatalf("the start at the crash rollback's limit over an unfinished restore = %d, want 1 at its store's open; stderr=%s", code, stderr)
		}
		requireLeft(t, r, binaries, stderr)
	})

	t.Run("the early rollback over refused settings", func(t *testing.T) {
		r := newRefusedInstall(t, "")
		r.withSettingsThisVersionRefuses(t, "load")
		standJournal(t, r.dataDir)
		binaries := watch(t, r.bin, r.bak)
		code, stderr := r.serve(t, serveOpts{})
		if code != 2 || code == supervision.RestartExitCode {
			t.Fatalf("a start whose settings this version refuses, over an unfinished restore = %d, want 2; stderr=%s", code, stderr)
		}
		requireLeft(t, r, binaries, stderr)
	})

	t.Run("the crash rollback a refused start hands the install to", func(t *testing.T) {
		r := newRefusedInstall(t, "")
		r.crashedThreeTimes(t)
		r.withSettingsThisVersionRefuses(t, "load")
		standJournal(t, r.dataDir)
		binaries := watch(t, r.bin, r.bak)
		code, stderr := r.serve(t, serveOpts{})
		if code != 2 || code == supervision.RestartExitCode {
			t.Fatalf("a refused start at the crash rollback's limit over an unfinished restore = %d, want 2; stderr=%s", code, stderr)
		}
		requireLeft(t, r, binaries, stderr)
	})
}

// TestARestoreOverAnArmedInstallKeepsEveryRollbackOff (T35, serve's
// half): bridge restore of a snapshot at this version's ladder top, made in
// another data dir, over an armed install that recorded nothing, records the
// store change; three starts that crash then bring the count to the crash
// rollback's limit, and the next start refuses the rollback (a migration of
// the store began, which a restore is), serves, and leaves the backup where it
// is. internal/backup's half pins the record and the refusals of each
// rollback's check.
func TestARestoreOverAnArmedInstallKeepsEveryRollbackOff(t *testing.T) {
	r := newRefusedInstall(t, "")
	src := t.TempDir()
	srcDB := manifest.DefaultDBPath(src)
	s, err := manifest.OpenStore(srcDB)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	snap, err := backup.Snapshot(t.Context(), backup.Sources{DataDir: src, ManifestDB: srcDB})
	if err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := runRestoreYes(t, r.cfg, snap); code != 0 {
		t.Fatalf("bridge restore over the armed install = %d\nstderr:\n%s", code, stderr)
	}
	if st := markerOf(t, r.dataDir); st.StoreMigratedBy != version.ServerVersion || st.DataDirOpenedBy != version.ServerVersion {
		t.Fatalf("the marker after the restore = %+v, want the store change and the data dir recorded as this version's", st)
	}
	r.crashedThreeTimes(t)
	binaries := watch(t, r.bin, r.bak)
	_, seams := newProbationWatch(longProbation)
	sb := r.bootWith(t, serveOpts{probation: seams})
	requireRefusal(t, r, sb.stderr.String(), "began a migration", "began a migration")
	stopServed(t, sb)
	requireInstallKept(t, binaries, r)
}

// TestTheDoctorSaysNotCheckedWhereTheStoreLockRefuses (T5's doctor rows): a
// store lock that refuses (no lock manager to answer, a restore's hold, an
// unfinished restore) leaves the doctor's store-migrations line and its
// probes' opens "not checked", opening nothing of the database.
func TestTheDoctorSaysNotCheckedWhereTheStoreLockRefuses(t *testing.T) {
	for _, c := range []struct {
		name string
		set  func(t *testing.T, dataDir string)
	}{
		{"a lock that cannot be taken", func(t *testing.T, _ string) {
			t.Cleanup(manifest.SetStoreLockTryForTest(func(*os.File, bool) error { return errors.New("the lock manager did not answer") }))
		}},
		{"an unfinished restore", func(t *testing.T, dataDir string) { standJournal(t, dataDir) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			dataDir := t.TempDir()
			db := manifest.DefaultDBPath(dataDir)
			s, err := manifest.OpenStore(db)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			c.set(t, dataDir)
			var reached atomic.Bool
			t.Cleanup(manifest.SetBeforeMigrationLockForTest(func(string) { reached.Store(true) }))
			line, ok := storeMigrationsLine(db, "the install awaits its confirmation")
			if !ok || line.Status != doctor.Warn || !strings.HasPrefix(line.Summary, "not checked:") {
				t.Errorf("the store-migrations line = %+v (shown %v), want a warn, not checked", line, ok)
			}
			if _, err := doctorOpenStore(db); err == nil || !strings.HasPrefix(err.Error(), "not checked:") || !manifest.AdmissionRefused(err) {
				t.Errorf("a probe's open = %v, want it not checked", err)
			}
			if reached.Load() {
				t.Error("a refused probe reached the store's files")
			}
		})
	}
}

// TestEveryRestoreTakesTheStoresLockBeforeItsPrompt: bridge restore beside a
// command that has the library database open (a store open in this process
// stands in for it; the kernel's answer across processes is internal/
// manifest's T1) is refused before it looks at its snapshot and before its
// prompt, exit 1, naming why: no prompt on either stream, and the restore's
// own source worker never run. The look before the prompt
// (backup.CheckRestoreSource) runs no such hook, so a directory that is no
// snapshot shows the order: it is refused for the lock, never as no snapshot
// (exit 2), which a hold taken after the look, still before the prompt,
// would answer.
func TestEveryRestoreTakesTheStoresLockBeforeItsPrompt(t *testing.T) {
	cfgPath, dataDir, snap := seedRestorableInstall(t, closedAdminAddr(t))
	var looked atomic.Int32
	t.Cleanup(backup.SetOnSourceWorkerEndForTest(func(d string) {
		if d == snap {
			looked.Add(1)
		}
	}))
	s, err := manifest.OpenStore(manifest.DefaultDBPath(dataDir))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var stdout, stderr strings.Builder
	code := restoreCmd(context.Background(), []string{"--config", cfgPath, snap}, strings.NewReader("yes\n"), &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "a bridge command has the library database open") ||
		strings.Contains(stderr.String(), "Type 'yes'") || strings.Contains(stdout.String(), "Restore from") {
		t.Fatalf("bridge restore beside an open store = %d, want 1 before its prompt\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	if n := looked.Load(); n != 0 {
		t.Errorf("bridge restore looked at its snapshot %d time(s) before the store's lock refused it", n)
	}
	stdout.Reset()
	stderr.Reset()
	code = restoreCmd(context.Background(), []string{"--config", cfgPath, t.TempDir()}, strings.NewReader("yes\n"), &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "a bridge command has the library database open") ||
		strings.Contains(stderr.String(), "not a recognized snapshot") {
		t.Fatalf("bridge restore of a directory that is no snapshot, beside an open store = %d, want 1 for the lock before the snapshot is looked at\nstdout:\n%s\nstderr:\n%s",
			code, stdout.String(), stderr.String())
	}
}
