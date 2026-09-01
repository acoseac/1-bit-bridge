package manifest

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func searchStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if ok, _ := s.SearchAvailable(context.Background()); !ok {
		t.Skip("FTS5 not compiled into this driver build; the assertions would be vacuous")
	}
	return s
}

func addSearchTrack(t *testing.T, s *Store, path, title string) {
	t.Helper()
	if err := s.UpsertTrack(context.Background(), &Track{
		Path: path, Title: title, Artist: "Aphex Twin", Album: "Selected Ambient Works",
		Size: 1000, ModTime: time.Unix(1, 0),
	}); err != nil {
		t.Fatal(err)
	}
}

// suppress marks a path duplicate-suppressed the way the stamping pass
// does, so the fixture is the real column rather than a stand-in.
func suppress(t *testing.T, s *Store, path string) {
	t.Helper()
	if _, err := s.db.Exec(`UPDATE tracks SET dupe_suppressed = 1 WHERE path = ?`, path); err != nil {
		t.Fatal(err)
	}
}

// TestSearchServedTracksExcludesSuppressedCopies is the assertion this
// whole endpoint lives or dies on. tracks_fts is trigger-populated from
// tracks, so it contains duplicate-suppressed rows — copies /v1/manifest
// deliberately withholds. Serving them would contradict every count
// beside them.
//
// Negative-controlled against the UNJOINED query, which is what the
// admin path (correctly) runs: it returns both copies.
func TestSearchServedTracksExcludesSuppressedCopies(t *testing.T) {
	s := searchStore(t)
	ctx := context.Background()
	addSearchTrack(t, s, "Lossless/Xtal.flac", "Xtal")
	addSearchTrack(t, s, "Backup/Xtal.flac", "Xtal")
	suppress(t, s, "Backup/Xtal.flac")

	// Precondition: the UNRESTRICTED search still sees both, so the
	// assertion below is about the restriction and not about the fixture
	// having failed to index.
	all, err := s.SearchTracks(ctx, "Xtal", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("precondition: unrestricted search returned %d hits, want 2 — the fixture did not index",
			len(all))
	}

	served, err := s.SearchServedTracks(ctx, "Xtal", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(served) != 1 {
		t.Fatalf("served search returned %d hits, want 1; suppressed copies must not reach /v1\ngot: %+v",
			len(served), served)
	}
	if served[0].Path != "Lossless/Xtal.flac" {
		t.Errorf("served hit = %q, want the unsuppressed copy", served[0].Path)
	}
}

// TestSearchServedTracksUsesTheIndexedPlan pins the query plan. Review of
// the plan warned the join could trick SQLite into a full table scan;
// measured, it does not — tracks.path is the PRIMARY KEY and the MATCH
// forces the virtual table to be the outer loop. Three lines of test that
// keep a future rewrite from quietly losing that.
func TestSearchServedTracksUsesTheIndexedPlan(t *testing.T) {
	s := searchStore(t)
	for i := 0; i < 500; i++ {
		addSearchTrack(t, s, fmt.Sprintf("A%03d/Track.flac", i), fmt.Sprintf("Song Title Number %d", i))
	}

	rows, err := s.db.Query("EXPLAIN QUERY PLAN "+servedSearchSQL, `"song"*`, 50)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var a, b, c int
		var detail string
		if err := rows.Scan(&a, &b, &c, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	joined := strings.Join(plan, " | ")

	// The MATCH must drive.
	if !strings.Contains(joined, "VIRTUAL TABLE INDEX") {
		t.Errorf("the FTS match is not driving the plan:\n%s", joined)
	}
	// The join side must be a seek, not a scan.
	if !strings.Contains(joined, "SEARCH t") {
		t.Errorf("the tracks join is not an index seek:\n%s", joined)
	}
	if strings.Contains(joined, "SCAN t") {
		t.Errorf("the tracks table is being SCANNED — the join fell off the primary key:\n%s", joined)
	}
	t.Logf("plan: %s", joined)
}

// TestSearchServedTracksPreservesRankOrder — ORDER BY rank must survive
// the join. BM25 puts the closer title first; a plan change that dropped
// the ordering would be invisible without this.
func TestSearchServedTracksPreservesRankOrder(t *testing.T) {
	s := searchStore(t)
	ctx := context.Background()
	addSearchTrack(t, s, "A/Ambient.flac", "Ambient")
	addSearchTrack(t, s, "B/Other.flac", "Ambient Ambient Ambient")

	hits, err := s.SearchServedTracks(ctx, "Ambient", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 {
		t.Fatalf("got %d hits, want 2", len(hits))
	}
	// Whatever BM25 decides, the ordering must be STABLE and derived from
	// rank rather than from insertion order — assert it matches the
	// unrestricted search's ordering for the same rows.
	all, err := s.SearchTracks(ctx, "Ambient", 50)
	if err != nil {
		t.Fatal(err)
	}
	for i := range hits {
		if hits[i].Path != all[i].Path {
			t.Errorf("served ordering diverges from the unrestricted ordering at %d: %q vs %q",
				i, hits[i].Path, all[i].Path)
		}
	}
}

func TestSearchServedTracksSanitisesAndBounds(t *testing.T) {
	s := searchStore(t)
	ctx := context.Background()
	addSearchTrack(t, s, "A/Track.flac", "Hello World")

	// FTS5 operators cannot reach the MATCH — buildFTSMatchExpr strips
	// every non-alphanumeric rune. These must not error.
	for _, q := range []string{
		`"`, `NEAR(a b)`, `a OR b`, `a AND b`, `a NOT b`, `*`, `^a`, `a:b`, `((((`, `"unclosed`,
	} {
		if _, err := s.SearchServedTracks(ctx, q, 10); err != nil {
			t.Errorf("SearchServedTracks(%q) errored: %v — sanitisation should make this impossible", q, err)
		}
	}

	// Empty after sanitisation is neither an error nor a hit.
	hits, err := s.SearchServedTracks(ctx, "!!!", 10)
	if err != nil || hits != nil {
		t.Errorf("punctuation-only query: hits=%v err=%v; want (nil, nil)", hits, err)
	}

	// The hard cap holds regardless of what the caller asks for.
	for i := 0; i < 20; i++ {
		addSearchTrack(t, s, fmt.Sprintf("Z%03d/Hello.flac", i), "Hello There")
	}
	got, err := s.SearchServedTracks(ctx, "Hello", 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) > searchHardCap {
		t.Errorf("returned %d hits with limit=1000000; the hard cap is %d", len(got), searchHardCap)
	}
}
