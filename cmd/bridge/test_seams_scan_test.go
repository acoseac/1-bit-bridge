package main

import (
	"bufio"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// isSeamName reports whether name is a seam's: exported and ending in
// "ForTest", the name a hook a non-test file declares for tests alone takes.
func isSeamName(name string) bool {
	return ast.IsExported(name) && strings.HasSuffix(name, "ForTest")
}

// isTestSupportPackage reports whether a package of this name is a
// test-support package, a helper only tests import (backuptest, sweeptest):
// one whose name ends in "test" and is not main. The pin holds the claim to
// the code: no non-test file outside such a package may import one.
func isTestSupportPackage(name string) bool {
	return name != "main" && strings.HasSuffix(name, "test")
}

// seamDecl is one top-level declaration of a seam in a non-test file: its
// file's slash path below the root, its line, what it declares (func, method,
// var or type) and its name's node, the one identifier of the seam's name
// there that is no reference.
type seamDecl struct {
	file string
	line int
	kind string
	name *ast.Ident
}

// seamRef is a reference to a seam in a non-test file: an identifier of a
// seam's name that is no declaration's name node, a selector's Sel included,
// or a //go:linkname directive naming one, whose ident is nil.
type seamRef struct {
	name         string
	file         string // slash path below the root
	line, column int
	pos          token.Pos
	ident        *ast.Ident
	testSupport  bool // its file's package is a test-support package
}

// String is the reference as the pin names it: file:line:column: name.
func (r seamRef) String() string {
	return fmt.Sprintf("%s:%d:%d: %s", r.file, r.line, r.column, r.name)
}

// seamScan is what scanSeams reads of a module: every non-test Go file, every
// seam they declare and every reference they make to one.
type seamScan struct {
	module string
	fset   *token.FileSet
	files  map[string]*ast.File  // by slash path below the root
	seams  map[string][]seamDecl // by the seam's name
	refs   []seamRef             // in file, line and column order
	// testSupportDirs are the directories, slash paths below the root,
	// whose non-test files declare a test-support package.
	testSupportDirs map[string]bool
}

// scanSeams parses every non-test Go file of the module at root, whatever
// its build constraints, a file name's GOOS and GOARCH included, walking as
// the citation guard walks (skipsForCitations: another checkout, another
// module, build output and make test's TMPDIR are not this module's files),
// and finds in them every seam declared at the top level (an exported
// function, method, var or type whose name ends in "ForTest") and every
// reference to one. A reference is told by its name alone: nothing is
// type-checked, so an identifier that would resolve to something else of a
// seam's name is a reference too, a false positive for the pin's allowlist,
// and no reference a type checker could not resolve is missed.
func scanSeams(root string) (*seamScan, error) {
	module, err := goModModule(root)
	if err != nil {
		return nil, err
	}
	s := &seamScan{
		module:          module,
		fset:            token.NewFileSet(),
		files:           map[string]*ast.File{},
		seams:           map[string][]seamDecl{},
		testSupportDirs: map[string]bool{},
	}
	err = sweepWalkDir(root, func(p string, d os.DirEntry, err error) error {
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
		f, err := parser.ParseFile(s.fset, p, nil, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		s.files[rel] = f
		if isTestSupportPackage(f.Name.Name) {
			s.testSupportDirs[path.Dir(rel)] = true
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	declNodes := map[*ast.Ident]bool{}
	for _, rel := range s.sortedFiles() {
		for _, decl := range s.files[rel].Decls {
			for _, d := range seamDeclsOf(decl) {
				d.file, d.line = rel, s.fset.Position(d.name.Pos()).Line
				s.seams[d.name.Name] = append(s.seams[d.name.Name], d)
				declNodes[d.name] = true
			}
		}
	}
	for _, rel := range s.sortedFiles() {
		f := s.files[rel]
		testSupport := isTestSupportPackage(f.Name.Name)
		add := func(name string, pos token.Pos, id *ast.Ident) {
			p := s.fset.Position(pos)
			s.refs = append(s.refs, seamRef{name: name, file: rel, line: p.Line, column: p.Column, pos: pos, ident: id, testSupport: testSupport})
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && !declNodes[id] {
				if _, seam := s.seams[id.Name]; seam {
					add(id.Name, id.Pos(), id)
				}
			}
			return true
		})
		// A linkname directive refers to what it names without an
		// identifier: its local name or its target, a package path and a
		// name after the last dot.
		for _, cg := range f.Comments {
			for _, c := range cg.List {
				fields := strings.Fields(c.Text)
				if len(fields) == 0 || fields[0] != "//go:linkname" {
					continue
				}
				for _, field := range fields[1:] {
					name := field[strings.LastIndex(field, ".")+1:]
					if _, seam := s.seams[name]; seam {
						add(name, c.Pos(), nil)
					}
				}
			}
		}
	}
	sort.SliceStable(s.refs, func(i, j int) bool {
		a, b := s.refs[i], s.refs[j]
		if a.file != b.file {
			return a.file < b.file
		}
		if a.line != b.line {
			return a.line < b.line
		}
		return a.column < b.column
	})
	return s, nil
}

// seamDeclsOf is every seam a top-level declaration declares, with its kind
// and its name's node.
func seamDeclsOf(decl ast.Decl) []seamDecl {
	var out []seamDecl
	switch d := decl.(type) {
	case *ast.FuncDecl:
		if isSeamName(d.Name.Name) {
			kind := "func"
			if d.Recv != nil {
				kind = "method"
			}
			out = append(out, seamDecl{kind: kind, name: d.Name})
		}
	case *ast.GenDecl:
		for _, spec := range d.Specs {
			switch spec := spec.(type) {
			case *ast.ValueSpec:
				if d.Tok != token.VAR {
					continue
				}
				for _, n := range spec.Names {
					if isSeamName(n.Name) {
						out = append(out, seamDecl{kind: "var", name: n})
					}
				}
			case *ast.TypeSpec:
				if isSeamName(spec.Name.Name) {
					out = append(out, seamDecl{kind: "type", name: spec.Name})
				}
			}
		}
	}
	return out
}

// sortedFiles is the scan's files' paths in order.
func (s *seamScan) sortedFiles() []string {
	out := make([]string, 0, len(s.files))
	for rel := range s.files {
		out = append(out, rel)
	}
	sort.Strings(out)
	return out
}

// goModModule is the module path the go.mod at root declares.
func goModModule(root string) (string, error) {
	f, err := os.Open(filepath.Join(root, "go.mod"))
	if err != nil {
		return "", err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); strings.HasPrefix(line, "module ") {
			return strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "module ")), `"`), nil
		}
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	return "", errors.New("go.mod declares no module")
}
