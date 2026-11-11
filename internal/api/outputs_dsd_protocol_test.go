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
	// transports are playback's Transport words by constant: an
	// observation's transport is any of them, a tuple's TransportDoP alone
	// (TupleTransport; native DSD is named by none).
	transports := map[string]string{}
	protocolGoFiles(t, filepath.Join(root, "internal/player/playback"), func(f *ast.File) {
		ast.Inspect(f, func(n ast.Node) bool {
			if v, ok := n.(*ast.ValueSpec); ok {
				for i, name := range v.Names {
					if strings.HasPrefix(name.Name, "Transport") && i < len(v.Values) {
						transports[name.Name] = protocolString(t, v.Values[i])
					}
				}
			}
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
						// The nativeStream adapter and cached sink positions have
						// numeric states. Every string observation must resolve;
						// a new expression must not silently escape the census.
						if sel, ok := a.Rhs[i].(*ast.SelectorExpr); ok {
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
	// Since B728's cutover (B7) production's tuples are the effective
	// policy's (internal/player/policy): its pause words must be the
	// approval's, and its native formats among them, so the documented lists
	// hold what either publishes.
	policyPause, policyNative := map[string]bool{}, map[string]bool{}
	protocolGoFiles(t, filepath.Join(root, "internal/player/policy"), func(f *ast.File) {
		ast.Inspect(f, func(n ast.Node) bool {
			v, ok := n.(*ast.ValueSpec)
			if !ok {
				return true
			}
			for i, name := range v.Names {
				switch {
				case strings.HasPrefix(name.Name, "Pause") && i < len(v.Values):
					policyPause[protocolString(t, v.Values[i])] = true
				case name.Name == "nativeFormats":
					list, ok := v.Values[i].(*ast.CompositeLit)
					if !ok {
						t.Fatal("unresolved nativeFormats initializer")
					}
					for _, entry := range list.Elts {
						sel, ok := entry.(*ast.SelectorExpr)
						if !ok {
							t.Fatalf("unresolved native format %T", entry)
						}
						value, found := formats[sel.Sel.Name]
						if !found {
							t.Fatalf("unknown native format constant %s", sel.Sel.Name)
						}
						policyNative[value] = true
					}
				}
			}
			return true
		})
	})
	if len(policyPause) == 0 || !reflect.DeepEqual(policyPause, pause) {
		t.Errorf("the effective policy's pause words %v, the approval's %v: production and the harness publish one vocabulary", policyPause, pause)
	}
	if len(policyNative) == 0 {
		t.Error("the effective policy names no native format")
	}
	for value := range policyNative {
		if !approvedFormats[value] {
			t.Errorf("the effective policy's native format %s is not among the approval's", value)
		}
	}
	// A published tuple is one of the approval's formats (B470 2d.2): a
	// native DSD word (DSDWordBytes) or DoP's PCM carrier word, the one the
	// approval takes that is no DSD word.
	dop := map[string]bool{}
	for value := range approvedFormats {
		if format.DSDWordBytes(format.ALSAFormat(value)) > 0 {
			native[value] = true
		} else {
			dop[value] = true
		}
	}
	// Every Transport word an observation names, converted's included (B728
	// B8 publishes a converted occurrence's observation).
	observed := map[string]bool{}
	for _, value := range transports {
		observed[value] = true
	}
	tupleTransport := map[string]bool{}
	if value, ok := transports["TransportDoP"]; ok {
		tupleTransport[value] = true
	}
	read := dsdReadSideTokens(t, root)
	for _, tc := range []struct {
		name, field string
		values      map[string]bool
	}{
		{"tuple_format", "dsdPlayback.tuples[].format", approvedFormats},
		{"tuple_transport", "dsdPlayback.tuples[].transport", tupleTransport},
		{"pause", "dsdPlayback.tuples[].pause", pause},
		{"tuple_evidence", "dsdPlayback.tuples[].evidence", read.rungs},
		{"converted_dither", "dsdPlayback.converted[].dither", read.dithers},
		{"converted_filter", "dsdPlayback.converted[].filter", read.filters},
		{"refused_reason", "dsdPlayback.refused[].reason", read.refusals},
		{"warning_code", "dsdPlayback.warnings[].code", read.warningCodes},
		{"warning_reason", "dsdPlayback.warnings[].reason", read.warningReasons},
		{"state", "playback.state", states},
		{"transport", "playback.transport", observed},
		{"pcm_format", "playback.format` (PCM)", pcm},
		{"dsd_format", "playback.format` (native DSD)", native},
		{"dop_format", "playback.format` (DoP)", dop},
		{"playback_evidence", "playback.evidence", read.rungs},
		{"playback_dither", "playback.converted.dither", read.dithers},
		{"playback_filter", "playback.converted.filter", read.filters},
		{"dsd_mode", "dsdMode", read.modes},
		{"caveat_code", "dsdDAC.profile.caveats[].code", read.caveatCodes},
		{"caveat_transport", "dsdDAC.profile.caveats[].transport", read.caveatTransports},
		{"choice_mode", "dsdDAC.choice.mode", read.modes},
		{"option_mode", "dsdDAC.options[].mode", read.optionModes},
		{"option_evidence", "dsdDAC.options[].rates[].evidence", read.rungs},
		{"option_blocked", "dsdDAC.options[].blocked", read.blocked},
		{"option_dither", "dsdDAC.options[].dither", read.dithers},
		{"first_use_transport", "dsdDAC.firstUse[].transport", read.transports},
		{"first_use_state", "dsdDAC.firstUse[].state", read.firstUse},
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
