//go:build !windows

package transcode

import (
	"os"
	"syscall"
	"testing"
)

// TestSameVolumeTellsAnotherVolumeApart: /dev is a filesystem of its own
// (devfs on macOS, devtmpfs or a container's tmpfs on Linux), so a
// directory there is on another volume than the test's temp directory, and
// the pre-flight checks the two needs apart.
func TestSameVolumeTellsAnotherVolumeApart(t *testing.T) {
	root := t.TempDir()
	if deviceOf(t, root) == deviceOf(t, "/dev") {
		t.Skip("this host's /dev is on the temp directory's volume")
	}
	same, err := SameVolume(root, "/dev")
	if err != nil || same {
		t.Errorf("SameVolume(%q, /dev) = %v, %v; want false", root, same, err)
	}
}

// deviceOf is the device os.Stat reports for path.
func deviceOf(t *testing.T, path string) uint64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("stat %q carries no device", path)
	}
	return uint64(st.Dev)
}
