package binding

import (
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strconv"
	"testing"
)

// TestStatesIsEveryStateDeclared: States names every State binding.go
// declares, once each and in their order, read from the source (B470 PR 5b):
// GET /v1/outputs passes a state on as the player's output reason, from a
// closed list cmd/bridge maps and PROTOCOL.md names, so a state added here
// must reach that list, which enumerates States.
func TestStatesIsEveryStateDeclared(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "binding.go", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	var declared []State
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs := spec.(*ast.ValueSpec)
			if id, ok := vs.Type.(*ast.Ident); !ok || id.Name != "State" {
				continue
			}
			for _, v := range vs.Values {
				lit, ok := v.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					t.Fatalf("a State whose value is no string literal: %#v", v)
				}
				s, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatal(err)
				}
				declared = append(declared, State(s))
			}
		}
	}
	if len(declared) < 2 {
		t.Fatalf("binding.go declares %d States: the reader of its source is broken", len(declared))
	}
	if got := States(); !slices.Equal(got, declared) {
		t.Fatalf("States() = %q, want every State binding.go declares, in order: %q", got, declared)
	}
}
