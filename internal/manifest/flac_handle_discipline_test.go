package manifest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/sweeptest"
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
	sweep, err := leakyFlacCalls(filepath.WalkDir, root)
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	// A walk that reads nothing reports nothing, and until these floors the
	// test had none. The tree held 410 non-test and 739 test files when they
	// were set, and one file importing the package: internal/manifest's
	// fixture writer, the kind of file this guard was written about. Files
	// that do not import it are parsed and never judged, so the count alone
	// cannot show that the walk reached a single call it guards.
	if sweep.nonTest < 100 || sweep.test < 100 {
		t.Fatalf("parsed %d non-test and %d test .go files under %s, want >=100 of each — "+
			"the walk is not seeing the tree", sweep.nonTest, sweep.test, root)
	}
	if sweep.importers == 0 {
		t.Fatalf("no file under %s imports %s, so no call was judged — the walk is not "+
			"reaching the fixture writer, or nothing imports the package any more and "+
			"this guard has nothing left to guard", root, mewkizFlacPkg)
	}

	if len(sweep.calls) > 0 {
		t.Errorf("mewkiz/flac path-taking constructors leak the file handle "+
			"(Stream.Close cannot close a *bufio.Reader), which blocks "+
			"deletion on Windows:\n\t%s", strings.Join(sweep.calls, "\n\t"))
	}
}

// flacSweep is what leakyFlacCalls found under a root: each call of a
// leakyFlacConstructors entry, as "file:line: call — use …", the non-test
// and test files it parsed, and how many of those import mewkizFlacPkg,
// which are the only files whose calls it can judge.
type flacSweep struct {
	calls         []string
	nonTest, test int
	importers     int
}

// leakyFlacCalls walks the Go files under root for calls of the
// leakyFlacConstructors entries. It walks with walk: filepath.WalkDir, or in
// a test a sweeptest.Removal's WalkDir, which stands in for another process
// removing a directory while the sweep is under way.
func leakyFlacCalls(walk func(root string, fn fs.WalkDirFunc) error, root string) (sweep flacSweep, err error) {
	err = walk(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return flacDirRule(root, path, d.Name())
		}
		if strings.HasSuffix(path, ".go") {
			sweep.judge(root, path)
		}
		return nil
	})
	return sweep, err
}

// flacDirRule is leakyFlacCalls' answer for a directory: SkipDir for one
// whose code is not this checkout's, nil to descend.
func flacDirRule(root, path, name string) error {
	// The rules below are for the directories under the root. A checkout's
	// own directory may be called anything, and one whose name began with
	// "_" skipped the whole tree.
	if path == root {
		return nil
	}
	// Skip VCS metadata, vendored trees, and the `_`-prefixed scratch dirs
	// the repo uses for throwaway helpers (both are already invisible to
	// `go ./...`). And another checkout inside this one
	// (sweeptest.IsOtherCheckout), such as Claude Code's worktrees of other
	// branches: none of it is this checkout's code, and a call in progress
	// there failed this checkout's run. And make test's TMPDIR, the root's
	// own .tmp (sweeptest.IsMakeTestTmpDir), where every package's tests keep
	// their temporary files while the suite runs: a leaky call there was
	// reported as this module's, and a directory there removed between its
	// listing and its own failed the walk (B567). A .tmp further down is the
	// tree's.
	if name == ".git" || name == "vendor" || strings.HasPrefix(name, "_") ||
		sweeptest.IsMakeTestTmpDir(root, path) || sweeptest.IsOtherCheckout(root, path) {
		return filepath.SkipDir
	}
	return nil
}

// judge parses the Go file at path, counts it, and records each call in it
// of a leakyFlacConstructors entry, named by its path relative to root.
func (s *flacSweep) judge(root, path string) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		// A file that does not parse is not this test's problem — the
		// build will say so far more clearly.
		return
	}
	if strings.HasSuffix(path, "_test.go") {
		s.test++
	} else {
		s.nonTest++
	}

	local, ok := localNameFor(file, mewkizFlacPkg)
	if !ok {
		return
	}
	s.importers++

	rel, err := filepath.Rel(root, path)
	if err != nil {
		rel = path
	}
	ast.Inspect(file, func(n ast.Node) bool {
		if name, ok := leakyFlacCall(n, local); ok {
			s.calls = append(s.calls, rel+":"+
				strconv.Itoa(fset.Position(n.Pos()).Line)+
				": "+local+"."+name+" — use "+leakyFlacConstructors[name])
		}
		return true
	})
}

