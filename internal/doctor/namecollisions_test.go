package doctor

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestCheckNameCollisions pins the check's answers: no probe wired (skip), a
// probe that fails (warn: "don't know" is not "fine"), no groups (ok, with
// what was compared), and groups (warn, naming the sample and counting the
// rest). A managed bridge still runs it: the library is its user's.
func TestCheckNameCollisions(t *testing.T) {
	ctx := context.Background()
	probe := func(nc NameCollisions, err error) func(context.Context) (NameCollisions, error) {
		return func(context.Context) (NameCollisions, error) { return nc, err }
	}

	t.Run("no probe skips", func(t *testing.T) {
		c := checkNameCollisions(ctx, Deps{})
		if c.Status != OK || !strings.Contains(c.Summary, "no manifest") {
			t.Fatalf("got %+v, want ok/no manifest", c)
		}
	})
	t.Run("probe error warns rather than answering ok", func(t *testing.T) {
		c := checkNameCollisions(ctx, Deps{NameCollisions: probe(NameCollisions{}, errors.New("database is locked"))})
		if c.Status != Warn || !strings.Contains(c.Hint, "database is locked") {
			t.Fatalf("got %+v, want warn carrying the error", c)
		}
	})
	t.Run("no groups is ok and says what it compared", func(t *testing.T) {
		c := checkNameCollisions(ctx, Deps{NameCollisions: probe(NameCollisions{Paths: 25000}, nil)})
		if c.Status != OK || !strings.HasPrefix(c.Summary, "25000 track path(s) under the library roots; ") || c.Hint != "" {
			t.Fatalf("got %+v, want ok naming 25000 paths and no hint", c)
		}
	})
	t.Run("groups warn, the sample is named and the rest counted", func(t *testing.T) {
		c := checkNameCollisions(ctx, Deps{NameCollisions: probe(NameCollisions{
			Paths: 9, Groups: 3,
			Sample: [][]string{{"Artist", "artist"}, {"Album/AB.flac", "Album/Ab.flac", "Album/ab.flac"}},
		}, nil)})
		if c.Status != Warn || !strings.HasPrefix(c.Summary, "3 group(s) ") {
			t.Fatalf("got %+v, want a warn counting three groups", c)
		}
		for _, want := range []string{
			`Groups: "Artist" and "artist"; "Album/AB.flac", "Album/Ab.flac" and "Album/ab.flac" (+1 more)`,
			"ignoring case and Unicode composition",
		} {
			if !strings.Contains(c.Hint, want) {
				t.Errorf("hint lacks %q:\n%s", want, c.Hint)
			}
		}
	})
	t.Run("managed still runs", func(t *testing.T) {
		c := checkNameCollisions(ctx, Deps{Managed: true, NameCollisions: probe(NameCollisions{
			Paths: 2, Groups: 1, Sample: [][]string{{"Artist", "artist"}},
		}, nil)})
		if c.Status != Warn {
			t.Fatalf("got %+v, want the warn an unmanaged bridge gets", c)
		}
	})
}

// TestDescribeNameCollisionSaysWhichSpellingIsComposed: a composed and a
// decomposed spelling print alike, so where a group mixes the two each path
// says which it is, judged by its last component (the parent is the same
// for every path of a group, composed or not). A group of one form says
// nothing of composition, and a control character is quoted, not printed.
func TestDescribeNameCollisionSaysWhichSpellingIsComposed(t *testing.T) {
	const decomposedParent = "Bjo\u0308rk"
	for _, tc := range []struct {
		name  string
		paths []string
		want  string
	}{
		{"mixed", []string{decomposedParent + "/Cafe\u0301", decomposedParent + "/Caf\u00e9"},
			`"` + decomposedParent + `/Cafe` + "\u0301" + `" (decomposed) and "` + decomposedParent + `/Caf` + "\u00e9" + `" (composed)`},
		{"one form", []string{"CAFE\u0301", "cafe\u0301"}, `"CAFE` + "\u0301" + `" and "cafe` + "\u0301" + `"`},
		{"control character", []string{"Album\n", "album\n"}, `"Album\n" and "album\n"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := describeNameCollision(tc.paths); got != tc.want {
				t.Errorf("got  %s\nwant %s", got, tc.want)
			}
		})
	}
}
