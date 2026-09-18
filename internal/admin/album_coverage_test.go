package admin

import (
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// The coverage fixtures' two shapes, and the one mtime/size pair every
// row in this file shares — a variant is FRESH only while it matches its
// source's, so the seeds and the sidecars have to agree on both.
const (
	fixtureHiRate, fixtureHiBits = 96000.0, 24
	fixtureCDRate, fixtureCDBits = 44100.0, 16
	fixtureSize                  = 1000
)

func fixtureMTime() time.Time { return time.Unix(7, 0) }

// fixtureTrack builds one tagged track. The pointer fields are why this
// is a function rather than a literal: each track needs its OWN rate,
// bits and isDSD addresses, and a shared helper is the cheapest way to
// stop a future fixture aliasing them.
func fixtureTrack(path, title, album string, rate float64, bits int) *manifest.Track {
	no := false
	return &manifest.Track{
		Path: path, Title: title, Album: album, AlbumArtist: "Artist", Artist: "Artist",
		Codec: "FLAC", Size: fixtureSize, ModTime: fixtureMTime(),
		SampleRate: &rate, BitsPerSample: &bits, IsDSD: &no,
	}
}

func seedTracks(t *testing.T, st *manifest.Store, tracks ...*manifest.Track) {
	t.Helper()
	for _, tr := range tracks {
		if err := st.UpsertTrack(t.Context(), tr); err != nil {
			t.Fatal(err)
		}
	}
}

// addVariant writes one sidecar row against `path`, stamped fresh.
func addVariant(t *testing.T, st *manifest.Store, path, variantID string, rate, bits int) {
	t.Helper()
	if err := st.UpsertVariant(t.Context(), manifest.VariantRow{
		SourcePath: path, VariantID: variantID, SidecarPath: path + ".x",
		Format: "FLAC", SampleRate: rate, BitsPerSample: bits, SizeBytes: 100,
		SourceMTimeNS: fixtureMTime().UnixNano(), SourceSize: fixtureSize,
	}); err != nil {
		t.Fatal(err)
	}
}

// addOptimizedVariant is the CarPlay copy every test here reaches for.
func addOptimizedVariant(t *testing.T, st *manifest.Store, path string) {
	t.Helper()
	addVariant(t, st, path, "optimized-v2-48000-16", 48000, 16)
}

// seedCoverageLibrary stages three albums with deliberately different
// variant states: one fully CarPlay-covered, one untouched, and one
// CD-quality album that can never take a CarPlay copy at all.
func seedCoverageLibrary(t *testing.T, st *manifest.Store) {
	t.Helper()
	seedTracks(t, st,
		fixtureTrack("Hi/Covered/01.flac", "a", "Covered", fixtureHiRate, fixtureHiBits),
		fixtureTrack("Hi/Covered/02.flac", "b", "Covered", fixtureHiRate, fixtureHiBits),
		fixtureTrack("Hi/Bare/01.flac", "c", "Bare", fixtureHiRate, fixtureHiBits),
		fixtureTrack("Cd/Redbook/01.flac", "d", "Redbook", fixtureCDRate, fixtureCDBits),
	)
	for _, p := range []string{"Hi/Covered/01.flac", "Hi/Covered/02.flac"} {
		addOptimizedVariant(t, st, p)
	}
}

func albumsPage(t *testing.T, srv *Server, query string) (int, []map[string]any) {
	t.Helper()
	w, body := playerGet(t, srv, "/api/player/albums?limit=50&"+query)
	if w.Code != http.StatusOK {
		t.Fatalf("albums?%s: status %d body %s", query, w.Code, w.Body.String())
	}
	total := 0
	if f, ok := body["total"].(float64); ok {
		total = int(f)
	}
	raw, _ := body["albums"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, a := range raw {
		m, _ := a.(map[string]any)
		out = append(out, m)
	}
	return total, out
}

func titlesOf(albums []map[string]any) map[string]bool {
	out := map[string]bool{}
	for _, a := range albums {
		if s, ok := a["title"].(string); ok {
			out[s] = true
		}
	}
	return out
}

// TestAlbumGridCarriesCoveragePerAlbum: every tile gets its own numbers,
// against an eligible denominator. The CD album's optimize denominator
// is zero — it is already at the CarPlay target — which is the case a
// track-count denominator would have rendered as "0 of 1 missing".
func TestAlbumGridCarriesCoveragePerAlbum(t *testing.T) {
	srv, _, _ := newTestServer(t)
	seedCoverageLibrary(t, srv.deps.Manifest)

	_, albums := albumsPage(t, srv, "")
	byTitle := map[string]albumCoverage{}
	for _, a := range albums {
		var cov albumCoverage
		blob, _ := json.Marshal(a["variants"])
		_ = json.Unmarshal(blob, &cov)
		byTitle[a["title"].(string)] = cov
	}
	if got := byTitle["Covered"].Optimize; got.Covered != 2 || got.Eligible != 2 {
		t.Errorf("Covered optimize = %+v, want 2/2", got)
	}
	if got := byTitle["Bare"].Optimize; got.Covered != 0 || got.Eligible != 1 {
		t.Errorf("Bare optimize = %+v, want 0/1", got)
	}
	if got := byTitle["Redbook"].Optimize; got.Eligible != 0 || got.Exempt != 1 {
		t.Errorf("Redbook optimize = %+v, want eligible 0 / exempt 1", got)
	}
}

// TestAlbumGridNeedsFilterIsWholeLibrary is the reason the coverage
// snapshot exists rather than a per-page query. A filter applied to the
// PAGE would draw page 1 of the filtered list from page 1 of the
// unfiltered one, and report a total for the wrong set — so this checks
// the TOTAL as well as the contents.
func TestAlbumGridNeedsFilterIsWholeLibrary(t *testing.T) {
	srv, _, _ := newTestServer(t)
	seedCoverageLibrary(t, srv.deps.Manifest)

	total, albums := albumsPage(t, srv, "needs=optimize")
	got := titlesOf(albums)
	if total != 1 || !got["Bare"] {
		t.Fatalf("needs=optimize → total %d %v, want just Bare", total, got)
	}
	if got["Redbook"] {
		t.Error("an album already at the CarPlay target was listed as needing one")
	}
	if got["Covered"] {
		t.Error("a fully covered album was listed as needing more")
	}

	// Nothing has an upscale variant, and three of the four tracks are
	// upscale-eligible — so every album carrying one of those is listed.
	total, albums = albumsPage(t, srv, "needs=upscale")
	if total != 3 {
		t.Errorf("needs=upscale → total %d, want 3: %v", total, titlesOf(albums))
	}
}

// TestAlbumGridStaleFilter: an out-of-date copy is the one state a full
// bar hides, so it gets its own filter.
func TestAlbumGridStaleFilter(t *testing.T) {
	srv, _, _ := newTestServer(t)
	st := srv.deps.Manifest
	seedCoverageLibrary(t, st)

	if total, _ := albumsPage(t, srv, "needs=stale"); total != 0 {
		t.Fatalf("needs=stale on a fresh library → %d, want 0", total)
	}
	// Re-stamp one sidecar against a source that has moved on.
	if err := st.UpsertVariant(t.Context(), manifest.VariantRow{
		SourcePath: "Hi/Covered/01.flac", VariantID: "optimized-v2-48000-16",
		SidecarPath: "x", Format: "FLAC", SampleRate: 48000, BitsPerSample: 16,
		SizeBytes: 100, SourceMTimeNS: 1, SourceSize: 99,
	}); err != nil {
		t.Fatal(err)
	}
	srv.InvalidateAlbumCoverage()

	total, albums := albumsPage(t, srv, "needs=stale")
	if total != 1 || !titlesOf(albums)["Covered"] {
		t.Fatalf("needs=stale → total %d %v, want just Covered", total, titlesOf(albums))
	}
	// It is still COVERED: the batch walks skip a track that has a
	// variant of the kind, so promising otherwise would offer a
	// Generate that enqueues nothing.
	var cov albumCoverage
	blob, _ := json.Marshal(albums[0]["variants"])
	_ = json.Unmarshal(blob, &cov)
	if cov.Optimize.Covered != 2 || cov.Optimize.Stale != 1 {
		t.Errorf("Covered optimize = %+v, want covered 2 / stale 1", cov.Optimize)
	}
}

// TestAlbumGridRejectsAnUnknownNeedsValue: an unrecognised filter is a
// 400, never a silent fall-through to "everything". A typo that widened
// the view would read as the filter being broken.
func TestAlbumGridRejectsAnUnknownNeedsValue(t *testing.T) {
	srv, _, _ := newTestServer(t)
	seedCoverageLibrary(t, srv.deps.Manifest)
	w, _ := playerGet(t, srv, "/api/player/albums?needs=bogus")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400 (body %s)", w.Code, w.Body.String())
	}
}

// TestAlbumCoverageSnapshotKeysOnTheUpscaleTarget pins why eligibility
// is NOT baked into the library catalog: it depends on a runtime
// setting the catalog knows nothing about. Moving the target must move
// the denominators, not leave them until the next scan.
func TestAlbumCoverageSnapshotKeysOnTheUpscaleTarget(t *testing.T) {
	srv, _, _ := newTestServer(t)
	st := srv.deps.Manifest
	seedCoverageLibrary(t, st)

	// A 192/24 target leaves the 96/24 tracks upscale-eligible.
	if err := st.SetUpscaleTarget(t.Context(), 192000, 24); err != nil {
		t.Fatal(err)
	}
	srv.InvalidateAlbumCoverage()
	if total, albums := albumsPage(t, srv, "needs=upscale"); total != 3 {
		t.Fatalf("at 192/24 → total %d, want 3: %v", total, titlesOf(albums))
	}

	// Drop the target to 48/16 and the hi-res tracks are above it —
	// never downsampled, so no longer eligible. Only the CD track is.
	if err := st.SetUpscaleTarget(t.Context(), 48000, 16); err != nil {
		t.Fatal(err)
	}
	srv.InvalidateAlbumCoverage()
	total, albums := albumsPage(t, srv, "needs=upscale")
	if total != 1 || !titlesOf(albums)["Redbook"] {
		t.Fatalf("at 48/16 → total %d %v, want just Redbook", total, titlesOf(albums))
	}
}

// coverageOf reads one album's optimize coverage off the grid response.
func coverageOf(t *testing.T, srv *Server, title string) playerVariantCoverageDTO {
	t.Helper()
	_, albums := albumsPage(t, srv, "")
	for _, a := range albums {
		if a["title"] != title {
			continue
		}
		var cov albumCoverage
		blob, _ := json.Marshal(a["variants"])
		_ = json.Unmarshal(blob, &cov)
		return cov.Optimize
	}
	t.Fatalf("album %q not on the grid", title)
	return playerVariantCoverageDTO{}
}

// expireCoverage ages the snapshot past coverageTTL without touching the
// epoch or the target, which is precisely the "stale by the clock only"
// case. Rewriting builtAt rather than sleeping keeps the test off the
// wall clock — the Windows leg's ~15.6 ms granularity makes any timing
// assertion here unreliable, and this needs none.
func expireCoverage(t *testing.T, srv *Server) {
	t.Helper()
	c := srv.coverage.Load()
	if c == nil {
		t.Fatal("no coverage snapshot to expire")
	}
	aged := *c
	aged.builtAt = time.Now().Add(-2 * coverageTTL)
	srv.coverage.Store(&aged)
}

// TestClockStaleCoverageIsServedNotAwaited is the album grid's
// load-time contract, and it is asserted on CONTENT rather than on a
// duration: a request landing on a clock-stale snapshot must answer from
// the copy it has — the OLD number — and refresh behind itself, so the
// person opening the Albums page never waits on two whole-library scans.
//
// A blocking rebuild is exactly what the middle assertion catches: it
// would report the new coverage immediately, because it went and looked.
func TestClockStaleCoverageIsServedNotAwaited(t *testing.T) {
	srv, _, _ := newTestServer(t)
	seedCoverageLibrary(t, srv.deps.Manifest)

	if got := coverageOf(t, srv, "Bare"); got.Covered != 0 || got.Eligible != 1 {
		t.Fatalf("seeded Bare optimize = %+v, want 0/1", got)
	}

	// Plant a change the snapshot cannot know about. UpsertVariant is
	// what the auto-optimize sweeper does, and it deliberately does NOT
	// bump the catalog epoch — which is the entire reason coverage
	// carries a TTL at all.
	addOptimizedVariant(t, srv.deps.Manifest, "Hi/Bare/01.flac")
	expireCoverage(t, srv)

	if got := coverageOf(t, srv, "Bare"); got.Covered != 0 {
		t.Errorf("a clock-stale read reported covered=%d: the request blocked on a "+
			"rebuild instead of serving the snapshot it already had", got.Covered)
	}

	// ...and the refresh it kicked off lands, so the next reader is current.
	srv.WaitForCatalogRefresh()
	if got := coverageOf(t, srv, "Bare"); got.Covered != 1 {
		t.Errorf("after the background refresh Bare optimize = %+v, want covered 1", got)
	}
}

// TestAKnownWrongCoverageSnapshotIsRebuiltSynchronously is the other
// half, and it is what stops the fix above being "simplified" into
// always serving stale. An epoch bump means a scan happened and the
// ALBUM SET itself may have moved, so there is nothing worth serving —
// the answer has to be current before it is sent.
func TestAKnownWrongCoverageSnapshotIsRebuiltSynchronously(t *testing.T) {
	srv, _, _ := newTestServer(t)
	seedCoverageLibrary(t, srv.deps.Manifest)

	if got := coverageOf(t, srv, "Bare"); got.Covered != 0 {
		t.Fatalf("seeded Bare optimize covered = %d, want 0", got.Covered)
	}

	addOptimizedVariant(t, srv.deps.Manifest, "Hi/Bare/01.flac")
	srv.InvalidateLibraryCatalog() // a scan landed

	if got := coverageOf(t, srv, "Bare"); got.Covered != 1 {
		t.Errorf("after an epoch bump Bare optimize = %+v, want covered 1 without "+
			"waiting for a background refresh", got)
	}
}

// TestAConcurrentRebuildForAnotherTargetDoesNotJoinTheFlight pins the
// singleflight key to the snapshot identity.
//
// Keyed on a constant, a caller whose (epoch, rate, bits) differs from
// the one in flight JOINS it and is handed a map built for somebody
// else's upscale target — numbers the grid then renders as this target's
// bars and `filterAlbums` answers `needs=` from. The flight's internal
// re-check cannot catch it: it compares the LEADER's captured identity,
// which is by definition satisfied.
//
// The two targets are chosen so the answers cannot be confused: at
// 192000/24 every album has upscale-eligible tracks (4 in total), at
// 44100/16 none does. Joining is therefore visible as a non-zero count
// on the request that asked for the lower target.
func TestAConcurrentRebuildForAnotherTargetDoesNotJoinTheFlight(t *testing.T) {
	srv, _, _ := newTestServer(t)
	seedCoverageLibrary(t, srv.deps.Manifest)
	cat, err := srv.libraryCatalog(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	epoch := srv.catalogEpoch.Load()

	// Park the FIRST fold inside the flight, so the second caller
	// arrives while it is genuinely in progress rather than racing it.
	release := make(chan struct{})
	var builds atomic.Int32
	entered := make(chan struct{})
	coverageBuiltHookForTests = func() {
		if builds.Add(1) == 1 {
			close(entered)
			<-release
		}
	}
	// Drain before restoring, not after. A background refresher joined to
	// bgRefresh can still be inside the hook when a test ends, and the
	// cleanup's write would then race its read — the class this file's
	// sibling rule records as "a field deliberately left unsynchronised
	// binds TESTS too", which showed on CI and not in 26 local runs.
	// Nothing here spawns one today; the ordering is free and stops the
	// next test that does from having to know.
	t.Cleanup(func() {
		srv.WaitForCatalogRefresh()
		coverageBuiltHookForTests = nil
	})

	var wg sync.WaitGroup
	var hi, lo map[string]albumCoverage

	wg.Add(1)
	go func() {
		defer wg.Done()
		hi = srv.rebuildCoverage(t.Context(), cat, epoch, 192000, 24)
	}()
	<-entered // the 192k fold now owns the flight

	wg.Add(1)
	go func() {
		defer wg.Done()
		lo = srv.rebuildCoverage(t.Context(), cat, epoch, 44100, 16)
	}()
	close(release)
	wg.Wait()

	totalEligible := func(m map[string]albumCoverage) int {
		n := 0
		for _, a := range cat.Albums {
			n += m[a.ID].Upscale.Eligible
		}
		return n
	}
	if got := totalEligible(hi); got != 4 {
		t.Errorf("192000/24 upscale-eligible total = %d, want 4", got)
	}
	if got := totalEligible(lo); got != 0 {
		t.Errorf("44100/16 upscale-eligible total = %d, want 0 — this request was handed "+
			"the coverage built for 192000/24, i.e. it joined a flight for a different target", got)
	}
	if builds.Load() != 2 {
		t.Errorf("folds = %d, want 2 — the two targets must each build their own", builds.Load())
	}
}

// seedVariantFreeLibrary is the hosted tenant's shape and every fresh
// bridge's: real albums, a mix of eligible and not, and not one
// generated sidecar anywhere.
func seedVariantFreeLibrary(t *testing.T, st *manifest.Store) {
	t.Helper()
	seedTracks(t, st,
		fixtureTrack("Hi/Bare/01.flac", "a", "Bare", fixtureHiRate, fixtureHiBits),
		fixtureTrack("Cd/Redbook/01.flac", "b", "Redbook", fixtureCDRate, fixtureCDBits),
	)
}

// countCoverageBuilds installs the test hook and returns a reader for it.
//
// Drains before restoring, for the reason the concurrency test's cleanup
// records: a background refresher can still be inside the hook when a
// test ends, and the cleanup's write would race its read.
func countCoverageBuilds(t *testing.T, srv *Server) func() int {
	t.Helper()
	var n atomic.Int32
	coverageBuiltHookForTests = func() { n.Add(1) }
	t.Cleanup(func() {
		srv.WaitForCatalogRefresh()
		coverageBuiltHookForTests = nil
	})
	return func() int { return int(n.Load()) }
}

// TestAVariantFreeLibraryDoesNotBuildCoverage — with no sidecar anywhere,
// every album's coverage is a denominator and nothing else, so no tile can
// carry a badge. Computing that costs a full scan of `tracks` with three
// EXISTS subqueries, per page load, to produce something discarded. This is
// the hosted tenant's case: the cloud template ships upscale and optimize
// off, so those libraries never hold a variant at all.
func TestAVariantFreeLibraryDoesNotBuildCoverage(t *testing.T) {
	srv, _, _ := newTestServer(t)
	seedVariantFreeLibrary(t, srv.deps.Manifest)
	builds := countCoverageBuilds(t, srv)

	_, albums := albumsPage(t, srv, "")
	if len(albums) != 2 {
		t.Fatalf("got %d albums, want 2", len(albums))
	}
	if n := builds(); n != 0 {
		t.Errorf("coverage folded %d time(s) for a library with no variants — the whole "+
			"answer would have been discarded", n)
	}
	for _, a := range albums {
		if _, ok := a["variants"]; ok {
			t.Errorf("album %v carries a variants block with no variants in the library", a["title"])
		}
	}
}

// TestANeedsFilterStillBuildsCoverageWithoutVariants is the other half,
// and it guards a trap rather than a cost. The `needs=` filter reads the
// DENOMINATOR, which exists with or without a single sidecar — "which
// albums still need CarPlay copies" is a real question on a library that
// has never made one, and the answer is "all the eligible ones".
//
// Skipping the build here would make the filter unservable: an active
// predicate with no snapshot is refused (503) rather than dropped, so a
// variant-free bridge would answer every `needs=` query with an error.
// (`filterAlbums` used to drop the filter instead and serve the whole
// UNFILTERED library under a total — the worse failure, and the one
// TestANeedsFilterWithNoSnapshotIsRefusedNotWidened now pins shut.)
func TestANeedsFilterStillBuildsCoverageWithoutVariants(t *testing.T) {
	srv, _, _ := newTestServer(t)
	seedVariantFreeLibrary(t, srv.deps.Manifest)
	builds := countCoverageBuilds(t, srv)

	// `needs=optimize`, not `needs=upscale`: both seeded albums are
	// upscale-eligible against the default target, so that filter cannot
	// tell "applied" from "dropped" — the answer is the whole library
	// either way. Optimize narrows genuinely, because the CD album is
	// already at the CarPlay target and can never want a copy.
	total, albums := albumsPage(t, srv, "needs=optimize")
	if n := builds(); n == 0 {
		t.Fatal("needs=optimize answered without folding coverage — the filter reads the " +
			"denominator, and there is nothing else to read it from")
	}
	got := titlesOf(albums)
	if total != 1 || !got["Bare"] {
		t.Errorf("needs=optimize → total %d %v, want just Bare", total, got)
	}
	if got["Redbook"] {
		t.Error("an album already at the CarPlay target was listed as needing a copy — the " +
			"filter was not applied and the unfiltered library came back")
	}
}

// TestTheDefaultGridQueryDoesNotBuildCoverage is
// TestAVariantFreeLibraryDoesNotBuildCoverage asked with the query the
// player ACTUALLY sends. renderAlbums defaults `needs` to the literal
// "all" and the query builder drops only the empty string, so a default
// grid load is `needs=all` — and the handler's gate read the parameter's
// presence as "a filter is active". Every default load on a variant-free
// bridge paid both whole-library scans for a badge that could not appear,
// and the test beside this one passed because it sent no query at all.
//
// The test therefore sends the FULL default query, not a minimal one: a
// fixture must be the value the transformation would actually change.
func TestTheDefaultGridQueryDoesNotBuildCoverage(t *testing.T) {
	srv, _, _ := newTestServer(t)
	seedVariantFreeLibrary(t, srv.deps.Manifest)
	builds := countCoverageBuilds(t, srv)

	_, albums := albumsPage(t, srv, "sort=recent&quality=all&needs=all")
	if len(albums) != 2 {
		t.Fatalf("got %d albums, want 2", len(albums))
	}
	if n := builds(); n != 0 {
		t.Errorf("coverage folded %d time(s) under the player's default query — "+
			"`needs=all` is no filter, and the gate must read the parsed predicate, "+
			"not the parameter", n)
	}
	for _, a := range albums {
		if _, ok := a["variants"]; ok {
			t.Errorf("album %v carries a variants block with no variants in the library", a["title"])
		}
	}
}

// TestAnInvalidNeedsTokenIsRefusedBeforeAnyFold — the filter is parsed
// before coverage is consulted, so a bad token costs a 400 and nothing
// else. It used to reach the gate as "a filter is present", build the
// snapshot in full, and only then be refused by filterAlbums.
//
// Seeded WITH variants on purpose: on a variant-free library the gate
// alone would skip the build and the test would pass for the wrong
// reason.
func TestAnInvalidNeedsTokenIsRefusedBeforeAnyFold(t *testing.T) {
	srv, _, _ := newTestServer(t)
	seedCoverageLibrary(t, srv.deps.Manifest)
	builds := countCoverageBuilds(t, srv)

	w, body := playerGet(t, srv, "/api/player/albums?needs=bogus")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("needs=bogus: status %d body %s, want 400", w.Code, w.Body.String())
	}
	if body["error"] != "bad_request" {
		t.Errorf("error = %v, want bad_request", body["error"])
	}
	if n := builds(); n != 0 {
		t.Errorf("coverage folded %d time(s) for a query that was going to be refused", n)
	}
}

// TestANeedsFilterWithNoSnapshotIsRefusedNotWidened — when the filter is
// active and the snapshot cannot be built, the answer is a refusal, not
// the unfiltered library. filterAlbums used to drop the filter on a nil
// snapshot under a comment that called that the safe direction, and the
// response carried a plausible total for a set nobody asked for — a wrong
// answer shaped like a right one. Losing a BADGE is not losing the page,
// so the badge-only grid must still serve on the same fault.
func TestANeedsFilterWithNoSnapshotIsRefusedNotWidened(t *testing.T) {
	srv, _, _ := newTestServer(t)
	seedCoverageLibrary(t, srv.deps.Manifest)
	srv.failCoverageBuildForTests = func() error {
		return errors.New("injected: the coverage build failed")
	}

	w, body := playerGet(t, srv, "/api/player/albums?needs=optimize")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("needs=optimize with no snapshot: status %d body %s, want 503 — "+
			"an active filter must not be dropped into the unfiltered library",
			w.Code, w.Body.String())
	}
	if body["error"] != "coverage_unavailable" {
		t.Errorf("error = %v, want coverage_unavailable", body["error"])
	}
	if _, ok := body["albums"]; ok {
		t.Error("the refusal carried an album list")
	}

	// The badge-only grid degrades to no badges rather than to an error.
	w, body = playerGet(t, srv, "/api/player/albums?limit=50")
	if w.Code != http.StatusOK {
		t.Fatalf("badge-only grid on the same fault: status %d, want 200", w.Code)
	}
	if raw, _ := body["albums"].([]any); len(raw) != 3 {
		t.Errorf("badge-only grid served %d albums, want 3", len(raw))
	}
	for _, a := range body["albums"].([]any) {
		if m, _ := a.(map[string]any); m["variants"] != nil {
			t.Errorf("album %v carries a variants block the build could not have produced", m["title"])
		}
	}
}

// TestOneVariantIsEnoughToBuildCoverage pins the gate's edge: the skip is
// keyed on the library holding NO sidecar, not on the album in front of
// you having none, so a single row anywhere restores the old behaviour for
// every tile.
func TestOneVariantIsEnoughToBuildCoverage(t *testing.T) {
	srv, _, _ := newTestServer(t)
	seedVariantFreeLibrary(t, srv.deps.Manifest)
	addVariant(t, srv.deps.Manifest, "Cd/Redbook/01.flac", "upscaled-v2-96000-24", 96000, 24)
	builds := countCoverageBuilds(t, srv)

	_, albums := albumsPage(t, srv, "")
	if n := builds(); n != 1 {
		t.Errorf("coverage folded %d time(s), want 1 — one sidecar anywhere is enough", n)
	}
	var seen bool
	for _, a := range albums {
		if _, ok := a["variants"]; ok {
			seen = true
		}
	}
	if !seen {
		t.Error("no album carried a variants block although the library holds a sidecar")
	}
}
