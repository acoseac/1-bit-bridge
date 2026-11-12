package manifest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/dsn"
)

// The portrait the Deezer switch refused (backlog B761):
// tracks.deezer_pending_query, which MarkEnriched writes in the stamp's own
// statement, which a changed file's upsert drops with the rest of the
// enrichment, and which the enricher's portrait batch reads and clears only
// at the version it read.

const (
	pendingArtistMBID = "aaaaaaaa-0000-4000-8000-000000000761"
	pendingOtherMBID  = "bbbbbbbb-0000-4000-8000-000000000761"
)

// pendingQueryOf is the row's deezer_pending_query, and whether it holds one.
func pendingQueryOf(t *testing.T, s *Store, path string) (string, bool) {
	t.Helper()
	var q sql.NullString
	if err := s.db.QueryRow(`SELECT deezer_pending_query FROM tracks WHERE path = ?`, path).Scan(&q); err != nil {
		t.Fatalf("read the pending query of %s: %v", path, err)
	}
	return q.String, q.Valid
}

// openPendingStore is a store of its own, closed by the test's cleanup.
func openPendingStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// upsertForEnrichment stores tr and returns it as the store handed it back,
// carrying the version a stamp compares.
func upsertForEnrichment(t *testing.T, s *Store, tr Track) *Track {
	t.Helper()
	if tr.Size == 0 {
		tr.Size = 1
	}
	if tr.ModTime.IsZero() {
		tr.ModTime = time.Unix(1_700_000_000, 0)
	}
	if err := s.UpsertTrack(context.Background(), &tr); err != nil {
		t.Fatal(err)
	}
	return &tr
}

// stampRefused enriches path with artist mbid and a portrait the switch
// refused, asked by query.
func stampRefused(t *testing.T, s *Store, path, mbid, query string) {
	t.Helper()
	tr := upsertForEnrichment(t, s, Track{Path: path, Artist: "Artist", Album: "Album"})
	tr.ArtistMBID = mbid
	tr.PortraitPending(mbid, query)
	if err := s.MarkEnriched(context.Background(), tr); err != nil {
		t.Fatal(err)
	}
}

// TestMigrationV63KeepsAPortraitTheSwitchRefused: a store at v62, holding a
// row enriched before the column, migrates to the column and its partial
// index, and the row carries no query (nothing says its portrait was refused:
// "Retry missing" stays its way); a fresh store has both; and a re-run of
// v63's step over a store that holds a query keeps it.
func TestMigrationV63KeepsAPortraitTheSwitchRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v62.db")
	db, err := sql.Open("sqlite", dsn.File(path, "_pragma=journal_mode(WAL)"))
	if err != nil {
		t.Fatal(err)
	}
	const head = 62
	for _, m := range migrations {
		if m.version > head {
			continue
		}
		if _, err := db.Exec(m.sql); err != nil {
			t.Fatalf("apply migration %d (%s): %v", m.version, m.name, err)
		}
		if m.post != nil {
			if err := m.post(db); err != nil {
				t.Fatalf("migration %d (%s) post: %v", m.version, m.name, err)
			}
		}
	}
	tags := fmt.Sprintf(`{"path":"Legacy/Album/01.flac","artist":"Legacy","artistMBID":%q}`, pendingArtistMBID)
	if _, err := db.Exec(`INSERT INTO tracks (path, size, mtime_ns, tags_json, indexed_at, enriched_at)
		VALUES ('Legacy/Album/01.flac', 1, 1, ?, 42, 7)`, tags); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, head)); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if v := readUserVersion(t, s.db); v < 63 {
		t.Fatalf("user_version %d after the open, want 63 at least", v)
	}
	if q, ok := pendingQueryOf(t, s, "Legacy/Album/01.flac"); ok {
		t.Errorf("a row enriched before v63 carries the query %q: nothing says its portrait was refused", q)
	}
	var indexed, enriched int64
	if err := s.db.QueryRow(`SELECT indexed_at, enriched_at FROM tracks WHERE path = 'Legacy/Album/01.flac'`).Scan(&indexed, &enriched); err != nil {
		t.Fatal(err)
	}
	if indexed != 42 || enriched != 7 {
		t.Errorf("the migration moved the legacy row: indexed_at %d enriched_at %d, want 42 and 7", indexed, enriched)
	}
	requirePendingIndex(t, s)

	fresh := openPendingStore(t)
	requirePendingIndex(t, fresh)
	stampRefused(t, fresh, "Artist/Album/01.flac", pendingArtistMBID, "Artist")
	for _, m := range migrations {
		if m.version != 63 {
			continue
		}
		for run := 1; run <= 2; run++ {
			if _, err := fresh.db.Exec(m.sql); err != nil {
				t.Fatalf("run %d of v63: %v", run, err)
			}
			if m.post != nil {
				if err := m.post(fresh.db); err != nil {
					t.Fatalf("run %d of v63's post: %v", run, err)
				}
			}
		}
	}
	if q, ok := pendingQueryOf(t, fresh, "Artist/Album/01.flac"); !ok || q != "Artist" {
		t.Errorf("a re-run of v63 left the query %q (%v), want %q kept", q, ok, "Artist")
	}
}

