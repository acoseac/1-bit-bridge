package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// dsdReadTokens are the vocabularies of the DSD read side (B728 B8), each
// read from the definitions that emit it, never from a second list.
type dsdReadTokens struct {
	rungs, dithers, filters, refusals, warningCodes, warningReasons, modes, optionModes,
	caveatCodes, caveatTransports, blocked, transports, firstUse map[string]bool
}

// goPackage is one package's non-test files, parsed, and its constants by
// name.
type goPackage struct {
	files  map[string]*ast.File
	consts map[string]*ast.ValueSpec
}

// parseGoPackage parses dir's non-test files.
func parseGoPackage(t *testing.T, dir string) goPackage {
	t.Helper()
	p := goPackage{files: map[string]*ast.File{}, consts: map[string]*ast.ValueSpec{}}
	names, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil || len(names) == 0 {
		t.Fatalf("source glob %s: %v", dir, err)
	}
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		p.files[filepath.Base(name)] = f
		for _, d := range f.Decls {
			g, ok := d.(*ast.GenDecl)
			if !ok || g.Tok != token.CONST {
				continue
			}
			for _, s := range g.Specs {
				v := s.(*ast.ValueSpec)
				for _, n := range v.Names {
					p.consts[n.Name] = v
				}
			}
		}
	}
	return p
}

// tokenResolver resolves a constant expression of the packages it holds, by
// the name each is imported as.
type tokenResolver map[string]goPackage

// value is e's string, e a literal, a constant of pkg's or of a package by
// its name, or a conversion of one.
func (r tokenResolver) value(t *testing.T, pkg string, e ast.Expr) string {
	t.Helper()
	switch x := e.(type) {
	case *ast.BasicLit:
		return protocolString(t, x)
	case *ast.Ident:
		return r.constant(t, pkg, x.Name)
	case *ast.SelectorExpr:
		if id, ok := x.X.(*ast.Ident); ok {
			if _, known := r[id.Name]; known {
				return r.constant(t, id.Name, x.Sel.Name)
			}
		}
	case *ast.CallExpr:
		if len(x.Args) == 1 {
			return r.value(t, pkg, x.Args[0])
		}
	}
	t.Fatalf("%s: token expression %T needs a resolved string constant", pkg, e)
	return ""
}

// constant is name's value in pkg.
func (r tokenResolver) constant(t *testing.T, pkg, name string) string {
	t.Helper()
	spec, ok := r[pkg].consts[name]
	if !ok {
		t.Fatalf("%s.%s: no such constant", pkg, name)
	}
	for i, n := range spec.Names {
		if n.Name == name && i < len(spec.Values) {
			return r.value(t, pkg, spec.Values[i])
		}
	}
	t.Fatalf("%s.%s: no value", pkg, name)
	return ""
}

// named is every constant of pkg's whose name has prefix, by value.
func (r tokenResolver) named(t *testing.T, pkg, prefix string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for name := range r[pkg].consts {
		if strings.HasPrefix(name, prefix) {
			out[r.constant(t, pkg, name)] = true
		}
	}
	return out
}

// typed is every constant of pkg's declared with type typ, by value.
func (r tokenResolver) typed(t *testing.T, pkg, typ string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for name, spec := range r[pkg].consts {
		if id, ok := spec.Type.(*ast.Ident); ok && id.Name == typ {
			out[r.constant(t, pkg, name)] = true
		}
	}
	return out
}

// keyed is, in the funcs of pkg's file named (every func when funcs is
// empty), the value of key in each composite literal of type typ.
func (r tokenResolver) keyed(t *testing.T, pkg, file, typ, key string, funcs ...string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	f, ok := r[pkg].files[file]
	if !ok {
		t.Fatalf("%s has no %s", pkg, file)
	}
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || len(funcs) > 0 && !slices.Contains(funcs, fn.Name.Name) {
			continue
		}
		ast.Inspect(fn, func(n ast.Node) bool {
			c, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			if id, ok := c.Type.(*ast.Ident); !ok || id.Name != typ {
				return true
			}
			for _, el := range c.Elts {
				if kv, ok := el.(*ast.KeyValueExpr); ok {
					if k, ok := kv.Key.(*ast.Ident); ok && k.Name == key && !r.policyWord(kv.Value) {
						out[r.value(t, pkg, kv.Value)] = true
					}
				}
			}
			return true
		})
	}
	return out
}

// policyWord reports whether e is a refusal's own word read from a policy
// plan, string(<plan>.Reason), which the policy's own refusals list.
func (r tokenResolver) policyWord(e ast.Expr) bool {
	c, ok := e.(*ast.CallExpr)
	if !ok || len(c.Args) != 1 {
		return false
	}
	s, ok := c.Args[0].(*ast.SelectorExpr)
	if !ok || s.Sel.Name != "Reason" {
		return false
	}
	if id, ok := s.X.(*ast.Ident); ok {
		if _, isPkg := r[id.Name]; isPkg {
			return false
		}
	}
	return true
}

