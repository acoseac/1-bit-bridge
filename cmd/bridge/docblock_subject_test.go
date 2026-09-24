package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
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

// openerSubject is the identifier a doc comment's first sentence opens
// with. docOpener and anyOpener share it, so the two always capture the
// same name, which the coverage count relies on.
const openerSubject = `^\s*([A-Za-z_][A-Za-z0-9_]*)\s+`

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
var docOpener = regexp.MustCompile(openerSubject + `((?:re-)?(?:` +
	strings.Join(docVerbs, "|") + `))\b`)

// anyOpener is docOpener with the verb left open. It is only ever the
// denominator of the coverage floor, never a detector — see docVerbs for why.
var anyOpener = regexp.MustCompile(openerSubject + `((?:re-)?[a-z]+)\b`)

// docVerbCoverageFloor is the share of subject-first openers — correctly
// attached doc comments that open "<their own name> <word>" — whose word
// docVerbs must recognise.
//
// A misattached block is an ordinary doc comment that lost its subject, so it
// opens the way the rest of the tree's doc comments do, and this share is the
// detector's recall. It was 61% when #964 shipped — the rate that let 28 of
// 36 through — and about 90% once docVerbs was derived from the census. The
// floor fails a list that gets trimmed, or a vocabulary that drifts away from
// it, and names the words to add.
const docVerbCoverageFloor = 0.85

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
func TestNoDocblockNamesAnotherDeclaration(t *testing.T) {
	root := repoRootForCitations(t)
	type decl struct {
		file   string
		line   int
		hasDoc bool
	}
	// package dir -> ident -> where it is declared
	declared := map[string]map[string]decl{}
	var files []string

	walk := func(fn func(path string, f *ast.File, fset *token.FileSet)) {
		for _, p := range files {
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, p, nil, parser.ParseComments)
			if err != nil {
				t.Fatalf("parse %s: %v", p, err)
			}
			fn(p, f, fset)
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
		if strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
			files = append(files, path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(files) < 100 {
		t.Fatalf("walked %d non-test .go files, want >=100 — the scan is not seeing the tree", len(files))
	}

	record := func(dir, name, file string, line int, hasDoc bool) {
		// `var _ Iface = impl{}` declares nothing a doc could be about.
		if name == "_" {
			return
		}
		m := declared[dir]
		if m == nil {
			m = map[string]decl{}
			declared[dir] = m
		}
		if _, seen := m[name]; !seen {
			m[name] = decl{file, line, hasDoc}
		}
	}
	walk(func(path string, f *ast.File, fset *token.FileSet) {
		dir := filepath.Dir(path)
		for _, d := range f.Decls {
			switch n := d.(type) {
			case *ast.FuncDecl:
				record(dir, n.Name.Name, path, fset.Position(n.Pos()).Line, n.Doc != nil)
			case *ast.GenDecl:
				for _, sp := range n.Specs {
					switch s := sp.(type) {
					case *ast.TypeSpec:
						hasDoc := n.Doc != nil || s.Doc != nil
						record(dir, s.Name.Name, path, fset.Position(s.Pos()).Line, hasDoc)
					case *ast.ValueSpec:
						// Inside `( … )` only the spec's own doc or line comment
						// counts: the group's describes the group (see above).
						hasDoc := s.Doc != nil || s.Comment != nil ||
							(n.Doc != nil && !n.Lparen.IsValid())
						for _, id := range s.Names {
							record(dir, id.Name, path, fset.Position(id.Pos()).Line, hasDoc)
						}
					}
				}
			}
		}
	})

	checked, found := 0, 0
	subjectFirst, recognised := 0, 0
	unrecognised := map[string]int{}
	inspect := func(path, dir, subject string, names []string, doc *ast.CommentGroup, fset *token.FileSet) {
		if doc == nil {
			return
		}
		checked++
		text := doc.Text()
		m := docOpener.FindStringSubmatch(text)
		if a := anyOpener.FindStringSubmatch(text); a != nil && slices.Contains(names, a[1]) {
			subjectFirst++
			if m != nil {
				recognised++
			} else {
				unrecognised[a[2]]++
			}
		}
		if m == nil || slices.Contains(names, m[1]) {
			return
		}
		other, ok := declared[dir][m[1]]
		if !ok || other.hasDoc {
			return
		}
		found++
		t.Errorf("%s:%d — this doc comment opens %q but is attached to %s, so it "+
			"documents that instead, and %s at %s:%d has no doc of its own. "+
			"Move the block to its subject, or separate the two with a blank line.",
			path, fset.Position(doc.Pos()).Line, m[1]+" "+m[2], subject,
			m[1], filepath.Base(other.file), other.line)
	}
	walk(func(path string, f *ast.File, fset *token.FileSet) {
		dir := filepath.Dir(path)
		for _, d := range f.Decls {
			switch n := d.(type) {
			case *ast.FuncDecl:
				inspect(path, dir, fmt.Sprintf("%q", n.Name.Name), []string{n.Name.Name}, n.Doc, fset)
			case *ast.GenDecl:
				var all []string
				for _, sp := range n.Specs {
					switch s := sp.(type) {
					case *ast.TypeSpec:
						doc := s.Doc
						if doc == nil {
							doc = n.Doc
						}
						inspect(path, dir, fmt.Sprintf("%q", s.Name.Name), []string{s.Name.Name}, doc, fset)
					case *ast.ValueSpec:
						var names []string
						for _, id := range s.Names {
							names = append(names, id.Name)
						}
						all = append(all, names...)
						// Only a spec inside `( … )` can carry a doc of its own;
						// an ungrouped declaration's doc is the GenDecl's.
						inspect(path, dir, fmt.Sprintf("%s %s", n.Tok, strings.Join(names, ", ")), names, s.Doc, fset)
					}
				}
				if n.Tok == token.CONST || n.Tok == token.VAR {
					subject := fmt.Sprintf("%s %s", n.Tok, strings.Join(all, ", "))
					if n.Lparen.IsValid() {
						subject = fmt.Sprintf("the %s block (%s)", n.Tok, strings.Join(all, ", "))
					}
					inspect(path, dir, subject, all, n.Doc, fset)
				}
			}
		}
	})
	if checked < 500 {
		t.Fatalf("inspected %d doc comments, want >=500 — the scan is not reaching them, "+
			"so this guard would pass no matter what", checked)
	}
	if subjectFirst == 0 {
		t.Fatal("no doc comment opened with its own subject — the coverage floor measured nothing")
	}
	coverage := float64(recognised) / float64(subjectFirst)
	if coverage < docVerbCoverageFloor {
		words := make([]string, 0, len(unrecognised))
		for w := range unrecognised {
			words = append(words, w)
		}
		sort.Slice(words, func(i, j int) bool {
			if unrecognised[words[i]] != unrecognised[words[j]] {
				return unrecognised[words[i]] > unrecognised[words[j]]
			}
			return words[i] < words[j]
		})
		top := make([]string, 0, 15)
		for _, w := range words[:min(len(words), 15)] {
			top = append(top, fmt.Sprintf("%s (%d)", w, unrecognised[w]))
		}
		t.Errorf("docVerbs recognises %d of %d subject-first doc openers (%.1f%%), below the "+
			"%.0f%% floor: a misattached block opening with any other word passes unseen. "+
			"Most common unrecognised: %s. Add the verbs among them to docVerbs.",
			recognised, subjectFirst, 100*coverage, 100*docVerbCoverageFloor, strings.Join(top, ", "))
	}
	t.Logf("inspected %d doc comments across %d files; %d misattached; "+
		"docVerbs recognises %d of %d subject-first openers (%.1f%%)",
		checked, len(files), found, recognised, subjectFirst, 100*coverage)
}
