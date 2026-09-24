package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// blankKeeper is a blank reference whose only effect is to keep a name
// referenced: `var _ = E` at the top of a file, or `_ = E` / `var _ = E` in
// a function, where E does nothing but name things (identifiers, selectors,
// literals, composite literals of those, type conversions). The name is
// almost always an import ("silence unused-import warning"), and the keeper
// is dead code either way. Imports are per FILE: when the file uses the
// package elsewhere the keeper does nothing, and when it is the only use it
// keeps an import nothing needs.
type blankKeeper struct {
	file string // slash path relative to the scanned root
	line int
	form string // "var _ = " or "_ = ", as written
	top  bool   // a top-level declaration, not a statement in a function
	src  string // E as written, whitespace collapsed
	doc  string // the keeper's doc, whitespace collapsed; top-level form only
	// pkgs maps each import E names, by its name in the file, to how many
	// selector uses of it the file has outside its keepers. It is empty for
	// a keeper of the package's own declarations (`var _ = logger`), which
	// only the top-level form reports.
	pkgs map[string]int
}

// allowedKeeper is a keeper this sweep lets stand, and it stands on the
// condition its own doc names, not on its path. The entry is checked four
// ways on every run: the keeper must still exist, its doc must still say
// docSays, conditionMet must still find nothing, and the file must still
// use the import nowhere else (once it does, the keeper keeps nothing, however
// the condition came to be met). Each failure is reported, so an allowance
// ends when its reason does instead of outliving it.
type allowedKeeper struct {
	file    string // slash path relative to the repo root
	src     string // the keeper's expression, as written
	docSays string // the condition, as the keeper's doc states it
	// conditionMet names the declaration under root that meets the
	// condition, or returns "" while it is still unmet.
	conditionMet func(root string) (string, error)
}

// allowedKeepers lists the one keeper whose doc names a condition not yet
// met. internal/tsnet still returns errors.New strings from Status and
// ListenTLS, and its tests match them by substring; the keeper waits for the
// typed errors errors.Is could match. typedErrorIn decides when they have
// landed.
var allowedKeepers = []allowedKeeper{{
	file:    "internal/tsnet/tsnet_test.go",
	src:     "errors.Is",
	docSays: "Don't remove until typed errors land.",
	conditionMet: func(root string) (string, error) {
		return typedErrorIn(root, "internal/tsnet")
	},
}}

// TestNoBlankKeepers fails on every blank keeper in the tree (see
// blankKeeper), naming for each import whether the file uses it elsewhere
// (the keeper does nothing) or nowhere else (delete the import too). The
// top-level form is also reported when it names only its package's own
// declarations: Go never reports an unused package-level declaration, so
// such a keeper silences nothing at all.
//
// Measured before it was written (#996): 37 trees sampled every 30 commits
// along main's first-parent history held 24 distinct keepers. Three hand
// sweeps removed nine (c062ac95, #855, #994), each covering only its own
// scope, and #825 added one between two of them. #996 removed the other
// fourteen and left allowedKeepers' one. Every blank reference of these
// shapes in those trees was a keeper, and no other blank form ever held
// one (`var _ T`, `const _`, `type _`, `func _`), so the sweep reads only
// these. Such a reference can check at most that a name exists
// (`(*manifest.Store)(nil)` had a doc claiming it checked the store's
// methods), and every real use of the name makes that check; a name nothing
// uses needs none. The typed form, `var _ I = (*T)(nil)` or `var _
// func(*T) error = (*T).M`, is how Go states a compile-time assertion, and
// is never read.
//
// On a clean tree this finds nothing, so the tree cannot show that it still
// can. TestBlankKeeperScanOnFixtures runs the same scan over synthetic trees
// whose findings are known.
func TestNoBlankKeepers(t *testing.T) {
	scanBlankKeepers(t, repoRootForCitations(t), allowedKeepers, true)
}

// keeperVerdict is judgeBlankKeepers' answer about one keeper, or about an
// allowance that matched none.
type keeperVerdict struct {
	pos string // file:line, or the allowance's file when its keeper is gone
	src string
	why string // one of the keeper* constants
	msg string
}

