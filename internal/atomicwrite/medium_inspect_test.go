package atomicwrite

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
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

// TestInspectMediumRefusesARootThatIsNotADirectory pins the inspection's
// half of B606: InspectMedium refuses, as the strict writer refuses it
// (checkPath), a root that is there and is not a directory, with an error
// matching ErrRootNotADirectory that names the root and what is there: a
// regular file, a link to one, and a link to nothing, itself or at the head
// of a chain of links, which it no longer answers as fs.ErrNotExist, a root
// not made yet. It took a regular file there for storage it inspects. A link
// to a directory is taken, as the writer takes one, and a root below a link
// to nothing is still not there, for bridge doctor's walk up its path to
// judge.
func TestInspectMediumRefusesARootThatIsNotADirectory(t *testing.T) {
	base := realTempDir(t)
	file := filepath.Join(base, "file")
	if err := os.WriteFile(file, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		root func(t *testing.T) string
		says string
	}{
		{"a regular file", func(t *testing.T) string { return file }, file + " is a regular file, not a directory"},
		{"a link to a regular file", func(t *testing.T) string {
			symlink(t, file, filepath.Join(base, "to-file"))
			return filepath.Join(base, "to-file")
		}, filepath.Join(base, "to-file") + " leads to " + file + ", a regular file, not a directory"},
		{"a link to nothing", func(t *testing.T) string {
			symlink(t, filepath.Join(base, "nothing"), filepath.Join(base, "to-nothing"))
			return filepath.Join(base, "to-nothing")
		}, filepath.Join(base, "to-nothing") + " is a symbolic link to nothing"},
		{"a chain of links to nothing", func(t *testing.T) string {
			symlink(t, filepath.Join(base, "nothing-either"), filepath.Join(base, "end"))
			symlink(t, filepath.Join(base, "end"), filepath.Join(base, "chain"))
			return filepath.Join(base, "chain")
		}, filepath.Join(base, "chain") + " is a symbolic link to nothing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := tc.root(t)
			_, err := InspectMedium(root)
			switch {
			case !errors.Is(err, ErrRootNotADirectory):
				t.Errorf("InspectMedium(%s) = %v, want ErrRootNotADirectory", root, err)
			case errors.Is(err, fs.ErrNotExist):
				t.Errorf("InspectMedium(%s) = %v, which reads as a root not made yet", root, err)
			case !strings.Contains(err.Error(), tc.says):
				t.Errorf("InspectMedium(%s) = %v, want it to say %q", root, err, tc.says)
			}
			if err := ProbeDurable(root, filepath.Join(root, "reserved.json")); err == nil {
				t.Errorf("the strict writer's probe takes the root %s that InspectMedium refuses", root)
			}
		})
	}
	t.Run("a link to a directory", func(t *testing.T) {
		symlink(t, base, filepath.Join(base, "to-dir"))
		if _, err := InspectMedium(filepath.Join(base, "to-dir")); err != nil {
			t.Errorf("InspectMedium of a link to a directory = %v, want it inspected", err)
		}
	})
	t.Run("a root below a link to nothing", func(t *testing.T) {
		symlink(t, filepath.Join(base, "gone"), filepath.Join(base, "above"))
		if _, err := InspectMedium(filepath.Join(base, "above", "data")); !errors.Is(err, fs.ErrNotExist) || errors.Is(err, ErrRootNotADirectory) {
			t.Errorf("InspectMedium below a link to nothing = %v, want fs.ErrNotExist", err)
		}
	})
}