// requirePendingIndex fails unless the store holds the partial index on the
// rows that carry a query.
func requirePendingIndex(t *testing.T, s *Store) {
	t.Helper()
	var def string
	if err := s.db.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'index' AND name = 'idx_tracks_deezer_pending'`).Scan(&def); err != nil {
		t.Fatalf("idx_tracks_deezer_pending: %v", err)
	}
	if !strings.Contains(def, "deezer_pending_query IS NOT NULL") {
		t.Errorf("idx_tracks_deezer_pending is %s, want it partial on the rows that carry a query", def)
	}
}

// TestAStampWritesThePortraitItsPassLearnt: MarkEnriched writes the query in
// the stamp's own statement when the pass's portrait lookup waits (refused,
// or deferred), clears it when the pass settled the portrait (cached,
// fetched, or Deezer has none), and leaves it as it was when the pass learnt
// nothing of the portrait for the row's artist; a pending lookup with no
// artist MBID writes none, and a stamp over a row that changed writes nothing
// at all.
func TestAStampWritesThePortraitItsPassLearnt(t *testing.T) {
	ctx := context.Background()
	s := openPendingStore(t)
	const p = "Artist/Album/01.flac"
	stampRefused(t, s, p, pendingArtistMBID, "The Artist")
	if q, ok := pendingQueryOf(t, s, p); !ok || q != "The Artist" {
		t.Fatalf("a refused portrait's stamp left the query %q (%v), want %q", q, ok, "The Artist")
	}

	// A pass that learnt nothing of the portrait leaves it.
	again, err := s.GetTrack(ctx, p)
	if err != nil || again == nil {
		t.Fatalf("GetTrack: %v", err)
	}
	if err := s.MarkEnriched(ctx, again); err != nil {
		t.Fatal(err)
	}
	if q, ok := pendingQueryOf(t, s, p); !ok || q != "The Artist" {
		t.Errorf("a stamp that learnt nothing of the portrait left %q (%v), want it kept", q, ok)
	}

	// A lookup that waits again replaces the name; a settled one clears it.
	again.PortraitPending(pendingArtistMBID, "Another Name")
	if err := s.MarkEnriched(ctx, again); err != nil {
		t.Fatal(err)
	}
	if q, _ := pendingQueryOf(t, s, p); q != "Another Name" {
		t.Errorf("a second refusal left %q, want %q", q, "Another Name")
	}
	settled, err := s.GetTrack(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	settled.PortraitSettled()
	if err := s.MarkEnriched(ctx, settled); err != nil {
		t.Fatal(err)
	}
	if q, ok := pendingQueryOf(t, s, p); ok {
		t.Errorf("a settled portrait's stamp left the query %q", q)
	}

	// No artist MBID, no query: the portrait batch asks by an artist's MBID.
	bare := upsertForEnrichment(t, s, Track{Path: "Nobody/Album/01.flac", Artist: "Nobody"})
	bare.PortraitPending(bare.ArtistMBID, "Nobody")
	if err := s.MarkEnriched(ctx, bare); err != nil {
		t.Fatal(err)
	}
	if q, ok := pendingQueryOf(t, s, "Nobody/Album/01.flac"); ok {
		t.Errorf("a row with no artist MBID was given the query %q", q)
	}

	// A stamp over a row that changed since it was read writes no query.
	stale, err := s.GetTrack(ctx, "Nobody/Album/01.flac")
	if err != nil {
		t.Fatal(err)
	}
	upsertForEnrichment(t, s, Track{Path: "Nobody/Album/01.flac", Artist: "Nobody", Size: 2})
	stale.ArtistMBID = pendingOtherMBID
	stale.PortraitPending(pendingOtherMBID, "Nobody")
	if err := s.MarkEnriched(ctx, stale); !errors.Is(err, ErrTrackChanged) {
		t.Fatalf("a stamp over a changed row answered %v, want ErrTrackChanged", err)
	}
	if q, ok := pendingQueryOf(t, s, "Nobody/Album/01.flac"); ok {
		t.Errorf("a refused stamp over a changed row wrote the query %q", q)
	}
}

// TestRetryMissingKeepsAPendingPortrait: each of "Retry missing"'s resets
// re-queues the row and leaves its query, so a re-enrichment whose portrait
// attempt fails transiently leaves it for the portrait batch.
func TestRetryMissingKeepsAPendingPortrait(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name  string
		reset func(s *Store) (int64, error)
	}{
		{"ResetEnrichedMisses", func(s *Store) (int64, error) { return s.ResetEnrichedMisses(ctx) }},
		{"ResetEnrichedByArtistMBIDs", func(s *Store) (int64, error) {
			return s.ResetEnrichedByArtistMBIDs(ctx, []string{pendingArtistMBID})
		}},
		{"ResetEnrichedMissesUnderPrefix", func(s *Store) (int64, error) {
			return s.ResetEnrichedMissesUnderPrefix(ctx, "Artist")
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := openPendingStore(t)
			const p = "Artist/Album/01.flac"
			stampRefused(t, s, p, pendingArtistMBID, "Artist")
			n, err := c.reset(s)
			if err != nil || n != 1 {
				t.Fatalf("the reset re-queued %d (%v), want the one row", n, err)
			}
			if _, ok := unenrichedPathsOf(t, s)[p]; !ok {
				t.Fatalf("%s is not back in the queue", p)
			}
			if q, ok := pendingQueryOf(t, s, p); !ok || q != "Artist" {
				t.Errorf("the reset left the query %q (%v), want it kept", q, ok)
			}
		})
	}
}

// unenrichedPathsOf is the paths waiting for the enricher.
func unenrichedPathsOf(t *testing.T, s *Store) map[string]struct{} {
	t.Helper()
	rows, err := s.UnenrichedTracks(context.Background(), 1000)
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[string]struct{}, len(rows))
	for _, r := range rows {
		out[r.Path] = struct{}{}
	}
	return out
}

// TestAChangedFileDropsThePendingPortraitAndAnUnchangedScanKeepsIt: through
// the real scanner, a rescan of an unchanged file and the version-stale stamp
// leg keep the query; a file that changed is re-read, its row upserted with
// its enrichment reset, and its query dropped with it.
func TestAChangedFileDropsThePendingPortraitAndAnUnchangedScanKeepsIt(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	file := filepath.Join(root, "Artist", "Album", "01.flac")
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	writeMinimalFLAC(t, file, 44100, 16, map[string]string{"ARTIST": "Artist", "ALBUM": "Album", "TITLE": "One"})
	s := openPendingStore(t)
	sc := NewScanner([]string{root}, s, "")
	scanOnce(t, sc, "first scan")
	const p = "Artist/Album/01.flac"
	tr, err := s.GetTrack(ctx, p)
	if err != nil || tr == nil {
		t.Fatalf("GetTrack: %v", err)
	}
	tr.ArtistMBID = pendingArtistMBID
	tr.PortraitPending(pendingArtistMBID, "Artist")
	if err := s.MarkEnriched(ctx, tr); err != nil {
		t.Fatal(err)
	}

	scanOnce(t, sc, "unchanged rescan")
	if q, ok := pendingQueryOf(t, s, p); !ok || q != "Artist" {
		t.Errorf("a rescan of an unchanged file left %q (%v), want the query kept", q, ok)
	}
	if err := s.StampExtractorVersionBatch(ctx, []*Track{{Path: p}}); err != nil {
		t.Fatal(err)
	}
	if q, ok := pendingQueryOf(t, s, p); !ok || q != "Artist" {
		t.Errorf("the version-stale stamp leg left %q (%v), want the query kept", q, ok)
	}

	writeMinimalFLAC(t, file, 44100, 16, map[string]string{"ARTIST": "Artist", "ALBUM": "Album", "TITLE": "One, retagged"})
	later := time.Now().Add(time.Minute)
	if err := os.Chtimes(file, later, later); err != nil {
		t.Fatal(err)
	}
	scanOnce(t, sc, "rescan of the changed file")
	if _, ok := unenrichedPathsOf(t, s)[p]; !ok {
		t.Fatalf("the changed file's row was not reset for the enricher")
	}
	if q, ok := pendingQueryOf(t, s, p); ok {
		t.Errorf("a changed file's upsert kept the query %q: it goes with the rest of the enrichment", q)
	}
}

// TestPendingPortraitsAreTheEnrichedRowsThatCarryAQuery: the portrait batch's
// read takes, in path order from after its cursor and at most its limit, the
// enriched rows that carry a query, each with its artist MBID and its query,
// and never a row that waits for the enricher (whose own pass asks for the
// portrait) or one that carries none.
func TestPendingPortraitsAreTheEnrichedRowsThatCarryAQuery(t *testing.T) {
	ctx := context.Background()
	s := openPendingStore(t)
	stampRefused(t, s, "B/Album/01.flac", pendingOtherMBID, "B")
	stampRefused(t, s, "A/Album/01.flac", pendingArtistMBID, "A")
	stampRefused(t, s, "A/Album/02.flac", pendingArtistMBID, "A")
	stampRefused(t, s, "Queued/Album/01.flac", pendingArtistMBID, "A")
	if _, err := s.ResetEnrichedByPaths(ctx, []AcoustIDMatch{{Path: "Queued/Album/01.flac"}}); err != nil {
		t.Fatal(err)
	}
	none := upsertForEnrichment(t, s, Track{Path: "C/Album/01.flac", Artist: "C"})
	none.ArtistMBID = pendingOtherMBID
	if err := s.MarkEnriched(ctx, none); err != nil {
		t.Fatal(err)
	}

	got, err := s.PendingPortraits(ctx, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, r := range got {
		paths = append(paths, r.Path)
	}
	want := []string{"A/Album/01.flac", "A/Album/02.flac", "B/Album/01.flac"}
	if strings.Join(paths, ",") != strings.Join(want, ",") {
		t.Fatalf("PendingPortraits = %v, want %v", paths, want)
	}
	if got[0].ArtistMBID != pendingArtistMBID || got[0].Query != "A" || got[2].ArtistMBID != pendingOtherMBID || got[2].Query != "B" {
		t.Errorf("rows %+v, want each with its artist MBID and its query", got)
	}

	page, err := s.PendingPortraits(ctx, "A/Album/01.flac", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 1 || page[0].Path != "A/Album/02.flac" {
		t.Errorf("a page after A/Album/01.flac of one row = %+v, want A/Album/02.flac", page)
	}
	rest, err := s.PendingPortraits(ctx, "B/Album/01.flac", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rest) != 0 {
		t.Errorf("a page after the last row = %+v, want none", rest)
	}
}

// TestAPendingPortraitIsClearedOnlyAtTheVersionItWasRead: the clear takes a
// row only while it is the row the batch read: one the scanner rewrote, or
// deleted and inserted again, and enriched again with a query of its own,
// keeps that query.
func TestAPendingPortraitIsClearedOnlyAtTheVersionItWasRead(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name    string
		replace func(t *testing.T, s *Store, p string)
	}{
		{"retagged", func(t *testing.T, s *Store, p string) {
			upsertForEnrichment(t, s, Track{Path: p, Artist: "Artist", Album: "Album", Title: "Retagged", Size: 2})
		}},
		{"deleted and inserted again", func(t *testing.T, s *Store, p string) {
			if err := s.DeleteTrack(ctx, p); err != nil {
				t.Fatal(err)
			}
			upsertForEnrichment(t, s, Track{Path: p, Artist: "Artist", Album: "Album"})
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := openPendingStore(t)
			const p = "Artist/Album/01.flac"
			stampRefused(t, s, p, pendingArtistMBID, "Artist")
			read, err := s.PendingPortraits(ctx, "", 10)
			if err != nil || len(read) != 1 {
				t.Fatalf("PendingPortraits = %+v (%v)", read, err)
			}

			c.replace(t, s, p)
			again, err := s.GetTrack(ctx, p)
			if err != nil || again == nil {
				t.Fatalf("GetTrack: %v", err)
			}
			again.ArtistMBID = pendingArtistMBID
			again.PortraitPending(pendingArtistMBID, "Artist")
			if err := s.MarkEnriched(ctx, again); err != nil {
				t.Fatal(err)
			}

			n, err := s.ClearPendingPortraits(ctx, read)
			if err != nil {
				t.Fatal(err)
			}
			if n != 0 {
				t.Errorf("the stale clear took %d row(s), want none", n)
			}
			if q, ok := pendingQueryOf(t, s, p); !ok || q != "Artist" {
				t.Errorf("the replacement's query is %q (%v) after a stale clear, want it kept", q, ok)
			}

			current, err := s.PendingPortraits(ctx, "", 10)
			if err != nil || len(current) != 1 {
				t.Fatalf("PendingPortraits = %+v (%v)", current, err)
			}
			if n, err := s.ClearPendingPortraits(ctx, current); err != nil || n != 1 {
				t.Fatalf("a clear at the row's version took %d (%v), want 1", n, err)
			}
			if q, ok := pendingQueryOf(t, s, p); ok {
				t.Errorf("a clear at the row's version left %q", q)
			}
		})
	}
}

// TestAClearMovesNoDeltaCursor: a cleared query changes nothing a client
// reads, so the clear moves no row's indexed_at and leaves enriched_at.
func TestAClearMovesNoDeltaCursor(t *testing.T) {
	ctx := context.Background()
	s := openPendingStore(t)
	const p = "Artist/Album/01.flac"
	stampRefused(t, s, p, pendingArtistMBID, "Artist")
	var indexed, enriched int64
	if err := s.db.QueryRow(`SELECT indexed_at, enriched_at FROM tracks WHERE path = ?`, p).Scan(&indexed, &enriched); err != nil {
		t.Fatal(err)
	}
	rows, err := s.PendingPortraits(ctx, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := s.ClearPendingPortraits(ctx, rows); err != nil || n != 1 {
		t.Fatalf("clear took %d (%v)", n, err)
	}
	var indexed2, enriched2 int64
	if err := s.db.QueryRow(`SELECT indexed_at, enriched_at FROM tracks WHERE path = ?`, p).Scan(&indexed2, &enriched2); err != nil {
		t.Fatal(err)
	}
	if indexed2 != indexed || enriched2 != enriched {
		t.Errorf("the clear moved indexed_at %d -> %d, enriched_at %d -> %d", indexed, indexed2, enriched, enriched2)
	}
}

// TestThePendingPortraitReadUsesItsPartialIndex: the batch's read reaches its
// rows through idx_tracks_deezer_pending (the predicate carries the index's
// WHERE verbatim), and a full scan of tracks is what its absence costs.
func TestThePendingPortraitReadUsesItsPartialIndex(t *testing.T) {
	s := openPendingStore(t)
	for i := 0; i < 50; i++ {
		upsertForEnrichment(t, s, Track{Path: fmt.Sprintf("Filler/Album/%02d.flac", i), Artist: "Filler"})
	}
	stampRefused(t, s, "Artist/Album/01.flac", pendingArtistMBID, "Artist")
	if _, err := s.db.Exec(`ANALYZE`); err != nil {
		t.Fatal(err)
	}
	plan := explainPlan(t, s, pendingPortraitsSQL, "", 10)
	if !strings.Contains(plan, "idx_tracks_deezer_pending") {
		t.Errorf("the pending-portrait read does not use its partial index: %s", plan)
	}
}

// TestArtistMBIDPortraitPendingIsPerArtist: the route's probe answers whether
// any of an artist's rows carries a query.
func TestArtistMBIDPortraitPendingIsPerArtist(t *testing.T) {
	ctx := context.Background()
	s := openPendingStore(t)
	stampRefused(t, s, "Artist/Album/01.flac", pendingArtistMBID, "Artist")
	settled := upsertForEnrichment(t, s, Track{Path: "Artist/Album/02.flac", Artist: "Artist"})
	settled.ArtistMBID = pendingArtistMBID
	if err := s.MarkEnriched(ctx, settled); err != nil {
		t.Fatal(err)
	}
	other := upsertForEnrichment(t, s, Track{Path: "Other/Album/01.flac", Artist: "Other"})
	other.ArtistMBID = pendingOtherMBID
	if err := s.MarkEnriched(ctx, other); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		mbid string
		want bool
	}{{pendingArtistMBID, true}, {pendingOtherMBID, false}, {"cccccccc-0000-4000-8000-000000000761", false}, {"", false}} {
		got, err := s.ArtistMBIDPortraitPending(ctx, c.mbid)
		if err != nil || got != c.want {
			t.Errorf("ArtistMBIDPortraitPending(%q) = %v, %v; want %v", c.mbid, got, err, c.want)
		}
	}
}
