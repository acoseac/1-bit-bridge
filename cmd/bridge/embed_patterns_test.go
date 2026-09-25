package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestEveryEmbedPatternRefusesALeadingDot plants the names an editor and the
// OS leave beside a file, in every directory of every package that uses
// //go:embed, and requires the go command to embed exactly what it embeds
// without them.
//
// A glob's * matches a leading dot (`go doc embed`: "image/*" embeds
// "image/.tempfile"). Emacs locks a file it is editing with `.#<name>` beside
// it: a DANGLING symlink where it can, and a REGULAR file holding
// `user@host.pid:boot` where it cannot, which is always on Windows (emacs's
// filelock.c). Under `static/*`, `templates/*.html` and `*.tmpl` the first
// shape failed the BUILD of internal/admin, internal/packaging and
// cmd/bridge ("cannot embed irregular file static/.#app.js"). The second was
// EMBEDDED, and the console served its user@host at /static/.%23app.js. A
// Finder .DS_Store at the top of static/ shipped the same way.
//
// The plants go in through `go list -overlay`, so the tree is never written:
// a lock on disk would break the build of every package importing the one it
// sits in. An overlay file is always regular, and the regular shape answers
// for both. The go command asks a file's type only after a glob has matched
// its name. So a pattern that refuses the name never reaches the check that
// fails a build on a symlink, and a pattern that matches it embeds the
// regular file, where this test sees it. A directory walk never fails on a
// symlink at all: it skips irregular files, and skips names beginning with
// "." unless the pattern says all:. A regular dot file under all: is
// embedded, so this catches that form too.
//
// `-test`, because plain `go list` resolves a test file's patterns but
// neither the files they match nor their errors (measured when this was
// written).
func TestEveryEmbedPatternRefusesALeadingDot(t *testing.T) {
	root := goModuleRoot(t)

	var dirs, paths []string
	patterns := map[string][]string{}
	names := map[string]string{}
	for _, p := range goListPackages(t, root, "-json=ImportPath,Name,Dir,EmbedPatterns,TestEmbedPatterns,XTestEmbedPatterns", "./...") {
		all := slices.Concat(p.EmbedPatterns, p.TestEmbedPatterns, p.XTestEmbedPatterns)
		if len(all) == 0 {
			continue
		}
		dirs = append(dirs, p.Dir)
		paths = append(paths, p.ImportPath)
		patterns[p.ImportPath] = all
		names[p.Dir] = p.Name
	}
	if len(paths) == 0 {
		t.Fatal("go list found no package using //go:embed, so this test checks nothing")
	}

	// One backing file for every plant, holding what emacs writes into the
	// Windows-shape lock. Its content is never read: only the name matters.
	tmp := t.TempDir()
	backing := filepath.Join(tmp, "lock")
	if err := os.WriteFile(backing, []byte("someone@host.1234:1695000000"), 0o600); err != nil {
		t.Fatal(err)
	}
	plants := map[string]string{}
	for i, dir := range dirs {
		if err := plantEditorDetritus(dir, backing, plants); err != nil {
			t.Fatalf("plant beside %s: %v", dir, err)
		}
		// And one ordinary source file, which must show up in the package's
		// GoFiles: proof the go command read the overlay for this directory.
		// Without it, an overlay the go command ignored (a path it spells
		// differently, say) would leave both listings equal and this test
		// green over nothing.
		src := filepath.Join(tmp, fmt.Sprintf("seen%d.go", i))
		if err := os.WriteFile(src, []byte("package "+names[dir]+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		plants[filepath.Join(dir, overlaySeenFile)] = src
	}
	overlay := filepath.Join(t.TempDir(), "overlay.json")
	replace, err := json.Marshal(map[string]map[string]string{"Replace": plants})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(overlay, replace, 0o600); err != nil {
		t.Fatal(err)
	}

	const fields = "-json=ImportPath,Dir,ForTest,GoFiles,EmbedFiles,TestEmbedFiles,XTestEmbedFiles,Error"
	before := goListPackages(t, root, append([]string{"-test", fields}, paths...)...)
	after := map[string]goListPackage{}
	for _, p := range goListPackages(t, root, append([]string{"-test", "-overlay", overlay, fields}, paths...)...) {
		after[p.ImportPath] = p
	}

	// A test variant embeds its package's files as well as its own, so a file
	// is reported once, against the first listing that shows it.
	reported := map[string]bool{}
	firstReport := func(base string, files []string) []string {
		var out []string
		for _, f := range files {
			if k := base + "\x00" + f; !reported[k] {
				reported[k] = true
				out = append(out, f)
			}
		}
		return out
	}

	embedded := 0
	for _, b := range before {
		// The package whose patterns these are: a test variant names it in
		// ForTest, and the generated test main is "<path>.test".
		base := b.ForTest
		if base == "" {
			base = strings.TrimSuffix(b.ImportPath, ".test")
		}
		if b.Error != nil {
			t.Errorf("%s does not build even without a plant: %s", b.ImportPath, b.Error.Err)
			continue
		}
		// The package itself, as opposed to a test variant of it or its
		// generated test main.
		isPackage := slices.Contains(paths, b.ImportPath)
		// Every embedded file must have its lock planted beside it, or the
		// comparison below says nothing about the directory it sits in.
		if isPackage {
			for _, f := range slices.Concat(b.EmbedFiles, b.TestEmbedFiles, b.XTestEmbedFiles) {
				embedded++
				lock := filepath.Join(b.Dir, filepath.FromSlash(path.Dir(f)), ".#"+path.Base(f))
				if _, ok := plants[lock]; !ok {
					t.Errorf("%s embeds %s but nothing was planted beside it (%s), so a pattern "+
						"reaching that directory goes unchecked", b.ImportPath, f, lock)
				}
			}
		}
		a, ok := after[b.ImportPath]
		if !ok {
			t.Errorf("%s: missing from the planted listing", b.ImportPath)
			continue
		}
		if isPackage && !slices.Contains(a.GoFiles, overlaySeenFile) {
			t.Errorf("%s: the go command did not read the overlay in %s (no %s among its "+
				"GoFiles), so nothing planted there was checked", b.ImportPath, b.Dir, overlaySeenFile)
		}
		if a.Error != nil {
			// Not the lock's build failure: every plant here is a regular
			// file, which embed accepts. Report what go list said.
			t.Errorf("%s: with the plants in place, go list reports %s (patterns: %s)",
				b.ImportPath, a.Error.Err, strings.Join(patterns[base], " "))
			continue
		}
		for _, l := range []struct {
			field         string
			before, after []string
		}{
			{"EmbedFiles", b.EmbedFiles, a.EmbedFiles},
			{"TestEmbedFiles", b.TestEmbedFiles, a.TestEmbedFiles},
			{"XTestEmbedFiles", b.XTestEmbedFiles, a.XTestEmbedFiles},
		} {
			grew := slices.DeleteFunc(without(l.after, l.before), func(f string) bool {
				return path.Base(f) == overlaySeenFile // the probe's own instrument
			})
			if extra := firstReport(base, grew); len(extra) > 0 {
				t.Errorf("%s %s: with an editor's lock beside every file and a .DS_Store in every "+
					"directory, it also embeds %s. On Windows emacs's lock is a regular file, so "+
					"it ships; on macOS and Linux it is a dangling symlink, and the build fails. "+
					"Start every glob element with [^.], never * (patterns: %s)",
					b.ImportPath, l.field, strings.Join(extra, " "), strings.Join(patterns[base], " "))
			}
			if missing := without(l.before, l.after); len(missing) > 0 {
				t.Errorf("%s %s: the plants REMOVED %s from the embedded set",
					b.ImportPath, l.field, strings.Join(missing, " "))
			}
		}
	}
	if embedded == 0 {
		t.Fatal("no embedded file was listed, so nothing was compared")
	}
	t.Logf("%d packages, %d embedded files, %d plants", len(paths), embedded, len(plants))
}

// overlaySeenFile is the source file the probe adds to each package through
// the overlay, to learn whether the go command read it there.
const overlaySeenFile = "zz_overlay_seen.go"

// goListPackage is the subset of `go list -json` this file reads.
type goListPackage struct {
	ImportPath         string
	Name               string
	Dir                string
	ForTest            string
	GoFiles            []string
	EmbedPatterns      []string
	TestEmbedPatterns  []string
	XTestEmbedPatterns []string
	EmbedFiles         []string
	TestEmbedFiles     []string
	XTestEmbedFiles    []string
	Error              *struct{ Err string }
}

// goListPackages runs `go list -e` in dir with args and decodes the stream of
// JSON objects it prints. go test puts its own toolchain first on PATH, so
// "go" is the go command that built this test.
func goListPackages(t *testing.T, dir string, args ...string) []goListPackage {
	t.Helper()
	cmd := exec.Command("go", append([]string{"list", "-e"}, args...)...)
	cmd.Dir = dir
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%s: %v\n%s", strings.Join(cmd.Args, " "), err, stderr.String())
	}
	var pkgs []goListPackage
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var p goListPackage
		err := dec.Decode(&p)
		if errors.Is(err, io.EOF) {
			return pkgs
		}
		if err != nil {
			t.Fatalf("decode the output of %s: %v", strings.Join(cmd.Args, " "), err)
		}
		pkgs = append(pkgs, p)
	}
}

