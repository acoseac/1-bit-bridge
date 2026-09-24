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
// literals, operators and composite literals of those, type conversions).
// The name is almost always an import ("silence unused-import warning"), and
// the keeper is dead code either way. Imports are per FILE: when the file
// uses the package elsewhere the keeper does nothing, and when it is the
// only use it keeps an import nothing needs.
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
// one for every allowance that matched no keeper.
func judgeBlankKeepers(root string, keepers []blankKeeper, allowed []allowedKeeper) ([]keeperVerdict, error) {
	var out []keeperVerdict
	used := make([]bool, len(allowed))
	for _, k := range keepers {
		v := keeperVerdict{pos: fmt.Sprintf("%s:%d", k.file, k.line), src: k.src}
		v.why, v.msg = k.ordinaryVerdict()
		if i := allowanceFor(k, allowed); i >= 0 {
			used[i] = true
			var err error
			if v.why, v.msg, err = judgeAllowance(root, k, allowed[i], v.why, v.msg); err != nil {
				return nil, fmt.Errorf("%s: %w", v.pos, err)
			}
		}
		out = append(out, v)
	}
	for i, a := range allowed {
		if !used[i] {
			out = append(out, keeperVerdict{a.file, a.src, keeperStaleEntry,
				fmt.Sprintf("allowedKeepers lets `var _ = %s` stand here, and the file no longer has it. Delete the entry", a.src)})
		}
	}
	return out, nil
}

// allowanceFor returns the index of the entry in allowed that k is, or -1.
// Only a top-level keeper can be one.
func allowanceFor(k blankKeeper, allowed []allowedKeeper) int {
	for i, a := range allowed {
		if k.top && a.file == k.file && a.src == k.src {
			return i
		}
	}
	return -1
}

// judgeAllowance is the verdict on keeper k, which entry a allows; why and
// msg are k's ordinary verdict, which is what it becomes when the doc no
// longer states the condition.
func judgeAllowance(root string, k blankKeeper, a allowedKeeper, why, msg string) (string, string, error) {
	shape := k.form + k.src
	if !strings.Contains(k.doc, a.docSays) {
		return keeperUnstated, fmt.Sprintf("`%s` is allowed only while its doc says %q, and it no longer does: %s", shape, a.docSays, msg), nil
	}
	if why == keeperRedundant {
		return keeperServed, fmt.Sprintf("`%s` kept its import for a use the file now has, so it keeps nothing: %s", shape, msg), nil
	}
	met, err := a.conditionMet(root)
	if err != nil {
		return "", "", fmt.Errorf("checking the condition `%s` waits for: %w", shape, err)
	}
	if met != "" {
		return keeperMet, fmt.Sprintf("`%s` waited for %q, and %s. Write the check it was kept for, then delete it", shape, a.docSays, met), nil
	}
	return keeperAllowed, fmt.Sprintf("`%s` allowed: its doc says %q, and that has not happened yet", shape, a.docSays), nil
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
		noun, verb := "the import", "is"
		if len(alone) > 1 {
			noun, verb = "the imports", "are"
		}
		return keeperOnlyUse, fmt.Sprintf("`%s`: nothing in this file but a keeper uses %s, so %s %s kept for nothing (imports are per file). Delete it, and %s", shape, strings.Join(alone, " and "), noun, verb, noun)
	}
	return keeperRedundant, fmt.Sprintf("`%s` does nothing, since this file uses %s elsewhere. Delete it", shape, strings.Join(used, " and "))
}

// findBlankKeepers returns every blank keeper in the Go files under root
// that the go tool reads, with how many non-test and test files it walked.
func findBlankKeepers(root string) (keepers []blankKeeper, nonTest, test int, err error) {
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return keeperWalkDir(root, path, d.Name())
		}
		if name := d.Name(); !strings.HasSuffix(name, ".go") || goToolIgnores(name) {
			return nil
		}
		if strings.HasSuffix(path, "_test.go") {
			test++
		} else {
			nonTest++
		}
		found, err := blankKeepersInPath(root, path)
		keepers = append(keepers, found...)
		return err
	})
	return keepers, nonTest, test, err
}

