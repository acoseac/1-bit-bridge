package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
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
// The size projection reads the same field (since 2026-09-28), so this pins
// its gate too.
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

// TestNoDependencyIsDecidedFromTheConfigAtConstruction sweeps runServe for
// the shape of the size projection's defect: an admin.Deps field, or an
// argument of a With* option, whose value is a function literal CALLED where
// it is written, reading the config there. That value is decided once, from
// the config as runServe found it, so a Settings PATCH never reaches it
// however live the rest of the path is.
//
// ProjectedSize and AvailableDiskSpace had that shape until 2026-09-28: each
// answered nil unless `upscale.enabled` was true at boot, and the handler
// read nil as "feature off". So the projection endpoint answered 503 after
// upscaling was switched on in Settings, until a restart, and went on
// projecting after it was switched off, while /v1/health followed the
// switch; and the variants-dir panel read "0 B free" on every bridge booted
// with upscaling off. The literals still called in place decide on a wiring
// fact, a handle that is nil or not (the harvest client, the pool), and
// read the config only inside the closures they return, which run per
// request.
func TestNoDependencyIsDecidedFromTheConfigAtConstruction(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	depsLiterals, findings := constructionTimeConfigReads(fset, f)
	// The floor: a sweep that finds no Deps literal passes whatever main.go
	// says, which is how a move of the wiring would retire it without a word.
	if depsLiterals != 1 {
		t.Fatalf("runServe builds %d admin.Deps literals, want 1: the sweep reads that one", depsLiterals)
	}
	for _, finding := range findings {
		t.Errorf("%s: the value is decided once, from the config as it was when serve started, "+
			"and a Settings PATCH never reaches it. Wire it on every bridge and gate where it is "+
			"read, on a live predicate (WIRED vs ACTIVE).", finding)
	}
}