const (
	keeperRedundant  = "redundant"         // the file uses every package it names elsewhere
	keeperOnlyUse    = "only-use"          // it is an import's only use in the file
	keeperLocal      = "local"             // it names only its package's own declarations
	keeperAllowed    = "allowed"           // an allowedKeepers entry whose condition is unmet
	keeperMet        = "condition-met"     // an allowance whose condition has been met
	keeperServed     = "allowance-served"  // an allowance whose import the file now uses elsewhere
	keeperUnstated   = "condition-dropped" // an allowance whose keeper's doc no longer states it
	keeperStaleEntry = "stale-allowance"   // an allowance with no keeper left to allow
)

// scanBlankKeepers is TestNoBlankKeepers' scan of the tree under root,
// reported through r (docblock_subject_test.go's reporter): an Errorf per
// finding, a Logf per allowed keeper. It returns every verdict. wholeTree
// holds the scan to the floors that prove it reached this repo's whole tree;
// a fixture has none of those to meet.
func scanBlankKeepers(r docScanReporter, root string, allowed []allowedKeeper, wholeTree bool) []keeperVerdict {
	keepers, nonTest, test, err := findBlankKeepers(root)
	if err != nil {
		r.Fatalf("%v", err)
		return nil
	}
	if wholeTree && (nonTest < 100 || test < 100) {
		r.Fatalf("walked %d non-test and %d test .go files, want >=100 of each — the scan is not seeing the tree", nonTest, test)
		return nil
	}
	verdicts, err := judgeBlankKeepers(root, keepers, allowed)
	if err != nil {
		r.Fatalf("%v", err)
		return nil
	}
	for _, v := range verdicts {
		if v.why == keeperAllowed {
			r.Logf("%s: %s", v.pos, v.msg)
			continue
		}
		r.Errorf("%s: %s", v.pos, v.msg)
	}
	return verdicts
}

// judgeBlankKeepers returns a verdict for every keeper, in file order, and
// one for every allowance that matched no keeper. An allowance matches only
// a top-level keeper.
func judgeBlankKeepers(root string, keepers []blankKeeper, allowed []allowedKeeper) ([]keeperVerdict, error) {
	var out []keeperVerdict
	used := make([]bool, len(allowed))
	for _, k := range keepers {
		pos := fmt.Sprintf("%s:%d", k.file, k.line)
		why, msg := k.ordinaryVerdict()
		for i, a := range allowed {
			if !k.top || a.file != k.file || a.src != k.src {
				continue
			}
			used[i] = true
			if !strings.Contains(k.doc, a.docSays) {
				why = keeperUnstated
				msg = fmt.Sprintf("`%s%s` is allowed only while its doc says %q, and it no longer does: %s", k.form, k.src, a.docSays, msg)
				continue
			}
			if why == keeperRedundant {
				why = keeperServed
				msg = fmt.Sprintf("`%s%s` kept its import for a use the file now has, so it keeps nothing: %s", k.form, k.src, msg)
				continue
			}
			met, err := a.conditionMet(root)
			if err != nil {
				return nil, fmt.Errorf("%s: checking the condition `%s%s` waits for: %w", pos, k.form, k.src, err)
			}
			if met != "" {
				why = keeperMet
				msg = fmt.Sprintf("`%s%s` waited for %q, and %s. Write the check it was kept for, then delete it", k.form, k.src, a.docSays, met)
			} else {
				why = keeperAllowed
				msg = fmt.Sprintf("`%s%s` allowed: its doc says %q, and that has not happened yet", k.form, k.src, a.docSays)
			}
		}
		out = append(out, keeperVerdict{pos, k.src, why, msg})
	}
	for i, a := range allowed {
		if !used[i] {
			out = append(out, keeperVerdict{a.file, a.src, keeperStaleEntry,
				fmt.Sprintf("allowedKeepers lets `var _ = %s` stand here, and the file no longer has it. Delete the entry", a.src)})
		}
	}
	return out, nil
}

