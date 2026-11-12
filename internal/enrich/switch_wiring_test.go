package enrich

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"sort"
	"strings"
	"testing"
)

// The switch's wiring in this package (backlog B617), pinned from the
// source where the behavioural tests cannot see it: they set switchEvery,
// and they drive the clients the package builds today, not the next one.

// enrichSources parses this package's non-test files.
func enrichSources(t *testing.T) (*token.FileSet, []*ast.File) {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, e := range entries {
		name := e.Name()
		// The go tool ignores a name starting with "." or "_".
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") ||
			strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		files = append(files, f)
	}
	return fset, files
}

// TestTheEnrichersSendsRunOnTheSwitchsContext: a batch, and a page of the
// portraits the Deezer switch refused (backlog B761), runs on
// enrichgate.CancelWhenOff over enrich.enabled, each Deezer call on
// enrichgate.CancelWhenDeezerOff over enrich.deezerEnabled, both at the
// enricher's switchEvery, which nothing outside a test sets, so production
// asks each switch at enrichgate.Interval, once a second (enrichgate's own
// tests pin the interval and its bound); and every Deezer request the
// enricher makes is made inside deezerCall, on that context.
func TestTheEnrichersSendsRunOnTheSwitchsContext(t *testing.T) {
	fset, files := enrichSources(t)
	gates := map[string][]string{}
	type span struct{ from, to token.Pos }
	var deezerCalls []span
	var deezerRequests []*ast.CallExpr
	for _, f := range files {
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Body != nil {
				key := funcKey(fd)
				ast.Inspect(fd.Body, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					switch fun := types.ExprString(call.Fun); fun {
					case "enrichgate.CancelWhenOff", "enrichgate.CancelWhenDeezerOff":
						gates[key] = append(gates[key], types.ExprString(call))
					case "e.deezerCall":
						for _, arg := range call.Args {
							if lit, ok := arg.(*ast.FuncLit); ok {
								deezerCalls = append(deezerCalls, span{lit.Pos(), lit.End()})
							}
						}
					case "e.deezer.SearchArtist", "e.deezer.FetchImage":
						deezerRequests = append(deezerRequests, call)
					}
					return true
				})
			}
			ast.Inspect(d, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.AssignStmt:
					for _, lhs := range n.Lhs {
						if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "switchEvery" {
							t.Errorf("%s sets switchEvery: %s", fset.Position(n.Pos()), types.ExprString(lhs))
						}
					}
				case *ast.KeyValueExpr:
					if key, ok := n.Key.(*ast.Ident); ok && key.Name == "switchEvery" {
						t.Errorf("%s sets switchEvery in a literal", fset.Position(n.Pos()))
					}
				}
				return true
			})
		}
	}
	want := map[string][]string{
		"(*Enricher).runBatch":     {"enrichgate.CancelWhenOff(ctx, e.on, e.switchEvery)"},
		"(*Enricher).runPortraits": {"enrichgate.CancelWhenOff(ctx, e.on, e.switchEvery)"},
		"(*Enricher).deezerCall":   {"enrichgate.CancelWhenDeezerOff(ctx, e.deezerOn, e.switchEvery)"},
	}
	var keys []string
	for k := range gates {
		keys = append(keys, k)
	}
	for k := range want {
		if _, ok := gates[k]; !ok {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		if got := strings.Join(gates[k], "; "); got != strings.Join(want[k], "; ") {
			t.Errorf("%s asks a switch's context with %q, want %q", k, got, strings.Join(want[k], "; "))
		}
	}
	if len(deezerRequests) == 0 {
		t.Fatal("found no Deezer request in the enricher: the pin reads nothing")
	}
	for _, call := range deezerRequests {
		inside := false
		for _, s := range deezerCalls {
			inside = inside || (call.Pos() >= s.from && call.End() <= s.to)
		}
		if !inside {
			t.Errorf("%s: %s is not made inside deezerCall, so enrich.deezerEnabled's flip does not reach it",
				fset.Position(call.Pos()), types.ExprString(call.Fun))
		}
	}
}

// TestEveryClientSendsThroughTheSwitchsGate: each constructor in this
// package (a New function) that takes an *http.Client takes the client's own
// switches too (enrichgate.Switches), so no client is built without them,
// and builds its client over them (the parameter's Client), Deezer's over
// Deezer's (its ForDeezer first), so a request that starts after a
// switch-off, or an answer that arrives after it, is refused by the client
// the next constructor builds as well, whatever context it is handed.
func TestEveryClientSendsThroughTheSwitchsGate(t *testing.T) {
	_, files := enrichSources(t)
	want := map[string][]string{
		"NewMusicBrainzClient":   {"Client"},
		"NewCoverArtClient":      {"Client"},
		"NewITunesClient":        {"Client"},
		"NewAtlasPremiumFetcher": {"Client"},
		"NewDeezerClient":        {"ForDeezer", "Client"},
	}
	found := map[string]bool{}
	for _, f := range files {
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Recv != nil || fd.Body == nil || !strings.HasPrefix(fd.Name.Name, "New") || !takesAnHTTPClient(fd) {
				continue
			}
			name := fd.Name.Name
			found[name] = true
			methods, listed := want[name]
			if !listed {
				t.Errorf("%s takes an *http.Client and is not listed: take the client's enrichgate.Switches, build its client over them and list it", name)
				continue
			}
			sw := switchesParam(fd)
			if sw == "" {
				t.Errorf("%s takes no enrichgate.Switches, so a client can be built without its switches", name)
				continue
			}
			for _, m := range methods {
				if !callsNamed(fd, func(call *ast.CallExpr) bool { return types.ExprString(call.Fun) == sw+"."+m }) {
					t.Errorf("%s does not call %s.%s on its switches", name, sw, m)
				}
			}
		}
	}
	for name := range want {
		if !found[name] {
			t.Errorf("%s is listed and takes no *http.Client here: remove the entry", name)
		}
	}
}

// switchesParam is the name of fd's parameter of type enrichgate.Switches, or
// "" when it has none.
func switchesParam(fd *ast.FuncDecl) string {
	for _, p := range fd.Type.Params.List {
		if types.ExprString(p.Type) == "enrichgate.Switches" && len(p.Names) == 1 {
			return p.Names[0].Name
		}
	}
	return ""
}

// takesAnHTTPClient reports whether fd has a parameter of type *http.Client.
func takesAnHTTPClient(fd *ast.FuncDecl) bool {
	for _, p := range fd.Type.Params.List {
		if types.ExprString(p.Type) == "*http.Client" {
			return true
		}
	}
	return false
}
