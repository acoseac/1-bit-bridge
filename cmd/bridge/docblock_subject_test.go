package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"unicode"
)

// docVerbs are the words a Go doc comment in this tree puts after the name of
// the declaration it documents: "X is …", "X returns …", "X bounds …".
//
// The list is deliberately CLOSED. Matching "<Ident> <anything>" would flag
// every sentence that happens to open with a declared name, and the shape
// being caught is specifically a doc comment whose opening sentence has a
// declaration as its SUBJECT. The one such false positive in the tree when
// this was measured is a const group documented by a noun phrase
// ("eventBroker fan-out cardinality").
//
// It is also deliberately DERIVED, not recalled. #964 hand-picked 31 verbs,
// and a census on 2026-09-24 found they covered 61% of the openers this
// tree's correctly-attached doc comments use: 28 of the 36 misattached
// blocks it found opened with a verb outside them ("bootstrapTranscodeCmd
// runs …", "GetTrack fetches …", "Extract reads …"). A word is listed when
// at least five correctly-attached doc comments open with it, or when it
// opened one of those 36. Four of #964's picks meet neither bar (does,
// provides, takes, tracks) and are kept, since removing a verb can only
// lose recall. docVerbCoverageFloor keeps the list from quietly falling
// behind the vocabulary again.
//
// That census read non-test files only, and so does this list. Test files
// add testDocVerbs to it.
var docVerbs = []string{
	"accepts", "adapts", "adds", "answers", "appends", "applies", "are",
	"asks", "assembles", "attaches", "binds", "blocks", "bounds", "builds",
	"bundles", "caps", "captures", "carries", "checks", "classifies",
	"clears", "compares", "composes", "computes", "configures",
	"constructs", "controls", "converts", "copies", "counts", "covers",
	"creates", "decides", "decodes", "deletes", "derives", "describes",
	"dispatches", "does", "downloads", "drains", "drives", "drops",
	"emits", "encodes", "enforces", "enqueues", "enumerates", "escapes",
	"exposes", "extracts", "fetches", "fills", "filters", "finds", "fires",
	"flattens", "folds", "gates", "governs", "groups", "handles", "holds",
	"identifies", "implements", "inserts", "installs", "is", "keeps",
	"lets", "lists", "locates", "logs", "makes", "maps", "marks",
	"matches", "means", "mirrors", "names", "opens", "orders", "overrides",
	"owns", "paces", "pairs", "parses", "performs", "persists", "picks",
	"points", "polls", "prints", "probes", "processes", "provides",
	"queries", "queues", "reads", "rebuilds", "records", "reduces",
	"refuses", "rejects", "removes", "renders", "replaces", "reports",
	"resolves", "returns", "rewrites", "runs", "satisfies", "scans",
	"selects", "sends", "serializes", "serves", "signals", "sleeps",
	"splits", "stamps", "stops", "stores", "streams", "strips", "surfaces",
	"takes", "tracks", "transitions", "trims", "turns", "updates",
	"validates", "verifies", "walks", "wipes", "wires", "wraps", "writes",
}

// testDocVerbs are the words a _test.go file's doc comments put after their
// own name that docVerbs lacks: a test's doc opens "… pins the …" or
// "… asserts …", and a fixture's "newWatcherFixture stands up …". A test
// file is read against docVerbs and this list together, because its helpers,
// fakes and fixtures are documented like any other code.
//
// Derived by docVerbs' rule over the test files' own census (2026-09-24,
// #990): a word is listed when at least five correctly-attached test-file
// doc comments open with it, or when it opened one of the eleven misattached
// blocks that census found ("swaps" and "slices" opened one each and clear
// five nowhere). "regression" clears five and is not a verb: `\b` splits
// "regression-guards" at the hyphen. It stays out, as "and", "atomically"
// and "re" stayed out of docVerbs.
//
// It is kept apart rather than merged into docVerbs because the vocabulary
// is the test files' own. "pins" opens 1,033 of their 2,843 subject-first
// openers and 2 of the 4,522 outside them; these twenty open 16 of those
// 4,522 in all. Non-test files are therefore read exactly as #989 measured
// them, and docVerbs alone recognises 48% of the test files' openers.
var testDocVerbs = []string{
	"asserts", "confirms", "exercises", "extends", "forces", "guards",
	"lays", "locks", "pins", "plants", "proves", "pulls", "seeds",
	"slices", "spins", "stages", "stands", "swaps", "synthesises",
	"upserts",
}

