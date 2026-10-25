//go:build !windows

package fsutil

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// resolvedDir is t.TempDir() as the OS resolves it (on macOS the temp
// directory is reached through /var, a link to /private/var), so that what
// ResolveParent answers can be compared with it.
func resolvedDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// mustMkdirs makes each directory, failing the test on an error.
func mustMkdirs(t *testing.T, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

// mustSymlink makes link lead to target, failing the test on an error.
func mustSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

// TestResolveParentFollowsALinkAtTheLastElement: a path whose last element is
// a symbolic link answers the file the link leads to, its directory and its
// own name, through a relative link and a chain of them too: a lock taken
// beside the answer sits beside the target and is named after it, and a file
// renamed onto the answer replaces the target and leaves the link. Until
// B506's third round the link's own directory and name were answered, so the
// link and its target took two locks, and a save renamed a file over the
// link.
func TestResolveParentFollowsALinkAtTheLastElement(t *testing.T) {
	base := resolvedDir(t)
	cfg, dot := filepath.Join(base, "cfg"), filepath.Join(base, "dot")
	mustMkdirs(t, cfg, dot)
	target := filepath.Join(dot, "actual.yaml")
	if err := os.WriteFile(target, []byte("libraryName: Linked\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg, "plain.yaml"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	mustSymlink(t, target, filepath.Join(cfg, "alias.yaml"))
	mustSymlink(t, filepath.Join("..", "dot", "actual.yaml"), filepath.Join(cfg, "relative.yaml"))
	mustSymlink(t, filepath.Join(cfg, "alias.yaml"), filepath.Join(cfg, "chain.yaml"))
	for _, tc := range []struct{ path, dir, name string }{
		{filepath.Join(cfg, "alias.yaml"), dot, "actual.yaml"},
		{filepath.Join(cfg, "relative.yaml"), dot, "actual.yaml"},
		{filepath.Join(cfg, "chain.yaml"), dot, "actual.yaml"},
		{target, dot, "actual.yaml"},
		{filepath.Join(cfg, "plain.yaml"), cfg, "plain.yaml"},
		{filepath.Join(cfg, "missing.yaml"), cfg, "missing.yaml"},
	} {
		dir, name, err := ResolveParent(tc.path)
		if err != nil || dir != tc.dir || name != tc.name {
			t.Errorf("ResolveParent(%s) = %q, %q, %v; want %q, %q", tc.path, dir, name, err, tc.dir, tc.name)
		}
	}
	if info, err := os.Lstat(filepath.Join(cfg, "alias.yaml")); err != nil || info.Mode()&fs.ModeSymlink == 0 {
		t.Errorf("resolving the link changed it: %v, %v", info, err)
	}
}

// TestResolveParentResolvesMissingDirectoriesThroughTheirNearestAncestor: a
// path whose directories do not all exist yet (bridge init's --dir, made
// before the config is saved) answers its nearest existing ancestor as the OS
// resolves it, a link among it followed, and the missing directories after it
// as written. Until B506's third round it was an error.
func TestResolveParentResolvesMissingDirectoriesThroughTheirNearestAncestor(t *testing.T) {
	base := resolvedDir(t)
	real := filepath.Join(base, "real")
	mustMkdirs(t, real)
	mustSymlink(t, real, filepath.Join(base, "sym"))
	for _, tc := range []struct{ path, dir string }{
		{filepath.Join(base, "sym", "new", "nested", "dir", "bridge.yaml"), filepath.Join(real, "new", "nested", "dir")},
		{filepath.Join(base, "sym", "new", "bridge.yaml"), filepath.Join(real, "new")},
		{filepath.Join(base, "fresh", "bridge.yaml"), filepath.Join(base, "fresh")},
	} {
		dir, name, err := ResolveParent(tc.path)
		if err != nil || dir != tc.dir || name != "bridge.yaml" {
			t.Errorf("ResolveParent(%s) = %q, %q, %v; want %q, bridge.yaml", tc.path, dir, name, err, tc.dir)
		}
	}
	if _, err := os.Stat(filepath.Join(real, "new")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("resolving made a directory: %v", err)
	}
}

// TestResolveParentRefusesALinkThatLeadsNowhere: a last element that is a
// symbolic link to no file is refused (ErrDanglingLink), naming the path as
// given, and the link is left as it is and its target not made: a write
// there would either make the file the link names, wherever that is, or
// replace the link with a file, and neither is the writer's to choose.
func TestResolveParentRefusesALinkThatLeadsNowhere(t *testing.T) {
	base := resolvedDir(t)
	link := filepath.Join(base, "bridge.yaml")
	nowhere := filepath.Join(base, "gone", "bridge.yaml")
	mustSymlink(t, nowhere, link)
	dir, name, err := ResolveParent(link)
	var pe *fs.PathError
	if !errors.Is(err, ErrDanglingLink) || !errors.As(err, &pe) || pe.Path != link {
		t.Errorf("ResolveParent of a link that leads nowhere = %q, %q, %v; want a refusal naming %s", dir, name, err, link)
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&fs.ModeSymlink == 0 {
		t.Errorf("the link is gone or changed: %v, %v", info, err)
	}
	if _, err := os.Stat(filepath.Dir(nowhere)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("resolving made the link's target's directory: %v", err)
	}
}
