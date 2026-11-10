package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/doctor"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// requireDirectorySync skips a test of a recheck's veto where no directory
// can be synced (Windows): no check over a VERIFIED is recorded there
// (TestBridgeBackupVerifySaysWhatARecheckFoundWhereItCannotBeRecorded).
func requireDirectorySync(t *testing.T) {
	t.Helper()
	if !fsutil.SyncsParentDir {
		t.Skip("no directory can be synced here, so no check over a VERIFIED is recorded")
	}
}

// flipBundleByte changes the last byte of the file name in the snapshot in
// dir.
func flipBundleByte(t *testing.T, dir, name string) {
	t.Helper()
	p := filepath.Join(dir, name)
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)-1] ^= 0xFF
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// awaitHoldsLetGo waits until no hold given up in this process holds the
// backups folder at root, asked by that spelling (backup.StuckCheckHere,
// whose mark drops once all its hold held is let go), and until no snapshot
// there is owned, failing the test past a deadline.
func awaitHoldsLetGo(t *testing.T, root string) {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, held := backup.StuckCheckHere(root); !held {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("a hold given up was never let go")
		}
	}
	awaitVerificationsEnd(t, root)
}

// awaitMarked waits until a hold given up in this process holds the backups
// folder at root, and answers what it says.
func awaitMarked(t *testing.T, root string) backup.StuckCheck {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if s, held := backup.StuckCheckHere(root); held {
			return s
		}
		if time.Now().After(deadline) {
			t.Fatal("no hold given up holds the backups folder")
		}
	}
}

// parkChecks parks every verification's check on its thread from now on
// (the hook a check runs once its priority is lowered), with a deadline of
// 300 ms, so each is given up; the test's end lets them go and waits for
// every hold they left in the backups folder at root to end, before the
// seams are put back.
func parkChecks(t *testing.T, root string) func() {
	t.Helper()
	t.Cleanup(backup.SetVerifyDeadlineForTest(300 * time.Millisecond))
	out := make(chan struct{})
	t.Cleanup(backup.SetOnLowPriorityForTest(func() { <-out }))
	var once sync.Once
	release := func() { once.Do(func() { close(out) }) }
	t.Cleanup(func() {
		release()
		awaitHoldsLetGo(t, root)
	})
	return release
}

// TestBridgeBackupVerifySaysWhatARecheckFoundWhereItCannotBeRecorded is
// backlog B653's fourth round's case for bridge backup --verify where no
// directory can be synced (Windows, stood in for through its seam): a check of
// a verified snapshot whose database is damaged since runs, and the command
// says what it found (its database FAILED), that it was not recorded and why,
// and what the record it leaves as it was reads (VERIFIED), exits 1, and
// leaves no veto and the record byte for byte, which every reader reads as
// before. Red on the round before, which took the no-op barrier for a durable
// veto and recorded the FAILED.
func TestBridgeBackupVerifySaysWhatARecheckFoundWhereItCannotBeRecorded(t *testing.T) {
	cfgPath, _, root := backupInstall(t)
	dir := verifiedInstallSnapshot(t, cfgPath)
	flipBundleByte(t, dir, backup.ManifestDBFileName)
	mpath := filepath.Join(dir, backup.ManifestFile)
	before, err := os.ReadFile(mpath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(backup.SetSyncsParentDirForTest(false))
	var stdout, stderr bytes.Buffer
	code := backupCmd([]string{"--config", cfgPath, "--verify", dir}, &stdout, &stderr)
	out, errs := stdout.String(), stderr.String()
	if code != 1 {
		t.Errorf("bridge backup --verify = %d, want 1\n%s%s", code, out, errs)
	}
	found, record := strings.Index(out, "this check found:"), strings.Index(out, "its record, which this check leaves as it was, reads:")
	if found < 0 || record < found || !strings.Contains(out[found:record], "database: FAILED: hash-mismatch") ||
		!strings.Contains(out[record:], "database: VERIFIED") {
		t.Errorf("the command did not say what its check found, and apart what the record reads:\n%s", out)
	}
	if !strings.Contains(errs, "what this check found was not recorded") || !strings.Contains(errs, "no directory can be synced") {
		t.Errorf("the command did not say the finding was not recorded, and why:\n%s", errs)
	}
	if after, err := os.ReadFile(mpath); err != nil || !bytes.Equal(after, before) {
		t.Errorf("the record changed: %v", err)
	}
	for _, p := range []string{filepath.Join(dir, backup.VetoFileName), backup.RootVetoPath(dir)} {
		if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("a veto was left at %s: %v", filepath.Base(p), err)
		}
	}
	entries, err := backup.List(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.DirName == filepath.Base(dir) && (e.Verdict.DatabaseState != backup.StateVerified || e.Verdict.FilesState != backup.StateVerified) {
			t.Errorf("a reader reads %s/%s, want the VERIFIED it read before", e.Verdict.DatabaseState, e.Verdict.FilesState)
		}
	}
}