// leakyFlacCall reports the constructor n calls when n is a call of a
// leakyFlacConstructors entry through local, the file's name for the
// package.
func leakyFlacCall(n ast.Node, local string) (string, bool) {
	call, ok := n.(*ast.CallExpr)
	if !ok {
		return "", false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	if ident, ok := sel.X.(*ast.Ident); !ok || ident.Name != local {
		return "", false
	}
	_, banned := leakyFlacConstructors[sel.Sel.Name]
	return sel.Sel.Name, banned
}

// TestFlacSweepSkipsOtherCheckouts runs the sweep over a tree shaped like the
// main checkout: a root that is a checkout itself, with a leaky call below it
// that must be found, and other checkouts inside it whose work in progress
// must not be. One is Claude Code's worktree of another branch; the other
// sits at a plain path and has no go.mod of its own. Before the sweep
// skipped them, each failed this checkout's run.
//
// The root's own name begins with "_", as a checkout's directory may. The
// walk tests the names of the directories below the root, never the root's,
// or it skips the whole tree and reports nothing.
func TestFlacSweepSkipsOtherCheckouts(t *testing.T) {
	const leaky = "package p\n\nimport \"github.com/mewkiz/flac\"\n\n" +
		"func probe(path string) { s, _ := flac.ParseFile(path); _ = s }\n"
	root := filepath.Join(t.TempDir(), "_checkout")
	for rel, body := range map[string]string{
		".git/HEAD":  "ref: refs/heads/main\n",
		"p/probe.go": leaky,

		".claude/worktrees/old/.git":       "gitdir: /elsewhere/.git/worktrees/old\n",
		".claude/worktrees/old/go.mod":     "module example\n",
		".claude/worktrees/old/p/probe.go": leaky,

		"worktrees/plain/.git":       "gitdir: /elsewhere/.git/worktrees/plain\n",
		"worktrees/plain/p/probe.go": leaky,
	} {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	sweep, err := leakyFlacCalls(filepath.WalkDir, root)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join("p", "probe.go") + ":5: flac.ParseFile — use " + leakyFlacConstructors["ParseFile"]}
	if !slices.Equal(sweep.calls, want) || sweep.nonTest != 1 || sweep.test != 0 || sweep.importers != 1 {
		t.Errorf("found %q after parsing %d non-test and %d test files (%d importing the package), "+
			"want %q after parsing 1 non-test file — the sweep read another checkout, or stopped "+
			"reading this one", sweep.calls, sweep.nonTest, sweep.test, sweep.importers, want)
	}
}

// leakyFlacProbe is a production file that calls a leakyFlacConstructors
// entry on its fifth line.
const leakyFlacProbe = "package p\n\nimport \"github.com/mewkiz/flac\"\n\n" +
	"func probe(path string) { s, _ := flac.ParseFile(path); _ = s }\n"

// writeFlacSweepTree writes each file in files, named by its slash path
// below root, creating the directories it needs.
func writeFlacSweepTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// TestFlacSweepSkipsMakeTestsTMPDIRAtTheRoot plants, one per row, what make
// test leaves in the directory it gives every package's tests as TMPDIR when
// the caller sets none (sweeptest.MakeTestTmpDir, at the root), and the same
// call where the sweep must still find it.
//
// The suite runs every package's tests at once, so while the whole-tree pin
// walked the checkout, another package's temporary Go files sat in that
// directory: a leaky call there was reported as this module's, and a file
// there was counted as one of its own (B567). The row in that directory
// leaves the sweep's answer as it is without it. The rows in a directory of
// the same name below the root, in one whose name begins with it, and beside
// the Go program .github holds are found as before: the sweep skips that
// directory at the root alone, as the build-tag census does (B740).
func TestFlacSweepSkipsMakeTestsTMPDIRAtTheRoot(t *testing.T) {
	tmp := path.Join(sweeptest.MakeTestTmpDir, "TestSomething123", "001")
	for _, row := range []struct {
		name  string
		files map[string]string // slash paths below the root
		found string            // the file whose call the sweep must report, or "" for none
	}{
		{"a leaky call and a test file in make test's TMPDIR", map[string]string{
			path.Join(tmp, "p", "probe.go"):      leakyFlacProbe,
			path.Join(tmp, "p", "probe_test.go"): "package p\n",
		}, ""},
		{"a leaky call in a directory of that name below the root", map[string]string{
			path.Join("internal", "x", sweeptest.MakeTestTmpDir, "probe.go"): leakyFlacProbe,
		}, path.Join("internal", "x", sweeptest.MakeTestTmpDir, "probe.go")},
		{"a leaky call in a directory whose name begins with it", map[string]string{
			path.Join(sweeptest.MakeTestTmpDir+"foo", "probe.go"): leakyFlacProbe,
		}, path.Join(sweeptest.MakeTestTmpDir+"foo", "probe.go")},
		{"a leaky call beside the Go program .github holds", map[string]string{
			path.Join(".github", "scripts", "tool", "probe.go"): leakyFlacProbe,
		}, path.Join(".github", "scripts", "tool", "probe.go")},
	} {
		t.Run(row.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "checkout")
			writeFlacSweepTree(t, root, map[string]string{".git/HEAD": "ref: refs/heads/main\n", "p/probe.go": leakyFlacProbe})
			want, err := leakyFlacCalls(filepath.WalkDir, root)
			if err != nil {
				t.Fatal(err)
			}
			writeFlacSweepTree(t, root, row.files)
			if row.found != "" {
				want.calls = append(want.calls, filepath.FromSlash(row.found)+":5: flac.ParseFile — use "+leakyFlacConstructors["ParseFile"])
				want.nonTest++
				want.importers++
			}
			sweep, err := leakyFlacCalls(filepath.WalkDir, root)
			if err != nil {
				t.Fatal(err)
			}
			slices.Sort(want.calls)
			slices.Sort(sweep.calls)
			if !slices.Equal(sweep.calls, want.calls) || sweep.nonTest != want.nonTest || sweep.test != want.test || sweep.importers != want.importers {
				t.Errorf("found %q after parsing %d non-test and %d test files (%d importing the package), "+
					"want %q after %d, %d and %d", sweep.calls, sweep.nonTest, sweep.test, sweep.importers,
					want.calls, want.nonTest, want.test, want.importers)
			}
		})
	}
}