// keeperWalkDir is findBlankKeepers' answer for a directory: nil to descend,
// SkipDir for one no build of this module compiles, which is vendor,
// testdata and node_modules, a nested module, and a name the go tool ignores
// (goToolIgnores), decided before anything under it is opened.
func keeperWalkDir(root, path, name string) error {
	if path == root {
		return nil
	}
	if goToolIgnores(name) || name == "vendor" || name == "testdata" || name == "node_modules" {
		return filepath.SkipDir
	}
	if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
		return filepath.SkipDir
	}
	return nil
}

// blankKeepersInPath returns the keepers of the file at path, which it names
// by its slash path relative to root. It normalises CRLF first: no
// .gitattributes pins eol, so a Windows checkout has it.
func blankKeepersInPath(root, path string) ([]blankKeeper, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return nil, err
	}
	return blankKeepersInFile(filepath.ToSlash(rel), strings.ReplaceAll(string(raw), "\r\n", "\n"))
}

// keeperFile is one file's scan: its imports, how many selector uses of each
// it has, and the keepers found so far, whose pkgs hold their OWN uses until
// finish turns them into the file's uses outside every keeper.
type keeperFile struct {
	rel     string
	src     string
	fset    *token.FileSet
	imports map[string]bool
	uses    map[string]int
	found   []blankKeeper
}

// blankKeepersInFile returns the keepers in one file's source.
//
// A selector `x.Sel` names an import when x is one of the file's import
// names and no local named x is in scope there (localScopesIn). Nothing at
// the top level can shadow an import, since a package-level x would collide
// with it, so only a function's locals are tracked.
func blankKeepersInFile(rel, src string) ([]blankKeeper, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, rel, src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	kf := &keeperFile{rel: rel, src: src, fset: fset, imports: importNames(f), uses: map[string]int{}}
	for _, decl := range f.Decls {
		kf.scanDecl(decl)
	}
	kf.finish()
	return kf.found, nil
}

// importNames returns the names f's imports are known by, blank and dot
// imports aside.
func importNames(f *ast.File) map[string]bool {
	names := map[string]bool{}
	for _, imp := range f.Imports {
		if name := importName(imp); name != "_" && name != "." {
			names[name] = true
		}
	}
	return names
}

// scanDecl counts the import uses in decl and collects its keepers: the
// top-level form from a var declaration, and the statement form from the
// functions decl holds, a top-level `var _ = func() {…}` included.
func (kf *keeperFile) scanDecl(decl ast.Decl) {
	scope := localScopesIn(decl)
	ast.Inspect(decl, func(n ast.Node) bool {
		if name, ok := kf.importAt(n, scope); ok {
			kf.uses[name]++
		}
		return true
	})
	if gd, ok := decl.(*ast.GenDecl); ok && gd.Tok == token.VAR {
		kf.scanVarSpecs(gd, scope)
	}
	ast.Inspect(decl, func(n ast.Node) bool {
		kf.scanStatement(n, scope)
		return true
	})
}

