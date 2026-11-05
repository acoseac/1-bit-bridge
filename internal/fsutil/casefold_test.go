package fsutil

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
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

// probeFolder is a folder that is on no disk, for the probe's seam: its
// listing as the probe reads it, the files its names name now (two names of
// one file, a hard link's, share an id), whether it folds case, the spellings
// os.SameFile could not open a lookup by (on Windows it opens the path the
// lookup was asked for, and answers false where it cannot), and what happens
// to it before each lookup (change, handed the name looked up) and before each
// read of its whole listing (relist, the probe's re-read), so that a test can
// change it between the probe's lookups, or between them and the re-read.
type probeFolder struct {
	listing    []string
	ids        map[string]int
	folds      bool
	cannotOpen map[string]bool
	change     func(f *probeFolder, name string)
	relist     func(f *probeFolder)
}

// probeInfo is a probeFolder's lookup: the spelling it was asked for and the
// file it found.
type probeInfo struct {
	name string
	id   int
}

func (i probeInfo) Name() string         { return i.name }
func (probeInfo) Size() int64            { return 0 }
func (probeInfo) Mode() fs.FileMode      { return 0o644 }
func (probeInfo) ModTime() (t time.Time) { return t }
func (probeInfo) IsDir() bool            { return false }
func (probeInfo) Sys() any               { return nil }

// readDir answers the listing, n names of it at most where n > 0, as
// os.File.ReadDir does: fewer than n only at its end.
func (f *probeFolder) readDir(_ string, n int) ([]fs.DirEntry, error) {
	if n <= 0 && f.relist != nil {
		f.relist(f)
	}
	names := f.listing
	if n > 0 && len(names) > n {
		names = names[:n]
	}
	out := make([]fs.DirEntry, len(names))
	for i, name := range names {
		out[i] = fs.FileInfoToDirEntry(probeInfo{name: name})
	}
	return out, nil
}

func (f *probeFolder) lstat(p string) (fs.FileInfo, error) {
	name := filepath.Base(p)
	if f.change != nil {
		f.change(f, name)
	}
	if id, ok := f.ids[name]; ok {
		return probeInfo{name, id}, nil
	}
	if f.folds {
		for n, id := range f.ids {
			if strings.EqualFold(n, name) {
				return probeInfo{name, id}, nil
			}
		}
	}
	return nil, &fs.PathError{Op: "lstat", Path: p, Err: fs.ErrNotExist}
}

func (f *probeFolder) sameFile(a, b fs.FileInfo) bool {
	x, y := a.(probeInfo), b.(probeInfo)
	return !f.cannotOpen[x.name] && !f.cannotOpen[y.name] && x.id == y.id
}

// caseless is n names with no letter in them, which the probe passes over.
func caseless(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("%03d", i)
	}
	return out
}

// TestTwoNamesOfOneFileAreNoProofThatAFolderFoldsCase: a lookup of a name in
// another case that finds the same file proves one entry only where that
// spelling is no name of its own in the folder: on a case-sensitive volume
// (ext4, a case-sensitive APFS) a hard link's two names, folder.jpg and
// FOLDER.JPG, are one file and two entries (B651's ninth round, Codex and
// Claude on round 8). The whole listing is read for that spelling where the
// probe's first read was not all of it. A folder that folds lists one
// spelling alone, and answers so, however long its listing.
func TestTwoNamesOfOneFileAreNoProofThatAFolderFoldsCase(t *testing.T) {
	links := map[string]int{"folder.jpg": 1, "FOLDER.JPG": 1}
	for _, c := range []struct {
		name string
		f    *probeFolder
		want CaseFolding
	}{
		{"a hard link's two names", &probeFolder{listing: []string{"folder.jpg", "FOLDER.JPG"}, ids: links}, CaseSensitive},
		{"the other way round", &probeFolder{listing: []string{"FOLDER.JPG", "folder.jpg"}, ids: links}, CaseSensitive},
		{"the other name past the first read", &probeFolder{
			listing: append(append(caseless(foldingProbeEntries-1), "folder.jpg"), append(caseless(10), "FOLDER.JPG")...), ids: links}, CaseSensitive},
		{"a folder that folds", &probeFolder{listing: []string{"Folder.jpg"}, ids: map[string]int{"Folder.jpg": 1}, folds: true}, CaseInsensitive},
		{"a folder that folds, past the first read", &probeFolder{
			listing: append(append(caseless(foldingProbeEntries-1), "Folder.jpg"), caseless(10)...),
			ids:     map[string]int{"Folder.jpg": 1}, folds: true}, CaseInsensitive},
	} {
		if got := foldingIn(c.f, "/music"); got != c.want {
			t.Errorf("%s: the folder answered %s, want %s", c.name, got, c.want)
		}
	}
}

