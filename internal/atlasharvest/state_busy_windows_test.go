package atlasharvest

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"golang.org/x/sys/windows"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
)

// TestTheStateFileIsReadPastAHoldThatSharesNothing: a handle on the harvest
// state that shares nothing, as a scanner's can, refuses the open's read with
// ERROR_SHARING_VIOLATION (backlog B898). Let go from the read once it has
// heard that refusal, never by time, the open reads the file at its next read;
// held throughout, it answers busy after the schedule's five reads, each
// refused by Windows itself.
func TestTheStateFileIsReadPastAHoldThatSharesNothing(t *testing.T) {
	for _, letGo := range []bool{true, false} {
		t.Run(map[bool]string{true: "let go after a refusal", false: "held throughout"}[letGo], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "atlas-harvest.json")
			if err := os.WriteFile(path, []byte(`{"resultCursor":42}`), 0o600); err != nil {
				t.Fatal(err)
			}
			release := holdSharingNothing(t, path)
			if _, err := os.ReadFile(path); !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
				t.Fatalf("premise: the held file's read answered %v, want ERROR_SHARING_VIOLATION", err)
			}
			reads, refused, prev := 0, 0, readStateFile
			readStateFile = func(p string) ([]byte, error) {
				reads++
				data, err := os.ReadFile(p)
				if errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
					refused++
					if letGo {
						release()
					}
				}
				return data, err
			}
			t.Cleanup(func() { readStateFile = prev })
			s, err := OpenStateStore(path)
			if letGo && (err != nil || reads != 2 || refused != 1 || s.Snapshot().ResultCursor != 42) ||
				!letGo && (!errors.Is(err, atomicwrite.ErrReadBusy) || !errors.Is(err, windows.ERROR_SHARING_VIOLATION) || reads != 5 || refused != 5) {
				t.Fatalf("the open answered %v after %d reads, %d refused", err, reads, refused)
			}
		})
	}
}

// holdSharingNothing opens path sharing nothing, as a scanner's handle can,
// and answers its release, which the test's end makes too. The open is asked
// again as the typed read is: a scanner may hold a file just written.
func holdSharingNothing(t *testing.T, path string) (release func()) {
	t.Helper()
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	var h windows.Handle
	if err := atomicwrite.ReadWithRetryCtx(t.Context(), func() (err error) {
		h, err = windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	release = sync.OnceFunc(func() { _ = windows.CloseHandle(h) })
	t.Cleanup(release)
	return release
}
