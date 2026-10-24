//go:build unix

package backup_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// notAsRoot skips a permission case under root, whom no mode refuses.
func notAsRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root is refused by no mode")
	}
}

// chmodFor gives path mode until the test ends, then restore.
func chmodFor(t *testing.T, path string, mode, restore os.FileMode) {
	t.Helper()
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, restore) })
}

// directoryAdvice is the remedy, and the claim, a failure at the data
// directory itself ends its warning with: one remedy that always suffices.
func directoryAdvice(dir string) string {
	return "restore read, write and search (execute) access to the data directory " + dir +
		" and each directory above it, or clear an immutable flag or ACL on it: serve boot and start fail on it too when run as this user"
}

// completionWarning backs dir up and returns the one seal-completion failure
// the backup logged. backedUp says whether the backup itself must succeed: a
// backup that cannot search dir cannot, and only its warning is the case's.
func completionWarning(t *testing.T, dir string, backedUp bool) string {
	t.Helper()
	rec := loggingtest.Record(t)
	_, err := backup.Snapshot(t.Context(), backup.Sources{DataDir: dir, PlayerUUID: filepath.Join(dir, config.PlayerUUIDFile)})
	if backedUp && err != nil {
		t.Fatalf("the backup failed: %v", err)
	}
	lines := rec.Lines("player seal completion failed")
	if len(lines) != 1 {
		t.Fatalf("seal completion failures logged: %v (backup: %v)", lines, err)
	}
	return lines[0]
}

// backupsDir precreates dir's writable backups directory, for a fixture the
// backup can search but not write.
func backupsDir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, backup.BackupsDirName), 0o700); err != nil {
		t.Fatal(err)
	}
}

// requireAdvice requires err to be a permission failure whose warning ends
// with want.
func requireAdvice(t *testing.T, what string, err error, want string) {
	t.Helper()
	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("%s: not a permission failure: %v", what, err)
	}
	if advice := backup.SealCompletionAdvice(err); !strings.Contains(advice, want) {
		t.Fatalf("%s's advice:\n%s\nwant it to say:\n%s", what, advice, want)
	}
}

// TestALockFileThisUserCannotReadNamesReadAndWriteAccess: server.lock is
// opened for reading and writing, so a lock file this user may write but not
// read (0200) names both, never write access it already has.
func TestALockFileThisUserCannotReadNamesReadAndWriteAccess(t *testing.T) {
	notAsRoot(t)
	reservationStorage(t)
	dir := loneSealBox(t)
	lock := filepath.Join(dir, "server.lock")
	if err := os.WriteFile(lock, nil, 0o200); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(lock, 0o600) })
	want := "restore read and write access to " + lock + ": serve boot and start fail on it too when run as this user"
	if line := completionWarning(t, dir, true); !strings.Contains(line, want) {
		t.Fatalf("advice for an unreadable lock file:\n%s\nwant:\n%s", line, want)
	}
}

// TestALockFileTheOpenCouldNotCreateNamesTheDirectory: with no server.lock,
// or one that is a link the data directory resolves to nothing, the open
// failed creating the file: the data directory's access, never the file's.
func TestALockFileTheOpenCouldNotCreateNamesTheDirectory(t *testing.T) {
	notAsRoot(t)
	reservationStorage(t)
	for _, lock := range []string{"absent", "dangling_link"} {
		t.Run(lock, func(t *testing.T) {
			dir := loneSealBox(t)
			if lock == "dangling_link" {
				if err := os.Symlink("server.lock.missing", filepath.Join(dir, "server.lock")); err != nil {
					t.Fatal(err)
				}
			}
			backupsDir(t, dir)
			chmodFor(t, dir, 0o500, 0o700)
			if line := completionWarning(t, dir, true); !strings.Contains(line, directoryAdvice(dir)) {
				t.Fatalf("advice when server.lock could not be created:\n%s\nwant:\n%s", line, directoryAdvice(dir))
			}
		})
	}
}

