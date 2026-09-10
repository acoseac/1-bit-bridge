package main

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestEveryCitedTestNameExists is the mechanical version of a class this tree
// keeps producing, and it found six live instances when it was written.
//
// A docblock that names a test is making a claim about what is guarded — and
// this repo reads those claims as the reason the code beside them is safe to
// change. Five of the six were harmless drift (a case typo, a rename); two
// stood in for an invariant nothing pinned at all, and one was a false safety
// claim on a security boundary: managed_controls.go said a test "walks the
// registered routes so a new one cannot be added without a decision", and no
// test of that name had ever existed.
//
// The same shape as the stale-claim warnings CLAUDE.md already carries, except
// this one runs. It is deliberately whole-tree rather than per-package: the
// citations cross package boundaries (a comment in internal/manifest naming a
// test in internal/admin is legitimate), so a per-package scan would report
// every one of those as missing.
func TestEveryCitedTestNameExists(t *testing.T) {
	root := repoRootForCitations(t)
	cited := map[string][]string{} // test name -> files citing it
	defined := map[string]bool{}

	// A cited name is one that appears in NON-test source. A defined one is a
	// `func TestX(` in a _test.go file. Both anchored on the identifier, never
	// on surrounding prose.
	citedRe := regexp.MustCompile(`\bTest[A-Z][A-Za-z0-9_]{5,}`)
	definedRe := regexp.MustCompile(`(?m)^func (Test[A-Za-z0-9_]+)\(`)

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Vendored or generated trees have their own conventions and are
			// not ours to police.
			if n := d.Name(); n == ".git" || n == "dist" || n == "bin" || n == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		// CRLF-normalised: nothing pins eol, so a Windows checkout would make
		// the `(?m)^func` anchor and every literal below find nothing.
		src := strings.ReplaceAll(string(raw), "\r\n", "\n")
		if strings.HasSuffix(path, "_test.go") {
			for _, m := range definedRe.FindAllStringSubmatch(src, -1) {
				defined[m[1]] = true
			}
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		for _, name := range citedRe.FindAllString(src, -1) {
			cited[name] = append(cited[name], rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// Vacuous-pass guards on BOTH sides: a walk that stops finding files, or a
	// regex that stops matching, reports no problems — the one outcome that
	// hides the drift.
	if len(defined) < 200 {
		t.Fatalf("only %d test functions found — the walk or the regex is broken", len(defined))
	}
	if len(cited) < 5 {
		t.Fatalf("only %d cited test names found — the scan is broken", len(cited))
	}

	var missing []string
	for name, files := range cited {
		if defined[name] {
			continue
		}
		// A citation may name a PREFIX of the real function (a table-driven
		// parent, or a name truncated at a line wrap). Accept that: the point
		// is that something of that name exists to run.
		var prefixed bool
		for def := range defined {
			if strings.HasPrefix(def, name) {
				prefixed = true
				break
			}
		}
		if prefixed {
			continue
		}
		// Nor is an identifier that merely CONTAINS a Test-shaped word (
		// SetTestHashCost, allowTestAssetHost) a citation. Those are
		// substrings of a larger identifier, so require a word boundary before
		// the match in at least one citing file.
		if !citedAsAWholeIdentifier(t, root, files, name) {
			continue
		}
		missing = append(missing, name+"  (cited by "+strings.Join(unique(files), ", ")+")")
	}
	sort.Strings(missing)
	for _, m := range missing {
		t.Errorf("no test function of this name exists: %s\n"+
			"A docblock naming a guard is a claim about what is checked. Either write it, "+
			"or name the test that actually covers the invariant.", m)
	}
}

// citedAsAWholeIdentifier reports whether `name` appears in any of `files` with
// a non-identifier character before it — i.e. as a citation rather than as the
// tail of a longer identifier like SetTestHashCost.
func citedAsAWholeIdentifier(t *testing.T, root string, files []string, name string) bool {
	t.Helper()
	re := regexp.MustCompile(`(^|[^A-Za-z0-9_])` + regexp.QuoteMeta(name))
	for _, f := range unique(files) {
		raw, err := os.ReadFile(filepath.Join(root, f))
		if err != nil {
			t.Fatal(err)
		}
		if re.Match(raw) {
			return true
		}
	}
	return false
}

func unique(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// repoRootForCitations walks up from the package directory to the module root.
func repoRootForCitations(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("no go.mod found above the package directory")
	return ""
}
