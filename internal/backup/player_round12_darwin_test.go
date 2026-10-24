//go:build darwin

package backup_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// TestAnImmutableLockFileNamesTheFlag: macOS lets a user set the immutable
// flag (uchg) on their own file, and opening it for writing is then EPERM,
// which read and write access do not undo: the advice names the flag too.
func TestAnImmutableLockFileNamesTheFlag(t *testing.T) {
	notAsRoot(t)
	reservationStorage(t)
	dir := loneSealBox(t)
	lock := filepath.Join(dir, "server.lock")
	if err := os.WriteFile(lock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := unix.Chflags(lock, unix.UF_IMMUTABLE); err != nil {
		t.Fatalf("set the immutable flag: %v", err)
	}
	t.Cleanup(func() { _ = unix.Chflags(lock, 0) })
	want := "restore read and write access to " + lock + ", or clear an immutable flag or ACL on it: serve boot and start fail on it too when run as this user"
	if line := completionWarning(t, dir, true); !strings.Contains(line, want) {
		t.Fatalf("advice for an immutable lock file:\n%s\nwant:\n%s", line, want)
	}
}