// refusalsOf is the second argument of each call of refused in pkg's file,
// in the funcs named.
func (r tokenResolver) refusalsOf(t *testing.T, pkg, file string, funcs ...string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, d := range r[pkg].files[file].Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || !slices.Contains(funcs, fn.Name.Name) {
			continue
		}
		ast.Inspect(fn, func(n ast.Node) bool {
			c, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if id, ok := c.Fun.(*ast.Ident); ok && id.Name == "refused" && len(c.Args) == 3 {
				out[r.value(t, pkg, c.Args[1])] = true
			}
			return true
		})
	}
	return out
}

// dsdReadSideTokens reads the read side's vocabularies from the tree at
// root: the evidence ladder (policy.Rung), the converter's dither words and
// filter name, the words a rate is refused with (the policy's, from the
// functions that decide a rate it decides, and the backend's for a set it
// could not compute), the state's warning codes and reasons, modes and
// transports, the modes the read side lists an option for, the profile
// database's caveat codes and the transports its lint lets a caveat name,
// the gain check's words that block DoP, and first use's states.
func dsdReadSideTokens(t *testing.T, root string) dsdReadTokens {
	t.Helper()
	r := tokenResolver{}
	for name, dir := range map[string]string{"policy": "internal/player/policy", "dsdstate": "internal/player/dsdstate", "dacprofile": "internal/player/dacprofile",
		"dsdconv": "internal/player/dsdconv", "playback": "internal/player/playback"} {
		r[name] = parseGoPackage(t, filepath.Join(root, dir))
	}
	out := dsdReadTokens{
		rungs:          r.typed(t, "policy", "Rung"),
		dithers:        r.named(t, "dsdconv", "Dither"),
		filters:        map[string]bool{r.constant(t, "dsdconv", "FilterName"): true},
		warningCodes:   r.named(t, "dsdstate", "Warning"),
		warningReasons: r.named(t, "dsdstate", "Reason"),
		modes:          r.named(t, "dsdstate", "Mode"),
		transports:     r.named(t, "dsdstate", "Transport"),
		caveatCodes:    r.named(t, "dacprofile", "Caveat"),
		firstUse:       r.named(t, "playback", "FirstUse"),
		blocked:        r.keyed(t, "policy", "gain.go", "Gain", "Reason"),
	}
	// The policy decides DSD64 to DSD256 in Effective and decide, and refuses
	// in Intersect what a choice's write that landed uncertain leaves playing
	// two ways (B728 B9); At's own refusal is of a rate it does not decide,
	// which no refused entry is.
	out.refusals = r.refusalsOf(t, "policy", "policy.go", "Effective", "decide")
	for v := range r.refusalsOf(t, "policy", "choice.go", "Intersect") {
		out.refusals[v] = true
	}
	for v := range r.keyed(t, "policy", "policy.go", "Plan", "Reason", "Effective", "decide") {
		out.refusals[v] = true
	}
	for v := range r.keyed(t, "playback", "dsd_view.go", "DSDRefusal", "Reason") {
		out.refusals[v] = true
	}
	// The modes an option is listed for, by the read side's own list.
	out.optionModes = map[string]bool{}
	for _, f := range r["playback"].files {
		for _, d := range f.Decls {
			g, ok := d.(*ast.GenDecl)
			if !ok || g.Tok != token.VAR {
				continue
			}
			for _, s := range g.Specs {
				v := s.(*ast.ValueSpec)
				if len(v.Names) != 1 || v.Names[0].Name != "optionModes" {
					continue
				}
				list, ok := v.Values[0].(*ast.CompositeLit)
				if !ok {
					t.Fatal("unresolved optionModes initializer")
				}
				for _, e := range list.Elts {
					out.optionModes[r.value(t, "playback", e)] = true
				}
			}
		}
	}
	// The transports the lint lets a caveat name: its switch on the caveat's
	// transport, "" (both) being an absent member.
	out.caveatTransports = map[string]bool{}
	for _, d := range r["dacprofile"].files["lint.go"].Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "caveats" {
			continue
		}
		ast.Inspect(fn, func(n ast.Node) bool {
			sw, ok := n.(*ast.SwitchStmt)
			if !ok {
				return true
			}
			if s, ok := sw.Tag.(*ast.SelectorExpr); !ok || s.Sel.Name != "Transport" {
				return true
			}
			for _, st := range sw.Body.List {
				for _, e := range st.(*ast.CaseClause).List {
					if v := r.value(t, "dacprofile", e); v != "" {
						out.caveatTransports[v] = true
					}
				}
			}
			return false
		})
	}
	for name, m := range map[string]map[string]bool{"rungs": out.rungs, "dithers": out.dithers, "refusals": out.refusals, "warning codes": out.warningCodes,
		"warning reasons": out.warningReasons, "modes": out.modes, "option modes": out.optionModes, "caveat codes": out.caveatCodes,
		"caveat transports": out.caveatTransports, "blocked": out.blocked, "transports": out.transports, "first use": out.firstUse} {
		if len(m) == 0 {
			t.Fatalf("no %s were read from the tree", name)
		}
		if m[""] {
			t.Fatalf("%s: an empty word, which the wire sends as an absent member", name)
		}
	}
	return out
}
