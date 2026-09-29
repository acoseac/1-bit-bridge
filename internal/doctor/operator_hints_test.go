package doctor

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// A doctor report is read by an operator: in `bridge doctor`, under `bridge
// init`'s preflight, and on the console's doctor panel. Three hints named a
// Deps field instead, notes for whoever calls this package, and each reached
// operators (measured with the real binary on 2026-09-28):
//
//   - "pass Deps.port-apiPort", under port-api's "no port set", for every
//     config or `bridge init` flag naming :0, the ephemeral-port mode
//     config.validatePort accepts;
//   - "pass Deps.DataDir so doctor can inspect cert state", under tls-cert,
//     on every `bridge doctor` run before `bridge init`;
//   - "pass Deps.ConfigDir so doctor can verify write access", under
//     config-dir, on a `bridge doctor` run with no config and no HOME.

// TestHintsWithNothingToGradeSpeakToTheOperator drives the checks into the
// states that printed those hints, and requires a sentence about the install
// in each: what was not graded, and why. tls-cert is not among them since
// 2026-09-29 (backlog B61): with nothing to say where the certificate is, it
// answers ok "not checked" and why, as the port checks do for a config they
// cannot grade (TestTLSCertIsNotCheckedWhereNothingSaysWhereTheCertificateIs),
// and an ok prints no hint.
func TestHintsWithNothingToGradeSpeakToTheOperator(t *testing.T) {
	for _, tc := range []struct {
		name string
		c    Check
		// says is a phrase the operator's sentence must carry.
		says string
	}{
		{"port-api on port 0", checkAPIPort(t.Context(), Deps{APIPort: 0}), "port 0"},
		{"port-admin on port 0", checkAdminPort(t.Context(), Deps{AdminPort: 0}), "port 0"},
		{"config-dir with no directory", checkConfigDir(t.Context(), Deps{}), "HOME"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.c.Status != Warn {
				t.Fatalf("%s = %s %q, want the warn this state has always given", tc.c.Name, tc.c.Status, tc.c.Summary)
			}
			if strings.Contains(tc.c.Hint, "Deps.") || !strings.Contains(tc.c.Hint, tc.says) {
				t.Errorf("%s's hint is %q: it names a Deps field, or does not say %q", tc.c.Name, tc.c.Hint, tc.says)
			}
		})
	}
}

// TestNoStringInThisPackageNamesADepsField is the sweep behind that test:
// every string in the package's non-test source can reach a report, so none
// may name a Deps field. It reads string LITERALS by the AST, so the comments
// that discuss the fields (every docblock here) are not read, and a hint
// assembled from pieces ("pass Deps."+name+"Port") is still caught by its
// first piece.
func TestNoStringInThisPackageNamesADepsField(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	files, literals := 0, 0
	for _, e := range entries {
		name := e.Name()
		// The go tool's own rule, decided from the name before anything is
		// opened: an editor's .#lock of a file is not source.
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") ||
			strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(".", name), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		files++
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			literals++
			if s, err := strconv.Unquote(lit.Value); err == nil && strings.Contains(s, "Deps.") {
				t.Errorf("%s: the string %s names a Deps field, and every string here can reach an operator",
					fset.Position(lit.Pos()), lit.Value)
			}
			return true
		})
	}
	// A floor, so a sweep that read nothing cannot pass: the package has
	// dozens of files and hundreds of strings.
	if files < 10 || literals < 200 {
		t.Fatalf("the sweep read %d files and %d string literals; the package has more, so it looked "+
			"in the wrong place", files, literals)
	}
}
