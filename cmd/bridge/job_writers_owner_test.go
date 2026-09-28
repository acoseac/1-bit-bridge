package main

import (
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
)

// jobWriterSources are the packages, and the one file here, whose writes
// the job and database CLIs make (`scan`, `upscale` / `optimize` /
// `render` / `analyze`, `artwork`, `backup` / `restore`, `manifest`, a
// `library` change, `variants move`). Run as root over a service install,
// every entry they create must keep the install's owner, or the service
// cannot write it afterwards.
var jobWriterSources = []string{
	"internal/atomicwrite",
	"internal/manifest",
	"internal/transcode",
	"internal/analyze",
	"internal/backup",
	"cmd/bridge/variants.go",
}

// createsWithNoOwnerStep are the os calls that create an entry and leave no
// way to give it an owner, with what to call instead.
var createsWithNoOwnerStep = map[string]string{
	"MkdirAll":  "fsutil.MkdirAll",
	"Mkdir":     "fsutil.Mkdir",
	"MkdirTemp": "fsutil.Mkdir on a name of its own",
	"WriteFile": "os.OpenFile and fsutil.KeepOwner on the open file",
	"Create":    "os.OpenFile and fsutil.KeepOwner on the open file",
	"Link":      "a staged copy given away with fsutil.KeepOwner",
	"Symlink":   "nothing: no job writer makes links",
}

// ownerSteps are the fsutil calls that give what a writer creates the
// install's owner.
var ownerSteps = map[string]bool{
	"KeepOwner": true, "MkdirAll": true, "Mkdir": true,
	"MkdirAllShared": true, "MkdirAllLike": true, "Precreate": true,
}

// TestJobWritersKeepTheInstallOwner is the structural half of
// TestJobCLIsRunAsRootKeepTheInstallOwner, which needs root and so never
// runs in CI: in the job writers, no call creates an entry without an owner
// step. A directory is made through fsutil (MkdirAll and its kin), and a
// file this process creates (os.CreateTemp, or os.OpenFile with O_CREATE)
// sits in a function that calls fsutil.KeepOwner. What a child process
// creates (sox's output) is outside what a sweep can see: fsutil.Precreate
// covers it, and the root test pins it.
func TestJobWritersKeepTheInstallOwner(t *testing.T) {
	root := repoRootForCitations(t)
	var files []string
	for _, src := range jobWriterSources {
		p := filepath.Join(root, filepath.FromSlash(src))
		if strings.HasSuffix(src, ".go") {
			files = append(files, p)
			continue
		}
		entries, err := os.ReadDir(p)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			name := e.Name()
			// A name beginning with "." or "_" is not the package's source
			// (the go tool ignores it), and emacs's `.#name.go` lock is one.
			if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") ||
				strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
				continue
			}
			files = append(files, filepath.Join(p, name))
		}
	}
	steps := 0
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		rel, _ := filepath.Rel(root, f)
		problems, n := scanJobWriter(t, filepath.ToSlash(rel), src)
		steps += n
		for _, p := range problems {
			t.Errorf("%s, so a job CLI run as root leaves an entry the service cannot write", p)
		}
	}
	// The floor: a sweep that reads nothing, or finds no owner step in the
	// writers it exists for, passes whatever the code does.
	if len(files) < 20 || steps < 15 {
		t.Fatalf("read %d files and found %d owner steps; the sweep looks at nothing it was written for", len(files), steps)
	}
}

