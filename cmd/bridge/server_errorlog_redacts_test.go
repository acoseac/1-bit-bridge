package main

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEveryServeHTTPServerRedactsPeerAddresses pins that each http.Server
// the module builds, every one of which serve runs, takes its ErrorLog from
// internal/handshakelog: in the literal (`ErrorLog: handshakelog.ErrorLog()`,
// the tailnet server, the DLNA MediaServer and its loopback track route, the
// OpenHome surface), through a variable every assignment of which is a call
// into handshakelog (the console's, Wrap's pair in its TLS branch), or by the
// assignment Wrap makes (`lis, srv.ErrorLog = handshakelog.Wrap(lis)`, the
// LAN API), and in the last form BEFORE the server is first served, or its
// first handshakes go to net/http's default logger (CodeRabbit on #1055). A
// nil ErrorLog is that default, which prints a peer's address in every
// failed handshake and in a handler's recovered panic ("http: panic serving
// <addr>"), and the privacy page promises that client IPs are not logged for
// the phone-facing API.
//
// A population, not a list: a server anywhere in the module is found by
// shape, and fails here until it is wired or given a reason. It swept
// cmd/bridge alone until B604's second round, when the DLNA MediaServer's
// server, in internal/dlna, still had no ErrorLog (Codex on the integrated
// branch). Each package that builds one must be found, so a sweep that has
// stopped walking a directory does not pass.
func TestEveryServeHTTPServerRedactsPeerAddresses(t *testing.T) {
	root := repoRootForCitations(t)
	fset := token.NewFileSet()
	found := map[string]int{}
	for _, src := range parseModuleServerSources(t, fset, root) {
		for _, decl := range src.file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			for _, srv := range httpServerLiterals(fn.Body) {
				found[src.dir]++
				if problem := handshakeErrorLogProblem(fn.Body, srv); problem != "" {
					name := srv.name
					if name == "" {
						name = "(unnamed)"
					}
					t.Errorf("%s: the http.Server %s in %s %s",
						fset.Position(srv.lit.Pos()), name, fn.Name.Name, problem)
				}
			}
		}
	}
	for _, dir := range []string{"cmd/bridge", "internal/admin", "internal/dlna", "internal/player/openhome"} {
		if found[dir] == 0 {
			t.Errorf("found no http.Server literal in %s, which builds one; the sweep has stopped seeing its servers", dir)
		}
	}
}

// moduleSource is one parsed non-test Go file and the slash-separated
// directory, relative to the module root, that holds it.
type moduleSource struct {
	dir  string
	file *ast.File
}

// parseModuleServerSources parses every non-test Go file under root that
// mentions http.Server, skipping what the go tool ignores (a name beginning
// with "." or "_", and testdata) and vendor.
func parseModuleServerSources(t *testing.T, fset *token.FileSet, root string) []moduleSource {
	t.Helper()
	var out []moduleSource
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if path != root && (strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") ||
				name == "testdata" || name == "vendor") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") ||
			strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !bytes.Contains(raw, []byte("http.Server")) {
			return nil
		}
		f, err := parser.ParseFile(fset, path, raw, 0)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, filepath.Dir(path))
		if err != nil {
			return err
		}
		out = append(out, moduleSource{dir: filepath.ToSlash(rel), file: f})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// parsePackageSources parses this package's non-test Go files, skipping
// the names the go tool ignores.
func parsePackageSources(t *testing.T, fset *token.FileSet) []*ast.File {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var files []*ast.File
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") ||
			strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		files = append(files, f)
	}
	return files
}

// handshakeErrorLogProblem says what is wrong with srv's ErrorLog, or ""
// when it comes from handshakelog before the server is first served.
func handshakeErrorLogProblem(body *ast.BlockStmt, srv serverLiteral) string {
	if v := literalErrorLog(srv.lit); v != nil {
		if callsHandshakelog(v) {
			return ""
		}
		if id, ok := v.(*ast.Ident); ok {
			return errorLogVariableProblem(body, id.Name)
		}
		return "takes its ErrorLog from something other than a call into internal/handshakelog"
	}
	if srv.name == "" {
		return "sets no ErrorLog in its literal, and nothing names it to set one on later"
	}
	assigned := handshakeErrorLogAssignment(body, srv.name)
	if !assigned.IsValid() {
		return "gets no ErrorLog from internal/handshakelog, so net/http would log each failed handshake's client address, and a recovered panic's"
	}
	if served := firstServeCall(body, srv.name); served.IsValid() && served < assigned {
		return "is served before its ErrorLog is set from internal/handshakelog, so its first handshakes go to net/http's default logger"
	}
	return ""
}

