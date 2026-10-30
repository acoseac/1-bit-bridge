package atomicwrite

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// TestInspectMediumIsTheWritersOwnVerdict pins the one verdict bridge doctor
// and the strict writer share (B603, the orchestrator's amendment 8):
// InspectMedium resolves a root as the writer resolves one and answers what
// the package's own medium check, mediumOf as the package initializes it,
// answers of it and of the ID of its mount, on whatever host the tests run
// on, Linux's /dev/shm, a tmpfs, among them where there is one. A root that
// is not there is fs.ErrNotExist, the doctor's "not there yet", and an empty
// one is refused, never taken for the working directory.
func TestInspectMediumIsTheWritersOwnVerdict(t *testing.T) {
	dirs := []string{realTempDir(t)}
	if info, err := os.Stat("/dev/shm"); err == nil && info.IsDir() {
		dirs = append(dirs, "/dev/shm")
	}
	for _, dir := range dirs {
		m, err := InspectMedium(dir)
		if err != nil {
			t.Fatalf("InspectMedium(%s): %v", dir, err)
		}
		resolved, err := filepath.EvalSymlinks(dir)
		if err != nil {
			t.Fatal(err)
		}
		id, ok, err := mountIDOf(resolved)
		switch {
		case err != nil:
			t.Fatalf("mountIDOf(%s): %v", resolved, err)
		case !ok:
			// Windows gives no mount identity, and neither asks the medium.
			if m != (Medium{}) {
				t.Errorf("InspectMedium(%s) = %+v where the platform gives no mount ID", dir, m)
			}
			continue
		}
		if want := initialMediumOf(resolved, id); fmt.Sprint(m.Err) != fmt.Sprint(want) {
			t.Errorf("InspectMedium(%s) = %v, and the writer's own check answers %v", dir, m.Err, want)
		}
	}
	if _, err := InspectMedium(filepath.Join(realTempDir(t), "missing")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("InspectMedium of a root that is not there = %v, want fs.ErrNotExist", err)
	}
	if _, err := InspectMedium(""); !errors.Is(err, ErrNotUnderRoot) {
		t.Errorf("InspectMedium of no root = %v, want it refused", err)
	}
}
