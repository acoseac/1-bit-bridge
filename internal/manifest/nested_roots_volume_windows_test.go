//go:build windows

package manifest

import (
	"slices"
	"testing"
)

// TestAVolumeRootHoldsThePathsBelowIt: a library root at a share's root,
// \\server\share with its separator or without it, holds every path below
// it, as a drive's root C:\ does, for which root a path is and for the roots
// nested in it. Configured without its separator, its first element was
// \\server\share where the paths below it begin \\server\share\, so it held
// none of them: its subtree scans were refused and the roots nested in it went
// unseen (B651's ninth round, Codex on round 8). C: alone, a drive's current
// folder, stays apart from C:\. Lexical alone: no element past the volume
// differs in case, so no folder is asked how it compares names, and the share
// need not be there.
func TestAVolumeRootHoldsThePathsBelowIt(t *testing.T) {
	for _, c := range []struct {
		path string
		want []string
	}{
		{`\\server\share`, []string{`\\server\share\`}},
		{`\\server\share\`, []string{`\\server\share\`}},
		{`\\server\share\Album`, []string{`\\server\share\`, `Album`}},
		{`C:\`, []string{`C:\`}},
		{`C:\Album`, []string{`C:\`, `Album`}},
		{`C:`, []string{`C:`}},
	} {
		if got := pathElements(c.path); !slices.Equal(got, c.want) {
			t.Errorf("pathElements(%q) = %q, want %q", c.path, got, c.want)
		}
	}

	s := &Scanner{}
	look := s.caseLook()
	for _, c := range []struct {
		dir, abs, tail string
		ok             bool
	}{
		{`\\server\share`, `\\server\share\Album`, `Album`, true},
		{`\\server\share\`, `\\server\share\Album`, `Album`, true},
		{`\\server\share`, `\\server\share\Album\Disc 1`, `Album\Disc 1`, true},
		{`\\server\share`, `\\server\share`, `.`, true},
		{`\\server\share`, `\\server\share\`, `.`, true},
		{`\\server\share\`, `\\server\share`, `.`, true},
		{`\\SERVER\Share`, `\\server\share\Album`, `Album`, true},
		{`\\server\share`, `\\server\other\Album`, ``, false},
		{`C:\`, `C:\Album`, `Album`, true},
		{`C:\`, `C:\`, `.`, true},
		{`C:\`, `D:\Album`, ``, false},
		{`C:`, `C:\Album`, ``, false},
	} {
		for _, spare := range []bool{false, true} {
			if tail, ok := look.under(c.dir, c.abs, spare); ok != c.ok || tail != c.tail {
				t.Errorf("%s under %s (what is spared: %v): %q, %v; want %q, %v", c.abs, c.dir, spare, tail, ok, c.tail, c.ok)
			}
		}
	}

	share, classical := `\\server\share`, `\\server\share\Classical`
	roots := []string{share, classical}
	for _, c := range []struct{ abs, root, tail string }{
		{`\\server\share\Rock\Album`, share, `Rock\Album`},
		{`\\server\share\Classical\Bach`, classical, `Bach`},
		{`\\server\share`, share, `.`},
	} {
		if root, _, tail := s.deepestRootOf(roots, c.abs); root != c.root || tail != c.tail {
			t.Errorf("%s is %q's, below it %q; want %q's, below it %q", c.abs, root, tail, c.root, c.tail)
		}
	}
	if got := s.nestedRootsUnder(roots, share); len(got) != 1 || got[0].root != classical || got[0].tail != `Classical` {
		t.Errorf("the roots nested in %s are %+v, want %s alone, below it Classical", share, got, classical)
	}
}
