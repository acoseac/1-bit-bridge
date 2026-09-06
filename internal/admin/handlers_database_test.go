package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// postCompact drives POST /api/database/compact through the REAL
// Handler() and returns the recorder.
//
// One definition rather than the same five lines at every call site: the
// content-type and the loopback RemoteAddr are not incidental — they are
// what carries the request past csrfGuard and the loopback middleware, so
// a copy that drifts on either would be testing the middleware's refusal
// rather than the handler.
func postCompact(t *testing.T, h http.Handler) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/database/compact", nil)
	req.Header.Set("content-type", "application/json")
	req.RemoteAddr = "127.0.0.1:54321"
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func TestDatabaseCompactReclaimsAndReports(t *testing.T) {
	s, _, _ := newTestServer(t)
	h := s.Handler()

	rr := postCompact(t, h)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var got databaseCompactResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, rr.Body.String())
	}
	if got.BeforeBytes <= 0 || got.AfterBytes <= 0 {
		t.Errorf("implausible sizes: %+v", got)
	}
	// NOT `ReclaimedBytes == BeforeBytes-AfterBytes`. That was the shipped
	// assertion and it is a tautology: the handler computes the field from
	// those same two, so it holds against a Compact that never vacuums at
	// all, and it held against the negative figure the WAL-blind before
	// size used to produce. Assert the two properties that can actually
	// be false instead.
	if got.AfterBytes > got.BeforeBytes {
		t.Errorf("the compaction reported GROWTH: before=%d after=%d", got.BeforeBytes, got.AfterBytes)
	}
	if got.ReclaimedBytes < 0 {
		t.Errorf("ReclaimedBytes = %d; a compaction cannot return a negative number of bytes",
			got.ReclaimedBytes)
	}
}

// TestDatabaseCompactSurfacesInsufficientDiskSpace drives the 507 branch,
// which had never executed: newTestServer leaves Deps.DBFreeBytes nil, so
// Compact's headroom check was skipped in every admin test and the
// handler's ErrInsufficientDiskSpace mapping was unreachable.
func TestDatabaseCompactSurfacesInsufficientDiskSpace(t *testing.T) {
	s, _, _ := newTestServer(t)
	s.deps.DBFreeBytes = func(string) (int64, error) { return 1, nil }
	h := s.Handler()

	rr := postCompact(t, h)
	if rr.Code != http.StatusInsufficientStorage {
		t.Fatalf("status = %d, want 507; body=%s", rr.Code, rr.Body.String())
	}
	var env map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env["error"] != "insufficient-disk-space" {
		t.Errorf("error code = %v, want insufficient-disk-space", env["error"])
	}
}

// TestDatabaseCompactRefusedDuringScan pins the guard. A vacuum takes an
// exclusive lock; a scan takes s.mu for every batch flush, so overlapping
// them serialises the scan behind the whole vacuum.
func TestDatabaseCompactRefusedDuringScan(t *testing.T) {
	s, _, _ := newTestServer(t)
	h := s.Handler()

	// Drive the scanner's in-flight counter directly: standing up a real
	// long-running scan to hold the window open is far more fixture than
	// the assertion needs, and the counter IS the predicate.
	s.deps.Scanner.MarkScanInFlightForTests(true)
	defer s.deps.Scanner.MarkScanInFlightForTests(false)

	rr := postCompact(t, h)
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 while a scan is in flight; body=%s", rr.Code, rr.Body.String())
	}
}

