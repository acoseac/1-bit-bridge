package main

import (
	"bytes"
	"database/sql"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/dsn"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/supervision"
	"github.com/acoseac/1-bit-bridge/internal/updater"
	"github.com/acoseac/1-bit-bridge/internal/version"
)

// The crash rollback (backlog B660): updater.ProbationFailedStarts starts of
// an install's version that did not end cleanly put the backup back at the
// next start, before it writes anything, where the marker, the backup's
// version and the backup's settings check all allow it, and otherwise the
// start says why, records it, and goes on.

// withAStepThatFailsMidway puts the store in r's data dir one step behind,
// with a table named as the last step's index, so that step adds its
// columns and then fails at the index, user_version unmoved: a migration
// begun and not finished.
func (r *refusedInstall) withAStepThatFailsMidway(t *testing.T) {
	t.Helper()
	if latest := latestStoreVersion(t); latest != 60 {
		t.Fatalf("the obstacle stands in migration 60's way, and the ladder ends at %d: give the last step an obstacle of its own", latest)
	}
	r.withAnOlderStore(t)
	db := openRawStore(t, r.dataDir)
	if _, err := db.Exec(`CREATE TABLE idx_tracks_sample_format_stale (x INTEGER)`); err != nil {
		t.Fatal(err)
	}
}

