package admin

import (
	"fmt"
	"io/fs"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"testing/fstest"
)

// TestEmbeddedStaticTreeMatchesDisk guards a failure mode that is not a
// compile error, not a vet finding, and not covered by any handler
// test.
//
// The rule, stated precisely because the imprecise version misleads:
// `//go:embed static/[^.]*` embeds each entry directly inside static/
// whose name does not begin with ".", and a matched DIRECTORY is embedded
// recursively. At the top level the pattern admits "_"-prefixed names, so
// static/_probe.js would embed fine. But the recursive descent into a
// matched subdirectory silently SKIPS any entry whose name begins with
// "." or "_". So static/player/_util.js embeds NOTHING: it compiles, it
// works on a dev machine serving from disk, and it 404s in a release
// binary. Verified both ways when this test was written, and again for
// the [^.] pattern.
//
// Subdirectories are exactly where the player modules live, which is
// what makes this worth a test rather than a comment.
//
// The fix is NOT to switch the directive to `all:static`: that would
// suck a macOS .DS_Store (and any editor swap file) into every release
// binary. The fix is to notice, which is what this does. Until
// 2026-09-25 the pattern was `static/*`, which did the same at the top
// level. Its `*` matches a leading dot, so it embedded a top-level
// .DS_Store, and an editor's lock broke the build
// (TestEveryEmbedPatternRefusesALeadingDot).
func TestEmbeddedStaticTreeMatchesDisk(t *testing.T) {
	checkEmbeddedMatchesDisk(t, staticFS, "static", func(string) bool { return true })
}

// TestEmbeddedTemplatesMatchDisk is the same pin for templateFS: every .html
// file under templates/ is embedded, and nothing else is. New parses the
// templates at startup, so one that is missing from the embed fails the
// console's construction outright.
func TestEmbeddedTemplatesMatchDisk(t *testing.T) {
	checkEmbeddedMatchesDisk(t, templateFS, "templates", func(name string) bool {
		return path.Ext(name) == ".html"
	})
}

// checkEmbeddedMatchesDisk fails t for every disagreement embedDiskProblems
// finds between embedded and the package directory, and when nothing under
// root is embedded at all.
func checkEmbeddedMatchesDisk(t *testing.T, embedded fs.FS, root string, want func(name string) bool) {
	t.Helper()
	problems, n, err := embedDiskProblems(embedded, os.DirFS("."), root, want)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range problems {
		t.Error(p)
	}
	if n == 0 {
		t.Fatalf("nothing is embedded under %s/: the embed directive is broken", root)
	}
}

// embedDiskProblems compares the files embedded under root with the files on
// disk under root whose names want admits, and describes each disagreement.
// The disk side skips what the embed refuses: isEditorDetritus names, and
// any dot-directory. The embedded side skips a backup, which the embed cannot
// refuse inside a directory it walks. n is how many embedded files it
// compared.
func embedDiskProblems(embedded, disk fs.FS, root string, want func(name string) bool) (problems []string, n int, err error) {
	// Every file on disk under root, mapped to whether this FS should hold it.
	onDisk := map[string]bool{}
	err = fs.WalkDir(disk, root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// A dot-directory is a tool's (.cache, .vscode), and the embed
		// refuses one at every level: [^.] at the top, the walk below it.
		// A "_" directory is not skipped: the walk hides it too, so its
		// files are the 404 this comparison exists to report.
		if d.IsDir() {
			if p != root && strings.HasPrefix(d.Name(), ".") {
				return fs.SkipDir
			}
			return nil
		}
		if isEditorDetritus(d.Name()) {
			return nil
		}
		onDisk[p] = want(d.Name())
		return nil
	})
	if err != nil {
		return nil, 0, fmt.Errorf("walk %s/ on disk: %w", root, err)
	}

	inEmbed := map[string]bool{}
	err = fs.WalkDir(embedded, root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// A backup is embedded wherever a pattern walks its directory (see
		// isEditorDetritus), so on this side it is no disagreement either.
		if !d.IsDir() && !strings.HasSuffix(d.Name(), "~") {
			inEmbed[p] = true
		}
		return nil
	})
	if err != nil {
		return nil, 0, fmt.Errorf("walk embedded %s/: %w", root, err)
	}

	for p, wanted := range onDisk {
		if wanted && !inEmbed[p] {
			problems = append(problems, p+" exists on disk but is NOT embedded, so a release "+
				"build lacks it while a dev checkout serving from disk does not: the pattern "+
				"does not reach it, or a leading '.' or '_' in a directory below the top level "+
				"hides it from the walk")
		}
	}
	for p := range inEmbed {
		wanted, here := onDisk[p]
		switch {
		// First, whatever the disk side made of the name: a leading "." in
		// any element is the pattern's doing. "/"+p reaches the first
		// element too, which is the whole path when root is ".".
		case strings.Contains("/"+p, "/."):
			problems = append(problems, p+` is embedded, and a leading "." names an editor's `+
				`lock, a .DS_Store or a swap file: the pattern lets it through. Start every glob `+
				`element with [^.], never * (TestEveryEmbedPatternRefusesALeadingDot)`)
		case wanted:
		case here:
			problems = append(problems, p+" is embedded, but it is not a file this FS holds: "+
				"the pattern is wider than the FS it fills")
		default:
			problems = append(problems, p+" is embedded but missing from disk (stale build cache?)")
		}
	}
	sort.Strings(problems)
	return problems, len(inEmbed), nil
}

