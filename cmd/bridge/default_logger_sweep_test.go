package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// defaultLoggerSetter is a function whose call points slog.Default at
// another logger for the rest of the process, which a test file must not
// call by hand (TestNoTestSetsTheDefaultLoggerByHand).
type defaultLoggerSetter struct {
	importPath, name string
	// ownTests is the directory, a slash path relative to the module root,
	// whose test files may call it by hand: the package that tests it. That
	// directory only, not the ones below it.
	ownTests string
	// leaves says what a test that calls it by hand leaves behind.
	leaves string
}

// defaultLoggerSetters are the two ways into slog.SetDefault a test file
// can name. loggingtest's own tests call slog.SetDefault by hand because
// loggingtest.SetDefault is their subject: they build the prior default it
// has to put back. internal/logging's tests call Init, their subject,
// unqualified, which this scan does not read; there resetOnce puts back
// what Init changes, and that package's TestMain checks it did.
var defaultLoggerSetters = []defaultLoggerSetter{
	{
		importPath: "log/slog", name: "SetDefault", ownTests: "internal/logging/loggingtest",
		leaves: "putting back the previous default does not put back the log package's output and flags, " +
			"so every later line in the test binary goes into this test's handler",
	},
	{
		importPath: "github.com/acoseac/1-bit-bridge/internal/logging", name: "Init", ownTests: "internal/logging",
		leaves: "it points slog.Default at a handler on w for the rest of the test binary, with nothing to put " +
			"it back, and only the first call in a process does anything",
	},
}

// TestNoTestSetsTheDefaultLoggerByHand refuses every test file that points
// slog.Default at a logger of its own by hand: with slog.SetDefault, or
// with logging.Init, which calls it. A test does that through
// loggingtest.SetDefault(t, l), which puts back the previous default and
// the log package's output and flags when the test ends.
//
// The hand-rolled form, `prev := slog.Default(); slog.SetDefault(l);
// t.Cleanup(func() { slog.SetDefault(prev) })`, puts back the default
// alone. slog.SetDefault also points the log package's output at l's
// handler and zeroes its flags, and putting back slog's own default (the
// one every test binary starts with) undoes neither, while that handler
// writes THROUGH the log package. When this scan was written it found 29
// references in 12 files, and after each file's capturing test, a later
// test's slog.Info and log.Print both went into the finished test's
// handler: 0 of 2 lines reached the output, where 2 of 2 did with the
// later test run alone (go1.26.6, 2026-09-28).
//
// It reads test files only, since every capture in the tree is in one, and
// production code has one call, logging.Init's. A reference counts whether
// or not it is called (`defer slog.SetDefault(prev)`, a method value), and
// through whatever name the file imports the package by. A file that
// dot-imports one is reported too: its calls carry no package name to read.
// On a clean tree this finds nothing, so TestDefaultLoggerSweepOnAFixture
// runs the same scan over a tree whose findings are known.
func TestNoTestSetsTheDefaultLoggerByHand(t *testing.T) {
	scanDefaultLoggerSetters(t, repoRootForCitations(t), true)
}

// loggerSetterFinding is one reference the scan refuses: its file:line, the
// reference as written, and why it is refused.
type loggerSetterFinding struct {
	pos, ref, msg string
}

// loggerSweep is what findDefaultLoggerSetters found under a root, and how
// much it read: the test files it parsed, and how many of those import a
// setter's package by a name, the only files whose calls it can see.
type loggerSweep struct {
	findings             []loggerSetterFinding
	testFiles, importers int
}

// scanDefaultLoggerSetters is TestNoTestSetsTheDefaultLoggerByHand's scan
// of the tree under root, reported through r (docblock_subject_test.go's
// reporter): an Errorf per finding. wholeTree holds the scan to the floors
// that prove it reached this repo's whole tree; a fixture has none of those
// to meet.
func scanDefaultLoggerSetters(r docScanReporter, root string, wholeTree bool) loggerSweep {
	sweep, err := findDefaultLoggerSetters(root)
	if err != nil {
		r.Fatalf("walking %s: %v", root, err)
		return sweep
	}
	// A walk that reads nothing reports nothing. The tree held 823 test
	// files when these floors were set, 24 of them importing log/slog, the
	// package whose calls this scan exists for.
	if wholeTree && (sweep.testFiles < 100 || sweep.importers < 10) {
		r.Fatalf("parsed %d test files, %d of them importing a setter's package by name, want >=100 and >=10 — "+
			"the scan is not seeing the tree", sweep.testFiles, sweep.importers)
		return sweep
	}
	for _, f := range sweep.findings {
		r.Errorf("%s: %s", f.pos, f.msg)
	}
	return sweep
}