// TestDiagnosticsCarriesDatabaseSize pins the visibility half: an
// operator cannot sensibly decide whether to compact a file whose size
// they have never seen.
func TestDiagnosticsCarriesDatabaseSize(t *testing.T) {
	s, _, _ := newTestServer(t)
	h := s.Handler()

	dreq := httptest.NewRequest("GET", "/api/diagnostics", nil)
	dreq.RemoteAddr = "127.0.0.1:54321"
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, dreq)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	v, ok := body["databaseBytes"]
	if !ok {
		t.Fatal("diagnostics response carries no databaseBytes")
	}
	if n, _ := v.(float64); n <= 0 {
		t.Errorf("databaseBytes = %v, want a real file size", v)
	}
	// The FLOOR field, asserted by value rather than by key-presence. The
	// shipped test only checked the key existed, which is why nothing
	// could see that the number was being rendered as an estimate: it has
	// no omitempty, so it is always present whatever it holds.
	fp, ok := body["databaseFreePageBytes"]
	if !ok {
		t.Fatal("diagnostics response carries no databaseFreePageBytes")
	}
	if n, _ := fp.(float64); n < 0 {
		t.Errorf("databaseFreePageBytes = %v; a floor cannot be negative", fp)
	}
	if avail, _ := body["databaseStatsAvailable"].(bool); !avail {
		t.Error("databaseStatsAvailable = false against a healthy store")
	}
}

// TestDiagnosticsSaysWhenTheDatabaseStatsAreUnavailable is the assertion
// its retention twin never had either: RetentionCountsAvailable was only
// ever asserted TRUE, so hoisting the flag out of its error branch left
// every test green while the field's whole reason for existing
// evaporated. Both flags are pinned in the false direction here.
func TestDiagnosticsSaysWhenTheDatabaseStatsAreUnavailable(t *testing.T) {
	s, _, _ := newTestServer(t)
	// A closed store fails every PRAGMA and every COUNT, which is the
	// realistic shape of "the query failed" — and the one that used to
	// render as a bridge with a 0-byte database that had never recorded
	// anything.
	if err := s.deps.Manifest.Close(); err != nil {
		t.Fatal(err)
	}
	snap := s.diagnosticsSnapshot(context.Background())
	if snap.DatabaseStatsAvailable {
		t.Error("DatabaseStatsAvailable = true against a closed store")
	}
	if snap.RetentionCountsAvailable {
		t.Error("RetentionCountsAvailable = true against a closed store")
	}
	if snap.DatabaseBytes != 0 || snap.DatabaseFreePageBytes != 0 {
		t.Errorf("unavailable stats must stay zero, got bytes=%d free=%d",
			snap.DatabaseBytes, snap.DatabaseFreePageBytes)
	}
}

// TestTheConsoleNeverCallsTheFreePageFloorAnEstimate pins the half of the
// fix that lives in the browser, because that is where the wrong answer
// was actually rendered.
//
// databaseFreePageBytes is page_size x freelist_count -- only WHOLLY free
// pages. Scattered deletion, which is what every reaping path here
// produces, leaves intra-page fragmentation and no free pages: measured
// on a 72.5 MB store with every second row deleted, freelist_count was 0
// while a VACUUM returned half the file. The console printed "nothing to
// reclaim", which an operator correctly reads as "do not press this".
//
// A Go test cannot execute the JS, but it can pin the strings that carry
// the claim -- the same shape as this package's other static-asset
// guards. CRLF is normalised first: nothing pins eol in .gitattributes,
// so on a Windows checkout every newline-literal scan here would
// otherwise find nothing and pass vacuously.
func TestTheConsoleNeverCallsTheFreePageFloorAnEstimate(t *testing.T) {
	b, err := staticFS.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := strings.ReplaceAll(string(b), "\r\n", "\n")

	i := strings.Index(js, "function applyDiagnostics(")
	if i < 0 {
		t.Fatal("applyDiagnostics not found in app.js -- the scan is broken")
	}
	body := js[i:]
	if j := strings.Index(body[1:], "\nfunction "); j > 0 {
		body = body[:j+1]
	}
	// COMMENTS ONLY, not stripJSNoise -- that also blanks string literals,
	// and the literals are exactly what this scan is about. Stripping
	// comments is not optional either: the code beside these strings
	// explains the defect BY QUOTING IT, so an unstripped scan finds the
	// commentary and reports the bug as still present. Same trap this
	// repo's CSS guards already carry.
	body = jsBlockCommentRe.ReplaceAllString(body, " ")
	body = jsLineCommentRe.ReplaceAllString(body, " ")
	if !strings.Contains(body, "diag-db-reclaimable") {
		t.Fatal("applyDiagnostics no longer sets diag-db-reclaimable; the scan is looking at the wrong function")
	}

	if strings.Contains(body, "nothing to reclaim") {
		t.Error("app.js still renders \"nothing to reclaim\" for a zero free-page floor. " +
			"That floor reads 0 on a database a compaction would halve, so the string is a " +
			"confident wrong answer about the one number this panel exists to show.")
	}
	if !strings.Contains(body, "at least ") {
		t.Error("app.js does not qualify the free-page figure with \"at least\"; " +
			"it is a floor, not an estimate of what a compaction returns")
	}
	if strings.Contains(body, "databaseReclaimableBytes") {
		t.Error("app.js still reads the old databaseReclaimableBytes field, which no longer exists " +
			"on the wire -- the panel would render 0 for every bridge")
	}
	if !strings.Contains(body, "databaseFreePageBytes") {
		t.Error("app.js does not read databaseFreePageBytes")
	}
	if !strings.Contains(body, "databaseStatsAvailable") {
		t.Error("app.js does not branch on databaseStatsAvailable, so a failed PRAGMA renders as " +
			"a real 0 B reading")
	}
}

