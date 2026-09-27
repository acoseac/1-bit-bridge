package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEveryServeHTTPServerRedactsPeerAddresses pins that each http.Server
// this package builds takes its ErrorLog from internal/handshakelog: in the
// literal (`ErrorLog: handshakelog.ErrorLog()`, the tailnet server) or by
// the assignment Wrap makes (`lis, srv.ErrorLog = handshakelog.Wrap(lis)`,
// the LAN API), and in the second form BEFORE the server is first served,
// or its first handshakes go to net/http's default logger (CodeRabbit on
// #1055). A nil ErrorLog is that default, which prints a peer's address in
// every failed handshake, and the privacy page promises that client IPs
// are not logged for the phone-facing API.
//
// A population, not a list: a new server in this package is found by
// shape, and fails here until it is wired or given a reason. The floor of
// two keeps a sweep that stopped finding the servers from passing.
func TestEveryServeHTTPServerRedactsPeerAddresses(t *testing.T) {
	fset := token.NewFileSet()
	found := 0
	for _, f := range parsePackageSources(t, fset) {
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			for _, srv := range httpServerLiterals(fn.Body) {
				found++
				if problem := handshakeErrorLogProblem(fn.Body, srv); problem != "" {
					t.Errorf("%s: the http.Server %q in %s %s",
						fset.Position(srv.lit.Pos()), srv.name, fn.Name.Name, problem)
				}
			}
		}
	}
	if found < 2 {
		t.Fatalf("found %d http.Server literals, want at least the LAN and tailnet servers; the sweep has stopped seeing them", found)
	}
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
	if literalSetsHandshakeErrorLog(srv.lit) {
		return ""
	}
	assigned := handshakeErrorLogAssignment(body, srv.name)
	if !assigned.IsValid() {
		return "gets no ErrorLog from internal/handshakelog, so net/http would log each failed handshake's client address"
	}
	if served := firstServeCall(body, srv.name); served.IsValid() && served < assigned {
		return "is served before its ErrorLog is set from internal/handshakelog, so its first handshakes go to net/http's default logger"
	}
	return ""
}

type serverLiteral struct {
	name string
	lit  *ast.CompositeLit
}

// httpServerLiterals returns each `name := &http.Server{...}` (or `=`) in
// body.
func httpServerLiterals(body *ast.BlockStmt) []serverLiteral {
	var out []serverLiteral
	ast.Inspect(body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
			return true
		}
		u, ok := as.Rhs[0].(*ast.UnaryExpr)
		if !ok || u.Op != token.AND {
			return true
		}
		lit, ok := u.X.(*ast.CompositeLit)
		if !ok || !isSelector(lit.Type, "http", "Server") {
			return true
		}
		if id, ok := as.Lhs[0].(*ast.Ident); ok {
			out = append(out, serverLiteral{name: id.Name, lit: lit})
		}
		return true
	})
	return out
}

// literalSetsHandshakeErrorLog reports whether lit sets ErrorLog to a call
// into handshakelog.
func literalSetsHandshakeErrorLog(lit *ast.CompositeLit) bool {
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "ErrorLog" && callsHandshakelog(kv.Value) {
			return true
		}
	}
	return false
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

// isFieldOf reports whether e is `name.field`.
func isFieldOf(e ast.Expr, name, field string) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != field {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == name
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