// openerSubject is the identifier a doc comment's first sentence opens
// with. docOpener and anyOpener share it, so the two always capture the
// same name, which the coverage count relies on.
const openerSubject = `^\s*([A-Za-z_][A-Za-z0-9_]*)\s+`

// verbOpener builds the opener that recognises verbs: openerSubject, then
// one of the words, with docOpener's optional `re-`.
func verbOpener(verbs ...[]string) *regexp.Regexp {
	return regexp.MustCompile(openerSubject + `((?:re-)?(?:` +
		strings.Join(slices.Concat(verbs...), "|") + `))\b`)
}

// docOpener matches the opening of a Go doc comment: the identifier it
// documents, then one of docVerbs. The optional `re-` is there because `\b`
// splits a hyphenated verb at the hyphen, so "resetArtistImageGaps re-queues"
// — one of the 36 — would otherwise read as the word "re".
//
// Applied to the group's RENDERED text (ast.CommentGroup.Text), not to
// the first raw line: that strips `//` and `/* */` alike, drops
// directive lines, and skips a leading blank comment line — so a block
// comment or a docblock that opens with a bare `//` is seen rather than
// silently passing (Gemini on #964).
var docOpener = verbOpener(docVerbs)

// testDocOpener is docOpener for a _test.go file: docVerbs and testDocVerbs
// together.
var testDocOpener = verbOpener(docVerbs, testDocVerbs)

// anyOpener is docOpener with the verb left open. It is only ever the
// denominator of the coverage floor, never a detector — see docVerbs for why.
var anyOpener = regexp.MustCompile(openerSubject + `((?:re-)?[a-z]+)\b`)

// docVerbCoverageFloor is the share of subject-first openers — correctly
// attached doc comments that open "<their own name> <word>" — whose word
// docVerbs must recognise, in non-test files.
//
// A misattached block is an ordinary doc comment that lost its subject, so it
// opens the way the rest of the tree's doc comments do, and this share is the
// detector's recall. It was 61% when #964 shipped — the rate that let 28 of
// 36 through — and about 90% once docVerbs was derived from the census. The
// floor fails a list that gets trimmed, or a vocabulary that drifts away from
// it, and names the words to add.
const docVerbCoverageFloor = 0.85

// testDocVerbCoverageFloor is docVerbCoverageFloor for test files: the share
// of their subject-first openers that docVerbs and testDocVerbs together must
// recognise.
//
// Measured apart because one rate over both populations lets the larger carry
// the smaller. On the 2026-09-24 census a single 85% floor passed once "pins"
// alone was added, at 88.1% overall, while the test files' own rate was 84.5%.
//
// Each floor sits about five points under what its population measured when
// it was set: 90.3% for non-test files, 94.9% here (2,720 of 2,866). That
// leaves room for 140 test-file openers to go unrecognised, or for 156 new
// unrecognised ones.
const testDocVerbCoverageFloor = 0.90

// identifierShaped reports whether a doc comment's opening word can only be
// an identifier, never the first word of an English sentence. It can if it
// starts with a lowercase letter or an underscore ("fanout", "jpeg"), or if
// it has an uppercase letter after its first character and a lowercase letter
// somewhere ("pickVoted", "StatusCode", "Test_FileHandler_UpstreamOffline_503").
//
// An English sentence opens with a capital, so a capitalised word with no
// other capital ("The", "Snapshot", "Tailscale", "Removal") and an
// all-capitals one ("DST", "GET", "MP4") are left out: each could be either.
// On the 2026-09-24 census (#991), every opener of those two shapes that
// nothing declared and that a recognised verb followed was prose, 10 of 10.
// Brand names, tool names and units are the known cost: "iOS", "SQLite",
// "sox" and "dBFS" are identifier-shaped. None of them opened a doc with a
// recognised verb that day.
func identifierShaped(word string) bool {
	if word == "" {
		return false
	}
	if first := rune(word[0]); unicode.IsLower(first) || first == '_' {
		return true
	}
	return strings.IndexFunc(word, unicode.IsLower) >= 0 &&
		strings.IndexFunc(word[1:], unicode.IsUpper) >= 0
}

// namesNothingDeclared reports whether a doc comment's opening word names
// something nothing declares. The word must be identifierShaped, no package
// in the doc's directory may declare it (declaredHere), and it must not be
// one of Go's predeclared identifiers, which the language itself declares:
// "nil means …" and "error is …" name something real. No doc in this tree
// opens with a predeclared name and a recognised verb today.
func namesNothingDeclared(word string, declaredHere map[string]bool) bool {
	return identifierShaped(word) && !declaredHere[word] && types.Universe.Lookup(word) == nil
}

