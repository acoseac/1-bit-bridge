//go:build windows

package fsutil

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A config's directory reached through a directory junction (`mklink /J`, the
// ordinary way to point a folder at another volume on Windows, since it needs
// no privilege) is the junction's target, as the OS opens it. Since Go 1.23 a
// junction is ModeIrregular, and filepath.EvalSymlinks refuses one as an
// element of a path with ENOTDIR, so until B506's fourth round ResolveParent's
// EvalSymlinks of the whole path refused a file link inside a junction, and
// answered a plain file there by the junction's own spelling, which a junction
// repointed during a turn spells the same way. ResolveParent now resolves the
// directory, and a link at the last element, by the handle the OS opens
// (ResolveLinks, GetFinalPathNameByHandle). The file links are made through
// walkroot_test.go's symlinkOrSkip, which skips a test where Windows refuses
// this process one (Developer Mode off, not elevated).

// junctionedConfig is a directory real holding actual.yaml, a plain file,
// reached through the junction cfg, and real's own spelling as the OS names it
// (the temp directory can be spelled with 8.3 short names, which the handle's
// name turns into the long ones).
type junctionedConfig struct {
	base, real, cfg, canonReal string
}

func newJunctionedConfig(t *testing.T) junctionedConfig {
	t.Helper()
	base := t.TempDir()
	f := junctionedConfig{base: base, real: filepath.Join(base, "real"), cfg: filepath.Join(base, "cfg")}
	if err := os.MkdirAll(f.real, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.real, "actual.yaml"), []byte("libraryName: Before\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	makeJunction(t, f.real, f.cfg)
	canon, err := ResolveLinks(f.real)
	if err != nil {
		t.Fatal(err)
	}
	f.canonReal = canon
	return f
}

// repointJunction makes the junction link lead to target.
func repointJunction(t *testing.T, link, target string) {
	t.Helper()
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	makeJunction(t, target, link)
}

// TestEntryOfReadsTheEntryItself: the directory check reads a directory's own
// entry (entryOf), never what it leads to: a plain directory is no reparse
// point, a junction is a mount point and a directory, a file is no directory,
// and a path that is not there is a not-exist error. The check's decision on
// what it reads, a cloud folder's tag included, is
// TestCheckResolvedDirRefusesOnlyALinkOrAMove's, which no host without a sync
// provider can make natively.
func TestEntryOfReadsTheEntryItself(t *testing.T) {
	base := t.TempDir()
	real, junction, file := filepath.Join(base, "real"), filepath.Join(base, "junction"), filepath.Join(base, "file.txt")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	makeJunction(t, real, junction)
	for _, c := range []struct {
		name, path string
		tag        uint32
		dir        bool
	}{
		{"a plain directory", real, 0, true},
		{"a junction", junction, reparseTagMountPoint, true},
		{"a file", file, 0, false},
	} {
		tag, dir, err := entryOf(c.path)
		if err != nil || tag != c.tag || dir != c.dir {
			t.Errorf("entryOf(%s) = %#x, %v, %v; want %#x, %v", c.name, tag, dir, err, c.tag, c.dir)
		}
	}
	if _, _, err := entryOf(filepath.Join(base, "gone")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("entryOf of a path that is not there = %v, want a not-exist error", err)
	}
}

// TestResolveParentFollowsAJunctionToTheConfigsDirectory: a plain file
// reached through a junction is in the junction's target, named as the OS
// names that directory.
func TestResolveParentFollowsAJunctionToTheConfigsDirectory(t *testing.T) {
	f := newJunctionedConfig(t)
	dir, name, err := ResolveParent(filepath.Join(f.cfg, "actual.yaml"))
	if err != nil || dir != f.canonReal || name != "actual.yaml" {
		t.Errorf("ResolveParent through a junction = %q, %q, %v; want %q, actual.yaml", dir, name, err, f.canonReal)
	}
}

// TestResolveParentFollowsAFileLinkInsideAJunction: a file symbolic link in a
// directory reached through a junction is followed to its target, whose
// directory and own name are answered (Codex's case on B506's third round:
// serve read such a config while no writer could take its turn).
func TestResolveParentFollowsAFileLinkInsideAJunction(t *testing.T) {
	f := newJunctionedConfig(t)
	symlinkOrSkip(t, filepath.Join(f.real, "actual.yaml"), filepath.Join(f.real, "alias.yaml"))
	dir, name, err := ResolveParent(filepath.Join(f.cfg, "alias.yaml"))
	if err != nil || dir != f.canonReal || name != "actual.yaml" {
		t.Errorf("ResolveParent of a file link inside a junction = %q, %q, %v; want %q, actual.yaml", dir, name, err, f.canonReal)
	}
}

// TestResolveParentRefusesADanglingLinkInsideAJunction: a file symbolic link
// to no file, inside a directory reached through a junction, is refused as a
// link to nothing: an *fs.PathError wrapping ErrDanglingLink, never the link
// answered as itself.
func TestResolveParentRefusesADanglingLinkInsideAJunction(t *testing.T) {
	f := newJunctionedConfig(t)
	symlinkOrSkip(t, filepath.Join(f.real, "gone.yaml"), filepath.Join(f.real, "dangling.yaml"))
	dir, name, err := ResolveParent(filepath.Join(f.cfg, "dangling.yaml"))
	var pathErr *fs.PathError
	if !errors.Is(err, ErrDanglingLink) || !errors.As(err, &pathErr) {
		t.Errorf("ResolveParent of a dangling link inside a junction = %q, %q, %v; want an *fs.PathError wrapping ErrDanglingLink", dir, name, err)
	}
}

// TestResolveParentAnswersOneSpellingForAnUnmovedName: two resolutions of one
// name that nothing repointed answer one directory and one name, spelled
// without the `\\?\` prefix the handle's name carries: a file there, one that
// is not there yet, and one below directories that are not there yet, all
// through the junction.
func TestResolveParentAnswersOneSpellingForAnUnmovedName(t *testing.T) {
	f := newJunctionedConfig(t)
	for _, p := range []string{
		filepath.Join(f.cfg, "actual.yaml"),
		filepath.Join(f.cfg, "new.yaml"),
		filepath.Join(f.cfg, "new", "nested", "bridge.yaml"),
	} {
		dir1, name1, err1 := ResolveParent(p)
		dir2, name2, err2 := ResolveParent(p)
		if err1 != nil || err2 != nil || dir1 != dir2 || name1 != name2 {
			t.Errorf("ResolveParent(%s) twice = (%q, %q, %v) and (%q, %q, %v): want one answer", p, dir1, name1, err1, dir2, name2, err2)
			continue
		}
		if !strings.HasPrefix(dir1, f.canonReal) || strings.HasPrefix(dir1, `\\?\`) {
			t.Errorf("ResolveParent(%s) answered %s: want it under %s, spelled without the verbatim prefix", p, dir1, f.canonReal)
		}
	}
}

// TestResolveParentSeesAJunctionRepointed: a junction repointed to another
// directory resolves to that directory, so a turn that resolves its name
// again sees that the name opens another file.
func TestResolveParentSeesAJunctionRepointed(t *testing.T) {
	f := newJunctionedConfig(t)
	other := filepath.Join(f.base, "other")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(f.cfg, "actual.yaml")
	before, _, err := ResolveParent(name)
	if err != nil {
		t.Fatal(err)
	}
	repointJunction(t, f.cfg, other)
	after, _, err := ResolveParent(name)
	if err != nil {
		t.Fatal(err)
	}
	canonOther, err := ResolveLinks(other)
	if err != nil {
		t.Fatal(err)
	}
	if before != f.canonReal || after != canonOther {
		t.Errorf("ResolveParent before and after the junction moved = %q and %q; want %q and %q", before, after, f.canonReal, canonOther)
	}
}
