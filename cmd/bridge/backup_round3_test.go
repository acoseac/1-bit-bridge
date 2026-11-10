package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/doctor"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// verifyFaultEnv makes the re-exec'd test binary one run of bridge backup,
// its arguments the JSON list verifyArgsEnv holds, with the fault it names
// put in the verdict's write (runVerifyWithFault): a process of its own, so
// what it left is read by another, fresh, as after a restart. stuckCheckEnv
// makes it one verification whose check parks once it has the snapshot's
// database open, is given up at a short deadline, and is let go when the
// test closes its standard input (runStuckCheck). TestMain checks both before
// the suite starts; neither name starts with BRIDGE_.
const (
	verifyFaultEnv = "CMDBRIDGE_TEST_VERIFY_FAULT"
	verifyArgsEnv  = "CMDBRIDGE_TEST_VERIFY_ARGS"
	stuckCheckEnv  = "CMDBRIDGE_TEST_STUCK_CHECK"
)

// The lines a stuck check's process prints once its check is parked, and
// once its verification has answered; the line a verification's process
// prints once its check has ended, before its verdict is written; and the
// one it prints as its check begins, on its thread.
const (
	stuckParked   = "stuck check parked"
	stuckGivenUp  = "stuck check given up:"
	verifyChecked = "verification checked"
	verifyBegan   = "verification began its check"
)

// runVerifyWithFault is the re-exec'd test binary's whole run as one bridge
// backup with a fault in its verdict's write: stage (the staging refused),
// unsynced (the folder's sync once the rename has landed), noveto (no veto
// can be made), vetosync (a veto made does not sync), or killed (the process
// parks once its check has ended, until its standard input ends, for the
// test to kill it there). Whatever the fault, it says when its check begins
// (verifyBegan).
func runVerifyWithFault() int {
	injected := errors.New("injected: " + os.Getenv(verifyFaultEnv))
	always := func(string) error { return injected }
	backup.SetOnLowPriorityForTest(func() { fmt.Fprintln(os.Stdout, verifyBegan) })
	switch os.Getenv(verifyFaultEnv) {
	case "stage":
		backup.SetVerdictWriteFailuresForTest(always, nil, nil)
	case "unsynced":
		backup.SetSyncVerdictForTest(always)
	case "noveto":
		backup.SetVerdictWriteFailuresForTest(nil, nil, always)
	case "vetosync":
		backup.SetSyncVetoForTest(always)
	case "killed":
		backup.SetAfterCheckForTest(func(string) {
			fmt.Fprintln(os.Stdout, verifyChecked)
			_, _ = io.Copy(io.Discard, os.Stdin)
		})
	default:
		fmt.Fprintln(os.Stderr, "no such fault")
		return 99
	}
	var args []string
	if err := json.Unmarshal([]byte(os.Getenv(verifyArgsEnv)), &args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 99
	}
	return backupCmd(args, os.Stdout, os.Stderr)
}

// runStuckCheck is the re-exec'd test binary's whole run as one verification
// of the snapshot stuckCheckEnv names, in the backups folder after the list
// separator, whose check parks with the database open until its standard
// input ends (the test closes it, or ends), past a deadline of 300 ms.
func runStuckCheck() int {
	dir, root, _ := strings.Cut(os.Getenv(stuckCheckEnv), string(os.PathListSeparator))
	backup.SetVerifyDeadlineForTest(300 * time.Millisecond)
	letGo := make(chan struct{})
	var parked atomic.Bool
	backup.SetOnDatabaseOpenForTest(func() {
		if parked.CompareAndSwap(false, true) {
			fmt.Fprintln(os.Stdout, stuckParked)
			_, _ = io.Copy(io.Discard, os.Stdin)
			close(letGo)
		}
	})
	_, err := backup.VerifySnapshot(context.Background(), dir, backup.VerifyOptions{BackupsRoot: root})
	fmt.Fprintln(os.Stdout, stuckGivenUp, err)
	<-letGo
	// Its worker lets the folder go once the check has ended.
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if _, held := backup.StuckCheckOf(root); !held {
			break
		}
	}
	return 0
}

// verifiedInstallSnapshot takes a snapshot of the install at cfgPath through
// the one path every snapshot takes, and requires both its verdicts VERIFIED.
func verifiedInstallSnapshot(t *testing.T, cfgPath string) string {
	t.Helper()
	cfg, resolved, err := loadCLIConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	res := backup.SnapshotAndVerify(t.Context(), buildBackupSources(cfg, resolved), backup.SnapshotOptions{})
	if !res.Kept() || res.VerifyErr != nil || !res.Verify.Record.Passes() {
		t.Fatalf("the snapshot: kept %v, %v, %+v", res.Kept(), res.VerifyErr, res.Verify.Record)
	}
	return res.Dir
}