// ordinaryVerdict is the finding for k when no allowance applies.
func (k blankKeeper) ordinaryVerdict() (why, msg string) {
	shape := k.form + k.src
	if len(k.pkgs) == 0 {
		return keeperLocal, fmt.Sprintf("`%s` names only its package's own declarations. Go never reports an unused package-level declaration, so it silences nothing: delete it, and whatever it kept if nothing else uses that", shape)
	}
	names := make([]string, 0, len(k.pkgs))
	for name := range k.pkgs {
		names = append(names, name)
	}
	sort.Strings(names)
	var alone, used []string
	for _, name := range names {
		switch n := k.pkgs[name]; n {
		case 0:
			alone = append(alone, name)
		case 1:
			used = append(used, name+" (1 use)")
		default:
			used = append(used, fmt.Sprintf("%s (%d uses)", name, n))
		}
	}
	if len(alone) > 0 {
		return keeperOnlyUse, fmt.Sprintf("`%s`: nothing in this file but a keeper uses %s, so the import is kept for nothing (imports are per file). Delete it, and the import", shape, strings.Join(alone, " and "))
	}
	return keeperRedundant, fmt.Sprintf("`%s` does nothing, since this file uses %s elsewhere. Delete it", shape, strings.Join(used, " and "))
}

// findBlankKeepers returns every blank keeper in the Go files under root
// that the go tool reads, with how many non-test and test files it walked.
// It skips vendor, testdata and node_modules, a nested module, and a file
// or directory the go tool ignores (goToolIgnores), before opening it.
func findBlankKeepers(root string) (keepers []blankKeeper, nonTest, test int, err error) {
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if path == root {
				return nil
			}
			if goToolIgnores(name) || name == "vendor" || name == "testdata" || name == "node_modules" {
				return filepath.SkipDir
			}
			if _, statErr := os.Stat(filepath.Join(path, "go.mod")); statErr == nil {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") || goToolIgnores(name) {
			return nil
		}
		if strings.HasSuffix(name, "_test.go") {
			test++
		} else {
			nonTest++
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		found, err := blankKeepersInFile(filepath.ToSlash(rel), src)
		if err != nil {
			return err
		}
		keepers = append(keepers, found...)
		return nil
	})
	return keepers, nonTest, test, err
}