// TestNoDocblockNamesAnotherDeclaration.
//
// A doc comment for X glued — no blank line — onto the declaration of a
// different identifier Y. `go doc Y` then prints X's prose, and X is left
// with no documentation at all. It happens when something is INSERTED
// between a comment and its subject: #840 put lyricsStoreAdapter between
// variantDeleterAdapter's docblock and its type, and #953 did the same to
// advertisedEndpoints, whose orphaned paragraph was the written record of
// the "nil is NO list, not the old walk" invariant — with two live
// cross-references still pointing at it.
//
// This repo already treats a comment its neighbouring code contradicts as
// a defect ("A false comment that explains a design choice is how the next
// change gets made on the same reasoning"). A comment attached to the
// wrong declaration is the same failure with the blast radius of a rename.
//
// The census that produced this guard found 16 across the tree, all
// pre-existing, with a 5-of-5 spot-check rate.
//
// Only flagged when the named identifier is itself DECLARED in the same
// package and has NO doc of its own. Both conditions matter: without the
// first, prose legitimately naming a helper is reported; without the
// second, a sentence that opens by referring to a documented sibling is.
// That pair is what took the raw 467 candidates down to 21 and then to 16.
//
// Consts and vars are inspected too, on both sides (#989). They were not,
// and an insertion lands between a docblock and a function as easily as
// between two functions: processJob's whole docblock sat on
// `const variantFailureWriteTimeout` until #988 moved it back, and 18 of
// the 36 blocks the 2026-09-24 census found were glued onto a const or a
// var. The subject of a const or var declaration's doc is every name the
// declaration introduces. A grouped `const ( … )` block's doc describes the
// group, so any member may open it; a doc on one spec inside the group
// describes that spec. A grouped member counts as documented only by a doc
// or line comment of its OWN, not the group's: that is what lets a spec doc
// displaced inside a documented group be reported, and it added no finding
// on the tree it was measured against. The type the members share is not a
// subject: a group doc that opens by defining it is that type's doc in the
// wrong slot, and the census found none.
//
// Test files are inspected too (#990). A census of them found eleven
// blocks glued onto the wrong declaration, and two things differ. Their doc
// comments open with a vocabulary of their own, read through testDocVerbs
// and measured against a floor of their own. And "the same package" needs
// the package NAME, not just the directory, because an external `foo_test`
// package shares its directory with `foo`. A name is looked up the way the
// compiler scopes it. A non-test file sees the non-test files of its
// package. An internal test file (package `foo`) is compiled into that
// package, so it sees those names as well as the test files'. An external
// `foo_test` file sees only its own package. A name an internal test file
// sees in both scopes counts as documented if either declaration is. A test
// fake's undocumented method, named like the documented production
// declaration a test's prose describes, would otherwise turn that prose
// into a finding (Gemini consult on #990).
//
// That lookup has a measured cost. 31 test docs open by naming the
// production declaration they test ("loadCLIConfig is …"), 14 of them with a
// verb these lists recognise, and the second condition alone keeps those 14
// quiet: every one names a documented declaration. Delete one of those
// production docs and its test's prose is reported. The finding is then
// half right, since the production declaration really has lost its doc, and
// the fix is to restore that doc, not to move the test's.
//
// A doc comment that opens with a name nothing declares is reported too
// (#991). That is the other half of the first condition, and `go doc` then
// documents a declaration under a name no reader can search for. It comes
// from a rename the doc did not follow (routesToForegroundLane's doc said
// "routesToOptimizeChannel", the name #863 retired), from a doc written under
// a name nothing ever had ("recordIngest", "pickVoted"), and from import
// keepers documented as helpers that never existed ("ensurePathExists"). The
// census behind this arm found 25 across the tree. It reads the same opener
// as the first arm, and namesNothingDeclared keeps it to identifiers. The
// opening word must be identifierShaped, because "It is …", "Removal is …"
// and "DST is …" open with words nothing declares either. No package in the
// doc's DIRECTORY may declare it, not merely none its file sees, because an
// external `foo_test` file's prose names `foo`'s declarations
// (LooksLikeSnapshotDir, in internal/backup). And it must not be one of Go's
// predeclared names ("nil means …"). A name only ANOTHER directory declares
// is still reported: a doc opens with its own subject, and one doc in the
// tree opens with such a name, by coincidence and without a verb. On the
// unfixed tree that left 20 findings, and all 20 named nothing that exists.
// The other five follow the name with a dash, a colon or a stray word
// ("Test_FileHandler_UpstreamOffline_503 — …"), which neither arm reads.
func TestNoDocblockNamesAnotherDeclaration(t *testing.T) {
	root := repoRootForCitations(t)
	type decl struct {
		file   string
		line   int
		hasDoc bool
	}
	// scope is the set of files whose declarations are looked up together:
	// one package in one directory, test files apart from non-test ones.
	type scope struct {
		dir, pkg string
		test     bool
	}
	// scope -> ident -> where it is declared
	declared := map[scope]map[string]decl{}
	// dir -> every ident any of its packages declares, test files included
	declaredInDir := map[string]map[string]bool{}
	var files []string
	nonTestFiles, testFiles := 0, 0

	walk := func(fn func(path string, sc scope, f *ast.File, fset *token.FileSet)) {
		for _, p := range files {
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, p, nil, parser.ParseComments)
			if err != nil {
				t.Fatalf("parse %s: %v", p, err)
			}
			fn(p, scope{filepath.Dir(p), f.Name.Name, strings.HasSuffix(p, "_test.go")}, f, fset)
		}
	}
	if err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// `vendor` and `testdata` are skipped for the reason the go
			// tool skips them: they are not this module's code. Neither
			// exists here today (no `.go` under any testdata/, no vendor
			// dir), so this is the walk agreeing with the toolchain
			// rather than a live fix — but a `go mod vendor` would
			// otherwise parse thousands of dependency files and report
			// findings nobody here can act on (Gemini on #964).
			if name := d.Name(); path != root && (strings.HasPrefix(name, ".") ||
				strings.HasPrefix(name, "_") || name == "node_modules" ||
				name == "vendor" || name == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		// A file the go tool ignores is skipped too: a name beginning with
		// "." or "_" (`go help packages`). Editors create such files in
		// place — emacs's `.#name.go` lock is a dangling symlink — and
		// parsing one failed this guard over a file no build reads
		// (Gemini consult on #990).
		if name := d.Name(); !strings.HasSuffix(name, ".go") ||
			strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
			return nil
		}
		files = append(files, path)
		if strings.HasSuffix(path, "_test.go") {
			testFiles++
		} else {
			nonTestFiles++
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if nonTestFiles < 100 {
		t.Fatalf("walked %d non-test .go files, want >=100 — the scan is not seeing the tree", nonTestFiles)
	}
	if testFiles < 100 {
		t.Fatalf("walked %d _test.go files, want >=100 — the scan is not seeing the tests", testFiles)
	}

	record := func(sc scope, name, file string, line int, hasDoc bool) {
		// `var _ Iface = impl{}` declares nothing a doc could be about.
		if name == "_" {
			return
		}
		if declaredInDir[sc.dir] == nil {
			declaredInDir[sc.dir] = map[string]bool{}
		}
		declaredInDir[sc.dir][name] = true
		m := declared[sc]
		if m == nil {
			m = map[string]decl{}
			declared[sc] = m
		}
		if _, seen := m[name]; !seen {
			m[name] = decl{file, line, hasDoc}
		}
	}
	// lookup finds name as it is visible from a file in sc (see the
	// docblock above). An external `foo_test` file has its own package
	// name, so its second lookup finds nothing.
	lookup := func(sc scope, name string) (decl, bool) {
		d, ok := declared[sc][name]
		if !sc.test {
			return d, ok
		}
		if nd, nok := declared[scope{sc.dir, sc.pkg, false}][name]; nok && (!ok || nd.hasDoc) {
			return nd, true
		}
		return d, ok
	}
	walk(func(path string, sc scope, f *ast.File, fset *token.FileSet) {
		for _, d := range f.Decls {
			switch n := d.(type) {
			case *ast.FuncDecl:
				record(sc, n.Name.Name, path, fset.Position(n.Pos()).Line, n.Doc != nil)
			case *ast.GenDecl:
				for _, sp := range n.Specs {
					switch s := sp.(type) {
					case *ast.TypeSpec:
						hasDoc := n.Doc != nil || s.Doc != nil
						record(sc, s.Name.Name, path, fset.Position(s.Pos()).Line, hasDoc)
					case *ast.ValueSpec:
						// Inside `( … )` only the spec's own doc or line comment
						// counts: the group's describes the group (see above).
						hasDoc := s.Doc != nil || s.Comment != nil ||
							(n.Doc != nil && !n.Lparen.IsValid())
						for _, id := range s.Names {
							record(sc, id.Name, path, fset.Position(id.Pos()).Line, hasDoc)
						}
					}
				}
			}
		}
	})

	// population is what is counted apart for non-test and for test files:
	// each is read with its own opener and held to its own coverage floor.
	type population struct {
		name                     string
		files                    int
		opener                   *regexp.Regexp
		recognises               string // the list(s) the opener reads, with the verb, for messages
		extend                   string // the list a missing verb belongs in
		floor                    float64
		checked, found           int
		undeclared               int // docs opening with a name nothing declares
		subjectFirst, recognised int
		unrecognised             map[string]int
	}
	nonTest := &population{name: "non-test", files: nonTestFiles, opener: docOpener,
		recognises: "docVerbs recognises", extend: "docVerbs", floor: docVerbCoverageFloor,
		unrecognised: map[string]int{}}
	test := &population{name: "test", files: testFiles, opener: testDocOpener,
		recognises: "docVerbs and testDocVerbs recognise", extend: "testDocVerbs", floor: testDocVerbCoverageFloor,
		unrecognised: map[string]int{}}
	inspect := func(path string, sc scope, subject string, names []string, doc *ast.CommentGroup, fset *token.FileSet) {
		if doc == nil {
			return
		}
		p := nonTest
		if sc.test {
			p = test
		}
		p.checked++
		text := doc.Text()
		m := p.opener.FindStringSubmatch(text)
		if a := anyOpener.FindStringSubmatch(text); a != nil && slices.Contains(names, a[1]) {
			p.subjectFirst++
			if m != nil {
				p.recognised++
			} else {
				p.unrecognised[a[2]]++
			}
		}
		if m == nil || slices.Contains(names, m[1]) {
			return
		}
		other, ok := lookup(sc, m[1])
		if !ok {
			if namesNothingDeclared(m[1], declaredInDir[sc.dir]) {
				p.undeclared++
				dir, _ := filepath.Rel(root, sc.dir)
				t.Errorf("%s:%d — this doc comment opens %q, but no package in %s declares %s, so it "+
					"documents %s under a name that does not exist. Open it with the name of what it "+
					"documents, or delete it if what it describes is gone. If %s is not a Go name (a "+
					"tool, a product), open the sentence another way.",
					path, fset.Position(doc.Pos()).Line, m[1]+" "+m[2], filepath.ToSlash(dir), m[1],
					subject, m[1])
			}
			return
		}
		if other.hasDoc {
			return
		}
		p.found++
		t.Errorf("%s:%d — this doc comment opens %q but is attached to %s, so it "+
			"documents that instead, and %s at %s:%d has no doc of its own. "+
			"Move the block to its subject, or separate the two with a blank line.",
			path, fset.Position(doc.Pos()).Line, m[1]+" "+m[2], subject,
			m[1], filepath.Base(other.file), other.line)
	}
	walk(func(path string, sc scope, f *ast.File, fset *token.FileSet) {
		for _, d := range f.Decls {
			switch n := d.(type) {
			case *ast.FuncDecl:
				inspect(path, sc, fmt.Sprintf("%q", n.Name.Name), []string{n.Name.Name}, n.Doc, fset)
			case *ast.GenDecl:
				var all []string
				for _, sp := range n.Specs {
					switch s := sp.(type) {
					case *ast.TypeSpec:
						doc := s.Doc
						if doc == nil {
							doc = n.Doc
						}
						inspect(path, sc, fmt.Sprintf("%q", s.Name.Name), []string{s.Name.Name}, doc, fset)
					case *ast.ValueSpec:
						var names []string
						for _, id := range s.Names {
							names = append(names, id.Name)
						}
						all = append(all, names...)
						// Only a spec inside `( … )` can carry a doc of its own;
						// an ungrouped declaration's doc is the GenDecl's.
						inspect(path, sc, fmt.Sprintf("%s %s", n.Tok, strings.Join(names, ", ")), names, s.Doc, fset)
					}
				}
				if n.Tok == token.CONST || n.Tok == token.VAR {
					subject := fmt.Sprintf("%s %s", n.Tok, strings.Join(all, ", "))
					if n.Lparen.IsValid() {
						subject = fmt.Sprintf("the %s block (%s)", n.Tok, strings.Join(all, ", "))
					}
					inspect(path, sc, subject, all, n.Doc, fset)
				}
			}
		}
	})
	for _, p := range []*population{nonTest, test} {
		if p.checked < 500 {
			t.Fatalf("inspected %d doc comments in %s files, want >=500 — the scan is not reaching them, "+
				"so this guard would pass no matter what", p.checked, p.name)
		}
		if p.subjectFirst == 0 {
			t.Fatalf("no doc comment in a %s file opened with its own subject — the coverage floor measured nothing", p.name)
		}
		coverage := float64(p.recognised) / float64(p.subjectFirst)
		if coverage < p.floor {
			words := make([]string, 0, len(p.unrecognised))
			for w := range p.unrecognised {
				words = append(words, w)
			}
			sort.Slice(words, func(i, j int) bool {
				if p.unrecognised[words[i]] != p.unrecognised[words[j]] {
					return p.unrecognised[words[i]] > p.unrecognised[words[j]]
				}
				return words[i] < words[j]
			})
			top := make([]string, 0, 15)
			for _, w := range words[:min(len(words), 15)] {
				top = append(top, fmt.Sprintf("%s (%d)", w, p.unrecognised[w]))
			}
			t.Errorf("%s %d of %d subject-first doc openers in %s files (%.1f%%), below the "+
				"%.0f%% floor: a misattached block opening with any other word passes unseen. "+
				"Most common unrecognised: %s. Add the verbs among them to %s.",
				p.recognises, p.recognised, p.subjectFirst, p.name, 100*coverage, 100*p.floor,
				strings.Join(top, ", "), p.extend)
		}
		t.Logf("%s files: inspected %d doc comments across %d files; %d misattached; "+
			"%d opening with a name nothing declares; %s %d of %d subject-first openers (%.1f%%)",
			p.name, p.checked, p.files, p.found, p.undeclared, p.recognises, p.recognised,
			p.subjectFirst, 100*coverage)
	}
}