// findDefaultLoggerSetters parses every test file under root that the go
// tool reads (moduleDirRule, goToolIgnores), and records each reference to
// a defaultLoggerSetters entry outside that entry's own tests.
func findDefaultLoggerSetters(root string) (sweep loggerSweep, err error) {
	err = filepath.WalkDir(root, func(file string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return moduleDirRule(root, file, d.Name())
		}
		if name := d.Name(); !strings.HasSuffix(name, "_test.go") || goToolIgnores(name) {
			return nil
		}
		rel, err := filepath.Rel(root, file)
		if err != nil {
			return err
		}
		return sweep.judge(file, filepath.ToSlash(rel))
	})
	return sweep, err
}

// judge parses the test file at file, named rel below the root, and
// records its references to the setters its directory may not call. A file
// that does not parse fails the walk: it is no evidence of anything, and
// its package's build fails on it too.
func (s *loggerSweep) judge(file, rel string) error {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, parser.SkipObjectResolution)
	if err != nil {
		return err
	}
	s.testFiles++
	named := false
	for _, setter := range defaultLoggerSetters {
		for _, imp := range f.Imports {
			if p, err := strconv.Unquote(imp.Path.Value); err == nil && p == setter.importPath {
				named = s.judgeImport(f, fset, imp, rel, setter) || named
			}
		}
	}
	if named {
		s.importers++
	}
	return nil
}

// judgeImport records the references f makes to setter through imp, one of
// its imports of setter's package, unless rel's directory is setter's own
// tests. It reports whether imp names the package, which a blank or a dot
// import does not.
func (s *loggerSweep) judgeImport(f *ast.File, fset *token.FileSet, imp *ast.ImportSpec, rel string, setter defaultLoggerSetter) bool {
	name := importName(imp)
	own := path.Dir(rel) == setter.ownTests
	switch {
	case name == "_":
		return false
	case name == ".":
		if !own {
			s.findings = append(s.findings, loggerSetterFinding{
				pos: fmt.Sprintf("%s:%d", rel, fset.Position(imp.Pos()).Line),
				ref: "dot import of " + setter.importPath,
				msg: fmt.Sprintf("dot-imports %s, so this scan cannot see a call of its %s, which carries no "+
					"package name. Import it by name", setter.importPath, setter.name),
			})
		}
		return false
	case !own:
		s.referencesIn(f, fset, rel, name, setter)
	}
	return true
}

// referencesIn records every selector in f that names setter through name,
// the file's name for setter's package, called or not.
func (s *loggerSweep) referencesIn(f *ast.File, fset *token.FileSet, rel, name string, setter defaultLoggerSetter) {
	ast.Inspect(f, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != setter.name {
			return true
		}
		if id, ok := sel.X.(*ast.Ident); ok && id.Name == name {
			ref := name + "." + setter.name
			s.findings = append(s.findings, loggerSetterFinding{
				pos: fmt.Sprintf("%s:%d", rel, fset.Position(sel.Pos()).Line),
				ref: ref,
				msg: fmt.Sprintf("%s in a test: %s. Use loggingtest.SetDefault(t, l), which puts back the "+
					"default, the log package's output and its flags", ref, setter.leaves),
			})
		}
		return true
	})
}

// TestDefaultLoggerSweepOnAFixture runs scanDefaultLoggerSetters over a
// tree whose findings are known, written with LF and with CRLF line
// endings. On this repo's own tree a clean scan reports nothing, so a
// change that lost a shape, an exemption's boundary or a skip rule would
// pass there unseen. Each quiet file in loggerSweepFixture is one the scan
// must leave alone, for the reason its comment gives.
func TestDefaultLoggerSweepOnAFixture(t *testing.T) {
	want := []string{
		"internal/logging/ext_test.go:12 slog.SetDefault",
		"internal/logging/loggingtest/deeper/x_test.go:5 slog.SetDefault",
		"pkg/alias_test.go:6 stdslog.SetDefault",
		"pkg/alias_test.go:7 stdslog.SetDefault",
		"pkg/by_hand_test.go:11 slog.SetDefault",
		"pkg/by_hand_test.go:12 slog.SetDefault",
		"pkg/dot_test.go:3 dot import of log/slog",
		"pkg/init_test.go:9 logging.Init",
	}
	for _, crlf := range []bool{false, true} {
		t.Run(fmt.Sprintf("crlf=%v", crlf), func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "clone")
			writeFixtureTree(t, root, loggerSweepFixture, nil, crlf)
			rec := &docScanRecorder{t: t}
			sweep := scanDefaultLoggerSetters(rec, root, false)
			var got []string
			for _, f := range sweep.findings {
				got = append(got, f.pos+" "+f.ref)
			}
			sort.Strings(got)
			if strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Errorf("findings:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
			}
			if len(rec.errors) != len(want) {
				t.Errorf("reported %d errors, want %d:\n  %s", len(rec.errors), len(want), strings.Join(rec.errors, "\n  "))
			}
			// Nine test files are read, seven of them importing a setter's
			// package by name: not the blank or the dot import, nor any
			// file the skip rules leave out.
			if sweep.testFiles != 9 || sweep.importers != 7 {
				t.Errorf("read %d test files, %d of them importing a setter's package by name, want 9 and 7",
					sweep.testFiles, sweep.importers)
			}
		})
	}
}

