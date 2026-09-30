package fsutil

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/sweeptest"
)

// libraryRootWalks names every function in the bridge that hands
// filepath.WalkDir a directory that can be a configured library root. Each
// must start the walk from WalkableRoot's answer: it calls WalkableRoot
// itself, or calls the helper of its own file named here, which does.
//
// Walked as the root the config names, a root that is a link to a directory
// was one entry that is not a directory. That is how the scanner indexed
// nothing under a linked root and then read it as a clean-empty mount, how
// a subtree scan of it reaped every row, how the watcher registered no watch
// and the doctor counted no directory, and how a folder upscale of it
// enqueued the folder itself. Five walks, found by sweeping for the call,
// which is what this test does so that a sixth is found the same way.
var libraryRootWalks = map[string]string{
	"internal/manifest/scanner.go (*Scanner).walkRoot":    "",
	"internal/manifest/scanner.go (*Scanner).ScanSubtree": "",
	"internal/manifest/watcher.go (*Watcher).addTree":     "watchWalkStart",
	"internal/doctor/inotify_linux.go countDirs":          "",
	"internal/api/upscale.go (*Server).upscaleRequest":    "folderWalkPath",
}

// otherTreeWalks names every other function that walks a directory tree,
// with what it walks: none of them is ever handed a library root.
var otherTreeWalks = map[string]string{
	"internal/doctor/cgroup.go findCgroups":                "the cgroup2 mount",
	"internal/trash/restore.go (*Manager).pruneEmptyStamp": "one stamp directory of a root's trash, below the root",
	"internal/trash/restore.go (*Manager).Sweep":           "one stamp directory of a root's trash, below the root",
	"internal/trash/trash.go (*Manager).List":              "one stamp directory of a root's trash, below the root",
	"internal/integrity/inventory.go TakeSidecarInventory": "the variants directory, which resolveSidecarRoot resolves",
	"cmd/bridge/artwork.go artworkCacheHasOrphans":         "the artwork cache, which artworkWalkRoot resolves",
	"cmd/bridge/artwork.go runArtworkGC":                   "the artwork cache, which artworkWalkRoot resolves",
	"cmd/bridge/artwork.go sweepArtworkCache":              "the artwork cache, which artworkWalkRoot resolves",
}

// TestEveryWalkOfALibraryRootStartsFromWalkableRoot sweeps the bridge's
// production code for every call of filepath.WalkDir, filepath.Walk and
// fs.WalkDir, and requires each calling function to be named in one of the
// two tables above: a walk nobody has classified is the one that repeats the
// defect. A library-root walk must reach WalkableRoot. A table entry that
// matches no call fails too, so the tables cannot keep describing code that
// has moved.
//
// It sees that the function reaches WalkableRoot, not that the answer is the
// path it walks; the tests of each walk (TestScanner_ALinkedRootIsWalkedThrough
// and its neighbours, TestWatcherWatchesALinkedLibraryRoot,
// TestCountDirsCountsThroughALinkedRoot,
// TestUpscaleFolderRequestForALinkedRootWalksThroughIt) are what drive a
// linked root through it.
func TestEveryWalkOfALibraryRootStartsFromWalkableRoot(t *testing.T) {
	root := sweepModuleRoot(t)
	sweep, err := sweepTreeWalks(root)
	if err != nil {
		t.Fatalf("sweep %s: %v", root, err)
	}
	if sweep.files < 100 {
		t.Fatalf("parsed %d production .go files under %s, want >=100: the sweep is not seeing the tree", sweep.files, root)
	}
	for _, key := range sortedKeys(sweep.walks) {
		fn := sweep.walks[key]
		helper, isRootWalk := libraryRootWalks[key]
		_, isOther := otherTreeWalks[key]
		switch {
		case isRootWalk:
			if !fn.reachesWalkableRoot(helper) {
				t.Errorf("%s walks a library root without starting from fsutil.WalkableRoot (via %q): "+
					"a root that is a link to a directory would be walked as one entry", key, helper)
			}
		case !isOther:
			t.Errorf("%s walks a directory tree and is in neither table: if its directory can be a "+
				"configured library root, start the walk from fsutil.WalkableRoot and list it in "+
				"libraryRootWalks; otherwise list it in otherTreeWalks with what it walks", key)
		}
	}
	for _, table := range []map[string]string{libraryRootWalks, otherTreeWalks} {
		for key := range table {
			if _, ok := sweep.walks[key]; !ok {
				t.Errorf("%s is listed but no longer walks a tree: remove it from the table", key)
			}
		}
	}
}

// treeWalkSweep is what sweepTreeWalks found: each function that walks a
// tree, keyed "file function", and how many production files it parsed.
type treeWalkSweep struct {
	walks map[string]walkingFunc
	files int
}

// walkingFunc is one function that walks a tree, with the file it is in and
// that file's name for this package.
type walkingFunc struct {
	decl   *ast.FuncDecl
	file   *ast.File
	fsutil string
}

