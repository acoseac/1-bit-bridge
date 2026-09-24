package main

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
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
	cited, defined, mdCited := scanTestCitations(t, repoRootForCitations(t))

	// Vacuous-pass guards on BOTH sides: a walk that stops finding files, or a
	// regex that stops matching, reports no problems — the one outcome that
	// hides the drift.
	if len(defined) < 200 {
		t.Fatalf("only %d test functions found — the walk or the regex is broken", len(defined))
	}
	if len(cited) < 5 {
		t.Fatalf("only %d cited test names found — the scan is broken", len(cited))
	}

	// The markdown half needs its own floor for the reason the two sides
	// already have theirs: the Go citations alone clear the checks above, so a
	// `.md` branch that stopped matching would report a clean tree.
	if mdCited < 50 {
		t.Fatalf("only %d cited test names found in .md files — the markdown branch is broken", mdCited)
	}

	for _, m := range missingCitations(cited, defined) {
		t.Errorf("no test function of this name exists: %s\n"+
			"A docblock naming a guard is a claim about what is checked. Either write it, "+
			"or name the test that actually covers the invariant.", m)
	}
}

// TestScanTestCitationsReadsTestFileCommentsOnly pins the test-file branch
// on a fixture of its own rather than on whatever the tree happens to
// contain: the whole-tree run passed vacuously against a branch that skipped
// test files (the non-test citations alone clear the floor), and it caught a
// raw-source scan only because some string literal in the tree happens to
// spell a test name. One temporary tree with one non-test citation, one
// test-file COMMENT citation, one test-shaped string LITERAL and one real
// definition — the comment must be collected, the literal must not, and the
// two ghosts must be what missingCitations reports. (CodeRabbit on #921.)
func TestScanTestCitationsReadsTestFileCommentsOnly(t *testing.T) {
	root := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("prod.go", "package x\n\n// Guarded by TestGhostFromProd.\nfunc f() {}\n")
	write("x_test.go", "package x\n\nimport \"testing\"\n\n"+
		"// Pinned by TestGhostFromComment, which nothing defines.\n"+
		"func TestReal(t *testing.T) {\n\tua := \"TestGhostInLiteral/1.0\"\n\t_ = ua\n}\n")

	cited, defined, _ := scanTestCitations(t, root)
	if !defined["TestReal"] {
		t.Errorf("the fixture's real test was not collected as defined: %v", defined)
	}
	if defined["TestGhostInLiteral"] {
		// Definitions come from FuncDecls only; a literal that spells a test
		// name must not satisfy a citation of it (CodeRabbit on #922).
		t.Errorf("a test-shaped string literal was collected as a definition: %v", defined)
	}
	if _, ok := cited["TestGhostFromComment"]; !ok {
		t.Error("a citation in a _test.go COMMENT was not collected — the test-file branch is not reading comments")
	}
	if _, ok := cited["TestGhostFromProd"]; !ok {
		t.Error("a citation in non-test source was not collected")
	}
	if files, ok := cited["TestGhostInLiteral"]; ok {
		t.Errorf("a test-shaped STRING LITERAL in a _test.go file was collected as a citation (%v) — the branch is scanning source, not comments", files)
	}
	if files, ok := cited["TestReal"]; ok {
		// The definition's own name appears only in CODE here, never in a comment.
		t.Errorf("the defined test's name was collected from test CODE: %v", files)
	}
	// Exact strings, file names included: a citation attributed to the wrong
	// file would pass a prefix check (Gemini on #922).
	want := []string{
		"TestGhostFromComment  (cited by x_test.go)",
		"TestGhostFromProd  (cited by prod.go)",
	}
	missing := missingCitations(cited, defined)
	if len(missing) != len(want) {
		t.Fatalf("missingCitations = %v, want %v", missing, want)
	}
	for i, m := range missing {
		if m != want[i] {
			t.Errorf("missingCitations[%d] = %q, want %q", i, m, want[i])
		}
	}
}