// errorLogVariableProblem says what is wrong with the variable name a
// literal's ErrorLog is taken from, or "" when body assigns it only calls
// into handshakelog, at least once: one declared without a value is nil,
// net/http's default logger, until something assigns it.
func errorLogVariableProblem(body *ast.BlockStmt, name string) string {
	assigned := 0
	problem := ""
	ast.Inspect(body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.AssignStmt:
			for i, lhs := range x.Lhs {
				if id, ok := lhs.(*ast.Ident); !ok || id.Name != name {
					continue
				}
				assigned++
				rhs := x.Rhs[0]
				if len(x.Rhs) == len(x.Lhs) {
					rhs = x.Rhs[i]
				}
				if !callsHandshakelog(rhs) {
					problem = "takes its ErrorLog from " + name + ", which is assigned something other than a call into internal/handshakelog"
				}
			}
		case *ast.ValueSpec:
			for i, id := range x.Names {
				if id.Name != name {
					continue
				}
				assigned++
				if len(x.Values) == 0 {
					problem = "takes its ErrorLog from " + name + ", declared without a value, which is nil (net/http's default logger) wherever nothing else assigns it"
					continue
				}
				v := x.Values[0]
				if len(x.Values) == len(x.Names) {
					v = x.Values[i]
				}
				if !callsHandshakelog(v) {
					problem = "takes its ErrorLog from " + name + ", which is declared as something other than a call into internal/handshakelog"
				}
			}
		}
		return true
	})
	if assigned == 0 {
		return "takes its ErrorLog from " + name + ", which this function never assigns"
	}
	return problem
}

type serverLiteral struct {
	// name is what the literal is assigned to (`srv`, `s.httpServer`), or ""
	// for one a later statement cannot name.
	name string
	lit  *ast.CompositeLit
}

// httpServerLiterals returns each http.Server literal in body, `&http.Server{…}`
// or `http.Server{…}`, with the expression it is assigned to (`srv := …`,
// `s.httpServer = …`), and every other one (returned, passed, a field of
// another literal) with no name.
func httpServerLiterals(body *ast.BlockStmt) []serverLiteral {
	var out []serverLiteral
	named := map[*ast.CompositeLit]bool{}
	ast.Inspect(body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != len(as.Rhs) {
			return true
		}
		for i, rhs := range as.Rhs {
			if lit := httpServerLiteral(rhs); lit != nil {
				named[lit] = true
				out = append(out, serverLiteral{name: types.ExprString(as.Lhs[i]), lit: lit})
			}
		}
		return true
	})
	ast.Inspect(body, func(n ast.Node) bool {
		if lit, ok := n.(*ast.CompositeLit); ok && isSelector(lit.Type, "http", "Server") && !named[lit] {
			out = append(out, serverLiteral{lit: lit})
		}
		return true
	})
	return out
}

// httpServerLiteral returns e's http.Server literal when e is one, or the
// address of one.
func httpServerLiteral(e ast.Expr) *ast.CompositeLit {
	if u, ok := e.(*ast.UnaryExpr); ok && u.Op == token.AND {
		e = u.X
	}
	if lit, ok := e.(*ast.CompositeLit); ok && isSelector(lit.Type, "http", "Server") {
		return lit
	}
	return nil
}

// literalErrorLog returns the value lit sets ErrorLog to, or nil.
func literalErrorLog(lit *ast.CompositeLit) ast.Expr {
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "ErrorLog" {
			return kv.Value
		}
	}
	return nil
}

// handshakeErrorLogAssignment returns the position of the first statement
// in body that assigns name.ErrorLog from a call into handshakelog, or
// token.NoPos.
func handshakeErrorLogAssignment(body *ast.BlockStmt, name string) token.Pos {
	first := token.NoPos
	ast.Inspect(body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Rhs) != 1 || !callsHandshakelog(as.Rhs[0]) {
			return true
		}
		for _, lhs := range as.Lhs {
			if isFieldOf(lhs, name, "ErrorLog") && (!first.IsValid() || as.Pos() < first) {
				first = as.Pos()
			}
		}
		return true
	})
	return first
}

// firstServeCall returns the position of the first call in body that
// serves name (Serve, ServeTLS, ListenAndServe or ListenAndServeTLS), or
// token.NoPos. A call inside a goroutine's closure counts at its place in
// the source.
func firstServeCall(body *ast.BlockStmt, name string) token.Pos {
	first := token.NoPos
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		for _, method := range []string{"Serve", "ServeTLS", "ListenAndServe", "ListenAndServeTLS"} {
			if isFieldOf(call.Fun, name, method) && (!first.IsValid() || call.Pos() < first) {
				first = call.Pos()
			}
		}
		return true
	})
	return first
}

// isFieldOf reports whether e is `name.field`, name an identifier or a
// selector (`s.httpServer`).
func isFieldOf(e ast.Expr, name, field string) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != field {
		return false
	}
	return types.ExprString(sel.X) == name
}

func callsHandshakelog(e ast.Expr) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "handshakelog"
}

func isSelector(e ast.Expr, pkg, name string) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == pkg && sel.Sel.Name == name
}
