package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// TestServeGivesTheMediaServerItsOneSSDPSubsystem reads the wiring of B470 PR
// 5a1's one SSDP subsystem, which no serve test can start for real (DLNA
// would advertise on the LAN): cmd/bridge builds a subsystem in one place,
// newServeSSDP; runServe calls it once, hands the result to the MediaServer
// through dlnaWiring.SSDP and defers its Close; and startDLNAIfEnabled puts
// that field in the server's config, so the server registers there and
// builds no private subsystem of its own (TestTheServerRegistersWithTheSubsystemItIsGiven
// in internal/dlna pins the server's half). A second subsystem would bind a
// second set of listeners on port 1900, where a unicast M-SEARCH reaches
// only one.
func TestServeGivesTheMediaServerItsOneSSDPSubsystem(t *testing.T) {
	fset := token.NewFileSet()
	files := map[string]*ast.File{}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	builds := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files[name] = f
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok && isSelector(call.Fun, "dlna", "NewSSDPSubsystem") {
					builds++
					if fd.Name.Name != "newServeSSDP" {
						t.Errorf("%s builds an SSDP subsystem in %s, not newServeSSDP", fset.Position(call.Pos()), fd.Name.Name)
					}
				}
				return true
			})
		}
	}
	if builds != 1 {
		t.Errorf("cmd/bridge builds %d SSDP subsystems, want newServeSSDP's one", builds)
	}

	serve := topLevelFuncNamed(files["main.go"], "runServe")
	if serve == nil {
		t.Fatal("main.go has no runServe")
	}
	var subsystem string
	var calls, wired, closes int
	ast.Inspect(serve.Body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.AssignStmt:
			if len(x.Rhs) == 1 && len(x.Lhs) == 1 {
				if call, ok := x.Rhs[0].(*ast.CallExpr); ok && isIdent(call.Fun, "newServeSSDP") {
					calls++
					if id, ok := x.Lhs[0].(*ast.Ident); ok {
						subsystem = id.Name
					}
				}
			}
		case *ast.CompositeLit:
			if !isIdent(x.Type, "dlnaWiring") {
				return true
			}
			forEachKeyedElement(x, func(key *ast.Ident, value ast.Expr) {
				if key.Name == "SSDP" && subsystem != "" && isIdent(value, subsystem) {
					wired++
				}
			})
		case *ast.DeferStmt:
			ast.Inspect(x, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok && subsystem != "" && isSelector(call.Fun, subsystem, "Close") {
					closes++
				}
				return true
			})
		}
		return true
	})
	if calls != 1 || wired != 1 || closes != 1 {
		t.Errorf("runServe calls newServeSSDP %d time(s), hands its subsystem to dlnaWiring.SSDP %d, and defers its Close %d: want each once",
			calls, wired, closes)
	}

	start := topLevelFuncNamed(files["dlna_wiring.go"], "startDLNAIfEnabled")
	if start == nil {
		t.Fatal("dlna_wiring.go has no startDLNAIfEnabled")
	}
	configs, injected := 0, 0
	ast.Inspect(start.Body, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok || !isSelector(lit.Type, "dlna", "ServerConfig") {
			return true
		}
		configs++
		forEachKeyedElement(lit, func(key *ast.Ident, value ast.Expr) {
			if key.Name == "SSDP" && isSelector(value, "w", "SSDP") {
				injected++
			}
		})
		return true
	})
	if configs != 1 || injected != 1 {
		t.Errorf("startDLNAIfEnabled builds %d dlna.ServerConfig(s), %d of them with SSDP: w.SSDP; want one, with it", configs, injected)
	}
}
