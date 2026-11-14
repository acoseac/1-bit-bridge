package main

import (
	"errors"
	"fmt"
	"go/ast"
	"go/token"
	"path"
	"slices"
	"sort"
	"strings"
	"testing"
)

// testOnlySeam is a seam the pin named by hand before it scanned for every
// seam (backlog B872 part 2): its name and the file, below the module's root,
// that declares it.
type testOnlySeam struct{ name, file string }

// testOnlySeams are the seams backlog B872 part 2 adds in non-test files,
// which the scan must find declared where they are: a seam renamed or moved,
// or a scan that reads less of the module than it should, fails the pin.
var testOnlySeams = []testOnlySeam{
	{"SetRollbackObserverForTest", "internal/updater/rollback_observer.go"},
	{"SetRestorePreflightReadForTest", "internal/backup/seams.go"},
	{"SetProbeLimitForTest", "internal/doctor/doctor.go"},
	{"SetLsofCommandForTest", "internal/doctor/doctor_notwindows.go"},
	{"LsofForTest", "internal/doctor/doctor_notwindows.go"},
}

// seamExemption is a reference to a seam that a non-test file outside a
// test-support package makes by design: its file, its line and the seam, why
// only a test reaches it, and the proof of that the pin checks. within names
// the seam whose own declaration holds the reference, which runs only where
// that seam is called, and the pin holds that seam to its own rule. guard
// names the field of the nil check the reference sits under, which no
// non-test file sets.
type seamExemption struct {
	file   string
	line   int
	name   string
	why    string
	within string
	guard  string
}

// seamExemptions are the references to a seam a non-test file outside a
// test-support package may make, each by its file and line. Add one only
// with its reason and a proof the pin can check.
var seamExemptions = []seamExemption{
	{
		file: "cmd/bridge/main.go", line: 3901, name: "SetUpscaleTargetBeforeCommitForTest", guard: "upscaleTargetBeforeCommit",
		why: "serve hands its store the hold a boot test gives it (backlog B662), under a nil check of a serveOpts field only a test sets",
	},
	{
		file: "internal/advertise/reach.go", line: 116, name: "InterfaceForTest", within: "SetInterfacesForTest",
		why: "the type of SetInterfacesForTest's parameter; SetInterfacesForTest is a seam itself",
	},
	{
		file: "internal/fsutil/owner_unix.go", line: 414, name: "SimulateRootForTest", within: "SimulateRootRefusingForTest",
		why: "SimulateRootRefusingForTest, a seam itself, builds on SimulateRootForTest",
	},
}

// rollbackStages are the four stages of the rollback's put-back, each of
// which internal/updater/rollback_replace.go tells the observer of once.
var rollbackStages = []string{"RollbackStageBackup", "RollbackStageInstall", "RollbackStagePutBack", "RollbackStageVacate"}

// TestNoProductionCodeCallsATestOnlySeam: no non-test file refers to a seam,
// an exported function, method, var or type a non-test file declares for
// tests alone, whose name ends in "ForTest", but where only a test can reach
// the reference: in a test-support package, which no non-test file outside
// one imports, or at a line seamExemptions names, whose proof holds. The scan
// (scanSeams) reads every non-test Go file of the module whatever its build
// constraints and takes every identifier of a seam's name for a reference but
// a declaration's own name, a call, a function or method value, a type and a
// selector's name alike, so a name that resolves to something else is a
// false positive for the allowlist and never a miss. The pin must find each
// seam testOnlySeams names declared where it is, and the rollback's put-back
// telling the observer of each of its four stages once, so it reads the code
// the observer is wired into. A stale entry of the allowlist fails it too.
func TestNoProductionCodeCallsATestOnlySeam(t *testing.T) {
	scan, err := scanSeams(repoRootForCitations(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range testOnlySeams {
		if !scan.declaredIn(s.name, s.file) {
			t.Errorf("the scan found no declaration of %s in %s: a seam the pin named has moved, or the scan reads less than the module", s.name, s.file)
		}
	}
	if got := rollbackObservations(scan); !slices.Equal(got, rollbackStages) {
		t.Errorf("internal/updater/rollback_replace.go tells the observer of the stages %v, want each of %v once: the pin reads nothing the observer is wired into", got, rollbackStages)
	}
	for _, imp := range scan.testSupportImports() {
		t.Errorf("a non-test file imports a test-support package, whose references to seams the pin takes for a test's: %s", imp)
	}
	used := make([]bool, len(seamExemptions))
	var refs []string
	inTestSupport := 0
	for _, r := range scan.refs {
		if r.testSupport {
			inTestSupport++
			continue
		}
		i := slices.IndexFunc(seamExemptions, func(e seamExemption) bool {
			return e.file == r.file && e.line == r.line && e.name == r.name
		})
		if i < 0 {
			refs = append(refs, r.String())
			continue
		}
		used[i] = true
		if err := seamExemptions[i].proved(scan, r); err != nil {
			t.Errorf("the allowlist exempts %s (%s), and that no longer holds: %v", r, seamExemptions[i].why, err)
		}
	}
	if len(refs) != 0 {
		t.Errorf("production code refers to a test-only seam:\n%s", strings.Join(refs, "\n"))
	}
	for i, e := range seamExemptions {
		if !used[i] {
			t.Errorf("the allowlist exempts %s at %s:%d, where no reference to it is%s", e.name, e.file, e.line, scan.movedHint(e))
		}
	}
	t.Logf("%d seams declared over %d non-test files; %d references, %d in test-support packages, %d allowlisted",
		len(scan.seams), len(scan.files), len(scan.refs), inTestSupport, len(seamExemptions))
}

// declaredIn reports whether the scan found a declaration of the seam name
// in the file at rel.
func (s *seamScan) declaredIn(name, rel string) bool {
	return slices.ContainsFunc(s.seams[name], func(d seamDecl) bool { return d.file == rel })
}

// rollbackObservations is the stage each call of observeRollbackRename in
// internal/updater/rollback_replace.go names, sorted, "?" for an argument
// that is no constant's name.
func rollbackObservations(s *seamScan) []string {
	var stages []string
	f := s.files["internal/updater/rollback_replace.go"]
	if f == nil {
		return nil
	}
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if fn, ok := call.Fun.(*ast.Ident); ok && fn.Name == "observeRollbackRename" && len(call.Args) > 0 {
			stage := "?"
			if id, ok := call.Args[0].(*ast.Ident); ok {
				stage = id.Name
			}
			stages = append(stages, stage)
		}
		return true
	})
	sort.Strings(stages)
	return stages
}