// openRawStore opens the store in dataDir as SQLite alone, no ladder, closed
// when the test ends.
func openRawStore(t *testing.T, dataDir string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", dsn.File(manifest.DefaultDBPath(dataDir), "_pragma=busy_timeout(5000)"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// tracksHasColumn reports whether the store in dataDir's tracks table has
// the column name.
func tracksHasColumn(t *testing.T, dataDir, name string) bool {
	t.Helper()
	var n int
	if err := openRawStore(t, dataDir).QueryRow(`SELECT COUNT(*) FROM pragma_table_info('tracks') WHERE name = ?`, name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n == 1
}

// requireInstallKept requires the install as it was: the binary in place, the
// backup beside it.
func requireInstallKept(t *testing.T, binaries watchedFiles, r *refusedInstall) {
	t.Helper()
	binaries.requireUnchanged(t)
	if _, err := os.Lstat(r.bak); err != nil {
		t.Errorf("the backup: %v; want it kept beside the binary", err)
	}
}

// requireRefusal requires a refused crash rollback: said on serve's stderr
// with words, and recorded in the marker (State.RollbackRefused) with
// recorded, naming no path.
func requireRefusal(t *testing.T, r *refusedInstall, stderr, said, recorded string) {
	t.Helper()
	if lines := updaterLines(stderr); !strings.Contains(lines, "not rolling back the install") || !strings.Contains(lines, said) {
		t.Errorf("the start did not say it kept the install because %q; its updater lines: %q", said, lines)
	}
	st := markerOf(t, r.dataDir)
	if !strings.Contains(st.RollbackRefused, recorded) {
		t.Errorf("the marker's refusal = %q, want it to record %q", st.RollbackRefused, recorded)
	}
	if strings.Contains(st.RollbackRefused, r.dir) || strings.ContainsAny(st.RollbackRefused, `/\`) {
		t.Errorf("the recorded refusal %q names a path", st.RollbackRefused)
	}
}

// TestAMigrationThatFailsStillRefusesTheRollback: a step of the ladder that
// adds its columns and then fails leaves user_version where it was and the
// store changed. The start that began it recorded it first, so the count of
// the starts it fails, three, asks for a rollback that is refused: begun is
// changed, whatever user_version says.
func TestAMigrationThatFailsStillRefusesTheRollback(t *testing.T) {
	r := newRefusedInstall(t, "")
	r.withAStepThatFailsMidway(t)
	behind := storeVersion(t, r.dataDir)
	binaries := watch(t, r.bin, r.bak)
	for start := 1; start <= updater.ProbationFailedStarts; start++ {
		if code, stderr := r.serve(t, serveOpts{}); code != 1 || !strings.Contains(stderr, "migration 60") {
			t.Fatalf("start %d over a step that fails = %d, want 1 at the migration; stderr=%s", start, code, stderr)
		}
	}
	if st := markerOf(t, r.dataDir); st.StoreMigratedBy != version.ServerVersion || st.FailedStarts != updater.ProbationFailedStarts {
		t.Fatalf("the marker after the failed migrations = %+v, want the migration recorded and every start counted", st)
	}
	if v := storeVersion(t, r.dataDir); v != behind || !tracksHasColumn(t, r.dataDir, "sample_format_size") {
		t.Fatalf("the store after the failed step: user_version %d (want %d unmoved), its columns added %v (want true): "+
			"the test shows nothing", v, behind, tracksHasColumn(t, r.dataDir, "sample_format_size"))
	}
	code, stderr := r.serve(t, serveOpts{})
	if code != 1 {
		t.Fatalf("the fourth start = %d, want 1 at the migration, after it refused the rollback; stderr=%s", code, stderr)
	}
	requireRefusal(t, r, stderr, "began a migration", "began a migration")
	requireInstallKept(t, binaries, r)
}

// TestACommandsMigrationRefusesTheRollbackToo: the first store open after the
// install that migrates is `bridge doctor`'s, of this version, before any
// start of serve: it records the migration as serve's would, and the crash
// rollback after three starts that crashed before their store opened is
// refused.
func TestACommandsMigrationRefusesTheRollbackToo(t *testing.T) {
	r := newRefusedInstall(t, "")
	r.withAnOlderStore(t)
	var stdout, stderr bytes.Buffer
	run(t.Context(), []string{"doctor", "--config", r.cfg}, &stdout, &stderr)
	if st := markerOf(t, r.dataDir); st.StoreMigratedBy != version.ServerVersion {
		t.Fatalf("the marker after bridge doctor migrated the store = %+v, want the migration recorded; stderr=%s", st, stderr.String())
	}
	for start := 1; start <= updater.ProbationFailedStarts; start++ {
		r.startThatPanics(t)
	}
	binaries := watch(t, r.bin, r.bak)
	_, seams := newProbationWatch(longProbation)
	sb := r.bootWith(t, serveOpts{probation: seams})
	requireRefusal(t, r, sb.stderr.String(), "began a migration", "began a migration")
	stopServed(t, sb)
	requireInstallKept(t, binaries, r)
}

// TestAMigrationWhoseRecordCannotBeWrittenDoesNotRun: the record that a
// migration began is written before its first step, and a record that cannot
// be made durable refuses the store's open: no step runs, user_version and
// the schema as they were, and serve does not start.
func TestAMigrationWhoseRecordCannotBeWrittenDoesNotRun(t *testing.T) {
	r := newRefusedInstall(t, "")
	r.withAnOlderStore(t)
	behind := storeVersion(t, r.dataDir)
	var writes atomic.Int32
	var armed atomic.Bool
	t.Cleanup(updater.SetBeforeDisarmRecordForTest(func(string) {
		// The admission's record is the first disarm write, which must
		// land; the migration's record is the second.
		if writes.Add(1) == 2 {
			armed.Store(true)
		}
	}))
	t.Cleanup(updater.SetMarkerBarrierForTest(func(step string) error {
		if step == "sync" && armed.Load() {
			return syscall.EIO
		}
		return nil
	}))
	code, stderr, served := serveToItsEnd(t, serveOpts{configPath: r.cfg, binaryPath: r.bin})
	if served || code != 1 || !strings.Contains(stderr, "began a migration of the store, so no step is run") {
		t.Fatalf("serve over a migration whose record fails = %d (served %v), want 1 at the record; stderr=%s", code, served, stderr)
	}
	if !armed.Load() {
		t.Fatal("the migration's record was never written: the test shows nothing")
	}
	if v := storeVersion(t, r.dataDir); v != behind || tracksHasColumn(t, r.dataDir, "sample_format_size") {
		t.Errorf("the store after a refused record: user_version %d (want %d), the step's columns there %v (want false)",
			v, behind, tracksHasColumn(t, r.dataDir, "sample_format_size"))
	}
}

// refusedCrashRollback runs three starts of r's install that crash, lets lay
// change what the fourth start finds, and boots that start, which must refuse
// the crash rollback, saying said and recording recorded, count itself and
// serve; it then stops it, and requires the binary as it was. A rollback
// that would trade the crash for something no better is refused, and the
// start goes on, since it may be the one that works.
func refusedCrashRollback(t *testing.T, r *refusedInstall, lay func(), said, recorded string) {
	t.Helper()
	for start := 1; start <= updater.ProbationFailedStarts; start++ {
		r.startThatPanics(t)
	}
	lay()
	binaries := watch(t, r.bin)
	_, seams := newProbationWatch(longProbation)
	sb := r.bootWith(t, serveOpts{probation: seams})
	requireRefusal(t, r, sb.stderr.String(), said, recorded)
	if st := markerOf(t, r.dataDir); st.FailedStarts != updater.ProbationFailedStarts+1 || st.RollbackStarted {
		t.Errorf("the marker as the fourth start serves = %+v, want this start counted and no rollback under way", st)
	}
	stopServed(t, sb)
	binaries.requireUnchanged(t)
}

// requireBackupThere requires the backup beside r's binary.
func requireBackupThere(t *testing.T, r *refusedInstall) {
	t.Helper()
	if _, err := os.Lstat(r.bak); err != nil {
		t.Errorf("the backup after the refusal: %v; want it left beside the binary", err)
	}
}

// TestABackupThatRefusesTheSettingsIsNotPutBack: a backup whose settings
// check refuses this bridge's settings (a key the new version saved, say)
// would not start on them, so putting it back would trade one crash loop for
// another: the rollback is refused, the backup's own reasons recorded.
func TestABackupThatRefusesTheSettingsIsNotPutBack(t *testing.T) {
	r := newRefusedInstall(t, "")
	refusedCrashRollback(t, r, func() { t.Setenv(runAsBridgeCheckEnv, "refused") },
		"refuses this bridge's settings", "refuses this bridge's settings, so it would not start on them: line 3")
	requireBackupThere(t, r)
}

// TestABackupThatCannotAnswerIsNotPutBack: a backup whose settings check
// does not answer (it exits with another code) cannot say it starts on the
// settings, and is not put back at this start.
func TestABackupThatCannotAnswerIsNotPutBack(t *testing.T) {
	r := newRefusedInstall(t, "")
	refusedCrashRollback(t, r, func() { t.Setenv(runAsBridgeCheckEnv, "fails") },
		"settings check did not answer", "could not check this bridge's settings")
	requireBackupThere(t, r)
}

// TestABackupOfAnotherVersionIsNotPutBack: a backup that reports another
// version than the one the install replaced (here a copy of the new version
// itself) is not the last version known to run here, and is not put back.
func TestABackupOfAnotherVersionIsNotPutBack(t *testing.T) {
	r := newRefusedInstall(t, "")
	refusedCrashRollback(t, r, func() { t.Setenv(runAsBridgeVersionEnv, version.ServerVersion) },
		"is "+version.ServerVersion+", where the install replaced "+previousVersion,
		"the backup is "+version.ServerVersion+", where the install replaced "+previousVersion)
	requireBackupThere(t, r)
}

// TestAnAutomaticInstallOverAStagedVersionCannotRollBackPastIt: a manual
// install staged and not started, then an automatic install of this version
// over it, leaves the staged release as the backup while the marker names
// the version that ran as the one replaced (internal/updater's
// TestAnAutomaticInstallOverAStagedVersionLeavesTheStagedOneAsTheBackup lays
// it out through the updater's own install). The crash rollback's first
// check passes on that marker, and its second, the backup's version,
// refuses: the staged release never ran here, so it is not put back.
func TestAnAutomaticInstallOverAStagedVersionCannotRollBackPastIt(t *testing.T) {
	r := newRefusedInstall(t, "")
	refusedCrashRollback(t, r, func() { t.Setenv(runAsBridgeVersionEnv, "0.0.0-staged") },
		"is 0.0.0-staged, where the install replaced "+previousVersion,
		"the backup is 0.0.0-staged, where the install replaced "+previousVersion)
	requireBackupThere(t, r)
}

// TestNoBackupIsPutBackWhereThereIsNone: with no backup beside the binary
// there is nothing to put back, which the start says and records.
func TestNoBackupIsPutBackWhereThereIsNone(t *testing.T) {
	r := newRefusedInstall(t, "")
	refusedCrashRollback(t, r, func() {
		if err := os.Remove(r.bak); err != nil {
			t.Fatal(err)
		}
	}, "there is no backup to put back", "there is no backup beside the binary to put back")
}

// TestABackupThatPredatesTheCheckIsNotPutBack: a backup from before the
// settings check (0.2.2 and older) cannot say whether it loads the settings,
// and one that does not cannot start after it is put back, with the newer
// version held back: from 0.2.1 an `output:` block is a key its strict
// decoder refuses. So it is not put back, and the start records that the
// backup predates the check (B660's second round; until then it was put back
// with a warning).
func TestABackupThatPredatesTheCheckIsNotPutBack(t *testing.T) {
	r := newRefusedInstall(t, "")
	refusedCrashRollback(t, r, func() { t.Setenv(runAsBridgeCheckEnv, "predates") },
		"predates the settings check", "the backup predates the settings check")
	requireBackupThere(t, r)
}

// TestACrashRollbackNeedsBothLocks: the backup is put back only by the data
// dir's owner, holding the install marker's lock from its read through the
// rename. Where serve could not take the data dir's lock (an I/O error, a
// filesystem that keeps none), or the data dir's filesystem keeps no locks
// for the install marker, the start says it leaves the install, records
// nothing of it, and goes on, counting itself.
func TestACrashRollbackNeedsBothLocks(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts serveOpts
		lay  func(t *testing.T)
		said string
	}{
		{"the data dir's lock fails", serveOpts{dataDirTryLock: func(*os.File) error { return syscall.EIO }}, func(*testing.T) {},
			"the data dir's lock is not this process's"},
		{"the data dir's filesystem keeps no locks", serveOpts{dataDirTryLock: func(*os.File) error { return syscall.EOPNOTSUPP }},
			func(*testing.T) {}, "the data dir's lock is not this process's"},
		{"the install marker's lock is not kept", serveOpts{}, func(t *testing.T) {
			t.Cleanup(updater.SetStateTryLockForTest(func(*os.File) error { return syscall.EOPNOTSUPP }))
		}, "the data dir's filesystem keeps no locks"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRefusedInstall(t, "")
			for start := 1; start <= updater.ProbationFailedStarts; start++ {
				r.startThatPanics(t)
			}
			tc.lay(t)
			binaries := watch(t, r.bin, r.bak)
			_, seams := newProbationWatch(longProbation)
			opts := tc.opts
			opts.probation = seams
			sb := r.bootWith(t, opts)
			if lines := updaterLines(sb.stderr.String()); !strings.Contains(lines, "not rolling back the install") || !strings.Contains(lines, tc.said) {
				t.Errorf("the start did not say it leaves the install; its updater lines: %q", lines)
			}
			if st := markerOf(t, r.dataDir); st.RollbackRefused != "" || st.FailedStarts != updater.ProbationFailedStarts+1 {
				t.Errorf("the marker as the fourth start serves = %+v, want nothing recorded of the rollback, this start counted", st)
			}
			stopServed(t, sb)
			binaries.requireUnchanged(t)
		})
	}
}

// TestATransientRefusalAtTheFourthStartRollsBackAtTheFifth: a refusal is
// asked again at every start the count still asks for the rollback, so a
// settings check that did not answer at the fourth start, which then crashed
// too, does not stop the fifth from putting the backup back, its record
// clearing the refusal.
func TestATransientRefusalAtTheFourthStartRollsBackAtTheFifth(t *testing.T) {
	r := newRefusedInstall(t, "")
	for start := 1; start <= updater.ProbationFailedStarts; start++ {
		r.startThatPanics(t)
	}
	t.Setenv(runAsBridgeCheckEnv, "fails")
	r.startThatPanics(t)
	if st := markerOf(t, r.dataDir); st.RollbackRefused == "" || st.FailedStarts != updater.ProbationFailedStarts+1 {
		t.Fatalf("the marker after the fourth start = %+v, want its refusal recorded and the start counted", st)
	}
	t.Setenv(runAsBridgeCheckEnv, "")
	var atRename updater.State
	code, stderr, served := serveToItsEnd(t, serveOpts{configPath: r.cfg, binaryPath: r.bin,
		restoreBackup: func(dst, ext string) error {
			st, err := updater.LoadState(r.dataDir)
			if err != nil {
				t.Error(err)
			}
			atRename = st
			return updater.RollbackBinary(dst, ext)
		}})
	if served || code != supervision.RestartExitCode {
		t.Fatalf("the fifth start = %d (served %v), want %d before it served; stderr=%s", code, served, supervision.RestartExitCode, stderr)
	}
	if atRename.RollbackRefused != "" || !atRename.RollbackStarted || atRename.FailedStarts != updater.ProbationFailedStarts+1 {
		t.Errorf("the marker as the backup was put back = %+v, want the rollback's record, the refusal gone", atRename)
	}
	if got := reportedVersion(t, r.bin); !strings.Contains(got, previousVersion) {
		t.Errorf("the binary reports %q after the rollback, want %s", got, previousVersion)
	}
}

// crashRollbackStoppedBeforeItsRename runs three starts of r's install that
// crash, then a fourth whose crash rollback ends where a process killed
// between the rollback's record and its rename would: the record durable,
// this version's binary in place, the backup beside it.
func crashRollbackStoppedBeforeItsRename(t *testing.T, r *refusedInstall) {
	t.Helper()
	for start := 1; start <= updater.ProbationFailedStarts; start++ {
		r.startThatPanics(t)
	}
	binaries := watch(t, r.bin, r.bak)
	killed := false
	func() {
		defer func() { killed = recover() != nil }()
		var out, errs bytes.Buffer
		runServe(t.Context(), serveOpts{configPath: r.cfg, binaryPath: r.bin,
			restoreBackup: func(string, string) error { panic("the process ended before the rename") }}, &out, &errs)
	}()
	if !killed {
		t.Fatal("the crash rollback never reached its rename")
	}
	binaries.requireUnchanged(t)
	if st := markerOf(t, r.dataDir); st.Status != "" || !st.RollbackStarted || st.FailedStarts != updater.ProbationFailedStarts ||
		st.RejectedVersion != version.ServerVersion || st.MigrationVeto != version.ServerVersion {
		t.Fatalf("the marker after a rollback stopped before its rename = %+v, want the rollback's record with its count", st)
	}
}

// TestAStartFinishesACrashRollbackStoppedBeforeItsRename: the crash
// rollback's record is durable before its rename, and a process stopped
// between the two leaves the record with this version's binary in place: the
// record is the install still, its count with it, so the next start of this
// version, which asks before its admission, finishes the rollback.
func TestAStartFinishesACrashRollbackStoppedBeforeItsRename(t *testing.T) {
	r := newRefusedInstall(t, "")
	crashRollbackStoppedBeforeItsRename(t, r)
	code, stderr, served := serveToItsEnd(t, serveOpts{configPath: r.cfg, binaryPath: r.bin})
	if served || code != supervision.RestartExitCode {
		t.Fatalf("the next start = %d (served %v), want %d before it served; stderr=%s", code, served, supervision.RestartExitCode, stderr)
	}
	if got := reportedVersion(t, r.bin); !strings.Contains(got, previousVersion) {
		t.Errorf("the binary reports %q after the rollback, want %s", got, previousVersion)
	}
	requireNoStoreOpened(t, r.dataDir)
}

// TestARollbackOvertakenGoesBackOnProbation: a crash rollback stopped before
// its rename, and the next start of this version, which asks all three
// checks again, refuses it, the backup's settings check not answering this
// time: that start goes on, its admission lifts the veto of its migrations
// and counts it, and its housekeeping puts the install back on probation
// (BootRollbackOvertaken) with the count kept, the hold and the veto of this
// version gone, the backup kept. Until B660 the housekeeping stamped such an
// install installed.
func TestARollbackOvertakenGoesBackOnProbation(t *testing.T) {
	r := newRefusedInstall(t, "")
	crashRollbackStoppedBeforeItsRename(t, r)
	binaries := watch(t, r.bin, r.bak)
	t.Setenv(runAsBridgeCheckEnv, "fails")
	_, seams := newProbationWatch(longProbation)
	sb := r.bootWith(t, serveOpts{probation: seams})
	requireRefusal(t, r, sb.stderr.String(), "settings check did not answer", "could not check this bridge's settings")
	st := markerOf(t, r.dataDir)
	if st.Status != "installing" || st.RollbackStarted || st.FailedStarts != updater.ProbationFailedStarts+1 ||
		st.DataDirOpenedBy != version.ServerVersion || st.RejectedVersion != "" || st.MigrationVeto != "" {
		t.Errorf("the marker once the overtaking start serves = %+v, want the install on probation with its count, "+
			"its record, and no hold or veto of this version", st)
	}
	stopServed(t, sb)
	r.requireCount(t, updater.ProbationFailedStarts, "after the overtaking start's stop")
	binaries.requireUnchanged(t)
}

// TestAnOperatorsRecordIsNotFinishedByTheCrashRollback: an operator's
// rollback stopped before its rename leaves a record of the install with no
// count, since an operator's rollback refuses once a start has been admitted,
// and the early rollback's record carries none by design: the count is what
// tells the crash rollback's own record from the other two. The next start of
// this version leaves both binaries where they are and does not exit for a
// rollback; its admission counts it, and its housekeeping puts the install
// back on probation.
func TestAnOperatorsRecordIsNotFinishedByTheCrashRollback(t *testing.T) {
	for _, tc := range []struct {
		name   string
		record func(updater.State) updater.State
	}{
		{"an operator's", func(st updater.State) updater.State { return updater.OperatorRollbackRecord(st, version.ServerVersion) }},
		{"the early rollback's", func(st updater.State) updater.State {
			return updater.RollbackRecord(st, version.ServerVersion, version.ServerVersion+" refused this bridge's settings: line 3: x", "a digest")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRefusedInstall(t, "")
			r.marker = tc.record(r.marker)
			r.saveMarker(t, r.dataDir)
			binaries := watch(t, r.bin, r.bak)
			_, seams := newProbationWatch(longProbation)
			sb := r.bootWith(t, serveOpts{probation: seams})
			if lines := updaterLines(sb.stderr.String()); strings.Contains(lines, "not rolling back the install") || strings.Contains(lines, "rolled back") {
				t.Errorf("the crash rollback spoke over a record with no count; its updater lines: %q", lines)
			}
			r.requireCount(t, 1, "once the start over the record serves")
			stopServed(t, sb)
			binaries.requireUnchanged(t)
		})
	}
}