// scanJobWriter reports each call in src that creates an entry with no
// owner step, and counts the owner steps it saw.
func scanJobWriter(t *testing.T, name string, src []byte) (problems []string, steps int) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, name, src, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	osName, fsutilName := "", ""
	for _, imp := range f.Imports {
		path, _ := strconv.Unquote(imp.Path.Value)
		local := filepath.Base(path)
		if imp.Name != nil {
			local = imp.Name.Name
		}
		switch path {
		case "os":
			osName = local
		case "github.com/acoseac/1-bit-bridge/internal/fsutil":
			fsutilName = local
		}
	}
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		type creation struct {
			pos  token.Pos
			call string
		}
		var needKeep []creation
		keeps := false
		ast.Inspect(fn.Body, func(n ast.Node) bool {
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
			switch {
			case fsutilName != "" && pkg.Name == fsutilName && ownerSteps[sel.Sel.Name]:
				steps++
				if sel.Sel.Name == "KeepOwner" {
					keeps = true
				}
			case osName != "" && pkg.Name == osName:
				if instead, bad := createsWithNoOwnerStep[sel.Sel.Name]; bad {
					problems = append(problems, fmt.Sprintf("%s: %s calls os.%s, which creates an entry with no owner step (use %s)",
						fset.Position(call.Pos()), fn.Name.Name, sel.Sel.Name, instead))
				}
				if sel.Sel.Name == "CreateTemp" ||
					(sel.Sel.Name == "OpenFile" && len(call.Args) >= 2 && mentionsOCreate(call.Args[1])) {
					needKeep = append(needKeep, creation{pos: call.Pos(), call: "os." + sel.Sel.Name})
				}
			}
			return true
		})
		if keeps {
			continue
		}
		for _, c := range needKeep {
			problems = append(problems, fmt.Sprintf("%s: %s creates a file with %s and never gives it an owner (fsutil.KeepOwner)",
				fset.Position(c.pos), fn.Name.Name, c.call))
		}
	}
	sort.Strings(problems)
	return problems, steps
}

// mentionsOCreate reports whether an os.OpenFile flag expression names
// O_CREATE.
func mentionsOCreate(flag ast.Expr) bool {
	found := false
	ast.Inspect(flag, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "O_CREATE" {
			found = true
		}
		if id, ok := n.(*ast.Ident); ok && id.Name == "O_CREATE" {
			found = true
		}
		return !found
	})
	return found
}

// TestJobWriterSweepReportsEveryShape runs the sweep over synthetic source.
// On a clean tree it reports nothing, so the tree alone cannot show that it
// still reports anything; this shows each shape it exists to catch, and
// each it must let through.
func TestJobWriterSweepReportsEveryShape(t *testing.T) {
	const src = `package x

import (
	stdos "os"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

func mkdirAll(p string) { _ = stdos.MkdirAll(p, 0o755) }
func mkdir(p string)    { _ = stdos.Mkdir(p, 0o755) }
func mkdirTemp()        { _, _ = stdos.MkdirTemp("", "x") }
func writeFile(p string) { _ = stdos.WriteFile(p, nil, 0o600) }
func create(p string)   { _, _ = stdos.Create(p) }

func stagedAndGivenAway(p string) {
	f, _ := stdos.CreateTemp("", "x")
	_ = fsutil.KeepOwner(f, p)
}

func stagedAndLeftRoots(p string) {
	f, _ := stdos.CreateTemp("", "x")
	_ = f
}

func openCreateLeftRoots(p string) {
	f, _ := stdos.OpenFile(p, stdos.O_WRONLY|stdos.O_CREATE|stdos.O_TRUNC, 0o600)
	_ = f
}

func openCreateGivenAway(p string) {
	f, _ := stdos.OpenFile(p, stdos.O_WRONLY|stdos.O_CREATE, 0o600)
	_ = fsutil.KeepOwner(f, p)
}

func openExisting(p string) {
	f, _ := stdos.OpenFile(p, stdos.O_RDWR, 0)
	_ = f
}

func throughFsutil(p string) {
	_ = fsutil.MkdirAll(p, 0o755)
	_ = fsutil.MkdirAllShared(p, 0o700, p)
	_ = fsutil.MkdirAllLike(p, 0o755, p)
	_ = fsutil.Mkdir(p, 0o700)
	_ = fsutil.Precreate(p, 0o644, p)
}
`
	problems, steps := scanJobWriter(t, "x.go", []byte(src))
	wantIn := map[string]bool{
		"mkdirAll": false, "mkdir": false, "mkdirTemp": false, "writeFile": false, "create": false,
		"stagedAndLeftRoots": false, "openCreateLeftRoots": false,
	}
	for _, p := range problems {
		matched := false
		for fn := range wantIn {
			if strings.Contains(p, ": "+fn+" ") {
				wantIn[fn] = true
				matched = true
			}
		}
		if !matched {
			t.Errorf("reported a shape it must let through: %s", p)
		}
	}
	for fn, seen := range wantIn {
		if !seen {
			t.Errorf("did not report %s", fn)
		}
	}
	if len(problems) != len(wantIn) {
		t.Errorf("reported %d problems, want one per bad shape (%d):\n%s", len(problems), len(wantIn), strings.Join(problems, "\n"))
	}
	if steps != 7 {
		t.Errorf("counted %d owner steps, want 7 (two KeepOwner, five in throughFsutil)", steps)
	}
}
