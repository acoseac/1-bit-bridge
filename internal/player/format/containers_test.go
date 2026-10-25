package format

import (
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strconv"
	"testing"
)

// declaredContainers is every constant pcm.go declares of type Container,
// by its value, in the order it declares them: read from the source, so a
// container added there is one Containers must name.
func declaredContainers(t *testing.T) []Container {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "pcm.go", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	var out []Container
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs := spec.(*ast.ValueSpec)
			if id, ok := vs.Type.(*ast.Ident); !ok || id.Name != "Container" {
				continue
			}
			for _, v := range vs.Values {
				lit, ok := v.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					t.Fatalf("a Container constant whose value is no string literal: %#v", v)
				}
				s, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatal(err)
				}
				out = append(out, Container(s))
			}
		}
	}
	return out
}

// TestContainersIsEveryContainerDeclared: Containers names every Container
// pcm.go declares, once each and in their order (B470 PR 5b: the player's
// codecs are filtered from it), and IsDSD is true for DSF and DFF alone.
// Which decoder gives DSD is the decoders' own (decode's
// TestADecoderGivesDSDExactlyForADSDContainer).
func TestContainersIsEveryContainerDeclared(t *testing.T) {
	declared := declaredContainers(t)
	if len(declared) == 0 {
		t.Fatal("pcm.go declares no Container: the reader of its source is broken")
	}
	if got := Containers(); !slices.Equal(got, declared) {
		t.Fatalf("Containers() = %q, want every container pcm.go declares, in order: %q", got, declared)
	}
	for _, c := range declared {
		want := c == ContainerDSF || c == ContainerDFF
		if c.IsDSD() != want {
			t.Errorf("%s.IsDSD() = %v, want %v", c, c.IsDSD(), want)
		}
	}
	if Container("mp3").IsDSD() || Container("").IsDSD() {
		t.Error("a container no decoder reconstructs from is DSD")
	}
}