// reachesWalkableRoot reports whether the function calls WalkableRoot, or,
// when helper is named, calls the function of its file of that name, which
// calls WalkableRoot.
func (w walkingFunc) reachesWalkableRoot(helper string) bool {
	if helper == "" {
		return callsSelector(w.decl.Body, w.fsutil, "WalkableRoot")
	}
	if !callsNamed(w.decl.Body, helper) {
		return false
	}
	for _, decl := range w.file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == helper && fn.Body != nil {
			return callsSelector(fn.Body, w.fsutil, "WalkableRoot")
		}
	}
	return false
}

// sweepTreeWalks parses the production Go files under root, the files the go
// tool builds, and records each function that walks a tree.
func sweepTreeWalks(root string) (treeWalkSweep, error) {
	sweep := treeWalkSweep{walks: map[string]walkingFunc{}}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return sweepDirRule(root, path, d.Name())
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") ||
			strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
			return nil
		}
		return sweep.parse(root, path)
	})
	return sweep, err
}

// sweepDirRule is the go tool's rule for which directories hold packages it
// builds, and another checkout's: SkipDir for one that holds none of this
// checkout's production code, nil to descend.
func sweepDirRule(root, path, name string) error {
	if path == root {
		return nil
	}
	if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") || name == "testdata" || name == "vendor" ||
		sweeptest.IsOtherCheckout(root, path) {
		return filepath.SkipDir
	}
	if _, err := os.Lstat(filepath.Join(path, "go.mod")); err == nil {
		return filepath.SkipDir // another module
	}
	return nil
}

// parse records the functions of the file at path that walk a tree.
func (s *treeWalkSweep) parse(root, path string) error {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		return err
	}
	s.files++
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return err
	}
	rel = filepath.ToSlash(rel)
	fsutilName, _ := importName(file, "github.com/acoseac/1-bit-bridge/internal/fsutil")
	filepathName, hasFilepath := importName(file, "path/filepath")
	fsName, hasFS := importName(file, "io/fs")
	if !hasFilepath && !hasFS {
		return nil
	}
	for _, decl := range file.Decls {
		walks := false
		ast.Inspect(decl, func(n ast.Node) bool {
			walks = walks || (hasFilepath && (isSelectorCall(n, filepathName, "WalkDir") || isSelectorCall(n, filepathName, "Walk"))) ||
				(hasFS && isSelectorCall(n, fsName, "WalkDir"))
			return !walks
		})
		if !walks {
			continue
		}
		fn, _ := decl.(*ast.FuncDecl)
		if fn == nil || fn.Body == nil {
			s.walks[rel+" (package level)"] = walkingFunc{file: file, fsutil: fsutilName, decl: &ast.FuncDecl{Body: &ast.BlockStmt{}}}
			continue
		}
		s.walks[rel+" "+funcName(fn)] = walkingFunc{decl: fn, file: file, fsutil: fsutilName}
	}
	return nil
}

// funcName is a function's name as the tables spell it: `f`, or `(*T).m` and
// `(T).m` for a method.
func funcName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	switch recv := fn.Recv.List[0].Type.(type) {
	case *ast.StarExpr:
		if id, ok := recv.X.(*ast.Ident); ok {
			return "(*" + id.Name + ")." + fn.Name.Name
		}
	case *ast.Ident:
		return "(" + recv.Name + ")." + fn.Name.Name
	}
	return "(?)." + fn.Name.Name
}

// importName is the file's name for the package at importPath, and whether
// the file imports it under a name it can call it by.
func importName(file *ast.File, importPath string) (string, bool) {
	for _, imp := range file.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil || p != importPath {
			continue
		}
		if imp.Name != nil {
			return imp.Name.Name, imp.Name.Name != "_" && imp.Name.Name != "."
		}
		return importPath[strings.LastIndex(importPath, "/")+1:], true
	}
	return "", false
}

// isSelectorCall reports whether n is a call of pkg.name.
func isSelectorCall(n ast.Node, pkg, name string) bool {
	call, ok := n.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != name {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == pkg
}

// callsSelector reports whether body calls pkg.name.
func callsSelector(body ast.Node, pkg, name string) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		found = found || (pkg != "" && isSelectorCall(n, pkg, name))
		return !found
	})
	return found
}

// callsNamed reports whether body calls a function or method named name.
func callsNamed(body ast.Node, name string) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return !found
		}
		switch f := call.Fun.(type) {
		case *ast.Ident:
			found = found || f.Name == name
		case *ast.SelectorExpr:
			found = found || f.Sel.Name == name
		}
		return !found
	})
	return found
}

// sweepModuleRoot walks up from the working directory to the directory
// holding go.mod.
func sweepModuleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test's working directory")
		}
		dir = parent
	}
}

// sortedKeys returns m's keys in order, so failures read the same each run.
func sortedKeys(m map[string]walkingFunc) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
