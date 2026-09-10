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
	cited, defined := scanTestCitations(t, repoRootForCitations(t))

	// Vacuous-pass guards on BOTH sides: a walk that stops finding files, or a
	// regex that stops matching, reports no problems — the one outcome that
	// hides the drift.
	if len(defined) < 200 {
		t.Fatalf("only %d test functions found — the walk or the regex is broken", len(defined))
	}
	if len(cited) < 5 {
		t.Fatalf("only %d cited test names found — the scan is broken", len(cited))
	}

	for _, m := range missingCitations(cited, defined) {
		t.Errorf("no test function of this name exists: %s\n"+
			"A docblock naming a guard is a claim about what is checked. Either write it, "+
			"or name the test that actually covers the invariant.", m)
	}
}

// citedRe matches a citation: `Test` + an uppercase letter + the rest.
//
// The `\b` is what keeps it off `Test`-shaped substrings of larger identifiers
// — in `SetTestHashCost` and `allowTestAssetHost` the character before `Test`
// is a word character, so there is no boundary and no match. Verified rather
// than assumed; an explicit whole-identifier re-check was written first and was
// dead code. (Gemini on #897.)
//
// No minimum length: an earlier `{5,}` silently ignored every name shorter than
// ten characters — TestApply, TestLogin, TestParse — which is a large and
// ordinary slice of what a docblock might cite.
var citedRe = regexp.MustCompile(`\bTest[A-Z][A-Za-z0-9_]*`)

var definedRe = regexp.MustCompile(`(?m)^func (Test[A-Za-z0-9_]+)\(`)

// scanTestCitations walks the tree once, collecting names cited from non-test
// source and names defined in _test.go files.
func scanTestCitations(t *testing.T, root string) (cited map[string][]string, defined map[string]bool) {
	t.Helper()
	cited, defined = map[string][]string{}, map[string]bool{}
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
	return cited, defined
}

// missingCitations returns the cited names nothing defines, formatted with the
// files that cite them.
//
// A citation may name a PREFIX of the real function (a table-driven parent, or
// a name truncated at a line wrap). That is accepted: the point is that
// something of that name exists to run.
func missingCitations(cited map[string][]string, defined map[string]bool) []string {
	var missing []string
	for name, files := range cited {
		if defined[name] || definesWithPrefix(defined, name) {
			continue
		}
		missing = append(missing, name+"  (cited by "+strings.Join(unique(files), ", ")+")")
	}
	sort.Strings(missing)
	return missing
}

func definesWithPrefix(defined map[string]bool, name string) bool {
	for def := range defined {
		if strings.HasPrefix(def, name) {
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
