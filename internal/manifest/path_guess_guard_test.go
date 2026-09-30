package manifest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// TestNoExtractorFillsAPathGuessedFieldOnlyWhenEmpty: the scanner fills a
// track's Title, Album and Artist from its path (fillFromPath: the file name,
// the folder, the folder above) BEFORE it extracts, so an extractor that
// writes one of them only while it is empty never writes it in a scan, and the
// file's own tag loses to its name. A DFF's DIIN and a WAV's LIST/INFO lost
// theirs that way until ExtractorVersion 20 (backlog B140), while their
// extractor tests, which start from an empty Track, passed.
//
// So no function of the package may compare one of those fields with "" but
// the two whose question it is: fillFromPath, which makes the guess, and
// mergePostScanFields, which gives a fresh extract's empty field the stored
// row's. A tag reader answers with the value it read, and where two of a
// file's tags disagree it decides between their VALUES (containerText's
// applyUnder), never by asking whether the track still holds nothing.
func TestNoExtractorFillsAPathGuessedFieldOnlyWhenEmpty(t *testing.T) {
	guessed := map[string]bool{"Title": true, "Artist": true, "Album": true}
	allowed := map[string]bool{"fillFromPath": true, "mergePostScanFields": true}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	files, compared := 0, 0
	for _, e := range entries {
		name := e.Name()
		// A name beginning with "." or "_" is no source of the package's (the
		// go tool ignores it; emacs's `.#extractors.go` lock is one), decided
		// before the file is opened.
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") ||
			strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files++
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				be, ok := n.(*ast.BinaryExpr)
				if !ok || (be.Op != token.EQL && be.Op != token.NEQ) {
					return true
				}
				field := emptyComparedField(be)
				if !guessed[field] {
					return true
				}
				compared++
				if !allowed[fn.Name.Name] {
					t.Errorf("%s: %s compares .%s with \"\": the scanner has filled it from the path by then, so a tag written only while it is empty never reaches a scanned row",
						fset.Position(be.Pos()), fn.Name.Name, field)
				}
				return true
			})
		}
	}
	// The floor: fillFromPath alone compares all three, so a sweep that saw
	// fewer read nothing it was meant to.
	if files < 10 || compared < 3 {
		t.Fatalf("the sweep read %d files and saw %d comparisons: it did not read the package", files, compared)
	}
}

// emptyComparedField is the name of the field a comparison with "" selects
// (`x.Title == ""`, `"" != x.Album`), or "" for any other comparison.
func emptyComparedField(be *ast.BinaryExpr) string {
	sel, lit := be.X, be.Y
	if isEmptyStringLit(sel) {
		sel, lit = lit, sel
	}
	s, ok := sel.(*ast.SelectorExpr)
	if !ok || !isEmptyStringLit(lit) {
		return ""
	}
	return s.Sel.Name
}

// isEmptyStringLit reports whether e is an empty string literal, in either
// kind of quotes.
func isEmptyStringLit(e ast.Expr) bool {
	b, ok := e.(*ast.BasicLit)
	return ok && b.Kind == token.STRING && (b.Value == `""` || b.Value == "``")
}
