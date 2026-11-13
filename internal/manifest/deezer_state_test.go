package manifest

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/updater"
)

// What a stamp keeps of a portrait lookup that waits by one name when its
// pass ended another, and what an open keeps of the failed attempts no
// waiting row names any more (backlog B761, its third round).

// openPortraitStoreAt opens the store at dbPath on the clock now, which its
// writes and the open's check of the pending portraits read, and closes it in
// the test's cleanup.
func openPortraitStoreAt(t *testing.T, dbPath string, now func() time.Time) *Store {
	t.Helper()
	s, err := openStoreWithClock(dbPath, updater.MigrationPermit{}, now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestAStampClearsOnlyTheLookupOfTheNameItsPassEnded: a row waits for its
// artist's portrait by one name, and "Retry missing" hands it to a pass that
// reads it afresh, knowing nothing of that name, and whose answer ends
// another name of the same artist: the stamp keeps the row's lookup. A stamp
// whose pass ended the row's own name clears it, and so does one whose pass
// holds the portrait; a lookup kept for an artist the stamp no longer gives
// the row is dropped, whatever the pass ended. Until B761's third round the
// stamp of any name an answer ended cleared the row's lookup, whatever its
// name, and the batch never asked by it again.
func TestAStampClearsOnlyTheLookupOfTheNameItsPassEnded(t *testing.T) {
	ctx := context.Background()
	s := openPendingStore(t)
	const p = "Artist/Album/01.flac"
	stampRefused(t, s, p, pendingArtistMBID, "Name A")
	if n, err := s.ResetEnrichedByArtistMBIDs(ctx, []string{pendingArtistMBID}); err != nil || n != 1 {
		t.Fatalf("Retry missing re-queued %d (%v), want the row", n, err)
	}
	rows, err := s.UnenrichedTracks(ctx, 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("UnenrichedTracks = %d rows (%v), want the one", len(rows), err)
	}
	retried := rows[0]
	retried.PortraitNameEnded(pendingArtistMBID, "Name B")
	if err := s.MarkEnriched(ctx, &retried); err != nil {
		t.Fatal(err)
	}
	requirePendingRead(t, s, p, pendingArtistMBID, "Name A")

	own, err := s.GetTrack(ctx, p)
	if err != nil || own == nil {
		t.Fatalf("GetTrack: %v", err)
	}
	own.PortraitNameEnded(pendingArtistMBID, "Name B")
	own.PortraitNameEnded(pendingArtistMBID, "Name A")
	if err := s.MarkEnriched(ctx, own); err != nil {
		t.Fatal(err)
	}
	if q, ok := pendingQueryOf(t, s, p); ok {
		t.Errorf("a stamp whose pass ended the row's own name kept its lookup by %q", q)
	}

	const held = "Artist/Album/02.flac"
	stampRefused(t, s, held, pendingArtistMBID, "Name A")
	h, err := s.GetTrack(ctx, held)
	if err != nil || h == nil {
		t.Fatalf("GetTrack: %v", err)
	}
	h.PortraitSettled()
	if err := s.MarkEnriched(ctx, h); err != nil {
		t.Fatal(err)
	}
	if q, ok := pendingQueryOf(t, s, held); ok {
		t.Errorf("a stamp whose pass holds the portrait kept the lookup by %q", q)
	}

	const moved = "Artist/Album/03.flac"
	stampRefused(t, s, moved, pendingArtistMBID, "Name A")
	m, err := s.GetTrack(ctx, moved)
	if err != nil || m == nil {
		t.Fatalf("GetTrack: %v", err)
	}
	m.ArtistMBID = pendingOtherMBID
	m.PortraitNameEnded(pendingOtherMBID, "Name B")
	if err := s.MarkEnriched(ctx, m); err != nil {
		t.Fatal(err)
	}
	if q, ok := pendingQueryOf(t, s, moved); ok {
		t.Errorf("a stamp giving the row another artist kept the first artist's lookup by %q", q)
	}
}

// TestAPassKeepsTheFirstLookupThatWaits: within one pass, a lookup that waits
// by one name stays whatever the pass learns after it of another name of the
// same artist, an answer that ends that name or a second lookup that waits by
// it, so one track failing under two names keeps the first; and a name an
// answer ended before it takes nothing from it. Until B761's third round
// whatever came last overwrote the lookup.
func TestAPassKeepsTheFirstLookupThatWaits(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name  string
		learn func(tr *Track)
	}{
		{"an answer ends another name after it", func(tr *Track) {
			tr.PortraitPending(pendingArtistMBID, "Name A")
			tr.PortraitNameEnded(pendingArtistMBID, "Name B")
		}},
		{"another name waits after it", func(tr *Track) {
			tr.PortraitPending(pendingArtistMBID, "Name A")
			tr.PortraitPending(pendingArtistMBID, "Name B")
		}},
		{"an answer ended another name before it", func(tr *Track) {
			tr.PortraitNameEnded(pendingArtistMBID, "Name B")
			tr.PortraitPending(pendingArtistMBID, "Name A")
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := openPendingStore(t)
			const p = "Artist/Album/01.flac"
			tr := upsertForEnrichment(t, s, Track{Path: p, Artist: "Artist", Album: "Album"})
			tr.ArtistMBID = pendingArtistMBID
			c.learn(tr)
			if err := s.MarkEnriched(ctx, tr); err != nil {
				t.Fatal(err)
			}
			requirePendingRead(t, s, p, pendingArtistMBID, "Name A")
		})
	}
}

// TestAnOpenKeepsTheAttemptsOfABackoffStillRunning: an artist's portrait by a
// name fails, a changed file's upsert drops the last row that waited by that
// name, and the store is opened again before the retry is due: the count and
// the time of the last failure are still there, which every path asks before
// it asks Deezer. Until B761's third round each open forgot the attempts no
// waiting row named, so a restart after a changed file forgot a backoff still
// running.
func TestAnOpenKeepsTheAttemptsOfABackoffStillRunning(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "bridge.db")
	failedAt := time.Unix(1_800_000_000, 0)
	s := openPortraitStoreAt(t, dbPath, func() time.Time { return failedAt })
	const p = "Artist/Album/01.flac"
	stampRefused(t, s, p, pendingArtistMBID, "Name A")
	if _, err := s.NotePortraitFailure(ctx, pendingArtistMBID, "Name A", failedAt); err != nil {
		t.Fatal(err)
	}
	upsertForEnrichment(t, s, Track{Path: p, Artist: "Artist", Album: "Album", Size: 2})
	if q, ok := pendingQueryOf(t, s, p); ok {
		t.Fatalf("the changed file's upsert kept the lookup by %q: the case needs it dropped", q)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	reopened := openPortraitStoreAt(t, dbPath, func() time.Time { return failedAt.Add(30 * time.Second) })
	got, err := reopened.PortraitAttemptsOf(ctx, pendingArtistMBID, "Name A")
	if err != nil {
		t.Fatal(err)
	}
	if got.Failures != 1 || !got.LastFailedAt.Equal(failedAt) {
		t.Errorf("after the reopen the attempts at the name are %+v, want 1 failure, the last at %v: its backoff still runs",
			got, failedAt)
	}
}

// TestAnExhaustedLookupStaysSoAcrossADayAndAReopen: a lookup whose name has
// failed PortraitPendingAttempts times, which a row still waits by, keeps its
// count across a pause of more than a day and a reopen, so the route's probe
// goes on answering that the portrait is no longer pending: the age bound
// forgets the attempts no waiting row names, and those alone.
func TestAnExhaustedLookupStaysSoAcrossADayAndAReopen(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "bridge.db")
	at := time.Unix(1_800_000_000, 0)
	s := openPortraitStoreAt(t, dbPath, func() time.Time { return at })
	const p = "Artist/Album/01.flac"
	stampRefused(t, s, p, pendingArtistMBID, "Name A")
	for i := 0; i < PortraitPendingAttempts; i++ {
		if _, err := s.NotePortraitFailure(ctx, pendingArtistMBID, "Name A", at); err != nil {
			t.Fatal(err)
		}
	}
	requireProbe(t, s, pendingArtistMBID, false, "the name at the bound")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	reopened := openPortraitStoreAt(t, dbPath, func() time.Time { return at.Add(portraitAttemptsKept + time.Hour) })
	requireAttempts(t, reopened, pendingArtistMBID, "Name A", PortraitPendingAttempts)
	requireProbe(t, reopened, pendingArtistMBID, false, "the name at the bound, a day and an hour later")
	requirePendingRead(t, reopened, p, pendingArtistMBID, "Name A")
}
