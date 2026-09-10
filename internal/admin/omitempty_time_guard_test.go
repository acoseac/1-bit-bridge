package admin

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
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
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		fname := name
		f, err := parser.ParseFile(fset, filepath.Join(dir, fname), nil, 0)
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
				// Parsed, not substring-matched: `omitempty` in a SIBLING
				// tag (bson, gorm) on a field whose json tag has none would
				// otherwise read as a violation. (Gemini on #896.)
				raw, err := strconv.Unquote(field.Tag.Value)
				if err != nil {
					continue
				}
				jsonTag, ok := reflect.StructTag(raw).Lookup("json")
				if !ok || !slices.Contains(strings.Split(jsonTag, ",")[1:], "omitempty") {
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
				// (4) An EMBEDDED time.Time has no Names, and an empty name
				// in the message is the one thing that would make this report
				// unactionable.
				name := "time.Time (embedded)"
				if len(field.Names) > 0 {
					var names []string
					for _, id := range field.Names {
						names = append(names, id.Name)
					}
					name = strings.Join(names, ", ")
				}
				t.Errorf("%s: %s is a VALUE time.Time tagged omitempty — it will ship "+
					"\"0001-01-01T00:00:00Z\" rather than being omitted, and the client "+
					"parses a year-1 date where the tag promised absence. Make it a "+
					"*time.Time and set it through zeroTime / nilIfZeroTime.\n"+
					"NOTE: a template rendering it must move too — a nil pointer reaching "+
					"a value parameter is a render-time error, not a compile-time one.",
					filepath.Join(dir, fname), name)
			}
			return true
		})
	}
	// No vacuous-pass floor: zero violations is the PASSING state here, so a
	// count would only ever rise when one appears. The scan's own health is
	// covered by ParseFile failing loudly above — a directory that stopped
	// yielding .go files would be a build failure long before it reached here.
}
