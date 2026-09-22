package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// docOpenerVerbs are the words a Go doc comment uses immediately after
// the identifier it documents. Deliberately a closed list: matching
// "<Ident> <anything>" would flag every sentence that happens to open
// with a capitalised name, and the shape being caught is specifically a
// DOC COMMENT that lost its subject.
var docOpener = regexp.MustCompile(`^//\s+([A-Za-z_][A-Za-z0-9_]*)\s+` +
	`(is|are|implements|returns|holds|wraps|reports|takes|does|builds|maps|` +
	`counts|stamps|resolves|answers|records|serves|walks|parses|converts|` +
	`emits|adds|removes|creates|deletes|renders|describes|provides|tracks|` +
	`mirrors|handles)\b`)

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
			if name := d.Name(); path != root && (strings.HasPrefix(name, ".") ||
				strings.HasPrefix(name, "_") || name == "node_modules") {
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
					if ts, ok := sp.(*ast.TypeSpec); ok {
						hasDoc := n.Doc != nil || ts.Doc != nil
						record(dir, ts.Name.Name, path, fset.Position(ts.Pos()).Line, hasDoc)
					}
				}
			}
		}
	})

	checked, found := 0, 0
	inspect := func(path string, dir string, name string, doc *ast.CommentGroup, fset *token.FileSet) {
		if doc == nil || len(doc.List) == 0 {
			return
		}
		checked++
		m := docOpener.FindStringSubmatch(doc.List[0].Text)
		if m == nil || m[1] == name {
			return
		}
		other, ok := declared[dir][m[1]]
		if !ok || other.hasDoc {
			return
		}
		found++
		t.Errorf("%s:%d — this doc comment opens %q but is attached to %q. "+
			"`go doc %s` prints %s's documentation, and %s at %s:%d has none. "+
			"Move the block to its subject, or separate the two with a blank line.",
			path, fset.Position(doc.Pos()).Line, m[1]+" "+m[2], name,
			name, m[1], m[1], filepath.Base(other.file), other.line)
	}
	walk(func(path string, f *ast.File, fset *token.FileSet) {
		dir := filepath.Dir(path)
		for _, d := range f.Decls {
			switch n := d.(type) {
			case *ast.FuncDecl:
				inspect(path, dir, n.Name.Name, n.Doc, fset)
			case *ast.GenDecl:
				for _, sp := range n.Specs {
					if ts, ok := sp.(*ast.TypeSpec); ok {
						doc := ts.Doc
						if doc == nil {
							doc = n.Doc
						}
						inspect(path, dir, ts.Name.Name, doc, fset)
					}
				}
			}
		}
	})
	if checked < 500 {
		t.Fatalf("inspected %d doc comments, want >=500 — the scan is not reaching them, "+
			"so this guard would pass no matter what", checked)
	}
	t.Logf("inspected %d doc comments across %d files; %d misattached", checked, len(files), found)
}
