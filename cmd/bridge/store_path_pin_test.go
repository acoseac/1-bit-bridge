package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The store's open records a migration in the install marker in the
// database's own directory (internal/manifest's disarmBeforeMigrating, which
// takes filepath.Dir of the path it opens), so the boundary it rests on is
// that every store the bridge opens is its data dir's database, at the path
// manifest.DefaultDBPath gives it: OpenStore is never given the data dir,
// and cannot check it (backlog B613's round 3). This sweep pins that
// boundary in the code that opens a store.

const (
	manifestImport = "github.com/acoseac/1-bit-bridge/internal/manifest"
	backupImport   = "github.com/acoseac/1-bit-bridge/internal/backup"
)

// storeOpenExcused are the calls the sweep cannot follow to their path,
// keyed by file and callee, each with where the path comes from instead.
var storeOpenExcused = map[string]string{
	"internal/backup/backup.go:ResetBackupEpoch": "the restore's target for bridge.db, backup.Targets.ManifestDB, " +
		"which every Targets the bridge builds sets to manifest.DefaultDBPath (checked below)",
}

// storeFile is one production file of the module, parsed.
type storeFile struct {
	rel  string // slash path from the module root
	dir  string // its directory, slash path
	f    *ast.File
	imps map[string]string // local name → import path
}

func TestEveryStoreTheBridgeOpensIsItsDataDirsDatabase(t *testing.T) {
	root := repoRootForCitations(t)
	files := parseProductionFiles(t, root)
	opens, problems := 0, []string(nil)
	excusedSeen := map[string]bool{}
	for _, sf := range files {
		ast.Inspect(sf.f, func(n ast.Node) bool {
			fn, ok := n.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				return true
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || !callsIn(sf, call, manifestImport, "OpenStore") || len(call.Args) != 1 {
					return true
				}
				opens++
				if why := storePathProblem(files, sf, fn, call.Args[0], 0, excusedSeen); why != "" {
					problems = append(problems, sf.rel+": "+fn.Name.Name+" opens a store at "+why)
				}
				return true
			})
			return false
		})
	}
	if opens < 10 {
		t.Fatalf("found %d opens of the store; the sweep looks at nothing it was written for", opens)
	}
	for _, p := range problems {
		t.Error(p + ": every store the bridge opens is its data dir's database, at manifest.DefaultDBPath(dataDir), " +
			"since the open records its migration in the install marker beside it")
	}
	for key := range storeOpenExcused {
		if !excusedSeen[key] {
			t.Errorf("storeOpenExcused names %s, which the sweep never met: drop it", key)
		}
	}
	// The excused restore: every Targets the bridge builds names the
	// database by manifest.DefaultDBPath.
	targets := 0
	for _, sf := range files {
		ast.Inspect(sf.f, func(n ast.Node) bool {
			fn, ok := n.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				return true
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				lit, ok := n.(*ast.CompositeLit)
				if !ok || !typeIn(sf, lit.Type, backupImport, "Targets") {
					return true
				}
				for _, el := range lit.Elts {
					kv, ok := el.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "ManifestDB" {
						targets++
						if why := storePathProblem(files, sf, fn, kv.Value, 0, excusedSeen); why != "" {
							t.Errorf("%s: %s restores the database at %s", sf.rel, fn.Name.Name, why)
						}
					}
				}
				return true
			})
			return false
		})
	}
	if targets == 0 {
		t.Error("found no backup.Targets naming the database; the excused restore is not checked")
	}
}

// storePathProblem says why e, an argument in fn of sf, is not a data dir's
// database as manifest.DefaultDBPath names it, "" when it is: the call
// itself, a local assigned only from one, or a parameter of fn that every
// call of fn in the module passes one.
func storePathProblem(files []storeFile, sf storeFile, fn *ast.FuncDecl, e ast.Expr, depth int, excused map[string]bool) string {
	if depth > 4 {
		return "a path passed on through too many functions to follow"
	}
	switch x := e.(type) {
	case *ast.CallExpr:
		if callsIn(sf, x, manifestImport, "DefaultDBPath") {
			return ""
		}
		return "a path " + exprString(e) + " builds"
	case *ast.Ident:
		if i, ok := paramIndex(fn, x.Name); ok {
			return callersProblem(files, sf, fn, i, depth, excused)
		}
		assigned := 0
		var why string
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			as, ok := n.(*ast.AssignStmt)
			if !ok || len(as.Lhs) != len(as.Rhs) {
				return true
			}
			for k, l := range as.Lhs {
				if id, ok := l.(*ast.Ident); ok && id.Name == x.Name {
					assigned++
					if p := storePathProblem(files, sf, fn, as.Rhs[k], depth+1, excused); p != "" && why == "" {
						why = p
					}
				}
			}
			return true
		})
		if assigned == 0 {
			return x.Name + ", which the sweep cannot follow to its path"
		}
		return why
	}
	return exprString(e) + ", which the sweep cannot follow to its path"
}

