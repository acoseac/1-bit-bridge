package backup_test

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// userClaim is what every permission failure's advice ends with: the next
// serve boot and start meet it too, when they run as the backup's user.
const userClaim = ": serve boot and start fail on it too when run as this user"

// unixEntryRemedy is the union a permission failure of a step naming dir/name
// takes on unix: the access the step needed on the entry, when it names one,
// the data directory's remedy, and everything on the entry and what it links
// to that can refuse the step. path is dir/name as the platform spells it.
func unixEntryRemedy(dir, path, access string) string {
	s := "restore read, write and search (execute) access to the data directory " + dir +
		" and each directory above it, or clear an immutable or append-only flag or ACL on it; and clear any immutable or append-only flag or ACL on " +
		path + " and on what it links to, or replace a link at " + path + " with a regular file; and either own " + path +
		" or clear the sticky bit on " + dir
	if access != "" {
		s = "restore " + access + " access to " + path + "; " + s
	}
	return s
}

// windowsEntryRemedy is the same union on Windows, where full control of the
// entry covers its access.
func windowsEntryRemedy(dir, path string) string {
	return "restore full control of the data directory " + dir + " and of " + path +
		", and clear its read-only attribute, or replace a link at " + path +
		" with a regular file; and close any program holding " + path + " open"
}

// entryAdvice is the advice a permission failure of a step naming dir/name
// ends its warning with on this platform.
func entryAdvice(dir, name, access string) string {
	path := filepath.Join(dir, name)
	if runtime.GOOS == "windows" {
		return windowsEntryRemedy(dir, path) + userClaim
	}
	return unixEntryRemedy(dir, path, access) + userClaim
}

// requireAdvice requires err to be a permission failure whose warning ends
// with want.
func requireAdvice(t *testing.T, what string, err error, want string) {
	t.Helper()
	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("%s: not a permission failure: %v", what, err)
	}
	if advice := backup.SealCompletionAdvice(err); !strings.HasSuffix(advice, want) {
		t.Fatalf("%s's advice:\n%s\nwant it to end:\n%s", what, advice, want)
	}
}

// playerFiles is dir's player files with their bytes: the state a refused
// adoption must leave as it was.
func playerFiles(t *testing.T, dir string) map[string]string {
	t.Helper()
	state := map[string]string{}
	for _, name := range []string{config.PlayerUUIDFile, "player-ids.json", "player-initialized", "player-queue.json", sealFile} {
		if b, err := os.ReadFile(filepath.Join(dir, name)); err == nil {
			state[name] = string(b)
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
	return state
}

// TestOnlyAHeldLockMakesAdoptionBusy: a backup of a box whose player holds
// only a UUID adopts it under server.lock. Only a lock another open holds
// (fsutil.ErrLocked) makes the adoption busy, with its advice to stop the
// service. An interrupted or refused lock, or an I/O error, keeps its own
// text, never says busy and leaves the player's state as it was; a
// permission error carries the lock file's remedy.
func TestOnlyAHeldLockMakesAdoptionBusy(t *testing.T) {
	reservationStorage(t)
	for _, tc := range []struct {
		name string
		err  error
		is   error
	}{
		{"enolck", &os.PathError{Op: "flock", Path: "server.lock", Err: syscall.ENOLCK}, syscall.ENOLCK},
		{"eio", &os.PathError{Op: "flock", Path: "server.lock", Err: syscall.EIO}, syscall.EIO},
		{"eacces", &os.PathError{Op: "flock", Path: "server.lock", Err: syscall.EACCES}, syscall.EACCES},
		{"held", fmt.Errorf("flock server.lock: %w", fsutil.ErrLocked), fsutil.ErrLocked},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := uuidOnlyBox(t)
			before := playerFiles(t, dir)
			calls := 0
			t.Cleanup(backup.SetTryLockForTest(func(*os.File) error { calls++; return tc.err }))
			snap, err := backup.Snapshot(t.Context(), backup.Sources{DataDir: dir, PlayerUUID: filepath.Join(dir, config.PlayerUUIDFile)})
			if calls != 1 {
				t.Fatalf("the adoption's lock seam ran %d times", calls)
			}
			if err == nil || snap != "" || !errors.Is(err, tc.is) || !strings.Contains(err.Error(), tc.err.Error()) {
				t.Fatalf("adoption over a refused lock: %q %v", snap, err)
			}
			busy := strings.Contains(err.Error(), "busy")
			switch {
			case tc.name == "held" && (!busy || !strings.Contains(err.Error(), "stop the service and run the backup again")):
				t.Fatalf("a held lock is not busy: %v", err)
			case tc.name != "held" && busy:
				t.Fatalf("a %s claims the lock is busy: %v", tc.name, err)
			case tc.name == "eacces" && !strings.Contains(err.Error(), entryAdvice(dir, "server.lock", "read and write")):
				t.Fatalf("a refused lock's advice:\n%v\nwant it to end:\n%s", err, entryAdvice(dir, "server.lock", "read and write"))
			}
			if after := playerFiles(t, dir); fmt.Sprint(after) != fmt.Sprint(before) {
				t.Fatalf("the refused adoption changed the player's state: %v -> %v", before, after)
			}
		})
	}
}

