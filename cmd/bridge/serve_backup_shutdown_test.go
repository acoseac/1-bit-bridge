package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/backup/backuptest"
	"github.com/acoseac/1-bit-bridge/internal/sqlitetest"
)

// mainGivesUpOnTheSnapshotBy is how long after the cancel this test still
// expects the old shutdown to have returned. That shutdown is the HTTP
// drain (at most shutdownGrace), then the shared writer grace
// (shutdownGrace), then the store close, whose busy wait is the manifest
// DSN's 5s. Past this, a serve that is still running is the backup
// ticker's own wait, which is backupShutdownWait and starts only once the
// drain has returned.
const mainGivesUpOnTheSnapshotBy = 25 * time.Second

// TestAShutdownWaitsForTheStartupSnapshotToCloseItsFile is the serve-test
// failure Windows CI hit as "The process cannot access the file because it
// is being used by another process" on data/backups/<stamp>/bridge.db
// (backlog B309). The startup snapshot is a VACUUM INTO on the serve
// context. Cancelling it does not close the output file until the
// statement returns, and the shared writer grace gives that statement up.
// macOS deletes an open file, so the same run cleans its TempDir there.
func TestAShutdownWaitsForTheStartupSnapshotToCloseItsFile(t *testing.T) {
	if backupShutdownWait <= mainGivesUpOnTheSnapshotBy+shutdownGrace {
		t.Fatalf("backupShutdownWait %s must outlast %s plus a drain of %s",
			backupShutdownWait, mainGivesUpOnTheSnapshotBy, shutdownGrace)
	}

	root := t.TempDir()
	lib := filepath.Join(root, "Music")
	dataDir := filepath.Join(root, "data")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// The manifest database has to carry the park collation before serve
	// opens it. The startup snapshot is the only VACUUM, and it is what
	// the collation stops, with the snapshot file already created.
	backuptest.WriteSource(t, filepath.Join(dataDir, "bridge.db"))

	lan, admin := holdLoopback(t), holdLoopback(t)
	listenAddr := lan.addr
	consoleAddr := admin.addr
	configPath := filepath.Join(root, "bridge.yaml")
	yamlText := "libraryRoots:\n  - " + lib + "\ndataDir: " + dataDir +
		"\nadminAddress: " + consoleAddr + "\n"
	if err := os.WriteFile(configPath, []byte(yamlText), 0o600); err != nil {
		t.Fatal(err)
	}

	park := sqlitetest.ArmUntil(t, serveGiveUpTime(t))
	b := launchServe(t, func(ctx context.Context, stdout, stderr io.Writer) int {
		return runServe(ctx, serveOpts{
			configPath: configPath, addrOverride: listenAddr,
			lanListener: lan.ln, adminListener: admin.ln,
		}, stdout, stderr)
	})
	// Armed before the launch, so the snapshot parks during boot. The
	// drain is registered inside launchServe; this cleanup is after it
	// and so runs first, and a serve that is still in the snapshot can
	// leave it.
	t.Cleanup(park.Disarm)

	park.Wait(t)
	held, err := heldSnapshotFile(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if held == "" {
		t.Fatal("the parked snapshot's bridge.db is not open in this process")
	}

	b.stop()
	timer := time.NewTimer(mainGivesUpOnTheSnapshotBy)
	defer timer.Stop()
	select {
	case code := <-b.done:
		// Serve already gave the snapshot up. Read the hold before
		// releasing it, then let the statement finish so the TempDir
		// cleanup has a file it can delete on Windows.
		still, err := heldSnapshotFile(dataDir)
		if err != nil {
			park.Disarm()
			t.Fatal(err)
		}
		park.Disarm()
		releaseSnapshotFiles(t, dataDir)
		if still == "" {
			t.Fatalf("serve exited %d within %s and the snapshot file was already closed; the hold did not outlast shutdown\n%s",
				code, mainGivesUpOnTheSnapshotBy, b.stderr.String())
		}
		t.Fatalf("serve exited %d while %s was still open\n%s", code, still, b.stderr.String())
	case <-timer.C:
	}

	park.Disarm()
	select {
	case code := <-b.done:
		if code != 0 {
			t.Fatalf("serve exit code = %d, want 0\n%s", code, b.stderr.String())
		}
	case <-serveGiveUp(t):
		t.Fatalf("serve did not return after the snapshot was released\n%s", b.stderr.String())
	}
	releaseSnapshotFiles(t, dataDir)
	stderr := b.stderr.String()
	if strings.Contains(stderr, "did not drain within grace") || strings.Contains(stderr, "did not close its files") {
		t.Fatalf("shutdown gave the snapshot up\n%s", stderr)
	}
}

// heldSnapshotFile is the snapshot bridge.db this process still has open,
// or "" when every snapshot file under dataDir is closed or already gone.
func heldSnapshotFile(dataDir string) (string, error) {
	dbs, err := filepath.Glob(filepath.Join(dataDir, "backups", "*", "bridge.db"))
	if err != nil {
		return "", err
	}
	for _, db := range dbs {
		for _, path := range []string{db, db + "-journal", db + "-wal", db + "-shm"} {
			held, err := processHoldsFile(path)
			if err != nil {
				return "", err
			}
			if held {
				return path, nil
			}
		}
	}
	return "", nil
}

// releaseSnapshotFiles waits until the snapshot files are closed, so a
// failure path does not hand Windows a TempDir cleanup of a file the
// statement was still writing.
func releaseSnapshotFiles(t *testing.T, dataDir string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		held, err := heldSnapshotFile(dataDir)
		if err != nil {
			t.Error(err)
			return
		}
		if held == "" {
			return
		}
		if time.Now().After(deadline) {
			t.Errorf("snapshot file still open after the park was released: %s", held)
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}
