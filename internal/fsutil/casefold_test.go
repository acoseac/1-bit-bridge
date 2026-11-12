package fsutil

import (
	"fmt"
	"io"
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
// lookup was asked for, and answers false where it cannot), what happens to
// it before each lookup (change, handed the name looked up) and before each
// read of its listing after the first (relist, the probe's re-reads), so that
// a test can change it between the probe's lookups, or between them and a
// re-read, and how many entries each read of its listing asked for (reads,
// each n as readDir was handed it).
type probeFolder struct {
	listing    []string
	ids        map[string]int
	folds      bool
	cannotOpen map[string]bool
	change     func(f *probeFolder, name string)
	relist     func(f *probeFolder)
	reads      []int
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
// os.File.ReadDir does: fewer than n only at its end, and none there with
// io.EOF.
func (f *probeFolder) readDir(_ string, n int) ([]fs.DirEntry, error) {
	if len(f.reads) > 0 && f.relist != nil {
		f.relist(f)
	}
	f.reads = append(f.reads, n)
	names := f.listing
	if n > 0 && len(names) > n {
		names = names[:n]
	}
	if n > 0 && len(names) == 0 {
		return nil, io.EOF
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

// TestAListingThatChangedBeforeItsReReadTellsNothing: the probe reads the
// listing again for the swapped spelling, after the lookups, and answers
// case-sensitive only where the re-read holds both spellings, and
// case-insensitive only where it holds the probed one alone; one that holds
// the swapped spelling alone, or neither, says the entry changed between the
// lookups and the re-read, and the next entry is asked, the probe answering
// Unknown once every one is spent, never case-sensitive (B651's tenth round,
// Codex and Claude on round 9). A rename of Abba to ABBA on a volume that
// folds made it answer case-sensitive, the direction that keeps rows a scan
// must spare exact; the hard link's two names removed on a case-sensitive
// volume made it answer case-insensitive. A re-read that lacks the swapped
// spelling answers case-insensitive only once a lookup of that spelling after
// it still finds the file: the hard link's second name alone removed on a
// case-sensitive volume left the first, and the probe answered
// case-insensitive (B651's eleventh round, Codex and Claude on round 10).
// Each case runs in a folder the probe's first read is not all of, and in one
// it is all of, which the probe settled by that first read until B738 and
// reads again now as well.
func TestAListingThatChangedBeforeItsReReadTellsNothing(t *testing.T) {
	// listed is the folder's listing: first and then rest where the probe's
	// first read is all of it (small); else first at the end of that read,
	// after names with no letter, and rest past it.
	listed := func(small bool, first []string, rest ...string) []string {
		if small {
			return append(slices.Clone(first), rest...)
		}
		return append(append(caseless(foldingProbeEntries-len(first)), first...), append(caseless(10), rest...)...)
	}
	once := func(do func(f *probeFolder)) func(*probeFolder) {
		done := false
		return func(f *probeFolder) {
			if !done {
				done = true
				do(f)
			}
		}
	}
	renamed := func(small bool, next ...string) *probeFolder {
		f := &probeFolder{
			listing: listed(small, append([]string{"Abba"}, next...)),
			ids:     map[string]int{"Abba": 1, "Band": 2},
			folds:   true,
		}
		f.relist = once(func(f *probeFolder) {
			for i, n := range f.listing {
				if n == "Abba" {
					f.listing[i] = "ABBA"
				}
			}
			delete(f.ids, "Abba")
			f.ids["ABBA"] = 1
		})
		return f
	}
	// links is a hard link's two names, folder.jpg and FOLDER.JPG, in a
	// case-sensitive folder, those gone removed before the re-read.
	links := func(small bool, gone ...string) *probeFolder {
		f := &probeFolder{
			listing: listed(small, []string{"folder.jpg"}, "FOLDER.JPG"),
			ids:     map[string]int{"folder.jpg": 1, "FOLDER.JPG": 1},
		}
		f.relist = once(func(f *probeFolder) {
			f.listing = slices.DeleteFunc(f.listing, func(n string) bool { return slices.Contains(gone, n) })
			for _, n := range gone {
				delete(f.ids, n)
			}
		})
		return f
	}
	for _, small := range []bool{false, true} {
		folder := "a folder the first read is not all of"
		if small {
			folder = "a folder the first read is all of"
		}
		for _, c := range []struct {
			name string
			f    *probeFolder
			want CaseFolding
		}{
			{"an entry renamed to its other spelling before the re-read, in a folder that folds, another entry after it", renamed(small, "Band"), CaseInsensitive},
			{"an entry renamed to its other spelling before the re-read, in a folder that folds, no other entry", renamed(small), CaseUnknown},
			{"a hard link's two names removed before the re-read, in a case-sensitive folder", links(small, "folder.jpg", "FOLDER.JPG"), CaseUnknown},
			{"a hard link's other name alone removed before the re-read, in a case-sensitive folder", links(small, "FOLDER.JPG"), CaseUnknown},
		} {
			if got := foldingIn(c.f, "/music"); got != c.want {
				t.Errorf("%s, %s: the folder answered %s, want %s", folder, c.name, got, c.want)
			}
		}
	}
}

// TestASecondNameMadeBeforeTheLookupsIsInTheListingThatSettles: the listing
// that tells a hard link's two names from one entry found under two
// spellings is read after the lookups it settles (B738, Codex on #1397): in a
// case-sensitive folder whose listing the probe's first read was all of, a
// hard link FOLDER.JPG made beside folder.jpg between that read and the
// lookups is found by both lookups as one file and is in no listing read
// before them, and the probe, which settled by its first read, answered
// case-insensitive. The second name is added to the listing and to the
// lookups at the probe's first lookup (change), either way round, and beside
// names with no letter.
func TestASecondNameMadeBeforeTheLookupsIsInTheListingThatSettles(t *testing.T) {
	linkMade := func(listed, made string, others ...string) *probeFolder {
		f := &probeFolder{listing: append([]string{listed}, others...), ids: map[string]int{listed: 1}}
		done := false
		f.change = func(f *probeFolder, _ string) {
			if !done {
				done = true
				f.listing = append(f.listing, made)
				f.ids[made] = 1
			}
		}
		return f
	}
	for _, c := range []struct {
		name string
		f    *probeFolder
	}{
		{"FOLDER.JPG made beside folder.jpg", linkMade("folder.jpg", "FOLDER.JPG")},
		{"folder.jpg made beside FOLDER.JPG", linkMade("FOLDER.JPG", "folder.jpg")},
		{"FOLDER.JPG made beside folder.jpg and names with no letter", linkMade("folder.jpg", "FOLDER.JPG", caseless(10)...)},
	} {
		if got := foldingIn(c.f, "/music"); got != CaseSensitive {
			t.Errorf("%s between the first read and the lookups: the folder answered %s, want %s", c.name, got, CaseSensitive)
		}
	}
}

// TestEachEntryAskedIsSettledByAListingReadAfterItsOwnLookups: the probe
// reads the listing again for each entry it asks, after that entry's own
// lookups, and never settles one by a listing read for an entry asked before
// it, as one re-read kept for the probe would, which is the first read's
// fault again (the brief's review, Codex and Carbon): a hard link's two
// names Abba and ABBA, Abba removed before the re-read made for it, which so
// settles nothing, and a hard link FOLDER.JPG made beside folder.jpg before
// folder.jpg's lookups, which only a listing read after them holds.
func TestEachEntryAskedIsSettledByAListingReadAfterItsOwnLookups(t *testing.T) {
	f := &probeFolder{
		listing: []string{"Abba", "folder.jpg", "ABBA"},
		ids:     map[string]int{"Abba": 1, "ABBA": 1, "folder.jpg": 2},
	}
	relisted, linked := false, false
	f.relist = func(f *probeFolder) {
		if !relisted {
			relisted = true
			f.listing = slices.DeleteFunc(f.listing, func(n string) bool { return n == "Abba" })
			delete(f.ids, "Abba")
		}
	}
	f.change = func(f *probeFolder, name string) {
		if name == "folder.jpg" && !linked {
			linked = true
			f.listing = append(f.listing, "FOLDER.JPG")
			f.ids["FOLDER.JPG"] = 2
		}
	}
	if got := foldingIn(f, "/music"); got != CaseSensitive {
		t.Errorf("the folder answered %s, want %s", got, CaseSensitive)
	}
	if want := []int{foldingProbeEntries, foldingProbeEntries + 1, foldingProbeEntries + 1}; !slices.Equal(f.reads, want) {
		t.Errorf("the reads of the listing asked for %v entries, want %v: the first read, then one for each of the two entries asked", f.reads, want)
	}
}

// TestAFolderReadWholeIsReadAgainForOneEntryMoreAtMost: where the probe's
// first read was all of the listing, its re-read after an entry's lookups
// takes at most foldingProbeEntries+1 entries, which tell a folder still
// within that bound from one that grew past it since (the brief's review,
// Codex and Carbon). A folder that grew past it is never read whole, and its
// re-read settles nothing: the next entry is asked, with a re-read of its
// own, so a folder that folds answers Unknown, and a hard link's second name
// made past the bound, which no read the probe makes holds, is never taken
// for absent (a probe that took that re-read for the whole listing answered
// case-insensitive). A folder grown to foldingProbeEntries entries is read
// whole by it, and its second name found.
func TestAFolderReadWholeIsReadAgainForOneEntryMoreAtMost(t *testing.T) {
	// grown is f with made added to its listing at the probe's first
	// lookup, each name of link a second name of the file folder.jpg names.
	grown := func(f *probeFolder, made []string, link ...string) *probeFolder {
		done := false
		f.change = func(f *probeFolder, _ string) {
			if !done {
				done = true
				f.listing = append(f.listing, made...)
				for _, n := range link {
					f.ids[n] = f.ids["folder.jpg"]
				}
			}
		}
		return f
	}
	linked := func() *probeFolder {
		return &probeFolder{listing: []string{"folder.jpg"}, ids: map[string]int{"folder.jpg": 1}}
	}
	reread := foldingProbeEntries + 1
	for _, c := range []struct {
		name  string
		f     *probeFolder
		want  CaseFolding
		reads []int
	}{
		{"a hard link's second name made with the folder grown to the bound",
			grown(linked(), append(caseless(foldingProbeEntries-2), "FOLDER.JPG"), "FOLDER.JPG"),
			CaseSensitive, []int{foldingProbeEntries, reread}},
		{"a hard link's second name made past the bound",
			grown(linked(), append(caseless(foldingProbeEntries), "FOLDER.JPG"), "FOLDER.JPG"),
			CaseUnknown, []int{foldingProbeEntries, reread}},
		{"a hard link's second name made past the bound, and an entry after that tells",
			grown(&probeFolder{listing: []string{"folder.jpg", "Band"}, ids: map[string]int{"folder.jpg": 1, "Band": 2}},
				append(caseless(foldingProbeEntries), "FOLDER.JPG"), "FOLDER.JPG"),
			CaseSensitive, []int{foldingProbeEntries, reread}},
		{"a folder that folds, grown past the bound",
			grown(&probeFolder{listing: []string{"Folder.jpg", "Band"}, ids: map[string]int{"Folder.jpg": 1, "Band": 2}, folds: true},
				caseless(foldingProbeEntries)),
			CaseUnknown, []int{foldingProbeEntries, reread, reread}},
	} {
		if got := foldingIn(c.f, "/music"); got != c.want {
			t.Errorf("%s: the folder answered %s, want %s", c.name, got, c.want)
		}
		if !slices.Equal(c.f.reads, c.reads) {
			t.Errorf("%s: the reads of the listing asked for %v entries, want %v", c.name, c.f.reads, c.reads)
		}
	}
}

// TestAHardLinkMadeBeforeTheLookupsOnThisHostsVolume: on this host's own
// volume, where it holds two names of one file that differ in case alone (a
// case-sensitive one: ext4, tmpfs, a case-sensitive APFS), a folder holding
// folder.jpg and its hard link FOLDER.JPG answers case-sensitive, and so does
// one where FOLDER.JPG is made between the probe's first read and its lookups
// (B738), read and looked up through the os package's calls
// (linkAtFirstLookup makes the link). It skips, saying why, where the volume
// makes no such name: one that folds case, or makes no hard links.
func TestAHardLinkMadeBeforeTheLookupsOnThisHostsVolume(t *testing.T) {
	dir := t.TempDir()
	lower, upper := filepath.Join(dir, "folder.jpg"), filepath.Join(dir, "FOLDER.JPG")
	if err := os.WriteFile(lower, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(lower, upper); err != nil {
		t.Skipf("this host's volume makes no second name of a file that differs in case alone: %v", err)
	}
	if got := FoldingIn(dir); got != CaseSensitive {
		t.Errorf("a folder holding folder.jpg and its hard link FOLDER.JPG answered %s, want %s", got, CaseSensitive)
	}
	if err := os.Remove(upper); err != nil {
		t.Fatal(err)
	}
	l := &linkAtFirstLookup{from: lower, to: upper}
	got := foldingIn(l, dir)
	if !l.made || l.err != nil {
		t.Fatalf("FOLDER.JPG was not made at the first lookup (looked up: %v): %v", l.made, l.err)
	}
	if got != CaseSensitive {
		t.Errorf("a folder whose FOLDER.JPG was made between the first read and the lookups answered %s, want %s", got, CaseSensitive)
	}
}

// linkAtFirstLookup is foldingFS over the os package, which makes the hard
// link to (a second name of the file from names) just before the probe's
// first lookup, after its first read.
type linkAtFirstLookup struct {
	osFolding
	from, to string
	made     bool
	err      error
}

func (l *linkAtFirstLookup) lstat(name string) (fs.FileInfo, error) {
	if !l.made {
		l.made = true
		l.err = os.Link(l.from, l.to)
	}
	return os.Lstat(name)
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