// blankKeepersInFile returns the keepers in one file's source.
//
// A selector `x.Sel` names an import when x is one of the file's import
// names and the top-level declaration holding the selector declares no x of
// its own (declaredWithin). Nothing at the top level can shadow an import (a
// package-level x would collide with it); inside a function a local x may be
// what the selector names. Counting any x the declaration declares anywhere
// as a possible local makes the statement form miss a keeper rather than
// report a local variable, and makes a use count err low rather than high.
func blankKeepersInFile(rel string, src []byte) ([]blankKeeper, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, rel, src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	imports := map[string]bool{}
	for _, imp := range f.Imports {
		if name := importName(imp); name != "_" && name != "." {
			imports[name] = true
		}
	}
	text := func(n ast.Node) string {
		return strings.Join(strings.Fields(string(src[fset.Position(n.Pos()).Offset:fset.Position(n.End()).Offset])), " ")
	}

	uses := map[string]int{} // import name -> selector uses in the file
	var found []blankKeeper  // with pkgs holding each keeper's OWN uses
	for _, decl := range f.Decls {
		locals := declaredWithin(decl)
		ast.Inspect(decl, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok {
				if id, ok := sel.X.(*ast.Ident); ok && imports[id.Name] && !locals[id.Name] {
					uses[id.Name]++
				}
			}
			return true
		})
		keep := func(e ast.Expr, form string, top bool, doc *ast.CommentGroup) {
			if !onlyNames(e) {
				return
			}
			pkgs, others := namesIn(e, imports, locals)
			switch {
			case len(pkgs) > 0 && (top || !others):
			case len(pkgs) == 0 && top && others:
			default:
				// A statement naming a local (`_ = cfg`) marks it used,
				// and `var _ = 1` names nothing at all.
				return
			}
			k := blankKeeper{file: rel, line: fset.Position(e.Pos()).Line, form: form, top: top, src: text(e), pkgs: pkgs}
			if doc != nil {
				k.doc = strings.Join(strings.Fields(doc.Text()), " ")
			}
			found = append(found, k)
		}
		if gd, ok := decl.(*ast.GenDecl); ok && gd.Tok == token.VAR {
			for _, spec := range gd.Specs {
				vs := spec.(*ast.ValueSpec)
				doc := vs.Doc
				if doc == nil && !gd.Lparen.IsValid() {
					doc = gd.Doc
				}
				for _, e := range blankValues(vs) {
					keep(e, "var _ = ", true, doc)
				}
			}
		}
		// In a function, including one a top-level `var _ = func() {…}`
		// holds: `_ = E` and `var _ = E`.
		ast.Inspect(decl, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.AssignStmt:
				if x.Tok != token.ASSIGN || len(x.Lhs) != len(x.Rhs) {
					return true
				}
				for i, lhs := range x.Lhs {
					if id, ok := lhs.(*ast.Ident); ok && id.Name == "_" {
						keep(x.Rhs[i], "_ = ", false, nil)
					}
				}
			case *ast.DeclStmt:
				if gd, ok := x.Decl.(*ast.GenDecl); ok && gd.Tok == token.VAR {
					for _, spec := range gd.Specs {
						for _, e := range blankValues(spec.(*ast.ValueSpec)) {
							keep(e, "var _ = ", false, nil)
						}
					}
				}
			}
			return true
		})
	}
	// A keeper's use is no use: two keepers of one package (cmd/bridge's
	// update.go had them) keep each other's import alive, and deleting both
	// must delete it.
	inKeepers := map[string]int{}
	for i := range found {
		for name, own := range found[i].pkgs {
			inKeepers[name] += own
		}
	}
	for i := range found {
		for name := range found[i].pkgs {
			found[i].pkgs[name] = uses[name] - inKeepers[name]
		}
	}
	return found, nil
}

// blankValues returns the values vs gives its blank names. A typed spec
// asserts something (`var _ I = (*T)(nil)`), and a spec with fewer values
// than names takes a multi-value call, so neither returns any.
func blankValues(vs *ast.ValueSpec) []ast.Expr {
	if vs.Type != nil || len(vs.Values) != len(vs.Names) {
		return nil
	}
	var out []ast.Expr
	for i, name := range vs.Names {
		if name.Name == "_" {
			out = append(out, vs.Values[i])
		}
	}
	return out
}

// onlyNames reports whether evaluating e can do nothing but name things:
// identifiers, selectors, literals, composite literals of those, and
// conversions to a type that cannot be a function. A call whose callee could
// be a function is refused (`pkg.T(x)` could be either), so `var _ =
// registry.Register(x)` and a compile-time size assertion are never keepers.
func onlyNames(e ast.Expr) bool {
	switch x := e.(type) {
	case *ast.Ident, *ast.BasicLit:
		return true
	case *ast.SelectorExpr:
		return onlyNames(x.X)
	case *ast.ParenExpr:
		return onlyNames(x.X)
	case *ast.StarExpr:
		return onlyNames(x.X)
	case *ast.ArrayType, *ast.MapType, *ast.ChanType, *ast.FuncType, *ast.InterfaceType, *ast.StructType:
		return true
	case *ast.CompositeLit:
		for _, elt := range x.Elts {
			if kv, ok := elt.(*ast.KeyValueExpr); ok {
				elt = kv.Value
				if !onlyNames(kv.Key) {
					return false
				}
			}
			if !onlyNames(elt) {
				return false
			}
		}
		return true
	case *ast.CallExpr:
		return len(x.Args) == 1 && isTypeOnly(x.Fun) && onlyNames(x.Args[0])
	}
	return false
}

