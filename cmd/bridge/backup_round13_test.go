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
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	drainLoopOnCleanup(t, cancel, done, "backup ticker replacing a vetoed record")
	var stdout, stderr safeBuffer
	status := &sweepStatus[struct{}]{}
	src := backup.Sources{DataDir: dataDir, ManifestDB: manifest.DefaultDBPath(dataDir), BridgeYAML: cfgPath}
	go func() {
		defer close(done)
		runBackupTicker(ctx, src, func() int { return 7 }, staticInterval(24*time.Hour), nil, &stdout, &stderr, status)
	}()
	for deadline := time.Now().Add(30 * time.Second); !strings.Contains(stdout.String(), "): verified "); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("startup did not replace its vetoed snapshot\nstdout %s\nstderr %s", stdout.String(), stderr.String())
		}
	}
	cancel()
	waitClosed(t, done, "backup ticker")
	awaitHoldsLetGo(t, root)
	if dirs := snapshotDirs(t, root); len(dirs) != 2 {
		t.Fatalf("startup left %d snapshots, want vetoed original plus fresh snapshot", len(dirs))
	}
	if !strings.Contains(stderr.String(), "check found database VERIFIED") || !strings.Contains(stderr.String(), "files FAILED") {
		t.Errorf("startup did not exercise the mixed finding: %s", stderr.String())
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
}
