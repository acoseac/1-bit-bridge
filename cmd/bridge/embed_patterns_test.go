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
	paths, pkgs := embeddingPackages(t, root)
	overlay, plants := writeDetritusOverlay(t, paths, pkgs)

	const fields = "-json=ImportPath,Dir,ForTest,GoFiles,EmbedFiles,TestEmbedFiles,XTestEmbedFiles,Error"
	before := goListPackages(t, root, append([]string{"-test", fields}, paths...)...)
	after := map[string]goListPackage{}
	for _, p := range goListPackages(t, root, append([]string{"-test", "-overlay", overlay, fields}, paths...)...) {
		after[p.ImportPath] = p
	}

	c := embedComparison{pkgs: pkgs, plants: plants, after: after, reported: map[string]bool{}}
	for _, b := range before {
		c.compare(t, b)
	}
	if c.embedded == 0 {
		t.Fatal("no embedded file was listed, so nothing was compared")
	}
	t.Logf("%d packages, %d embedded files, %d plants", len(paths), c.embedded, len(plants))
}

// embeddingPackage is a package whose files carry at least one //go:embed
// pattern.
type embeddingPackage struct {
	name     string   // its package clause
	dir      string   // its directory, as the go command spells it
	patterns []string // its patterns, test files' included
}

// embeddingPackages lists, in go list's order, the packages under root with
// an embed pattern in any of their files.
func embeddingPackages(t *testing.T, root string) (paths []string, pkgs map[string]embeddingPackage) {
	t.Helper()
	pkgs = map[string]embeddingPackage{}
	for _, p := range goListPackages(t, root, "-json=ImportPath,Name,Dir,EmbedPatterns,TestEmbedPatterns,XTestEmbedPatterns", "./...") {
		all := slices.Concat(p.EmbedPatterns, p.TestEmbedPatterns, p.XTestEmbedPatterns)
		if len(all) == 0 {
			continue
		}
		paths = append(paths, p.ImportPath)
		pkgs[p.ImportPath] = embeddingPackage{name: p.Name, dir: p.Dir, patterns: all}
	}
	if len(paths) == 0 {
		t.Fatal("go list found no package using //go:embed, so this test checks nothing")
	}
	return paths, pkgs
}

