package main

import (
	"go/ast"
	"go/token"
	"testing"
)

// TestAPIServersLeaveOrdinaryRequestsTheirOwnContext requires the phone
// API servers to leave a request its own context. Parenting every request
// on the serve context cancels a manifest page and a favorites write the
// moment a stop begins. The event streams end through
// Server.EndEventStreamsWhen instead. The loopback boot covers the LAN
// pair. The tailnet pair is not started there, so putting BaseContext or
// ConnContext back on either pair is what this test sees. It also turns
// TestAnOrdinaryRequestInFlightAtShutdownCompletes red.
func TestAPIServersLeaveOrdinaryRequestsTheirOwnContext(t *testing.T) {
	fset := token.NewFileSet()
	httpN, h3N := 0, 0
	for _, f := range parsePackageSources(t, fset) {
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			switch {
			case isSelector(lit.Type, "http", "Server"):
				httpN++
				if literalSetsField(lit, "BaseContext") {
					t.Errorf("%s: http.Server sets BaseContext; that cancels every in-flight request when serve stops", fset.Position(lit.Pos()))
				}
			case isSelector(lit.Type, "http3", "Server"):
				h3N++
				if literalSetsField(lit, "ConnContext") {
					t.Errorf("%s: http3.Server sets ConnContext; that cancels every in-flight request when serve stops", fset.Position(lit.Pos()))
				}
			}
			return true
		})
	}
	if httpN < 2 || h3N < 2 {
		t.Fatalf("found %d http.Server and %d http3.Server literals; want the LAN and tailnet pair of each", httpN, h3N)
	}
}

func literalSetsField(lit *ast.CompositeLit, field string) bool {
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		id, ok := kv.Key.(*ast.Ident)
		if ok && id.Name == field {
			return true
		}
	}
	return false
}
