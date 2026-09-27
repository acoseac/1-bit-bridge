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
// the LAN API). A nil ErrorLog is net/http's default, which prints a
// peer's address in every failed handshake, and the privacy page promises
// that client IPs are not logged for the phone-facing API.
//
// A population, not a list: a new server in this package is found by
// shape, and fails here until it is wired or given a reason. The floor of
// two keeps a sweep that stopped finding the servers from passing.
func TestEveryServeHTTPServerRedactsPeerAddresses(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	found := 0
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
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			for _, srv := range httpServerLiterals(fn.Body) {
				found++
				if !literalSetsHandshakeErrorLog(srv.lit) && !assignsHandshakeErrorLog(fn.Body, srv.name) {
					t.Errorf("%s: the http.Server %q in %s gets no ErrorLog from internal/handshakelog, "+
						"so net/http would log each failed handshake's client address",
						fset.Position(srv.lit.Pos()), srv.name, fn.Name.Name)
				}
			}
		}
	}
	if found < 2 {
		t.Fatalf("found %d http.Server literals, want at least the LAN and tailnet servers; the sweep has stopped seeing them", found)
	}
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

// assignsHandshakeErrorLog reports whether body assigns name.ErrorLog from
// a call into handshakelog.
func assignsHandshakeErrorLog(body *ast.BlockStmt, name string) bool {
	hit := false
	ast.Inspect(body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Rhs) != 1 || !callsHandshakelog(as.Rhs[0]) {
			return true
		}
		for _, lhs := range as.Lhs {
			if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "ErrorLog" {
				if id, ok := sel.X.(*ast.Ident); ok && id.Name == name {
					hit = true
				}
			}
		}
		return true
	})
	return hit
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
