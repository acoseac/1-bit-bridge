package fsutil

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// fakeEntry is one entry of a fakeVolume: a directory, a file or a link (to
// target, a slash path below the root), with an identity sameFile compares,
// and other names the volume opens it under (a Windows short name, say) that
// no fold relates to its own.
type fakeEntry struct {
	name     string
	id       int
	mode     fs.FileMode
	target   string
	aliases  []string
	children []*fakeEntry
}

// fakeVolume is a spellingSys over a tree held in memory. folding makes it
// open a name under any spelling that folds to it (case and Unicode
// composition), as APFS does; without it a name opens only as listed, or
// through an alias. It counts the calls a Speller makes, by directory.
type fakeVolume struct {
	root       string
	top        *fakeEntry
	folding    bool
	unlistable map[string]bool // directories (slash paths below root) whose listing fails
	listed     map[string]int  // directories listed, by slash path below root
	lstats     int
}

func newFakeVolume(folding bool) *fakeVolume {
	return &fakeVolume{
		root:       filepath.FromSlash("/lib"),
		top:        &fakeEntry{name: "lib", id: 1, mode: fs.ModeDir},
		folding:    folding,
		unlistable: map[string]bool{},
		listed:     map[string]int{},
	}
}

// add makes the entries of rel (slash-separated), each a directory unless it
// is the last, which takes mode, and returns the last.
func (v *fakeVolume) add(rel string, mode fs.FileMode, aliases ...string) *fakeEntry {
	cur := v.top
	segs := strings.Split(rel, "/")
	for i, seg := range segs {
		var next *fakeEntry
		for _, c := range cur.children {
			if c.name == seg {
				next = c
			}
		}
		if next == nil {
			next = &fakeEntry{name: seg, id: v.nextID(), mode: fs.ModeDir}
			cur.children = append(cur.children, next)
		}
		if i == len(segs)-1 {
			next.mode = mode
			next.aliases = aliases
		}
		cur = next
	}
	return cur
}

// addLink makes rel a link to target, a slash path below the root.
func (v *fakeVolume) addLink(rel, target string) {
	v.add(rel, fs.ModeSymlink).target = target
}

func (v *fakeVolume) nextID() int {
	n := 0
	var walk func(e *fakeEntry)
	walk = func(e *fakeEntry) {
		n++
		for _, c := range e.children {
			walk(c)
		}
	}
	walk(v.top)
	return n + 1
}

// relOf maps a path the Speller built back to slash segments below root.
func (v *fakeVolume) relOf(name string) ([]string, error) {
	rel, err := filepath.Rel(v.root, name)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, &fs.PathError{Op: "lstat", Path: name, Err: syscall.ENOENT}
	}
	if rel == "." {
		return nil, nil
	}
	return strings.Split(filepath.ToSlash(rel), "/"), nil
}

// lookup opens the entry at name as a kernel does, matching a child by its
// listed name, else by an alias, else (folding) by its fold: a link above the
// last component is followed, the last component is not (Lstat), unless
// follow (an open, which a listing makes).
func (v *fakeVolume) lookup(name string, follow bool) (*fakeEntry, error) {
	segs, err := v.relOf(name)
	if err != nil {
		return nil, err
	}
	return v.walk(v.top, segs, follow, 0, name)
}

func (v *fakeVolume) walk(cur *fakeEntry, segs []string, follow bool, depth int, name string) (*fakeEntry, error) {
	if depth > 8 {
		return nil, &fs.PathError{Op: "lstat", Path: name, Err: syscall.ELOOP}
	}
	for i, seg := range segs {
		if !cur.mode.IsDir() {
			return nil, &fs.PathError{Op: "lstat", Path: name, Err: syscall.ENOTDIR}
		}
		next := v.child(cur, seg)
		if next == nil {
			return nil, &fs.PathError{Op: "lstat", Path: name, Err: syscall.ENOENT}
		}
		if next.mode&fs.ModeSymlink != 0 && (i < len(segs)-1 || follow) {
			resolved, err := v.walk(v.top, strings.Split(next.target, "/"), true, depth+1, name)
			if err != nil {
				return nil, err
			}
			next = resolved
		}
		cur = next
	}
	return cur, nil
}

