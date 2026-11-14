package main

import (
	"bufio"
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// testOnlySeam is a seam a non-test file declares for tests alone: its
// function's name and the file, below the module's root, that declares it.
type testOnlySeam struct{ name, file string }

// testOnlySeams are the seams backlog B872 part 2 adds in non-test files.
var testOnlySeams = []testOnlySeam{
	{"SetRollbackObserverForTest", "internal/updater/rollback_observer.go"},
	{"SetRestorePreflightReadForTest", "internal/backup/seams.go"},
	{"SetProbeLimitForTest", "internal/doctor/doctor.go"},
	{"SetLsofCommandForTest", "internal/doctor/doctor_notwindows.go"},
	{"LsofForTest", "internal/doctor/doctor_notwindows.go"},
}

// TestNoProductionCodeCallsATestOnlySeam: the seams backlog B872 part 2 adds
// in non-test files are a test's alone (testOnlySeams): the observer the
// rollback's put-back tells of each rename that fails, the stand-in for the
// read of a snapshot's manifest bridge restore makes before its prompt, the
// doctor's lsof probe limit and its lsof spawn. Each is nil, or production's
// own, until a test sets it. The pin type-checks every package of the module
// the go tool builds, but tests, as each of linux, darwin and windows builds
// it (seamReferences), resolving identifiers, and lists every reference to a
// seam outside its declaration's own name: a call or a function value, in
// the file that declares it, another file of its package or another package.
// A field, a local or a method that shares a seam's name resolves to itself,
// and is none. It must see each seam declared where it is, so a renamed seam
// fails it rather than pass it, and the rollback's put-back telling the
// observer of each of its four renames, so it reads the code the observer is
// wired into.
func TestNoProductionCodeCallsATestOnlySeam(t *testing.T) {
	refs, observed, declared := seamReferences(t, repoRootForCitations(t))
	for _, s := range testOnlySeams {
		if !declared[s.name] {
			t.Errorf("%s does not declare %s: the pin would look for a seam that is not there", s.file, s.name)
		}
	}
	if observed != 4 {
		t.Fatalf("the pin saw the rollback's put-back tell its observer of %d rename(s), want its four: it reads "+
			"nothing the observer is wired into", observed)
	}
	if len(refs) != 0 {
		t.Errorf("production code refers to a test-only seam:\n%s", strings.Join(refs, "\n"))
	}
}

// seamReferences is what TestNoProductionCodeCallsATestOnlySeam reads of the
// module at root: each reference to a test-only seam in a production file
// ("file:line:col: name"), deduplicated across the three platforms; how many
// identifiers of internal/updater/rollback_replace.go resolve to
// observeRollbackRename; and which seams the checker found declared where
// testOnlySeams says. Each package is type-checked with an importer that
// answers every import with an empty package of its path, so nothing past the
// module is loaded: what resolves is the package's own objects and the names
// of its imports, which is all a reference to a seam needs. A dot-import of a
// package that declares a seam is reported, since such a reference would not
// resolve here.
func seamReferences(t *testing.T, root string) (refs []string, observed int, declared map[string]bool) {
	t.Helper()
	module := modulePathOf(t, root)
	seamsOf := map[string]map[string]string{} // package path -> seam name -> declaring file
	for _, s := range testOnlySeams {
		p := module + "/" + path.Dir(s.file)
		if seamsOf[p] == nil {
			seamsOf[p] = map[string]string{}
		}
		seamsOf[p][s.name] = s.file
	}
	const replaceFile = "internal/updater/rollback_replace.go"
	updaterPath := module + "/internal/updater"
	dirs := productionGoFiles(t, root)
	declared = map[string]bool{}
	found := map[string]bool{}
	observedBy := map[string]int{}
	for _, goos := range []string{"linux", "darwin", "windows"} {
		ctxt := build.Default
		ctxt.GOOS, ctxt.GOARCH, ctxt.CgoEnabled = goos, "amd64", false
		ctxt.BuildTags = []string{"ts_omit_webclient", "ts_omit_debugeventbus"}
		for _, dir := range sortedKeys(dirs) {
			pkgPath := module
			if dir != "." {
				pkgPath += "/" + dir
			}
			fset := token.NewFileSet()
			var files []*ast.File
			importsASeam := false
			for _, name := range dirs[dir] {
				if ok, err := ctxt.MatchFile(filepath.Join(root, filepath.FromSlash(dir)), name); err != nil {
					t.Fatal(err)
				} else if !ok {
					continue
				}
				f, err := parser.ParseFile(fset, filepath.Join(root, filepath.FromSlash(dir), name), nil, parser.SkipObjectResolution)
				if err != nil {
					t.Fatal(err)
				}
				for _, imp := range f.Imports {
					ip := strings.Trim(imp.Path.Value, `"`)
					if seamsOf[ip] == nil {
						continue
					}
					importsASeam = true
					if imp.Name != nil && imp.Name.Name == "." {
						found[fmt.Sprintf("%s: dot-imports %s, whose seams the pin cannot resolve", fset.Position(imp.Pos()), ip)] = true
					}
				}
				files = append(files, f)
			}
			own := seamsOf[pkgPath]
			if len(files) == 0 || (own == nil && !importsASeam && pkgPath != updaterPath) {
				continue
			}
			info := &types.Info{Uses: map[*ast.Ident]types.Object{}}
			conf := types.Config{Importer: emptyImporter{}, Error: func(error) {}, FakeImportC: true}
			pkg, _ := conf.Check(pkgPath, fset, files, info)
			// The package's own seams, as its scope declares them for this
			// platform.
			ownObjs := map[types.Object]string{}
			for name, file := range own {
				obj, ok := pkg.Scope().Lookup(name).(*types.Func)
				if !ok {
					continue
				}
				if rel, err := filepath.Rel(root, fset.Position(obj.Pos()).Filename); err == nil && filepath.ToSlash(rel) == file {
					declared[name] = true
				}
				ownObjs[obj] = name
			}
			var observeObj types.Object
			if pkgPath == updaterPath {
				observeObj = pkg.Scope().Lookup("observeRollbackRename")
			}
			observedHere := 0
			for id, obj := range info.Uses {
				if name, ok := ownObjs[obj]; ok {
					found[fmt.Sprintf("%s: %s", relPosition(root, fset.Position(id.Pos())), name)] = true
				}
				if observeObj != nil && obj == observeObj {
					if rel, err := filepath.Rel(root, fset.Position(id.Pos()).Filename); err == nil && filepath.ToSlash(rel) == replaceFile {
						observedHere++
					}
				}
			}
			if observeObj != nil {
				observedBy[goos] = observedHere
			}
			// Another package's seams, named through the import of the
			// package that declares them.
			for _, f := range files {
				ast.Inspect(f, func(n ast.Node) bool {
					sel, ok := n.(*ast.SelectorExpr)
					if !ok {
						return true
					}
					x, ok := sel.X.(*ast.Ident)
					if !ok {
						return true
					}
					pn, ok := info.Uses[x].(*types.PkgName)
					if !ok {
						return true
					}
					if _, seam := seamsOf[pn.Imported().Path()][sel.Sel.Name]; seam {
						found[fmt.Sprintf("%s: %s.%s", relPosition(root, fset.Position(sel.Pos())), x.Name, sel.Sel.Name)] = true
					}
					return true
				})
			}
		}
	}
	for r := range found {
		refs = append(refs, r)
	}
	sort.Strings(refs)
	observed = observedBy["linux"]
	for goos, n := range observedBy {
		if n != observed {
			t.Errorf("the put-back's observations in %s are %d for %s and %d for linux", replaceFile, n, goos, observed)
		}
	}
	return refs, observed, declared
}

// emptyImporter answers every import with an empty, complete package of its
// path, named by its last element: the pin resolves a package's own objects
// and the names of its imports, never what an import declares.
type emptyImporter struct{}

func (emptyImporter) Import(p string) (*types.Package, error) {
	pkg := types.NewPackage(p, path.Base(p))
	pkg.MarkComplete()
	return pkg, nil
}

// productionGoFiles is every Go file of the module at root the go tool
// builds, but tests, by its directory's slash path below root, skipping what
// the citation guard skips (productionCalls' walk).
func productionGoFiles(t *testing.T, root string) map[string][]string {
	t.Helper()
	dirs := map[string][]string{}
	err := sweepWalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipsForCitations(root, p, d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || goToolIgnores(name) {
			return nil
		}
		rel, err := filepath.Rel(root, filepath.Dir(p))
		if err != nil {
			return err
		}
		dir := filepath.ToSlash(rel)
		dirs[dir] = append(dirs[dir], name)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return dirs
}

// modulePathOf is the module path go.mod at root declares.
func modulePathOf(t *testing.T, root string) string {
	t.Helper()
	f, err := os.Open(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		if line := strings.TrimSpace(s.Text()); strings.HasPrefix(line, "module ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "module "))
		}
	}
	t.Fatal("go.mod declares no module")
	return ""
}

// relPosition is pos with its file named by its slash path below root.
func relPosition(root string, pos token.Position) string {
	if rel, err := filepath.Rel(root, pos.Filename); err == nil {
		pos.Filename = filepath.ToSlash(rel)
	}
	return pos.String()
}

// sortedKeys is m's keys in order.
func sortedKeys(m map[string][]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
