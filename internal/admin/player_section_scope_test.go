package admin

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The section rail's links are static hrefs, so before applySectionScope
// they dropped `?source=` — clicking Artists inside an upstream landed on
// the whole library. The scope survived a sort change (the toolbar mutates
// the live URL) but not a section change, which is a difference no reader
// could be expected to predict.

var (
	jsSectionKeyRe = regexp.MustCompile(`\["([a-z]+)",\s*"`)
	jsScopedSetRe  = regexp.MustCompile(`(?s)SOURCE_SCOPED_SECTIONS = new Set\(\[(.*?)\]\)`)
	jsQuotedRe     = regexp.MustCompile(`"([a-z]+)"`)
)

// scopedSectionsFromBootJS reads the client's own list of sections a
// source scope narrows.
func scopedSectionsFromBootJS(t *testing.T) []string {
	t.Helper()
	src := readFile(t, filepath.Join("static", "player", "boot.js"))
	m := jsScopedSetRe.FindStringSubmatch(src)
	if m == nil {
		t.Fatal("boot.js: no SOURCE_SCOPED_SECTIONS set — the rail no longer " +
			"knows which sections a source can narrow")
	}
	var out []string
	for _, q := range jsQuotedRe.FindAllStringSubmatch(m[1], -1) {
		out = append(out, q[1])
	}
	if len(out) == 0 {
		t.Fatal("SOURCE_SCOPED_SECTIONS is empty; scoping to a source would " +
			"hide every section in the rail")
	}
	return out
}

// TestScopedSectionsAreRealSectionsTheServerActuallyFilters is the
// contract that keeps the rail honest in both directions.
//
// A key that is not a real section silently hides a section that should
// have stayed — a typo costs the reader a whole axis, and nothing else
// would notice. And a key whose endpoint ignores `source=` produces the
// opposite failure: a link that looks scoped, carries the parameter, and
// returns the whole library anyway. So the JS list drives real requests
// here rather than being compared against a third hand-written copy.
func TestScopedSectionsAreRealSectionsTheServerActuallyFilters(t *testing.T) {
	scoped := scopedSectionsFromBootJS(t)

	// Every scoped key must be a section the rail actually renders.
	src := readFile(t, filepath.Join("static", "player", "boot.js"))
	start := strings.Index(src, "const SECTIONS = [")
	if start < 0 {
		t.Fatal("boot.js: no SECTIONS list")
	}
	end := strings.Index(src[start:], "\n];")
	if end < 0 {
		t.Fatal("boot.js: unterminated SECTIONS list")
	}
	known := map[string]bool{}
	for _, m := range jsSectionKeyRe.FindAllStringSubmatch(src[start:start+end], -1) {
		known[m[1]] = true
	}
	for _, key := range scoped {
		if !known[key] {
			t.Errorf("SOURCE_SCOPED_SECTIONS names %q, which is not a section in "+
				"SECTIONS — a scope would hide a real section and link nowhere", key)
		}
	}

	srv, _, _ := newTestServer(t)
	seedHybridLibrary(t, srv.deps.Manifest)
	withTestUpstream(srv, true)

	// Genres and composers answer under "entries"; the others under their
	// own name. Anything else is a section this test does not know how to
	// drive, which is itself worth failing on.
	listKey := map[string]string{
		"albums": "albums", "artists": "artists",
		"genres": "entries", "composers": "entries",
	}
	count := func(target, key string) int {
		t.Helper()
		w, body := playerGet(t, srv, target)
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s = %d: %s", target, w.Code, w.Body.String())
		}
		rows, _ := body[key].([]any)
		return len(rows)
	}

	for _, key := range scoped {
		lk, ok := listKey[key]
		if !ok {
			t.Errorf("section %q is marked source-scoped but this test cannot "+
				"drive its endpoint; add it to listKey and prove it filters", key)
			continue
		}
		whole := count("/api/player/"+key, lk)
		narrowed := count("/api/player/"+key+"?source="+upstreamSourceID(), lk)
		if narrowed >= whole {
			t.Errorf("/api/player/%s returned %d rows scoped vs %d unscoped — the "+
				"rail links carry source= but the endpoint ignores it", key, narrowed, whole)
		}
	}
}

