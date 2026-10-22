package main

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/alsa"
	"github.com/acoseac/1-bit-bridge/internal/api"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/doctor"
	"github.com/acoseac/1-bit-bridge/internal/mpd"
)

// TestTheDoctorsOutputsBuilderReadsNoHostTree: bridge doctor's one-shot
// outputs builder reads the cards it is built with, never the host's own
// /proc by default. The localOutputs gate opens that path on Linux alone,
// so a macOS run never took it: stood on Linux here, with MPD stood in for
// and the cards left as the doctor builds them, a checklist run asks the
// host-read watch nothing, and the DAC line reads the empty tree this test
// binary reads in place of the host's.
func TestTheDoctorsOutputsBuilderReadsNoHostTree(t *testing.T) {
	before := len(hostReads())
	none := 0
	cfg := &config.Config{DataDir: t.TempDir(), ListenAddress: "127.0.0.1:0", Output: config.OutputConfig{MPD: "on", LoopbackPort: &none}}
	checks := doctorOutputChecklist(cfg, func(c *api.LocalOutputsConfig) {
		c.GOOS = "linux"
		c.Inspect = func(context.Context, mpd.Options) mpd.Report { return mpd.Report{} }
		c.Greet = func(context.Context, string) bool { return false }
		c.Getenv = func(string) string { return "" }
	})(context.Background())
	if read := hostReads()[before:]; len(read) > 0 {
		t.Errorf("the doctor's outputs builder read the host's trees (%d reads):\n%s", len(read), strings.Join(read, "\n"))
	}
	if dac := findCheck(t, doctor.Report{Checks: checks}, "dac"); dac.Summary != "No DAC found on this box" {
		t.Errorf("dac line %+v, want the empty tree's: no DAC", dac)
	}
}

// TestTheSuiteReadsEmptyTreesForTheHosts: in this test binary hostTrees is
// two empty directories, which TestMain made, and not the host's /proc and
// /sys, so every path that reads the host's trees through it reads nothing
// of the box the suite runs on.
func TestTheSuiteReadsEmptyTreesForTheHosts(t *testing.T) {
	if hostTrees.proc == alsa.DefaultProcRoot || hostTrees.sysfs == alsa.DefaultSysfsRoot {
		t.Fatalf("hostTrees is the host's own (%+v): TestMain did not set it", hostTrees)
	}
	for _, dir := range []string{hostTrees.proc, hostTrees.sysfs} {
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) != 0 {
			t.Errorf("%s is not an empty directory: %d entries, %v", dir, len(entries), err)
		}
	}
}

// TestOnlyHostTreesNamesTheHostsTrees: in this package's own code the
// host's /proc and /sys are named in output_host.go alone, an ALSA reader
// is never built without its root, and every outputs builder is handed
// its cards, since the one GET /v1/outputs falls back to reads the host's
// /proc. Any of the three would take a test that reaches it, on Linux, to
// the host's trees.
func TestOnlyHostTreesNamesTheHostsTrees(t *testing.T) {
	fset := token.NewFileSet()
	for _, name := range nonTestGoFilesInPackage(t) {
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.SelectorExpr:
				if isIdent(n.X, "alsa") && (n.Sel.Name == "DefaultProcRoot" || n.Sel.Name == "DefaultSysfsRoot") && name != "output_host.go" {
					t.Errorf("%s names alsa.%s: the host's trees are hostTrees", fset.Position(n.Pos()), n.Sel.Name)
				}
			case *ast.CompositeLit:
				sel, ok := n.Type.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				keys := map[string]bool{}
				forEachKeyedElement(n, func(key *ast.Ident, _ ast.Expr) { keys[key.Name] = true })
				for _, want := range []struct{ pkg, typ, key string }{
					{"alsa", "Reader", "ProcRoot"},
					{"alsa", "SysfsReader", "Root"},
					{"api", "LocalOutputsConfig", "Cards"},
				} {
					if isIdent(sel.X, want.pkg) && sel.Sel.Name == want.typ && !keys[want.key] {
						t.Errorf("%s builds a %s.%s without %s, which then reads the host's trees",
							fset.Position(n.Pos()), want.pkg, want.typ, want.key)
					}
				}
			}
			return true
		})
	}
}
