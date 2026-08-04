package manifest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// mewkizFlacPkg is the top-level mewkiz package. The `/meta` subpackage
// is fine and is what production actually imports — every function there
// takes a reader the caller owns.
const mewkizFlacPkg = "github.com/mewkiz/flac"

// leakyFlacConstructors are the two mewkiz/flac entry points that take a
// PATH rather than a reader. Both leak the file handle in v1.0.13:
//
//	func ParseFile(path string) (*Stream, error) {
//	        f, _ := os.Open(path)
//	        return Parse(f)          // Parse: stream = &Stream{r: bufio.NewReader(f)}
//	}
//	func (stream *Stream) Close() error {
//	        if closer, ok := stream.r.(io.Closer); ok { return closer.Close() }
//	        return nil               // *bufio.Reader is not an io.Closer -> nil
//	}
//
// So the *os.File is dropped on the floor and Close reports success
// having closed nothing. `flac.Open` has the identical shape via `New`.
// Their doc comments say "The Close method of the stream must be called
// when finished using it", which is what makes this so easy to get
// wrong: the call site looks correct.
//
// Use `os.Open` + `flac.Parse`/`flac.New` + `defer f.Close()` instead,
// so the handle has an owner.
var leakyFlacConstructors = map[string]string{
	"ParseFile": "os.Open + flac.Parse + defer f.Close()",
	"Open":      "os.Open + flac.New + defer f.Close()",
}

// TestNoLeakyFlacConstructors is a structural pin over the whole module.
//
// It exists because the consequence is invisible on the machine most of
// this is written on. On POSIX a leaked descriptor changes nothing an
// assertion can see — unlink succeeds against an open handle, and the fd
// dies with the test binary. On Windows the same handle blocks deletion
// outright, and it surfaces nowhere near the call: the failure lands in
// `t.TempDir`'s cleanup as "The process cannot access the file because it
// is being used by another process", attributed to whichever test wrote
// the fixture. That cost a full triage pass across 26 failures spanning
// three test files before the one-line fixture writer turned out to be
// the common cause.
//
// A grep would false-positive here: `extractors.go` mentions
// `flac.ParseFile` twice in comments describing the pre-#563 shape.
// Walking the AST looks only at call expressions, and resolves the
// import alias rather than assuming the local name is `flac`.
func TestNoLeakyFlacConstructors(t *testing.T) {
	root := moduleRoot(t)

	var found []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Skip VCS metadata, vendored trees, and the `_`-prefixed
			// scratch dirs the repo uses for throwaway helpers (both
			// are already invisible to `go ./...`).
			name := d.Name()
			if name == ".git" || name == "vendor" || strings.HasPrefix(name, "_") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}

		fset := token.NewFileSet()
		file, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			// A file that does not parse is not this test's problem —
			// the build will say so far more clearly.
			return nil //nolint:nilerr // deliberate: build reports parse errors
		}

		local, ok := localNameFor(file, mewkizFlacPkg)
		if !ok {
			return nil
		}

		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			rel = path
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			ident, ok := sel.X.(*ast.Ident)
			if !ok || ident.Name != local {
				return true
			}
			want, banned := leakyFlacConstructors[sel.Sel.Name]
			if !banned {
				return true
			}
			found = append(found, rel+":"+
				strconv.Itoa(fset.Position(call.Pos()).Line)+
				": "+local+"."+sel.Sel.Name+" — use "+want)
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}

	if len(found) > 0 {
		t.Errorf("mewkiz/flac path-taking constructors leak the file handle "+
			"(Stream.Close cannot close a *bufio.Reader), which blocks "+
			"deletion on Windows:\n\t%s", strings.Join(found, "\n\t"))
	}
}

// TestFixtureWritersLeaveNoOpenHandle is the behavioural half of the
// pin. TestNoLeakyFlacConstructors bans two known-leaky functions by
// name; this one asserts the property those functions violated, so a
// leak arriving by some other route (a stray `os.Create`, a future
// dependency with the same Close-closes-nothing shape) is caught too.
//
// On Windows an open handle blocks deletion, so "can we delete it right
// after writing it" is a direct read on whether anything still holds
// the file. On POSIX unlink succeeds regardless and this is vacuous —
// deliberately kept running anyway rather than guarded by GOOS, since a
// test that compiles and runs everywhere is one fewer thing that can
// rot unnoticed on the platform nobody develops on.
//
// The retry loop is the diagnosis, not flake-suppression: a handle WE
// hold is never released, so it can never start succeeding, while an
// antivirus scan-on-close window (the reason production writes go
// through `atomicwrite.RenameWithRetry`) clears in milliseconds. So a
// late success is reported as an external holder and does NOT fail —
// only never succeeding does. Defender was separately ruled out as the
// cause of the original 30 failures, with an exclusion verified applied
// to Go's actual `os.TempDir()`.
func TestFixtureWritersLeaveNoOpenHandle(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write func(t *testing.T, path string)
	}{
		{"flac", func(t *testing.T, p string) {
			writeMinimalFLAC(t, p, 44100, 16, map[string]string{"TITLE": "Handle"})
		}},
		{"dsf", func(t *testing.T, p string) {
			writeMinimalDSF(t, p, 2822400, map[string]string{"title": "Handle"})
		}},
		{"mp3", func(t *testing.T, p string) {
			writeMinimalMP3(t, p, map[string]string{"title": "Handle"})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "fixture."+tc.name)
			tc.write(t, path)

			// Budget is deliberately generous — 10s, not the ~500ms a
			// first draft used (Gemini on PR #629). A leak we hold is
			// never released, so waiting longer cannot make a real
			// failure pass; it only removes the chance that a loaded
			// Windows runner with Defender mid-scan is misread as one.
			// The cost is asymmetric: a slow success costs the wall
			// clock ONCE, on a path that normally returns on the first
			// attempt, while a false failure costs an investigation
			// into a bug that is not there.
			var lastErr error
			const attempts = 200
			for attempt := 1; attempt <= attempts; attempt++ {
				if lastErr = os.Remove(path); lastErr == nil {
					if attempt > 1 {
						t.Logf("removable only on attempt %d — an EXTERNAL "+
							"holder (antivirus / indexer) had it briefly; not "+
							"a handle of ours, which would never release",
							attempt)
					}
					return
				}
				time.Sleep(50 * time.Millisecond)
			}
			t.Fatalf("%s fixture still undeletable after %d attempts (~10s): %v\n"+
				"the writer leaked a handle — on Windows this surfaces later and "+
				"far away, as a t.TempDir RemoveAll cleanup failure attributed to "+
				"whichever test used the fixture", tc.name, attempts, lastErr)
		})
	}
}

// localNameFor returns the identifier this file binds the given import
// path to — the alias when one is written, otherwise the package's own
// name. Reports false when the file does not import it, or imports it
// blank (`_`, unusable) or dot-imported (no selector to match on).
func localNameFor(file *ast.File, importPath string) (string, bool) {
	for _, imp := range file.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil || p != importPath {
			continue
		}
		if imp.Name != nil {
			if imp.Name.Name == "_" || imp.Name.Name == "." {
				return "", false
			}
			return imp.Name.Name, true
		}
		// No alias: the mewkiz package declares `package flac`, which
		// matches its final path segment.
		return importBase(importPath), true
	}
	return "", false
}

// importBase is filepath.Base for import paths, which are
// slash-separated on every OS (so filepath.Base would be wrong on
// Windows, where the separator is a backslash).
func importBase(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// moduleRoot walks up from the test's working directory to the
// directory holding go.mod.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod found walking up from the test's working directory")
		}
		dir = parent
	}
}