// TestEmbedDiskProblemsJudgesEachSideByWhatItsRuleCanRefuse drives
// embedDiskProblems with what an editor and the OS leave beside the console's
// files, on both sides of the comparison.
//
// The embedded side holds what a pattern cannot refuse. A backup (`app.js~`)
// is embedded by any pattern that walks its directory, because the go tool's
// walk below a matched directory skips only "." and "_" names; static/[^.]*
// walks static/player. So a backup is no disagreement, whichever side it is
// on: it was reported as "embedded but missing from disk (stale build
// cache?)" about a file sitting on disk, every time emacs saved an asset. A
// leading "." IS refused, so one on the embedded side is a pattern letting
// it through, and the report says that rather than blaming a cache. The disk
// side skips a dot-directory, which the embed refuses at every level, and
// not a "_" one, whose files are the 404 the comparison exists to report. A
// file that is on disk but outside what the FS holds is a pattern wider than
// its FS, not a stale cache.
func TestEmbedDiskProblemsJudgesEachSideByWhatItsRuleCanRefuse(t *testing.T) {
	for _, tc := range []struct {
		name     string
		root     string
		want     func(name string) bool
		disk     fstest.MapFS
		embedded fstest.MapFS
		problems map[string]string // file -> a phrase its problem must contain
	}{
		{
			name: "static",
			root: "static",
			want: func(string) bool { return true },
			disk: fstest.MapFS{
				"static/app.js":           {},
				"static/app.js~":          {},
				"static/.#app.js":         {},
				"static/.DS_Store":        {},
				"static/player/boot.js":   {},
				"static/player/boot.js~":  {},
				"static/player/.#boot.js": {},
				"static/player/_util.js":  {},
				// A tool's dot-directory. The embed refuses it at every level
				// ([^.] at the top, the walk below), so it is not the console's.
				"static/.cache/tool.js":        {},
				"static/player/.cache/deep.js": {},
				// A "_" directory is hidden from the walk too, but it is not a
				// tool's: its files are the 404 this comparison reports.
				"static/player/_lib/util.js": {},
				// Dot files an editor did not leave. isEditorDetritus takes
				// every leading ".", so neither reads as a console file.
				"static/.env":            {},
				"static/player/.gitkeep": {},
			},
			embedded: fstest.MapFS{
				"static/app.js":          {},
				"static/app.js~":         {},
				"static/player/boot.js":  {},
				"static/player/boot.js~": {},
				"static/.#app.js":        {}, // what static/* embedded from a Windows-shape lock
				"static/.env":            {}, // ...and from any other top-level dot file
				"static/gone.js":         {},
			},
			problems: map[string]string{
				"static/.#app.js":            `a leading "."`,
				"static/.env":                `a leading "."`,
				"static/gone.js":             "missing from disk",
				"static/player/_util.js":     "is NOT embedded",
				"static/player/_lib/util.js": "is NOT embedded",
			},
		},
		{
			// A file this FS should not hold, embedded by a pattern wider than
			// its own: it is on disk, so "missing from disk" would be false.
			name: "templates",
			root: "templates",
			want: func(name string) bool { return path.Ext(name) == ".html" },
			disk: fstest.MapFS{
				"templates/page.html": {},
				"templates/notes.txt": {},
			},
			embedded: fstest.MapFS{
				"templates/page.html": {},
				"templates/notes.txt": {},
			},
			problems: map[string]string{
				"templates/notes.txt": "wider than",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			problems, _, err := embedDiskProblems(tc.embedded, tc.disk, tc.root, tc.want)
			if err != nil {
				t.Fatal(err)
			}
			want := maps.Clone(tc.problems)
			for _, p := range problems {
				file, _, _ := strings.Cut(p, " ")
				phrase, ok := want[file]
				switch {
				case !ok:
					t.Errorf("unexpected problem: %s", p)
				case !strings.Contains(p, phrase):
					t.Errorf("the problem for %s should say %q: %s", file, phrase, p)
				}
				delete(want, file)
			}
			for file := range want {
				t.Errorf("no problem reported for %s", file)
			}
		})
	}
}

