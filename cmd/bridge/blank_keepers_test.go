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
// a function, where E has one of the four shapes keeperName reads. The name
// is almost always an import ("silence unused-import warning"), and the
// keeper is dead code either way. Imports are per FILE: when the file
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
// fourteen and left allowedKeepers' one. Each of the 24 took one of the
// shapes keeperName reads, and no other blank form ever held one (`var _ T`,
// `const _`, `type _`, `func _`), so the sweep reads nothing else. Such a
// reference can check at most that a name exists (`(*manifest.Store)(nil)`
// had a doc claiming it checked the store's methods), and every real use of
// the name makes that check; a name nothing uses needs none. The typed
// form, `var _ I = (*T)(nil)`, is how Go states a compile-time assertion,
// and is never read.
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
	// dotImport is whether the file dot-imports a package, whose names it
	// then uses bare, like its own.
	dotImport bool
	uses      map[string]int
	found     []blankKeeper
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
	kf := &keeperFile{rel: rel, src: src, fset: fset, imports: importNames(f), dotImport: hasDotImport(f), uses: map[string]int{}}
	for _, decl := range f.Decls {
		kf.scanDecl(decl)
	}
	kf.finish()
	return kf.found, nil
}

// importNames returns the names f's imports are known by, blank and dot
// imports aside, and cgo's pseudo-package "C" too: that import carries the
// preamble's #cgo directives, so "delete it, and the import" is never the
// right advice for it.
func importNames(f *ast.File) map[string]bool {
	names := map[string]bool{}
	for _, imp := range f.Imports {
		// An import path may be a raw string (`C`), so compare it unquoted
		// (CodeRabbit on #996).
		if path, err := strconv.Unquote(imp.Path.Value); err == nil && path == "C" {
			continue
		}
		if name := importName(imp); name != "_" && name != "." {
			names[name] = true
		}
	}
	return names
}