// TestBridgeBackupVerifySaysWhatAGivenUpVerificationsRecordReads is backlog
// B653's fourth round's red for what bridge backup --verify says of a
// verification given up (the Claude API's Low on round 3, both reviews of the
// round's brief): never what the INCOMPLETE it was given up with would say,
// but how far its write went and what its record reads as read back, which
// every reader reads, and that its step still holds the snapshots. Given up
// over a PENDING, it wrote INCOMPLETE; over a FAILED, whose merge keeps the
// FAILED, its write found nothing to change and the record reads FAILED; and
// where its INCOMPLETE's write fails, its write failed, says why, and the
// record reads as before (the PENDING of a snapshot taken just now, with no
// owner to ask, which reads by its age). Its fifth round's red (Codex's P2 on
// round 4, the review of the round's brief): "it wrote nothing" only where no
// write began; a write whose rename landed before its sync did not answer did
// not finish, which says so, the record read back reading what it wrote; and
// a record not read back is said so, after how far the write went, and never
// printed, its read-back given up said as that since the sixth round. Red on
// the round before, which said "it wrote nothing" wherever what was found was
// not recorded.
func TestBridgeBackupVerifySaysWhatAGivenUpVerificationsRecordReads(t *testing.T) {
	for _, tc := range []struct {
		name  string
		wrote string
		reads backup.State
		why   backup.Reason
	}{
		{"over a PENDING", "it wrote its record, which reads:", backup.StateIncomplete, backup.ReasonDeadline},
		{"over a FAILED", "its write found nothing to change, and its record reads:", backup.StateFailed, backup.ReasonHash},
		{"whose INCOMPLETE is not written", "its write failed, and its record reads:", backup.StatePending, ""},
		{"whose write did not finish", "its write did not finish, and its record reads:", backup.StateIncomplete, backup.ReasonDeadline},
		{"whose record was not read back", "it wrote its record; its read-back was given up, so its record was not read", backup.StateIncomplete, backup.ReasonDeadline},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfgPath, _, root := backupInstall(t)
			cfg, resolved, err := loadCLIConfig(cfgPath)
			if err != nil {
				t.Fatal(err)
			}
			dir, err := backup.Snapshot(t.Context(), buildBackupSources(cfg, resolved))
			if err != nil {
				t.Fatal(err)
			}
			switch tc.name {
			case "over a FAILED":
				// Both verdicts FAILED, so the record holds no VERIFIED.
				flipBundleByte(t, dir, backup.ManifestDBFileName)
				flipBundleByte(t, dir, "bridge.yaml")
				res, err := backup.VerifySnapshot(t.Context(), dir, backup.VerifyOptions{BackupsRoot: root})
				if err != nil || res.Record.DatabaseState != backup.StateFailed || res.Record.FilesState != backup.StateFailed {
					t.Fatalf("the check before: %v, %s/%s; want FAILED/FAILED recorded", err, res.Record.DatabaseState, res.Record.FilesState)
				}
			case "whose INCOMPLETE is not written":
				t.Cleanup(backup.SetVerdictWriteFailuresForTest(func(string) error { return errors.New("injected: the staging failed") }, nil, nil))
			}
			mpath := filepath.Join(dir, backup.ManifestFile)
			stalled := make(chan struct{})
			var letGo sync.Once
			switch tc.name {
			case "whose write did not finish":
				// The rename lands, and the folder's sync after it does not
				// answer until the test's end.
				t.Cleanup(backup.SetSyncVerdictForTest(func(path string) error {
					if path == mpath {
						<-stalled
					}
					return fsutil.SyncParentDir(path)
				}))
			case "whose record was not read back":
				// The verification reads the manifest a third time to read its
				// record back, after its first read and its write's: that read
				// does not answer until the test's end.
				var reads atomic.Int32
				t.Cleanup(backup.SetOnManifestReadForTest(func(path string) {
					if path == mpath && reads.Add(1) == 3 {
						<-stalled
					}
				}))
			}
			parkChecks(t, root)
			// Registered after parkChecks, so run before its wait for the holds.
			t.Cleanup(func() { letGo.Do(func() { close(stalled) }) })
			var stdout, stderr bytes.Buffer
			code := backupCmd([]string{"--config", cfgPath, "--verify", dir}, &stdout, &stderr)
			out, errs := stdout.String(), stderr.String()
			if code != 1 || !strings.Contains(errs, "the verification was given up, and its step still holds the snapshots until it ends") ||
				!strings.Contains(errs, backup.ErrCheckGivenUp.Error()) {
				t.Fatalf("bridge backup --verify = %d, want 1 and its verification given up\n%s%s", code, out, errs)
			}
			var whys []backup.Reason
			if tc.why != "" {
				whys = []backup.Reason{tc.why}
			}
			reads := "database: " + backup.StateWords(tc.reads, whys)
			at := strings.Index(out, tc.wrote)
			switch {
			case tc.name == "whose record was not read back":
				if at < 0 || strings.Contains(out, "database:") {
					t.Errorf("the command did not say %q alone, with no record printed:\n%s", tc.wrote, out)
				}
				letGo.Do(func() { close(stalled) })
			case at < 0 || !strings.Contains(out[at:], reads):
				t.Errorf("the command did not say %q, then %q:\n%s", tc.wrote, reads, out)
			}
			if strings.Contains(out, "it wrote nothing") {
				t.Errorf("the command said it wrote nothing of a write that began:\n%s", out)
			}
			letGo.Do(func() { close(stalled) })
			for _, never := range []string{"this check found", "was not recorded"} {
				if strings.Contains(out+errs, never) {
					t.Errorf("the command said %q of a verification given up:\n%s%s", never, out, errs)
				}
			}
			if tc.name == "whose INCOMPLETE is not written" && !strings.Contains(errs, "injected: the staging failed") {
				t.Errorf("the command did not say its write failed:\n%s", errs)
			}
			// What it says the record reads is what a reader reads.
			entries, err := backup.List(root)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range entries {
				if e.DirName != filepath.Base(dir) {
					continue
				}
				if got := backup.StateWords(e.Verdict.DatabaseState, e.Verdict.DatabaseReasons); got != backup.StateWords(tc.reads, whys) {
					t.Errorf("a reader reads database %s, want %s", got, backup.StateWords(tc.reads, whys))
				}
			}
		})
	}
}

