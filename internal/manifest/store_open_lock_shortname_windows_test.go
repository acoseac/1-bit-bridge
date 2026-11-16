//go:build windows

package manifest

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// shortNameOf is the 8.3 short name Windows gives path, "" where the volume
// gives it none. GetShortPathName answers the length it wrote, without the
// terminating NUL, where the buffer held the name, and the length the name
// needs, with it, where the buffer was too small: an answer as long as the
// buffer or longer is a name it did not write whole.
func shortNameOf(t *testing.T, path string) string {
	t.Helper()
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n, err := windows.GetShortPathName(p, &buf[0], uint32(len(buf)))
	if err != nil || n == 0 || int(n) >= len(buf) {
		return ""
	}
	if s := windows.UTF16ToString(buf[:n]); s != path {
		return s
	}
	return ""
}

// TestAShortNameOfTheDataDirectoryIsTheSameHold: a data directory reached by
// its 8.3 short name is the hold its long name took, one entry, one
// descriptor. Go's filepath.EvalSymlinks names each component as the
// directory lists it (FindFirstFile), so the two keys met before B653 PR
// 2a's second round too: this keeps them one, whatever the registry's lookup
// becomes.
func TestAShortNameOfTheDataDirectoryIsTheSameHold(t *testing.T) {
	long := filepath.Join(t.TempDir(), "a data directory with a long name")
	if err := os.MkdirAll(long, 0o755); err != nil {
		t.Fatal(err)
	}
	short := shortNameOf(t, long)
	if short == "" {
		t.Skip("the volume gives this directory no 8.3 short name: 8.3 names are off on it")
	}
	first := filepath.Join(long, "bridge.db")
	openAndClose(t, first)
	requireOneHoldAcrossSpellings(t, first, filepath.Join(short, "bridge.db"))
}