// importAt returns the import n names, when n is a selector `x.Sel` whose x
// is an import name with no local of that name in scope at x.
func (kf *keeperFile) importAt(n ast.Node, scope localScopes) (string, bool) {
	sel, ok := n.(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	id, ok := sel.X.(*ast.Ident)
	if !ok || !kf.imports[id.Name] || scope.covers(id.Name, id.Pos()) {
		return "", false
	}
	return id.Name, true
}

// scanVarSpecs collects the keepers of a top-level var declaration. A
// spec's doc is its own, or the declaration's when it is not grouped.
func (kf *keeperFile) scanVarSpecs(gd *ast.GenDecl, scope localScopes) {
	for _, spec := range gd.Specs {
		vs := spec.(*ast.ValueSpec)
		doc := vs.Doc
		if doc == nil && !gd.Lparen.IsValid() {
			doc = gd.Doc
		}
		for _, e := range blankValues(vs) {
			kf.keep(e, "var _ = ", true, doc, scope)
		}
	}
}

// scanStatement collects the keeper n is, if it is `_ = E` or `var _ = E`
// in a function.
func (kf *keeperFile) scanStatement(n ast.Node, scope localScopes) {
	switch x := n.(type) {
	case *ast.AssignStmt:
		kf.scanAssign(x, scope)
	case *ast.DeclStmt:
		kf.scanDeclStmt(x, scope)
	}
}

// scanAssign collects the keepers of an assignment: each `_ = E` pair.
func (kf *keeperFile) scanAssign(as *ast.AssignStmt, scope localScopes) {
	if as.Tok != token.ASSIGN || len(as.Lhs) != len(as.Rhs) {
		return
	}
	for i, lhs := range as.Lhs {
		if id, ok := lhs.(*ast.Ident); ok && id.Name == "_" {
			kf.keep(as.Rhs[i], "_ = ", false, nil, scope)
		}
	}
}

// scanDeclStmt collects the keepers of a var declaration in a function.
func (kf *keeperFile) scanDeclStmt(ds *ast.DeclStmt, scope localScopes) {
	gd, ok := ds.Decl.(*ast.GenDecl)
	if !ok || gd.Tok != token.VAR {
		return
	}
	for _, spec := range gd.Specs {
		for _, e := range blankValues(spec.(*ast.ValueSpec)) {
			kf.keep(e, "var _ = ", false, nil, scope)
		}
	}
}

// keep records e as a keeper when it is one. The top-level form is one when
// E names an import or, naming none, names a declaration of its own package.
// The statement form is one only when E names imports and nothing else: a
// statement naming a local (`_ = cfg`) is how Go code marks it used, and
// `var _ = 1` names nothing at all.
func (kf *keeperFile) keep(e ast.Expr, form string, top bool, doc *ast.CommentGroup, scope localScopes) {
	if !onlyNames(e) {
		return
	}
	pkgs, others := kf.namesIn(e, scope)
	isKeeper := len(pkgs) > 0 && (top || !others) || len(pkgs) == 0 && top && others
	if !isKeeper {
		return
	}
	k := blankKeeper{file: kf.rel, line: kf.fset.Position(e.Pos()).Line, form: form, top: top, src: kf.text(e), pkgs: pkgs}
	if doc != nil {
		k.doc = strings.Join(strings.Fields(doc.Text()), " ")
	}
	kf.found = append(kf.found, k)
}

// text returns n's source with its whitespace collapsed.
func (kf *keeperFile) text(n ast.Node) string {
	return strings.Join(strings.Fields(kf.src[kf.fset.Position(n.Pos()).Offset:kf.fset.Position(n.End()).Offset]), " ")
}

// namesIn returns how many times e names each import (importAt), and whether
// it names anything else: a local or package-level name. A predeclared name
// (nil, int, error) is neither. A composite literal's key counts as a name
// even when it is a struct field's: in a map literal the same identifier is
// a variable, the two look alike without types, and skipping keys reported
// `_ = map[string]int{key: http.StatusOK}`, whose deletion leaves the local
// key unused (a Gemini consult on #996). The statement form misses
// `_ = url.URL{Scheme: "https"}` instead, which is the safe direction.
func (kf *keeperFile) namesIn(e ast.Expr, scope localScopes) (pkgs map[string]int, others bool) {
	pkgs = map[string]int{}
	var visit func(n ast.Node) bool
	visit = func(n ast.Node) bool {
		if name, ok := kf.importAt(n, scope); ok {
			pkgs[name]++
			return false
		}
		switch x := n.(type) {
		case *ast.SelectorExpr:
			// Sel names a field or method, never a declaration in scope, so
			// `(*bytes.Buffer).Len` and `http.DefaultClient.Do` name only
			// their import (CodeRabbit on #996).
			ast.Inspect(x.X, visit)
			return false
		case *ast.Ident:
			if types.Universe.Lookup(x.Name) == nil {
				others = true
			}
		}
		return true
	}
	ast.Inspect(e, visit)
	return pkgs, others
}

// finish turns each keeper's own uses into the file's uses outside every
// keeper. A keeper's use is no use: two keepers of one package (cmd/bridge's
// update.go had them) keep each other's import alive, and deleting both
// must delete it.
func (kf *keeperFile) finish() {
	inKeepers := map[string]int{}
	for i := range kf.found {
		for name, own := range kf.found[i].pkgs {
			inKeepers[name] += own
		}
	}
	for i := range kf.found {
		for name := range kf.found[i].pkgs {
			kf.found[i].pkgs[name] = kf.uses[name] - inKeepers[name]
		}
	}
}

// localScopes maps each name a declaration's functions declare to the
// spans in which a local of that name is in scope.
type localScopes map[string][]scopeSpan

// scopeSpan is a half-open span of source positions.
type scopeSpan struct{ from, to token.Pos }

// covers reports whether a local named name is in scope at pos.
func (s localScopes) covers(name string, pos token.Pos) bool {
	for _, span := range s[name] {
		if span.from <= pos && pos < span.to {
			return true
		}
	}
	return false
}

// localScopesIn returns the locals decl's functions declare, each scoped as
// Go scopes it. A variable, constant or type declared in a function is in
// scope from the end of its declaring statement (a type from its name) to
// the end of its innermost block, where an if, for, switch or select clause
// is a block of its own; a parameter, result or receiver is in scope in its
// function's body; a range variable in its loop's body. So in
// `path := path.Base(p)` the right-hand side still names the import, and an
// `_ = path.Join` above that line is still a keeper (CodeRabbit on #996,
// against a first draft that took any name declared anywhere in the
// function for a local everywhere in it).
func localScopesIn(decl ast.Decl) localScopes {
	s := localScopes{}
	var stack []ast.Node
	ast.Inspect(decl, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		s.declare(n, innermostBlockEnd(stack))
		stack = append(stack, n)
		return true
	})
	return s
}

