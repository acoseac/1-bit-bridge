package sweeptest

import (
	"os"
	"path/filepath"
	"runtime"
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
		{"a worktree, whose .git is a gitdir file", filepath.Join(root, "worktree"), true},
		{"a clone, whose .git is a directory", filepath.Join(root, "clone"), true},
		{"a plain directory", filepath.Join(root, "plain"), false},
		{"a directory holding .github, .gitignore and .gitattributes", filepath.Join(root, "alike"), false},
		{"a directory whose child is a checkout", filepath.Join(root, "parent"), false},
		{"the child that is", filepath.Join(root, "parent", "child"), true},
		{"the root's own .git directory", filepath.Join(root, ".git"), false},
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