// TestTheEntryRemedyPerPlatform pins the union a permission failure of a step
// naming an entry takes, by the text it prints on each platform, and through
// the tags the lock file's open and a marker's durable write put on a
// failure: on unix the step's access to the entry, the directory's remedy,
// any flag or ACL on the entry and what it links to or a link replaced, and a
// sticky directory's owner; on Windows full control of both, the read-only
// attribute, a link replaced and a program holding the entry closed.
func TestTheEntryRemedyPerPlatform(t *testing.T) {
	const unixDir, windowsDir = "/srv/bridge", `C:\bridge`
	lockUnix := unixEntryRemedy(unixDir, unixDir+"/server.lock", "read and write")
	markerUnix := unixEntryRemedy(unixDir, unixDir+"/player-initialized", "")
	lockWindows := windowsEntryRemedy(windowsDir, windowsDir+`\server.lock`)
	markerWindows := windowsEntryRemedy(windowsDir, windowsDir+`\player-initialized`)
	for _, tc := range []struct{ goos, dir, name, access, want string }{
		{"linux", unixDir, "server.lock", "read and write", lockUnix},
		{"darwin", unixDir, "player-initialized", "", markerUnix},
		{"windows", windowsDir, "server.lock", "read and write", lockWindows},
		{"windows", windowsDir, "player-initialized", "", markerWindows},
	} {
		if got := backup.EntryRemedy(tc.goos, tc.dir, tc.name, tc.access); got != tc.want {
			t.Errorf("%s %s:\n%s\nwant:\n%s", tc.goos, tc.name, got, tc.want)
		}
	}
	refused := &os.PathError{Op: "open", Path: "server.lock", Err: syscall.EACCES}
	for _, tc := range []struct {
		goos, dir string
		err       error
		want      string
	}{
		{"linux", unixDir, backup.LockFileFailure(unixDir, refused), lockUnix},
		{"linux", unixDir, backup.MarkerWriteFailure(unixDir, "player-initialized", refused), markerUnix},
		{"windows", windowsDir, backup.LockFileFailure(windowsDir, refused), lockWindows},
		{"windows", windowsDir, backup.MarkerWriteFailure(windowsDir, "player-initialized", refused), markerWindows},
	} {
		advice := backup.SealCompletionAdviceOn(tc.goos, tc.err)
		if !strings.HasSuffix(advice, "; "+tc.want+userClaim) || !strings.Contains(advice, refused.Error()) {
			t.Errorf("%s advice through the tag:\n%s\nwant it to end:\n%s", tc.goos, advice, tc.want+userClaim)
		}
	}
}
