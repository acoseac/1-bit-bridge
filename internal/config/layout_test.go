package config

import (
	"go/ast"
	"go/parser"
	"go/token"
	"math/rand/v2"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// TestThePredicateComparesEachOSsFormsByTheirNames is the layout check's
// predicate (pathForms.within) over each OS's forms, on any host: POSIX's,
// compared byte for byte, and Windows', its drive letters, UNC shares and
// device paths read as Windows reads them and its names compared without
// regard to case. A path is inside a root, or is the root, by its names,
// never by a string prefix (backlog B884's case 3: /music2 is not inside
// /music), and one that is not absolute in that OS's forms is inside nothing.
func TestThePredicateComparesEachOSsFormsByTheirNames(t *testing.T) {
	type row struct {
		p, root string
		want    bool
	}
	posix := []row{
		{"/music/data", "/music", true},
		{"/music", "/music", true},
		{"/music/", "/music", true},
		{"/music/./data/", "/music/", true},
		{"/music/../music/data", "/music", true},
		{"//music/data", "/music", true},
		{"/x", "/", true},
		{"/", "/", true},
		{"/music2", "/music", false},
		{"/music2/data", "/music", false},
		{"/mus", "/music", false},
		{"/music", "/music/data", false},
		{"/music/../data", "/music", false},
		// POSIX compares names byte for byte: a case a macOS volume folds is
		// that volume's to say, and asking it is a filesystem touch.
		{"/Music/data", "/music", false},
		{"music/data", "/music", false},
		{"/music/data", "music", false},
		{"", "/", false},
		{`C:\Music\data`, `C:\Music`, false},
	}
	windows := []row{
		{`C:\Music\data`, `C:\Music`, true},
		{`C:\Music`, `C:\Music`, true},
		{`C:\Music\`, `C:\Music`, true},
		{`c:\music\DATA`, `C:\Music`, true},
		{`C:/Music/data`, `C:\Music`, true},
		{`C:\Music\..\Music\data`, `C:\Music`, true},
		{`C:\Music\data`, `C:\`, true},
		{`C:\Müsic\data`, `C:\MÜSIC`, true},
		{`C:\K\data`, `C:\k`, true},
		{`C:\Music2`, `C:\Music`, false},
		{`C:\Music2\data`, `C:\Music`, false},
		{`D:\Music\data`, `C:\Music`, false},
		{`C:\Music`, `C:\Music\data`, false},
		// Unicode's simple upper-casing, as NTFS's table maps a code unit:
		// the Kelvin sign is upper-case already, and k upper-cases to K.
		{"C:\\\u212a\\data", `C:\k`, false},
		{`\\nas\share\Music\data`, `\\NAS\Share\Music`, true},
		{`\\nas\share\x`, `\\nas\share`, true},
		{`//nas/share/x`, `\\nas\share`, true},
		{`\\nas\share2\x`, `\\nas\share`, false},
		{`\\nas2\share\x`, `\\nas\share`, false},
		{`\\?\C:\Music\data`, `C:\Music`, true},
		{`\\.\C:\Music\data`, `C:\Music`, true},
		{`\??\C:\Music\data`, `C:\Music`, true},
		{`C:\Music\data`, `\\?\c:\music`, true},
		{`\\?\UNC\nas\share\x`, `\\nas\share`, true},
		{`\\?\UNC\nas\share\x`, `\\?\unc\NAS\share`, true},
		{`\\?\D:\Music\data`, `C:\Music`, false},
		{`\Music\data`, `C:\Music`, false},
		{`C:Music\data`, `C:\Music`, false},
		{`/music/data`, `/music`, false},
	}
	for _, forms := range []struct {
		goos string
		rows []row
	}{{"linux", posix}, {"darwin", posix}, {"windows", windows}} {
		f := formsOf(forms.goos)
		for _, r := range forms.rows {
			if got := f.within(f.clean(r.p), f.clean(r.root)); got != r.want {
				t.Errorf("%s: %q inside or equal to %q = %v, want %v", forms.goos, r.p, r.root, got, r.want)
			}
			if !f.isAbs(r.p) || !f.isAbs(r.root) {
				// The layout resolves a relative path against the config's
				// directory (the next test).
				continue
			}
			// The layout's own way in: the data directory against the root,
			// beside a config file outside both.
			cfg := "/elsewhere/bridge.yaml"
			if forms.goos == "windows" {
				cfg = `E:\elsewhere\bridge.yaml`
			}
			if got := layoutOn(forms.goos, cfg, r.p, []string{r.root}).dataIn == 0; got != r.want {
				t.Errorf("%s: the data directory %q in a layout with root %q is inside it: %v, want %v", forms.goos, r.p, r.root, got, r.want)
			}
		}
	}
}

// TestTheLayoutResolvesRelativePathsAsLoadDoes: a relative data directory or
// root is made absolute against the config file's directory, as Load's
// resolvePaths makes it (ResolvePath), Windows' quirks included: a root
// relative to the drive (\data) and one relative to another drive's current
// directory (D:data) are joined to that directory as filepath.Join joins
// them. With no absolute config file there is nothing to resolve a relative
// path against, and it is inside nothing; an absolute one is still compared.
// A root that is "" is passed over; with several roots holding a file, the
// first in libraryRoots order is the one named.
func TestTheLayoutResolvesRelativePathsAsLoadDoes(t *testing.T) {
	for _, tc := range []struct {
		name, goos, cfg, data string
		roots                 []string
		configIn, dataIn      int
	}{
		{"the default data directory beside a config in the root", "linux", "/srv/cfg/bridge.yaml", "./data", []string{"/srv/cfg"}, 0, 0},
		{"a data directory that climbs out of the root", "linux", "/srv/cfg/bridge.yaml", "../data", []string{"/srv/cfg"}, 0, -1},
		{"a root that is the config's directory", "linux", "/srv/cfg/bridge.yaml", "data", []string{"."}, 0, 0},
		{"a root beside the config's directory", "linux", "/srv/cfg/bridge.yaml", "data", []string{"../music"}, -1, -1},
		{"a data directory in the second root", "linux", "/srv/cfg/bridge.yaml", "/srv/music/.bridge", []string{"/srv/videos", "/srv/music"}, -1, 1},
		{"nested roots name the first in order", "linux", "/srv/cfg/bridge.yaml", "/m/sub/data", []string{"/m", "/m/sub"}, -1, 0},
		{"nested roots in the other order", "linux", "/srv/cfg/bridge.yaml", "/m/sub/data", []string{"/x", "/m/sub", "/m"}, -1, 1},
		{"an empty root is passed over", "linux", "/srv/cfg/bridge.yaml", "./data", []string{"", "/srv/cfg"}, 1, 1},
		{"no data directory", "linux", "/srv/cfg/bridge.yaml", "", []string{"/srv/cfg"}, 0, -1},
		{"a relative config resolves nothing relative", "linux", "bridge.yaml", "./data", []string{"."}, -1, -1},
		{"a relative config still compares what is absolute", "linux", "bridge.yaml", "/m/data", []string{"/m"}, -1, 0},
		{"Windows' default data directory", "windows", `C:\cfg\bridge.yaml`, `.\data`, []string{`C:\cfg`}, 0, 0},
		{"Windows joins a drive-rooted path to the config's directory", "windows", `C:\cfg\bridge.yaml`, `\data`, []string{`C:\data`}, -1, -1},
		{"…so it is inside the config's directory", "windows", `C:\cfg\bridge.yaml`, `\data`, []string{`C:\cfg`}, 0, 0},
		{"Windows joins a drive-relative path as Join does", "windows", `C:\cfg\bridge.yaml`, `D:data`, []string{`C:\cfg`}, 0, 0},
		{"…and it is not on that drive", "windows", `C:\cfg\bridge.yaml`, `D:data`, []string{`D:\`}, -1, -1},
		{"a UNC config's relative paths", "windows", `\\nas\share\cfg\bridge.yaml`, "data", []string{`\\NAS\share`}, 0, 0},
		{"a relative root on Windows", "windows", `C:\cfg\bridge.yaml`, `C:\music\x`, []string{`..\music`}, -1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := layoutOn(tc.goos, tc.cfg, tc.data, tc.roots)
			if l.configIn != tc.configIn || l.dataIn != tc.dataIn {
				t.Errorf("layoutOn(%s, %q, %q, %q): the config file in root %d and the data directory in root %d, want %d and %d",
					tc.goos, tc.cfg, tc.data, tc.roots, l.configIn, l.dataIn, tc.configIn, tc.dataIn)
			}
		})
	}
}

// TestTheLayoutsFormsAreThisHostsFilepath holds the layout check's port of
// Go's path functions to Go's own, on the host the test runs on: POSIX's on
// Linux and macOS, Windows' on Windows, where this compares the port of
// volume names, IsAbs, Clean (its postClean included), Dir and Join with
// filepath's own, and the resolution against the config's directory with
// ResolvePath, which Load resolves paths with. Over hand-picked paths and a
// seeded sample of strings made of the names and marks Windows reads as
// volumes, separators and dots.
func TestTheLayoutsFormsAreThisHostsFilepath(t *testing.T) {
	f := formsOf(runtime.GOOS)
	paths := []string{
		"", ".", "..", "/", "//", "///", "a", "a/b", "/a/b/", "a/../..", "/../a", "./a",
		`\`, `\\`, `\\\`, `C:`, `C:\`, `C:/`, `c:a`, `C:\a\..\..`, `C:..`, `\a`, `\??\C:\x`, `\??`,
		`\\?\C:\`, `\\?\C:`, `\\.\C:\a`, `\\?\UNC\h\s\x`, `\\.\UNC\h\s`, `\\.\unc\h`, `\\h\s`,
		`\\h\s\`, `\\h\s\x\..\..`, `//h/s/x`, `\\h`, `\\..\s`, `a:b`, `a/../c:`, `a/../b:c/../d`,
		`\a\..\??\c:\x`, `\\?`, `\\?\`, `\\.\`, `\\?\a\b\..`, `C:\a.\b `, "é/ü",
	}
	tokens := []string{"a", "b", "C:", "c:", ":", ".", "..", "/", `\`, "?", "??", "UNC", "unc", " ", "x.y", "é"}
	rng := rand.New(rand.NewPCG(884, 1))
	for range 20000 {
		var b strings.Builder
		for range 1 + rng.IntN(6) {
			b.WriteString(tokens[rng.IntN(len(tokens))])
		}
		paths = append(paths, b.String())
	}
	bases := []string{"/", "/srv/cfg", "/srv/cfg/"}
	if runtime.GOOS == "windows" {
		bases = []string{`C:\`, `C:\cfg`, `C:\cfg\`, `c:/cfg`, `\\nas\share`, `\\nas\share\cfg`, `\\?\C:\cfg`, `\\.\UNC\h\s\cfg`}
	}
	for _, base := range bases {
		if !filepath.IsAbs(base) {
			t.Fatalf("the base %q is not absolute on %s", base, runtime.GOOS)
		}
	}
	for _, p := range paths {
		if got, want := f.volumeLen(p), len(filepath.VolumeName(p)); got != want {
			t.Errorf("volumeLen(%q) = %d, want %d", p, got, want)
		}
		if got, want := f.isAbs(p), filepath.IsAbs(p); got != want {
			t.Errorf("isAbs(%q) = %v, want %v", p, got, want)
		}
		if got, want := f.clean(p), filepath.Clean(p); got != want {
			t.Errorf("clean(%q) = %q, want %q", p, got, want)
		}
		if p != "" {
			if got, want := f.dir(p), filepath.Dir(p); got != want {
				t.Errorf("dir(%q) = %q, want %q", p, got, want)
			}
		}
		for _, base := range bases {
			if p != "" {
				if got, want := f.join(base, p), filepath.Join(base, p); got != want {
					t.Errorf("join(%q, %q) = %q, want %q", base, p, got, want)
				}
			}
			got, _ := f.absolute(base, p)
			if want := ResolvePath(base, p); got != want {
				t.Errorf("absolute(%q, %q) = %q, ResolvePath gives %q", base, p, got, want)
			}
		}
	}
}

// TestTheLayoutCheckRunsOnStringsAlone holds layout.go, the layout check and
// its words, to strings: it imports nothing that reaches a filesystem (os,
// path/filepath, io/fs, syscall, fsutil among them), and every call it makes
// is to a function or method declared in it, a builtin, strings.Builder's
// methods, or a function of the string packages it imports, runtime only for
// its GOOS. So no path it is asked about is stat'ed, resolved through a link
// or opened, and a library root whose mount does not answer cannot hold it,
// at bridge init, bridge doctor or serve (backlog B884's case 5). Read from
// the source, so it says which call reaches out, whatever the answer.
func TestTheLayoutCheckRunsOnStringsAlone(t *testing.T) {
	src, err := os.ReadFile("layout.go")
	if err != nil {
		t.Fatal(err)
	}
	file, err := parser.ParseFile(token.NewFileSet(), "layout.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	stringPackages := map[string]bool{"runtime": true, "strconv": true, "strings": true, "unicode": true, "unicode/utf8": true}
	imported := map[string]string{}
	for _, imp := range file.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			t.Fatal(err)
		}
		name := path[strings.LastIndex(path, "/")+1:]
		if imp.Name != nil {
			name = imp.Name.Name
		}
		imported[name] = path
		if !stringPackages[path] {
			t.Errorf("layout.go imports %s: the layout check runs on strings alone, and reaches no filesystem", path)
		}
	}
	declared := map[string]bool{}
	for _, d := range file.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			declared[d.Name.Name] = true
		case *ast.GenDecl:
			for _, s := range d.Specs {
				if ts, ok := s.(*ast.TypeSpec); ok {
					declared[ts.Name.Name] = true
				}
			}
		}
	}
	builtins := map[string]bool{"append": true, "len": true, "make": true, "copy": true, "string": true, "byte": true, "rune": true}
	builderMethods := map[string]bool{"WriteString": true, "WriteByte": true, "Len": true, "String": true}
	calls := 0
	ast.Inspect(file, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok {
			if x, ok := sel.X.(*ast.Ident); ok && imported[x.Name] == "runtime" && sel.Sel.Name != "GOOS" {
				t.Errorf("layout.go uses runtime.%s: it reads runtime.GOOS alone", sel.Sel.Name)
			}
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		calls++
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			if !declared[fn.Name] && !builtins[fn.Name] {
				t.Errorf("layout.go calls %s, which it does not declare: a function of another file may reach a filesystem", fn.Name)
			}
		case *ast.SelectorExpr:
			if x, ok := fn.X.(*ast.Ident); ok {
				if path, isPackage := imported[x.Name]; isPackage {
					if !stringPackages[path] || path == "runtime" {
						t.Errorf("layout.go calls %s.%s: the layout check stats, resolves and opens nothing", x.Name, fn.Sel.Name)
					}
					return true
				}
			}
			if !declared[fn.Sel.Name] && !builderMethods[fn.Sel.Name] {
				t.Errorf("layout.go calls a method %s it does not declare", fn.Sel.Name)
			}
		case *ast.ArrayType:
			// A conversion, []byte(s).
		default:
			t.Errorf("layout.go calls a %T", fn)
		}
		return true
	})
	// A floor, so a sweep that read nothing cannot pass.
	if calls < 80 {
		t.Errorf("read %d calls in layout.go, fewer than the check makes", calls)
	}
}

// TestALayoutJoinedOnlyThroughALinkIsMissed is the check's documented limit
// (backlog B884's case 6): a data directory spelled through a symbolic link
// that leads into a library root is inside it on the disk, and the lexical
// check does not see it, since seeing it means resolving the link, which is
// a filesystem touch. The same layout spelled without the link is found.
func TestALayoutJoinedOnlyThroughALinkIsMissed(t *testing.T) {
	base := t.TempDir()
	music := filepath.Join(base, "music")
	if err := os.MkdirAll(filepath.Join(music, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(music, link); err != nil {
		t.Skipf("no symbolic link can be made here: %v", err)
	}
	throughLink := filepath.Join(link, "data")
	onDisk, err := filepath.EvalSymlinks(throughLink)
	if err != nil {
		t.Fatal(err)
	}
	resolvedMusic, err := filepath.EvalSymlinks(music)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(onDisk) != resolvedMusic {
		t.Fatalf("%s resolves to %s, not into %s: the fixture joins nothing", throughLink, onDisk, resolvedMusic)
	}
	cfgFile := filepath.Join(base, "bridge.yaml")
	if l := LayoutOf(cfgFile, throughLink, []string{music}); l.Inside() {
		t.Errorf("the data directory %s, inside %s only through a link, was found inside it: %q", throughLink, music, l.Summary(true))
	}
	if l := LayoutOf(cfgFile, filepath.Join(music, "data"), []string{music}); !l.Inside() {
		t.Errorf("the data directory spelled without the link was not found inside %s", music)
	}
}

// The words of the advice to move bridge.yaml (backlog B884's round 2): the
// settings Load resolves beside the config, as the config spells them and in
// resolvePaths' order, what a BRIDGE_* variable that gives one does, and
// dataDir's place where the data directory moves too.
const (
	wantMovedKeys      = "libraryRoots, dataDir, tlsCertPath, tlsKeyPath, autocert.cacheDir, upscale.variantsDir and upscale.tempDir"
	wantMoveEnv        = "a value its BRIDGE_* environment variable gives resolves the same way, and is changed where that variable is set"
	wantDataDirFollows = "dataDir to the data directory's new path: it moves with the data directory and is not pinned to where it is now"
)

// TestTheLayoutsWordsNameWhatLiesInsideWhichRoot pins the words bridge init,
// bridge doctor and serve say a layout in: what lies inside which root, why
// that is a risk, and what to do, for the config file (moved, keeping where
// its relative paths point), the data directory (moved, and dataDir set) and
// both (one warning naming both). The CLI's words name each by its path, the
// root beside its place in libraryRoots, and offer the platform's default
// directory only where it lies outside every root; the journal's name each
// by its setting and carry no path (B505), and keep round 1's words.
func TestTheLayoutsWordsNameWhatLiesInsideWhichRoot(t *testing.T) {
	const home = "/home/u/.config/1-bit-bridge"
	for _, tc := range []struct {
		name                string
		cfg, data, platform string
		roots               []string
		summary, byNames    string
		advice, adviceNames string
	}{
		{
			name: "the data directory", cfg: "/etc/b/bridge.yaml", data: "/m/.bridge", platform: home, roots: []string{"/m"},
			summary: "the data directory /m/.bridge is inside library root /m (libraryRoots[0])",
			byNames: "the data directory (dataDir) is inside library root libraryRoots[0]",
			advice: layoutRisk + "; move the data directory outside every library root, to " + home +
				"/data for example (the platform's default), and set dataDir in bridge.yaml to its new path",
			adviceNames: layoutRisk + "; move the data directory outside every library root, and set dataDir in bridge.yaml to its new path",
		},
		{
			name: "the data directory equal to the root", cfg: "/etc/b/bridge.yaml", data: "/m", platform: home, roots: []string{"/m"},
			summary: "the data directory /m is inside library root /m (libraryRoots[0])",
			byNames: "the data directory (dataDir) is inside library root libraryRoots[0]",
			advice: layoutRisk + "; move the data directory outside every library root, to " + home +
				"/data for example (the platform's default), and set dataDir in bridge.yaml to its new path",
			adviceNames: layoutRisk + "; move the data directory outside every library root, and set dataDir in bridge.yaml to its new path",
		},
		{
			name: "the config file", cfg: "/m/conf/bridge.yaml", data: "/var/lib/b", platform: home, roots: []string{"/x", "/m"},
			summary: "the config file /m/conf/bridge.yaml is inside library root /m (libraryRoots[1])",
			byNames: "the config file is inside library root libraryRoots[1]",
			advice: layoutRisk + "; move bridge.yaml outside every library root, to " + home + " for example (the platform's default); " +
				"before you move it, set each of " + wantMovedKeys + " that is relative in it to an absolute path, since each resolves " +
				"beside bridge.yaml (dataDir to /var/lib/b, where the data directory is now); " + wantMoveEnv,
			adviceNames: layoutRisk + "; move bridge.yaml outside every library root, first setting dataDir in it to the data " +
				"directory's absolute path: a relative dataDir resolves beside bridge.yaml",
		},
		{
			name: "both in one root", cfg: "/m/b/bridge.yaml", data: "/m/b/data", platform: home, roots: []string{"/m", "/m/b"},
			summary: "the config file /m/b/bridge.yaml and the data directory /m/b/data are inside library root /m (libraryRoots[0])",
			byNames: "the config file and the data directory (dataDir) are inside library root libraryRoots[0]",
			advice: layoutRisk + "; move bridge.yaml and the data directory outside every library root, to " + home +
				" for example (the platform's default); before you move bridge.yaml, set each of " + wantMovedKeys + " that is relative " +
				"in it to an absolute path, since each resolves beside bridge.yaml (" + wantDataDirFollows + "); " + wantMoveEnv,
			adviceNames: layoutRisk + "; move bridge.yaml and the data directory outside every library root, keeping dataDir " +
				"pointed at the data directory: a relative dataDir (./data unless set) resolves beside bridge.yaml",
		},
		{
			name: "both in two roots", cfg: "/a/bridge.yaml", data: "/b/data", platform: "", roots: []string{"/b", "/a"},
			summary: "the config file /a/bridge.yaml is inside library root /a (libraryRoots[1]), and the data directory /b/data inside library root /b (libraryRoots[0])",
			byNames: "the config file is inside library root libraryRoots[1], and the data directory (dataDir) inside library root libraryRoots[0]",
			advice: layoutRisk + "; move bridge.yaml and the data directory outside every library root; before you move " +
				"bridge.yaml, set each of " + wantMovedKeys + " that is relative in it to an absolute path, since each resolves beside " +
				"bridge.yaml (" + wantDataDirFollows + "); " + wantMoveEnv,
			adviceNames: layoutRisk + "; move bridge.yaml and the data directory outside every library root, keeping dataDir " +
				"pointed at the data directory: a relative dataDir (./data unless set) resolves beside bridge.yaml",
		},
		{
			name: "a platform default inside a root is offered nowhere", cfg: "/etc/b/bridge.yaml", data: "/home/u/data", platform: home, roots: []string{"/home/u"},
			summary:     "the data directory /home/u/data is inside library root /home/u (libraryRoots[0])",
			byNames:     "the data directory (dataDir) is inside library root libraryRoots[0]",
			advice:      layoutRisk + "; move the data directory outside every library root, and set dataDir in bridge.yaml to its new path",
			adviceNames: layoutRisk + "; move the data directory outside every library root, and set dataDir in bridge.yaml to its new path",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := layoutOn("linux", tc.cfg, tc.data, tc.roots)
			if got := l.Summary(true); got != tc.summary {
				t.Errorf("Summary(byPath) = %q\nwant %q", got, tc.summary)
			}
			if got := l.Summary(false); got != tc.byNames {
				t.Errorf("Summary(by setting) = %q\nwant %q", got, tc.byNames)
			}
			if got := l.Advice(true, tc.platform); got != tc.advice {
				t.Errorf("Advice(byPath) = %q\nwant %q", got, tc.advice)
			}
			if got := l.Advice(false, ""); got != tc.adviceNames {
				t.Errorf("Advice(by setting) = %q\nwant %q", got, tc.adviceNames)
			}
			if got, want := l.Line(true, tc.platform), tc.summary+": "+tc.advice; got != want {
				t.Errorf("Line(byPath) = %q\nwant %q", got, want)
			}
			journal := l.Line(false, "")
			for _, p := range append(slices.Clone(tc.roots), tc.cfg, tc.data, home) {
				if strings.Contains(journal, p) {
					t.Errorf("the journal's line names the path %s: %q", p, journal)
				}
			}
		})
	}
	healthy := layoutOn("linux", "/etc/b/bridge.yaml", "/var/lib/b", []string{"/m"})
	if healthy.Inside() || healthy.Summary(true) != "" || healthy.Advice(true, home) != "" || healthy.Line(true, home) != "" {
		t.Errorf("a layout with nothing inside a root says %q, %q, %q", healthy.Summary(true), healthy.Advice(true, home), healthy.Line(true, home))
	}
}

// TestTheAdviceToMoveBridgeYAMLKeepsEveryRelativePathsMeaning (backlog B884's
// round 2): wherever the CLI's words say to move bridge.yaml, the config file
// alone or with the data directory, they say to set, before it moves, each
// setting Load resolves beside it that is relative to an absolute path, every
// one of them named as the config spells it and in resolvePaths' own order
// (read from resolvePaths here, so a setting added there is a setting the
// words must name), and that a value a BRIDGE_* variable gives resolves the
// same way and is changed where that variable is set. A config moved alone
// pins dataDir where the data directory is; one moved with the data directory
// says dataDir follows it, never that it stays. The journal's words, which
// name no value (B505), are round 1's.
func TestTheAdviceToMoveBridgeYAMLKeepsEveryRelativePathsMeaning(t *testing.T) {
	keys := settingsResolvedBesideTheConfig(t)
	list := strings.Join(keys[:len(keys)-1], ", ") + " and " + keys[len(keys)-1]
	keep := "set each of " + list + " that is relative in it to an absolute path, since each resolves beside bridge.yaml"
	for _, tc := range []struct {
		name, cfg, data, journal string
		both                     bool
	}{
		{"the config file alone", "/m/conf/bridge.yaml", "/var/lib/b",
			"the config file is inside library root libraryRoots[0]: " + layoutRisk + "; move bridge.yaml outside every library " +
				"root, first setting dataDir in it to the data directory's absolute path: a relative dataDir resolves beside bridge.yaml", false},
		{"the config file and the data directory", "/m/b/bridge.yaml", "/m/b/data",
			"the config file and the data directory (dataDir) are inside library root libraryRoots[0]: " + layoutRisk +
				"; move bridge.yaml and the data directory outside every library root, keeping dataDir pointed at the data " +
				"directory: a relative dataDir (./data unless set) resolves beside bridge.yaml", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := layoutOn("linux", tc.cfg, tc.data, []string{"/m"})
			advice := l.Advice(true, "/home/u/.config/1-bit-bridge")
			if !strings.Contains(advice, keep) {
				t.Errorf("the advice to move bridge.yaml does not say %q:\n%s", keep, advice)
			}
			if !strings.Contains(advice, wantMoveEnv) {
				t.Errorf("the advice to move bridge.yaml does not say what a BRIDGE_* variable that gives one does:\n%s", advice)
			}
			pinned := "(dataDir to " + tc.data + ", where the data directory is now)"
			if got := strings.Contains(advice, wantDataDirFollows); got != tc.both {
				t.Errorf("the advice says dataDir follows the data directory: %v, want %v:\n%s", got, tc.both, advice)
			}
			if got := strings.Contains(advice, pinned); got == tc.both {
				t.Errorf("the advice pins dataDir where the data directory is: %v, want %v:\n%s", got, !tc.both, advice)
			}
			if got := l.Line(false, ""); got != tc.journal {
				t.Errorf("the journal's line changed:\n%s\nwant round 1's\n%s", got, tc.journal)
			}
		})
	}
}

// settingsResolvedBesideTheConfig is every setting resolvePaths resolves
// against the config file's directory, in its order, as the config spells it:
// each assignment of a ResolvePath call in its body, its field path read back
// to the yaml keys of Config's fields.
func settingsResolvedBesideTheConfig(t *testing.T) []string {
	t.Helper()
	src, err := os.ReadFile("config.go")
	if err != nil {
		t.Fatal(err)
	}
	file, err := parser.ParseFile(token.NewFileSet(), "config.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	var body *ast.BlockStmt
	for _, d := range file.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Recv != nil && fn.Name.Name == "resolvePaths" {
			body = fn.Body
		}
	}
	if body == nil {
		t.Fatal("config.go declares no resolvePaths method")
	}
	var keys []string
	ast.Inspect(body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
			return true
		}
		call, ok := as.Rhs[0].(*ast.CallExpr)
		if !ok {
			return true
		}
		if fn, ok := call.Fun.(*ast.Ident); !ok || fn.Name != "ResolvePath" {
			return true
		}
		target := as.Lhs[0]
		if ix, ok := target.(*ast.IndexExpr); ok {
			target = ix.X
		}
		var fields []string
		for {
			sel, ok := target.(*ast.SelectorExpr)
			if !ok {
				break
			}
			fields = append([]string{sel.Sel.Name}, fields...)
			target = sel.X
		}
		typ := reflect.TypeOf(Config{})
		var path []string
		for _, name := range fields {
			f, ok := typ.FieldByName(name)
			if !ok {
				t.Fatalf("resolvePaths resolves %s, which Config has no field for", strings.Join(fields, "."))
			}
			path = append(path, strings.Split(f.Tag.Get("yaml"), ",")[0])
			typ = f.Type
		}
		keys = append(keys, strings.Join(path, "."))
		return true
	})
	// A floor, so a sweep that matched nothing cannot pass: seven when
	// written (backlog B884's round 2).
	if len(keys) < 7 {
		t.Fatalf("read %d settings resolvePaths resolves, fewer than it does: %q", len(keys), keys)
	}
	return keys
}
