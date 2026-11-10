package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

func TestStartupReplacesAVetoedSnapshotEvenWhenItsDatabasePasses(t *testing.T) {
	cfgPath, dataDir, root := backupInstall(t)
	t.Cleanup(func() { awaitHoldsLetGo(t, root) })
	dir := verifiedInstallSnapshot(t, cfgPath)
	awaitHoldsLetGo(t, root)
	flipBundleByte(t, dir, "bridge.yaml")
	t.Cleanup(backup.SetSyncsParentDirForTest(false))
	t.Cleanup(func() { awaitHoldsLetGo(t, root) })
	checked, err := backup.VerifySnapshot(context.Background(), dir, backup.VerifyOptions{BackupsRoot: root})
	awaitHoldsLetGo(t, root)
	if !errors.Is(err, backup.ErrRecheckNotRecorded) || checked.Found.DatabaseState != backup.StateVerified || checked.Found.FilesState != backup.StateFailed || checked.Record.DatabaseState != backup.StateIncomplete {
		t.Fatalf("files-only veto: %+v, %v", checked, err)
	}
	src := backup.Sources{DataDir: dataDir, ManifestDB: manifest.DefaultDBPath(dataDir), BridgeYAML: cfgPath}
	_, stderr := runBackupStartupToCompletion(t, root, src)
	if dirs := snapshotDirs(t, root); len(dirs) != 2 {
		t.Fatalf("startup left %d snapshots, want vetoed original plus fresh snapshot", len(dirs))
	}
	if !strings.Contains(stderr, "check found database VERIFIED") || !strings.Contains(stderr, "files FAILED") {
		t.Errorf("startup did not exercise the mixed finding: %s", stderr)
	}
	if _, err := os.Stat(filepath.Join(dir, backup.ManifestFile)); err != nil {
		t.Errorf("lost original snapshot: %v", err)
	}
	st, err := backup.ReadStatus(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if pass := decideStartupPass(st, time.Now(), 24*time.Hour); pass.action != startupSkipVerified {
		t.Errorf("next startup would not skip its fresh verified snapshot: %+v", pass)
	}
	stdout, _ := runBackupStartupToCompletion(t, root, src)
	if !strings.Contains(stdout, "recent verified snapshot") || len(snapshotDirs(t, root)) != 2 {
		t.Errorf("second start did not keep exactly one replacement: %s", stdout)
	}
}

func TestStartupKeepsALeftoverVetoWithoutAddingSnapshots(t *testing.T) {
	cfgPath, dataDir, root := backupInstall(t)
	t.Cleanup(backup.SetSyncsParentDirForTest(false))
	t.Cleanup(backup.SetRemoveVetoForTest(func(string) error { return errors.New("removal refused") }))
	t.Cleanup(func() { awaitHoldsLetGo(t, root) })
	src := backup.Sources{DataDir: dataDir, ManifestDB: manifest.DefaultDBPath(dataDir), BridgeYAML: cfgPath}
	res := backup.SnapshotAndVerify(context.Background(), src, backup.SnapshotOptions{})
	awaitHoldsLetGo(t, root)
	if !res.Kept() || !res.Verify.Found.Passes() || !res.Verify.RecordRead || res.Verify.Record.DatabaseState != backup.StateIncomplete || res.VerifyErr == nil {
		t.Fatalf("initial passing check with leftover veto: %+v", res)
	}
	for boot := 1; boot <= 2; boot++ {
		stdout, stderr := runBackupStartupToCompletion(t, root, src)
		if !strings.Contains(stdout, "verifying it in place") ||
			!strings.Contains(stderr, "check found database VERIFIED, files VERIFIED") ||
			!strings.Contains(stderr, backup.ErrRecheckNotRecorded.Error()) {
			t.Errorf("start %d did not recheck the leftover veto: stdout %s; stderr %s", boot, stdout, stderr)
		}
		if dirs := snapshotDirs(t, root); len(dirs) != 1 || dirs[0] != res.Dir {
			t.Errorf("start %d added snapshots despite a passing check: %v; want only %s", boot, dirs, res.Dir)
		}
	}
}

// runBackupStartupToCompletion waits until the shared loop asks for its next
// interval, after the whole boot pass (including any replacement) has returned.
// Retention is off so an unwanted capture cannot be hidden by a prune.
func runBackupStartupToCompletion(t *testing.T, root string, src backup.Sources) (string, string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done, bootDone := make(chan struct{}), make(chan struct{})
	drainLoopOnCleanup(t, cancel, done, "backup ticker startup")
	var stdout, stderr safeBuffer
	calls := 0
	interval := func() time.Duration {
		calls++
		if calls == 2 {
			close(bootDone)
		}
		return 24 * time.Hour
	}
	go func() {
		defer close(done)
		runBackupTicker(ctx, src, func() int { return 0 }, interval, nil, &stdout, &stderr, &sweepStatus[struct{}]{})
	}()
	bootWait := time.NewTimer(30 * time.Second)
	defer bootWait.Stop()
	select {
	case <-bootDone:
	case <-bootWait.C:
		t.Fatalf("backup startup did not finish: stdout %s; stderr %s", stdout.String(), stderr.String())
	}
	cancel()
	waitClosed(t, done, "backup ticker")
	awaitHoldsLetGo(t, root)
	return stdout.String(), stderr.String()
}
