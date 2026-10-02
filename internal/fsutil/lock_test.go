package fsutil

import (
	"bufio"
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// lockHolderEnv makes this test binary, run again by
// TestALockDiesWithTheProcessThatHeldIt, the process that holds the lock:
// its value is the file to lock.
const lockHolderEnv = "FSUTIL_TEST_LOCK_HOLDER"

// lockHolderReady is the line the holder prints once it holds the lock.
const lockHolderReady = "lock held"

// TestTryLockRefusesASecondOpenOfTheFile takes the lock through one open of
// a file and requires a second open in this same process to be refused
// with ErrLocked until the first unlocks. The lock is the open file's, not
// the process's: fcntl's record locks, which every open in a process
// shares, would let the second through, and `bridge serve` runs in-process
// from the launcher menu and the tests.
func TestTryLockRefusesASecondOpenOfTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")
	first, second := openForLock(t, path), openForLock(t, path)
	if err := TryLock(first); err != nil {
		t.Fatalf("TryLock on a free file: %v", err)
	}
	if err := TryLock(second); !errors.Is(err, ErrLocked) {
		t.Fatalf("TryLock through a second open = %v, want ErrLocked", err)
	}
	if err := Unlock(first); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	if err := TryLock(second); err != nil {
		t.Fatalf("TryLock once the first open unlocked = %v, want nil", err)
	}
}

// TestALockIsReleasedWhenItsFileCloses requires a lock whose file is
// closed without an Unlock to be free again. On Windows the release a
// close makes may lag (LockFileEx's documentation), so the second open is
// given a moment.
func TestALockIsReleasedWhenItsFileCloses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")
	first := openForLock(t, path)
	if err := TryLock(first); err != nil {
		t.Fatalf("TryLock on a free file: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	requireLockFreed(t, openForLock(t, path), "its file closed")
}

// TestALockDiesWithTheProcessThatHeldIt runs this test binary again as a
// process that takes the lock and holds it, requires the lock to be
// refused while that process lives, kills it (SIGKILL on unix,
// TerminateProcess on Windows: an exit that runs nothing of its own), and
// requires the lock to be free again. That is what keeps the lock from
// going stale after a crash, the objection to a lock FILE: a supervisor's
// restart after a kill must find the data dir free.
func TestALockDiesWithTheProcessThatHeldIt(t *testing.T) {
	if path := os.Getenv(lockHolderEnv); path != "" {
		holdLockUntilStdinCloses(t, path)
		return
	}
	path := filepath.Join(t.TempDir(), "lock")
	cmd := exec.Command(os.Args[0], "-test.run=^TestALockDiesWithTheProcessThatHeldIt$")
	cmd.Env = append(os.Environ(), lockHolderEnv+"="+path)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	// The writer is kept by cmd and closed only by Wait: the holder holds
	// until it is killed, or until this binary dies and the pipe closes.
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
	killed := false
	t.Cleanup(func() {
		if !killed {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	ready := false
	for sc := bufio.NewScanner(out); sc.Scan(); {
		if sc.Text() == lockHolderReady {
			ready = true
			break
		}
	}
	if !ready {
		t.Fatalf("the holder exited before it held the lock: %s", stderr.String())
	}

	ours := openForLock(t, path)
	if err := TryLock(ours); !errors.Is(err, ErrLocked) {
		t.Fatalf("TryLock while another process holds the lock = %v, want ErrLocked", err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	killed = true
	requireLockFreed(t, ours, "the process that held it was killed")
}

// holdLockUntilStdinCloses is the holder's side: it locks path, says so,
// and waits for its stdin to close.
func holdLockUntilStdinCloses(t *testing.T, path string) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := TryLock(f); err != nil {
		t.Fatal(err)
	}
	os.Stdout.WriteString(lockHolderReady + "\n")
	_, _ = bufio.NewReader(os.Stdin).ReadString(0)
}

// openForLock opens path for TryLock, creating it, and closes it when the
// test ends.
func openForLock(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

// requireLockFreed requires TryLock on f to succeed within a few seconds,
// the lag Windows may take to release a lock whose holder went.
func requireLockFreed(t *testing.T, f *os.File, after string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := TryLock(f)
		if err == nil {
			return
		}
		if !errors.Is(err, ErrLocked) || time.Now().After(deadline) {
			t.Fatalf("TryLock after %s = %v, want nil", after, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
