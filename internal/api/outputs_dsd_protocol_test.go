package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/player/format"
)

// Read the emitting definitions, not a second list of expected tokens. Parsing
// ignores comments and includes platform-specific files on every test host.
func TestDSDOutputProtocolTokens(t *testing.T) {
	root := filepath.Join("..", "..")
	doc, err := os.ReadFile(filepath.Join(root, "PROTOCOL.md"))
	if err != nil {
		t.Fatal(err)
	}
	formats := map[string]string{}
	protocolGoFiles(t, filepath.Join(root, "internal/player/format"), func(f *ast.File) {
		ast.Inspect(f, func(n ast.Node) bool {
			v, ok := n.(*ast.ValueSpec)
			if !ok {
				return true
			}
			typ, ok := v.Type.(*ast.Ident)
			if !ok || typ.Name != "ALSAFormat" {
				return true
			}
			for i, name := range v.Names {
				formats[name.Name] = protocolString(t, v.Values[i])
			}
			return true
		})
	})
	native, pcm, pause, states := map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
	protocolGoFiles(t, filepath.Join(root, "internal/player/playback"), func(f *ast.File) {
		ast.Inspect(f, func(n ast.Node) bool {
			if v, ok := n.(*ast.ValueSpec); ok && len(v.Names) == 1 && v.Names[0].Name == "packings" {
				if len(v.Values) != 1 {
					t.Fatal("packings must have one initializer")
				}
				table, ok := v.Values[0].(*ast.CompositeLit)
				if !ok {
					t.Fatal("unresolved packings initializer")
				}
				for _, row := range table.Elts {
					pair, ok := row.(*ast.KeyValueExpr)
					if !ok {
						t.Fatal("unkeyed packings row")
					}
					list, ok := pair.Value.(*ast.CompositeLit)
					if !ok {
						t.Fatal("unresolved packings row")
					}
					for _, entry := range list.Elts {
						sel, ok := entry.(*ast.SelectorExpr)
						if !ok {
							t.Fatalf("unresolved packing %T", entry)
						}
						pkg, ok := sel.X.(*ast.Ident)
						if !ok || pkg.Name != "format" {
							t.Fatal("packing outside format constants")
						}
						value, found := formats[sel.Sel.Name]
						if !found {
							t.Fatalf("unknown packing constant %s", sel.Sel.Name)
						}
						pcm[value] = true
					}
				}
			}
			if a, ok := n.(*ast.AssignStmt); ok {
				for i, lhs := range a.Lhs {
					name := ""
					switch x := lhs.(type) {
					case *ast.Ident:
						name = x.Name
					case *ast.SelectorExpr:
						name = x.Sel.Name
					}
					if (name == "state" || name == "State") && len(a.Rhs) == len(a.Lhs) {
						// The nativeStream adapter has a separate numeric sink
						// state. Every string observation assignment must resolve;
						// a new expression must not silently escape the census.
						if sel, ok := a.Rhs[i].(*ast.SelectorExpr); ok && name == "state" {
							if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "alsapcm" && strings.HasPrefix(sel.Sel.Name, "Sink") {
								continue
							}
						}
						states[protocolString(t, a.Rhs[i])] = true
					}
				}
			}
			if c, ok := n.(*ast.CompositeLit); ok {
				if typ, ok := c.Type.(*ast.Ident); ok && typ.Name == "PlaybackFacts" {
					for _, el := range c.Elts {
						kv, ok := el.(*ast.KeyValueExpr)
						if !ok {
							t.Fatal("unkeyed PlaybackFacts")
						}
						if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "State" {
							if id, ok := kv.Value.(*ast.Ident); !ok || id.Name != "state" {
								states[protocolString(t, kv.Value)] = true
							}
						}
					}
				}
			}
			return true
		})
	})
	approval, err := parser.ParseFile(token.NewFileSet(), filepath.Join(root, "internal/player/qualification/approval.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	approvedFormats := map[string]bool{}
	ast.Inspect(approval, func(n ast.Node) bool {
		b, ok := n.(*ast.BinaryExpr)
		if !ok || b.Op != token.NEQ {
			return true
		}
		s, ok := b.X.(*ast.SelectorExpr)
		if ok {
			switch s.Sel.Name {
			case "Pause":
				pause[protocolString(t, b.Y)] = true
			case "Format":
				approvedFormats[protocolString(t, b.Y)] = true
			}
		}
		return true
	})
	// The published tuple filter is DSDWordBytes over the approval's formats.
	for value := range approvedFormats {
		if format.DSDWordBytes(format.ALSAFormat(value)) > 0 {
			native[value] = true
		}
	}
	for _, tc := range []struct {
		name, field string
		values      map[string]bool
	}{
		{"native_format", "dsdPlayback.tuples[].format", native},
		{"pause", "dsdPlayback.tuples[].pause", pause},
		{"state", "playback.state", states},
		{"pcm_format", "playback.format` (PCM)", pcm},
		{"dsd_format", "playback.format` (native DSD)", native},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prefix := "| `" + tc.field
			if !strings.Contains(tc.field, "`") {
				prefix += "`"
			}
			prefix += " | "
			var got []string
			rows := 0
			for line := range strings.SplitSeq(string(doc), "\n") {
				if strings.HasPrefix(line, prefix) {
					rows++
					for _, m := range regexp.MustCompile("`([^`]+)`").FindAllStringSubmatch(strings.TrimPrefix(line, prefix), -1) {
						got = append(got, m[1])
					}
				}
			}
			var want []string
			for v := range tc.values {
				want = append(want, v)
			}
			slices.Sort(want)
			slices.Sort(got)
			if rows != 1 || len(want) == 0 || !reflect.DeepEqual(got, want) {
				t.Fatalf("%s: documented %v; emitting definitions %v (rows=%d)", tc.field, got, want, rows)
			}
		})
	}
}

func protocolGoFiles(t *testing.T, dir string, visit func(*ast.File)) {
	t.Helper()
	names, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil || len(names) == 0 {
		t.Fatalf("source glob: %v", err)
	}
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		visit(f)
	}
}
func protocolString(t *testing.T, e ast.Expr) string {
	t.Helper()
	b, ok := e.(*ast.BasicLit)
	if !ok || b.Kind != token.STRING {
		t.Fatalf("token expression %T needs a resolved string", e)
	}
	s, err := strconv.Unquote(b.Value)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