// TestScanTestCitationsAppliesTheMarkdownPolicy pins the `.md` branch on a
// fixture, for the reason its sibling above records: a whole-tree run passes
// vacuously against a branch that collects nothing, because the Go citations
// alone clear every floor that was there before.
//
// Four names in one temporary tree, one per rule — an ordinary citation that
// must be collected, a plan-document name that must not, a metasyntactic
// placeholder that must not, and the conductor repo's test that must not —
// plus the count the floor reads, which must be exactly the one.
func TestScanTestCitationsAppliesTheMarkdownPolicy(t *testing.T) {
	root := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// One placeholder and the foreign-repo name are taken from the maps
	// themselves, so this stays correct if either list is edited.
	var placeholder string
	for n := range mdPlaceholderNames {
		placeholder = n
		break
	}
	var foreign string
	for n := range mdForeignRepoTests {
		foreign = n
		break
	}
	write("notes.md", "Pinned by TestGhostFromMarkdown.\n"+
		"Run it with -run "+placeholder+".\n"+
		"The proxy is pinned by "+foreign+".\n")
	write("ops/plan-something.md", "Will be pinned by TestGhostInAPlan.\n")
	write("x_test.go", "package x\n\nimport \"testing\"\n\nfunc TestReal(t *testing.T) { _ = t }\n")

	cited, _, mdCited := scanTestCitations(t, root)

	if _, ok := cited["TestGhostFromMarkdown"]; !ok {
		t.Error("an ordinary .md citation was not collected — the markdown branch is not reading files")
	}
	if files, ok := cited["TestGhostInAPlan"]; ok {
		t.Errorf("a plan document's name was collected (%v) — ops/plan-*.md enumerates "+
			"tests still to be written and is not claiming they exist", files)
	}
	if files, ok := cited[placeholder]; ok {
		t.Errorf("the placeholder %s was collected from an example command line: %v", placeholder, files)
	}
	if files, ok := cited[foreign]; ok {
		t.Errorf("%s belongs to another repository and cannot be verified here, "+
			"but was collected: %v", foreign, files)
	}
	if mdCited != 1 {
		t.Errorf("mdCited = %d, want exactly 1 (only TestGhostFromMarkdown) — the "+
			"count the vacuous-pass floor reads must not include the exempted names", mdCited)
	}
}