// isTypeOnly reports whether e can only be a type, so a call of it is a
// conversion.
func isTypeOnly(e ast.Expr) bool {
	switch x := e.(type) {
	case *ast.ParenExpr:
		return isTypeOnly(x.X)
	case *ast.StarExpr, *ast.ArrayType, *ast.MapType, *ast.ChanType, *ast.FuncType, *ast.InterfaceType, *ast.StructType:
		return true
	}
	return false
}

// namesIn returns how many times e names each import (by selector, and not
// where a local of that name may be meant), and whether it names anything
// else: a local or package-level name. A predeclared name (nil, int, error)
// is neither. A composite literal's key counts as a name even when it is a
// struct field's: in a map literal the same identifier is a variable, the two
// look alike without types, and skipping keys reported
// `_ = map[string]int{key: http.StatusOK}`, whose deletion leaves the local
// key unused (a Gemini consult on #996). The statement form misses
// `_ = url.URL{Scheme: "https"}` instead, which is the safe direction.
func namesIn(e ast.Expr, imports, locals map[string]bool) (pkgs map[string]int, others bool) {
	pkgs = map[string]int{}
	ast.Inspect(e, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.SelectorExpr:
			if id, ok := x.X.(*ast.Ident); ok && imports[id.Name] && !locals[id.Name] {
				pkgs[id.Name]++
				return false
			}
		case *ast.Ident:
			if types.Universe.Lookup(x.Name) == nil {
				others = true
			}
		}
		return true
	})
	return pkgs, others
}

// declaredWithin returns every name decl declares below its own top-level
// names: a function's receiver, type parameters, parameters and results, and
// anything declared in its body or in a function literal inside it. Scope is
// over-approximated on purpose, a name declared in one block counting for
// all of decl (see blankKeepersInFile).
func declaredWithin(decl ast.Decl) map[string]bool {
	names := map[string]bool{}
	fields := func(fl *ast.FieldList) {
		if fl == nil {
			return
		}
		for _, field := range fl.List {
			for _, n := range field.Names {
				names[n.Name] = true
			}
		}
	}
	ident := func(e ast.Expr) {
		if id, ok := e.(*ast.Ident); ok {
			names[id.Name] = true
		}
	}
	ast.Inspect(decl, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.FuncDecl:
			fields(x.Recv)
		case *ast.FuncType:
			fields(x.TypeParams)
			fields(x.Params)
			fields(x.Results)
		case *ast.AssignStmt:
			if x.Tok == token.DEFINE {
				for _, lhs := range x.Lhs {
					ident(lhs)
				}
			}
		case *ast.RangeStmt:
			if x.Tok == token.DEFINE {
				ident(x.Key)
				ident(x.Value)
			}
		case *ast.DeclStmt:
			if gd, ok := x.Decl.(*ast.GenDecl); ok {
				for _, spec := range gd.Specs {
					switch s := spec.(type) {
					case *ast.ValueSpec:
						for _, n := range s.Names {
							names[n.Name] = true
						}
					case *ast.TypeSpec:
						names[s.Name.Name] = true
					}
				}
			}
		}
		return true
	})
	return names
}

// importName is the name an import is known by in its file: the explicit
// one, else the last path element without a major-version element ("/v2")
// or suffix (".v3"), a "go-" prefix or a "-go" suffix. That is the package's
// own name for every unnamed import in this tree; the one path it gets
// wrong, github.com/prometheus/client_model/go, is only ever imported under
// an explicit name.
func importName(imp *ast.ImportSpec) string {
	if imp.Name != nil {
		return imp.Name.Name
	}
	path, err := strconv.Unquote(imp.Path.Value)
	if err != nil {
		return ""
	}
	parts := strings.Split(path, "/")
	name := parts[len(parts)-1]
	if len(parts) > 1 && majorVersionElem.MatchString(name) {
		name = parts[len(parts)-2]
	}
	name = majorVersionSuffix.ReplaceAllString(name, "")
	name = strings.TrimPrefix(name, "go-")
	return strings.TrimSuffix(name, "-go")
}

