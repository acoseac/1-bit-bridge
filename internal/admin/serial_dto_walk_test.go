package admin

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/usbid"
)

// moduleImport is this module's import path, whose packages the walk
// follows a field's type into.
const moduleImport = "github.com/acoseac/1-bit-bridge"

// guardImport is the package of the serial guard, usbid.Serial.
const guardImport = moduleImport + "/internal/usbid"

// TestNoDTOHoldsASerialAsText walks the type of every field of every type
// the console (this package), /v1 (internal/api), the settings
// (internal/config) and the CLI's JSON (cmd/bridge) declare, through every
// type of this module a field leads to, and fails on a field named for a
// serial that holds it as text: a string, a []string, a []byte or a
// pointer to one. A serial is a usbid.Serial, which encodes and prints as a
// redaction everywhere but the config file, or a label of it (a field whose
// name ends in Label, holding Serial.Label()). A numeric serial (an Ogg
// stream's) is no USB serial and is not looked at.
//
// The walk is over the source, since Go cannot list a package's types by
// reflection: each declared type is a root, a field's named type is
// followed into the package that declares it when that package is this
// module's, and the guard is not descended into.
func TestNoDTOHoldsASerialAsText(t *testing.T) {
	repo, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	w := &serialWalk{t: t, repo: repo, pkgs: map[string]*walkPkg{}, seen: map[string]bool{}}
	roots := 0
	for _, dir := range []string{"internal/admin", "internal/api", "internal/config", "cmd/bridge"} {
		p := w.pkg(dir)
		names := make([]string, 0, len(p.types))
		for name := range p.types {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			w.named(dir, name)
			roots++
		}
	}
	if roots < 200 || len(w.seen) < roots {
		t.Fatalf("the walk saw %d roots and %d types; the census is broken, so this test proves nothing", roots, len(w.seen))
	}
	for _, want := range []string{"internal/config.PlayerDevice", "internal/alsa.USBIdentity", "cmd/bridge.dacCardView"} {
		if !w.seen[want] {
			t.Errorf("the walk never reached %s, which holds a serial or its label", want)
		}
	}
}

// TestTheSettingsResponseCarriesNoSerial: the settings JSON is built from a
// config whose player is bound by serial, and neither the response nor its
// JSON carries the serial.
func TestTheSettingsResponseCarriesNoSerial(t *testing.T) {
	const serial = "GOBAR-7Q2X-91KZ-4471"
	cfg := &config.Config{LibraryName: "Kitchen", Output: config.OutputConfig{Player: config.PlayerConfig{Enabled: true,
		Device: config.PlayerDevice{Binding: config.BindingSerial, VendorID: "20b1", ProductID: "3009", Serial: usbid.New(serial)}}}}
	resp := settingsResponseFromConfig(cfg, false)
	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	for _, out := range []string{string(b), fmt.Sprintf("%+v", resp), fmt.Sprintf("%#v", resp)} {
		for i := 0; i+6 <= len(serial); i++ {
			if strings.Contains(out, serial[i:i+6]) {
				t.Fatalf("the settings response carries the serial: %s", out)
			}
		}
	}
}

// serialWalk is one walk in progress.
type serialWalk struct {
	t    *testing.T
	repo string
	pkgs map[string]*walkPkg
	seen map[string]bool
}

// walkPkg is one package's declared types and each file's imports.
type walkPkg struct {
	types   map[string]*ast.TypeSpec
	imports map[*ast.TypeSpec]map[string]string // a type's file: name → import path
}

