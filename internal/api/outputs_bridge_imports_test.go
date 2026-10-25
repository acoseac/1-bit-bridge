package api

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// playerTree is the import path every player package sits below.
const playerTree = "github.com/acoseac/1-bit-bridge/internal/player"

// TestTheAPIImportsNoPlayerPackage pins that this package imports no
// internal/player package (B470 PR 5b): GET /v1/outputs builds the element
// of kind bridge from BridgeOutputFacts, plain facts cmd/bridge fills, so no
// player type reaches the wire by an encoding of its own. Every source file
// is read, whatever its build constraint, so one built on Linux alone is held
// to it on any host. The package's tests are not: they are no part of what
// it encodes.
func TestTheAPIImportsNoPlayerPackage(t *testing.T) {
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	read := 0
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		read++
		for _, imp := range f.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatalf("%s: import %s: %v", name, imp.Path.Value, err)
			}
			if p == playerTree || strings.HasPrefix(p, playerTree+"/") {
				t.Errorf("%s imports %s: internal/api imports no internal/player package, and a fact the element needs goes in BridgeOutputFacts", name, p)
			}
		}
	}
	if read < 10 {
		t.Fatalf("read %d source files: the glob found too few", read)
	}
}