// declare records the locals n declares; blockEnd is where the innermost
// block enclosing n ends.
func (s localScopes) declare(n ast.Node, blockEnd token.Pos) {
	switch x := n.(type) {
	case *ast.FuncDecl:
		if x.Body != nil {
			s.addFields(x.Body, x.Recv, x.Type.TypeParams, x.Type.Params, x.Type.Results)
		}
	case *ast.FuncLit:
		s.addFields(x.Body, x.Type.Params, x.Type.Results)
	case *ast.AssignStmt:
		if x.Tok == token.DEFINE {
			for _, lhs := range x.Lhs {
				s.add(lhs, x.End(), blockEnd)
			}
		}
	case *ast.RangeStmt:
		if x.Tok == token.DEFINE {
			s.add(x.Key, x.Body.Pos(), x.Body.End())
			s.add(x.Value, x.Body.Pos(), x.Body.End())
		}
	case *ast.DeclStmt:
		s.addDeclStmt(x, blockEnd)
	}
}

// addFields scopes every name in lists to body.
func (s localScopes) addFields(body *ast.BlockStmt, lists ...*ast.FieldList) {
	for _, list := range lists {
		if list == nil {
			continue
		}
		for _, field := range list.List {
			for _, name := range field.Names {
				s.add(name, body.Pos(), body.End())
			}
		}
	}
}

// addDeclStmt scopes the names a var, const or type declaration in a
// function declares.
func (s localScopes) addDeclStmt(ds *ast.DeclStmt, blockEnd token.Pos) {
	gd, ok := ds.Decl.(*ast.GenDecl)
	if !ok {
		return
	}
	for _, spec := range gd.Specs {
		switch sp := spec.(type) {
		case *ast.ValueSpec:
			for _, name := range sp.Names {
				s.add(name, sp.End(), blockEnd)
			}
		case *ast.TypeSpec:
			s.add(sp.Name, sp.Name.Pos(), blockEnd)
		}
	}
}

// add scopes e, when it is an identifier other than _, to [from, to).
func (s localScopes) add(e ast.Expr, from, to token.Pos) {
	if id, ok := e.(*ast.Ident); ok && id.Name != "_" {
		s[id.Name] = append(s[id.Name], scopeSpan{from, to})
	}
}

