package fsutil

import (
	"os"
	"path/filepath"
	"testing"
)

// B741's round 4 (CI on #1441, red on macOS and Windows): a recorded spelling
// of a data dir can run through an alias of its ancestors, a link whose
// target lies at another depth, as macOS's /var is (/private/var, one folder
// deeper) and as a Windows short (8.3) name stands for its long one. The
// comparison resolved the spellings and then laid the record's letters, one a
// folder of the recorded spelling, over the resolved one, so a spelling
// through another link, or by the real path, read as another data dir. Each
// case records a path through an alias and compares it with a spelling that
// resolves alike, with the letter that decides in both directions: a folder
// recorded as folding case takes a case-only difference for one path, and the
// same folder recorded as keeping case does not, which shows the letter
// stayed on its folder.

// aliasLinks makes the links a case needs, each at link and leading to
// target, skipping the case, saying why, where this host makes none.
func aliasLinks(t *testing.T, links map[string]string) {
	t.Helper()
	for link, target := range links {
		if err := os.MkdirAll(target, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, link); err != nil {
			t.Skipf("this host makes no link here: %v", err)
		}
	}
}

// lettersOf is a probe stand-in answering, for each folder letters names,
// its letter, and CaseSensitive for every other.
func lettersOf(letters map[string]CaseFolding) func(string) CaseFolding {
	return func(dir string) CaseFolding {
		if f, ok := letters[dir]; ok {
			return f
		}
		return CaseSensitive
	}
}

// TestARecordedPathComparesThroughAnAliasOfItsAncestors: a path recorded
// through an alias whose target is deeper (`a`, a link to `x/real`) or
// shallower (`p/q/b`, a link to `shallow`), its leaf and the folders below the
// alias not there (a volume away), compared with the spelling by the alias's
// target: the folder whose letter decides is the recorded one, below the
// alias, counted from the leaf, whatever depth the alias's target has; with a
// case-only difference in the leaf, in a folder between the alias and the
// leaf, or in several with their letters mixed. Recorded by the real path and
// compared through the alias is a guard, which compared alike before, the
// temporary directory taken by its real path so that it is one: macOS spells
// that directory through /var, a link to /private/var (one folder deeper),
// which TestSamePathAsRecordedProbesNoVolume compares through as the host
// spells it. A folding as long as the resolved spelling's folders, not the
// recorded one's, is no folding of the recorded path: each of its folders is
// CaseUnknown.
func TestARecordedPathComparesThroughAnAliasOfItsAncestors(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	deepAlias, deepTarget := filepath.Join(base, "a"), filepath.Join(base, "x", "real")
	shallowAlias, shallowTarget := filepath.Join(base, "p", "q", "b"), filepath.Join(base, "shallow")
	aliasLinks(t, map[string]string{deepAlias: deepTarget, shallowAlias: shallowTarget})
	j := filepath.Join
	const (
		I = CaseInsensitive
		S = CaseSensitive
	)
	mixed := map[string]CaseFolding{deepAlias: S, j(deepAlias, "One"): I, j(deepAlias, "One", "Two"): S}
	for _, tc := range []struct {
		name          string
		recorded, now string
		letters       map[string]CaseFolding
		same          bool
	}{
		{"a deeper target's alias, the leaf's folder folding case",
			j(deepAlias, "Bridge"), j(deepTarget, "bridge"), map[string]CaseFolding{deepAlias: I}, true},
		{"a deeper target's alias, the leaf's folder keeping case",
			j(deepAlias, "Bridge"), j(deepTarget, "bridge"), map[string]CaseFolding{deepAlias: S}, false},
		{"a shallower target's alias, the leaf's folder folding case",
			j(shallowAlias, "Bridge"), j(shallowTarget, "bridge"), map[string]CaseFolding{shallowAlias: I}, true},
		{"a shallower target's alias, the leaf's folder keeping case",
			j(shallowAlias, "Bridge"), j(shallowTarget, "bridge"), map[string]CaseFolding{shallowAlias: S}, false},
		{"a case-only difference between the alias and the leaf, its folder folding case",
			j(deepAlias, "Sub", "Bridge"), j(deepTarget, "sub", "Bridge"), map[string]CaseFolding{deepAlias: I}, true},
		{"a case-only difference between the alias and the leaf, its folder keeping case",
			j(deepAlias, "Sub", "Bridge"), j(deepTarget, "sub", "Bridge"), map[string]CaseFolding{deepAlias: S}, false},
		{"mixed letters, the difference in the folder that folds",
			j(deepAlias, "One", "Two", "Bridge"), j(deepTarget, "One", "two", "Bridge"), mixed, true},
		{"mixed letters, the difference in the alias's own folder, which keeps case",
			j(deepAlias, "One", "Two", "Bridge"), j(deepTarget, "one", "Two", "Bridge"), mixed, false},
		{"mixed letters, the difference in the leaf's folder, which keeps case",
			j(deepAlias, "One", "Two", "Bridge"), j(deepTarget, "One", "Two", "bridge"), mixed, false},
		{"mixed letters, a difference where it folds and one where it keeps case",
			j(deepAlias, "One", "Two", "Bridge"), j(deepTarget, "One", "two", "bridge"), mixed, false},
		{"recorded by the real path, compared through the alias, folding case (a guard)",
			j(deepTarget, "Bridge"), j(deepAlias, "bridge"), map[string]CaseFolding{deepTarget: I}, true},
		{"recorded by the real path, compared through the alias, keeping case (a guard)",
			j(deepTarget, "Bridge"), j(deepAlias, "bridge"), map[string]CaseFolding{deepTarget: S}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			folding := FoldingOnTheWay(tc.recorded, lettersOf(tc.letters), map[string]CaseFolding{})
			if got := SamePathAsRecorded(tc.recorded, tc.now, folding); got != tc.same {
				t.Errorf("SamePathAsRecorded(%q, %q, %s) = %v, want %v", tc.recorded, tc.now, FormatFolding(folding), got, tc.same)
			}
		})
	}

	t.Run("a folding as long as the resolved spelling's folders", func(t *testing.T) {
		recorded, now := j(deepAlias, "Bridge"), j(deepTarget, "bridge")
		resolved := FoldingOnTheWay(j(deepTarget, "Bridge"), lettersOf(nil), map[string]CaseFolding{})
		if len(resolved) == len(FoldingOnTheWay(recorded, lettersOf(nil), map[string]CaseFolding{})) {
			t.Fatalf("the alias's target is as deep as the alias: the case tests nothing")
		}
		if !SamePathAsRecorded(recorded, now, resolved) {
			t.Errorf("SamePathAsRecorded(%q, %q, %s) = false: a folding of another spelling's folders says nothing of the recorded one's",
				recorded, now, FormatFolding(resolved))
		}
	})
}