// TestIdentifierShapedTellsNamesFromSentenceWords pins identifierShaped's
// verdict on the words the census sorted. The two shapes that can open an
// English sentence stay out: a capitalised word with no other capital, and
// an all-capitals one. Brand names, tool names and units are
// identifier-shaped, which is the cost its docblock names.
func TestIdentifierShapedTellsNamesFromSentenceWords(t *testing.T) {
	for _, c := range []struct {
		word string
		want bool
	}{
		// Stale names the census found, both shapes.
		{"expectedTeamID", true},
		{"pickVoted", true},
		{"StatusCode", true},
		{"TestStatusJSONFlag", true},
		{"Test_FileHandler_UpstreamOffline_503", true},
		{"JobSpecVariantID_OptimizeKind", true},
		{"fanout", true},
		{"jpeg", true},
		{"_leading", true},
		// Sentence words the census found opening a doc with a recognised
		// verb, declared by nothing.
		{"The", false},
		{"Snapshot", false},
		{"Tailscale", false},
		{"Removal", false},
		{"It", false},
		{"DST", false},
		{"GET", false},
		{"MP4", false},
		{"A", false},
		{"", false},
		// Brand names, tool names and units: identifier-shaped, and no opener
		// of this shape has been prose yet.
		{"iOS", true},
		{"SQLite", true},
		{"UPnP", true},
		{"sox", true},
		{"dBFS", true},
	} {
		if got := identifierShaped(c.word); got != c.want {
			t.Errorf("identifierShaped(%q) = %v, want %v", c.word, got, c.want)
		}
	}
}

// TestNamesNothingDeclaredAsksTheDirectoryAndTheLanguage pins the two
// conditions namesNothingDeclared adds to identifierShaped, against a
// synthetic directory. The tree cannot pin them: on a clean tree the arm has
// nothing to report, and the one directory-declared opener it holds today
// (LooksLikeSnapshotDir) lasts only as long as that doc's wording.
func TestNamesNothingDeclaredAsksTheDirectoryAndTheLanguage(t *testing.T) {
	here := map[string]bool{"pick": true, "LooksLikeSnapshotDir": true}
	for _, c := range []struct {
		word string
		want bool
	}{
		{"pickVoted", true},
		{"fanout", true},
		// Declared by a package in the directory.
		{"pick", false},
		{"LooksLikeSnapshotDir", false},
		// Declared by the language.
		{"nil", false},
		{"iota", false},
		{"error", false},
		{"len", false},
		{"any", false},
		// Not identifier-shaped, whatever declares it.
		{"It", false},
	} {
		if got := namesNothingDeclared(c.word, here); got != c.want {
			t.Errorf("namesNothingDeclared(%q) = %v, want %v", c.word, got, c.want)
		}
	}
}
