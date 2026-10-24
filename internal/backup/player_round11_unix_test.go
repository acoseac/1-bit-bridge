//go:build unix

package backup_test

import (
	"bufio"
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// withinDeadline runs fn and fails the test, without waiting for it, when
// it has not returned in time: what a FIFO opened for reading would do.
func withinDeadline(t *testing.T, what string, fn func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		return err
	case <-time.After(20 * time.Second):
		t.Fatalf("%s blocked: it opened the named pipe at the seal path", what)
		return nil
	}
}

// TestAFIFOSealIsKeptWithoutOpeningIt: a named pipe at the seal path is a
// seal too. Start and the preflight's advice keep it, without opening it.
func TestAFIFOSealIsKeptWithoutOpeningIt(t *testing.T) {
	reservationStorage(t)
	t.Run("start", func(t *testing.T) {
		dir := t.TempDir()
		sealedMint(t, dir)
		if err := os.Remove(filepath.Join(dir, "player-initialized")); err != nil {
			t.Fatal(err)
		}
		seal := filepath.Join(dir, sealFile)
		if err := os.Remove(seal); err != nil {
			t.Fatal(err)
		}
		if err := syscall.Mkfifo(seal, 0600); err != nil {
			t.Fatal(err)
		}
		rec := loggingtest.Record(t)
		err := withinDeadline(t, "start", func() error { _, _, err := backup.EnsurePlayerUUID(dir); return err })
		noMoveAsideAdvice(t, rec, err)
		if err != nil {
			t.Fatalf("start refused a FIFO seal: %v", err)
		}
		if m := markerAt(t, dir); !m.Sealed || !m.Minted {
			t.Fatalf("start did not record the seal again: %+v", m)
		}
		if entryMode(t, seal) != os.ModeNamedPipe {
			t.Fatal("FIFO seal replaced")
		}
		identityRestoreRefused(t, dir)
	})
	t.Run("advice", func(t *testing.T) {
		dir := t.TempDir()
		sealedMint(t, dir)
		seal := filepath.Join(dir, sealFile)
		if err := os.Remove(seal); err != nil {
			t.Fatal(err)
		}
		if err := syscall.Mkfifo(seal, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "player-initialized"), []byte(`{"version":0,"sealed":true}`), 0600); err != nil {
			t.Fatal(err)
		}
		rec := loggingtest.Record(t)
		if err := withinDeadline(t, "the preflight", func() error { return backup.CheckPlayerStart(dir) }); err != nil {
			t.Fatal(err)
		}
		if lines := rec.Lines(damagedMarkerWarning); len(lines) != 1 || len(rec.Lines(soleSealWarning)) != 0 {
			t.Fatalf("advice beside a FIFO seal: move-aside %v, sole-seal %v", lines, rec.Lines(soleSealWarning))
		}
	})
}

// TestSealCompletionAdviceNamesTheReadItLacked: a seal file this user cannot
// read, in a data directory it can write, gets the read advice naming the
// file, never write advice that changes nothing, and claims the recurrence
// only for this user: a CLI backup need not run as serve does.
func TestSealCompletionAdviceNamesTheReadItLacked(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a file whatever its mode")
	}
	reservationStorage(t)
	dir := loneSealBox(t)
	seal := filepath.Join(dir, sealFile)
	if err := os.Chmod(seal, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(seal, 0600) })
	rec := loggingtest.Record(t)
	if _, err := backup.Snapshot(t.Context(), backup.Sources{DataDir: dir, PlayerUUID: filepath.Join(dir, config.PlayerUUIDFile)}); err != nil {
		t.Fatalf("a failed completion stopped the backup: %v", err)
	}
	lines := rec.Lines("player seal completion failed")
	if len(lines) != 1 || !strings.Contains(lines[0], "restore read access to "+seal) || strings.Contains(lines[0], "write access") ||
		!strings.Contains(lines[0], "serve boot and start fail on it too when run as this user") {
		t.Fatalf("unreadable seal's advice: %v", lines)
	}
}

const serverLockHolderEnv = "B524_TEST_HOLD_SERVER_LOCK"

// TestBusySealCompletionAcrossProcesses: serve holds server.lock in another
// process, as it does in production. The backup's completion is busy, logged
// as deferred, and the next lifecycle operation completes it.
func TestBusySealCompletionAcrossProcesses(t *testing.T) {
	if dir := os.Getenv(serverLockHolderEnv); dir != "" {
		holdServerLockUntilStdinCloses(t, dir)
		return
	}
	reservationStorage(t)
	dir := loneSealBox(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestBusySealCompletionAcrossProcesses$")
	cmd.Env = append(os.Environ(), serverLockHolderEnv+"="+dir)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if _, err := cmd.StdinPipe(); err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	stopped := false
	stop := func() {
		if !stopped {
			stopped = true
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}
	t.Cleanup(stop)
	ready := false
	for sc := bufio.NewScanner(out); sc.Scan(); {
		if sc.Text() == "holding server.lock" {
			ready = true
			break
		}
	}
	if !ready {
		t.Fatalf("the holder exited before it held server.lock: %s", stderr.String())
	}
	rec := loggingtest.Record(t)
	if _, err := backup.Snapshot(t.Context(), backup.Sources{DataDir: dir, PlayerUUID: filepath.Join(dir, config.PlayerUUIDFile)}); err != nil {
		t.Fatalf("busy completion stopped the backup: %v", err)
	}
	if lines := rec.Lines("player seal completion deferred"); len(lines) != 1 || !strings.Contains(lines[0], "the next lifecycle operation") {
		t.Fatalf("busy completion across processes: %v (failed: %v)", lines, rec.Lines("player seal completion failed"))
	}
	stop()
	if err := backup.AdoptPlayerState(dir); err != nil {
		t.Fatal(err)
	}
	if m := markerAt(t, dir); !m.Sealed || !m.Minted {
		t.Fatalf("the next lifecycle operation did not complete the seal: %+v", m)
	}
}

// holdServerLockUntilStdinCloses is the holder's side: it takes dir's
// server.lock as serve does, says so, and holds it until its stdin closes.
func holdServerLockUntilStdinCloses(t *testing.T, dir string) {
	f, err := fsutil.OpenLockFile(dir, "server.lock")
	if err != nil {
		t.Fatal(err)
	}
	if err := fsutil.TryLock(f); err != nil {
		t.Fatal(err)
	}
	os.Stdout.WriteString("holding server.lock\n")
	_, _ = bufio.NewReader(os.Stdin).ReadString(0)
}