// testSupportImports is each import of a test-support package by a non-test
// file outside one, as "file imports path".
func (s *seamScan) testSupportImports() []string {
	var out []string
	for _, rel := range s.sortedFiles() {
		f := s.files[rel]
		if isTestSupportPackage(f.Name.Name) {
			continue
		}
		for _, imp := range f.Imports {
			p := strings.Trim(imp.Path.Value, "\"`")
			dir, ok := strings.CutPrefix(p, s.module+"/")
			if p == s.module {
				dir, ok = ".", true
			}
			if ok && s.testSupportDirs[dir] {
				out = append(out, rel+" imports "+p)
			}
		}
	}
	return out
}

// proved checks the reason e gives for its reference r against what the scan
// read: the declaration that holds r, or the nil check r sits under and every
// non-test file's writes of its field.
func (e seamExemption) proved(s *seamScan, r seamRef) error {
	switch {
	case e.within != "":
		fn := s.enclosingFunc(r)
		if fn == nil || fn.Name.Name != e.within || !s.declaredIn(e.within, r.file) {
			return fmt.Errorf("it is not inside the declaration of %s, a seam %s declares", e.within, r.file)
		}
		return nil
	case e.guard != "":
		if r.ident == nil || !s.underNilCheckOf(r, e.guard) {
			return fmt.Errorf("it is not inside an if that checks %s against nil", e.guard)
		}
		if w := s.fieldWrites(e.guard, path.Dir(r.file)); len(w) != 0 {
			return fmt.Errorf("a non-test file sets %s: %s", e.guard, strings.Join(w, ", "))
		}
		return nil
	}
	return errors.New("the entry names no proof")
}

// enclosingFunc is the top-level function or method declaration, signature
// and body, that holds r, or nil.
func (s *seamScan) enclosingFunc(r seamRef) *ast.FuncDecl {
	for _, decl := range s.files[r.file].Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Pos() <= r.pos && r.pos < fn.End() {
			return fn
		}
	}
	return nil
}

// underNilCheckOf reports whether r's identifier lies in the body of an if
// whose condition is x.field != nil.
func (s *seamScan) underNilCheckOf(r seamRef, field string) bool {
	var stack []ast.Node
	found := false
	ast.Inspect(s.files[r.file], func(n ast.Node) bool {
		if found {
			return false
		}
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		if n == ast.Node(r.ident) {
			found = true
			return false
		}
		stack = append(stack, n)
		return true
	})
	if !found {
		return false
	}
	for _, n := range stack {
		ifs, ok := n.(*ast.IfStmt)
		if !ok || r.pos < ifs.Body.Pos() || r.pos >= ifs.Body.End() {
			continue
		}
		if c, ok := ifs.Cond.(*ast.BinaryExpr); ok && c.Op == token.NEQ &&
			(isFieldSelector(c.X, field) && isNilIdent(c.Y) || isNilIdent(c.X) && isFieldSelector(c.Y, field)) {
			return true
		}
	}
	return false
}

