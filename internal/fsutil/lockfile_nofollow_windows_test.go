//go:build windows

package fsutil

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestALockFileThatIsADirectoryOrAJunctionIsNotAFile: on Windows a
// directory, or a junction to one, at a lock file's name opens as itself and
// is refused as no file (ErrLockFileNotAFile), for writing and for reading,
// with nothing made through the junction. Without FILE_FLAG_BACKUP_SEMANTICS
// CreateFile answered ERROR_ACCESS_DENIED for both, which reads as a lock
// file this user may not write (OpenRefusedForWriting), never as one that is
// no file (B653 PR 2a's second round).
func TestALockFileThatIsADirectoryOrAJunctionIsNotAFile(t *testing.T) {
	for _, kind := range []string{"a directory", "a junction to a directory"} {
		for _, write := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s, opened for writing %v", kind, write), func(t *testing.T) {
				dir := t.TempDir()
				const name = "bridge.db-open.lock"
				target := filepath.Join(t.TempDir(), "target")
				if err := os.Mkdir(target, 0o755); err != nil {
					t.Fatal(err)
				}
				if kind == "a directory" {
					if err := os.Mkdir(filepath.Join(dir, name), 0o755); err != nil {
						t.Fatal(err)
					}
				} else {
					makeJunction(t, target, filepath.Join(dir, name))
				}
				f, err := OpenLockFileNoFollow(dir, name, filepath.Join(dir, "bridge.db"), 0o644, write)
				if f != nil {
					_ = f.Close()
				}
				if !errors.Is(err, ErrLockFileNotAFile) {
					t.Fatalf("OpenLockFileNoFollow over %s = %v, want ErrLockFileNotAFile", kind, err)
				}
				if OpenRefusedForWriting(err) {
					t.Errorf("the refusal reads as a lock file this user may not write: %v", err)
				}
				if entries, err := os.ReadDir(target); err != nil || len(entries) != 0 {
					t.Errorf("the junction's target holds %v (%v), want nothing made through it", entries, err)
				}
			})
		}
	}
}