// backupLines are the doctor's backups lines for the install at cfgPath, as
// both doctors read them (doctorBackups).
func backupLines(t *testing.T, cfgPath string) []doctor.Check {
	t.Helper()
	cfg, _, err := loadCLIConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	return doctorBackups(cfg)(t.Context())
}

// killedOnceChecked starts cmd, a verification's process that parks once its
// check has ended (the killed fault), and kills it there; a process the test
// leaves goes on once its standard input ends, and ends by itself.
func killedOnceChecked(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr safeBuffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	checked, done := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() { _ = cmd.Wait() })
	drainLoopOnCleanup(t, func() { _ = stdin.Close() }, done, "the verification's output reader")
	go func() {
		defer close(done)
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			if sc.Text() == verifyChecked {
				close(checked)
				break
			}
		}
		_, _ = io.Copy(io.Discard, stdout)
	}()
	select {
	case <-checked:
	case <-done:
		t.Fatalf("the verification's process ended before its check did: %s", stderr.String())
	case <-time.After(2 * time.Minute):
		t.Fatalf("the verification's check never ended: %s", stderr.String())
	}
	_ = cmd.Process.Kill()
	<-done
	// Reaped, so every file it held is closed, its locks with them.
	_ = cmd.Wait()
}

// TestAFreshProcessReadsNoVerifiedAVerificationCouldNotRecord is backlog
// B653's third round's red for what a verification that could not record
// leaves the next process (Claude's first finding on round 2, and the
// reviews of its brief): a snapshot verified once, its database damaged since
// and its files not (mixed verdicts, Codex's second finding), is checked by a
// bridge backup --verify of its own process, whose write of what it found
// fails (its staging refused; its folder's sync failing once the rename has
// landed, and the rename then lost to a crash, its record put back), or that
// can put no veto in place durably (none made; one made that does not sync),
// or that is killed once its check has ended, before its write. Each but the
// killed one says, apart, what its check found and what the record reads, or
// that it checked nothing and what the earlier check's record says, and exits 1;
// a process that put no veto in place never begins its check, nor says what
// a check found. Then a fresh process reads it: where a check
// completed, no reader (the pruner's List, the doctor's lines, the console's
// ReadStatus) reads a VERIFIED, and the prune keeps the older verified
// snapshot; where none ran, the record reads as it did.
//
// What round 2 did with each (measured on its tip with these cases, round 3's
// record, backlog B653): round 2 put a veto in place, synced, before its write
// wherever the record held a VERIFIED, after its check. So stage and unsynced
// are green there on every reader, the lost rename included (the veto stood),
// and red on the words alone; noveto checked, then refused the write, the
// record kept VERIFIED: green on the record, red on the words, and on the
// check the refused process began; vetosync had no seam there, its check
// writing FAILED over the VERIFIED, which is red through the shim alone, not
// round 2's answer to that fault; and killed, between round 2's check and the
// veto its write made, left the VERIFIED with none: red on every reader, and
// on the prune, which removed the older verified snapshot. Where no directory
// can be synced no veto is made and no check over a VERIFIED is recorded
// (TestBridgeBackupVerifySaysWhatARecheckFoundWhereItCannotBeRecorded), so it
// skips there.
func TestAFreshProcessReadsNoVerifiedAVerificationCouldNotRecord(t *testing.T) {
	requireDirectorySync(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		fault string
		// checked: the check ran, and its write failed.
		checked bool
		// crash puts back the record the rename replaced.
		crash bool
		said  []string
	}{
		{fault: "stage", checked: true, said: []string{"this check found:", "database: FAILED", "its record reads", "was not recorded"}},
		{fault: "unsynced", checked: true, crash: true, said: []string{"this check found:", "database: FAILED", "not known to survive a crash"}},
		{fault: "noveto", said: []string{"was not checked", "from an earlier check, which this one did not repeat", "database: VERIFIED"}},
		{fault: "vetosync", said: []string{"was not checked", "from an earlier check, which this one did not repeat", "database: VERIFIED"}},
		// Killed once its check has found the database FAILED, before its
		// write: says nothing, exits by the kill.
		{fault: "killed", checked: true},
	} {
		t.Run(tc.fault, func(t *testing.T) {
			cfgPath, _, root := backupInstall(t)
			anchor := verifiedInstallSnapshot(t, cfgPath)
			time.Sleep(1100 * time.Millisecond)
			dir := verifiedInstallSnapshot(t, cfgPath)
			mpath := filepath.Join(dir, backup.ManifestFile)
			before, err := os.ReadFile(mpath)
			if err != nil {
				t.Fatal(err)
			}
			db := filepath.Join(dir, backup.ManifestDBFileName)
			data, err := os.ReadFile(db)
			if err != nil {
				t.Fatal(err)
			}
			data[len(data)-1] ^= 0xFF
			if err := os.WriteFile(db, data, 0o600); err != nil {
				t.Fatal(err)
			}
			args, err := json.Marshal([]string{"--config", cfgPath, "--verify", dir})
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(exe, "-test.run=^$")
			cmd.Env = append(os.Environ(), verifyFaultEnv+"="+tc.fault, verifyArgsEnv+"="+string(args))
			if tc.fault == "killed" {
				killedOnceChecked(t, cmd)
			} else {
				var stdout, stderr bytes.Buffer
				cmd.Stdout, cmd.Stderr = &stdout, &stderr
				err = cmd.Run()
				var exit *exec.ExitError
				if !errors.As(err, &exit) || exit.ExitCode() != 1 {
					t.Fatalf("the verification's process: %v, want exit 1\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
				}
				said := stdout.String() + stderr.String()
				for _, want := range tc.said {
					if !strings.Contains(said, want) {
						t.Errorf("the verification did not say %q:\n%s", want, said)
					}
				}
				if began := strings.Contains(stdout.String(), verifyBegan); began != tc.checked {
					t.Errorf("the verification's process began its check %v, want %v:\n%s", began, tc.checked, said)
				}
				if !tc.checked && strings.Contains(said, "this check found") {
					t.Errorf("a process that checked nothing said what a check found:\n%s", said)
				}
			}
			if tc.crash {
				if err := os.WriteFile(mpath, before, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			// What a fresh process reads.
			entries, err := backup.List(root)
			if err != nil {
				t.Fatal(err)
			}
			var v backup.Verification
			for _, e := range entries {
				if e.DirName == filepath.Base(dir) {
					v = e.Verdict
				}
			}
			st, err := backup.ReadStatus(t.Context(), root)
			if err != nil {
				t.Fatal(err)
			}
			nv, _ := st.NewestVerified()
			lines := backupLines(t, cfgPath)
			newest := ""
			for _, c := range lines {
				if c.Name == "backup-newest" {
					newest = c.Summary
				}
			}
			if !tc.checked {
				if v.DatabaseState != backup.StateVerified || v.FilesState != backup.StateVerified {
					t.Errorf("a refused check left the record reading %s/%s, want the VERIFIED it read before", v.DatabaseState, v.FilesState)
				}
				if got, _ := os.ReadFile(mpath); !bytes.Equal(got, before) {
					t.Error("a refused check wrote the record")
				}
				return
			}
			if v.DatabaseState == backup.StateVerified || v.FilesState == backup.StateVerified {
				t.Errorf("the pruner reads %s/%s after a check whose record was not written", v.DatabaseState, v.FilesState)
			}
			if nv.DirName != filepath.Base(anchor) {
				t.Errorf("the console's newest verified snapshot is %q, want the older %q", nv.DirName, filepath.Base(anchor))
			}
			if strings.Contains(newest, "database VERIFIED") || strings.Contains(newest, "files VERIFIED") {
				t.Errorf("the doctor reads the snapshot verified: %q", newest)
			}
			if _, err := backup.Prune(root, 1); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(anchor); err != nil {
				t.Errorf("the prune let the older verified snapshot go: %v", err)
			}
		})
	}
}

// TestACheckThatDidNotEndInAnotherProcessHoldsTheFolderUntilItEnds: a
// verification in a process of its own, whose check is given up past its
// deadline with the snapshot's database open, holds the backups folder's
// kernel lock until the check ends (backlog B653's third round, the review of
// its brief: another process for the kernel's lock): meanwhile bridge doctor
// says so (backup-check, naming the snapshot), bridge backup --verify in this
// process answers at once that a check that did not end still runs, and a
// prune is skipped for it; once the test lets the check go and its process
// has ended, the doctor says nothing of it, a prune runs, and the marker the
// check left is gone.
func TestACheckThatDidNotEndInAnotherProcessHoldsTheFolderUntilItEnds(t *testing.T) {
	cfgPath, dataDir, root := backupInstall(t)
	src := backup.Sources{DataDir: dataDir, ManifestDB: manifest.DefaultDBPath(dataDir)}
	dir, err := backup.Snapshot(t.Context(), src)
	if err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^$")
	cmd.Env = append(os.Environ(), stuckCheckEnv+"="+dir+string(os.PathListSeparator)+root)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr safeBuffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var letGo sync.Once
	release := func() { letGo.Do(func() { _ = stdin.Close() }) }
	lines := make(chan string, 16)
	done := make(chan struct{})
	// Registered first, so run last: the process reaped once its check is let
	// go and its output read to the end.
	t.Cleanup(func() { _ = cmd.Wait() })
	drainLoopOnCleanup(t, release, done, "the check's process's output reader")
	go func() {
		defer close(done)
		defer close(lines)
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			select {
			case lines <- sc.Text():
			default:
			}
		}
	}()
	deadline := time.After(2 * time.Minute)
	for given := false; !given; {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatalf("the check's process ended before its check was given up: %s", stderr.String())
			}
			given = strings.HasPrefix(line, stuckGivenUp)
			if given && !strings.Contains(line, backup.ErrCheckGivenUp.Error()) {
				t.Fatalf("the check's process said %q, want its check given up", line)
			}
		case <-deadline:
			t.Fatalf("the check was never given up: %s", stderr.String())
		}
	}
	// The marker its worker writes on a goroutine of its own.
	for wait := time.Now().Add(30 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(filepath.Join(root, ".check-still-running")); err == nil {
			break
		}
		if time.Now().After(wait) {
			t.Fatal("the check's process wrote no marker")
		}
	}
	line := func(name string) (doctor.Check, bool) {
		for _, c := range backupLines(t, cfgPath) {
			if c.Name == name {
				return c, true
			}
		}
		return doctor.Check{}, false
	}
	if c, ok := line("backup-check"); !ok || c.Status != doctor.Warn || !strings.Contains(c.Summary, filepath.Base(dir)) {
		t.Errorf("the doctor's backup-check line beside the check that runs on: %+v, %v", c, ok)
	}
	var out, errs bytes.Buffer
	asked := time.Now()
	if code := backupCmd([]string{"--config", cfgPath, "--verify", dir}, &out, &errs); code != 1 || !strings.Contains(errs.String(), backup.ErrCheckStillRunning.Error()) {
		t.Errorf("bridge backup --verify beside the check that runs on = %d\n%s%s", code, out.String(), errs.String())
	}
	if took := time.Since(asked); took > 5*time.Second {
		t.Errorf("bridge backup --verify waited %v to be refused", took)
	}
	pr, err := backup.Prune(root, 7)
	if err != nil || !pr.Skipped || !errors.Is(pr.SkippedFor, backup.ErrCheckStillRunning) {
		t.Errorf("a prune beside the check that runs on: %+v, %v; want it skipped for the check", pr, err)
	}

	release()
	<-done
	if err := cmd.Wait(); err != nil {
		t.Fatalf("the check's process: %v: %s", err, stderr.String())
	}
	if c, ok := line("backup-check"); ok {
		t.Errorf("the doctor still says a check holds the folder once its process has ended: %+v", c)
	}
	if pr, err := backup.Prune(root, 7); err != nil || pr.Skipped {
		t.Errorf("a prune once the check's process ended: %+v, %v; want it run", pr, err)
	}
	if _, err := os.Stat(filepath.Join(root, ".check-still-running")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the marker of the ended check is still there: %v", err)
	}
}

