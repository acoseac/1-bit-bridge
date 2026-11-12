//go:build unix

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/backup"
)

// stallManifest puts a named pipe where the snapshot in dir keeps its
// manifest, so a read of it waits for the pipe's writer, a mount that stopped
// answering's stand-in. It answers its feed, which writes the manifest's bytes
// to a reader there is and closes the pipe; the test's end feeds one left and
// puts the manifest back.
func stallManifest(t *testing.T, dir string) (feed func()) {
	t.Helper()
	path := filepath.Join(dir, backup.ManifestFile)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	feed = func() {
		// A write end that does not wait opens only where a reader has the
		// pipe open; a reader to come opens nothing then, and reads the
		// manifest put back.
		if w, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			_, _ = w.Write(data)
			_ = w.Close()
		}
	}
	t.Cleanup(func() {
		feed()
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Error(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Error(err)
		}
	})
	return feed
}

// TestBridgeRestoreAnswersAStalledSnapshotWithinItsBoundBeforeItsPrompt is
// backlog B826's second round's red for item 2 (Codex's P2 on #1452): bridge
// restore's first read of the snapshot, before its prompt, is bounded as the
// restore's admission is, so a snapshot whose manifest does not answer (a
// named pipe stands in for a mount that stopped answering) is refused within
// that bound, in its words, before any prompt and with nothing written. Red on
// round 1, whose first read (LooksLikeSnapshotDir) waited on the pipe until it
// was fed; its control puts that unbounded read back.
func TestBridgeRestoreAnswersAStalledSnapshotWithinItsBoundBeforeItsPrompt(t *testing.T) {
	_, _, snapA := seedRestorableInstall(t, closedAdminAddr(t))
	cfgB, dataB, _ := seedRestorableInstall(t, closedAdminAddr(t))
	before := readInstallState(t, cfgB, dataB)
	feed := stallManifest(t, snapA)
	prev := verifyCmdLockWait
	verifyCmdLockWait = 100 * time.Millisecond
	t.Cleanup(func() { verifyCmdLockWait = prev })

	var stdout, stderr safeBuffer
	done := make(chan int, 1)
	t.Cleanup(func() {
		feed()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Error("bridge restore never returned once its snapshot answered")
		}
	})
	start := time.Now()
	go func() {
		done <- restoreCmd(context.Background(), []string{"--config", cfgB, snapA}, strings.NewReader(""), &stdout, &stderr)
	}()
	// Its bound: the lock wait, the watchdog's slack (twice StopWriteBound),
	// then StopWriteBound.
	bound := verifyCmdLockWait + 3*backup.StopWriteBound
	var code int
	select {
	case code = <-done:
		done <- code
	case <-time.After(bound + 10*time.Second):
		t.Fatalf("bridge restore did not answer within %v of a snapshot that does not answer", bound+10*time.Second)
	}
	if took := time.Since(start); took > bound+2*time.Second {
		t.Errorf("bridge restore answered after %v, past its bound of %v", took, bound)
	}
	if code != 1 || !strings.Contains(stderr.String(), "did not answer within") || !strings.Contains(stderr.String(), "nothing was restored") {
		t.Errorf("bridge restore beside a snapshot that does not answer = %d\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	if strings.Contains(stderr.String(), "Type 'yes'") || strings.Contains(stdout.String(), "Restore from") {
		t.Errorf("bridge restore asked for its confirmation of a snapshot that does not answer:\n%s%s", stdout.String(), stderr.String())
	}
	requireInstallUntouched(t, cfgB, dataB, before)
}
