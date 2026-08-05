package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// seedDupeFixture stamps two groups (one same-format with a suppressed
// loser, one different-format fully served) straight through the store's
// sanctioned writer — the handler tests exercise the read side, not the
// stamping pass (that's pinned in internal/manifest).
func seedDupeFixture(t *testing.T, store *manifest.Store) {
	t.Helper()
	ctx := context.Background()
	for _, p := range []string{"A/x/01.flac", "B/x/01.flac", "C/y/01.flac", "D/y/01.flac"} {
		tr := &manifest.Track{Path: p, Size: 100, ModTime: time.Unix(0, 0).UTC(), Title: "T"}
		if err := store.UpsertTrack(ctx, tr); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.ApplyDupeStamps(ctx, []manifest.DupeStamp{
		{Path: "A/x/01.flac", GroupID: "g-same", Tier: "same-format", Suppressed: true},
		{Path: "B/x/01.flac", GroupID: "g-same", Tier: "same-format"},
		{Path: "C/y/01.flac", GroupID: "g-diff", Tier: "different-format"},
		{Path: "D/y/01.flac", GroupID: "g-diff", Tier: "different-format"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveDupeSummary(ctx, manifest.DupeSummary{
		SchemaVersion: manifest.DupeSummarySchemaVersion,
		StampedAt:     time.Unix(1000, 0).UTC(),
		Policy:        "highest-quality",
		Scanned:       4, Groups: 2, Suppressed: 1, Served: 3,
		Tiers: []manifest.DupeTierSummary{
			{Tier: "same-format", Groups: 1, RedundantFiles: 1, NonLargestBytes: 100, Suppressed: 1},
			{Tier: "different-format", Groups: 1, RedundantFiles: 1, NonLargestBytes: 100},
		},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestAPIDuplicatesSummary(t *testing.T) {
	srv, _, _ := newTestServer(t)
	store := srv.deps.Manifest
	h := srv.Handler()

	// Fresh install: stamped=false, live policy still reported.
	var empty duplicatesSummaryResponse
	if code := doJSON(t, h, "GET", "/api/duplicates/summary", nil, &empty); code != 200 {
		t.Fatalf("empty summary: %d", code)
	}
	if empty.Stamped || empty.Policy == "" {
		t.Fatalf("fresh install: %+v", empty)
	}

	seedDupeFixture(t, store)
	var got duplicatesSummaryResponse
	if code := doJSON(t, h, "GET", "/api/duplicates/summary", nil, &got); code != 200 {
		t.Fatalf("summary: %d", code)
	}
	if !got.Stamped || got.Groups != 2 || got.Suppressed != 1 || got.Served != 3 {
		t.Fatalf("summary payload: %+v", got)
	}
	if got.StampedPolicy != "highest-quality" || len(got.Tiers) != 2 {
		t.Fatalf("summary tiers/policy: %+v", got)
	}
}

func TestAPIDuplicatesGroups(t *testing.T) {
	srv, _, _ := newTestServer(t)
	store := srv.deps.Manifest
	seedDupeFixture(t, store)
	h := srv.Handler()

	var all duplicatesGroupsResponse
	if code := doJSON(t, h, "GET", "/api/duplicates/groups", nil, &all); code != 200 {
		t.Fatalf("groups: %d", code)
	}
	if len(all.Groups) != 2 {
		t.Fatalf("got %d groups, want 2", len(all.Groups))
	}
	// Members carry the live serving state.
	var sawSuppressed bool
	for _, g := range all.Groups {
		if g.GroupID == "g-same" {
			for _, m := range g.Members {
				if m.Path == "A/x/01.flac" && m.Suppressed {
					sawSuppressed = true
				}
			}
		}
	}
	if !sawSuppressed {
		t.Fatalf("suppressed member state missing: %+v", all.Groups)
	}

	// Tier filter narrows; unknown tier is a 400.
	var diff duplicatesGroupsResponse
	if code := doJSON(t, h, "GET", "/api/duplicates/groups?tier=different-format", nil, &diff); code != 200 {
		t.Fatalf("tier filter: %d", code)
	}
	if len(diff.Groups) != 1 || diff.Groups[0].GroupID != "g-diff" {
		t.Fatalf("tier filter payload: %+v", diff.Groups)
	}
	if code := doJSON(t, h, "GET", "/api/duplicates/groups?tier=bogus", nil, nil); code != http.StatusBadRequest {
		t.Fatalf("bogus tier: got %d, want 400", code)
	}

	// Cursor pages by group id (limit=1 → two pages, then done).
	var p1 duplicatesGroupsResponse
	if code := doJSON(t, h, "GET", "/api/duplicates/groups?limit=1", nil, &p1); code != 200 {
		t.Fatalf("page1: %d", code)
	}
	if len(p1.Groups) != 1 || p1.NextCursor == "" {
		t.Fatalf("page1: %+v", p1)
	}
	var p2 duplicatesGroupsResponse
	if code := doJSON(t, h, "GET", "/api/duplicates/groups?limit=1&cursor="+p1.NextCursor, nil, &p2); code != 200 {
		t.Fatalf("page2: %d", code)
	}
	if len(p2.Groups) != 1 || p2.Groups[0].GroupID == p1.Groups[0].GroupID || p2.NextCursor != "" {
		t.Fatalf("page2: %+v", p2)
	}
}

func TestAPIDuplicatesSweep(t *testing.T) {
	srv, _, _ := newTestServer(t)
	h := srv.Handler()

	// Unwired: 503 (test harness has no sweeper).
	req := httptest.NewRequest("POST", "/api/duplicates/sweep", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	req.Header.Set("Content-Type", "application/json")
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)
	if rw.Code != http.StatusServiceUnavailable {
		t.Fatalf("unwired sweep: %d", rw.Code)
	}

	fired := 0
	srv.deps.TriggerDuplicatesPass = func() bool { fired++; return true }
	rw = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/api/duplicates/sweep", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rw, req)
	if rw.Code != http.StatusAccepted || fired != 1 {
		t.Fatalf("wired sweep: code=%d fired=%d", rw.Code, fired)
	}
}

// TestJobsSnapshotCarriesDuplicatesCard: the /api/jobs aggregate gains
// the duplicates section (policy always present; summary numbers once
// stamped).
func TestJobsSnapshotCarriesDuplicatesCard(t *testing.T) {
	srv, _, _ := newTestServer(t)
	store := srv.deps.Manifest
	seedDupeFixture(t, store)
	var jobs struct {
		Duplicates jobsDuplicates `json:"duplicates"`
	}
	if code := doJSON(t, srv.Handler(), "GET", "/api/jobs", nil, &jobs); code != 200 {
		t.Fatalf("jobs: %d", code)
	}
	if !jobs.Duplicates.Stamped || jobs.Duplicates.Groups != 2 || jobs.Duplicates.Suppressed != 1 {
		t.Fatalf("jobs duplicates card: %+v", jobs.Duplicates)
	}
	if jobs.Duplicates.Policy == "" {
		t.Fatal("jobs duplicates card must always carry the live policy")
	}
}

// TestDuplicatesPageRenders pins the page shell's load-bearing ids and
// the read-only framing (the never-deletes header), plus the subnav
// entry on sibling pages.
func TestDuplicatesPageRenders(t *testing.T) {
	srv, _, _ := newTestServer(t)
	h := srv.Handler()
	req := httptest.NewRequest("GET", "/library/duplicates", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)
	if rw.Code != 200 {
		t.Fatalf("/library/duplicates: %d", rw.Code)
	}
	body := rw.Body.String()
	for _, want := range []string{
		"duplicates-page-root",
		"dupes-policy",
		"dupes-reevaluate",
		"dupes-tier-filter",
		"dupes-groups-list",
		"nothing is ever deleted",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/library/duplicates body missing %q", want)
		}
	}
	// The Library subnav on a sibling page links here.
	req = httptest.NewRequest("GET", "/library", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	rw = httptest.NewRecorder()
	h.ServeHTTP(rw, req)
	if !strings.Contains(rw.Body.String(), "/library/duplicates") {
		t.Error("library subnav missing the Duplicates link")
	}
}

// TestAPIDuplicatesSweepAndSummaryReportScanInFlight pins the feedback
// contract found live on the first production deploy: with a scan
// running, the 202 body and the summary both say so, so the UI can
// explain the deferred nudge instead of looking like a dead button.
func TestAPIDuplicatesSweepAndSummaryReportScanInFlight(t *testing.T) {
	srv, _, _ := newTestServer(t)
	srv.deps.TriggerDuplicatesPass = func() bool { return true }
	h := srv.Handler()

	var sum duplicatesSummaryResponse
	if code := doJSON(t, h, "GET", "/api/duplicates/summary", nil, &sum); code != 200 {
		t.Fatalf("summary: %d", code)
	}
	if sum.ScanInFlight {
		t.Fatal("idle test scanner must report scanInFlight=false")
	}
	var ack struct {
		Triggered    bool `json:"triggered"`
		ScanInFlight bool `json:"scanInFlight"`
	}
	if code := doJSON(t, h, "POST", "/api/duplicates/sweep", nil, &ack); code != 202 {
		t.Fatalf("sweep: %d", code)
	}
	if !ack.Triggered || ack.ScanInFlight {
		t.Fatalf("idle sweep ack: %+v", ack)
	}
}