// TestDiagnosticsCarriesRetentionCounts pins the visibility half of the
// retention work. The knobs default to off, so the numbers are the only
// thing that turns "keep everything" from an inherited default into a
// decision.
func TestDiagnosticsCarriesRetentionCounts(t *testing.T) {
	s, _, _ := newTestServer(t)
	h := s.Handler()
	ctx := context.Background()

	// Seed one registration and two events, one of them older.
	if err := s.deps.Manifest.UpsertDeviceRegistration(ctx, "dev", "tok", "Phone"); err != nil {
		t.Fatal(err)
	}
	oldest := time.Now().Add(-72 * time.Hour)
	if err := s.deps.Manifest.InsertHistoryBatch(ctx, []manifest.PlaybackHistoryRow{
		{DeviceToken: "dev", Path: "a.flac", StartedAt: oldest.UnixNano(), DurationUsed: 30},
		{DeviceToken: "dev", Path: "b.flac", StartedAt: time.Now().UnixNano(), DurationUsed: 30},
	}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", "/api/diagnostics", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}

	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if got, _ := body["playbackHistoryRows"].(float64); got != 2 {
		t.Errorf("playbackHistoryRows = %v, want 2", body["playbackHistoryRows"])
	}
	if got, _ := body["deviceRegistrationRows"].(float64); got != 1 {
		t.Errorf("deviceRegistrationRows = %v, want 1", body["deviceRegistrationRows"])
	}
	ts, _ := body["oldestPlaybackStartedAt"].(string)
	if ts == "" {
		t.Fatal("oldestPlaybackStartedAt missing; the operator cannot see how far back the table goes")
	}
	parsed, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		t.Fatalf("oldestPlaybackStartedAt %q is not RFC3339: %v", ts, err)
	}
	// RFC3339 formats to SECOND precision, so compare against the truncated
	// value — asserting against a nanosecond-precision time.Now() with a
	// one-second tolerance is a flake waiting for a slow runner, and the
	// Windows leg blocks merges now. (Gemini MEDIUM.)
	if want := oldest.UTC().Truncate(time.Second); !parsed.Equal(want) {
		t.Errorf("oldestPlaybackStartedAt = %v, want %v", parsed, want)
	}

	if avail, _ := body["retentionCountsAvailable"].(bool); !avail {
		t.Error("retentionCountsAvailable is false on a working store; the UI would say 'unavailable'")
	}
}

// An EMPTY history table must omit the timestamp rather than emit a zero
// one — the same trap the enrichmentProgress.lastEnrichedAt pointer
// exists for: a client would parse 0001-01-01 as a real, very old date.
func TestDiagnosticsOmitsTheOldestEventWhenHistoryIsEmpty(t *testing.T) {
	s, _, _ := newTestServer(t)
	req := httptest.NewRequest("GET", "/api/diagnostics", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)

	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if _, present := body["oldestPlaybackStartedAt"]; present {
		t.Errorf("oldestPlaybackStartedAt present on an empty table: %v",
			body["oldestPlaybackStartedAt"])
	}
}
