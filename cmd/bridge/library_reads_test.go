package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// libraryReaders are the packages whose code reads files of the library in
// this process, each with what it reads: a path a client names, a path the
// scan walked, a path the manifest holds. What reads the bridge's own files
// (its config, its caches, its logs) is elsewhere, and is not asked.
var libraryReaders = map[string]string{
	"internal/api":      "the /v1 byte routes and the listing, over paths a client names",
	"internal/dlna":     "the DLNA file route, over the manifest's paths",
	"internal/manifest": "the scanner's extractors, the folder art and the lyrics sidecars, over paths the walk found",
	"internal/analyze":  "the analysis job's STREAMINFO read, over the manifest's paths",
	"internal/acoustid": "the fingerprint prefix read, over the manifest's paths",
}

// TestEveryLibraryReadOpensAsAFile fails on every production call in the
// packages that read library files (libraryReaders) that opens a file for
// reading with os.Open, os.OpenFile without a write flag, os.ReadFile or
// ioutil.ReadFile. Such a read opens through fsutil.OpenAsFile (or reads
// through fsutil.ReadAsFile), which refuses what does not open as a file and
// never waits on a named pipe, and a directory it lists opens through
// fsutil.OpenDir (os.ReadDir and filepath.WalkDir open with O_DIRECTORY
// already).
//
// A plain open of a named pipe waits for a writer, and nothing can cancel the
// wait. The byte routes stopped making one in #1082
// (TestEveryServedFileIsOpenedAsAFile), and until 2026-09-30 the scanner's
// extractors, its folder-art and sidecar reads, the listing's directory open
// and the analysis and fingerprint reads still did: a file replaced by a named
// pipe after the walk, or a named pipe called cover.jpg, held a scan worker
// and the scan with it, and a directory replaced by one held a listing.
//
// On a clean tree this finds nothing, so the tree cannot show that it still
// can. TestLibraryReadSweepOnFixtures runs the same scan over sources whose
// findings are known.
func TestEveryLibraryReadOpensAsAFile(t *testing.T) {
	root := repoRootForCitations(t)
	var findings []string
	files, asFile := 0, 0
	pkgs := make([]string, 0, len(libraryReaders))
	for pkg := range libraryReaders {
		pkgs = append(pkgs, pkg)
	}
	sort.Strings(pkgs)
	for _, pkg := range pkgs {
		entries, err := os.ReadDir(filepath.Join(root, filepath.FromSlash(pkg)))
		if err != nil {
			t.Fatalf("%s (%s): %v", pkg, libraryReaders[pkg], err)
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || goToolIgnores(name) {
				continue
			}
			src, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(pkg), name))
			if err != nil {
				t.Fatal(err)
			}
			found, n, err := libraryReadOpensIn(pkg+"/"+name, string(src))
			if err != nil {
				t.Fatal(err)
			}
			files++
			asFile += n
			findings = append(findings, found...)
		}
	}
	t.Logf("read %d production .go files in %d packages, holding %d reads through fsutil", files, len(pkgs), asFile)
	// A scan that reads nothing reports nothing. The five packages held 205
	// production files and 22 reads through fsutil when these floors were
	// set.
	if files < 100 || asFile < 15 {
		t.Fatalf("read %d production .go files holding %d reads through fsutil, want >=100 and >=15: "+
			"the scan is not seeing the packages", files, asFile)
	}
	for _, f := range findings {
		t.Errorf("%s; a library file opens with fsutil.OpenAsFile (or fsutil.ReadAsFile, "+
			"fsutil.OpenDir for a directory), which never waits on a named pipe", f)
	}
}