func (v *fakeVolume) child(dir *fakeEntry, seg string) *fakeEntry {
	for _, c := range dir.children {
		if c.name == seg {
			return c
		}
	}
	for _, c := range dir.children {
		for _, a := range c.aliases {
			if a == seg {
				return c
			}
		}
	}
	if v.folding {
		for _, c := range dir.children {
			if spellingFold(cases.Fold(), c.name) == spellingFold(cases.Fold(), seg) {
				return c
			}
		}
	}
	return nil
}

func (v *fakeVolume) lstat(name string) (fs.FileInfo, error) {
	v.lstats++
	e, err := v.lookup(name, false)
	if err != nil {
		return nil, err
	}
	return fakeInfo{e}, nil
}

func (v *fakeVolume) names(dir string, fn func(string) bool) error {
	e, err := v.lookup(dir, true)
	if err != nil {
		return err
	}
	segs, _ := v.relOf(dir)
	key := strings.Join(segs, "/")
	if !e.mode.IsDir() {
		return &fs.PathError{Op: "open", Path: dir, Err: syscall.ENOTDIR}
	}
	if v.unlistable[key] {
		return &fs.PathError{Op: "open", Path: dir, Err: syscall.EACCES}
	}
	v.listed[key]++
	for _, c := range e.children {
		if !fn(c.name) {
			return nil
		}
	}
	return nil
}

func (v *fakeVolume) sameFile(a, b fs.FileInfo) bool {
	return a.(fakeInfo).e.id == b.(fakeInfo).e.id
}

func (v *fakeVolume) speller() *Speller {
	return newSpeller(v.root, v)
}

type fakeInfo struct{ e *fakeEntry }

func (i fakeInfo) Name() string       { return i.e.name }
func (i fakeInfo) Size() int64        { return 0 }
func (i fakeInfo) Mode() fs.FileMode  { return i.e.mode }
func (i fakeInfo) ModTime() time.Time { return time.Time{} }
func (i fakeInfo) IsDir() bool        { return i.e.mode.IsDir() }
func (i fakeInfo) Sys() any           { return nil }

// TestSpellerReadsTheSpellingTheListingCarries: on a volume that opens a name
// under any spelling of its case and composition, a path comes back as the
// listings spell it, component by component, the file included.
func TestSpellerReadsTheSpellingTheListingCarries(t *testing.T) {
	nfc, nfd := norm.NFC.String("Café"), norm.NFD.String("Café")
	for _, tc := range []struct{ name, disk, asked string }{
		{"as listed", "Artist/Album/01.flac", "Artist/Album/01.flac"},
		{"case", "Artist/Album/01.flac", "artist/ALBUM/01.FLAC"},
		{"composed asked, decomposed listed", nfd + "/Album", nfc + "/album"},
		{"decomposed asked, composed listed", nfc + "/Album", nfd + "/Album"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := newFakeVolume(true)
			v.add(tc.disk, 0)
			got, err := v.speller().Spell(tc.asked)
			if err != nil {
				t.Fatalf("Spell(%+q): %v", tc.asked, err)
			}
			if got != tc.disk {
				t.Errorf("Spell(%+q) = %+q, want %+q", tc.asked, got, tc.disk)
			}
		})
	}
}

// TestSpellerMatchesByIdentityNotByName: a name the fold relates to the
// asked spelling is a CANDIDATE, never the answer. On a case-sensitive
// volume "artist" and "Artist" are two folders, and each is spelled as
// itself; where the asked spelling opens one entry (through an alias) while
// another entry's name folds like it, the answer is the entry it opens.
func TestSpellerMatchesByIdentityNotByName(t *testing.T) {
	v := newFakeVolume(false)
	v.add("Artist/Album", fs.ModeDir)
	v.add("artist/Album", fs.ModeDir)
	for _, asked := range []string{"Artist/Album", "artist/Album"} {
		if got, err := v.speller().Spell(asked); err != nil || got != asked {
			t.Errorf("Spell(%q) = %q, %v; want it as asked: a case-sensitive volume lists both", asked, got, err)
		}
	}

	v = newFakeVolume(false)
	v.add("artist", fs.ModeDir)
	v.add("Real Name", fs.ModeDir, "ARTIST")
	if got, err := v.speller().Spell("ARTIST"); err != nil || got != "Real Name" {
		t.Errorf("Spell(ARTIST) = %q, %v; want %q, the entry ARTIST opens, not %q, whose name folds like it",
			got, err, "Real Name", "artist")
	}
}