// TestTheTickerTakesNoSnapshotWhileACheckThatDidNotEndHoldsTheFolder: while a
// check that did not end holds the backups folder, a snapshot's verification
// would be refused and nothing there is pruned, so the ticker's pass takes
// none, saying why, and the doctor's backup-check line names the snapshot;
// once the check has ended, the next pass takes one (backlog B653's third
// round).
func TestTheTickerTakesNoSnapshotWhileACheckThatDidNotEndHoldsTheFolder(t *testing.T) {
	cfgPath, dataDir, root := backupInstall(t)
	src := backup.Sources{DataDir: dataDir, ManifestDB: manifest.DefaultDBPath(dataDir)}
	dir, err := backup.Snapshot(t.Context(), src)
	if err != nil {
		t.Fatal(err)
	}
	// A day old, so the start would take a snapshot.
	raw, err := os.ReadFile(filepath.Join(dir, backup.ManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	var mf backup.Manifest
	if err := json.Unmarshal(raw, &mf); err != nil {
		t.Fatal(err)
	}
	mf.CreatedAt = time.Now().UTC().Add(-48 * time.Hour)
	if raw, err = json.Marshal(mf); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, backup.ManifestFile), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(backup.SetVerifyDeadlineForTest(300 * time.Millisecond))
	entered, out := make(chan struct{}), make(chan struct{})
	var parked atomic.Bool
	t.Cleanup(backup.SetOnDatabaseOpenForTest(func() {
		if parked.CompareAndSwap(false, true) {
			close(entered)
			<-out
		}
	}))
	var letGo sync.Once
	release := func() { letGo.Do(func() { close(out) }) }
	verified := make(chan error, 1)
	callerDone := make(chan struct{})
	// Registered last, so run first: the check let go and its verification
	// answered before the seams are put back.
	t.Cleanup(func() {
		release()
		waitClosed(t, callerDone, "backup verify")
		awaitVerificationsEnd(t, root)
	})
	go func() {
		defer close(callerDone)
		_, err := backup.VerifySnapshot(context.Background(), dir, backup.VerifyOptions{BackupsRoot: root})
		verified <- err
	}()
	<-entered
	select {
	case err := <-verified:
		if !errors.Is(err, backup.ErrCheckGivenUp) {
			t.Fatalf("the verification answered %v, want its check given up", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the check was never given up")
	}
	bootPass := func() (string, string) {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		drainLoopOnCleanup(t, cancel, done, "the backup ticker")
		var stdout, stderr safeBuffer
		status := &sweepStatus[struct{}]{}
		go func() {
			defer close(done)
			runBackupTicker(ctx, src, func() int { return 7 }, staticInterval(24*time.Hour), nil, &stdout, &stderr, status)
		}()
		deadline := time.Now().Add(60 * time.Second)
		for {
			if _, _, lastEnd, _, _ := status.snapshot(); !lastEnd.IsZero() || strings.Contains(stderr.String(), "no snapshot taken") {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("the pass did not end\nstdout %s\nstderr %s", stdout.String(), stderr.String())
			}
			time.Sleep(5 * time.Millisecond)
		}
		cancel()
		waitClosed(t, done, "the backup ticker")
		return stdout.String(), stderr.String()
	}
	stdout, stderr := bootPass()
	if n := len(snapshotDirs(t, root)); n != 1 || !strings.Contains(stderr, "no snapshot taken: a check of "+filepath.Base(dir)+" that did not end") {
		t.Errorf("a pass beside the check that runs on: %d snapshots, want 1, said why\nstdout %s\nstderr %s", n, stdout, stderr)
	}
	found := false
	for _, c := range backupLines(t, cfgPath) {
		if c.Name == "backup-check" {
			found = c.Status == doctor.Warn && strings.Contains(c.Summary, filepath.Base(dir))
		}
	}
	if !found {
		t.Error("the doctor says nothing of the check that runs on")
	}
	release()
	awaitVerificationsEnd(t, root)
	for wait := time.Now().Add(30 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, held := backup.StuckCheckOf(root); !held {
			break
		}
		if time.Now().After(wait) {
			t.Fatal("the check still reads as running once it was let go")
		}
	}
	stdout, stderr = bootPass()
	if n := len(snapshotDirs(t, root)); n != 2 {
		t.Errorf("a pass once the check ended: %d snapshots, want 2\nstdout %s\nstderr %s", n, stdout, stderr)
	}
}

// TestAStartupCheckThatFindsTheDatabaseFailedTakesASnapshotWhetherOrNotItsRecordIsWritten
// is backlog B653's third round's red for the startup pass's check in place
// (Claude's second finding on round 2): a check of the newest snapshot that
// finds its database FAILED leaves no recovery point from the last day, so
// the pass takes a snapshot after it, decided on what the check found, which
// a record its write could not write does not change. Red on the round
// before, which decided on the record and an answer with no error, and took
// none.
func TestAStartupCheckThatFindsTheDatabaseFailedTakesASnapshotWhetherOrNotItsRecordIsWritten(t *testing.T) {
	_, dataDir, root := backupInstall(t)
	src := backup.Sources{DataDir: dataDir, ManifestDB: manifest.DefaultDBPath(dataDir)}
	dir, err := backup.Snapshot(t.Context(), src)
	if err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(dir, backup.ManifestDBFileName)
	data, err := os.ReadFile(db)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)-1] ^= 0xFF
	if err := os.WriteFile(db, data, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(backup.SetVerdictWriteFailuresForTest(func(d string) error {
		if d == dir {
			return errors.New("injected: the staging failed")
		}
		return nil
	}, nil, nil))
	t.Cleanup(func() { awaitHoldsLetGo(t, root) })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	drainLoopOnCleanup(t, cancel, done, "the backup ticker")
	var stdout, stderr safeBuffer
	status := &sweepStatus[struct{}]{}
	go func() {
		defer close(done)
		runBackupTicker(ctx, src, func() int { return 7 }, staticInterval(24*time.Hour), nil, &stdout, &stderr, status)
	}()
	// The check in place says it was kept, then the snapshot after it comes
	// at once, or not at all.
	waitFor := func(cond func() bool, within time.Duration) bool {
		for deadline := time.Now().Add(within); !cond(); time.Sleep(10 * time.Millisecond) {
			if time.Now().After(deadline) {
				return false
			}
		}
		return true
	}
	if !waitFor(func() bool { return strings.Contains(stderr.String(), "was kept") }, 2*time.Minute) {
		t.Fatalf("the check in place never reported\nstdout %s\nstderr %s", stdout.String(), stderr.String())
	}
	waitFor(func() bool {
		return len(snapshotDirs(t, root)) >= 2 && strings.Contains(stdout.String(), "): verified ")
	}, 30*time.Second)
	cancel()
	waitClosed(t, done, "the backup ticker")
	if n := len(snapshotDirs(t, root)); n != 2 {
		t.Errorf("a start whose check in place found the database FAILED and could not record it left %d snapshots, want 2\nstdout %s\nstderr %s",
			n, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "its check found database FAILED") {
		t.Errorf("the pass did not say what its check found:\n%s", stderr.String())
	}
}

// TestATickerStoppedWhileItsProbeOfTheLockHangsReturnsWithinItsBound is
// backlog B653's third round's case for the ticker's own reads before a
// capture (Codex's third finding on round 2): stopped while its probe of the
// backups folder's lock does not answer (a hung mount), the ticker returns
// within backup.StopWriteBound of the stop, and says nothing of the stop. Not
// red at the round before by any real object (a local filesystem's open of a
// lock file does not hang); its control is the probe taken back onto the
// ticker's goroutine. The manifest read that hangs is
// TestATickerStoppedWhileItsStartupListingReadsANamedPipeReturnsWithinItsBound's.
func TestATickerStoppedWhileItsProbeOfTheLockHangsReturnsWithinItsBound(t *testing.T) {
	_, dataDir, _ := backupInstall(t)
	src := backup.Sources{DataDir: dataDir, ManifestDB: manifest.DefaultDBPath(dataDir)}
	entered, out := make(chan struct{}), make(chan struct{})
	var parked atomic.Bool
	answered := make(chan struct{})
	t.Cleanup(backup.SetOpenProbeForTest(func(path string) (*os.File, error) {
		if parked.CompareAndSwap(false, true) {
			defer close(answered)
			close(entered)
			<-out
		}
		return os.OpenFile(path, os.O_RDWR, 0)
	}))
	var letGo sync.Once
	release := func() { letGo.Do(func() { close(out) }) }
	// Registered after the seam, so run before its restore: the probe has
	// answered.
	t.Cleanup(func() {
		release()
		if parked.Load() {
			<-answered
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	// The drain, run first, lets the probe go too, so a ticker held in it
	// (a probe run on its own goroutine) ends.
	drainLoopOnCleanup(t, func() { release(); cancel() }, done, "the backup ticker")
	var stdout, stderr safeBuffer
	go func() {
		defer close(done)
		runBackupTicker(ctx, src, func() int { return 7 }, staticInterval(24*time.Hour), nil, &stdout, &stderr, nil)
	}()
	select {
	case <-entered:
	case <-time.After(60 * time.Second):
		t.Fatalf("the ticker never probed the lock\nstdout %s\nstderr %s", stdout.String(), stderr.String())
	}
	stoppedAt := time.Now()
	cancel()
	select {
	case <-done:
		if took := time.Since(stoppedAt); took > backup.StopWriteBound+time.Second {
			t.Errorf("the ticker returned %v after the stop; the bound is %v", took, backup.StopWriteBound)
		}
	case <-time.After(backup.StopWriteBound + 10*time.Second):
		t.Fatal("the ticker did not return while its probe of the lock hung")
	}
	if got := stderr.String(); got != "" {
		t.Errorf("a stopped pass was reported: %s", got)
	}
}