// libraryReadOpensIn returns each read-open call in the Go source src, named
// rel, as "rel:line: os.X", with how many reads it makes through fsutil's
// OpenAsFile, ReadAsFile and OpenDir. The packages are found by the names src
// imports them under.
func libraryReadOpensIn(rel, src string) (findings []string, asFile int, err error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, rel, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, 0, err
	}
	osName := localNameOf(f, "os")
	ioutilName := localNameOf(f, "io/ioutil")
	fsutilName := localNameOf(f, "github.com/acoseac/1-bit-bridge/internal/fsutil")
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		read := false
		switch {
		case pkg.Name == fsutilName:
			switch sel.Sel.Name {
			case "OpenAsFile", "ReadAsFile", "OpenDir":
				asFile++
			}
		case pkg.Name == osName:
			switch sel.Sel.Name {
			case "Open", "ReadFile":
				read = true
			case "OpenFile":
				read = len(call.Args) < 2 || !namesAWriteFlag(call.Args[1])
			}
		case pkg.Name == ioutilName:
			read = sel.Sel.Name == "ReadFile"
		}
		if read {
			findings = append(findings, fmt.Sprintf("%s:%d: %s.%s", rel, fset.Position(call.Pos()).Line, pkg.Name, sel.Sel.Name))
		}
		return true
	})
	return findings, asFile, nil
}

// namesAWriteFlag reports whether an os.OpenFile flag expression names
// O_WRONLY or O_RDWR: an open that writes, which is not a read of the
// library.
func namesAWriteFlag(flags ast.Expr) bool {
	writes := false
	ast.Inspect(flags, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && (id.Name == "O_WRONLY" || id.Name == "O_RDWR") {
			writes = true
		}
		return !writes
	})
	return writes
}

// TestLibraryReadSweepOnFixtures runs TestEveryLibraryReadOpensAsAFile's scan
// over sources whose findings are known: each read-open it must report, under
// the default import names and under others, and what it must pass (a write,
// a directory listing, a read through fsutil).
func TestLibraryReadSweepOnFixtures(t *testing.T) {
	const header = "package p\n\nimport (\n\t\"io/ioutil\"\n\t\"os\"\n\n\t\"github.com/acoseac/1-bit-bridge/internal/fsutil\"\n)\n\n"
	for _, tc := range []struct {
		name, src string
		want      []string
		asFile    int
	}{
		{"os.Open", header + "func f() {\n\t_, _ = os.Open(\"x\")\n}\n",
			[]string{"p.go:11: os.Open"}, 0},
		{"os.ReadFile", header + "func f() {\n\t_, _ = os.ReadFile(\"x\")\n}\n",
			[]string{"p.go:11: os.ReadFile"}, 0},
		{"ioutil.ReadFile", header + "func f() {\n\t_, _ = ioutil.ReadFile(\"x\")\n}\n",
			[]string{"p.go:11: ioutil.ReadFile"}, 0},
		{"os.OpenFile read-only", header + "func f() {\n\t_, _ = os.OpenFile(\"x\", os.O_RDONLY|syscall.O_NONBLOCK, 0)\n}\n",
			[]string{"p.go:11: os.OpenFile"}, 0},
		{"os.Open under another import name", "package p\n\nimport goos \"os\"\n\nfunc f() {\n\t_, _ = goos.Open(\"x\")\n}\n",
			[]string{"p.go:6: goos.Open"}, 0},
		{"an open inside a closure", header + "var f = func() {\n\t_, _ = os.Open(\"x\")\n}\n",
			[]string{"p.go:11: os.Open"}, 0},
		{"os.OpenFile that writes", header + "func f() {\n\t_, _ = os.OpenFile(\"x\", os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)\n\t_, _ = os.OpenFile(\"y\", os.O_RDWR, 0)\n}\n",
			nil, 0},
		{"a directory listed", header + "func f() {\n\t_, _ = os.ReadDir(\"dir\")\n}\n",
			nil, 0},
		{"reads through fsutil", header + "func f() {\n\t_, _, _ = fsutil.OpenAsFile(\"x\")\n\t_, _ = fsutil.ReadAsFile(\"y\")\n\t_, _ = fsutil.OpenDir(\"z\")\n}\n",
			nil, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, asFile, err := libraryReadOpensIn("p.go", tc.src)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Join(got, "\n") != strings.Join(tc.want, "\n") || asFile != tc.asFile {
				t.Errorf("found %q with %d reads through fsutil, want %q with %d", got, asFile, tc.want, tc.asFile)
			}
		})
	}
}