// writeDetritusOverlay writes a `go list -overlay` file planting editor
// detritus in every package's directory tree (plantEditorDetritus), and
// returns its path with the plants. Each package also gets one ordinary
// source file, overlaySeenFile, which must show up in its GoFiles: proof the
// go command read the overlay there. Without it, an overlay the go command
// ignored (a path it spells differently, say) would leave both listings equal
// and the test green over nothing.
func writeDetritusOverlay(t *testing.T, paths []string, pkgs map[string]embeddingPackage) (string, map[string]string) {
	t.Helper()
	tmp := t.TempDir()
	// One backing file for every plant, holding what emacs writes into the
	// Windows-shape lock. Its content is never read: only the name matters.
	backing := filepath.Join(tmp, "lock")
	if err := os.WriteFile(backing, []byte("someone@host.1234:1695000000"), 0o600); err != nil {
		t.Fatal(err)
	}
	plants := map[string]string{}
	for i, ip := range paths {
		p := pkgs[ip]
		if err := plantEditorDetritus(p.dir, backing, plants); err != nil {
			t.Fatalf("plant beside %s: %v", p.dir, err)
		}
		src := filepath.Join(tmp, fmt.Sprintf("seen%d.go", i))
		if err := os.WriteFile(src, []byte("package "+p.name+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		plants[filepath.Join(p.dir, overlaySeenFile)] = src
	}
	overlay := filepath.Join(tmp, "overlay.json")
	replace, err := json.Marshal(map[string]map[string]string{"Replace": plants})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(overlay, replace, 0o600); err != nil {
		t.Fatal(err)
	}
	return overlay, plants
}

// embedComparison holds the two listings the probe compares, and what it has
// counted and reported so far.
type embedComparison struct {
	pkgs     map[string]embeddingPackage
	plants   map[string]string
	after    map[string]goListPackage // the planted listing, by import path
	reported map[string]bool          // package + "\x00" + file, reported once
	embedded int                      // embedded files checked for a plant
}

// compare checks one entry of the plain listing against the planted one.
func (c *embedComparison) compare(t *testing.T, b goListPackage) {
	t.Helper()
	if b.Error != nil {
		t.Errorf("%s does not build even without a plant: %s", b.ImportPath, b.Error.Err)
		return
	}
	// The package itself, as opposed to a test variant of it or its
	// generated test main.
	_, isPackage := c.pkgs[b.ImportPath]
	if isPackage {
		c.checkPlantedBeside(t, b)
	}
	a, ok := c.after[b.ImportPath]
	if !ok {
		t.Errorf("%s: missing from the planted listing", b.ImportPath)
		return
	}
	if isPackage && !slices.Contains(a.GoFiles, overlaySeenFile) {
		t.Errorf("%s: the go command did not read the overlay in %s (no %s among its "+
			"GoFiles), so nothing planted there was checked", b.ImportPath, b.Dir, overlaySeenFile)
	}
	if a.Error != nil {
		// Not the lock's build failure: every plant here is a regular file,
		// which embed accepts. Report what go list said.
		t.Errorf("%s: with the plants in place, go list reports %s (patterns: %s)",
			b.ImportPath, a.Error.Err, c.patternsOf(b))
		return
	}
	c.compareFiles(t, b, "EmbedFiles", b.EmbedFiles, a.EmbedFiles)
	c.compareFiles(t, b, "TestEmbedFiles", b.TestEmbedFiles, a.TestEmbedFiles)
	c.compareFiles(t, b, "XTestEmbedFiles", b.XTestEmbedFiles, a.XTestEmbedFiles)
}

// checkPlantedBeside counts b's embedded files and requires a lock planted
// beside each, or the comparison says nothing about the directory it sits in.
func (c *embedComparison) checkPlantedBeside(t *testing.T, b goListPackage) {
	t.Helper()
	for _, f := range slices.Concat(b.EmbedFiles, b.TestEmbedFiles, b.XTestEmbedFiles) {
		c.embedded++
		lock := filepath.Join(b.Dir, filepath.FromSlash(path.Dir(f)), ".#"+path.Base(f))
		if _, ok := c.plants[lock]; !ok {
			t.Errorf("%s embeds %s but nothing was planted beside it (%s), so a pattern "+
				"reaching that directory goes unchecked", b.ImportPath, f, lock)
		}
	}
}

// compareFiles reports what one of b's embedded-file lists gained or lost
// under the plants. A test variant embeds its package's files as well as its
// own, so a gained file is reported once, against the first listing that
// shows it.
func (c *embedComparison) compareFiles(t *testing.T, b goListPackage, field string, before, after []string) {
	t.Helper()
	base := c.baseOf(b)
	var extra []string
	for _, f := range without(after, before) {
		if path.Base(f) == overlaySeenFile { // the probe's own instrument
			continue
		}
		if k := base + "\x00" + f; !c.reported[k] {
			c.reported[k] = true
			extra = append(extra, f)
		}
	}
	if len(extra) > 0 {
		t.Errorf("%s %s: with an editor's lock beside every file and a .DS_Store in every "+
			"directory, it also embeds %s. On Windows emacs's lock is a regular file, so "+
			"it ships; on macOS and Linux it is a dangling symlink, and the build fails. "+
			"Start every glob element with [^.], never * (patterns: %s)",
			b.ImportPath, field, strings.Join(extra, " "), c.patternsOf(b))
	}
	if missing := without(before, after); len(missing) > 0 {
		t.Errorf("%s %s: the plants REMOVED %s from the embedded set",
			b.ImportPath, field, strings.Join(missing, " "))
	}
}

// baseOf names the package whose patterns b's files come from: a test
// variant names it in ForTest, and the generated test main is "<path>.test".
func (c *embedComparison) baseOf(b goListPackage) string {
	if b.ForTest != "" {
		return b.ForTest
	}
	return strings.TrimSuffix(b.ImportPath, ".test")
}

// patternsOf lists the embed patterns behind b, for a report.
func (c *embedComparison) patternsOf(b goListPackage) string {
	return strings.Join(c.pkgs[c.baseOf(b)].patterns, " ")
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
