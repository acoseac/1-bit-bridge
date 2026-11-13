package sweeptest

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
)

// TestIsOtherCheckout plants each shape a `.git` entry takes, and each
// near-miss, below a root that is a checkout itself, as every real sweep's
// root is. The root must never count, however it is spelled, and nothing but
// a `.git` entry in the directory itself may.
func TestIsOtherCheckout(t *testing.T) {
	root := t.TempDir()
	mkdir := func(rel string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(rel)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(rel, body string) {
		t.Helper()
		mkdir(filepath.Dir(filepath.FromSlash(rel)))
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(rel)), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mkdir(".git")
	write("worktree/.git", "gitdir: /elsewhere/.git/worktrees/worktree\n")
	mkdir("clone/.git")
	mkdir("plain")
	write("alike/.github/pull_request_template.md", "")
	write("alike/.gitignore", "")
	write("alike/.gitattributes", "")
	mkdir("parent/child/.git")

	for _, c := range []struct {
		name string
		dir  string
		want bool
	}{
		{"the root, which holds a .git directory", root, false},
		{"the root, spelled with a trailing separator and a dot", root + string(filepath.Separator) + ".", false},
		{"a worktree or a submodule, whose .git is a gitdir file", filepath.Join(root, "worktree"), true},
		{"a clone, whose .git is a directory", filepath.Join(root, "clone"), true},
		{"a plain directory", filepath.Join(root, "plain"), false},
		{"a directory holding .github, .gitignore and .gitattributes", filepath.Join(root, "alike"), false},
		{"a directory whose child is a checkout", filepath.Join(root, "parent"), false},
		{"the child that is", filepath.Join(root, "parent", "child"), true},
	} {
		if got := IsOtherCheckout(root, c.dir); got != c.want {
			t.Errorf("%s: IsOtherCheckout(root, %q) = %v, want %v", c.name, c.dir, got, c.want)
		}
	}

	t.Run("a dangling .git symlink", func(t *testing.T) {
		dir := filepath.Join(root, "dangling")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(root, "gone"), filepath.Join(dir, ".git")); err != nil {
			t.Skipf("cannot create a symlink on this host: %v", err)
		}
		// os.Stat would follow the link and miss. The link is still a
		// statement that this directory is somebody's checkout.
		if !IsOtherCheckout(root, dir) {
			t.Error("a directory whose .git is a dangling symlink was not taken for a checkout")
		}
	})

	t.Run("a directory that cannot be searched", func(t *testing.T) {
		if runtime.GOOS == "windows" || os.Geteuid() == 0 {
			t.Skip("permission bits do not stop this process from searching a directory here")
		}
		dir := filepath.Join(root, "sealed")
		if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, 0o600); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
		// It holds a .git, but os.Lstat cannot see it. Answering "a
		// checkout" would skip it in silence; answering no hands it to the
		// walk, which fails on it with the real error.
		if IsOtherCheckout(root, dir) {
			t.Error("a directory os.Lstat could not search was taken for a checkout, which skips it in silence")
		}
	})
}

// TestIsMakeTestTmpDir: make test's TMPDIR is the root's own MakeTestTmpDir,
// however the root and the directory are spelled, and nothing else: not a
// directory of that name further down, not a name that begins with it, not
// a directory below it, and never the root, whatever it is called.
func TestIsMakeTestTmpDir(t *testing.T) {
	root := filepath.Join(t.TempDir(), "checkout")
	sep := string(filepath.Separator)
	tmp := filepath.Join(root, MakeTestTmpDir)
	for _, c := range []struct {
		name string
		root string
		dir  string
		want bool
	}{
		{"the root's own", root, tmp, true},
		{"the root spelled with a trailing separator and a dot", root + sep + ".", tmp, true},
		{"the directory spelled with a trailing separator", root, tmp + sep, true},
		{"below a relative root", ".", MakeTestTmpDir, true},
		{"below a root that climbs, as the hash-cost guard's does", filepath.Join("..", ".."), filepath.Join("..", "..", MakeTestTmpDir), true},
		{"one further down", root, filepath.Join(root, "internal", "x", MakeTestTmpDir), false},
		{"a name that begins with it", root, tmp + "foo", false},
		{"a name that continues it after a dot", root, tmp + ".old", false},
		{"the name without its dot", root, filepath.Join(root, "tmp"), false},
		{"a directory below it", root, filepath.Join(tmp, "TestX123"), false},
		{"the root", root, root, false},
		{"a root that bears the name", tmp, tmp, false},
	} {
		if got := IsMakeTestTmpDir(c.root, c.dir); got != c.want {
			t.Errorf("%s: IsMakeTestTmpDir(%q, %q) = %v, want %v", c.name, c.root, c.dir, got, c.want)
		}
	}
}

// TestARemovalRemovesWhatTheWalkReaches: a walk through a Removal removes
// its Dir at the moment it reaches it, once the directory above it has been
// listed, so a walk that reads Dir then fails to list it, as a sweep fails
// on a directory another process removed mid-walk; a walk that skips what
// holds Dir never reaches it, and leaves it where it was.
func TestARemovalRemovesWhatTheWalkReaches(t *testing.T) {
	tree := func(t *testing.T) (root, dir string) {
		t.Helper()
		root = t.TempDir()
		dir = filepath.Join(root, "outer", "gone")
		if err := os.MkdirAll(filepath.Join(dir, "below"), 0o755); err != nil {
			t.Fatal(err)
		}
		return root, dir
	}
	t.Run("a walk that reads it", func(t *testing.T) {
		root, dir := tree(t)
		r := &Removal{Dir: dir}
		var reached []string
		err := r.WalkDir(root, func(path string, _ fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			reached = append(reached, path)
			return nil
		})
		if !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("the walk answered %v, want its listing of %s to fail as gone", err, dir)
		}
		if !r.Walked || !r.Reached {
			t.Errorf("Walked = %v, Reached = %v, want both", r.Walked, r.Reached)
		}
		if _, err := os.Lstat(dir); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s is still there (%v)", dir, err)
		}
		// The walk reached the directory itself, before its listing, and
		// nothing below it.
		if !slices.Contains(reached, dir) || slices.Contains(reached, filepath.Join(dir, "below")) {
			t.Errorf("the walk reached %q, want %s and nothing below it", reached, dir)
		}
	})
	t.Run("a walk that skips what holds it", func(t *testing.T) {
		root, dir := tree(t)
		r := &Removal{Dir: dir}
		err := r.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() && d.Name() == "outer" {
				return filepath.SkipDir
			}
			return nil
		})
		if err != nil {
			t.Errorf("the walk answered %v, want nil", err)
		}
		if !r.Walked || r.Reached {
			t.Errorf("Walked = %v, Reached = %v, want a walk that never reached %s", r.Walked, r.Reached, dir)
		}
		if _, err := os.Lstat(dir); err != nil {
			t.Errorf("%s is gone (%v), though the walk never reached it", dir, err)
		}
	})
}
