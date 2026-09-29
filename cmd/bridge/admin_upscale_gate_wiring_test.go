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
	requireConsoleGateIsTheV1Gate(t, "WithUpscale", "UpscaleActive",
		"a Deps without it reads the gate as off, and the console refuses every batch while "+
			"/v1/health says upscaling is on",
		"the console's batch gate and /v1/health's upscaleEnabled")
}

// TestConsoleCarPlayGateIsTheV1CarPlayGate pins `OptimizeActive:
// carPlayOptimizeActiveFn` in runServe's admin.Deps: the console's
// optimize kind (its batch submit and its projection) reads the closure
// WithCarPlayOptimize hands /v1, the one /v1/health's carPlayOptimize and
// the auto-optimize sweeper read.
//
// Until 2026-09-28 the field was a copy that read the upscale flag and the
// CarPlay switch and nothing of sox. Both of its readers ask UpscaleActive
// first, so no request answered differently, which is exactly why no
// behavioural test can pin this. A reader that asked it alone would have
// heard "on" from a bridge without sox, and the sweeper's gate was the
// same kind of copy: it ran jobs there that could only fail
// (TestServeWithoutSoxReportsUpscalingOffOnEverySurface).
func TestConsoleCarPlayGateIsTheV1CarPlayGate(t *testing.T) {
	requireConsoleGateIsTheV1Gate(t, "WithCarPlayOptimize", "OptimizeActive",
		"a Deps without it reads the CarPlay kind as always on",
		"the console's optimize kind and /v1/health's carPlayOptimize")
}

// requireConsoleGateIsTheV1Gate requires runServe's admin.Deps literal to
// set field, once, to the very identifier main.go hands the /v1 server's
// option method as its first argument. missing says what a Deps without
// the field does; pair names the two surfaces that could otherwise
// disagree.
func requireConsoleGateIsTheV1Gate(t *testing.T, option, field, missing, pair string) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var (
		v1Gates      []ast.Expr // the option's first argument, per call
		depsLiterals int
		consoleGates []ast.Expr // the field's value, per admin.Deps literal
	)
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CallExpr:
			if sel, ok := x.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == option && len(x.Args) > 0 {
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
				if key, ok := kv.Key.(*ast.Ident); ok && key.Name == field {
					consoleGates = append(consoleGates, kv.Value)
				}
			}
		}
		return true
	})
	// Floors: a sweep that finds neither half passes whatever main.go says.
	if len(v1Gates) != 1 || depsLiterals != 1 {
		t.Fatalf("main.go has %d %s calls and %d admin.Deps literals, want one of each; "+
			"this test compares the two and cannot tell which pair is meant", len(v1Gates), option, depsLiterals)
	}
	v1, ok := v1Gates[0].(*ast.Ident)
	if !ok {
		t.Fatalf("%s is handed %T at %s, not a named closure the console could share",
			option, v1Gates[0], fset.Position(v1Gates[0].Pos()))
	}
	if len(consoleGates) != 1 {
		t.Fatalf("admin.Deps sets %s %d times, want once: %s", field, len(consoleGates), missing)
	}
	if got, ok := consoleGates[0].(*ast.Ident); !ok || got.Name != v1.Name {
		t.Errorf("admin.Deps.%s at %s is not %s, the closure %s gets, so %s can disagree",
			field, fset.Position(consoleGates[0].Pos()), v1.Name, option, pair)
	}
}

// TestAStaleDownloadRendersUnderTheV1KindGates pins the gates a stale
// download's re-render reads (backlog B82): runServe's one renditionGates
// literal must name, field by field, the very closures the /v1 server's
// options get, the ones POST /v1/upscale and /v1/health read. A copy could
// answer differently (the sox half of a gate is where copies have drifted
// before: TestConsoleCarPlayGateIsTheV1CarPlayGate), and then a download
// would render a kind a client's request for it is refused. No behavioural
// test sees this wiring: the harness builds its own gates.
func TestAStaleDownloadRendersUnderTheV1KindGates(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	serve := topLevelFuncNamed(f, "runServe")
	if serve == nil {
		t.Fatal("main.go has no runServe")
	}
	options := map[string][]string{} // option → the identifiers the /v1 server's calls hand it
	var gates []*ast.CompositeLit
	ast.Inspect(serve.Body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CallExpr:
			sel, ok := x.Fun.(*ast.SelectorExpr)
			if !ok || len(x.Args) == 0 || !chainStartsAtAPINew(sel.X) {
				return true
			}
			if id, ok := x.Args[0].(*ast.Ident); ok {
				options[sel.Sel.Name] = append(options[sel.Sel.Name], id.Name)
			} else {
				options[sel.Sel.Name] = append(options[sel.Sel.Name], fmt.Sprintf("a %T", x.Args[0]))
			}
		case *ast.CompositeLit:
			if id, ok := x.Type.(*ast.Ident); ok && id.Name == "renditionGates" {
				gates = append(gates, x)
			}
		}
		return true
	})
	if len(gates) != 1 {
		t.Fatalf("runServe builds %d renditionGates literals, want 1: this test compares that one with the /v1 options", len(gates))
	}
	fields := map[string]string{}
	forEachKeyedElement(gates[0], func(key *ast.Ident, value ast.Expr) {
		if id, ok := value.(*ast.Ident); ok {
			fields[key.Name] = id.Name
		} else {
			fields[key.Name] = fmt.Sprintf("a %T", value)
		}
	})
	for field, option := range map[string]string{"upscale": "WithUpscale", "optimize": "WithCarPlayOptimize", "pcm": "WithDSDRender"} {
		v1 := options[option]
		if len(v1) != 1 {
			t.Errorf("the /v1 server's chain calls %s %d times (%v), want once", option, len(v1), v1)
			continue
		}
		if fields[field] != v1[0] {
			t.Errorf("renditionGates.%s at %s is %q, not %s, the closure %s gets: "+
				"a stale download could render what a request for the kind is refused",
				field, fset.Position(gates[0].Pos()), fields[field], v1[0], option)
		}
	}
}

// chainStartsAtAPINew reports whether x is a method chain rooted at
// `api.New(...)`: the /v1 server's option calls, as against another type's
// method of the same name (the coordinator's WithDSDRender takes the caps).
func chainStartsAtAPINew(x ast.Expr) bool {
	for {
		switch e := x.(type) {
		case *ast.CallExpr:
			if sel, ok := e.Fun.(*ast.SelectorExpr); ok {
				if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "api" && sel.Sel.Name == "New" {
					return true
				}
				x = sel.X
				continue
			}
			return false
		case *ast.SelectorExpr:
			x = e.X
		default:
			return false
		}
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