// loggerSweepByHand is a test file that calls slog.SetDefault by hand, at
// line 5.
const loggerSweepByHand = "package x\n\nimport \"log/slog\"\n\nfunc set(l *slog.Logger) { slog.SetDefault(l) }\n"

// loggerSweepFixture is the tree TestDefaultLoggerSweepOnAFixture scans:
// slash path -> source.
var loggerSweepFixture = map[string]string{
	// The hand-rolled capture: the install and the restore are both
	// reported.
	"pkg/by_hand_test.go": `package pkg

import (
	"io"
	"log/slog"
	"testing"
)

func TestCapture(t *testing.T) {
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
}
`,
	// Under another name, deferred, and as a method value.
	"pkg/alias_test.go": `package pkg

import stdslog "log/slog"

func restore(prev *stdslog.Logger) {
	defer stdslog.SetDefault(prev)
	set := stdslog.SetDefault
	_ = set
}
`,
	// A dot import hides the package name the scan reads.
	"pkg/dot_test.go": "package pkg\n\nimport . \"log/slog\"\n\nfunc quiet() { SetDefault(Default()) }\n",
	// logging.Init is slog.SetDefault behind a sync.Once.
	"pkg/init_test.go": `package pkg

import (
	"io"

	"github.com/acoseac/1-bit-bridge/internal/logging"
)

func quietLogs() { logging.Init(io.Discard) }
`,
	// Left alone: the way a test does it, a comment and a string naming the
	// call, another package's SetDefault.
	"pkg/quiet_test.go": `package pkg

import (
	"io"
	"log/slog"
	"testing"

	"example.com/config"
	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// slog.SetDefault(prev) alone is what the scan refuses.
func TestQuiet(t *testing.T) {
	loggingtest.SetDefault(t, slog.New(slog.NewTextHandler(io.Discard, nil)))
	config.SetDefault()
	t.Log("slog.SetDefault")
}
`,
	// A blank import names nothing to call.
	"pkg/blank_test.go": "package pkg\n\nimport _ \"log/slog\"\n",
	// Production code is not this scan's to judge.
	"pkg/prod.go": loggerSweepByHand,
	// The package whose SetDefault puts it all back tests it by hand, and
	// only that directory may, not one below it.
	"internal/logging/loggingtest/own_test.go":      loggerSweepByHand,
	"internal/logging/loggingtest/deeper/x_test.go": loggerSweepByHand,
	// internal/logging's tests may call its own Init, and nothing else here.
	"internal/logging/ext_test.go": `package logging_test

import (
	"io"
	"log/slog"

	"github.com/acoseac/1-bit-bridge/internal/logging"
)

func setup(l *slog.Logger) {
	logging.Init(io.Discard)
	slog.SetDefault(l)
}
`,
	// The root is a checkout, as every real root is, and is read. Nothing
	// the go tool ignores is read, nor another checkout. The lock file is
	// not Go, so opening it would fail the scan.
	".git/HEAD":                   "ref: refs/heads/main\n",
	"pkg/.#by_hand_test.go":       "user@host.4242:1700000000",
	"pkg/_draft_test.go":          loggerSweepByHand,
	"_scratch/x_test.go":          loggerSweepByHand,
	"pkg/testdata/x_test.go":      loggerSweepByHand,
	"worktrees/mid/.git":          "gitdir: /elsewhere/.git/worktrees/mid\n",
	"worktrees/mid/pkg/x_test.go": loggerSweepByHand,
}