// TestPlayerBootCallIsLast pins the module's entry point to the bottom of
// the file.
//
// boot() reaches most of boot.js, so every `const` and `let` it can touch
// has to be initialised before it runs — and a top-level `boot()` near the
// top puts every declaration below it in the temporal dead zone. This has
// silently emptied the player twice in one sitting (the sources-rail TTL,
// then SOURCE_SCOPED_SECTIONS), each time with no failing test and no
// symptom beyond a ReferenceError in the console and a page that rendered
// nothing at all.
//
// Function declarations hoist, so the call's position changes nothing
// except which half of the file is safe to declare in.
func TestPlayerBootCallIsLast(t *testing.T) {
	src := readFile(t, filepath.Join("static", "player", "boot.js"))
	call := strings.LastIndex(src, "\nboot();")
	if call < 0 {
		t.Fatal("boot.js no longer calls boot(); the player never starts")
	}
	after := src[call+len("\nboot();"):]
	for _, line := range strings.Split(after, "\n") {
		if strings.HasPrefix(line, "const ") || strings.HasPrefix(line, "let ") {
			t.Errorf("top-level declaration after boot(): %q\n"+
				"boot() runs at module load, so anything it reaches must be "+
				"declared above the call or it is in the temporal dead zone.", line)
		}
	}
}

// TestScopeClearLinkDropsOnlyTheSource guards the way OUT of a scope.
//
// The banner's clear link is built by deleting one parameter from the live
// URL, so it must preserve the rest — dropping the sort alongside the
// scope would silently reorder the grid the reader is looking at.
func TestScopeClearLinkDropsOnlyTheSource(t *testing.T) {
	fn := extractJSFunction(t,
		readFile(t, filepath.Join("static", "player", "views.js")), "sourceScopeBanner")
	if !strings.Contains(fn, `searchParams.delete("source")`) {
		t.Error("the clear link no longer deletes just the source parameter")
	}
	for _, wrong := range []string{`new URL(location.pathname`, `href = "/albums"`} {
		if strings.Contains(fn, wrong) {
			t.Errorf("the clear link rebuilds the URL (%q) instead of removing one "+
				"parameter; the current sort and axis filters would be lost", wrong)
		}
	}
}

// TestSectionLinksCarryTheSourceScope pins the half the endpoint test
// above cannot see.
//
// That one proves the four endpoints filter; this one proves the rail
// actually ASKS them to. They are separate failures with the same
// symptom — the reported "it reverts back to the library view" was this
// half, with the endpoints working perfectly all along — and a build with
// the href rewrite removed passes the endpoint test unchanged.
func TestSectionLinksCarryTheSourceScope(t *testing.T) {
	src := readFile(t, filepath.Join("static", "player", "boot.js"))
	fn := extractJSFunction(t, src, "applySectionScope")

	// Rewritten from the UNSCOPED base, so adding and removing a scope is
	// idempotent — deriving the next href from the live one would compound
	// the query string on every navigation.
	if !strings.Contains(fn, "dataset.base") {
		t.Error("applySectionScope no longer reads data-base; rewriting the live " +
			"href would compound the query string across navigations")
	}
	href := regexp.MustCompile(`a\.href\s*=\s*([^;]+);`).FindStringSubmatch(fn)
	if href == nil {
		t.Fatal("applySectionScope does not assign a.href; the rail links cannot " +
			"carry the scope at all")
	}
	if !strings.Contains(href[1], "qs") {
		t.Errorf("a.href is assigned %q, which does not include the scope query — "+
			"clicking a section would drop ?source= and land on the whole library",
			strings.TrimSpace(href[1]))
	}
	if !strings.Contains(fn, "encodeURIComponent(source)") {
		t.Error("the source is not encoded into the href")
	}
	if !strings.Contains(extractJSFunction(t, src, "route"), "applySectionScope()") {
		t.Error("route() no longer calls applySectionScope; the rail stops " +
			"tracking the scope after the first navigation")
	}
}

