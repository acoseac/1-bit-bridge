package backup_test

import (
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/acoseac/1-bit-bridge/internal/backup"
)

// neitherAbsentNorPresent is an Lstat error that says neither that the
// entry is there nor that it is not: on Windows, a file another handle holds
// open without sharing it. syscall.ENOTDIR is ERROR_PATH_NOT_FOUND here,
// which reads as no entry.
var neitherAbsentNorPresent = windows.ERROR_SHARING_VIOLATION

// withoutBackupPrivileges disables SeBackupPrivilege and SeRestorePrivilege
// in this process's token until the test ends. An administrator's token can
// hold both enabled (nomos's SSH sessions do), and Go opens a file with
// FILE_FLAG_BACKUP_SEMANTICS, which with them enabled reads and writes
// through any entry in its ACL.
func withoutBackupPrivileges(t *testing.T) {
	t.Helper()
	var token windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_ADJUST_PRIVILEGES|windows.TOKEN_QUERY, &token); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = token.Close() })
	for _, name := range []string{"SeBackupPrivilege", "SeRestorePrivilege"} {
		var luid windows.LUID
		if err := windows.LookupPrivilegeValue(nil, windows.StringToUTF16Ptr(name), &luid); err != nil {
			t.Fatal(err)
		}
		off := windows.Tokenprivileges{PrivilegeCount: 1, Privileges: [1]windows.LUIDAndAttributes{{Luid: luid}}}
		var was windows.Tokenprivileges
		var size uint32
		if err := windows.AdjustTokenPrivileges(token, false, &off, uint32(unsafe.Sizeof(was)), &was, &size); err != nil {
			t.Fatalf("disable %s: %v", name, err)
		}
		t.Cleanup(func() {
			if was.PrivilegeCount > 0 {
				_ = windows.AdjustTokenPrivileges(token, false, &was, 0, nil, nil)
			}
		})
	}
}

// TestALockFileDeniedReadingNamesTheEntry: a deny-read entry in
// server.lock's ACL, which full control of the data directory does not
// undo, refuses the backup's open of it for reading and writing. The advice
// is the lock file's union, which names the entry itself.
func TestALockFileDeniedReadingNamesTheEntry(t *testing.T) {
	withoutBackupPrivileges(t)
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
