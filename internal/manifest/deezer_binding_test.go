package manifest

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/dsn"
)

// A pending portrait bound to the artist it was taken for, and the failed
// attempts the store keeps of each artist's portrait by each name (backlog
// B761, its second round).

// v62MarkEnrichedSQL is MarkEnriched's statement as v62's binary runs it
// (3a6b924e): it knows nothing of the pending portrait's columns, so it
// writes an artist over them and leaves them as they are. Binds (tags_json,
// enriched_at, clock, path, version).
const v62MarkEnrichedSQL = `
		UPDATE tracks
		   SET tags_json   = ?,
		       enriched_at = ?,
		       indexed_at  = MAX(?, COALESCE((SELECT MAX(indexed_at) FROM tracks), 0) + 1, COALESCE((SELECT MAX(deleted_at) FROM manifest_deletions), 0) + 1, COALESCE((SELECT CAST(v AS INTEGER) FROM scan_state WHERE k = 'deletion_journal_coverage_start_ns'), 0) + 1)
		 WHERE path = ? AND indexed_at = ?
		RETURNING indexed_at`

// olderWriterGivesAnotherArtist is v62's enricher stamping path with artist
// mbid: v62MarkEnrichedSQL over the row as it stands, through a connection
// of its own on the database at dbPath, as an older binary run against the
// data dir writes it.
func olderWriterGivesAnotherArtist(t *testing.T, dbPath, path, mbid string) {
	t.Helper()
	db, err := sql.Open("sqlite", dsn.File(dbPath, "_pragma=busy_timeout(5000)"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var tags string
	var version int64
	if err := db.QueryRow(`SELECT json_set(tags_json, '$.artistMBID', ?), indexed_at FROM tracks WHERE path = ?`, mbid, path).Scan(&tags, &version); err != nil {
		t.Fatalf("read %s as the older binary: %v", path, err)
	}
	now := time.Now().UnixNano()
	var wrote int64
	if err := db.QueryRow(v62MarkEnrichedSQL, tags, now, now, path, version).Scan(&wrote); err != nil {
		t.Fatalf("the older binary's stamp of %s: %v", path, err)
	}
}

// TestAPendingPortraitIsBoundToTheArtistItWasTakenFor: a stamp keeps a
// pending portrait only for the artist it was taken for. A stamp that gives
// the row another artist and learnt nothing of the portrait drops the
// query; a pending write for the row's artist replaces it, and the batch's
// read pairs it with that artist alone; a pending write for an artist the
// stamp does not give the row writes nothing. On main the stamp kept the
// first artist's name beside the second artist's MBID, which the batch
// asked Deezer by and filed under the second.
func TestAPendingPortraitIsBoundToTheArtistItWasTakenFor(t *testing.T) {
	ctx := context.Background()
	s := openPendingStore(t)
	const p = "Artist/Album/01.flac"
	stampRefused(t, s, p, pendingArtistMBID, "Name A")
	requirePendingRead(t, s, p, pendingArtistMBID, "Name A")

	moved, err := s.GetTrack(ctx, p)
	if err != nil || moved == nil {
		t.Fatalf("GetTrack: %v", err)
	}
	moved.ArtistMBID = pendingOtherMBID
	if err := s.MarkEnriched(ctx, moved); err != nil {
		t.Fatal(err)
	}
	if q, ok := pendingQueryOf(t, s, p); ok {
		t.Fatalf("a stamp giving the row another artist kept the first artist's name %q", q)
	}

	again, err := s.GetTrack(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	again.PortraitPending(pendingOtherMBID, "Name B")
	if err := s.MarkEnriched(ctx, again); err != nil {
		t.Fatal(err)
	}
	requirePendingRead(t, s, p, pendingOtherMBID, "Name B")

	foreign, err := s.GetTrack(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	foreign.PortraitPending(pendingArtistMBID, "Name A")
	if err := s.MarkEnriched(ctx, foreign); err != nil {
		t.Fatal(err)
	}
	requirePendingRead(t, s, p, pendingOtherMBID, "Name B")

	same, err := s.GetTrack(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MarkEnriched(ctx, same); err != nil {
		t.Fatal(err)
	}
	requirePendingRead(t, s, p, pendingOtherMBID, "Name B")
}

// requirePendingRead fails unless the batch's read takes path, paired with
// mbid and query.
func requirePendingRead(t *testing.T, s *Store, path, mbid, query string) {
	t.Helper()
	rows, err := s.PendingPortraits(context.Background(), "", 1000)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Path == path {
			if r.ArtistMBID != mbid || r.Query != query {
				t.Fatalf("the batch reads %s as %s by %q, want %s by %q", path, r.ArtistMBID, r.Query, mbid, query)
			}
			return
		}
	}
	t.Fatalf("the batch does not read %s, want it pending for %s by %q", path, mbid, query)
}

// TestAnOlderBinarysArtistChangeDropsThePendingPortraitAtTheOpen: a row
// stamped with a pending portrait, then given another artist by v62's
// stamp, which keeps the columns it does not know, keeps no query once this
// version opens the store again: neither the batch nor the route pairs the
// first artist's name with the second. A row the older binary did not touch
// keeps its query. On main the reopened store paired them.
func TestAnOlderBinarysArtistChangeDropsThePendingPortraitAtTheOpen(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "bridge.db")
	s, err := OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	const moved, kept = "Artist/Album/01.flac", "Artist/Album/02.flac"
	stampRefused(t, s, moved, pendingArtistMBID, "Name A")
	stampRefused(t, s, kept, pendingArtistMBID, "Name A")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	olderWriterGivesAnotherArtist(t, dbPath, moved, pendingOtherMBID)

	reopened, err := OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if q, ok := pendingQueryOf(t, reopened, moved); ok {
		t.Errorf("the row the older binary gave another artist keeps the first artist's name %q", q)
	}
	rows, err := reopened.PendingPortraits(ctx, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Path != kept || rows[0].ArtistMBID != pendingArtistMBID || rows[0].Query != "Name A" {
		t.Errorf("the batch reads %+v, want only %s, for its own artist by %q", rows, kept, "Name A")
	}
	if pending, err := reopened.ArtistMBIDPortraitPending(ctx, pendingOtherMBID); err != nil || pending {
		t.Errorf("the route's probe for the second artist = %v (%v), want false: nothing waits for its portrait", pending, err)
	}
	if pending, err := reopened.ArtistMBIDPortraitPending(ctx, pendingArtistMBID); err != nil || !pending {
		t.Errorf("the route's probe for the first artist = %v (%v), want true: its untouched row waits", pending, err)
	}
}

// v63Row is a row as v63's binary left it: a query with no artist beside it.
type v63Row struct {
	path, artistMBID, query string
	indexed, enriched       int64
}

// writeV63Store makes the database at dbPath as v63's binary left it, with
// rows, each of whose query names no artist.
func writeV63Store(t *testing.T, dbPath string, rows []v63Row) {
	t.Helper()
	if err := WriteDatabaseAtVersionForTest(dbPath, 63); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dsn.File(dbPath, "_pragma=busy_timeout(5000)"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, r := range rows {
		tags := fmt.Sprintf(`{"path":%q,"artist":"Artist","album":"Album","artistMBID":%q}`, r.path, r.artistMBID)
		var query any
		if r.query != "" {
			query = r.query
		}
		if _, err := db.Exec(`INSERT INTO tracks (path, size, mtime_ns, tags_json, indexed_at, enriched_at, deezer_pending_query)
			VALUES (?, 1, 1, ?, ?, ?, ?)`, r.path, tags, r.indexed, r.enriched, query); err != nil {
			t.Fatal(err)
		}
	}
}

// TestTheQueriesV63KeptUnboundAreDroppedAtTheUpgrade: v63 kept a query with
// no artist beside it, and an older binary may have given its row another
// artist since, so the upgrade binds none of them to the artist the row
// names now, which would certify that mismatch: it drops them all, the row an
// older binary moved and the row it did not alike, and they come back as rows
// enriched before v63 do, through "Retry missing". The step moves no row's
// indexed_at or enriched_at, and a re-run of it keeps a bound query. On main
// the reopened store kept both and paired the moved row's name with its new
// artist.
func TestTheQueriesV63KeptUnboundAreDroppedAtTheUpgrade(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "bridge.db")
	const moved, untouched, legacy = "Moved/Album/01.flac", "Untouched/Album/01.flac", "Legacy/Album/01.flac"
	writeV63Store(t, dbPath, []v63Row{
		{path: moved, artistMBID: pendingArtistMBID, query: "Name A", indexed: 40, enriched: 4},
		{path: untouched, artistMBID: pendingArtistMBID, query: "Name A", indexed: 41, enriched: 5},
		{path: legacy, artistMBID: pendingArtistMBID, indexed: 42, enriched: 6},
	})
	olderWriterGivesAnotherArtist(t, dbPath, moved, pendingOtherMBID)
	var movedIndexed, movedEnriched int64
	{
		db, err := sql.Open("sqlite", dsn.File(dbPath, "_pragma=busy_timeout(5000)"))
		if err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRow(`SELECT indexed_at, enriched_at FROM tracks WHERE path = ?`, moved).Scan(&movedIndexed, &movedEnriched); err != nil {
			t.Fatal(err)
		}
		_ = db.Close()
	}

	s, err := OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	for _, p := range []string{moved, untouched} {
		if q, ok := pendingQueryOf(t, s, p); ok {
			t.Errorf("%s keeps v63's unbound query %q after the upgrade", p, q)
		}
	}
	if rows, err := s.PendingPortraits(ctx, "", 10); err != nil || len(rows) != 0 {
		t.Errorf("the batch reads %+v (%v) after the upgrade, want nothing", rows, err)
	}
	for _, c := range []struct {
		path              string
		indexed, enriched int64
	}{{moved, movedIndexed, movedEnriched}, {untouched, 41, 5}, {legacy, 42, 6}} {
		var indexed, enriched int64
		if err := s.db.QueryRow(`SELECT indexed_at, enriched_at FROM tracks WHERE path = ?`, c.path).Scan(&indexed, &enriched); err != nil {
			t.Fatal(err)
		}
		if indexed != c.indexed || enriched != c.enriched {
			t.Errorf("the upgrade moved %s: indexed_at %d enriched_at %d, want %d and %d", c.path, indexed, enriched, c.indexed, c.enriched)
		}
	}

	fresh := openPendingStore(t)
	stampRefused(t, fresh, "Artist/Album/01.flac", pendingArtistMBID, "Artist")
	rerunMigration(t, fresh, latestLadderVersion())
	requirePendingRead(t, fresh, "Artist/Album/01.flac", pendingArtistMBID, "Artist")
}

// latestLadderVersion is the step the ladder ends at.
func latestLadderVersion() int { return migrations[len(migrations)-1].version }

// rerunMigration runs the ladder's step version over s's database twice, as
// an open interrupted after it and before user_version's write would.
func rerunMigration(t *testing.T, s *Store, version int) {
	t.Helper()
	for _, m := range migrations {
		if m.version != version {
			continue
		}
		for run := 1; run <= 2; run++ {
			if _, err := s.db.Exec(m.sql); err != nil {
				t.Fatalf("run %d of v%d: %v", run, version, err)
			}
			if m.post != nil {
				if err := m.post(s.db); err != nil {
					t.Fatalf("run %d of v%d's post: %v", run, version, err)
				}
			}
		}
	}
}

// TestPortraitAttemptsAreKeptPerArtistAndName: each failed attempt at an
// artist's portrait by a name adds one to that name's count and records
// when, apart from the artist's other names; forgetting a name forgets its
// count alone, forgetting the artist every one of its names; and across a
// reopen the counts a pending row names stay whatever their age, and one no
// row names stays for portraitAttemptsKept past its last failure, a day to
// the nanosecond included, and goes once older (B761's third round: until
// then the reopen forgot it at once).
func TestPortraitAttemptsAreKeptPerArtistAndName(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "bridge.db")
	s, err := OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	first, second := time.Unix(1_800_000_000, 0), time.Unix(1_800_000_060, 0)
	if _, err := s.NotePortraitFailure(ctx, pendingArtistMBID, "Name A", first); err != nil {
		t.Fatal(err)
	}
	got, err := s.NotePortraitFailure(ctx, pendingArtistMBID, "Name A", second)
	if err != nil {
		t.Fatal(err)
	}
	if got.Failures != 2 || !got.LastFailedAt.Equal(second) {
		t.Fatalf("two failures by one name = %+v, want 2, the last at %v", got, second)
	}
	if _, err := s.NotePortraitFailure(ctx, pendingArtistMBID, "Name A2", first); err != nil {
		t.Fatal(err)
	}
	requireAttempts(t, s, pendingArtistMBID, "Name A", 2)
	requireAttempts(t, s, pendingArtistMBID, "Name A2", 1)
	requireAttempts(t, s, pendingOtherMBID, "Name A", 0)

	if err := s.ForgetPortraitAttempts(ctx, pendingArtistMBID, "Name A2"); err != nil {
		t.Fatal(err)
	}
	requireAttempts(t, s, pendingArtistMBID, "Name A2", 0)
	requireAttempts(t, s, pendingArtistMBID, "Name A", 2)
	if _, err := s.NotePortraitFailure(ctx, pendingArtistMBID, "Name A2", first); err != nil {
		t.Fatal(err)
	}
	if err := s.ForgetArtistPortraitAttempts(ctx, pendingArtistMBID); err != nil {
		t.Fatal(err)
	}
	requireAttempts(t, s, pendingArtistMBID, "Name A", 0)
	requireAttempts(t, s, pendingArtistMBID, "Name A2", 0)

	// Across a reopen a day and a nanosecond after second: the count a
	// pending row names stays, older though it is; of those no row names,
	// the one older than a day goes, and the one a day old to the
	// nanosecond and a later one stay.
	stampRefused(t, s, "Artist/Album/01.flac", pendingArtistMBID, "Name A")
	for i := 0; i < 3; i++ {
		if _, err := s.NotePortraitFailure(ctx, pendingArtistMBID, "Name A", second); err != nil {
			t.Fatal(err)
		}
	}
	reopenAt := second.Add(portraitAttemptsKept + time.Nanosecond)
	for _, a := range []struct {
		name string
		at   time.Time
	}{
		{"Nobody Waits", second},
		{"A Day Ago", second.Add(time.Nanosecond)},
		{"Lately", reopenAt.Add(-time.Hour)},
	} {
		if _, err := s.NotePortraitFailure(ctx, pendingOtherMBID, a.name, a.at); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openPortraitStoreAt(t, dbPath, func() time.Time { return reopenAt })
	requireAttempts(t, reopened, pendingArtistMBID, "Name A", 3)
	requireAttempts(t, reopened, pendingOtherMBID, "Nobody Waits", 0)
	requireAttempts(t, reopened, pendingOtherMBID, "A Day Ago", 1)
	requireAttempts(t, reopened, pendingOtherMBID, "Lately", 1)
}

// requireAttempts fails unless the store keeps failures failed attempts at
// mbid's portrait by query.
func requireAttempts(t *testing.T, s *Store, mbid, query string, failures int) {
	t.Helper()
	got, err := s.PortraitAttemptsOf(context.Background(), mbid, query)
	if err != nil {
		t.Fatal(err)
	}
	if got.Failures != failures {
		t.Fatalf("attempts at %s by %q = %+v, want %d failures", mbid, query, got, failures)
	}
}

// TestArtistMBIDPortraitPendingEndsAtTheBound: the route's probe answers
// pending while one of the artist's rows keeps a lookup by a name that has
// failed fewer than PortraitPendingAttempts times, and stops at that bound,
// though the row keeps its query for the batch to ask again; another name of
// the artist below the bound keeps it pending; and the bound survives a
// reopen. On main the probe answered pending for as long as any query was
// kept, which a lookup that keeps failing is, for good.
func TestArtistMBIDPortraitPendingEndsAtTheBound(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "bridge.db")
	s, err := OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	const p = "Artist/Album/01.flac"
	stampRefused(t, s, p, pendingArtistMBID, "Name A")
	requireProbe(t, s, pendingArtistMBID, true, "no failure yet")
	at := time.Unix(1_800_000_000, 0)
	for i := 1; i < PortraitPendingAttempts; i++ {
		if _, err := s.NotePortraitFailure(ctx, pendingArtistMBID, "Name A", at); err != nil {
			t.Fatal(err)
		}
	}
	requireProbe(t, s, pendingArtistMBID, true, fmt.Sprintf("%d failures", PortraitPendingAttempts-1))
	if _, err := s.NotePortraitFailure(ctx, pendingArtistMBID, "Name A", at); err != nil {
		t.Fatal(err)
	}
	requireProbe(t, s, pendingArtistMBID, false, fmt.Sprintf("%d failures", PortraitPendingAttempts))
	if q, ok := pendingQueryOf(t, s, p); !ok || q != "Name A" {
		t.Fatalf("the bound dropped the row's query (%q, %v): the batch still asks by it", q, ok)
	}

	stampRefused(t, s, "Artist/Album/02.flac", pendingArtistMBID, "Name A2")
	requireProbe(t, s, pendingArtistMBID, true, "another name below the bound")
	stampRefused(t, s, "Artist/Album/02.flac", pendingArtistMBID, "Name A")
	requireProbe(t, s, pendingArtistMBID, false, "every name at the bound")

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	requireProbe(t, reopened, pendingArtistMBID, false, "every name at the bound, after a reopen")
}

// requireProbe fails unless the route's probe answers want for mbid.
func requireProbe(t *testing.T, s *Store, mbid string, want bool, when string) {
	t.Helper()
	got, err := s.ArtistMBIDPortraitPending(context.Background(), mbid)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("ArtistMBIDPortraitPending(%s) with %s = %v, want %v", mbid, when, got, want)
	}
}

// TestThePortraitProbeAndTheOpensCheckReadOnlyThePendingRows: the route's
// probe seeks the artist's pending rows in idx_tracks_deezer_pending_artist,
// and the open's check of each pending row's artist reads the pending rows
// alone, through a partial index, never every track.
func TestThePortraitProbeAndTheOpensCheckReadOnlyThePendingRows(t *testing.T) {
	s := openPendingStore(t)
	for i := 0; i < 50; i++ {
		upsertForEnrichment(t, s, Track{Path: fmt.Sprintf("Filler/Album/%02d.flac", i), Artist: "Filler"})
	}
	stampRefused(t, s, "Artist/Album/01.flac", pendingArtistMBID, "Artist")
	if _, err := s.db.Exec(`ANALYZE`); err != nil {
		t.Fatal(err)
	}
	if plan := explainPlan(t, s, artistPortraitPendingSQL, pendingArtistMBID, PortraitPendingAttempts); !strings.Contains(plan, "idx_tracks_deezer_pending_artist") {
		t.Errorf("the route's probe does not seek the artist's pending rows: %s", plan)
	}
	if plan := explainPlan(t, s, strayPendingPortraitsSQL); !strings.Contains(plan, "idx_tracks_deezer_pending") {
		t.Errorf("the open's check does not read the pending rows alone: %s", plan)
	}
}
