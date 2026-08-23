package admin

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestEmbeddedStaticTreeMatchesDisk guards a failure mode that is not a
// compile error, not a vet finding, and not covered by any handler
// test.
//
// The rule, stated precisely because the imprecise version misleads:
// `//go:embed static/*` embeds each entry directly inside static/,
// and a matched DIRECTORY is embedded recursively. At the top level
// the explicit `*` matches everything, INCLUDING "_"-prefixed names —
// so static/_probe.js would embed fine. But the recursive descent into
// a matched subdirectory silently SKIPS any entry whose name begins
// with "." or "_". So static/player/_util.js embeds NOTHING: it
// compiles, it works on a dev machine serving from disk, and it 404s
// in a release binary. Verified both ways when this test was written.
//
// Subdirectories are exactly where the player modules live, which is
// what makes this worth a test rather than a comment.
//
// The fix is NOT to switch the directive to `all:static`: that would
// suck a macOS .DS_Store (and any editor swap file) into every release
// binary. The fix is to notice, which is what this does.
func TestEmbeddedStaticTreeMatchesDisk(t *testing.T) {
	onDisk := map[string]bool{}
	err := filepath.WalkDir("static", func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		name := d.Name()
		// Editor/OS detritus is legitimately absent from the embed and
		// must not be in the repo either; skip rather than fail so a
		// stray local .DS_Store doesn't break an unrelated test run.
		if strings.HasPrefix(name, ".") || strings.HasSuffix(name, "~") {
			return nil
		}
		onDisk[filepath.ToSlash(p)] = true
		return nil
	})
	if err != nil {
		t.Fatalf("walk disk static/: %v", err)
	}

	embedded := map[string]bool{}
	err = fs.WalkDir(staticFS, "static", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			embedded[p] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk embedded static/: %v", err)
	}

	for p := range onDisk {
		if !embedded[p] {
			t.Errorf("%s exists on disk but is NOT embedded — a leading '.' or '_' in a "+
				"path segment excludes it from //go:embed static/*, so it will 404 in a "+
				"release build while working from a dev checkout", p)
		}
	}
	for p := range embedded {
		if !onDisk[p] {
			t.Errorf("%s is embedded but missing from disk (stale build cache?)", p)
		}
	}
	if len(embedded) == 0 {
		t.Fatal("no embedded static assets — the embed directive is broken")
	}
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
	m := regexp.MustCompile("(?m)^var artworkMBIDPattern = regexp\\.MustCompile\\(`([^`]+)`\\)$").
		FindSubmatch(src)
	if m == nil {
		t.Fatal("could not find artworkMBIDPattern in internal/api/artwork.go — if it " +
			"was renamed or reshaped, update this test rather than deleting it: it is " +
			"the only thing keeping the admin twin in lockstep")
	}
	v1 := regexp.MustCompile(string(m[1]))

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