// innermostBlockEnd returns where the innermost block among stack, a node's
// ancestors, ends: a braced block, a function, or the implicit block of an
// if, for, switch or select clause.
func innermostBlockEnd(stack []ast.Node) token.Pos {
	for i := len(stack) - 1; i >= 0; i-- {
		switch stack[i].(type) {
		case *ast.BlockStmt, *ast.IfStmt, *ast.ForStmt, *ast.RangeStmt, *ast.SwitchStmt,
			*ast.TypeSwitchStmt, *ast.CaseClause, *ast.CommClause, *ast.FuncLit, *ast.FuncDecl:
			return stack[i].End()
		}
	}
	return token.NoPos
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
// identifiers, selectors, literals, operators on those (`&pkg.T{}`,
// `time.Second * 5`, Gemini on #996), composite literals, and conversions
// to a type that cannot be a function. A call whose callee could be a
// function is refused (`pkg.T(x)` could be either), so `var _ =
// registry.Register(x)` and a compile-time size assertion are never keepers,
// and so is anything else that does something: a receive waits, and an
// operator that can panic (panicFree) or a slice-to-array conversion
// (convertsToArray) can end the program. A dereference can panic on nil
// too, but `*pkg.P` is spelled like the method expression `(*pkg.T).M`,
// which is reported on purpose, so a star is read.
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
	case *ast.UnaryExpr:
		return x.Op != token.ARROW && onlyNames(x.X)
	case *ast.BinaryExpr:
		return panicFree(x.Op) && onlyNames(x.X) && onlyNames(x.Y)
	case *ast.CompositeLit:
		return (x.Type == nil || typeOnlyNames(x.Type)) && eltsOnlyName(x.Elts)
	case *ast.CallExpr:
		return len(x.Args) == 1 && isTypeOnly(x.Fun) && typeOnlyNames(x.Fun) && !convertsToArray(x.Fun) && onlyNames(x.Args[0])
	}
	return false
}

// panicFree reports whether a binary operator can never panic at run time.
// Division and remainder panic on a zero divisor, a shift on a negative
// count, and == or != on two interfaces holding one uncomparable type, so
// `_ = 1 / pkg.Divisor` does something a keeper does not (CodeRabbit on
// #996). The rest cannot.
func panicFree(op token.Token) bool {
	switch op {
	case token.ADD, token.SUB, token.MUL, token.AND, token.OR, token.XOR, token.AND_NOT,
		token.LAND, token.LOR, token.LSS, token.GTR, token.LEQ, token.GEQ:
		return true
	}
	return false
}

// convertsToArray reports whether a conversion's type is an array or a
// pointer to one. Converting a slice to either panics when the slice is too
// short.
func convertsToArray(t ast.Expr) bool {
	for {
		switch x := t.(type) {
		case *ast.ParenExpr:
			t = x.X
		case *ast.StarExpr:
			t = x.X
		case *ast.ArrayType:
			return x.Len != nil
		default:
			return false
		}
	}
}

// typeOnlyNames reports whether a type expression only names things. A type
// holds an expression only in an array length or a type argument, and the
// length is where a compile-time assertion computes with a call:
// `var _ = [unsafe.Sizeof(x) - 8]byte{}` is not a keeper, and neither is one
// whose length sits in a struct's field, a function's parameter or an
// interface's method (CodeRabbit on #996). A type argument makes a generic
// instantiation, `set.Of[int]{}`, which is one.
func typeOnlyNames(t ast.Expr) bool {
	switch x := t.(type) {
	case *ast.Ident, *ast.SelectorExpr:
		return true
	case *ast.StructType:
		return fieldTypesOnlyName(x.Fields)
	case *ast.InterfaceType:
		return fieldTypesOnlyName(x.Methods)
	case *ast.FuncType:
		return fieldTypesOnlyName(x.TypeParams) && fieldTypesOnlyName(x.Params) && fieldTypesOnlyName(x.Results)
	case *ast.Ellipsis:
		return x.Elt == nil || typeOnlyNames(x.Elt)
	case *ast.UnaryExpr:
		return x.Op == token.TILDE && typeOnlyNames(x.X)
	case *ast.BinaryExpr:
		return x.Op == token.OR && typeOnlyNames(x.X) && typeOnlyNames(x.Y)
	case *ast.ParenExpr:
		return typeOnlyNames(x.X)
	case *ast.StarExpr:
		return typeOnlyNames(x.X)
	case *ast.ArrayType:
		return arrayLenOnlyNames(x.Len) && typeOnlyNames(x.Elt)
	case *ast.MapType:
		return typeOnlyNames(x.Key) && typeOnlyNames(x.Value)
	case *ast.ChanType:
		return typeOnlyNames(x.Value)
	case *ast.IndexExpr:
		return typeOnlyNames(x.X) && typeOnlyNames(x.Index)
	case *ast.IndexListExpr:
		return typeOnlyNames(x.X) && allTypesOnlyName(x.Indices)
	}
	return false
}