// isEditorDetritus reports whether a name under static/ belongs to an editor
// or the OS rather than to the console: a leading "." (a .DS_Store, emacs's
// `.#name` lock, a `._name` AppleDouble file) or a trailing "~" (a backup).
// None of it is the console's and none of it may be in the repo, so every
// test that reads static/ from disk skips it rather than fail an unrelated
// run over it.
//
// The embed refuses the first kind and cannot refuse the second. A leading
// "." is refused at the top level by the [^.] the pattern starts with, and
// below it by the go tool's walk. A backup is embedded wherever a pattern
// walks its directory, because that walk skips only "." and "_" names. (A
// glob bound to an extension, like templates/[^.]*.html, never matches one.)
// So embedDiskProblems tolerates a backup on the embedded side as well. (This
// comment called all of it "legitimately absent from the embed" until
// 2026-09-25, when neither half was true at the top level of static/.)
//
// Skipping is also the only safe way to handle one. Emacs's lock is a
// DANGLING symlink where it can make one, and a REGULAR file holding
// `user@host.pid:boot` where it cannot (always on Windows), so the file
// either cannot be opened or opens as something that is not JavaScript.
// Deliberately NOT the go tool's "_" rule as well: a top-level `_name.js`
// is embedded by `static/[^.]*` and ships, so the parity guards must read it.
func isEditorDetritus(name string) bool {
	return strings.HasPrefix(name, ".") || strings.HasSuffix(name, "~")
}

// TestStaticAssetsCarryPinnedContentType pins the two headers that
// native ES modules make load-bearing. A <script type="module"> is
// MIME-checked unconditionally and hard-fails on the wrong type, and
// on Windows mime.TypeByExtension consults the registry — where ".js"
// is routinely re-registered as "text/plain". That failure is
// invisible on a macOS dev box, so it gets a test rather than a
// manual smoke check.
func TestStaticAssetsCarryPinnedContentType(t *testing.T) {
	srv, _, _ := newTestServer(t)
	for _, tc := range []struct{ path, wantType string }{
		{"/static/app.js", "text/javascript; charset=utf-8"},
		{"/static/app.css", "text/css; charset=utf-8"},
	} {
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		req.RemoteAddr = "127.0.0.1:54321"
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s: status %d", tc.path, w.Code)
		}
		if got := w.Header().Get("Content-Type"); got != tc.wantType {
			t.Errorf("GET %s Content-Type = %q, want %q", tc.path, got, tc.wantType)
		}
		if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("GET %s X-Content-Type-Options = %q, want nosniff", tc.path, got)
		}
		if got := w.Header().Get("Cache-Control"); got != "no-cache" {
			t.Errorf("GET %s Cache-Control = %q, want no-cache — a stale module can "+
				"otherwise outlive a version bump, because relative import specifiers "+
				"do not inherit the entry module's ?v= query", tc.path, got)
		}
	}
}