// fieldWrites is every place in a non-test file that can set a field of this
// name, by its name alone: a selector of it assigned to, incremented or
// decremented, ranged into or whose address is taken, a key of it in a
// composite literal, and a composite literal without keys of a struct type
// that declares it. An unexported field is set only in the package that
// declares it, the directory dir (the reference's), so only that package's
// files are read for one; an exported field's, every file's.
func (s *seamScan) fieldWrites(field, dir string) []string {
	files := map[string]*ast.File{}
	for rel, f := range s.files {
		if ast.IsExported(field) || path.Dir(rel) == dir {
			files[rel] = f
		}
	}
	holders := map[string]bool{}
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			if ts, ok := n.(*ast.TypeSpec); ok {
				if st, ok := ts.Type.(*ast.StructType); ok {
					for _, fl := range st.Fields.List {
						for _, nm := range fl.Names {
							if nm.Name == field {
								holders[ts.Name.Name] = true
							}
						}
					}
				}
			}
			return true
		})
	}
	var out []string
	for _, rel := range s.sortedFiles() {
		f, ok := files[rel]
		if !ok {
			continue
		}
		at := func(n ast.Node) {
			p := s.fset.Position(n.Pos())
			out = append(out, fmt.Sprintf("%s:%d:%d", rel, p.Line, p.Column))
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.AssignStmt:
				for _, l := range n.Lhs {
					if isFieldSelector(l, field) {
						at(l)
					}
				}
			case *ast.IncDecStmt:
				if isFieldSelector(n.X, field) {
					at(n)
				}
			case *ast.RangeStmt:
				if n.Tok == token.ASSIGN && (isFieldSelector(n.Key, field) || isFieldSelector(n.Value, field)) {
					at(n)
				}
			case *ast.UnaryExpr:
				if n.Op == token.AND && isFieldSelector(n.X, field) {
					at(n)
				}
			case *ast.CompositeLit:
				for _, el := range n.Elts {
					if kv, ok := el.(*ast.KeyValueExpr); ok {
						if k, ok := kv.Key.(*ast.Ident); ok && k.Name == field {
							at(kv)
						}
					}
				}
				if holders[typeName(n.Type)] && unkeyed(n) {
					at(n)
				}
				// An element whose type the literal elides, as in
				// []T{{a, b}}, a map's key or value included.
				for _, el := range elidedOfHolder(n, holders) {
					if unkeyed(el) {
						at(el)
					}
				}
			}
			return true
		})
	}
	return out
}

// unkeyed reports whether a composite literal sets its fields by position.
func unkeyed(lit *ast.CompositeLit) bool {
	if len(lit.Elts) == 0 {
		return false
	}
	_, keyed := lit.Elts[0].(*ast.KeyValueExpr)
	return !keyed
}

// elidedOfHolder is each element of lit, a slice, array or map literal of a
// holder type (or a pointer to one), written without its type.
func elidedOfHolder(lit *ast.CompositeLit, holders map[string]bool) []*ast.CompositeLit {
	var keyHolds, valueHolds bool
	switch t := lit.Type.(type) {
	case *ast.ArrayType:
		valueHolds = holders[typeName(starless(t.Elt))]
	case *ast.MapType:
		keyHolds, valueHolds = holders[typeName(starless(t.Key))], holders[typeName(starless(t.Value))]
	}
	var out []*ast.CompositeLit
	take := func(e ast.Expr) {
		if u, ok := e.(*ast.UnaryExpr); ok && u.Op == token.AND {
			e = u.X
		}
		if c, ok := e.(*ast.CompositeLit); ok && c.Type == nil {
			out = append(out, c)
		}
	}
	for _, el := range lit.Elts {
		kv, isKV := el.(*ast.KeyValueExpr)
		switch {
		case isKV:
			if keyHolds {
				take(kv.Key)
			}
			if valueHolds {
				take(kv.Value)
			}
		case valueHolds:
			take(el)
		}
	}
	return out
}

// starless is e without a pointer's star.
func starless(e ast.Expr) ast.Expr {
	if s, ok := e.(*ast.StarExpr); ok {
		return s.X
	}
	return e
}

// movedHint says where the scan finds the seam an unused entry names in the
// entry's file, where it finds it at one place the allowlist does not name.
func (s *seamScan) movedHint(e seamExemption) string {
	var lines []int
	for _, r := range s.refs {
		if r.file == e.file && r.name == e.name && !r.testSupport &&
			!slices.ContainsFunc(seamExemptions, func(o seamExemption) bool { return o.file == r.file && o.line == r.line && o.name == r.name }) {
			lines = append(lines, r.line)
		}
	}
	if len(lines) != 1 {
		return ""
	}
	return fmt.Sprintf(" (it is at line %d now: if that is the reference the entry is for, move the entry there)", lines[0])
}

// isFieldSelector reports whether e is x.field.
func isFieldSelector(e ast.Expr, field string) bool {
	sel, ok := e.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == field
}

// typeName is the name a composite literal's type is written with, its
// package left off, or "" for a literal whose type is not named.
func typeName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.SelectorExpr:
		return t.Sel.Name
	case *ast.IndexExpr:
		return typeName(t.X)
	case *ast.IndexListExpr:
		return typeName(t.X)
	}
	return ""
}