// arrayLenOnlyNames reports whether an array type's length only names
// things: absent (a slice), `...`, or a constant expression with no call.
func arrayLenOnlyNames(n ast.Expr) bool {
	if n == nil {
		return true
	}
	if _, ok := n.(*ast.Ellipsis); ok {
		return true
	}
	return onlyNames(n)
}

// fieldTypesOnlyName reports whether the type of every field in fl only
// names things: a struct's fields, a function's parameters and results, an
// interface's methods and embedded types.
func fieldTypesOnlyName(fl *ast.FieldList) bool {
	if fl == nil {
		return true
	}
	for _, f := range fl.List {
		if !typeOnlyNames(f.Type) {
			return false
		}
	}
	return true
}

// allTypesOnlyName reports whether every type in ts only names things.
func allTypesOnlyName(ts []ast.Expr) bool {
	for _, t := range ts {
		if !typeOnlyNames(t) {
			return false
		}
	}
	return true
}

// eltsOnlyName reports whether every element of a composite literal, key
// and value alike, only names things.
func eltsOnlyName(elts []ast.Expr) bool {
	for _, elt := range elts {
		if kv, ok := elt.(*ast.KeyValueExpr); ok {
			if !onlyNames(kv.Key) {
				return false
			}
			elt = kv.Value
		}
		if !onlyNames(elt) {
			return false
		}
	}
	return true
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
	abs := filepath.Join(root, filepath.FromSlash(dir))
	entries, err := os.ReadDir(abs)
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || goToolIgnores(name) || strings.HasSuffix(name, "_test.go") {
			continue
		}
		if found, err := typedErrorInFile(filepath.Join(abs, name), dir+"/"+name); found != "" || err != nil {
			return found, err
		}
	}
	return "", nil
}

// typedErrorInFile is typedErrorIn for the one file at path, which its
// answer names rel. It normalises CRLF as blankKeepersInPath does.
func typedErrorInFile(path, rel string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, rel, strings.ReplaceAll(string(raw), "\r\n", "\n"), parser.SkipObjectResolution)
	if err != nil {
		return "", err
	}
	for _, decl := range f.Decls {
		if at, what := typedError(decl); at != nil {
			return fmt.Sprintf("%s:%d declares %s", rel, fset.Position(at.Pos()).Line, what), nil
		}
	}
	return "", nil
}

// typedError returns the node in decl that gives its package a typed error,
// and what it declares, or nil.
func typedError(decl ast.Decl) (ast.Node, string) {
	switch d := decl.(type) {
	case *ast.GenDecl:
		return sentinelIn(d)
	case *ast.FuncDecl:
		if isErrorMethod(d) {
			return d, "an Error() string method"
		}
	}
	return nil, ""
}

// sentinelIn returns the first name gd declares a sentinel error under, or
// nil.
func sentinelIn(gd *ast.GenDecl) (ast.Node, string) {
	for _, spec := range gd.Specs {
		vs, ok := spec.(*ast.ValueSpec)
		if !ok {
			continue
		}
		for i, n := range vs.Names {
			if sentinelName.MatchString(n.Name) || (i < len(vs.Values) && isSentinelValue(vs.Values[i])) {
				return n, n.Name
			}
		}
	}
	return nil, ""
}