// hasDotImport reports whether f dot-imports a package.
func hasDotImport(f *ast.File) bool {
	for _, imp := range f.Imports {
		if imp.Name != nil && imp.Name.Name == "." {
			return true
		}
	}
	return false
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

// keep records e as a keeper when it has a keeper's shape (keeperName) and
// the name it keeps is an import or, in the top-level form only, a bare
// identifier of the file's own package. A statement naming a local
// (`_ = cfg`) is how Go code marks it used, a selector through a
// package-level variable (`var _ = cfg.Name`) can dereference it, and in a
// file with a dot import a bare name may be the import's rather than the
// package's own, which the syntax cannot tell apart.
func (kf *keeperFile) keep(e ast.Expr, form string, top bool, doc *ast.CommentGroup, scope localScopes) {
	name, ok := keeperName(e)
	if !ok {
		return
	}
	pkgs := map[string]int{}
	if pkg, isImport := kf.importAt(name, scope); isImport {
		pkgs[pkg] = 1
	} else if _, isIdent := name.(*ast.Ident); !top || !isIdent || kf.dotImport {
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

// keeperName returns the name e keeps, when e has one of the shapes a
// keeper takes: N, N{}, &N{} or (*N)(nil), where N is a name (isName). All
// 24 keepers in the 37 history trees took the first, second or last.
// Nothing else is read. A general "E only names things" test came first,
// and each review round found a construct it misread (#996): an operator
// that panics, a compile-time assertion in an array length, a call spelled
// like a conversion. A keeper spelled any other way goes unseen, which is
// the safe direction; CLAUDE.md's rule still says to delete it.
//
// (*N)(nil) is the one ambiguous shape. It is also a call through a
// pointer-to-function variable with a nil argument. Measured on 2026-09-24,
// no exported pointer-to-function variable is declared in this module, the
// standard library or any module it depends on.
func keeperName(e ast.Expr) (ast.Expr, bool) {
	switch x := e.(type) {
	case *ast.CompositeLit:
		return emptyLiteralOf(x)
	case *ast.UnaryExpr:
		if lit, ok := x.X.(*ast.CompositeLit); ok && x.Op == token.AND {
			return emptyLiteralOf(lit)
		}
		return nil, false
	case *ast.CallExpr:
		return nilConversionTo(x)
	}
	return e, isName(e)
}

// emptyLiteralOf returns the type of lit when lit is N{}: no elements, and
// a type that is a name.
func emptyLiteralOf(lit *ast.CompositeLit) (ast.Expr, bool) {
	return lit.Type, len(lit.Elts) == 0 && isName(lit.Type)
}

// nilConversionTo returns N when call is (*N)(nil).
func nilConversionTo(call *ast.CallExpr) (ast.Expr, bool) {
	paren, ok := call.Fun.(*ast.ParenExpr)
	if !ok || len(call.Args) != 1 {
		return nil, false
	}
	star, ok := paren.X.(*ast.StarExpr)
	if !ok {
		return nil, false
	}
	arg, ok := call.Args[0].(*ast.Ident)
	return star.X, ok && arg.Name == "nil" && isName(star.X)
}

// isName reports whether e is a name a keeper can keep: an identifier that
// is not predeclared, or a selector on one (pkg.X). Evaluating either can
// do nothing but name it; keep decides which kinds of name count.
func isName(e ast.Expr) bool {
	switch x := e.(type) {
	case *ast.Ident:
		return types.Universe.Lookup(x.Name) == nil
	case *ast.SelectorExpr:
		_, ok := x.X.(*ast.Ident)
		return ok
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
		if at, what := sentinelIn(d); at != nil {
			return at, what
		}
		return errorInterfaceIn(d)
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

// errorInterfaceIn returns the first interface type gd declares that is an
// error, one embedding `error` or declaring `Error() string` (as
// net.Error does), or nil. errors.As matches one, so it is a typed error
// as much as a sentinel is.
func errorInterfaceIn(gd *ast.GenDecl) (ast.Node, string) {
	for _, spec := range gd.Specs {
		ts, ok := spec.(*ast.TypeSpec)
		if !ok {
			continue
		}
		if it, ok := ts.Type.(*ast.InterfaceType); ok && isErrorInterface(it) {
			return ts.Name, "the error interface " + ts.Name.Name
		}
	}
	return nil, ""
}

// isErrorInterface reports whether it embeds error or declares a method
// `Error() string`.
func isErrorInterface(it *ast.InterfaceType) bool {
	for _, m := range it.Methods.List {
		if id, ok := m.Type.(*ast.Ident); ok && len(m.Names) == 0 && id.Name == "error" {
			return true
		}
		if ft, ok := m.Type.(*ast.FuncType); ok && len(m.Names) == 1 && m.Names[0].Name == "Error" && returnsOnlyString(ft) {
			return true
		}
	}
	return false
}

// returnsOnlyString reports whether ft takes nothing and returns a string.
func returnsOnlyString(ft *ast.FuncType) bool {
	if ft.Params.NumFields() != 0 || ft.Results.NumFields() != 1 {
		return false
	}
	id, ok := ft.Results.List[0].Type.(*ast.Ident)
	return ok && id.Name == "string"
}

// isErrorMethod reports whether fd is a method `Error() string`.
func isErrorMethod(fd *ast.FuncDecl) bool {
	return fd.Recv != nil && fd.Name.Name == "Error" && returnsOnlyString(fd.Type)
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
		{"an error interface meets it", map[string]string{
			"internal/tsnet/err.go": "package tsnet\n\ntype Error interface {\n\terror\n\tTimeout() bool\n}\n",
		}, false, keeperMet},
		{"an interface with an Error method meets it", map[string]string{
			"internal/tsnet/err.go": "package tsnet\n\ntype Failure interface {\n\tError() string\n}\n",
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
	// The statement form reads the same shapes as the top-level one.
	"keep/statements.go": `package keep

import "bytes"

func c() {
	_ = (*bytes.Buffer)(nil)
	_ = &bytes.Reader{}
	_ = bytes.Buffer{}
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
	// An empty literal of a name, or its address, is a keeper.
	"keep/literals.go": `package keep

import "sync"

var _ = &sync.Mutex{}
var _ = sync.WaitGroup{}
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
	"bytes"
	"errors"
	"example.com/events"
	"example.com/set"
	"example.com/settings"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"
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

// A literal, one of a predeclared type, or a predeclared name keeps
// nothing, and a selector through a package-level variable can
// dereference it.
var _ = 1
var _ = [2]int{}
var _ = true
var cfgVar = struct{ Field int }{}
var _ = cfgVar.Field

// None of these has one of keeperName's four shapes, so none is read,
// whatever it keeps: an operator, a type argument, a literal of an unnamed
// type, a conversion to one, a method expression, a selector chain, a
// star call with an argument, a name in an array length.
var _ = time.Second * 5
var _ = set.Of[int]{}
var _ = struct{ M sync.Mutex }{}
var _ = (func(...time.Duration))(nil)
var _ = (*bytes.Buffer).Len
var _ = http.DefaultClient.Do
var _ = (*settings.Hook)(0)
var _ = struct{ _ [settings.MinSize - 8]byte }{}

func use(cfg struct{ Name string }) {
	_ = cfg                    // marks a local used
	_ = cfg.Name               // a local's field
	_ = []any{fmt.Sprint, cfg} // marks a local used, beside a package
	_ = fmt.Sprint()           // a call
	os := struct{ Args []string }{}
	_ = os.Args // os is the local here, not the import
	key := "k"
	_ = map[string]any{key: fmt.Sprint} // a literal with elements
	_ = url.URL{Scheme: "https"}        // so is this, whatever its keys
	_ = <-events.Done                   // a receive waits
}
`,
	// A bare name in a file with a dot import may be the import's, so the
	// local form is not read there; and cgo's pseudo-package carries the
	// preamble, so its import is never one to delete.
	"keep/dot.go":     "package keep\n\nimport . \"sync\"\n\nvar _ = Mutex{}\n",
	"keep/cgo.go":     "package keep\n\n// #cgo LDFLAGS: -lm\nimport \"C\"\n\nvar _ = (*C.char)(nil)\n",
	"keep/cgo_raw.go": "package keep\n\n// #cgo LDFLAGS: -lm\nimport `C`\n\nvar _ = (*C.char)(nil)\n",
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
	"keep/literals.go: &sync.Mutex{}: " + keeperOnlyUse,
	"keep/literals.go: sync.WaitGroup{}: " + keeperOnlyUse,
	"keep/names.go: isatty.IsTerminal: " + keeperOnlyUse,
	"keep/names.go: tool.Run: " + keeperOnlyUse,
	"keep/names.go: yaml.Marshal: " + keeperOnlyUse,
	"keep/only_use_test.go: (*strings.Builder)(nil): " + keeperOnlyUse,
	"keep/only_use_test.go: http.ErrServerClosed: " + keeperOnlyUse,
	"keep/only_use_test.go: io.Copy: " + keeperOnlyUse,
	"keep/only_use_test.go: time.Second: " + keeperOnlyUse,
	"keep/redundant.go: fmt.Sprintf: " + keeperRedundant,
	"keep/scope.go: path.Join: " + keeperRedundant,
	"keep/shadow.go: path.Join: " + keeperOnlyUse,
	"keep/statements.go: &bytes.Reader{}: " + keeperOnlyUse,
	"keep/statements.go: bytes.Buffer{}: " + keeperOnlyUse,
	"keep/statements.go: (*bytes.Buffer)(nil): " + keeperOnlyUse,
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
