package dsdconv

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/sweeptest"
)

// module is this repository's module path.
const module = "github.com/acoseac/1-bit-bridge"

// TestNoConvertedStreamIsServed (B728 B11; CLAUDE.md, the bullet on no
// server-side transcoding on the wire): the player's converted PCM reaches
// the player's DAC alone, never /v1, DLNA or the loopback route. The
// converter's one importer in the module's code is the player's backend
// (internal/player/playback), which hands every converted frame to its sink
// and exports nothing that gives samples out; cmd/bridge, which wires the
// routes beside the player, imports no converter; and the packages that serve
// the wire, /v1 (internal/api), DLNA, its file route and the loopback track
// route (internal/dlna) and the console (internal/admin), reach neither the
// converter nor the backend through their imports, at any depth.
func TestNoConvertedStreamIsServed(t *testing.T) {
	root := moduleRoot(t)
	imports, files := moduleImports(t, root)
	if files < 500 {
		t.Fatalf("the sweep parsed %d of the module's Go files: too few, it read the wrong tree", files)
	}
	self := module + "/internal/player/dsdconv"
	backend := module + "/internal/player/playback"
	var importers []string
	for pkg, imps := range imports {
		if slices.Contains(imps, self) {
			importers = append(importers, pkg)
		}
	}
	slices.Sort(importers)
	if !slices.Equal(importers, []string{backend}) {
		t.Errorf("the converter is imported by %v; want the player's backend alone", importers)
	}
	for _, served := range []string{"internal/api", "internal/dlna", "internal/admin"} {
		pkg := module + "/" + served
		if _, ok := imports[pkg]; !ok {
			t.Fatalf("%s is not in the sweep: the guard reads nothing of it", served)
		}
		for reached := range reach(imports, pkg) {
			if reached == self || reached == backend {
				t.Errorf("%s reaches %s through its imports: a route could serve the player's converted PCM", served, reached)
			}
		}
	}
	for _, imp := range imports[module+"/cmd/bridge"] {
		if imp == self {
			t.Errorf("cmd/bridge imports the converter: the routes it wires could serve the player's converted PCM")
		}
	}
	for _, fn := range samplesOut(t, filepath.Join(root, "internal", "player", "playback")) {
		t.Errorf("the backend exports %s, which gives decoded or converted samples out", fn)
	}
}

// moduleRoot walks up from the test's working directory to the directory
// holding go.mod.
func moduleRoot(t *testing.T) string {
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

// moduleImports is each package of the module under root and the module's
// packages its code imports (test files aside, every build tag's files
// counted), and the files it parsed. It decides what it opens from each
// name: it descends no directory below root that is another checkout
// (sweeptest.IsOtherCheckout), VCS metadata or hidden, test data, vendored or
// scratch (a "_" name), and it opens only a Go file that is no test's.
func moduleImports(t *testing.T, root string) (map[string][]string, int) {
	t.Helper()
	imports := map[string][]string{}
	files := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if path != root && (strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") || name == "testdata" || name == "vendor" || sweeptest.IsOtherCheckout(root, path)) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		files++
		rel, err := filepath.Rel(root, filepath.Dir(path))
		if err != nil {
			return err
		}
		pkg := module
		if rel != "." {
			pkg += "/" + filepath.ToSlash(rel)
		}
		if _, ok := imports[pkg]; !ok {
			imports[pkg] = nil
		}
		for _, imp := range f.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				return err
			}
			if (p == module || strings.HasPrefix(p, module+"/")) && !slices.Contains(imports[pkg], p) {
				imports[pkg] = append(imports[pkg], p)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return imports, files
}

// reach is every module package from imports, at any depth.
func reach(imports map[string][]string, from string) map[string]bool {
	seen := map[string]bool{}
	stack := []string{from}
	for len(stack) > 0 {
		p := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for _, q := range imports[p] {
			if !seen[q] {
				seen[q] = true
				stack = append(stack, q)
			}
		}
	}
	return seen
}

// samplesOut are the exported functions and methods of the package in dir
// that answer a decoder, a converter, a reader or samples: what would hand
// the PCM the player plays to something but its sink.
func samplesOut(t *testing.T, dir string) []string {
	t.Helper()
	names, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || !fn.Name.IsExported() || fn.Type.Results == nil {
				continue
			}
			for _, r := range fn.Type.Results.List {
				if gives := typeText(r.Type); gives != "" {
					out = append(out, filepath.Base(name)+": "+fn.Name.Name+" ("+gives+")")
				}
			}
		}
	}
	return out
}

// typeText names what a result type gives out of a forbidden kind, "" for
// none: a decoder, a converter, a reader or a slice of samples.
func typeText(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.StarExpr:
		return typeText(x.X)
	case *ast.ArrayType:
		if id, ok := x.Elt.(*ast.Ident); ok && id.Name == "int32" {
			return "[]int32"
		}
	case *ast.SelectorExpr:
		pkg, ok := x.X.(*ast.Ident)
		if !ok {
			return ""
		}
		switch {
		case pkg.Name == "dsdconv":
			return "dsdconv." + x.Sel.Name
		case pkg.Name == "decode" && x.Sel.Name == "Decoder":
			return "decode.Decoder"
		case pkg.Name == "io" && strings.HasSuffix(x.Sel.Name, "Reader"):
			return "io." + x.Sel.Name
		}
	}
	return ""
}
