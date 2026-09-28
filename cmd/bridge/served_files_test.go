package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestEveryServedFileIsOpenedAsAFile fails on every production declaration
// that passes http.ServeContent a file it opened itself with os.Open or
// os.OpenFile. A route that serves a file's bytes opens it through
// fsutil.OpenAsFile, which refuses what does not open as a file
// (fsutil.NotAFile) and never waits on a named pipe.
//
// Until 2026-09-28 /v1/download, /v1/read, the web player's audio route and
// the DLNA file route opened library paths with os.Open, so a FIFO named
// like a track held its request, and the updater session it had begun,
// until something wrote to it; the scanner had stopped indexing such
// entries that day (#1070), and a route serves whatever path a client
// names. The fix enumerated four routes, and this is what keeps a fifth
// from being written the old way: the cache routes (artwork, booklets,
// playlist covers, waveforms) serve files the bridge wrote itself, and open
// them the same way so that the rule has no exceptions to keep.
//
// It reads one declaration at a time, so a file opened in one function and
// served from another goes unseen. No such split exists today, and every
// byte route in the tree opens and serves in one place.
//
// On a clean tree this finds nothing, so the tree cannot show that it still
// can. TestServedFileSweepOnFixtures runs the same scan over sources whose
// findings are known.
func TestEveryServedFileIsOpenedAsAFile(t *testing.T) {
	root := repoRootForCitations(t)
	var findings []string
	files, serves := 0, 0
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// The blank-keeper sweep's rule for a directory: nothing
			// under one that no build of this module compiles.
			return keeperWalkDir(root, path, d.Name())
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || goToolIgnores(name) {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		found, n, err := servedFileOpensIn(filepath.ToSlash(rel), string(src))
		files++
		serves += n
		findings = append(findings, found...)
		return err
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	t.Logf("read %d production .go files holding %d http.ServeContent calls", files, serves)
	// A walk that reads nothing reports nothing. The tree held 438
	// production files and 12 http.ServeContent calls when these floors
	// were set.
	if files < 100 || serves < 5 {
		t.Fatalf("read %d production .go files and %d http.ServeContent calls under %s, "+
			"want >=100 and >=5 — the walk is not seeing the tree", files, serves, root)
	}
	for _, f := range findings {
		t.Errorf("%s serves a file it opened with os.Open or os.OpenFile; open it with "+
			"fsutil.OpenAsFile, which never waits on a named pipe", f)
	}
}

// servedFileOpensIn returns each top-level declaration of the Go source src,
// named rel, that calls http.ServeContent and also os.Open or os.OpenFile,
// as "rel:line: name", with how many http.ServeContent calls src makes. The
// packages are found by the names src imports them under.
func servedFileOpensIn(rel, src string) (findings []string, serves int, err error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, rel, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, 0, err
	}
	httpName, osName := localNameOf(f, "net/http"), localNameOf(f, "os")
	if httpName == "" {
		return nil, 0, nil
	}
	for _, decl := range f.Decls {
		var serve, open bool
		ast.Inspect(decl, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			switch {
			case !ok:
			case pkg.Name == httpName && sel.Sel.Name == "ServeContent":
				serve = true
				serves++
			case pkg.Name == osName && (sel.Sel.Name == "Open" || sel.Sel.Name == "OpenFile"):
				open = true
			}
			return true
		})
		if serve && open {
			name := "a package-level declaration"
			if fd, ok := decl.(*ast.FuncDecl); ok {
				name = fd.Name.Name
			}
			findings = append(findings, fmt.Sprintf("%s:%d: %s", rel, fset.Position(decl.Pos()).Line, name))
		}
	}
	return findings, serves, nil
}

// localNameOf returns the name f knows the package at path by, or "" when f
// does not import it under a name a call can be qualified with.
func localNameOf(f *ast.File, path string) string {
	for _, imp := range f.Imports {
		if p, err := strconv.Unquote(imp.Path.Value); err != nil || p != path {
			continue
		}
		if name := importName(imp); name != "_" && name != "." {
			return name
		}
	}
	return ""
}

// TestServedFileSweepOnFixtures runs TestEveryServedFileIsOpenedAsAFile's
// scan over sources whose findings are known: a handler that opens with
// os.Open or os.OpenFile and serves, under the default import names and
// under others, and inside a closure; and three that must pass, one opening
// through fsutil.OpenAsFile, one that opens a directory it never serves, and
// one serving bytes it never opened.
func TestServedFileSweepOnFixtures(t *testing.T) {
	const header = "package p\n\nimport (\n\t\"bytes\"\n\t\"net/http\"\n\t\"os\"\n\n\t\"example/fsutil\"\n)\n\n"
	for _, tc := range []struct {
		name, src string
		want      []string
		serves    int
	}{
		{"os.Open then ServeContent", header +
			"func serve(w http.ResponseWriter, r *http.Request) {\n\tf, _ := os.Open(\"x\")\n\thttp.ServeContent(w, r, \"x\", time.Time{}, f)\n}\n",
			[]string{"p.go:11: serve"}, 1},
		{"os.OpenFile under other import names", "package p\n\nimport (\n\tgoos \"os\"\n\tstdhttp \"net/http\"\n)\n\n" +
			"func (s *S) serve(w stdhttp.ResponseWriter, r *stdhttp.Request) {\n\tf, _ := goos.OpenFile(\"x\", goos.O_RDONLY, 0)\n\tstdhttp.ServeContent(w, r, \"x\", time.Time{}, f)\n}\n",
			[]string{"p.go:8: serve"}, 1},
		{"inside a closure", header +
			"func Handler() http.HandlerFunc {\n\treturn func(w http.ResponseWriter, r *http.Request) {\n\t\tf, _ := os.Open(\"x\")\n\t\thttp.ServeContent(w, r, \"x\", time.Time{}, f)\n\t}\n}\n",
			[]string{"p.go:11: Handler"}, 1},
		{"opened as a file", header +
			"func serve(w http.ResponseWriter, r *http.Request) {\n\tf, info, _ := fsutil.OpenAsFile(\"x\")\n\thttp.ServeContent(w, r, info.Name(), info.ModTime(), f)\n}\n",
			nil, 1},
		{"a directory opened and never served", header +
			"func list(w http.ResponseWriter, r *http.Request) {\n\td, _ := os.Open(\"dir\")\n\t_, _ = d.Readdir(-1)\n}\n",
			nil, 0},
		{"bytes served and never opened", header +
			"func serve(w http.ResponseWriter, r *http.Request) {\n\thttp.ServeContent(w, r, \"\", time.Time{}, bytes.NewReader(nil))\n}\n",
			nil, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, serves, err := servedFileOpensIn("p.go", tc.src)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Join(got, "\n") != strings.Join(tc.want, "\n") || serves != tc.serves {
				t.Errorf("found %q with %d serves, want %q with %d", got, serves, tc.want, tc.serves)
			}
		})
	}
}
