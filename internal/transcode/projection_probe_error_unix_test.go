//go:build !windows

package transcode

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// underAFile is a directory path whose parent is a regular file: its stat
// fails with ENOTDIR, which is not "not there", so neither probe walks past
// it to an ancestor (NearestExistingDir) and each fails on it.
func underAFile(t *testing.T, root string) string {
	t.Helper()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "a file")
	if err := os.WriteFile(file, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(file, "variants")
}

// TestAVolumeComparisonThatCannotStatADirectoryNamesNeither: SameVolume
// answers a directory it cannot stat, on either side, with the stat's
// operation and errno and neither directory, and the error still matches
// the errno and unwraps to the *fs.PathError os.Stat gave. So does the
// free-space probe of the same directory.
func TestAVolumeComparisonThatCannotStatADirectoryNamesNeither(t *testing.T) {
	root := filepath.Join(t.TempDir(), "Private Volume")
	blocked := underAFile(t, root)
	ok := t.TempDir()
	for _, tc := range []struct {
		name string
		a, b string
	}{
		{"the first directory", blocked, ok},
		{"the second directory", ok, blocked},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := SameVolume(tc.a, tc.b)
			requireNoDirectory(t, "SameVolume", err, root, "stat")
			if !errors.Is(err, syscall.ENOTDIR) {
				t.Errorf("SameVolume = %v, want it to match ENOTDIR", err)
			}
			var pe *fs.PathError
			if !errors.As(err, &pe) {
				t.Errorf("SameVolume = %v, want it to unwrap to os.Stat's *fs.PathError", err)
			}
			if strings.Contains(err.Error(), ok) {
				t.Errorf("SameVolume: %q names the directory it could read", err)
			}
		})
	}
	_, err := AvailableDiskSpaceNearest(blocked, ProbeRoleVariants)
	requireNoDirectory(t, "AvailableDiskSpaceNearest", err, root, "statfs")
	if !errors.Is(err, syscall.ENOTDIR) {
		t.Errorf("AvailableDiskSpaceNearest = %v, want it to match ENOTDIR", err)
	}
}
