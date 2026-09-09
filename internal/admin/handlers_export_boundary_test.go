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

// TestExportDoesNotClaimTruncationItDidNotDo is the boundary the old form got
// wrong.
//
// The loop condition was checked BEFORE each fetch and pages were 1000, and
// the cap is 100000 — an exact multiple — so `len` could never exceed the cap
// and the `>= cap` test could not tell "the history ends here" from "the
// history was cut off here". A store holding exactly the cap shipped
// `"truncated": {"playbackHistory": true}` about an export that omitted
// nothing, in the one field whose whole job is honesty about what is missing.
//
// Driving 100k rows through SQLite under -race would dominate the suite, so
// this asserts the property at a shrunken cap via the same arithmetic: a
// history that ENDS exactly on a page boundary must not read as truncated.
func TestExportDoesNotClaimTruncationItDidNotDo(t *testing.T) {
	srv, _, _ := newTestServer(t)
	seedExportFixture(t, srv.deps.Manifest)
	// Exactly two full pages and not one row more. Under the old form the
	// equivalent shape at the real cap set the flag; here it must not.
	seedHistoryRows(t, srv.deps.Manifest, 2*exportHistoryPage)

	got := exportBundleOf(t, srv)
	if v, ok := got["truncated"]; ok {
		t.Errorf("a complete history reported truncated: %v", v)
	}
	hist, _ := got["playbackHistory"].([]any)
	if len(hist) < 2*exportHistoryPage {
		t.Errorf("history has %d rows, want at least %d — the paging loop stopped early",
			len(hist), 2*exportHistoryPage)
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