// TestDetailLinksCarryTheSourceScope pins the other half of the reported
// "it takes me back to browse".
//
// The rail's own links were fixed first; a tile's link is the same bug
// one level down. Clicking an album inside an upstream landed on
// /album/<id> with no scope, so the sidebar reverted to Browse and the
// rail un-narrowed — the reader was out of the source without having
// asked to leave.
func TestDetailLinksCarryTheSourceScope(t *testing.T) {
	src := readFile(t, filepath.Join("static", "player", "views.js"))
	for _, spec := range []struct{ fn, what string }{
		{"albumTile", "an album tile"},
		{"artistTile", "an artist tile"},
	} {
		fn := extractJSFunction(t, src, spec.fn)
		if !strings.Contains(fn, "scopedHref(") {
			t.Errorf("%s builds its link without scopedHref; clicking it inside a "+
				"source drops the scope and lands on the whole library", spec.what)
		}
	}
	// The axis rows are built inline in renderAxis rather than in a named
	// tile builder, so they are checked from that function's body.
	if !strings.Contains(extractJSFunction(t, src, "renderAxis"), "scopedHref(") {
		t.Error("the genre/composer rows build their links without scopedHref")
	}
	// And scopedHref must read the LIVE url rather than take the scope as
	// an argument — a builder shared by a scoped and an unscoped grid would
	// otherwise need a branch, which is where the missed call site lives.
	sh := extractJSFunction(t, src, "scopedHref")
	if !strings.Contains(sh, "location.search") {
		t.Error("scopedHref no longer reads the live URL")
	}
	// The VALUE must be encoded, but not by a named mechanism: URL's
	// searchParams.set encodes inherently, and pinning encodeURIComponent
	// would have failed a strictly better implementation. What must never
	// appear is the raw value interpolated into a template string.
	if strings.Contains(sh, "${source}") {
		t.Error("scopedHref interpolates the raw source into the href")
	}
	if !strings.Contains(sh, `searchParams.set("source"`) &&
		!strings.Contains(sh, "encodeURIComponent(source)") {
		t.Error("scopedHref does not encode the source into the href")
	}
}

// TestCrumbsAreRootedAtTheSource pins the origin root the reader asked
// for: "Chord 2go > Albums > Waltz for Debby".
//
// A failed name lookup must yield NO root rather than a placeholder — a
// crumb reading "Source" tells the reader less than one that says
// nothing, and the crumb is the one place a wrong label is load-bearing.
func TestCrumbsAreRootedAtTheSource(t *testing.T) {
	fn := extractJSFunction(t,
		readFile(t, filepath.Join("static", "player", "views.js")), "sourceRootedCrumbs")
	if !strings.Contains(fn, "api.sourceNames()") {
		t.Error("the crumb root no longer resolves the source's name")
	}
	if !strings.Contains(fn, "if (!source) return items") {
		t.Error("sourceRootedCrumbs no longer passes an unscoped page through " +
			"unchanged; every crumb would grow a root")
	}
	if !strings.Contains(fn, "if (!name) return items") {
		t.Error("an unresolvable source no longer degrades to no root; the crumb " +
			"would show a placeholder where a real name belongs")
	}
}

// TestScopedHrefPreservesAnExistingQuery runs the SHIPPED function under
// node against a stubbed location.
//
// Every caller today passes a bare path, so this is forward-defense — but
// the failure it prevents is silent: appending "?source=…" to a path that
// already carries a query produces a second "?" and a URL that means
// nothing, and nothing in the UI would look wrong until a link was
// followed.
func TestScopedHrefPreservesAnExistingQuery(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; this test executes the shipped client source")
	}
	fn := extractJSFunction(t,
		readFile(t, filepath.Join("static", "player", "views.js")), "scopedHref")

	script := fn + `
globalThis.location = { search: "?source=abc%20def", origin: "http://x" };
console.log(JSON.stringify([
  scopedHref("/album/1"),
  scopedHref("/albums?sort=title"),
  scopedHref("/albums?source=stale"),
]));
`
	dir := t.TempDir()
	path := filepath.Join(dir, "scoped.mjs")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, path).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	var got []string
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("client returned %q: %v", out, err)
	}
	want := []string{
		"/album/1?source=abc+def",
		// The existing parameter survives and the separator is "&".
		"/albums?sort=title&source=abc+def",
		// A stale scope already on the path is REPLACED, not duplicated.
		"/albums?source=abc+def",
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("scopedHref case %d = %q, want %q", i, got[i], w)
		}
	}
}