// TestSpellerFindsANameTheFoldDoesNotRelate: a volume that opens an entry
// under a name no fold relates to its own (a Windows 8.3 short name) is
// answered by identity over the whole listing.
func TestSpellerFindsANameTheFoldDoesNotRelate(t *testing.T) {
	v := newFakeVolume(true)
	v.add("Other", fs.ModeDir)
	v.add("Long Artist Name/Album", fs.ModeDir)
	v.top.children[1].aliases = []string{"LONGAR~1"}
	got, err := v.speller().Spell("LONGAR~1/Album")
	if err != nil || got != "Long Artist Name/Album" {
		t.Errorf("Spell(LONGAR~1/Album) = %q, %v; want %q", got, err, "Long Artist Name/Album")
	}
}

// TestSpellerAsksNoStatOfAPathAsListed: the common answer, a path the client
// spelled as listed, costs one Lstat per component and no comparison, even
// where (a case-sensitive volume) a sibling's name folds like it.
func TestSpellerAsksNoStatOfAPathAsListed(t *testing.T) {
	v := newFakeVolume(false)
	v.add("artist/album/01.FLAC", 0)
	v.add("Artist/album/01.FLAC", 0)
	v.add("Artist/Album/01.FLAC", 0)
	v.add("Artist/Album/01.flac", 0)
	if got, err := v.speller().Spell("Artist/Album/01.flac"); err != nil || got != "Artist/Album/01.flac" {
		t.Fatalf("Spell = %q, %v", got, err)
	}
	if v.lstats != 3 {
		t.Errorf("%d Lstats for a three-component path spelled as listed, want 3", v.lstats)
	}
}

// TestSpellerRefusesWhatAWalkDoesNotDescend: a path through a link to a
// directory, or through a file, has no on-disk spelling, and the link's
// target is never listed; the LAST component is spelled whatever it is.
func TestSpellerRefusesWhatAWalkDoesNotDescend(t *testing.T) {
	v := newFakeVolume(true)
	v.add("Real/Album/01.flac", 0)
	v.addLink("Linked", "Real")
	v.add("file.flac", 0)
	for _, asked := range []string{"Linked/Album", "linked/Album/01.flac", "file.flac/x"} {
		if got, err := v.speller().Spell(asked); !errors.Is(err, errNotSpelled) {
			t.Errorf("Spell(%q) = %q, %v; want errNotSpelled: a walk does not descend it", asked, got, err)
		}
	}
	if n := v.listed["Linked"]; n != 0 {
		t.Errorf("the link was listed %d times, want never", n)
	}
	if got, err := v.speller().Spell("LINKED"); err != nil || got != "Linked" {
		t.Errorf("Spell(LINKED) = %q, %v; want %q: the last component is spelled whatever it is", got, err, "Linked")
	}
}