var (
	majorVersionElem   = regexp.MustCompile(`^v[0-9]+$`)
	majorVersionSuffix = regexp.MustCompile(`\.v[0-9]+$`)
	// sentinelName is Go's naming convention for a sentinel error.
	sentinelName = regexp.MustCompile(`^[Ee]rr[A-Z0-9_]`)
)

// typedErrorIn names the first top-level declaration in dir's non-test
// files that gives its package a typed error, or returns "" when there is
// none: a sentinel (a var or const named ErrX or errX, or initialised by
// errors.New, errors.Join, fmt.Errorf or another package's sentinel), or a
// method `Error() string`, which makes its receiver an error type. An
// errors.New inside a function, which is what internal/tsnet returns today,
// is neither.
//
// It lists the directory rather than globbing it: under a checkout whose path
// holds `[`, the root joins the pattern, matches nothing, and reads as "no
// typed errors" for ever (Gemini on #996).
func typedErrorIn(root, dir string) (string, error) {
	entries, err := os.ReadDir(filepath.Join(root, filepath.FromSlash(dir)))
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || goToolIgnores(name) || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(root, filepath.FromSlash(dir), name)
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return "", err
		}
		at := func(n ast.Node) string {
			return fmt.Sprintf("%s/%s:%d", dir, name, fset.Position(n.Pos()).Line)
		}
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for i, n := range vs.Names {
						if sentinelName.MatchString(n.Name) || (i < len(vs.Values) && isSentinelValue(vs.Values[i])) {
							return fmt.Sprintf("%s declares %s", at(n), n.Name), nil
						}
					}
				}
			case *ast.FuncDecl:
				if d.Recv == nil || d.Name.Name != "Error" || d.Type.Params.NumFields() != 0 || d.Type.Results.NumFields() != 1 {
					continue
				}
				if id, ok := d.Type.Results.List[0].Type.(*ast.Ident); ok && id.Name == "string" {
					return fmt.Sprintf("%s declares an Error() string method", at(d)), nil
				}
			}
		}
	}
	return "", nil
}

// isSentinelValue reports whether e makes the var it initialises a sentinel:
// a call of errors.New, errors.Join or fmt.Errorf, or another package's
// sentinel (`net.ErrClosed`).
func isSentinelValue(e ast.Expr) bool {
	if call, ok := e.(*ast.CallExpr); ok {
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		pkg, ok := sel.X.(*ast.Ident)
		return ok && (pkg.Name == "errors" && (sel.Sel.Name == "New" || sel.Sel.Name == "Join") || pkg.Name == "fmt" && sel.Sel.Name == "Errorf")
	}
	sel, ok := e.(*ast.SelectorExpr)
	return ok && sentinelName.MatchString(sel.Sel.Name)
}