func TestStaticContentType(t *testing.T) {
	for _, tc := range []struct{ ext, want string }{
		{".js", "text/javascript; charset=utf-8"},
		{".mjs", "text/javascript; charset=utf-8"},
		{".JS", "text/javascript; charset=utf-8"},
		{".css", "text/css; charset=utf-8"},
		{".json", "application/json; charset=utf-8"},
		{".svg", "image/svg+xml"},
		{".png", "image/png"},
		{".ico", ""}, // unknown → fall through to the file server's own sniffing
		{"", ""},
	} {
		if got := staticContentType(tc.ext); got != tc.want {
			t.Errorf("staticContentType(%q) = %q, want %q", tc.ext, got, tc.want)
		}
	}
}

// TestAdminArtworkPatternMatchesV1 is the lockstep pin the admin
// pattern's docblock claims and did not have. The admin route is the
// loopback twin of /v1/artwork; when the two id alphabets disagree,
// covers every paired iOS client can fetch become unreachable from the
// console — which is exactly what had happened: /v1 grew the 16-hex
// artworkVersion arm and the admin copy did not.
//
// It reads the regex literal out of internal/api's SOURCE rather than
// importing the package, for a hard reason: internal/api imports
// internal/admin, so any import here — test file or not — is a cycle.
// A hand-copied second literal would be two copies that can be wrong
// together (the failure mode this repo has hit before), so the test
// parses the one that ships.
func TestAdminArtworkPatternMatchesV1(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "api", "artwork.go"))
	if err != nil {
		t.Fatalf("read /v1 artwork source: %v", err)
	}
	// Normalise line endings before matching. Go's (?m) makes "$" match
	// before a "\n", but a CRLF checkout leaves the "\r" INSIDE the
	// line, so `\)$` never matches and the test fails with "could not
	// find the pattern" — on Windows only, and invisible on a macOS or
	// Linux dev box. That is exactly how it shipped red.
	text := strings.ReplaceAll(string(src), "\r\n", "\n")
	m := regexp.MustCompile("(?m)^var artworkMBIDPattern = regexp\\.MustCompile\\(`([^`]+)`\\)$").
		FindStringSubmatch(text)
	if m == nil {
		t.Fatal("could not find artworkMBIDPattern in internal/api/artwork.go — if it " +
			"was renamed or reshaped, update this test rather than deleting it: it is " +
			"the only thing keeping the admin twin in lockstep")
	}
	v1 := regexp.MustCompile(m[1])

	for _, id := range []string{
		"0007f5c9-27af-4221-9f1f-9dc3ef224875",
		"0007F5C9-27AF-4221-9F1F-9DC3EF224875",
		"local-" + strings.Repeat("a", 64),
		"0123456789abcdef",
		"", "..", "../etc/passwd", "0123456789ABCDEF",
		"local-" + strings.Repeat("a", 63),
		"0123456789abcde", "0123456789abcdef0",
	} {
		want := v1.MatchString(id)
		if got := adminArtworkMBIDPattern.MatchString(id); got != want {
			t.Errorf("id %q: admin pattern = %v, /v1 pattern = %v — the two must accept "+
				"the same set or a cover reachable from the phone is unreachable from "+
				"the console", id, got, want)
		}
	}
}

// TestAdminArtworkLadderCandidates pins the dedupe: a request naming a
// ladder size must not re-stat the same path twice on a full miss.
func TestAdminArtworkLadderCandidates(t *testing.T) {
	for _, tc := range []struct {
		size int
		want []int
	}{
		{500, []int{500, 1200, 250}},
		{1200, []int{1200, 500, 250}},
		{250, []int{250, 1200, 500}},
		{640, []int{640, 1200, 500, 250}},
	} {
		got := adminArtworkLadderCandidates(tc.size)
		if len(got) != len(tc.want) {
			t.Fatalf("size %d: got %v, want %v", tc.size, got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("size %d: got %v, want %v", tc.size, got, tc.want)
			}
		}
	}
}