// TestConstructionTimeConfigReadsOnAFixture runs the sweep over synthetic
// source. On a clean tree it reports nothing, so the tree alone cannot show
// that it still reports anything; this shows each shape it reports and each
// it leaves alone.
func TestConstructionTimeConfigReadsOnAFixture(t *testing.T) {
	const src = `package main

func runServe() {
	_, _ = admin.New(admin.Deps{
		BootHolder: func() func() int {
			live := cfgHolder.Load()
			if live == nil || !live.Upscale.Enabled {
				return nil
			}
			return func() int { return 1 }
		}(),
		BootSnapshot: func() bool { return cfg.Upscale.Enabled }(),
		BootSnapshotPassed: func() *adapter { return newAdapter(cfg) }(),
		LiveCfgCalled: func() func() {
			if !liveCfg().Upscale.Enabled {
				return nil
			}
			return func() {}
		}(),
		PredicateCalled: func() func() {
			if !upscaleActiveFn() {
				return nil
			}
			return func() {}
		}(),
		WiringFact: func() func(string) {
			if harvestClient == nil {
				return nil
			}
			return harvestClient.NudgeBookletFetch
		}(),
		ReadInsideTheReturnedClosure: func() *adapter {
			return &adapter{dir: func() string { return cfgHolder.Load().DataDir }}
		}(),
		PredicatePassedNotCalled: func() *adapter { return &adapter{gate: upscaleActiveFn} }(),
		FieldNamedCfg: func() *adapter { return &adapter{cfg: nil, other: s.cfg} }(),
		NotCalledInPlace: func() bool { return liveCfg().Upscale.Enabled },
	})
	apiSrv.WithThing(func() bool { return cfg.Demo.Enabled }())
	apiSrv.WithOther(func() bool { return liveCfg().Demo.Enabled })
	go func() { _ = cfg.Upscale.Enabled }()
}
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "fixture.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	depsLiterals, findings := constructionTimeConfigReads(fset, f)
	if depsLiterals != 1 {
		t.Errorf("found %d admin.Deps literals in the fixture, want 1", depsLiterals)
	}
	want := []string{
		"admin.Deps.BootHolder reads cfgHolder",
		"admin.Deps.BootSnapshot reads cfg",
		"admin.Deps.BootSnapshotPassed reads cfg",
		"admin.Deps.LiveCfgCalled calls liveCfg()",
		"admin.Deps.PredicateCalled calls upscaleActiveFn()",
		"an argument of WithThing reads cfg",
	}
	var got []string
	for _, finding := range findings {
		// Drop the "fixture.go:L:C: " position; the fixture's line numbers
		// are not what this test is about.
		_, rest, _ := strings.Cut(finding, ": ")
		got = append(got, rest)
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("the sweep reported:\n%s\n\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// constructionTimeConfigReads reports, in runServe, every admin.Deps field
// and every argument of a With* call whose value is a function literal
// called in place and reading the config in its own body: the boot
// snapshot (`cfg`), the live holder (`cfgHolder`), `liveCfg()`, or a live
// predicate (a name ending in ActiveFn, CapsFn or EnabledFn) called there.
// A read inside a function literal nested in it runs later, per call, and
// is not one. It also returns how many admin.Deps literals runServe builds.
func constructionTimeConfigReads(fset *token.FileSet, f *ast.File) (depsLiterals int, findings []string) {
	serve := topLevelFuncNamed(f, "runServe")
	if serve == nil || serve.Body == nil {
		return 0, nil
	}
	report := func(where string, value ast.Expr) {
		call, lit := calledInPlace(value)
		if lit == nil {
			return
		}
		if read := configReadIn(lit.Body); read != "" {
			findings = append(findings, fmt.Sprintf("%s: %s %s", fset.Position(call.Pos()), where, read))
		}
	}
	ast.Inspect(serve.Body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CompositeLit:
			if isAdminDepsType(x.Type) {
				depsLiterals++
				forEachKeyedElement(x, func(key *ast.Ident, value ast.Expr) {
					report("admin.Deps."+key.Name, value)
				})
			}
		case *ast.CallExpr:
			if sel, ok := x.Fun.(*ast.SelectorExpr); ok && strings.HasPrefix(sel.Sel.Name, "With") {
				for _, arg := range x.Args {
					report("an argument of "+sel.Sel.Name, arg)
				}
			}
		}
		return true
	})
	return depsLiterals, findings
}

// topLevelFuncNamed returns f's function (not method) named name, or nil.
func topLevelFuncNamed(f *ast.File, name string) *ast.FuncDecl {
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil && fd.Name.Name == name {
			return fd
		}
	}
	return nil
}

// calledInPlace returns value's call and function literal when value calls
// a function literal where it is written, `func() T { … }()`, or two nils.
func calledInPlace(value ast.Expr) (*ast.CallExpr, *ast.FuncLit) {
	call, ok := value.(*ast.CallExpr)
	if !ok {
		return nil, nil
	}
	lit, ok := call.Fun.(*ast.FuncLit)
	if !ok {
		return nil, nil
	}
	return call, lit
}

// forEachKeyedElement calls fn with the key and value of every `Name: value`
// element of lit.
func forEachKeyedElement(lit *ast.CompositeLit, fn func(key *ast.Ident, value ast.Expr)) {
	for _, el := range lit.Elts {
		if kv, ok := el.(*ast.KeyValueExpr); ok {
			if key, ok := kv.Key.(*ast.Ident); ok {
				fn(key, kv.Value)
			}
		}
	}
}

// configReadIn describes the first config read in body that runs when body
// does, or returns "". It does not descend into a nested function literal,
// and a field name (`x.cfg`, a `cfg:` key) is not a read.
func configReadIn(body *ast.BlockStmt) string {
	fieldNames := map[*ast.Ident]bool{}
	ast.Inspect(body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.SelectorExpr:
			fieldNames[x.Sel] = true
		case *ast.CompositeLit:
			forEachKeyedElement(x, func(key *ast.Ident, _ ast.Expr) { fieldNames[key] = true })
		}
		return true
	})
	read := ""
	ast.Inspect(body, func(n ast.Node) bool {
		if read != "" {
			return false
		}
		switch x := n.(type) {
		case *ast.FuncLit:
			return false
		case *ast.CallExpr:
			if id, ok := x.Fun.(*ast.Ident); ok && isLiveConfigCall(id.Name) {
				read = "calls " + id.Name + "()"
				return false
			}
		case *ast.Ident:
			if !fieldNames[x] && (x.Name == "cfg" || x.Name == "cfgHolder") {
				read = "reads " + x.Name
			}
		}
		return true
	})
	return read
}

// isLiveConfigCall reports whether a call of the function named name reads
// the live config: liveCfg, or one of runServe's live predicates.
func isLiveConfigCall(name string) bool {
	return name == "liveCfg" || strings.HasSuffix(name, "ActiveFn") ||
		strings.HasSuffix(name, "CapsFn") || strings.HasSuffix(name, "EnabledFn")
}