// TestScanTestCitationsOpensOnlyWhatGoBuildsOrGitTracks plants, one per row,
// a file an editor or a checkout leaves beside the ones this guard scans, and
// requires the scan to finish as if it were not there.
//
// Each row failed while the walk selected files by suffix alone. Emacs locks
// a file it is editing with `.#<name>` beside it, as a DANGLING symlink where
// it can and as a REGULAR file holding the lock string where it cannot
// (always on Windows): the first failed os.ReadFile, the second failed
// parser.ParseFile. An untracked doc was opened and only then discarded, so
// one that cannot be opened failed the run. A tree with no `.git` (a
// fixture, or a source archive) has no tracked set to exclude a lock beside
// a doc. And a test in a file whose name begins with "_" was counted as
// defined, although the go tool never compiles it, so a docblock citing it
// passed.
func TestScanTestCitationsOpensOnlyWhatGoBuildsOrGitTracks(t *testing.T) {
	// A lock's contents, as emacs writes them: user@host.pid:boot.
	const lockData = "someone@host.1:1"
	// symlink plants a dangling symlink, the shape emacs's lock takes where
	// it can make one. A host that cannot create symlinks (Windows without
	// the privilege) skips the row rather than fail: emacs cannot make its
	// symlink lock there either, and the regular-file row covers the lock
	// such a host does see.
	symlink := func(t *testing.T, target, path string) {
		t.Helper()
		if err := os.Symlink(target, path); err != nil {
			t.Skipf("cannot create a symlink on this host (%v); emacs writes its "+
				"regular-file lock here, which the regular-file row covers", err)
		}
	}
	rows := []struct {
		name string
		// notCheckout scans the tree as one with no `.git`: no tracked set,
		// so every doc in it is in scope.
		notCheckout bool
		plant       func(t *testing.T, root string)
	}{
		{"an emacs lock beside a Go file, as a dangling symlink", false, func(t *testing.T, root string) {
			symlink(t, lockData, filepath.Join(root, ".#prod.go"))
		}},
		{"an emacs lock beside a test file, as a regular file", false, func(t *testing.T, root string) {
			if err := os.WriteFile(filepath.Join(root, ".#x_test.go"), []byte(lockData), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"a test in a file the go tool ignores", false, func(t *testing.T, root string) {
			body := "package x\n\nimport \"testing\"\n\nfunc TestParkedNeverRuns(t *testing.T) { _ = t }\n"
			if err := os.WriteFile(filepath.Join(root, "_parked_test.go"), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"an untracked doc that cannot be opened", false, func(t *testing.T, root string) {
			symlink(t, "gone.md", filepath.Join(root, "local.md"))
		}},
		{"an emacs lock beside a tracked doc", false, func(t *testing.T, root string) {
			symlink(t, lockData, filepath.Join(root, ".#notes.md"))
		}},
		{"an emacs lock beside a doc, outside a git checkout", true, func(t *testing.T, root string) {
			symlink(t, lockData, filepath.Join(root, ".#notes.md"))
		}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			root := t.TempDir()
			write := func(name, body string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			// The same small tree under every row: one real test, one citation
			// of it from each side, and one citation of the parked test, which
			// exists only in the row that plants it and never runs in any.
			write("prod.go", "package x\n\n// Guarded by TestReal and by TestParkedNeverRuns.\nfunc f() {}\n")
			write("x_test.go", "package x\n\nimport \"testing\"\n\nfunc TestReal(t *testing.T) { _ = t }\n")
			write("notes.md", "Pinned by TestReal.\n")
			row.plant(t, root)

			tracked := map[string]bool{"notes.md": true}
			if row.notCheckout {
				tracked = nil
			}
			cited, defined, mdCited := scanTestCitationsIn(t, root, tracked)

			if !defined["TestReal"] {
				t.Errorf("the tree's real test was not collected as defined: %v", defined)
			}
			if !slices.Contains(cited["TestReal"], "notes.md") || mdCited != 1 {
				t.Errorf("the tracked doc was not scanned (cited by %v, mdCited = %d) — "+
					"the markdown half must still read what git tracks", cited["TestReal"], mdCited)
			}
			want := []string{"TestParkedNeverRuns  (cited by prod.go)"}
			if got := missingCitations(cited, defined); !slices.Equal(got, want) {
				t.Errorf("missingCitations = %q, want %q — a test the go tool never "+
					"compiles cannot satisfy a citation", got, want)
			}
		})
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
// ten characters — …Apply, …Login, …Parse — which is a large and
// ordinary slice of what a docblock might cite.
//
// The `Test` prefix is ELIDED on those three, per the rule #946 set for a
// note that must NAME a token it is only talking about: this file's comment
// groups are scanned like any other docblock, so spelling them in full made
// the guard collect three citations of its own prose. They passed, which is
// worse than failing — each was satisfied by definesWithPrefix against
// eighty-eight, eighteen and fifteen unrelated tests.
var citedRe = regexp.MustCompile(`\bTest[A-Z][A-Za-z0-9_]*`)

// The markdown docs are scanned too, and they need three exemptions that Go
// source does not. Each is a real category, not a convenience:
//
//   - **Plan documents** (`ops/plan-*.md`) enumerate tests still TO BE
//     WRITTEN. Thirty of the forty stale names in the tree when this was
//     extended were in one such file, and every one of them was correct as
//     written: a plan naming its intended coverage is not claiming the
//     coverage exists.
//   - **Illustrative placeholders.** The docs carry example command lines —
//     a `-run` argument standing for "whichever test you are chasing" — and
//     AGENTS.md describes the PATTERN as "when a section below says 'pinned
//     by <name>'". Those three metasyntactic names are listed in the map
//     below and deliberately NOT spelled in this comment: this file is a
//     `_test.go`, so its comment groups are scanned like any other docblock,
//     and naming them here would report them as missing. (The map entries
//     are code, which the test-file branch does not read.) A real test of
//     one of those names would go uncited; that is the price, and at three
//     and seven characters the trade is obvious.
//   - **Tests in another repository.** The deployment runbook cites the
//     conductor repo's guard on its haproxy config. It is a true citation
//     and unverifiable from here, so the name carries the repo that owns it
//     rather than being quietly dropped.
//
// Everything else in a `.md` file is treated exactly like a docblock, because
// it is read the same way — and `CLAUDE.md` is the file this harness loads on
// every session, so a stale guard name there is the most expensive kind.
var (
	mdPlaceholderNames = map[string]bool{
		"TestX": true, "TestFoo": true, "TestBar": true,
		// The two single-letter forms, from a `-run '^(…|…)$'` example
		// in the engineering log. They reached this list only when
		// somebody looked on purpose, because definesWithPrefix
		// answered first: a five-character citation prefixes hundreds
		// of this module's real test names, so it was "verified" by
		// matching almost everything. See that function's docblock —
		// which describes them rather than spelling them, for the
		// reason it gives there.
		"TestA": true, "TestB": true,
	}
	mdForeignRepoTests = map[string]string{
		"TestTenantsStayPassthroughOn443": "1-bit-conductor (private)",
	}
)

// trackedMarkdownSet returns the `.md` paths git reports as tracked, or nil
// meaning "no ignore rules to honour, scan everything".
//
// This exists because several ops documents are GITIGNORED — `ops/*audit*.md`,
// `ops/coordinates.local.md`, the bug reviews — and they differ from machine to
// machine. A scan that read them would answer differently in different
// checkouts: the 2026-08-06 audit on this machine cites a test that has since
// been split in two, so the guard failed here and would have passed on CI and
// on a fresh clone. A test whose verdict depends on untracked local files is
// not a guard.
//
// The discriminator is `.git`, and each branch fails in the honest direction. A
// root with no `.git` is a fixture's temp directory, which has no ignore rules,
// so everything in it is in scope. A root WITH `.git` must get an answer from
// git; if git is absent or errors there, that is a broken environment and the
// test says so rather than quietly widening its scope back to every file.
func trackedMarkdownSet(t *testing.T, root string) map[string]bool {
	t.Helper()
	// Lstat, and ONLY fs.ErrNotExist returns nil. Any other error — a
	// permission failure, an I/O error, a dangling `.git` symlink — is not
	// evidence that there are no ignore rules to honour, and treating it as
	// such silently disables the filter and puts gitignored docs back in
	// scope: the exact environment-dependence this function exists to remove,
	// one level up. Lstat rather than Stat so a dangling symlink fails here
	// instead of reading as absent. (CodeRabbit, PR #946.)
	if _, err := os.Lstat(filepath.Join(root, ".git")); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil // not a checkout: a fixture tree, nothing to filter
		}
		t.Fatalf("stat %s: %v — cannot tell whether this root has ignore rules, and "+
			"guessing either way makes this test answer differently in different "+
			"checkouts", filepath.Join(root, ".git"), err)
	}
	// A missing git binary is named as such rather than left as a bare exec
	// failure: it is the one cause an operator can act on directly. It is NOT
	// a t.Skip — a skipped guard looks exactly like a passing one, and this
	// one's whole job is to stop the scope silently widening. (Gemini, #946.)
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatalf("git is not on PATH (%v), but %s is a checkout — the markdown scan "+
			"cannot tell tracked docs from gitignored local ones without it, and "+
			"scanning both makes this test answer differently in different checkouts",
			err, root)
	}
	out, err := exec.Command("git", "-C", root, "ls-files", "-z", "--", "*.md").Output()
	if err != nil {
		t.Fatalf("git ls-files in %s: %v — the markdown scan cannot tell tracked "+
			"docs from gitignored local ones without it, and scanning both makes "+
			"this test answer differently in different checkouts", root, err)
	}
	set := map[string]bool{}
	for _, name := range strings.Split(string(out), "\x00") {
		if name != "" {
			set[filepath.ToSlash(name)] = true
		}
	}
	if len(set) == 0 {
		t.Fatalf("git reported no tracked .md files under %s — the scan would be vacuous", root)
	}
	return set
}

// isPlanDoc reports whether rel is a plan document, whose test names are
// intentions rather than claims.
func isPlanDoc(rel string) bool {
	return strings.HasPrefix(filepath.ToSlash(rel), "ops/plan-")
}

// scanTestCitations is scanTestCitationsIn over the docs git tracks under
// root: the entry point the whole-tree guard and the fixtures share.
func scanTestCitations(t *testing.T, root string) (cited map[string][]string, defined map[string]bool, mdCited int) {
	t.Helper()
	return scanTestCitationsIn(t, root, trackedMarkdownSet(t, root))
}

// scanTestCitationsIn walks the tree once, collecting names defined in
// _test.go files and names cited from non-test source AND from the COMMENTS
// of test files. trackedMD is trackedMarkdownSet's answer, taken as a
// parameter so a fixture can pin the markdown half without a git checkout.
//
// Test files were skipped entirely at first, and fifteen stale citations sat
// in their docblocks — a renamed sibling named under its old name, a
// historical note naming a test that no longer exists, and one first
// sentence claiming a property the test beneath it does not pin. A test file
// is parsed ONCE and both halves come off the AST: definitions from its
// top-level `Test…` FuncDecls, citations from its comment groups and nothing
// else — a test file's CODE names tests legitimately, and its string
// literals can hold anything (a User-Agent value spelled like a test name,
// say — one does). Gemini on #921 folded the two passes into one.
func scanTestCitationsIn(t *testing.T, root string, trackedMD map[string]bool) (cited map[string][]string, defined map[string]bool, mdCited int) {
	t.Helper()
	cited, defined = map[string][]string{}, map[string]bool{}
	mdCitations := map[string]bool{}
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
		rel, _ := filepath.Rel(root, path)
		if !opensForCitations(d.Name(), rel, trackedMD) {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		// CRLF-normalised: nothing pins eol, so a Windows checkout would make
		// the `(?m)^func` anchor and every literal below find nothing.
		src := strings.ReplaceAll(string(raw), "\r\n", "\n")
		switch {
		case strings.HasSuffix(path, ".md"):
			collectMarkdownCitations(rel, src, cited, mdCitations)
			return nil
		case strings.HasSuffix(path, "_test.go"):
			return collectTestFileCitations(path, rel, src, cited, defined)
		default:
			for _, name := range citedRe.FindAllString(src, -1) {
				cited[name] = append(cited[name], rel)
			}
			return nil
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	// Returned rather than stashed in a package var: two tests call this, and
	// shared mutable state between them is a seam nobody asked for.
	return cited, defined, len(mdCitations)
}

// opensForCitations reports whether the walk opens a file at all, decided
// from its NAME before anything is read. Each half takes the rule that
// already says what it owns:
//
//   - A doc is opened only if git tracks it (trackedMarkdownSet). The walk
//     used to open every `.md` and discard an untracked one AFTER reading
//     it, so a gitignored local doc that could not be opened failed the
//     run over a file it was never going to scan, and so did emacs's
//     `.#CLAUDE.md` while CLAUDE.md was open. Nor is a doc opened whose
//     name begins with ".", an editor's or the OS's: a tree with no `.git`
//     (a fixture, or a source archive) has no tracked set, and a lock
//     there is still not a doc. Not "_" as well, as the go tool would: a
//     tracked `_name.md` is a real doc.
//   - A Go file is opened unless the go tool ignores it (goToolIgnores): an
//     editor's lock beside it, or a file no build compiles.
//
// No DIRECTORY rule is added. `.github/` holds a tracked doc and a tracked
// Go file this guard reads, so the go tool's `.`-directory rule would drop
// both.
func opensForCitations(name, rel string, trackedMD map[string]bool) bool {
	switch {
	case strings.HasSuffix(name, ".md"):
		if strings.HasPrefix(name, ".") {
			return false
		}
		return trackedMD == nil || trackedMD[filepath.ToSlash(rel)]
	case strings.HasSuffix(name, ".go"):
		return !goToolIgnores(name)
	}
	return false
}

// collectMarkdownCitations applies the `.md` policy declared above to a
// document the walk has opened: skip a plan, and skip the two exempt name
// classes. Whether it is opened at all (git must track it) is
// opensForCitations's call. Split out of the walk for SonarCloud go:S3776 —
// the callback had grown to a cognitive complexity of 50 against the 15
// allowed, most of it nesting rather than logic.
func collectMarkdownCitations(rel, src string, cited map[string][]string, mdCitations map[string]bool) {
	if isPlanDoc(rel) {
		return
	}
	for _, name := range citedRe.FindAllString(src, -1) {
		if mdPlaceholderNames[name] {
			continue
		}
		if _, foreign := mdForeignRepoTests[name]; foreign {
			continue
		}
		cited[name] = append(cited[name], rel)
		mdCitations[name] = true
	}
}

// collectTestFileCitations takes definitions from a test file's top-level
// `Test…` FuncDecls and citations from its comment groups ONLY — its code
// names tests legitimately, and its string literals can hold anything.
func collectTestFileCitations(path, rel, src string, cited map[string][]string, defined map[string]bool) error {
	f, err := parser.ParseFile(token.NewFileSet(), path, src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return err
	}
	for _, decl := range f.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && strings.HasPrefix(fn.Name.Name, "Test") {
			defined[fn.Name.Name] = true
		}
	}
	var comments strings.Builder
	for _, g := range f.Comments {
		comments.WriteString(g.Text())
		comments.WriteByte('\n')
	}
	for _, name := range citedRe.FindAllString(comments.String(), -1) {
		cited[name] = append(cited[name], rel)
	}
	return nil
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

// definesWithPrefix is what accepts a citation that names a PREFIX of the
// real function: a table-driven parent (`TestArtwork` for
// `TestArtwork/ArtistImageReturns404…`, where the regex stops at the `/`),
// or a name truncated at a line wrap. Both are ordinary in a docblock and
// both point at something that exists, which is all this guard claims.
//
// **It does not verify a SHORT name, and cannot.** A citation is accepted
// when it prefixes ANY defined test, so the shorter it is the more certainly
// it passes: measured over this module, the two single-letter placeholders
// in mdPlaceholderNames prefix 392 and 131 real test names respectively.
// They are exempted there by name rather than caught here, and that is
// deliberate — the obvious alternative, a minimum length, was tried and
// reverted (see citedRe: it silently ignored every name under ten
// characters, and real tests are that short), and requiring
// equality-or-`name+"_"` would reject ten legitimate truncations in the
// tree today.
//
// The two are DESCRIBED rather than spelled, and that is this rule biting
// its own author: mdPlaceholderNames is consulted only by
// collectMarkdownCitations, so writing them out in a Go docblock collects
// them as ordinary citations — which then pass through the very masking
// this paragraph is about. Same elision the citedRe note above uses, same
// reason. (Gemini on #958.)
//
// So the rule to keep is the one the exemption map encodes: a citation too
// short to identify anything is a PLACEHOLDER, and belongs in that map where
// a reader can see it, not passing quietly through here.
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
