package backup_test

import (
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"

	"github.com/acoseac/1-bit-bridge/internal/backup"
)

// neitherAbsentNorPresent is an Lstat error that says neither that the
// entry is there nor that it is not: on Windows, a file another handle holds
// open without sharing it. syscall.ENOTDIR is ERROR_PATH_NOT_FOUND here,
// which reads as no entry.
var neitherAbsentNorPresent = windows.ERROR_SHARING_VIOLATION

// TestALockFileDeniedReadingNamesTheEntry: a deny-read entry in
// server.lock's ACL, which full control of the data directory does not
// undo, refuses the backup's open of it for reading and writing. The advice
// is the lock file's union, which names the entry itself.
func TestALockFileDeniedReadingNamesTheEntry(t *testing.T) {
	dir := t.TempDir()
	lock := filepath.Join(dir, "server.lock")
	if err := os.WriteFile(lock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	me, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("icacls", lock, "/deny", me.Username+":(R)").CombinedOutput(); err != nil {
		t.Fatalf("deny %s reading %s: %v\n%s", me.Username, lock, err, out)
	}
	t.Cleanup(func() { _ = exec.Command("icacls", lock, "/remove:d", me.Username).Run() })
	if f, err := os.Open(lock); err == nil {
		_ = f.Close()
		t.Fatalf("the deny-read entry did not take: %s opened for reading", lock)
	}
	requireAdvice(t, "a lock file this user is denied reading", backup.OpenSealLock(dir), entryAdvice(dir, "server.lock", "read and write"))
}
