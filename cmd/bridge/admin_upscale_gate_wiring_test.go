package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestConsoleBatchGateIsTheV1UpscaleGate pins the one line no internal/admin
// test can see: `UpscaleActive: upscaleActiveFn` in runServe's admin.Deps.
//
// Every admin test builds its own Deps, so with that line deleted they all
// stay green while the console, reading a nil gate as off, refuses every
// batch on a bridge that has upscaling on. With another closure there, the
// console and /v1/health could disagree about whether the feature is on,
// which is the split the live gate exists to remove. So the admin.Deps
// literal's UpscaleActive must be the very identifier WithUpscale is handed.
//
// AST rather than a text scan: this package's commentary names what it
// discusses, the Deps literal's own comment included.
func TestConsoleBatchGateIsTheV1UpscaleGate(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var (
		v1Gates      []ast.Expr // WithUpscale's first argument, per call
		depsLiterals int
		consoleGates []ast.Expr // UpscaleActive's value, per admin.Deps literal
	)
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CallExpr:
			if sel, ok := x.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "WithUpscale" && len(x.Args) > 0 {
				v1Gates = append(v1Gates, x.Args[0])
			}
		case *ast.CompositeLit:
			if !isAdminDepsType(x.Type) {
				return true
			}
			depsLiterals++
			for _, el := range x.Elts {
				kv, ok := el.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "UpscaleActive" {
					consoleGates = append(consoleGates, kv.Value)
				}
			}
		}
		return true
	})
	// Floors: a sweep that finds neither half passes whatever main.go says.
	if len(v1Gates) != 1 || depsLiterals != 1 {
		t.Fatalf("main.go has %d WithUpscale calls and %d admin.Deps literals, want one of each; "+
			"this test compares the two and cannot tell which pair is meant", len(v1Gates), depsLiterals)
	}
	v1, ok := v1Gates[0].(*ast.Ident)
	if !ok {
		t.Fatalf("WithUpscale is handed %T at %s, not a named closure the console could share",
			v1Gates[0], fset.Position(v1Gates[0].Pos()))
	}
	if len(consoleGates) != 1 {
		t.Fatalf("admin.Deps sets UpscaleActive %d times, want once: a Deps without it reads the "+
			"gate as off, and the console refuses every batch while /v1/health says upscaling is on",
			len(consoleGates))
	}
	if got, ok := consoleGates[0].(*ast.Ident); !ok || got.Name != v1.Name {
		t.Errorf("admin.Deps.UpscaleActive at %s is not %s, the closure WithUpscale gets, so the "+
			"console's batch gate and /v1/health's upscaleEnabled can disagree",
			fset.Position(consoleGates[0].Pos()), v1.Name)
	}
}

// isAdminDepsType reports whether a composite literal's type is `admin.Deps`.
func isAdminDepsType(x ast.Expr) bool {
	sel, ok := x.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Deps" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "admin"
}
