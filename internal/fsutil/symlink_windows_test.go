//go:build windows

package fsutil

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// makeJunction makes link a directory junction to target. `mklink /J` needs
// no privilege, where a symbolic link does, so this runs on any Windows host.
func makeJunction(t *testing.T, target, link string) {
	t.Helper()
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
		t.Fatalf("mklink /J %s %s: %v: %s", link, target, err, out)
	}
}

// junctionFixture is a directory `real` holding `sub`, a junction `lib` to
// it, a junction `chain` to `lib`, and `real`'s own canonical spelling (the
// temp directory can be spelled with 8.3 short names, which every resolution
// turns into the long ones).
type junctionFixture struct {
	base, real, lib, chain, canonReal string
}

func newJunctionFixture(t *testing.T) junctionFixture {
	t.Helper()
	base := t.TempDir()
	f := junctionFixture{
		base:  base,
		real:  filepath.Join(base, "real"),
		lib:   filepath.Join(base, "lib"),
		chain: filepath.Join(base, "chain"),
	}
	if err := os.MkdirAll(filepath.Join(f.real, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	makeJunction(t, f.real, f.lib)
	makeJunction(t, f.lib, f.chain)
	canon, err := filepath.EvalSymlinks(f.real)
	if err != nil {
		t.Fatal(err)
	}
	f.canonReal = canon
	return f
}

// TestEvalSymlinksOrCleanFollowsAJunction: a junction, a junction to a
// junction, and a path through a junction to something not made yet all
// resolve to the junction target's own path. Before junctions were
// followed, the first two came back as the junction itself and the third as
// a path under it (filepath.EvalSymlinks leaves a junction at the end of a
// path alone and fails with ENOTDIR through one), so nothing that compared
// them with the target's path could see they were the same directory.
func TestEvalSymlinksOrCleanFollowsAJunction(t *testing.T) {
	f := newJunctionFixture(t)
	for _, c := range []struct{ in, want string }{
		{f.lib, f.canonReal},
		{f.chain, f.canonReal},
		{filepath.Join(f.lib, "sub"), filepath.Join(f.canonReal, "sub")},
		{filepath.Join(f.chain, "sub", "variants", "new"), filepath.Join(f.canonReal, "sub", "variants", "new")},
	} {
		if got := EvalSymlinksOrClean(c.in); got != c.want {
			t.Errorf("EvalSymlinksOrClean(%s) = %s, want %s", c.in, got, c.want)
		}
	}
}

// TestEvalSymlinksOrCleanKeepsADanglingJunction: a junction whose target is
// gone resolves no further than the junction, as a dangling symbolic link
// does, and a path below it keeps its tail.
func TestEvalSymlinksOrCleanKeepsADanglingJunction(t *testing.T) {
	f := newJunctionFixture(t)
	gone := filepath.Join(f.base, "gone")
	if err := os.Mkdir(gone, 0o755); err != nil {
		t.Fatal(err)
	}
	dangling := filepath.Join(f.base, "dangling")
	makeJunction(t, gone, dangling)
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	canonBase, err := filepath.EvalSymlinks(f.base)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(canonBase, "dangling", "variants")
	if got := EvalSymlinksOrClean(filepath.Join(dangling, "variants")); got != want {
		t.Errorf("EvalSymlinksOrClean through a dangling junction = %s, want %s", got, want)
	}
}

// TestResolveLinksFallsBackOnAJunctionLoop: a path that is there but that no
// handle can be opened through (two junctions pointing at each other; the
// open fails with ERROR_CANT_RESOLVE_FILENAME, not a not-exist error) gets
// filepath.EvalSymlinks's answer, the junction as it is, with no error: what
// every caller had before, where refusing would fail a nesting check or a
// sidecar walk outright.
func TestResolveLinksFallsBackOnAJunctionLoop(t *testing.T) {
	base := t.TempDir()
	a, b := filepath.Join(base, "loop-a"), filepath.Join(base, "loop-b")
	makeJunction(t, b, a)
	makeJunction(t, a, b)
	want, err := filepath.EvalSymlinks(a)
	if err != nil {
		t.Fatalf("premise: EvalSymlinks of a junction loop: %v", err)
	}
	if got, err := ResolveLinks(a); err != nil || got != want {
		t.Errorf("ResolveLinks(a junction loop) = %q, %v; want EvalSymlinks's %q", got, err, want)
	}
}

// TestIsUnderAnySeesThroughAJunction is the containment every nesting check
// makes (config.validateVariantsDir, the admin variants-dir handler,
// `bridge variants move`, and the write paths that must stay inside a
// root): a directory reached through a junction on either side, or a chain
// of them, is where the junction points. Measured before junctions were
// followed, every nested case here read as outside, so a variants directory
// spelled by the target's own path inside a junction'd library root was
// accepted, which is what the check exists to refuse.
func TestIsUnderAnySeesThroughAJunction(t *testing.T) {
	f := newJunctionFixture(t)
	for _, c := range []struct {
		name      string
		candidate string
		root      string
		nested    bool
	}{
		{"the target's own spelling, under a junction'd root", filepath.Join(f.real, "variants"), f.lib, true},
		{"a junction's spelling, under the target", filepath.Join(f.lib, "variants"), f.real, true},
		{"through a chain, under the target", filepath.Join(f.chain, "sub", "variants"), f.real, true},
		{"the target's own spelling, under a chain", filepath.Join(f.real, "sub"), f.chain, true},
		{"a sibling of the target", filepath.Join(f.base, "elsewhere"), f.lib, false},
		{"the junction's parent, above the target", f.base, f.lib, false},
	} {
		got := IsUnderAny(c.candidate, []string{c.root})
		if (got != "") != c.nested {
			t.Errorf("%s: IsUnderAny(%s, [%s]) = %q, want nested=%v", c.name, c.candidate, c.root, got, c.nested)
		}
	}
}