// isErrorMethod reports whether fd is a method `Error() string`.
func isErrorMethod(fd *ast.FuncDecl) bool {
	if fd.Recv == nil || fd.Name.Name != "Error" || fd.Type.Params.NumFields() != 0 || fd.Type.Results.NumFields() != 1 {
		return false
	}
	id, ok := fd.Type.Results.List[0].Type.(*ast.Ident)
	return ok && id.Name == "string"
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
// state its entry can be in, and one case writes the whole tree with CRLF
// line endings.
func TestBlankKeeperScanOnFixtures(t *testing.T) {
	const tsnetKeeper = "internal/tsnet/tsnet_test.go: errors.Is: "
	for _, c := range []struct {
		name    string
		changes map[string]string // path -> new content, "" to delete
		crlf    bool              // write every file with CRLF line endings
		tsnet   string            // the verdict on the tsnet keeper
	}{
		{"condition unmet", nil, false, keeperAllowed},
		{"a CRLF checkout", nil, true, keeperAllowed},
		{"a sentinel meets it by its name", map[string]string{
			"internal/tsnet/err.go": "package tsnet\n\nvar ErrNotStarted = newError(\"tsnet: not started\")\n",
		}, false, keeperMet},
		{"a sentinel meets it by its initialiser", map[string]string{
			"internal/tsnet/err.go": "package tsnet\n\nimport \"errors\"\n\nvar notStarted = errors.New(\"tsnet: not started\")\n",
		}, false, keeperMet},
		{"a sentinel meets it by another package's", map[string]string{
			"internal/tsnet/err.go": "package tsnet\n\nimport \"net\"\n\nvar closed = net.ErrClosed\n",
		}, false, keeperMet},
		{"an error type meets it", map[string]string{
			"internal/tsnet/err.go": "package tsnet\n\ntype notStarted struct{}\n\nfunc (notStarted) Error() string { return \"not started\" }\n",
		}, false, keeperMet},
		{"the doc drops the condition", map[string]string{
			"internal/tsnet/tsnet_test.go": "package tsnet\n\nimport \"errors\"\n\n// This blank reference keeps errors imported.\nvar _ = errors.Is\n",
		}, false, keeperUnstated},
		{"the file uses the import now", map[string]string{
			"internal/tsnet/tsnet_test.go": "package tsnet\n\nimport \"errors\"\n\n// This blank reference keeps errors imported for the check typed errors\n// will allow. Don't remove until typed errors land.\nvar _ = errors.Is\n\nfunc typed(err error) bool { return errors.Is(err, nil) }\n",
		}, false, keeperServed},
		{"the keeper is gone", map[string]string{
			"internal/tsnet/tsnet_test.go": "",
		}, false, keeperStaleEntry},
	} {
		t.Run(c.name, func(t *testing.T) {
			// A checkout's path may hold glob metacharacters.
			root := filepath.Join(t.TempDir(), "clone[1]")
			writeKeeperTree(t, root, keeperFixtureTree, c.changes, c.crlf)
			rec := &docScanRecorder{t: t}
			verdicts := scanBlankKeepers(rec, root, allowedKeepers, false)
			checkKeeperVerdicts(t, rec, verdicts, append(append([]string(nil), keeperFixtureVerdicts...), tsnetKeeper+c.tsnet))
		})
	}
}

// keeperFixtureTree is the tree TestBlankKeeperScanOnFixtures scans, before
// each case's changes: path -> source.
var keeperFixtureTree = map[string]string{
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
	// A predeclared name (nil) is not a local, and a selector's field or
	// method name names no declaration at all, so each statement below
	// names only an import and is a keeper.
	"keep/statements.go": `package keep

import (
	"bytes"
	"net/http"
)

func c() {
	_ = (*bytes.Buffer)(nil)
	_ = (*bytes.Buffer).Len
	_ = http.DefaultClient.Do
}
`,
	"keep/local.go": `package keep

var logger = struct{}{}

var _ = logger
`,
	// A use of the package's name where a local of that name is in scope
	// does not count as a use of the import, however the local was
	// declared, and a statement naming only such a local is no keeper.
	"keep/shadow.go": `package keep

import "path"

func base() string {
	path := struct{ Base string }{}
	return path.Base
}

func param(path struct{ Base string }) string { return path.Base }

func ranged() {
	for _, path := range []struct{ Base string }{} {
		_ = path.Base
	}
}

func declared() string {
	var path struct{ Base string }
	return path.Base
}

var lit = func(path struct{ Base string }) string { return path.Base }

var _ = path.Join
`,
	// A local's scope begins after its declaring statement, so both uses
	// below name the import: the keeper above the declaration is found,
	// and the declaration's own right-hand side is a use.
	"keep/scope.go": `package keep

import "path"

func early() string {
	_ = path.Join
	path := path.Base("a/b")
	return path
}
`,
	// An operator or a type argument only names things too.
	"keep/operators.go": `package keep

import (
	"example.com/set"
	"sync"
	"time"
)

var _ = &sync.Mutex{}
var _ = time.Second * 5
var _ = set.Of[int]{}
var _ = struct{ M sync.Mutex }{}
var _ = (func(...time.Duration))(nil)
`,
	// Each import's name is its package's: a ".v3" suffix, a "go-" prefix
	// and a "/v2" element are not part of it.
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
	"example.com/events"
	"example.com/settings"
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

// A compile-time size assertion indexes and calls, or calls in a
// literal's array length.
var _ = [1]struct{}{}[unsafe.Sizeof(int64(0))-8]
var _ = [unsafe.Sizeof(int64(0)) - 8]byte{}

// So does a length nested in a field, a parameter or a method.
var _ = struct{ _ [unsafe.Sizeof(int64(0)) - 8]byte }{}
var _ = (func([unsafe.Sizeof(int64(0)) - 8]byte))(nil)
var _ = (interface{ M([unsafe.Sizeof(int64(0)) - 8]byte) })(nil)

// Each of these can panic at run time, which a keeper cannot.
func panics() {
	_ = 1 / settings.Divisor
	_ = 1 << settings.Shift
	_ = settings.A == settings.B
	_ = [4]byte(settings.Bytes)
}

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
	_ = <-events.Done                   // a receive waits
}
`,
	// Nothing the go tool ignores is read, nor another module, nor vendored
	// or node_modules code. The lock file is not Go, so opening it would
	// fail the scan.
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

// keeperFixtureVerdicts is what every case of TestBlankKeeperScanOnFixtures
// finds outside internal/tsnet, as "file: keeper: verdict".
var keeperFixtureVerdicts = []string{
	"keep/local.go: logger: " + keeperLocal,
	"keep/names.go: isatty.IsTerminal: " + keeperOnlyUse,
	"keep/names.go: tool.Run: " + keeperOnlyUse,
	"keep/names.go: yaml.Marshal: " + keeperOnlyUse,
	"keep/only_use_test.go: (*strings.Builder)(nil): " + keeperOnlyUse,
	"keep/operators.go: &sync.Mutex{}: " + keeperOnlyUse,
	"keep/operators.go: (func(...time.Duration))(nil): " + keeperOnlyUse,
	"keep/operators.go: struct{ M sync.Mutex }{}: " + keeperOnlyUse,
	"keep/operators.go: set.Of[int]{}: " + keeperOnlyUse,
	"keep/operators.go: time.Second * 5: " + keeperOnlyUse,
	"keep/only_use_test.go: http.ErrServerClosed: " + keeperOnlyUse,
	"keep/only_use_test.go: io.Copy: " + keeperOnlyUse,
	"keep/only_use_test.go: time.Second: " + keeperOnlyUse,
	"keep/redundant.go: fmt.Sprintf: " + keeperRedundant,
	"keep/scope.go: path.Join: " + keeperRedundant,
	"keep/shadow.go: path.Join: " + keeperOnlyUse,
	"keep/statements.go: (*bytes.Buffer)(nil): " + keeperOnlyUse,
	"keep/statements.go: (*bytes.Buffer).Len: " + keeperOnlyUse,
	"keep/statements.go: http.DefaultClient.Do: " + keeperOnlyUse,
	"keep/twice.go: strconv.IntSize: " + keeperOnlyUse,
	"keep/twice.go: strconv.IntSize: " + keeperOnlyUse,
}

// writeKeeperTree writes tree under root with changes applied (an empty
// change deletes the file), with CRLF line endings when crlf is set.
func writeKeeperTree(t *testing.T, root string, tree, changes map[string]string, crlf bool) {
	t.Helper()
	files := map[string]string{}
	for rel, src := range tree {
		files[rel] = src
	}
	for rel, src := range changes {
		files[rel] = src
	}
	for rel, src := range files {
		if src == "" {
			continue
		}
		if crlf {
			src = strings.ReplaceAll(src, "\n", "\r\n")
		}
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// checkKeeperVerdicts compares verdicts with want as a sorted list of
// "file: keeper: verdict", and requires one reported error for every verdict
// but an allowance.
func checkKeeperVerdicts(t *testing.T, rec *docScanRecorder, verdicts []keeperVerdict, want []string) {
	t.Helper()
	var got []string
	wantErrors := 0
	for _, v := range verdicts {
		file, _, _ := strings.Cut(v.pos, ":")
		got = append(got, file+": "+v.src+": "+v.why)
	}
	for _, w := range want {
		if !strings.HasSuffix(w, ": "+keeperAllowed) {
			wantErrors++
		}
	}
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("verdicts:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
	if len(rec.errors) != wantErrors {
		t.Errorf("reported %d errors, want %d:\n  %s", len(rec.errors), wantErrors, strings.Join(rec.errors, "\n  "))
	}
}
