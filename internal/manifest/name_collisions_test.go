package manifest

import (
	"slices"
	"strings"
	"testing"
)

// TestNameCollisionKeyIsTheRenameFold holds the name-collisions key to
// renameFold, the deletion pass's key: an ASCII name takes strings.ToLower,
// which must give renameFold's answer, and every other name takes renameFold.
// The first case holds every ASCII character, so a character the two
// lowercases map differently would show here.
func TestNameCollisionKeyIsTheRenameFold(t *testing.T) {
	var ascii strings.Builder
	for b := 0; b < 0x80; b++ {
		ascii.WriteByte(byte(b))
	}
	for _, name := range []string{
		ascii.String(),
		"01 Come Together.flac",
		"already lowercase",
		"",
		"Café",                // composed
		"Café",               // decomposed
		"CAFÉ",               // decomposed, upper case
		"İstanbul",            // capital I with a dot
		"Ρ̓",                  // lowercases to a sequence NFC composes
		"Straße",              // ß has no single-letter capital to meet
		"MIXED ascii and Été", // non-ASCII late in the name
	} {
		if got, want := nameCollisionKey(name), renameFold(name); got != want {
			t.Errorf("nameCollisionKey(%q) = %q, renameFold gives %q", name, got, want)
		}
	}
}

// TestFindNameCollisionsGroupsEverySpellingOfAName: three spellings of one
// album folder are one group of three, in byte order, and the tracks under
// them are no groups of their own.
func TestFindNameCollisionsGroupsEverySpellingOfAName(t *testing.T) {
	r := FindNameCollisions([]string{
		"Artist/Album/01 One.flac",
		"Artist/ALBUM/01 One.flac",
		"Artist/album/01 One.flac",
		"Artist/Other/01 One.flac",
	}, 5)
	if r.Paths != 4 || r.Groups != 1 || len(r.First) != 1 {
		t.Fatalf("got %+v, want 4 paths and one group", r)
	}
	if want := []string{"Artist/ALBUM", "Artist/Album", "Artist/album"}; !slices.Equal(r.First[0].Paths, want) {
		t.Errorf("group = %q, want %q", r.First[0].Paths, want)
	}
}

// TestFindNameCollisionsCountsANameMetTwiceOnce: a stale file row "x" beside
// the folder "x" that replaced it puts the name "x" in its folder's list
// twice, apart ("x b/…" sorts between them). That is one spelling, not a
// group. Nor are two of one name in different folders.
func TestFindNameCollisionsCountsANameMetTwiceOnce(t *testing.T) {
	r := FindNameCollisions([]string{
		"Album/x",
		"Album/x b/01.flac",
		"Album/x.flac",
		"Album/x/01.flac",
		"Other/x/01.flac",
	}, 5)
	if r.Groups != 0 {
		t.Errorf("got %d group(s) %+v, want none", r.Groups, r.First)
	}
}

// TestFindNameCollisionsAnswersAnUnsortedListAsASortedOne: the walk relies on
// byte order (a folder's paths are contiguous), so a list in another order is
// sorted first, and its caller's slice is left as it was.
func TestFindNameCollisionsAnswersAnUnsortedListAsASortedOne(t *testing.T) {
	paths := []string{
		"b/Two/01.flac",
		"a/One/01.flac",
		"b/two/01.flac",
		"a/ONE/01.flac",
		"b/Two/02.flac",
	}
	given := slices.Clone(paths)
	r := FindNameCollisions(paths, 5)
	if !slices.Equal(paths, given) {
		t.Errorf("the caller's slice was reordered: %q", paths)
	}
	if r.Groups != 2 || len(r.First) != 2 {
		t.Fatalf("got %+v, want two groups", r)
	}
	if !slices.Equal(r.First[0].Paths, []string{"a/ONE", "a/One"}) || !slices.Equal(r.First[1].Paths, []string{"b/Two", "b/two"}) {
		t.Errorf("groups = %q, %q", r.First[0].Paths, r.First[1].Paths)
	}
}

// TestFindNameCollisionsKeepsTheFirstGroupsInPathOrder: First is bounded
// and holds the groups that sort first, whichever order the walk settles
// them in: a folder's groups come out of a map, and a folder is settled after
// its subfolders, so the top-level pair, settled last, still comes first. A
// bound of zero names none and still counts every group.
func TestFindNameCollisionsKeepsTheFirstGroupsInPathOrder(t *testing.T) {
	var paths []string
	for _, n := range []string{"e", "d", "c", "b", "a"} {
		paths = append(paths, "Deep/Folder/"+n+".flac", "Deep/Folder/"+strings.ToUpper(n)+".flac")
	}
	paths = append(paths, "Aaa/01.flac", "AAA/01.flac")
	r := FindNameCollisions(paths, 3)
	if r.Groups != 6 {
		t.Fatalf("Groups = %d, want 6", r.Groups)
	}
	var firsts []string
	for _, g := range r.First {
		firsts = append(firsts, g.Paths[0])
	}
	if want := []string{"AAA", "Deep/Folder/A.flac", "Deep/Folder/B.flac"}; !slices.Equal(firsts, want) {
		t.Errorf("first paths = %q, want %q", firsts, want)
	}
	if r := FindNameCollisions(paths, 0); r.Groups != 6 || len(r.First) != 0 {
		t.Errorf("a bound of zero: got %d group(s), %d named, want 6 and none", r.Groups, len(r.First))
	}
}
