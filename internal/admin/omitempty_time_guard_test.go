package admin

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoOmitemptyValueTimeOnTheWire ships with the fix rather than after it.
//
// `omitempty` does NOT drop a zero time.Time. It is a struct, and
// encoding/json's emptiness rule does not know about it — so a value field
// tagged omitempty serialises "0001-01-01T00:00:00Z" on every response, and the
// client parses a real, very old date exactly where the tag promised absence.
//
// This repo had already met the trap and fixed it correctly in three structs,
// each citing Qodo on PR #102 — and was violating it in ten fields across five
// files, including the /v1/health DTO iOS decodes. Documenting the rule three
// times did not stop the eleventh; a rule and the guard that pins it should not
// land in separate PRs, or the window between them is exactly when the next
// instance appears.
//
// AST rather than a text scan: the question is about a FIELD's type and tag
// together, which a regex can only approximate, and this package's own
// commentary quotes the broken form while explaining it.
func TestNoOmitemptyValueTimeOnTheWire(t *testing.T) {
	for _, dir := range []string{".", "../api"} {
		checkOmitemptyTimes(t, dir)
	}
}

func checkOmitemptyTimes(t *testing.T, dir string) {
	t.Helper()
	fset := token.NewFileSet()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			st, ok := n.(*ast.StructType)
			if !ok {
				return true
			}
			for _, field := range st.Fields.List {
				if field.Tag == nil {
					continue
				}
				tag := field.Tag.Value
				if !strings.Contains(tag, "json:") || !strings.Contains(tag, "omitempty") {
					continue
				}
				sel, ok := field.Type.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Time" {
					continue
				}
				pkg, ok := sel.X.(*ast.Ident)
				if !ok || pkg.Name != "time" {
					continue
				}
				checked++
				var names []string
				for _, id := range field.Names {
					names = append(names, id.Name)
				}
				t.Errorf("%s: %s is a VALUE time.Time tagged omitempty — it will ship "+
					"\"0001-01-01T00:00:00Z\" rather than being omitted, and the client "+
					"parses a year-1 date where the tag promised absence. Make it a "+
					"*time.Time and set it through zeroTime / nilIfZeroTime.\n"+
					"NOTE: a template rendering it must move too — a nil pointer reaching "+
					"a value parameter is a render-time error, not a compile-time one.",
					filepath.Join(dir, name), strings.Join(names, ", "))
			}
			return true
		})
	}
	// No vacuous-pass floor on `checked`: zero violations is the PASSING state
	// and the count only rises when one appears. The scan's own health is
	// covered by the parse failing loudly above.
}
