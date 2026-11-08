package fsutil

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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
// spells it; so is recorded by the real path with its last folder in another
// case (`x/REAL`, not there where the volume keeps case), compared through
// the alias, which off Windows only the comparison with now as it resolves
// takes. A
// folding as long as the resolved spelling's folders, not the recorded
// one's, is no folding of the recorded path: each of its folders is
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
	// The target's folder spelled in another case: on Windows the folder that
	// is there, whatever the letter (TestACaseOnlyDifferenceInAFolderThatIsThere).
	x, upper := j(base, "x"), j(base, "x", "REAL")
	_, err = os.Stat(upper)
	oneFolder := runtime.GOOS == "windows" && err == nil
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
		{"recorded in another case than the folder the alias leads to, its folder folding case (a guard)",
			j(upper, "Bridge"), j(deepAlias, "bridge"), map[string]CaseFolding{x: I, upper: I}, true},
		{"recorded in another case than the folder the alias leads to, its folder keeping case (a guard)",
			j(upper, "Bridge"), j(deepAlias, "bridge"), map[string]CaseFolding{x: S, upper: I}, oneFolder},
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

// TestARecordedPathComparesAsWindowsResolvesIt stands in, on any host, for
// how Windows resolves a path (resolveLinks there): a folder that is there is
// named by its long name in the case its volume keeps, whatever spelling
// reaches it, its short (8.3) name or one in another case, and what is not
// there does not resolve. A path recorded by a folder's short name and
// compared by its long one, or the reverse, is judged by the letter of the
// folder the leaf is looked up in, never read as two folders; a folder that is
// there, spelled in another case, is one folder whatever the letter of the
// folder above it; a difference below it, not there, is the letters'.
// TestARecordedPathComparesThroughAShortName and
// TestACaseOnlyDifferenceInAFolderThatIsThere ask Windows itself.
func TestARecordedPathComparesAsWindowsResolvesIt(t *testing.T) {
	j := filepath.Join
	root := volumeRoot()
	users := j(root, "Users")
	long, short := j(users, "runneradmin"), j(users, "RUNNER~1")
	mnt, shortMnt := j(long, "usb"), j(short, "usb")
	// Each folder that is there, by each spelling that reaches it, as the
	// volume folds it, and its name on disk.
	there := map[string]string{}
	for _, f := range [][2]string{{root, root}, {users, users}, {long, long}, {short, long}, {mnt, mnt}, {shortMnt, mnt}} {
		there[strings.ToLower(f[0])] = f[1]
	}
	windows := func(p string) (string, error) {
		if onDisk, ok := there[strings.ToLower(p)]; ok {
			return onDisk, nil
		}
		return "", &fs.PathError{Op: "resolve", Path: p, Err: fs.ErrNotExist}
	}
	const (
		I = CaseInsensitive
		S = CaseSensitive
	)
	for _, tc := range []struct {
		name          string
		recorded, now string
		letters       map[string]CaseFolding
		same          bool
	}{
		{"recorded by the short name, the leaf's folder folding case",
			j(shortMnt, "Bridge"), j(mnt, "bridge"), map[string]CaseFolding{shortMnt: I}, true},
		{"recorded by the short name, the leaf's folder keeping case",
			j(shortMnt, "Bridge"), j(mnt, "bridge"), map[string]CaseFolding{shortMnt: S}, false},
		{"compared by the short name in another case, the leaf's folder folding case",
			j(mnt, "Bridge"), j(users, "runner~1", "USB", "bridge"), map[string]CaseFolding{mnt: I}, true},
		{"compared by the short name in another case, the leaf's folder keeping case",
			j(mnt, "Bridge"), j(users, "runner~1", "USB", "bridge"), map[string]CaseFolding{mnt: S}, false},
		{"a folder that is there in another case, its folder recorded as keeping case",
			j(mnt, "Bridge"), j(long, "USB", "Bridge"), map[string]CaseFolding{long: S}, true},
		{"a case-only difference below the folder that is there, its folder keeping case",
			j(mnt, "Sub", "Bridge"), j(long, "USB", "SUB", "Bridge"), map[string]CaseFolding{long: S, mnt: S}, false},
		{"a case-only difference below the folder that is there, its folder folding case",
			j(mnt, "Sub", "Bridge"), j(long, "USB", "SUB", "Bridge"), map[string]CaseFolding{long: S, mnt: I}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			folding := FoldingOnTheWay(tc.recorded, lettersOf(tc.letters), map[string]CaseFolding{})
			if got := sameAbsAsRecorded(tc.recorded, tc.now, folding, windows); got != tc.same {
				t.Errorf("sameAbsAsRecorded(%q, %q, %s) as Windows resolves = %v, want %v", tc.recorded, tc.now, FormatFolding(folding), got, tc.same)
			}
		})
	}
}
