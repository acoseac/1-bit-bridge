package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// soxProbeCallers are the functions in this package allowed to call
// transcode.PrecheckSox or transcode.ProbeSox themselves, each with the
// reason it may. Everything serve reads about sox goes through the one
// TTL-cached probe (soxToolchainCache), which the upscale and analysis gates
// read too.
var soxProbeCallers = map[string]string{
	"soxToolchainCache.snapshot": "the shared probe itself",
	"soxFeatureReady":            "serve's boot-time courtesy line, printed once",
	"soxCLIReady":                "a CLI command's preflight, once per run",
	"runUpscaleBatch":            "a CLI command's classifier probe, once per run",
}

// soxPrecheckWirings are the struct fields in runServe that hand a sox
// precheck to a surface reporting `soxAvailable`: the console's two stats
// endpoints (admin.Deps.UpscalePrecheck) and the /v1 pair. Each must be the
// shared probe's precheck, the probe the gate behind the same surface's
// `enabled` reads.
var soxPrecheckWirings = []struct{ typeName, field string }{
	{"Deps", "UpscalePrecheck"},
	{"upscaleStatsAdapter", "soxPrecheck"},
	{"analysisStatsAdapter", "soxPrecheck"},
}

// TestServeReadsSoxThroughTheSharedProbe pins that a surface serve builds
// answers sox availability from the one probe its gate reads.
//
// Until 2026-09-28 three surfaces kept a 30 s cache of their own: the
// console's stats endpoints, on top of the shared probe, and the /v1 pair's
// two adapters, each over a transcode.PrecheckSox of its own. A surface
// whose `enabled` came from the shared probe and whose `soxAvailable` came
// from another cache answered the two from probes up to 30 s apart: measured
// on a live bridge, `enabled: false` beside `soxAvailable: true` for 14 s
// after sox left the PATH. So no function here may probe sox except those
// soxProbeCallers names, and each precheck runServe wires must be
// `soxCache.precheck`.
func TestServeReadsSoxThroughTheSharedProbe(t *testing.T) {
	fset := token.NewFileSet()
	var callers []string
	seen := map[string]bool{}
	wired := map[string][]string{}
	matches, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	parsed := 0
	for _, name := range matches {
		if strings.HasSuffix(name, "_test.go") || goToolIgnores(name) {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		parsed++
		for fn, pos := range soxProbeCalls(f) {
			seen[fn] = true
			if _, ok := soxProbeCallers[fn]; !ok {
				callers = append(callers, fn+" at "+fset.Position(pos).String())
			}
		}
		for key, values := range precheckWiringsIn(f) {
			wired[key] = append(wired[key], values...)
		}
	}
	if parsed < 20 || !seen["soxToolchainCache.snapshot"] {
		t.Fatalf("parsed %d files and found the shared probe's own call %v: the scan no longer "+
			"reaches this package, so it proves nothing", parsed, seen["soxToolchainCache.snapshot"])
	}
	sort.Strings(callers)
	for _, c := range callers {
		t.Errorf("%s probes sox itself. Read the shared probe instead (soxCache.snapshot, "+
			"soxCache.precheck): a probe of its own answers sox availability from another "+
			"moment than the gate does, so a surface can say enabled beside soxAvailable=false, "+
			"or the reverse. A CLI command that probes once per run belongs in soxProbeCallers.", c)
	}
	for fn := range soxProbeCallers {
		if !seen[fn] {
			t.Errorf("soxProbeCallers allows %s, which no longer probes sox; drop the entry", fn)
		}
	}
	for _, w := range soxPrecheckWirings {
		key := w.typeName + "." + w.field
		values := wired[key]
		if len(values) == 0 {
			t.Errorf("no %s literal in this package sets %s, so that surface reports no "+
				"soxAvailable, or one from a probe this test cannot see", w.typeName, w.field)
		}
		for _, v := range values {
			if v != "soxCache.precheck" {
				t.Errorf("%s is %s, want soxCache.precheck: the shared probe the gate reads", key, v)
			}
		}
	}
}

// soxProbeCalls maps every function in f that calls transcode.PrecheckSox
// or transcode.ProbeSox, named "Recv.Method" or "Func", to the first such
// call's position. It reads the file's own name for the transcode import.
func soxProbeCalls(f *ast.File) map[string]token.Pos {
	pkg := importNameOf(f, "github.com/acoseac/1-bit-bridge/internal/transcode")
	out := map[string]token.Pos{}
	if pkg == "" {
		return out
	}
	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		name := fd.Name.Name
		if fd.Recv != nil && len(fd.Recv.List) == 1 {
			name = recvTypeName(fd.Recv.List[0].Type) + "." + name
		}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			id, ok := sel.X.(*ast.Ident)
			if !ok || id.Name != pkg {
				return true
			}
			if sel.Sel.Name == "PrecheckSox" || sel.Sel.Name == "ProbeSox" {
				if _, dup := out[name]; !dup {
					out[name] = call.Pos()
				}
			}
			return true
		})
	}
	return out
}

// precheckWiringsIn maps "Type.field" to the rendered value of every keyed
// element soxPrecheckWirings names, in every composite literal of f.
func precheckWiringsIn(f *ast.File) map[string][]string {
	out := map[string][]string{}
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		typeName := literalTypeName(lit.Type)
		for _, w := range soxPrecheckWirings {
			if w.typeName != typeName {
				continue
			}
			for _, el := range lit.Elts {
				kv, ok := el.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				if key, ok := kv.Key.(*ast.Ident); ok && key.Name == w.field {
					out[typeName+"."+w.field] = append(out[typeName+"."+w.field], renderExpr(kv.Value))
				}
			}
		}
		return true
	})
	return out
}

// importNameOf is the name f uses for the package at path (importName's
// answer for its import), "" when f does not import it.
func importNameOf(f *ast.File, path string) string {
	for _, imp := range f.Imports {
		if strings.Trim(imp.Path.Value, `"`) == path {
			return importName(imp)
		}
	}
	return ""
}

// recvTypeName is a method receiver's type name, pointer or not.
func recvTypeName(x ast.Expr) string {
	if star, ok := x.(*ast.StarExpr); ok {
		x = star.X
	}
	if id, ok := x.(*ast.Ident); ok {
		return id.Name
	}
	return "?"
}

// literalTypeName is a composite literal's type name, the selector's name
// for a qualified one (admin.Deps is "Deps"), "" for any other shape.
func literalTypeName(x ast.Expr) string {
	switch t := x.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.SelectorExpr:
		return t.Sel.Name
	}
	return ""
}

// renderExpr renders an identifier or a selector chain ("soxCache.precheck"),
// and names any other expression by its AST type ("*ast.FuncLit"), which no
// wiring may be.
func renderExpr(x ast.Expr) string {
	switch e := x.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		return renderExpr(e.X) + "." + e.Sel.Name
	}
	return fmt.Sprintf("%T", x)
}
