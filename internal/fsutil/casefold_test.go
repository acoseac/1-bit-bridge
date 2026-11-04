package fsutil

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// foldingVolume stands in for volumes this host may not have, over a real
// temporary tree: a lookup finds the entry its name spells exactly, and in a
// folder of folds also the entry whose name is the same with case folded, as a
// volume that folds case finds it; in every other folder nothing else, as a
// case-sensitive volume does, whatever this host's volume is.
type foldingVolume struct{ folds map[string]bool }

func (foldingVolume) readDir(dir string, n int) ([]fs.DirEntry, error) {
	return osFolding{}.readDir(dir, n)
}

func (v foldingVolume) lstat(name string) (fs.FileInfo, error) {
	dir, base := filepath.Dir(name), filepath.Base(name)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.Name() == base {
			return os.Lstat(filepath.Join(dir, e.Name()))
		}
	}
	if v.folds[dir] {
		for _, e := range entries {
			if strings.EqualFold(e.Name(), base) {
				return os.Lstat(filepath.Join(dir, e.Name()))
			}
		}
	}
	return nil, &fs.PathError{Op: "lstat", Path: name, Err: fs.ErrNotExist}
}

func (foldingVolume) sameFile(a, b fs.FileInfo) bool { return os.SameFile(a, b) }

// mkdirs makes each folder, with its parents.
func mkdirs(t *testing.T, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

// TestFoldingIsJudgedByTheNamesInTheFolderItself: how a folder compares names
// is read from the names in it, never from the folder's own name in the folder
// above, which another volume may compare (Codex on round 6): an exFAT stick
// mounted on a folder of an ext4 volume folds the names in it, and a
// case-sensitive volume mounted on a folder of one that folds case does not.
// The volumes are stood in for (foldingVolume).
func TestFoldingIsJudgedByTheNamesInTheFolderItself(t *testing.T) {
	base := t.TempDir()
	parent := filepath.Join(base, "music")
	mount := filepath.Join(parent, "usb")
	mkdirs(t, filepath.Join(mount, "library"))
	for _, c := range []struct {
		name  string
		folds map[string]bool
		want  CaseFolding
	}{
		{"a volume that folds case, mounted on one that does not", map[string]bool{mount: true}, CaseInsensitive},
		{"a case-sensitive volume, mounted on one that folds case", map[string]bool{base: true, parent: true}, CaseSensitive},
	} {
		if got := foldingIn(foldingVolume{c.folds}, mount); got != c.want {
			t.Errorf("%s: the folder answered %s, want %s", c.name, got, c.want)
		}
	}
}

// TestFoldingIsUnknownWhereTheFolderCannotTell: a folder whose listing offers
// no name with a letter in it (an empty one, a bare mount point among them, or
// one of numbered folders), one that is not there and one that cannot be read
// answer CaseUnknown, never the answer of the folder above or of the platform
// (Codex on round 6).
func TestFoldingIsUnknownWhereTheFolderCannotTell(t *testing.T) {
	base := t.TempDir()
	empty, caseless := filepath.Join(base, "empty"), filepath.Join(base, "caseless")
	mkdirs(t, empty, filepath.Join(caseless, "2024"), filepath.Join(caseless, "01 - 02"))
	dirs := []string{empty, caseless, filepath.Join(base, "missing")}
	if runtime.GOOS != "windows" && os.Geteuid() != 0 {
		unreadable := filepath.Join(base, "unreadable")
		mkdirs(t, filepath.Join(unreadable, "Album"))
		if err := os.Chmod(unreadable, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(unreadable, 0o755) })
		dirs = append(dirs, unreadable)
	}
	for _, dir := range dirs {
		if got := FoldingIn(dir); got != CaseUnknown {
			t.Errorf("the folder %s answered %s, want %s", filepath.Base(dir), got, CaseUnknown)
		}
	}
}

// TestFoldingInWritesNothing: a probe that cannot tell leaves the folder as it
// was: it makes no name to look up, since a folder it asks about is in a
// library root (B651's rules) and may be on a read-only volume (Argon on the
// brief).
func TestFoldingInWritesNothing(t *testing.T) {
	base := t.TempDir()
	empty, caseless := filepath.Join(base, "empty"), filepath.Join(base, "caseless")
	mkdirs(t, empty, filepath.Join(caseless, "2024"))
	for _, dir := range []string{empty, caseless} {
		before, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		FoldingIn(dir)
		after, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.EqualFunc(before, after, func(a, b fs.DirEntry) bool { return a.Name() == b.Name() }) {
			t.Errorf("the probe of %s changed its listing from %v to %v", filepath.Base(dir), before, after)
		}
	}
}

// TestFoldingInTellsTwoEntriesApart: a folder that holds two spellings of a
// name as two entries does not fold case, whichever of them the probe looks up.
// Two such entries can be made on a case-sensitive volume alone.
func TestFoldingInTellsTwoEntriesApart(t *testing.T) {
	dir := t.TempDir()
	mkdirs(t, filepath.Join(dir, "Ab"))
	if err := os.Mkdir(filepath.Join(dir, "AB"), 0o755); err != nil {
		t.Skipf("this host's volume holds one of two spellings of a name: %v", err)
	}
	if got := FoldingIn(dir); got != CaseSensitive {
		t.Errorf("a folder holding Ab and AB answered %s, want %s", got, CaseSensitive)
	}
}

// TestFoldingInAnswersAsThisHostsVolumeLooksNamesUp: on this host's own
// volume, a folder holding a name with letters in it answers, and its answer
// is what a lookup of that name in another case finds.
func TestFoldingInAnswersAsThisHostsVolumeLooksNamesUp(t *testing.T) {
	dir := t.TempDir()
	mkdirs(t, filepath.Join(dir, "Probe"))
	_, err := os.Lstat(filepath.Join(dir, "PROBE"))
	want := CaseSensitive
	if err == nil {
		want = CaseInsensitive
	}
	if got := FoldingIn(dir); got != want {
		t.Errorf("this host's folder answered %s; a lookup of PROBE beside Probe says %s", got, want)
	}
}

// TestSwapASCIICase: the probe's other spelling of a name swaps the case of
// its ASCII letters alone, and a name with none offers no other spelling.
func TestSwapASCIICase(t *testing.T) {
	for _, c := range []struct {
		name, want string
		ok         bool
	}{
		{"usb", "USB", true},
		{"USB", "usb", true},
		{"Ab", "AB", true},
		{"ÉCOLE", "École", true},
		{"2024", "2024", false},
		{"Ünïcödé", "ÜNïCöDé", true},
	} {
		if got, ok := swapASCIICase(c.name); got != c.want || ok != c.ok {
			t.Errorf("swapASCIICase(%q) = %q, %v; want %q, %v", c.name, got, ok, c.want, c.ok)
		}
	}
}
