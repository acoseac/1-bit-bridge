//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// ENOLCK is flock's answer where the kernel or the filesystem's lock manager
// has no lock to give: an NFS mount whose lock manager is not running, and on
// Linux any filesystem whose kernel cannot allocate a lock record. It does not
// say the filesystem keeps no locks, so a writer given it must not go on
// beside a writer that holds the lock. Until B506's third round the first
// ENOLCK took the turn without the lock.

// answerNoLock answers a try at the lock as flock does where there is no lock
// to give.
func answerNoLock(f *os.File) error {
	return &os.PathError{Op: "flock", Path: f.Name(), Err: unix.ENOLCK}
}

// TestAFilesystemThatGivesNoLockRefusesTheTurn: a writer whose every try at
// the lock answers ENOLCK until the wait is over, while another writer holds
// the lock, takes no turn: it is refused with a *NoLockManagerError that
// names the file and what to do, and nothing the file holds.
func TestAFilesystemThatGivesNoLockRefusesTheTurn(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := writeConfigFile(t, dir, "libraryName: Secret-Name-Not-For-Logs\n")
	holdLock(t, path)
	const wait = 300 * time.Millisecond
	tries := 0
	start := time.Now()
	l, err := lockFile(path, func(f *os.File) error { tries++; return answerNoLock(f) }, wait)
	took := time.Since(start)
	if err == nil {
		defer l.Unlock()
		t.Fatalf("a writer whose every try answered ENOLCK took the turn (unheld: %v), beside the writer that holds the lock", l.Unheld())
	}
	var refused *NoLockManagerError
	if !errors.As(err, &refused) || !errors.Is(err, ErrNoLockManager) || !errors.Is(err, unix.ENOLCK) ||
		refused.Path != path || refused.Wait != wait {
		t.Fatalf("refusal = %v, want a *NoLockManagerError for %s after %s that wraps ENOLCK", err, path, wait)
	}
	if errors.Is(err, ErrBusy) {
		t.Errorf("refusal %v reads as another writer holding the lock: no try found it held", err)
	}
	if took < wait || tries < 3 {
		t.Errorf("refused after %s and %d tries, want it to ask again until the wait of %s was over", took, tries, wait)
	}
	msg := err.Error()
	for _, want := range []string{path, "nothing was written", "lock manager", "local locking", "lock service"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal %q does not say %q", msg, want)
		}
	}
	if strings.Contains(msg, "Secret-Name") {
		t.Errorf("refusal %q carries a value of the file", msg)
	}
}

// TestNoLockToGiveThenAHeldLockIsBusy: a writer whose try answers ENOLCK and
// then finds the lock held, until the wait is over, is refused as busy: a try
// that found the lock held says another writer has the turn.
func TestNoLockToGiveThenAHeldLockIsBusy(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := writeConfigFile(t, dir, "libraryName: Before\n")
	holdLock(t, path)
	first := true
	l, err := lockFile(path, func(f *os.File) error {
		if first {
			first = false
			return answerNoLock(f)
		}
		return fsutil.TryLock(f)
	}, 300*time.Millisecond)
	if err == nil {
		defer l.Unlock()
		t.Fatalf("an ENOLCK and then a held lock took the turn (unheld: %v), beside the writer that holds the lock", l.Unheld())
	}
	if !errors.Is(err, ErrBusy) || errors.Is(err, ErrNoLockManager) {
		t.Errorf("refusal = %v, want a *BusyError: a try found the lock held", err)
	}
}

// TestNoLockToGiveOnceIsAskedAgain: an ENOLCK and then a free lock is a turn
// that holds the lock: the answer is asked again, as contention is, and never
// taken for a filesystem that keeps no locks.
func TestNoLockToGiveOnceIsAskedAgain(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := writeConfigFile(t, dir, "libraryName: Before\n")
	first := true
	l, err := lockFile(path, func(f *os.File) error {
		if first {
			first = false
			return answerNoLock(f)
		}
		return fsutil.TryLock(f)
	}, time.Second)
	if err != nil {
		t.Fatalf("an ENOLCK and then a free lock was refused: %v", err)
	}
	defer l.Unlock()
	if l.Unheld() != nil {
		t.Fatalf("one ENOLCK took the turn without the lock: %v", l.Unheld())
	}
	other, err := fsutil.OpenLockFile(filepath.Dir(path), LockFileName(path))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if err := fsutil.TryLock(other); !errors.Is(err, fsutil.ErrLocked) {
		t.Errorf("another open of the lock file while the turn is held = %v, want ErrLocked", err)
	}
}
