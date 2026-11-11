//go:build unix

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// pipeWriter waits until a reader has the named pipe at path open, and
// answers a writer of it, which the reader's read then waits on, as a read of
// a hung mount waits.
func pipeWriter(t *testing.T, path string) *os.File {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		f, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0)
		if err == nil {
			return f
		}
		if !errors.Is(err, syscall.ENXIO) {
			t.Fatalf("open the manifest's pipe for writing: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatal("nothing read the manifest's pipe")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestATickerStoppedWhileItsStartupListingReadsANamedPipeReturnsWithinItsBound
// is backlog B653's third round's red for the ticker's own reads before a
// capture (Codex's third finding on round 2): a snapshot folder whose
// manifest is a named pipe no one writes holds the startup pass's listing
// (backup.ReadStatus), as a read of a hung mount does, and the ticker, stopped
// there, returns within backup.StopWriteBound of the stop and says nothing of
// the stop; the read is answered once the test ends it. Red on the round
// before, whose listing ran on the ticker's own goroutine.
func TestATickerStoppedWhileItsStartupListingReadsANamedPipeReturnsWithinItsBound(t *testing.T) {
	_, dataDir, root := backupInstall(t)
	src := backup.Sources{DataDir: dataDir, ManifestDB: manifest.DefaultDBPath(dataDir)}
	pipeDir := filepath.Join(root, "2026-01-01T00-00-00Z")
	if err := os.MkdirAll(pipeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	mpath := filepath.Join(pipeDir, backup.ManifestFile)
	if err := syscall.Mkfifo(mpath, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	drainLoopOnCleanup(t, cancel, done, "the backup ticker")
	var stdout, stderr safeBuffer
	go func() {
		defer close(done)
		runBackupTicker(ctx, src, func() int { return 7 }, staticInterval(24*time.Hour), nil, &stdout, &stderr, nil)
	}()
	w := pipeWriter(t, mpath)
	// Registered last, so run first: the read the ticker left is answered,
	// an end of the pipe, before the drain.
	t.Cleanup(func() { _ = w.Close() })
	stoppedAt := time.Now()
	cancel()
	select {
	case <-done:
		if took := time.Since(stoppedAt); took > backup.StopWriteBound+time.Second {
			t.Errorf("the ticker returned %v after the stop; the bound is %v", took, backup.StopWriteBound)
		}
	case <-time.After(backup.StopWriteBound + 10*time.Second):
		t.Fatal("the ticker did not return while its startup listing read a pipe no one writes")
	}
	if got := stderr.String(); got != "" {
		t.Errorf("a stopped pass was reported: %s", got)
	}
}