// TestAListingThatChangedBeforeItsReReadTellsNothing: where the probe reads
// the whole listing again for the swapped spelling (its first read was not
// all of it), it answers case-sensitive only where the re-read holds both
// spellings, and case-insensitive only where it holds the probed one alone;
// one that holds the swapped spelling alone, or neither, says the entry
// changed between the lookups and the re-read, and the next entry is asked,
// the probe answering Unknown once every one is spent, never case-sensitive
// (B651's tenth round, Codex and Claude on round 9). A rename of Abba to ABBA
// on a volume that folds made it answer case-sensitive, the direction that
// keeps rows a scan must spare exact; the hard link's two names removed on a
// case-sensitive volume made it answer case-insensitive.
func TestAListingThatChangedBeforeItsReReadTellsNothing(t *testing.T) {
	renamed := func(next ...string) *probeFolder {
		f := &probeFolder{
			listing: append(append(caseless(foldingProbeEntries-1-len(next)), append([]string{"Abba"}, next...)...), caseless(10)...),
			ids:     map[string]int{"Abba": 1, "Band": 2},
			folds:   true,
		}
		done := false
		f.relist = func(f *probeFolder) {
			if done {
				return
			}
			done = true
			for i, n := range f.listing {
				if n == "Abba" {
					f.listing[i] = "ABBA"
				}
			}
			delete(f.ids, "Abba")
			f.ids["ABBA"] = 1
		}
		return f
	}
	removed := &probeFolder{
		listing: append(append(caseless(foldingProbeEntries-1), "folder.jpg"), append(caseless(10), "FOLDER.JPG")...),
		ids:     map[string]int{"folder.jpg": 1, "FOLDER.JPG": 1},
	}
	removedOnce := false
	removed.relist = func(f *probeFolder) {
		if removedOnce {
			return
		}
		removedOnce = true
		f.listing = slices.DeleteFunc(f.listing, func(n string) bool { return n == "folder.jpg" || n == "FOLDER.JPG" })
		clear(f.ids)
	}
	for _, c := range []struct {
		name string
		f    *probeFolder
		want CaseFolding
	}{
		{"an entry renamed to its other spelling before the re-read, in a folder that folds, another entry after it", renamed("Band"), CaseInsensitive},
		{"an entry renamed to its other spelling before the re-read, in a folder that folds, no other entry", renamed(), CaseUnknown},
		{"a hard link's two names removed before the re-read, in a case-sensitive folder", removed, CaseUnknown},
	} {
		if got := foldingIn(c.f, "/music"); got != c.want {
			t.Errorf("%s: the folder answered %s, want %s", c.name, got, c.want)
		}
	}
}

// TestAnEntryThatChangesBetweenTheLookupsTellsNothing: the probe answers only
// from lookups that agree with themselves, the entry it probes still the file
// it first found, and the other spelling's too where it was found; otherwise
// it asks the next entry (B651's ninth round, Codex and Claude on round 8).
// An entry removed between the two lookups, or replaced by a save that
// renames a new file over it, or one os.SameFile cannot open by either
// spelling (Windows answers false then), made a folder that folds answer
// case-sensitive; one renamed to its other spelling between the lookups made a
// case-sensitive folder answer case-insensitive.
func TestAnEntryThatChangesBetweenTheLookupsTellsNothing(t *testing.T) {
	once := func(at string, do func(f *probeFolder)) func(*probeFolder, string) {
		done := false
		return func(f *probeFolder, name string) {
			if name == at && !done {
				done = true
				do(f)
			}
		}
	}
	folding := func() *probeFolder {
		return &probeFolder{listing: []string{"Album", "Band"}, ids: map[string]int{"Album": 1, "Band": 2}, folds: true}
	}
	removed, saved, unopened, otherUnopened := folding(), folding(), folding(), folding()
	removed.change = once("ALBUM", func(f *probeFolder) { delete(f.ids, "Album") })
	saved.change = once("ALBUM", func(f *probeFolder) { f.ids["Album"] = 3 })
	unopened.cannotOpen = map[string]bool{"Album": true}
	otherUnopened.cannotOpen = map[string]bool{"ALBUM": true}
	renamed := &probeFolder{listing: []string{"Album"}, ids: map[string]int{"Album": 1}}
	renamed.change = once("ALBUM", func(f *probeFolder) {
		delete(f.ids, "Album")
		f.ids["ALBUM"] = 1
	})
	for _, c := range []struct {
		name string
		f    *probeFolder
		want CaseFolding
	}{
		{"an entry removed between the lookups, in a folder that folds", removed, CaseInsensitive},
		{"an entry a save replaced between the lookups, in a folder that folds", saved, CaseInsensitive},
		{"an entry os.SameFile cannot open, in a folder that folds", unopened, CaseInsensitive},
		{"an entry os.SameFile cannot open by its other spelling, in a folder that folds", otherUnopened, CaseInsensitive},
		{"an entry renamed to its other spelling between the lookups, in a case-sensitive folder", renamed, CaseUnknown},
	} {
		if got := foldingIn(c.f, "/music"); got != c.want {
			t.Errorf("%s: the folder answered %s, want %s", c.name, got, c.want)
		}
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
