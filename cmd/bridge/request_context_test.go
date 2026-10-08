package main

import (
	"context"
	"go/ast"
	"go/token"
	"testing"
	"time"
)

func TestCancelWithEndsWhenEitherContextEnds(t *testing.T) {
	t.Run("parent", func(t *testing.T) {
		parent, cancel := context.WithCancel(context.Background())
		ctx := cancelWith(parent, context.Background())
		cancel()
		waitForDone(t, ctx)
	})
	t.Run("also", func(t *testing.T) {
		also, cancel := context.WithCancel(context.Background())
		ctx := cancelWith(context.Background(), also)
		cancel()
		waitForDone(t, ctx)
	})
	t.Run("parent already done", func(t *testing.T) {
		parent, cancel := context.WithCancel(context.Background())
		cancel()
		waitForDone(t, cancelWith(parent, context.Background()))
	})
	t.Run("also already done", func(t *testing.T) {
		also, cancel := context.WithCancel(context.Background())
		cancel()
		waitForDone(t, cancelWith(context.Background(), also))
	})
}

func waitForDone(t *testing.T, ctx context.Context) {
	t.Helper()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("context still open")
	}
}

// TestAPIServersParentRequestsOnTheServeContext requires every phone API
// server this package builds to parent its requests on the serve context.
// The loopback boot covers the LAN pair. The tailnet pair is not started
// there, so dropping its wiring would leave this test as the only red.
func TestAPIServersParentRequestsOnTheServeContext(t *testing.T) {
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
				if !literalCalls(lit, "BaseContext", "apiBaseContext") {
					t.Errorf("%s: http.Server does not set BaseContext from apiBaseContext", fset.Position(lit.Pos()))
				}
			case isSelector(lit.Type, "http3", "Server"):
				h3N++
				if !literalCalls(lit, "ConnContext", "apiConnContext") {
					t.Errorf("%s: http3.Server does not set ConnContext from apiConnContext", fset.Position(lit.Pos()))
				}
			}
			return true
		})
	}
	if httpN < 2 || h3N < 2 {
		t.Fatalf("found %d http.Server and %d http3.Server literals; want the LAN and tailnet pair of each", httpN, h3N)
	}
}

func literalCalls(lit *ast.CompositeLit, field, fn string) bool {
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		id, ok := kv.Key.(*ast.Ident)
		if !ok || id.Name != field {
			continue
		}
		call, ok := kv.Value.(*ast.CallExpr)
		if !ok {
			return false
		}
		name, ok := call.Fun.(*ast.Ident)
		return ok && name.Name == fn
	}
	return false
}