// TestTheTickerSaysWhatAGivenUpVerificationsRecordReads: the ticker's report
// of a verification given up (backlog B653's fourth round, as bridge
// backup's) says how far its write went and what its record reads as read
// back, and that its step still holds the snapshots: over the PENDING of the
// snapshot its pass took, it wrote INCOMPLETE; where that write fails, its
// write failed, says why, and the record reads PENDING, its owner, the pass's
// own capture, holding it still; and where its rename landed before its sync
// did not answer, its write did not finish, and the record reads what it
// wrote (the fifth round). A record over a FAILED is not one a pass
// verifies. Red on the round before, which said "it wrote nothing" wherever
// what was found was not recorded.
func TestTheTickerSaysWhatAGivenUpVerificationsRecordReads(t *testing.T) {
	for _, tc := range []struct {
		name  string
		wrote string
	}{
		{"over a PENDING", "it wrote its record, which reads database INCOMPLETE: deadline, files INCOMPLETE: deadline"},
		{"whose INCOMPLETE is not written", "its write failed, and its record reads database PENDING, files PENDING"},
		{"whose write did not finish", "its write did not finish, and its record reads database INCOMPLETE: deadline, files INCOMPLETE: deadline"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, dataDir, root := backupInstall(t)
			src := backup.Sources{DataDir: dataDir, ManifestDB: manifest.DefaultDBPath(dataDir)}
			if tc.name == "whose INCOMPLETE is not written" {
				t.Cleanup(backup.SetVerdictWriteFailuresForTest(func(string) error { return errors.New("injected: the staging failed") }, nil, nil))
			}
			stalled := make(chan struct{})
			var letGo, first sync.Once
			if tc.name == "whose write did not finish" {
				// The first verdict's rename lands, and the folder's sync after
				// it does not answer until the test's end.
				t.Cleanup(backup.SetSyncVerdictForTest(func(path string) error {
					stall := false
					first.Do(func() { stall = true })
					if stall {
						<-stalled
					}
					return fsutil.SyncParentDir(path)
				}))
			}
			releaseChecks := parkChecks(t, root)
			// Registered after parkChecks, so run before its wait for the holds.
			t.Cleanup(func() { letGo.Do(func() { close(stalled) }) })
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			drainLoopOnCleanup(t, func() { letGo.Do(func() { close(stalled) }); releaseChecks(); cancel() }, done, "the backup ticker")
			var stdout, stderr safeBuffer
			go func() {
				defer close(done)
				runBackupTicker(ctx, src, func() int { return 7 }, staticInterval(24*time.Hour), nil, &stdout, &stderr, &sweepStatus[struct{}]{})
			}()
			for deadline := time.Now().Add(60 * time.Second); !strings.Contains(stderr.String(), "was given up"); time.Sleep(10 * time.Millisecond) {
				if time.Now().After(deadline) {
					t.Fatalf("the pass never said its verification was given up\nstdout %s\nstderr %s", stdout.String(), stderr.String())
				}
			}
			cancel()
			waitClosed(t, done, "the backup ticker")
			errs := stderr.String()
			if !strings.Contains(errs, "its step still holds the snapshots until it ends; "+tc.wrote) {
				t.Errorf("the pass did not say %q:\n%s", tc.wrote, errs)
			}
			if tc.name == "whose INCOMPLETE is not written" && !strings.Contains(errs, "injected: the staging failed") {
				t.Errorf("the pass did not say its write failed:\n%s", errs)
			}
		})
	}
}

