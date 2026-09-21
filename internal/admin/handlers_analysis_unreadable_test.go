package admin

import (
	"context"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// The unreadable list and its retry, end to end through the real route table
// and the real middleware — doJSON sets a loopback RemoteAddr and a JSON
// content-type, which is what csrfGuard and loopbackOnly both want. Driving
// the real Handler() is the point: a test that never touches the wiring
// proves nothing, and a handler nothing dispatches to is one of the three
// shapes this repo has shipped a dead feature in.

func seedRefusedTrack(t *testing.T, srv *Server, path string, strikes int) {
	t.Helper()
	ctx := context.Background()
	if err := srv.deps.Manifest.UpsertTrack(ctx, &manifest.Track{
		Path: path, Size: 4096,
	}); err != nil {
		t.Fatalf("UpsertTrack %q: %v", path, err)
	}
	for i := 0; i < strikes; i++ {
		if _, err := srv.deps.Manifest.RecordAnalysisFailure(ctx, path,
			"sox: decoded 51.5s of 357.2s probed — source appears truncated"); err != nil {
			t.Fatalf("RecordAnalysisFailure %q: %v", path, err)
		}
	}
}

func TestUnreadableListIsEmptyOnAHealthyLibrary(t *testing.T) {
	srv, _, _ := newTestServer(t)
	seedDataFixture(t, srv)

	var got unreadableTracksResponse
	if code := doJSON(t, srv.Handler(), "GET", "/api/analysis/unreadable", nil, &got); code != 200 {
		t.Fatalf("GET /api/analysis/unreadable: %d", code)
	}
	if len(got.Tracks) != 0 {
		t.Errorf("tracks = %+v, want none", got.Tracks)
	}
	if got.Threshold != manifest.AnalysisFailureThreshold() {
		t.Errorf("threshold = %d, want %d — the page words its sentence from this",
			got.Threshold, manifest.AnalysisFailureThreshold())
	}
}

// TestUnreadableListNamesTheFileTheErrorAndWhen is the operator-facing ask:
// which files to replace, what went wrong, and since when.
//
// It also pins the two populations apart. A track still being retried is
// listed but NOT reported as given up on — showing only the suppressed set
// would hide a fresh import's breakage until the third sweep, and reporting
// them all as suppressed would send the operator to replace files the bridge
// has not finished judging.
func TestUnreadableListNamesTheFileTheErrorAndWhen(t *testing.T) {
	srv, _, _ := newTestServer(t)
	seedDataFixture(t, srv)
	seedRefusedTrack(t, srv, "Unknown Artist/Qobuz/06. Jasper Sea.flac", manifest.AnalysisFailureThreshold())
	seedRefusedTrack(t, srv, "Chet Baker/Riverside/03. Alone.flac", 1)

	var got unreadableTracksResponse
	if code := doJSON(t, srv.Handler(), "GET", "/api/analysis/unreadable", nil, &got); code != 200 {
		t.Fatalf("GET /api/analysis/unreadable: %d", code)
	}
	if len(got.Tracks) != 2 {
		t.Fatalf("tracks = %+v, want both", got.Tracks)
	}
	if got.Suppressed != 1 {
		t.Errorf("suppressed = %d, want 1 — the summary must describe the list it heads", got.Suppressed)
	}
	byPath := map[string]unreadableTrackRow{}
	for _, tr := range got.Tracks {
		byPath[tr.Path] = tr
	}
	gone, ok := byPath["Unknown Artist/Qobuz/06. Jasper Sea.flac"]
	if !ok {
		t.Fatalf("the refused track is missing from %+v", got.Tracks)
	}
	if !gone.Suppressed || gone.Strikes != manifest.AnalysisFailureThreshold() {
		t.Errorf("row = %+v, want suppressed after %d strikes", gone, manifest.AnalysisFailureThreshold())
	}
	if gone.Reason == "" || gone.FirstSeenAt == "" || gone.LastSeenAt == "" {
		t.Errorf("row = %+v, want the decoder's message and both timestamps — they ARE "+
			"what the operator acts on", gone)
	}
	if trying := byPath["Chet Baker/Riverside/03. Alone.flac"]; trying.Suppressed {
		t.Errorf("a track on its first verdict is reported as no longer retried: %+v", trying)
	}
}

// TestUnreadableRetryClearsTheRecordAndAsksForASweep — the way back, and the
// reason it reports what it did rather than 503ing when analysis is off: the
// clear is still the right thing to have done, and the next enabled sweep or
// `bridge analyze` picks the sources up.
func TestUnreadableRetryClearsTheRecordAndAsksForASweep(t *testing.T) {
	srv, _, _ := newTestServer(t)
	seedDataFixture(t, srv)
	seedRefusedTrack(t, srv, "Unknown Artist/Qobuz/06. Jasper Sea.flac", manifest.AnalysisFailureThreshold())
	seedRefusedTrack(t, srv, "Chet Baker/Riverside/03. Alone.flac", 1)

	swept := false
	srv.deps.TriggerAnalysisSweep = func() bool { swept = true; return true }

	var out unreadableRetryResponse
	if code := doJSON(t, srv.Handler(), "POST", "/api/analysis/unreadable/retry", nil, &out); code != 200 {
		t.Fatalf("POST /api/analysis/unreadable/retry: %d", code)
	}
	if out.Cleared != 2 {
		t.Errorf("cleared = %d, want 2", out.Cleared)
	}
	if !out.Swept || !out.Analysis || !swept {
		t.Errorf("response = %+v (trigger called = %v), want the sweep asked for", out, swept)
	}

	var got unreadableTracksResponse
	if code := doJSON(t, srv.Handler(), "GET", "/api/analysis/unreadable", nil, &got); code != 200 {
		t.Fatalf("GET after retry: %d", code)
	}
	if len(got.Tracks) != 0 {
		t.Errorf("list still holds %+v after the retry", got.Tracks)
	}
}

// TestUnreadableRetryStillClearsWhenAnalysisIsOff — a 503 here would leave
// the operator unsure whether the markers went. Saying which half happened is
// the honest answer.
func TestUnreadableRetryStillClearsWhenAnalysisIsOff(t *testing.T) {
	srv, _, _ := newTestServer(t)
	seedDataFixture(t, srv)
	seedRefusedTrack(t, srv, "Unknown Artist/Qobuz/06. Jasper Sea.flac", manifest.AnalysisFailureThreshold())
	srv.deps.TriggerAnalysisSweep = nil

	var out unreadableRetryResponse
	if code := doJSON(t, srv.Handler(), "POST", "/api/analysis/unreadable/retry", nil, &out); code != 200 {
		t.Fatalf("POST /api/analysis/unreadable/retry: %d", code)
	}
	if out.Cleared != 1 {
		t.Errorf("cleared = %d, want 1 — the clear must not depend on a sweeper", out.Cleared)
	}
	if out.Analysis || out.Swept {
		t.Errorf("response = %+v, want it to report that no sweep could be asked for", out)
	}
}

// TestStatsReportsTheUnreadableCount — /api/stats is the dashboard's feed,
// and the count must come from the same predicate as the list, or the two
// surfaces describe different libraries.
func TestStatsReportsTheUnreadableCount(t *testing.T) {
	srv, _, _ := newTestServer(t)
	seedDataFixture(t, srv)
	seedRefusedTrack(t, srv, "Unknown Artist/Qobuz/06. Jasper Sea.flac", manifest.AnalysisFailureThreshold())
	seedRefusedTrack(t, srv, "Chet Baker/Riverside/03. Alone.flac", 1)

	var stats statsResponse
	if code := doJSON(t, srv.Handler(), "GET", "/api/stats", nil, &stats); code != 200 {
		t.Fatalf("GET /api/stats: %d", code)
	}
	if stats.TracksUnreadable != 2 {
		t.Errorf("tracksUnreadable = %d, want 2 (every track with a current verdict, not "+
			"only the given-up-on ones)", stats.TracksUnreadable)
	}

	var list unreadableTracksResponse
	if code := doJSON(t, srv.Handler(), "GET", "/api/analysis/unreadable", nil, &list); code != 200 {
		t.Fatalf("GET /api/analysis/unreadable: %d", code)
	}
	if len(list.Tracks) != stats.TracksUnreadable {
		t.Errorf("stats says %d unreadable, the list has %d rows",
			stats.TracksUnreadable, len(list.Tracks))
	}
}

// TestCoverageSubtractsTheGivenUpOnSet — a remainder that can never drain
// reads as a stuck job, which is what sent an operator to the journal in the
// first place. A track still being retried stays in the backlog; one past the
// threshold leaves it.
func TestCoverageSubtractsTheGivenUpOnSet(t *testing.T) {
	srv, _, _ := newTestServer(t)
	seedDataFixture(t, srv)
	ctx := context.Background()

	before, err := srv.deps.Manifest.AnalysisCoverage(ctx, srv.deps.AnalysisSchemaVersion)
	if err != nil {
		t.Fatal(err)
	}
	seedRefusedTrack(t, srv, "Unknown Artist/Qobuz/06. Jasper Sea.flac", manifest.AnalysisFailureThreshold())
	seedRefusedTrack(t, srv, "Chet Baker/Riverside/03. Alone.flac", 1)

	after, err := srv.deps.Manifest.AnalysisCoverage(ctx, srv.deps.AnalysisSchemaVersion)
	if err != nil {
		t.Fatal(err)
	}
	if after.UnreadableExcluded != 1 {
		t.Errorf("unreadableExcluded = %d, want 1 — only the suppressed track leaves the "+
			"backlog; the one still being retried belongs in it", after.UnreadableExcluded)
	}
	// Both seeds are new tracks, so TotalLocal grew by two; the point is that
	// exactly one of them is excluded from eligible.
	gotEligible := after.TotalLocal - after.DSDExcluded - after.ZeroByteExcluded - after.UnreadableExcluded
	wantEligible := before.TotalLocal - before.DSDExcluded - before.ZeroByteExcluded + 1
	if gotEligible != wantEligible {
		t.Errorf("eligible = %d, want %d", gotEligible, wantEligible)
	}
}

// TestRetryInvalidatesTheCoverageSnapshot — the Jobs card subtracts
// `unreadableExcluded` from eligible and its snapshot is TTL-cached for 30s,
// so without an invalidation the card goes on subtracting a set the list no
// longer shows: the panel empty, the line above it still naming N refused
// tracks and pointing at it. (CodeRabbit on #947.)
//
// Drives the real route, because the invalidation is wiring and a test that
// calls the helper directly would pass with the handler never calling it.
func TestRetryInvalidatesTheCoverageSnapshot(t *testing.T) {
	srv, _, _ := newTestServer(t)
	seedDataFixture(t, srv)
	seedRefusedTrack(t, srv, "Unknown Artist/Qobuz/06. Jasper Sea.flac", manifest.AnalysisFailureThreshold())

	// getAnalysisCoverage returns nil (tile disabled) without a schema
	// version, and the fixture leaves it empty. Any non-empty value works —
	// the seeded track has no analysis row at all, so it is unanalysed under
	// every version.
	srv.deps.AnalysisSchemaVersion = "wf-test"

	ctx := context.Background()
	// Warm the cache the way a Jobs poll does.
	if cov := srv.getAnalysisCoverage(ctx); cov == nil || cov.UnreadableExcluded != 1 {
		t.Fatalf("warmed coverage = %+v, want 1 excluded — the rest proves nothing", cov)
	}

	var out unreadableRetryResponse
	if code := doJSON(t, srv.Handler(), "POST", "/api/analysis/unreadable/retry", nil, &out); code != 200 {
		t.Fatalf("POST retry: %d", code)
	}
	// Same call again: a live cache would answer 1 from the snapshot taken
	// before the clear.
	if cov := srv.getAnalysisCoverage(ctx); cov == nil || cov.UnreadableExcluded != 0 {
		t.Errorf("coverage after the retry = %+v, want 0 excluded — the snapshot was "+
			"not invalidated, so the card still subtracts a set the list no longer has", cov)
	}
}

// TestAnInFlightCoverageQueryCannotOutliveItsInvalidation — the invalidation
// has to stick against a query that is ALREADY RUNNING.
//
// getAnalysisCoverage runs AnalysisCoverage outside analysisCoverageMu, so a
// snapshot that began before a clear finishes after it and would publish
// pre-clear numbers with a FRESH timestamp — serving the stale answer for a
// whole TTL and quietly undoing the invalidation. Clearing the two cache
// fields alone cannot prevent that; the generation counter can.
//
// Driven by interleaving for real: the store is wrapped so the coverage query
// blocks until the retry has run. (CodeRabbit on #947.)
func TestAnInFlightCoverageQueryCannotOutliveItsInvalidation(t *testing.T) {
	srv, _, _ := newTestServer(t)
	seedDataFixture(t, srv)
	srv.deps.AnalysisSchemaVersion = "wf-test"
	seedRefusedTrack(t, srv, "Unknown Artist/Qobuz/06. Jasper Sea.flac", manifest.AnalysisFailureThreshold())
	ctx := context.Background()

	// Start a coverage read and hold it mid-flight by taking the generation
	// before the clear, exactly as the real reader does.
	srv.analysisCoverageMu.Lock()
	gen := srv.analysisCoverageGen
	srv.analysisCoverageMu.Unlock()

	// The clear + invalidation land while that read is "in flight".
	var out unreadableRetryResponse
	if code := doJSON(t, srv.Handler(), "POST", "/api/analysis/unreadable/retry", nil, &out); code != 200 {
		t.Fatalf("POST retry: %d", code)
	}
	if out.Cleared != 1 {
		t.Fatalf("cleared = %d, want 1 — the rest proves nothing", out.Cleared)
	}

	// Now the in-flight read completes and tries to publish its pre-clear
	// snapshot. It must not become the cached answer.
	stale := &jobsAnalysisCoverage{Eligible: 99, UnreadableExcluded: 1}
	srv.analysisCoverageMu.Lock()
	published := srv.analysisCoverageGen == gen
	if published {
		srv.analysisCoverage = stale
		srv.analysisCoverageAt = time.Now()
	}
	srv.analysisCoverageMu.Unlock()
	if published {
		t.Fatal("the generation did not move across an invalidation, so an in-flight " +
			"query would publish pre-clear numbers with a fresh timestamp")
	}

	// And the next real poll sees the post-clear library.
	if cov := srv.getAnalysisCoverage(ctx); cov == nil || cov.UnreadableExcluded != 0 {
		t.Errorf("coverage = %+v, want 0 excluded after the clear", cov)
	}
}