// TestSpellerRefusals: a path that names nothing, and one below a directory
// that cannot be listed, are refused, with an error that names the path
// library-relative, never the root. A path that is not one name per
// component is refused before anything is stat'd: on Windows a backslash or
// a volume in a component is a path to filepath.Join, which can name an
// entry outside the directory.
func TestSpellerRefusals(t *testing.T) {
	v := newFakeVolume(true)
	v.add("Artist/Album/01.flac", 0)
	v.unlistable["Artist"] = true
	malformed := []string{"a//b", "./a", "a/../b", "/abs", "a/", "..", "Artist\x00/Album"}
	if filepath.Separator != '/' {
		malformed = append(malformed, `Artist\..\..\outside`, "C:outside", "Artist/C:")
	}
	for _, asked := range append([]string{"Nobody/Album", "artist/album/01.flac"}, malformed...) {
		_, err := v.speller().Spell(asked)
		if !errors.Is(err, errNotSpelled) {
			t.Errorf("Spell(%q): %v, want errNotSpelled", asked, err)
			continue
		}
		if strings.Contains(err.Error(), v.root) {
			t.Errorf("Spell(%q): %q names the root %q", asked, err, v.root)
		}
	}
	v.lstats = 0
	for _, asked := range malformed {
		_, _ = v.speller().Spell(asked)
	}
	if v.lstats != 0 {
		t.Errorf("%d Lstats for paths that are not one name per component, want none", v.lstats)
	}
	for _, asked := range []string{"", "."} {
		if got, err := v.speller().Spell(asked); err != nil || got != asked {
			t.Errorf("Spell(%q) = %q, %v; want it back: it is the root", asked, got, err)
		}
	}
}

// TestSpellerListsAFolderOnceForABatch: paths in one folder resolve the
// folders above them once per Speller (the folder holding a path's last
// component is listed for each path), and a folder it has resolved answers
// for itself, asked as a path's last component, with no listing at all.
func TestSpellerListsAFolderOnceForABatch(t *testing.T) {
	v := newFakeVolume(true)
	v.add("Artist/Album/01.flac", 0)
	v.add("Artist/Album/02.flac", 0)
	v.add("Artist/Other/03.flac", 0)
	sp := v.speller()
	for _, asked := range []string{"artist/album/01.flac", "artist/album/02.flac", "artist/other/03.flac"} {
		if _, err := sp.Spell(asked); err != nil {
			t.Fatal(err)
		}
	}
	if v.listed[""] != 1 || v.listed["Artist"] != 2 || v.listed["Artist/Album"] != 2 {
		t.Errorf("listed %v, want the root once, Artist once per folder below it, "+
			"and Artist/Album once per file in it", v.listed)
	}
	lstats := v.lstats
	if got, err := sp.Spell("artist/album"); err != nil || got != "Artist/Album" {
		t.Errorf("Spell(artist/album) = %q, %v; want Artist/Album", got, err)
	}
	if v.listed["Artist"] != 2 || v.lstats != lstats {
		t.Errorf("a folder the batch resolved, asked as a last component, listed %v and took %d Lstats, want neither",
			v.listed, v.lstats-lstats)
	}
}

// TestSpellerOnThisVolume runs the real filesystem: a path as listed comes
// back as it is everywhere, and where the volume opens another spelling of
// it (macOS: case and composition; Windows: case) that spelling comes back
// as listed. A path through a link to a directory is refused.
func TestSpellerOnThisVolume(t *testing.T) {
	root := t.TempDir()
	disk := filepath.Join(root, "Artist", norm.NFC.String("Café"))
	if err := os.MkdirAll(disk, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(disk, "01.flac"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	want := "Artist/" + norm.NFC.String("Café") + "/01.flac"
	if got, err := NewSpeller(root).Spell(want); err != nil || got != want {
		t.Errorf("Spell(%+q) = %+q, %v; want it as asked", want, got, err)
	}
	for _, asked := range []string{
		"ARTIST/" + norm.NFC.String("CAFÉ") + "/01.FLAC",
		"Artist/" + norm.NFD.String("Café") + "/01.flac",
	} {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(asked))); err != nil {
			t.Logf("this volume does not open %+q: %v", asked, err)
			continue
		}
		if got, err := NewSpeller(root).Spell(asked); err != nil || got != want {
			t.Errorf("Spell(%+q) = %+q, %v; want %+q", asked, got, err, want)
		}
	}
	if err := os.Symlink(filepath.Join(root, "Artist"), filepath.Join(root, "Linked")); err != nil {
		t.Skipf("cannot make a symlink here: %v", err)
	}
	if got, err := NewSpeller(root).Spell("Linked/" + norm.NFC.String("Café")); !errors.Is(err, errNotSpelled) {
		t.Errorf("Spell through a link = %+q, %v; want errNotSpelled", got, err)
	}
}
