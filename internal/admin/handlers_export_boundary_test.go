package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// seedHistoryRows inserts n playback rows in batches, via the batch API.
// Singular inserts each take Store.mu and run their own BEGIN/COMMIT/fsync,
// which under -race is the difference between seconds and minutes.
func seedHistoryRows(t *testing.T, st *manifest.Store, n int) {
	t.Helper()
	ctx := context.Background()
	const chunk = 500
	base := time.Now().Add(-time.Duration(n) * time.Second).UnixNano()
	for start := 0; start < n; start += chunk {
		end := min(start+chunk, n)
		rows := make([]manifest.PlaybackHistoryRow, 0, end-start)
		for i := start; i < end; i++ {
			rows = append(rows, manifest.PlaybackHistoryRow{
				DeviceToken:  "d3adb33fd3adb33fd3adb33fd3adb33f",
				Path:         "Ada/Album/01 Song.flac",
				StartedAt:    base + int64(i)*int64(time.Second),
				DurationUsed: 1,
				Codec:        "FLAC",
			})
		}
		if err := st.InsertHistoryBatch(ctx, rows); err != nil {
			t.Fatalf("seed history: %v", err)
		}
	}
}

func exportBundleOf(t *testing.T, srv *Server) map[string]any {
	t.Helper()
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, loopbackReq("GET", "/api/export", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/export: status = %d (%s)", w.Code, w.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return got
}

// shrinkExportCaps makes the cap boundary reachable in a test. The property
// under test is "a history that ENDS exactly at the cap must not read as
// truncated", and reaching it at the production cap means 100,000 rows through
// SQLite under -race — which would dominate the whole suite for one boundary.
//
// Per-SERVER, not package-level: a package var mutated by a test is a write
// the race detector can pair with a concurrent read from an earlier test's
// still-live httptest server, and buildExport read the cap twice, so a change
// landing between those reads could panic the slice. (Gemini, PR #881.)
//
// The RATIO is what the code depends on, not the magnitudes: the page size
// must not divide the cap, or the loop can never overshoot and the two cases
// become indistinguishable. TestExportPageSizeDoesNotDivideTheCap pins that
// for the shipped values.
func shrinkExportCaps(t *testing.T, srv *Server, capRows, page int) {
	t.Helper()
	srv.testExportCap, srv.testExportPage = capRows, page
}

// TestExportDoesNotClaimTruncationItDidNotDo is the boundary the old form got
// wrong.
//
// The loop condition was checked BEFORE each fetch, pages were 1000, and the
// cap is 100000 — an exact multiple — so len could never exceed the cap and
// the `>= cap` test could not tell "the history ends here" from "the history
// was cut off here". A store holding exactly the cap shipped
// `"truncated": {"playbackHistory": true}` about an export that omitted
// nothing, in the one field whose whole job is honesty about what is missing.
func TestExportDoesNotClaimTruncationItDidNotDo(t *testing.T) {
	srv, _, _ := newTestServer(t)
	const capRows = 10
	shrinkExportCaps(t, srv, capRows, 3)
	seedExportFixture(t, srv.deps.Manifest)
	// The fixture already contributes one play, so seed cap-1 more to land
	// EXACTLY on the cap.
	seedHistoryRows(t, srv.deps.Manifest, capRows-1)

	got := exportBundleOf(t, srv)
	hist, _ := got["playbackHistory"].([]any)
	if len(hist) != capRows {
		t.Fatalf("history has %d rows, want exactly the cap (%d) — the fixture is not on the boundary",
			len(hist), capRows)
	}
	if v, ok := got["truncated"]; ok {
		t.Errorf("a history that ENDS at the cap reported truncated: %v", v)
	}
}

// TestExportReportsTruncationWhenItReallyTruncated is the other side, and the
// reason the test above is not satisfied by simply never setting the flag.
func TestExportReportsTruncationWhenItReallyTruncated(t *testing.T) {
	srv, _, _ := newTestServer(t)
	const capRows = 10
	shrinkExportCaps(t, srv, capRows, 3)
	seedExportFixture(t, srv.deps.Manifest)
	seedHistoryRows(t, srv.deps.Manifest, capRows*2)

	got := exportBundleOf(t, srv)
	hist, _ := got["playbackHistory"].([]any)
	if len(hist) != capRows {
		t.Errorf("history has %d rows, want the cap (%d)", len(hist), capRows)
	}
	tr, ok := got["truncated"].(map[string]any)
	if !ok {
		t.Fatalf("a genuinely truncated history reported no truncation: %v", got["truncated"])
	}
	if tr["playbackHistory"] != true {
		t.Errorf("truncated.playbackHistory = %v, want true", tr["playbackHistory"])
	}
	if n, _ := tr["limit"].(float64); int(n) != capRows {
		t.Errorf("truncated.limit = %v, want %d", tr["limit"], capRows)
	}
}

// TestExportPageSizeDoesNotDivideTheCap pins the arithmetic the test above
// depends on. If the two ever become commensurate the loop can no longer
// overshoot, and "ends at the cap" becomes indistinguishable from "cut off at
// the cap" again — silently, with every test still green.
func TestExportPageSizeDoesNotDivideTheCap(t *testing.T) {
	if exportHistoryPage <= 0 || exportHistoryPage > 1000 {
		t.Fatalf("exportHistoryPage = %d; ListHistory clamps anything over 1000 to 200",
			exportHistoryPage)
	}
	if exportHistoryCap%exportHistoryPage == 0 {
		t.Errorf("exportHistoryCap (%d) is a multiple of exportHistoryPage (%d): the loop can never overshoot, so a history that ENDS at the cap reads as truncated",
			exportHistoryCap, exportHistoryPage)
	}
}

// TestExportSurvivesAnUnsetConfigHolder pins the nil guard. The holder's
// atomic pointer is nil before the first Store, and an unguarded
// Load().LibraryName panicked the handler into a 500.
func TestExportSurvivesAnUnsetConfigHolder(t *testing.T) {
	srv, _, _ := newTestServer(t)
	seedExportFixture(t, srv.deps.Manifest)
	srv.deps.CfgHolder = nil

	got := exportBundleOf(t, srv)
	if _, ok := got["playbackHistory"]; !ok {
		t.Error("the bundle is missing its history block")
	}
}