// TestBlankKeeperScanOnFixtures runs scanBlankKeepers, with the real
// allowedKeepers, over synthetic trees whose findings are known. On this
// repo's own tree a clean scan reports nothing, so a change that lost a
// shape, a skip rule, the scope check or an allowance's condition would pass
// there unseen. The quiet shapes are the ones the scan must leave alone, each
// for the reason its comment gives; the allowance cases walk it through every
// state its entry can be in.
func TestBlankKeeperScanOnFixtures(t *testing.T) {
	base := map[string]string{
		"keep/redundant.go": `package keep

import "fmt"

func say() string { return fmt.Sprint("x") }

// quiet the import.
var _ = fmt.Sprintf
`,
		"keep/only_use_test.go": `package keep

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

var _ = io.Copy

var (
	_ = (*strings.Builder)(nil)
)

func TestIt(t *testing.T) {
	_ = http.ErrServerClosed
	var _ = time.Second
}
`,
		// Two keepers of one package keep each other's import alive.
		"keep/twice.go": `package keep

import "strconv"

func a() { _ = strconv.IntSize }

func b() { _ = strconv.IntSize }
`,
		// A predeclared name (nil) is not a local, so a statement naming one
		// beside a package is still a keeper.
		"keep/statements.go": `package keep

import "bytes"

func c() { _ = (*bytes.Buffer)(nil) }
`,
		"keep/local.go": `package keep

var logger = struct{}{}

var _ = logger
`,
		// A use of the package's name where a local of that name may be
		// meant does not count as a use of the import.
		"keep/shadow.go": `package keep

import "path"

func base() string {
	path := struct{ Base string }{}
	return path.Base
}

var _ = path.Join
`,
		// Each import's name is its package's: a ".v3" suffix, a "go-"
		// prefix and a "/v2" element are not part of it.
		"keep/names.go": `package keep

import (
	"github.com/example/tool/v2"
	"github.com/mattn/go-isatty"
	"gopkg.in/yaml.v3"
)

var _ = tool.Run
var _ = isatty.IsTerminal
var _ = yaml.Marshal
`,
		"keep/quiet.go": `package keep

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"unsafe"
)

type iface interface{ M() }

type impl struct{}

func (impl) M() {}

// A typed blank asserts an interface.
var _ iface = impl{}

// A call could do anything, a registration for one.
var _ = errors.New("registered")

// A compile-time size assertion indexes and calls.
var _ = [1]struct{}{}[unsafe.Sizeof(int64(0))-8]

// A literal, or one of a predeclared type, keeps nothing.
var _ = 1
var _ = [2]int{}

func use(cfg struct{ Name string }) {
	_ = cfg                    // marks a local used
	_ = cfg.Name               // a local's field
	_ = []any{fmt.Sprint, cfg} // marks a local used, beside a package
	_ = fmt.Sprint()           // a call
	os := struct{ Args []string }{}
	_ = os.Args // os is the local here, not the import
	key := "k"
	_ = map[string]any{key: fmt.Sprint} // the key is a local, too
	_ = url.URL{Scheme: "https"}        // a field key reads as a name: missed, the safe way
}
`,
		// Nothing the go tool ignores is read, nor another module, nor
		// vendored or node_modules code. The lock file is not Go, so opening
		// it would fail the scan.
		"keep/.#redundant.go":     "user@host.4242:1700000000",
		"_scratch/s.go":           "package s\n\nimport \"io\"\n\nvar _ = io.EOF\n",
		"keep/testdata/t.go":      "package t\n\nimport \"io\"\n\nvar _ = io.EOF\n",
		"nested/go.mod":           "module nested\n",
		"nested/n.go":             "package n\n\nimport \"io\"\n\nvar _ = io.EOF\n",
		"vendor/v/v.go":           "package v\n\nimport \"io\"\n\nvar _ = io.EOF\n",
		"web/node_modules/m/m.go": "package m\n\nimport \"io\"\n\nvar _ = io.EOF\n",
		"internal/tsnet/ts.go":    "package tsnet\n\nimport \"errors\"\n\nfunc Status() error { return errors.New(\"tsnet: Status called before Start\") }\n",
		"internal/tsnet/tsnet_test.go": `package tsnet

import "errors"

// This blank reference keeps errors imported for the check typed errors
// will allow. Don't remove until typed errors land.
var _ = errors.Is
`,
	}
	shared := []string{
		"keep/local.go: logger: " + keeperLocal,
		"keep/names.go: isatty.IsTerminal: " + keeperOnlyUse,
		"keep/names.go: tool.Run: " + keeperOnlyUse,
		"keep/names.go: yaml.Marshal: " + keeperOnlyUse,
		"keep/only_use_test.go: (*strings.Builder)(nil): " + keeperOnlyUse,
		"keep/only_use_test.go: http.ErrServerClosed: " + keeperOnlyUse,
		"keep/only_use_test.go: io.Copy: " + keeperOnlyUse,
		"keep/only_use_test.go: time.Second: " + keeperOnlyUse,
		"keep/redundant.go: fmt.Sprintf: " + keeperRedundant,
		"keep/shadow.go: path.Join: " + keeperOnlyUse,
		"keep/statements.go: (*bytes.Buffer)(nil): " + keeperOnlyUse,
		"keep/twice.go: strconv.IntSize: " + keeperOnlyUse,
		"keep/twice.go: strconv.IntSize: " + keeperOnlyUse,
	}
	for _, c := range []struct {
		name    string
		changes map[string]string // path -> new content, "" to delete
		tsnet   string            // the verdict on the tsnet keeper
	}{
		{"condition unmet", nil, "internal/tsnet/tsnet_test.go: errors.Is: " + keeperAllowed},
		{"a sentinel meets it by its name", map[string]string{
			"internal/tsnet/err.go": "package tsnet\n\nvar ErrNotStarted = newError(\"tsnet: not started\")\n",
		}, "internal/tsnet/tsnet_test.go: errors.Is: " + keeperMet},
		{"a sentinel meets it by its initialiser", map[string]string{
			"internal/tsnet/err.go": "package tsnet\n\nimport \"errors\"\n\nvar notStarted = errors.New(\"tsnet: not started\")\n",
		}, "internal/tsnet/tsnet_test.go: errors.Is: " + keeperMet},
		{"a sentinel meets it by another package's", map[string]string{
			"internal/tsnet/err.go": "package tsnet\n\nimport \"net\"\n\nvar closed = net.ErrClosed\n",
		}, "internal/tsnet/tsnet_test.go: errors.Is: " + keeperMet},
		{"an error type meets it", map[string]string{
			"internal/tsnet/err.go": "package tsnet\n\ntype notStarted struct{}\n\nfunc (notStarted) Error() string { return \"not started\" }\n",
		}, "internal/tsnet/tsnet_test.go: errors.Is: " + keeperMet},
		{"the doc drops the condition", map[string]string{
			"internal/tsnet/tsnet_test.go": "package tsnet\n\nimport \"errors\"\n\n// This blank reference keeps errors imported.\nvar _ = errors.Is\n",
		}, "internal/tsnet/tsnet_test.go: errors.Is: " + keeperUnstated},
		{"the file uses the import now", map[string]string{
			"internal/tsnet/tsnet_test.go": "package tsnet\n\nimport \"errors\"\n\n// This blank reference keeps errors imported for the check typed errors\n// will allow. Don't remove until typed errors land.\nvar _ = errors.Is\n\nfunc typed(err error) bool { return errors.Is(err, nil) }\n",
		}, "internal/tsnet/tsnet_test.go: errors.Is: " + keeperServed},
		{"the keeper is gone", map[string]string{
			"internal/tsnet/tsnet_test.go": "",
		}, "internal/tsnet/tsnet_test.go: errors.Is: " + keeperStaleEntry},
	} {
		t.Run(c.name, func(t *testing.T) {
			// A checkout's path may hold glob metacharacters.
			root := filepath.Join(t.TempDir(), "clone[1]")
			for rel, src := range base {
				if next, changed := c.changes[rel]; changed {
					src = next
				}
				if src == "" {
					continue
				}
				writeKeeperFixture(t, root, rel, src)
			}
			for rel, src := range c.changes {
				if _, inBase := base[rel]; !inBase && src != "" {
					writeKeeperFixture(t, root, rel, src)
				}
			}
			rec := &docScanRecorder{t: t}
			verdicts := scanBlankKeepers(rec, root, allowedKeepers, false)
			var got []string
			for _, v := range verdicts {
				file, _, _ := strings.Cut(v.pos, ":")
				got = append(got, file+": "+v.src+": "+v.why)
			}
			want := append(append([]string(nil), shared...), c.tsnet)
			sort.Strings(got)
			sort.Strings(want)
			if strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Errorf("verdicts:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
			}
			// Every verdict but an allowance is reported as an error.
			wantErrors := len(want)
			if strings.HasSuffix(c.tsnet, keeperAllowed) {
				wantErrors--
			}
			if len(rec.errors) != wantErrors {
				t.Errorf("reported %d errors, want %d:\n  %s", len(rec.errors), wantErrors, strings.Join(rec.errors, "\n  "))
			}
		})
	}
}

// writeKeeperFixture writes src to rel under root, creating its directory.
func writeKeeperFixture(t *testing.T, root, rel, src string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
}
