package librarycat

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestExpandNumericRefs — cases lifted from GenreNormalizer.swift's
// own docstring for expandNumericRefs.
func TestExpandNumericRefs(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"(17)", "Rock"},
		{"(17)Hard Rock", "Hard Rock"},
		{"(17)(79)", "Rock; Hard Rock"},
		{"((17)", "(17)"},                // "((" escape: literal paren starts the refinement
		{"(17) (79)", "Rock; Hard Rock"}, // taggers insert spaces between refs
		{"17", "Rock"},                   // ID3v2.4 bare numeric
		{"0", "Blues"},
		// Out of range → the literal stays.
		{"192", "192"},
		{"(192)", ""}, // parenthesised out-of-range ref is DROPPED
		// A bare numeric expands only when the WHOLE string is digits.
		{"1980s", "1980s"},
		{"80s Pop", "80s Pop"},
		{"Rock", "Rock"},
		{"", ""},
		{"   ", ""},
		{"Folk, World, & Country", "Folk, World, & Country"},
	} {
		if got := expandNumericRefs(tc.in); got != tc.want {
			t.Errorf("expandNumericRefs(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestGenreSegments pins the split rule, whose whole value is what it
// REFUSES to split on: a comma ("Folk, World, & Country" is one genre)
// and a slash ("Pop/Rock" is one label). Getting either wrong shatters
// a library's genre list into fragments.
func TestGenreSegments(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want []string
	}{
		{"Rock", []string{"Rock"}},
		{"Rock; Pop", []string{"Rock", "Pop"}},
		{"Rock;Pop", []string{"Rock", "Pop"}},
		{"Rock ; Pop", []string{"Rock", "Pop"}},
		{"Rock\x00Pop", []string{"Rock", "Pop"}},
		// Deduped by key within one call.
		{"Rock; rock", []string{"Rock"}},
		// NEVER split on these.
		{"Folk, World, & Country", []string{"Folk, World, & Country"}},
		{"Pop/Rock", []string{"Pop/Rock"}},
		// Numeric expansion feeds the split.
		{"(17)(79)", []string{"Rock", "Hard Rock"}},
		{"", nil},
		{";;", nil},
	} {
		got := displays(genreSegments(tc.in))
		if !eqStrings(got, tc.want) {
			t.Errorf("genreSegments(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestComposerSegments pins the one axis where "/" IS a separator
// (ID3v2.3 TCOM is a slash-separated composer list) — the mirror image
// of the genre rule, and a place where copying one rule to the other
// would be silently wrong in both directions.
func TestComposerSegments(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want []string
	}{
		{"Lennon/McCartney", []string{"Lennon", "McCartney"}},
		{"Bach, J.S.; Gounod, Charles", []string{"Bach, J.S.", "Gounod, Charles"}},
		{"Ludwig van Beethoven", []string{"Ludwig van Beethoven"}},
		{"", nil},
	} {
		got := displays(composerSegments(tc.in))
		if !eqStrings(got, tc.want) {
			t.Errorf("composerSegments(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
	// The inversion fold puts both tag forms in ONE group.
	a := composerGroupKey("Beethoven, Ludwig van")
	b := composerGroupKey("Ludwig van Beethoven")
	if a != b {
		t.Errorf("comma and natural forms must share a key: %q vs %q", a, b)
	}
	// Ensembles are never mangled — the "&" and " and " guards.
	for _, ensemble := range []string{"Crosby, Stills & Nash", "Peter, Paul and Mary"} {
		if _, ok := commaInvertedForm(ensemble); ok {
			t.Errorf("commaInvertedForm(%q) inverted an ensemble name", ensemble)
		}
	}
}

// TestComposerSortName — cases from the Swift docstring, INCLUDING the
// documented miss. "Ralph Vaughan Williams" buckets under W, not V;
// that is pinned as behaviour so a future change is a deliberate one.
func TestComposerSortName(t *testing.T) {
	for _, tc := range []struct {
		display  string
		variants []string
		want     string
	}{
		{"Ludwig van Beethoven", []string{"Ludwig van Beethoven"}, "Beethoven Ludwig van"},
		// A comma-form variant anywhere in the group wins VERBATIM.
		{"Ludwig van Beethoven", []string{"Beethoven, Ludwig van", "Ludwig van Beethoven"},
			"Beethoven, Ludwig van"},
		{"Johann Strauss II", []string{"Johann Strauss II"}, "Strauss Johann II"},
		{"John Williams Jr.", []string{"John Williams Jr."}, "Williams John Jr."},
		// Documented accepted miss: multi-word surname, no comma form.
		{"Ralph Vaughan Williams", []string{"Ralph Vaughan Williams"}, "Williams Ralph Vaughan"},
		{"Prince", []string{"Prince"}, "Prince"},
		{"", nil, ""},
	} {
		if got := composerSortName(tc.display, tc.variants); got != tc.want {
			t.Errorf("composerSortName(%q, %v) = %q, want %q",
				tc.display, tc.variants, got, tc.want)
		}
	}
}

// TestID3GenreTableMatchesDhowden re-extracts the table from the
// dependency's SOURCE and compares. A hand-copied 192-entry table is
// exactly the kind of thing that is wrong in one entry and never
// noticed; and because the bridge's own MP3 extraction expands through
// dhowden's table, a silent divergence would mean a reference expanded
// here groups differently from the same file's manifest value.
func TestID3GenreTableMatchesDhowden(t *testing.T) {
	out, err := exec.Command("go", "env", "GOMODCACHE").Output()
	if err != nil {
		t.Skipf("go env GOMODCACHE: %v", err)
	}
	modBytes, err := os.ReadFile(filepath.Join("..", "..", "go.mod"))
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	m := regexp.MustCompile(`(?m)^\s*(github\.com/dhowden/tag)\s+(\S+)`).FindStringSubmatch(string(modBytes))
	if m == nil {
		t.Skip("dhowden/tag not found in go.mod")
	}
	src, err := os.ReadFile(filepath.Join(strings.TrimSpace(string(out)),
		"github.com", "dhowden", "tag@"+m[2], "id3v2.go"))
	if err != nil {
		t.Skipf("read dhowden id3v2.go (module cache may be cold): %v", err)
	}
	block := regexp.MustCompile(`(?s)var id3v2Genres = \[\.\.\.\]string\{(.*?)\n\}`).FindSubmatch(src)
	if block == nil {
		t.Fatal("could not locate id3v2Genres in dhowden/tag — if upstream reshaped it, " +
			"update this test rather than deleting it: it is the only thing keeping " +
			"our copy honest")
	}
	lits := regexp.MustCompile(`"((?:[^"\\]|\\.)*)"`).FindAllStringSubmatch(string(block[1]), -1)
	if len(lits) != len(id3GenreTable) {
		t.Fatalf("dhowden table has %d entries, ours has %d", len(lits), len(id3GenreTable))
	}
	for i, lit := range lits {
		if lit[1] != id3GenreTable[i] {
			t.Errorf("genre[%d]: ours %q, dhowden %q", i, id3GenreTable[i], lit[1])
		}
	}
}

func displays(segs []Segment) []string {
	if len(segs) == 0 {
		return nil
	}
	out := make([]string, len(segs))
	for i, s := range segs {
		out[i] = s.Display
	}
	return out
}

func eqStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
