//go:build windows

package updater

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// inUseFailure is what a rename answers while another process holds its
// file: Windows' own ERROR_SHARING_VIOLATION, which the rollback takes for
// a file in use through the platform's inUseByAnotherProcess, untouched.
func inUseFailure(*testing.T) error { return windows.ERROR_SHARING_VIOLATION }

// TestOnlyWindowsSharingAndLockViolationsAreAFileInUse: the rollback makes
// a rename again for a sharing or a lock violation alone, wrapped as
// os.Rename wraps it; a mapped image's refusal or a permission (Access is
// denied, backlog B202), and every other failure, it does not.
func TestOnlyWindowsSharingAndLockViolationsAreAFileInUse(t *testing.T) {
	for _, tc := range []struct {
		errno windows.Errno
		want  bool
	}{
		{windows.ERROR_SHARING_VIOLATION, true},
		{windows.ERROR_LOCK_VIOLATION, true},
		{windows.ERROR_ACCESS_DENIED, false},
		{windows.ERROR_FILE_NOT_FOUND, false},
		{windows.ERROR_PATH_NOT_FOUND, false},
		{windows.ERROR_NOT_SAME_DEVICE, false},
	} {
		err := &os.LinkError{Op: "rename", Old: "a", New: "b", Err: tc.errno}
		if got := inUseByAnotherProcess(err); got != tc.want {
			t.Errorf("inUseByAnotherProcess(%v) = %v, want %v", err, got, tc.want)
		}
	}
}

// holdWithoutSharingItsDeletion opens path as a scanner may, sharing its
// reading and not its deletion, so that Windows refuses a rename of it
// with a sharing violation until the handle is let go; let go also when
// the test ends.
func holdWithoutSharingItsDeletion(t *testing.T, path string) (letGo func()) {
	t.Helper()
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ, windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatalf("hold %s: %v", path, err)
	}
	closed := false
	letGo = func() {
		if !closed {
			closed = true
			_ = windows.CloseHandle(h)
		}
	}
	t.Cleanup(letGo)
	return letGo
}

// TestARollbackPutsBackABackupAnotherHandleHeldOnceItIsLetGo is backlog
// B872's refusal on Windows itself: another handle holds the backup open
// without sharing its deletion, as Defender holds an executable it scans,
// so the backup's rename meets a real sharing violation. Let go at the
// rollback's first wait, after that refusal and before its next attempt,
// the backup is put back; held through every attempt, the running binary
// is put back from the aside, "(restored)", and the backup stays.
func TestARollbackPutsBackABackupAnotherHandleHeldOnceItIsLetGo(t *testing.T) {
	for _, tc := range []struct {
		name  string
		letGo bool
	}{
		{"let go after the first refusal", true},
		{"held through every attempt", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			dst := filepath.Join(dir, "bridge.exe")
			bak := dst + ".bak"
			if err := os.WriteFile(dst, []byte("RUNNING"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(bak, []byte("KNOWN-GOOD"), 0o755); err != nil {
				t.Fatal(err)
			}
			letGo := holdWithoutSharingItsDeletion(t, bak)
			if err := os.Rename(bak, filepath.Join(dir, "elsewhere")); !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
				t.Fatalf("a rename of the held backup = %v, want a sharing violation: the test shows nothing", err)
			}
			var waits []time.Duration
			prev := rollbackWait
			t.Cleanup(func() { rollbackWait = prev })
			rollbackWait = func(d time.Duration) {
				waits = append(waits, d)
				if tc.letGo {
					letGo()
				}
			}

			err := replaceRunningBinary(dst, bak)
			letGo()
			want, wantBak := "KNOWN-GOOD", false
			// A scanner of the host's may hold a file a moment more, which
			// the rollback waits out too: the waits are at least the test's.
			if tc.letGo {
				if err != nil {
					t.Fatalf("the rollback = %v, want the backup put back once its holder let it go", err)
				}
				if len(waits) < 1 || waits[0] != 50*time.Millisecond {
					t.Errorf("the rollback waited %v, want 50ms first, for the refusal of the held backup", waits)
				}
			} else {
				want, wantBak = "RUNNING", true
				if err == nil || !strings.HasSuffix(err.Error(), "(restored)") || !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
					t.Fatalf("the rollback = %v, want the sharing violation and the binary restored", err)
				}
				if len(waits) < len(rollbackRenameBackoff)-1 {
					t.Errorf("the rollback waited %v, want the backup's rename's whole schedule", waits)
				}
			}
			if got, err := os.ReadFile(dst); err != nil || string(got) != want {
				t.Errorf("the binary's place holds %q (%v), want %q", got, err, want)
			}
			if _, err := os.Stat(bak); (err == nil) != wantBak {
				t.Errorf("the backup's place: %v, want the backup there %v", err, wantBak)
			}
			if asides := asidesOf(t, dst); len(asides) != 0 {
				t.Errorf("asides left beside the binary: %v", asides)
			}
		})
	}
}