// callersProblem checks argument i of every call of fn in the module.
func callersProblem(files []storeFile, sf storeFile, fn *ast.FuncDecl, i, depth int, excused map[string]bool) string {
	if fn.Recv != nil {
		return "a parameter of the method " + fn.Name.Name + ", whose callers the sweep does not follow"
	}
	importPath := "github.com/acoseac/1-bit-bridge/" + sf.dir
	calls := 0
	var why string
	for _, cf := range files {
		ast.Inspect(cf.f, func(n ast.Node) bool {
			cfn, ok := n.(*ast.FuncDecl)
			if !ok || cfn.Body == nil {
				return true
			}
			ast.Inspect(cfn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || !callsIn(cf, call, importPath, fn.Name.Name) || i >= len(call.Args) {
					return true
				}
				calls++
				if key := cf.rel + ":" + fn.Name.Name; storeOpenExcused[key] != "" {
					excused[key] = true
					return true
				}
				if p := storePathProblem(files, cf, cfn, call.Args[i], depth+1, excused); p != "" && why == "" {
					why = p + " (through " + fn.Name.Name + ", called in " + cf.rel + ")"
				}
				return true
			})
			return false
		})
	}
	if calls == 0 {
		return "a parameter of " + fn.Name.Name + ", which nothing in the module calls"
	}
	return why
}

// callsIn reports whether call calls name in the package importPath: a
// selector on that import's local name, or the bare name inside the package.
func callsIn(sf storeFile, call *ast.CallExpr, importPath, name string) bool {
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		return fun.Name == name && "github.com/acoseac/1-bit-bridge/"+sf.dir == importPath
	case *ast.SelectorExpr:
		pkg, ok := fun.X.(*ast.Ident)
		return ok && fun.Sel.Name == name && sf.imps[pkg.Name] == importPath
	}
	return false
}

// typeIn reports whether e names the type name of the package importPath.
func typeIn(sf storeFile, e ast.Expr, importPath, name string) bool {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name == name && "github.com/acoseac/1-bit-bridge/"+sf.dir == importPath
	case *ast.SelectorExpr:
		pkg, ok := x.X.(*ast.Ident)
		return ok && x.Sel.Name == name && sf.imps[pkg.Name] == importPath
	}
	return false
}

func paramIndex(fn *ast.FuncDecl, name string) (int, bool) {
	i := 0
	for _, field := range fn.Type.Params.List {
		if len(field.Names) == 0 {
			i++
			continue
		}
		for _, n := range field.Names {
			if n.Name == name {
				return i, true
			}
			i++
		}
	}
	return 0, false
}

func exprString(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.CallExpr:
		return exprString(x.Fun) + "(…)"
	case *ast.SelectorExpr:
		return exprString(x.X) + "." + x.Sel.Name
	case *ast.Ident:
		return x.Name
	}
	return "an expression"
}

// parseProductionFiles parses every Go file the module builds, its tests
// excepted.
func parseProductionFiles(t *testing.T, root string) []storeFile {
	t.Helper()
	var files []storeFile
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return moduleDirRule(root, path, d.Name())
		}
		if name := d.Name(); !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || goToolIgnores(name) {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		f, err := parser.ParseFile(fset, rel, raw, 0)
		if err != nil {
			return err
		}
		imps := map[string]string{}
		for _, imp := range f.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				return err
			}
			local := p[strings.LastIndex(p, "/")+1:]
			if imp.Name != nil {
				local = imp.Name.Name
			}
			imps[local] = p
		}
		dir := filepath.ToSlash(filepath.Dir(rel))
		files = append(files, storeFile{rel: rel, dir: dir, f: f, imps: imps})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) < 300 {
		t.Fatalf("parsed %d production files; the sweep reads nothing it was written for", len(files))
	}
	return files
}