// goModuleRoot asks the go command which module this test belongs to.
func goModuleRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("go", "env", "GOMOD").Output()
	if err != nil {
		t.Fatalf("go env GOMOD: %v", err)
	}
	gomod := strings.TrimSpace(string(out))
	if gomod == "" || gomod == os.DevNull {
		t.Fatalf("go env GOMOD = %q: not inside a module", gomod)
	}
	return filepath.Dir(gomod)
}

// plantEditorDetritus adds to plants a .DS_Store in every directory under
// dir, and beside every regular file the lock emacs leaves while editing it,
// `.#<name>`, each backed by backing. It stops at a nested module, as embed
// does, and never enters .git.
func plantEditorDetritus(dir, backing string, plants map[string]string) error {
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != dir {
				if d.Name() == ".git" {
					return filepath.SkipDir
				}
				if _, err := os.Stat(filepath.Join(p, "go.mod")); err == nil {
					return filepath.SkipDir
				}
			}
			plants[filepath.Join(p, ".DS_Store")] = backing
			return nil
		}
		if d.Type().IsRegular() && !strings.HasPrefix(d.Name(), ".") {
			plants[filepath.Join(filepath.Dir(p), ".#"+d.Name())] = backing
		}
		return nil
	})
}

// without returns the members of a that b lacks, in a's order.
func without(a, b []string) []string {
	var out []string
	for _, s := range a {
		if !slices.Contains(b, s) {
			out = append(out, s)
		}
	}
	return out
}