// TestFlacSweepFinishesWhileMakeTestsTMPDIRChanges: a directory in make
// test's TMPDIR that another process removes while the sweep is under way,
// as go test removes a test's temporary directory and the go command a work
// directory, is one the sweep never lists, so it finishes with the answer
// the tree gives without it. Reading that directory, the sweep failed on the
// directory it had just listed (B567). A sweeptest.Removal stands in for the
// other process at that moment of the walk, handed to the sweep as its walk.
//
// The last row removes the TMPDIR itself, which the walk reaches as an entry
// of the root's listing: it is gone before the sweep decides anything about
// it, so the sweep must skip it by its name alone, before it lists it or asks
// anything of it. A sweep that listed it, or that failed on what a stat of it
// answers, fails there.
func TestFlacSweepFinishesWhileMakeTestsTMPDIRChanges(t *testing.T) {
	for _, gone := range []struct {
		name    string
		dir     string // a slash path below make test's TMPDIR, "" for the TMPDIR itself
		reached bool   // whether the walk reaches it: the root's listing names the TMPDIR alone
	}{
		{"a test's temporary directory", "TestSomething123", false},
		{"the go command's work directory", "go-build123/b001", false},
		{"make test's TMPDIR itself", "", true},
	} {
		t.Run(gone.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "checkout")
			writeFlacSweepTree(t, root, map[string]string{".git/HEAD": "ref: refs/heads/main\n", "p/probe.go": leakyFlacProbe})
			want, err := leakyFlacCalls(filepath.WalkDir, root)
			if err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(root, sweeptest.MakeTestTmpDir, filepath.FromSlash(gone.dir))
			writeFlacSweepTree(t, dir, map[string]string{"probe.go": leakyFlacProbe})
			removal := &sweeptest.Removal{Dir: dir}
			sweep, err := leakyFlacCalls(removal.WalkDir, root)
			if !removal.Walked {
				t.Fatal("the sweep did not walk through the stand-in, so this case proves nothing")
			}
			if err != nil {
				t.Fatalf("the sweep failed: %v", err)
			}
			if removal.Reached != gone.reached {
				if gone.reached {
					t.Fatalf("the sweep never reached %s, which the root's listing names, so this case proves nothing", dir)
				}
				t.Errorf("the sweep reached %s, in make test's TMPDIR", dir)
			}
			if !slices.Equal(sweep.calls, want.calls) || sweep.nonTest != want.nonTest || sweep.test != want.test || sweep.importers != want.importers {
				t.Errorf("found %q after parsing %d non-test and %d test files (%d importing the package), "+
					"want %q after %d, %d and %d", sweep.calls, sweep.nonTest, sweep.test, sweep.importers,
					want.calls, want.nonTest, want.test, want.importers)
			}
		})
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
