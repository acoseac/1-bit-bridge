package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// TestTheStartupPassSaysWhyARecheckThatRecordedNothingPrunedNothing is backlog
// B653's fifth round's red for what the ticker says of the prune after a
// recheck that recorded nothing (the Claude API's High on round 4, and the
// brief's review of its words): where no directory can be synced (Windows,
// stood in for through its seam), the startup pass's check in place of a
// snapshot whose record holds a VERIFIED beside a check that did not end
// records nothing, so it prunes nothing, and says why in those words, never
// as a lock held elsewhere. Red on the round before, which pruned after it.
func TestTheStartupPassSaysWhyARecheckThatRecordedNothingPrunedNothing(t *testing.T) {
	_, dataDir, root := backupInstall(t)
	src := backup.Sources{DataDir: dataDir, ManifestDB: manifest.DefaultDBPath(dataDir)}
	dir, err := backup.Snapshot(t.Context(), src)
	if err != nil {
		t.Fatal(err)
	}
	if res, err := backup.VerifySnapshot(t.Context(), dir, backup.VerifyOptions{BackupsRoot: root}); err != nil || !res.Record.Passes() {
		t.Fatalf("the snapshot's check: %v, %+v", err, res.Record)
	}
	// Its database's check did not end since: its files stay VERIFIED, so
	// the startup pass checks it again in place of a new snapshot.
	mpath := filepath.Join(dir, backup.ManifestFile)
	data, err := os.ReadFile(mpath)
	if err != nil {
		t.Fatal(err)
	}
	var m backup.Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	m.Verification.DatabaseState, m.Verification.DatabaseReasons = backup.StateIncomplete, []backup.Reason{backup.ReasonDeadline}
	if data, err = json.MarshalIndent(m, "", "  "); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mpath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(backup.SetSyncsParentDirForTest(false))
	t.Cleanup(func() { awaitHoldsLetGo(t, root) })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	drainLoopOnCleanup(t, cancel, done, "the backup ticker")
	var stdout, stderr safeBuffer
	go func() {
		defer close(done)
		runBackupTicker(ctx, src, func() int { return 7 }, staticInterval(24*time.Hour), nil, &stdout, &stderr, &sweepStatus[struct{}]{})
	}()
	for deadline := time.Now().Add(2 * time.Minute); !strings.Contains(stderr.String(), "which was not recorded"); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the check in place never reported\nstdout %s\nstderr %s", stdout.String(), stderr.String())
		}
	}
	// The prune is reported on stdout after the verdict on stderr.
	for deadline := time.Now().Add(2 * time.Minute); !strings.Contains(stdout.String(), "backup (startup): prune skipped: the check of a snapshot whose record holds a verified verdict recorded nothing, so no prune trusts that record"); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the prune never reported\nstdout %s\nstderr %s", stdout.String(), stderr.String())
		}
	}
	cancel()
	waitClosed(t, done, "the backup ticker")
	out := stdout.String()
	if !strings.Contains(out, "backup (startup): prune skipped: the check of a snapshot whose record holds a verified verdict recorded nothing, so no prune trusts that record") {
		t.Errorf("the pass did not say why it pruned nothing:\nstdout %s\nstderr %s", out, stderr.String())
	}
	if strings.Contains(out, "held their lock") {
		t.Errorf("the pass said a lock held elsewhere skipped the prune:\n%s", out)
	}
}