// TestAnUnreadableDataDirectoryFailsAtTheSealBarrier is the reviewed
// sequence: a data directory this user may write and search but not read
// (0300), a 0600 server.lock, a seal that is a directory and no
// player-initialized. Completion keeps the seal and fails at the barrier,
// which opens the directory to sync it: the directory remedy, before any
// marker is written. A backup's own lock open needs to read the directory
// too, fails there first, and gives the same remedy.
func TestAnUnreadableDataDirectoryFailsAtTheSealBarrier(t *testing.T) {
	notAsRoot(t)
	reservationStorage(t)
	dir := loneSealBox(t)
	replaceSealWith(t, dir, "directory")
	if err := os.WriteFile(filepath.Join(dir, "server.lock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	backupsDir(t, dir)
	chmodFor(t, dir, 0o300, 0o700)
	err := backup.CompletePlayerSeal(dir)
	requireAdvice(t, "the seal barrier", err, directoryAdvice(dir))
	if _, err := os.Lstat(filepath.Join(dir, "player-initialized")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("completion went past the barrier: %v", err)
	}
	requireAdvice(t, "the backup's lock open", backup.OpenSealLock(dir), directoryAdvice(dir))
	if line := completionWarning(t, dir, false); !strings.Contains(line, directoryAdvice(dir)) {
		t.Fatalf("the backup's advice:\n%s\nwant:\n%s", line, directoryAdvice(dir))
	}
}

// TestAnUnreadableDataDirectoryFailsAtTheMarkerWrite: a sealed marker not
// yet minted, beside player-uuid and no seal file, in a 0300 data directory.
// Completion writes the minted marker, which lands, and then cannot sync the
// directory: the directory remedy, at the marker write.
func TestAnUnreadableDataDirectoryFailsAtTheMarkerWrite(t *testing.T) {
	notAsRoot(t)
	reservationStorage(t)
	dir := t.TempDir()
	sealedMint(t, dir)
	if err := os.Remove(filepath.Join(dir, sealFile)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "player-initialized"), []byte(`{"version":1,"sealed":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	chmodFor(t, dir, 0o300, 0o700)
	err := backup.CompletePlayerSeal(dir)
	requireAdvice(t, "the marker write", err, directoryAdvice(dir))
	if m := markerAt(t, dir); !m.Sealed || !m.Minted {
		t.Fatalf("completion failed before the marker write: %+v", m)
	}
}

// TestADataDirectoryThisUserCannotSearchFailsAtTheFirstLstat: no search
// (0600) fails the completion's first Lstat, the seal's. The backup cannot go
// on there either; its completion warning gives the directory remedy.
func TestADataDirectoryThisUserCannotSearchFailsAtTheFirstLstat(t *testing.T) {
	notAsRoot(t)
	reservationStorage(t)
	dir := loneSealBox(t)
	chmodFor(t, dir, 0o600, 0o700)
	line := completionWarning(t, dir, false)
	if !strings.Contains(line, "lstat "+filepath.Join(dir, sealFile)) || !strings.Contains(line, directoryAdvice(dir)) {
		t.Fatalf("advice at the first Lstat:\n%s\nwant:\n%s", line, directoryAdvice(dir))
	}
}

// TestEveryDirectoryStepNamesTheDirectory drives each step that searches,
// syncs or writes the data directory, where no backup's earlier step would
// fail first: the Lstats of the seal's entry check, the allocation evidence,
// a marker's ensure and the lock file's, and a marker's durable write. Each
// gives the one directory remedy.
func TestEveryDirectoryStepNamesTheDirectory(t *testing.T) {
	notAsRoot(t)
	reservationStorage(t)
	steps := []struct {
		name string
		mode os.FileMode
		run  func(dir string) error
	}{
		{"seal_entry_check", 0o600, func(dir string) error { _, err := backup.PlayerEntryNotAFile(dir, sealFile); return err }},
		{"allocation_evidence", 0o600, func(dir string) error { _, err := backup.PlayerAllocationEvidence(dir); return err }},
		{"marker_ensure_lstat", 0o600, func(dir string) error { return backup.EnsurePlayerMarker(dir, sealFile) }},
		{"lock_file_lstat", 0o600, func(dir string) error { return backup.OpenSealLock(dir) }},
		{"marker_ensure_write", 0o500, func(dir string) error { return backup.EnsurePlayerMarker(dir, "player-initialized") }},
	}
	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			dir := t.TempDir()
			chmodFor(t, dir, step.mode, 0o700)
			requireAdvice(t, step.name, step.run(dir), directoryAdvice(dir))
		})
	}
}

// TestAMarkerOpenRefusedAsNotPermittedNamesTheFlag: an EPERM opening a marker,
// rather than an EACCES, is a flag or an ACL that read access does not undo,
// and the advice says so beside the file it names.
func TestAMarkerOpenRefusedAsNotPermittedNamesTheFlag(t *testing.T) {
	notAsRoot(t)
	reservationStorage(t)
	dir := loneSealBox(t)
	seal := filepath.Join(dir, sealFile)
	t.Cleanup(backup.SetReadMarkerForTest(func(path string) ([]byte, error) {
		if path == seal {
			return nil, &os.PathError{Op: "open", Path: path, Err: syscall.EPERM}
		}
		return backup.ReadMarkerBytes(path)
	}))
	want := "restore read access to " + seal + ", or clear an immutable flag or ACL on it: serve boot and start fail on it too when run as this user"
	if line := completionWarning(t, dir, true); !strings.Contains(line, want) {
		t.Fatalf("advice for a marker open refused as not permitted:\n%s\nwant:\n%s", line, want)
	}
}

// TestAnyOtherLockEntryKeepsItsAdvice: a lock file that is neither a regular
// file nor a link to nothing, here a named pipe this user may not open,
// keeps the lock file's write advice.
func TestAnyOtherLockEntryKeepsItsAdvice(t *testing.T) {
	notAsRoot(t)
	reservationStorage(t)
	dir := loneSealBox(t)
	lock := filepath.Join(dir, "server.lock")
	if err := syscall.Mkfifo(lock, 0); err != nil {
		t.Fatal(err)
	}
	want := "restore write access to " + lock + ": serve boot and start fail on it too when run as this user"
	if line := completionWarning(t, dir, true); !strings.Contains(line, want) {
		t.Fatalf("advice for a named pipe at server.lock:\n%s\nwant:\n%s", line, want)
	}
}