// TestTheDoctorNamesAHoldGivenUpWhileTheBackupsFolderDoesNotAnswer is backlog
// B653's fourth round's case for the doctor beside a step that hangs under
// the backups folder's lock (Claude's Medium on round 3, both reviews of the
// round's brief): a verification whose verdict's write hangs is given up and
// marks the folder, and with every read of the folder stalled too (its
// listing, the resolution of its links, the lock's probe), the doctor's
// backups lines, which both doctors take, answer within their bound:
// backup-check names the snapshot and its step from this process's mark,
// which asks no filesystem, and backup-newest says the folder did not answer.
// Red on the round before, whose doctor listed the folder first, with no
// bound, beside a write that was never given up.
func TestTheDoctorNamesAHoldGivenUpWhileTheBackupsFolderDoesNotAnswer(t *testing.T) {
	cfgPath, dataDir, root := backupInstall(t)
	cfg, _, err := loadCLIConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	c, err := backup.CaptureSnapshot(t.Context(), backup.Sources{DataDir: dataDir, ManifestDB: manifest.DefaultDBPath(dataDir)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(backup.SetVerifyDeadlineForTest(300 * time.Millisecond))
	entered, out := make(chan struct{}), make(chan struct{})
	var enter, letGo sync.Once
	t.Cleanup(backup.SetVerdictWriteFailuresForTest(func(d string) error {
		if d == c.Dir {
			enter.Do(func() { close(entered) })
			<-out
		}
		return nil
	}, nil, nil))
	callerDone := make(chan struct{})
	release := func() {
		letGo.Do(func() { close(out) })
		waitClosed(t, callerDone, "finish capture")
		awaitHoldsLetGo(t, root)
	}
	drainLoopOnCleanup(t, release, callerDone, "finish capture")
	done := make(chan backup.Result, 1)
	go func() {
		defer close(callerDone)
		done <- backup.FinishCapture(context.Background(), c, nil, backup.VerifyOptions{})
	}()
	<-entered
	select {
	case res := <-done:
		if !errors.Is(res.VerifyErr, backup.ErrCheckGivenUp) || !strings.Contains(res.VerifyErr.Error(), "its verdict's write") {
			t.Fatalf("the verification answered %v, want it given up at its verdict's write", res.VerifyErr)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the verification whose write hangs was never given up")
	}
	awaitMarked(t, root)
	// The folder stops answering: every read of it waits until the test's end.
	stalled := make(chan struct{})
	stall := func() error { <-stalled; return errors.New("stalled") }
	t.Cleanup(backup.SetReadDirForTest(func(string) ([]os.DirEntry, error) { return nil, stall() }))
	t.Cleanup(backup.SetMetadataForTest(func(string) (string, error) { return "", stall() }, func(string) (os.FileInfo, error) { return nil, stall() }))
	t.Cleanup(backup.SetOpenProbeForTest(func(string) (*os.File, error) { return nil, stall() }))
	prevBound, prevEnd := doctorBackupsBound, onDoctorBackupsReadEnd
	readEnded := make(chan struct{})
	doctorBackupsBound, onDoctorBackupsReadEnd = 300*time.Millisecond, func() { close(readEnded) }
	t.Cleanup(func() { doctorBackupsBound, onDoctorBackupsReadEnd = prevBound, prevEnd })
	doctorDone := make(chan struct{})
	// Registered last, so run first: the folder answers again, the doctor's
	// read left on it ends, and the write is let go and its hold has ended,
	// before a stand-in is put back.
	t.Cleanup(func() {
		close(stalled)
		waitClosed(t, doctorDone, "backup doctor")
		select {
		case <-readEnded:
		case <-time.After(30 * time.Second):
			t.Error("the doctor's read of the backups folder never ended")
		}
		release()
	})
	asked := time.Now()
	answered := make(chan []doctor.Check, 1)
	go func() { defer close(doctorDone); answered <- doctorBackups(cfg)(context.Background()) }()
	var lines []doctor.Check
	select {
	case lines = <-answered:
	case <-time.After(doctorBackupsBound + 10*time.Second):
		t.Fatal("the doctor's backups lines did not answer while the folder did not")
	}
	if took := time.Since(asked); took > doctorBackupsBound+2*time.Second {
		t.Errorf("the doctor's backups lines took %v; their bound is %v", took, doctorBackupsBound)
	}
	var check, newest doctor.Check
	for _, l := range lines {
		switch l.Name {
		case "backup-check":
			check = l
		case "backup-newest":
			newest = l
		}
	}
	if check.Status != doctor.Warn || !strings.Contains(check.Summary, filepath.Base(c.Dir)) || !strings.Contains(check.Summary, "its verdict's write") {
		t.Errorf("backup-check = %+v; want a warn naming %s and its step", check, filepath.Base(c.Dir))
	}
	if !strings.Contains(newest.Summary, "did not answer") {
		t.Errorf("backup-newest = %+v; want it saying the folder did not answer", newest)
	}
}