// TestTheDoctorKeepsOneReadOfABackupsFolderThatDoesNotAnswer is backlog
// B653's fifth round's red for the doctor's reads of a backups folder that
// does not answer (the Claude API's Low on round 4): one read of the folder is
// out at a time, so doctor runs while it is out wait for that one, and say it
// did not answer, rather than each leaving a read of its own behind. Red on
// the round before, which began a read for every run.
func TestTheDoctorKeepsOneReadOfABackupsFolderThatDoesNotAnswer(t *testing.T) {
	cfgPath, _, _ := backupInstall(t)
	cfg, _, err := loadCLIConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	stalled := make(chan struct{})
	var letGo sync.Once
	stall := func() error { <-stalled; return errors.New("stalled") }
	t.Cleanup(backup.SetReadDirForTest(func(string) ([]os.DirEntry, error) { return nil, stall() }))
	t.Cleanup(backup.SetMetadataForTest(func(string) (string, error) { return "", stall() }, func(string) (os.FileInfo, error) { return nil, stall() }))
	t.Cleanup(backup.SetOpenProbeForTest(func(string) (*os.File, error) { return nil, stall() }))
	prevBound, prevEnd := doctorBackupsBound, onDoctorBackupsReadEnd
	var reads atomic.Int32
	ended := make(chan struct{}, 8)
	doctorBackupsBound = 200 * time.Millisecond
	onDoctorBackupsReadEnd = func() {
		reads.Add(1)
		ended <- struct{}{}
	}
	t.Cleanup(func() { doctorBackupsBound, onDoctorBackupsReadEnd = prevBound, prevEnd })
	// Registered last, so run first: the folder answers again, and every read
	// left on it has ended, before a stand-in is put back.
	t.Cleanup(func() {
		letGo.Do(func() { close(stalled) })
		quiet := time.NewTimer(2 * time.Second)
		defer quiet.Stop()
	drain:
		for {
			select {
			case <-ended:
				quiet.Reset(2 * time.Second)
			case <-quiet.C:
				break drain
			}
		}
	})
	for run := 1; run <= 3; run++ {
		lines := doctorBackups(cfg)(context.Background())
		answered := false
		for _, l := range lines {
			if l.Name == "backup-newest" && strings.Contains(l.Summary, "did not answer") {
				answered = true
			}
		}
		if !answered {
			t.Errorf("run %d: the doctor's lines did not say the folder did not answer: %+v", run, lines)
		}
	}
	letGo.Do(func() { close(stalled) })
	select {
	case <-ended:
	case <-time.After(30 * time.Second):
		t.Fatal("the doctor's read of the backups folder never ended")
	}
	// Any other read the runs left ends as soon as the folder answers.
	time.Sleep(500 * time.Millisecond)
	if n := reads.Load(); n != 1 {
		t.Errorf("three doctor runs left %d reads of the folder that did not answer, want one", n)
	}
}

// TestTheTickerAndTheDoctorNameAPruneGivenUp: a prune given up that holds the
// backups folder (backlog B653's fifth round: the prune holds the folder as
// every holder does, PruneContext) is named as a prune, never as a check of a
// snapshot with no name, by the ticker's reason for taking no snapshot and by
// the doctor's backup-check line. The prune runs once the snapshot's
// verification has ended (endedInstallSnapshot, backlog B869): one run on its
// answer could meet the lock its worker had yet to let go, and a prune that
// meets it is skipped with no error, never given up.
func TestTheTickerAndTheDoctorNameAPruneGivenUp(t *testing.T) {
	cfgPath, _, root := backupInstall(t)
	endedInstallSnapshot(t, cfgPath, root)
	t.Cleanup(backup.SetVerifyDeadlineForTest(300 * time.Millisecond))
	stalled := make(chan struct{})
	var letGo sync.Once
	t.Cleanup(backup.SetOnPruneForTest(func() { <-stalled }))
	callerDone := make(chan struct{})
	drainLoopOnCleanup(t, func() { letGo.Do(func() { close(stalled) }) }, callerDone, "backup prune")
	t.Cleanup(func() {
		letGo.Do(func() { close(stalled) })
		waitClosed(t, callerDone, "backup prune")
		awaitHoldsLetGo(t, root)
	})
	type answer struct {
		res backup.PruneResult
		err error
	}
	pruned := make(chan answer, 1)
	go func() {
		defer close(callerDone)
		res, err := backup.Prune(root, 1)
		pruned <- answer{res, err}
	}()
	select {
	case a := <-pruned:
		if !errors.Is(a.err, backup.ErrPruneGivenUp) {
			t.Fatalf("the prune answered %+v, %v; want it given up", a.res, a.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the prune whose removals hang was never given up")
	}
	if s := awaitMarked(t, root); !s.Pruning() {
		t.Fatalf("the folder is marked %+v, want the prune's hold", s)
	}
	why, held := captureHeldBack(context.Background(), root)
	if !held || !strings.Contains(why, "a prune of the snapshots that did not end still holds the backups folder") {
		t.Errorf("the ticker's reason = %q, %v; want the prune named", why, held)
	}
	lines := backupLines(t, cfgPath)
	found := false
	for _, l := range lines {
		if l.Name == "backup-check" {
			found = true
			if !strings.Contains(l.Summary, "a prune of the snapshots did not end (its removals)") {
				t.Errorf("backup-check = %q; want the prune named, at its removals", l.Summary)
			}
		}
	}
	if !found {
		t.Errorf("the doctor has no backup-check line beside a prune given up: %+v", lines)
	}
}
