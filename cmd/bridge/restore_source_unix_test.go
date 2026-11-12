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
	// The look's worker outlives the command's answer, its read of the pipe
	// under way: it is fed and its end observed (backlog B871's item 7) before
	// the hook's restore, the pipe's removal and the fixtures' (cleanups
	// registered before the one that does it, so run after it).
	ended := lookWorkerEnd(t, snapA)
	feed := stallManifest(t, snapA)
	prev := verifyCmdLockWait
	verifyCmdLockWait = 100 * time.Millisecond
	t.Cleanup(func() { verifyCmdLockWait = prev })
	run := newRestoreRun(t)
	t.Cleanup(func() {
		if !run.started.Load() {
			return
		}
		// The pipe is fed until the worker ends: a worker not at its read
		// yet when the first feed comes is fed when it gets there.
		giveUp := time.After(restoreWaitWithin(t, 30*time.Second))
		for {
			feed()
			select {
			case <-ended:
				return
			case <-giveUp:
				t.Error("the worker of bridge restore's look never ended once its snapshot answered")
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
	})

	var stdout, stderr safeBuffer
	start := time.Now()
	run.start(context.Background(), []string{"--config", cfgB, snapA}, strings.NewReader(""), &stdout, &stderr)
	// Its bound: the lock wait, the watchdog's slack (twice StopWriteBound),
	// then StopWriteBound.
	bound := verifyCmdLockWait + 3*backup.StopWriteBound
	var code int
	select {
	case code = <-run.done:
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