// pkg parses a package's non-test files, once.
func (w *serialWalk) pkg(dir string) *walkPkg {
	if p, ok := w.pkgs[dir]; ok {
		return p
	}
	p := &walkPkg{types: map[string]*ast.TypeSpec{}, imports: map[*ast.TypeSpec]map[string]string{}}
	w.pkgs[dir] = p
	entries, err := os.ReadDir(filepath.Join(w.repo, filepath.FromSlash(dir)))
	if err != nil {
		w.t.Fatalf("read %s: %v", dir, err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") ||
			strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(w.repo, filepath.FromSlash(dir), name), nil, 0)
		if err != nil {
			w.t.Fatalf("parse %s/%s: %v", dir, name, err)
		}
		imports := map[string]string{}
		for _, im := range f.Imports {
			path, _ := strconv.Unquote(im.Path.Value)
			local := path[strings.LastIndex(path, "/")+1:]
			if im.Name != nil {
				local = im.Name.Name
			}
			imports[local] = path
		}
		for _, d := range f.Decls {
			g, ok := d.(*ast.GenDecl)
			if !ok || g.Tok != token.TYPE {
				continue
			}
			for _, s := range g.Specs {
				ts := s.(*ast.TypeSpec)
				p.types[ts.Name.Name] = ts
				p.imports[ts] = imports
			}
		}
	}
	return p
}

// named walks the type name declares in dir, once.
func (w *serialWalk) named(dir, name string) {
	key := dir + "." + name
	if w.seen[key] {
		return
	}
	w.seen[key] = true
	ts, ok := w.pkg(dir).types[name]
	if !ok {
		return
	}
	w.expr(dir, w.pkg(dir).imports[ts], key, ts.Type)
}

// expr walks a type expression met in dir's file with imports.
func (w *serialWalk) expr(dir string, imports map[string]string, at string, e ast.Expr) {
	switch x := e.(type) {
	case *ast.Ident:
		if !isPredeclared(x.Name) {
			w.named(dir, x.Name)
		}
	case *ast.SelectorExpr:
		pkg, ok := x.X.(*ast.Ident)
		if !ok {
			return
		}
		path := imports[pkg.Name]
		if path == guardImport && x.Sel.Name == "Serial" {
			return
		}
		if rel, ok := strings.CutPrefix(path, moduleImport+"/"); ok {
			w.named(rel, x.Sel.Name)
		}
	case *ast.StarExpr:
		w.expr(dir, imports, at, x.X)
	case *ast.ArrayType:
		w.expr(dir, imports, at, x.Elt)
	case *ast.MapType:
		w.expr(dir, imports, at, x.Key)
		w.expr(dir, imports, at, x.Value)
	case *ast.IndexExpr:
		w.expr(dir, imports, at, x.X)
		w.expr(dir, imports, at, x.Index)
	case *ast.IndexListExpr:
		w.expr(dir, imports, at, x.X)
		for _, i := range x.Indices {
			w.expr(dir, imports, at, i)
		}
	case *ast.StructType:
		for _, f := range x.Fields.List {
			for _, name := range fieldNames(f) {
				if strings.Contains(strings.ToLower(name), "serial") && holdsText(f.Type) && !strings.HasSuffix(name, "Label") {
					w.t.Errorf("%s.%s holds a serial as text; a serial is a usbid.Serial, or its Label in a field named …Label", at, name)
				}
			}
			w.expr(dir, imports, at, f.Type)
		}
	}
}

// fieldNames names a struct field, an embedded one by its type's name.
func fieldNames(f *ast.Field) []string {
	if len(f.Names) > 0 {
		var out []string
		for _, n := range f.Names {
			out = append(out, n.Name)
		}
		return out
	}
	e := f.Type
	if s, ok := e.(*ast.StarExpr); ok {
		e = s.X
	}
	switch x := e.(type) {
	case *ast.Ident:
		return []string{x.Name}
	case *ast.SelectorExpr:
		return []string{x.Sel.Name}
	}
	return nil
}

// holdsText reports whether a field's type is text: string, []byte, or a
// pointer, slice or array of those.
func holdsText(e ast.Expr) bool {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name == "string"
	case *ast.StarExpr:
		return holdsText(x.X)
	case *ast.ArrayType:
		if id, ok := x.Elt.(*ast.Ident); ok && (id.Name == "byte" || id.Name == "uint8") {
			return true
		}
		return holdsText(x.Elt)
	}
	return false
}

// isPredeclared reports whether name is one of Go's predeclared types.
func isPredeclared(name string) bool {
	switch name {
	case "bool", "byte", "complex64", "complex128", "error", "float32", "float64", "int", "int8", "int16", "int32",
		"int64", "rune", "string", "uint", "uint8", "uint16", "uint32", "uint64", "uintptr", "any", "comparable":
		return true
	}
	return false
}
