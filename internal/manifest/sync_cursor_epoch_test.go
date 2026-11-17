package manifest

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/formatgate"
)

// TestACursorPastAnythingThisHistoryIssuedIsIncomplete: the clock guard
// (DeltaSinceCovered, backlog B653). A delta cursor at or past the stamp the
// next write would carry was issued by no write of this history: one from
// before a step back of the clock, until the clock passes it again, and one
// past a history whose stamps ran ahead of the clock, until its watermark or
// the clock passes it. Such a cursor answers deltaIncomplete with no
// deletions; a cursor below that stamp, the watermark itself included, stays
// covered. The guard's scope ends where the clock has passed a cursor again,
// which the last case pins: what that leaves is backlog B919's.
func TestACursorPastAnythingThisHistoryIssuedIsIncomplete(t *testing.T) {
	ctx := context.Background()
	// Every instant sits an hour past the real clock, so the coverage start
	// OpenStore seeded from the real clock is below each of them.
	base := time.Now().Add(time.Hour).Truncate(time.Second)
	at := func(sec int) time.Time { return base.Add(time.Duration(sec) * time.Second) }
	clock := func(s *Store, sec int) { s.now = func() time.Time { return at(sec) } }
	covered := func(t *testing.T, s *Store, since time.Time) bool {
		t.Helper()
		ok, err := s.DeltaSinceCovered(ctx, since)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	delta := func(t *testing.T, s *Store, since time.Time) *Manifest {
		t.Helper()
		m, err := BuildManifest(ctx, s, []string{"/lib"}, since, formatgate.All())
		if err != nil {
			t.Fatal(err)
		}
		return m
	}

	t.Run("a cursor from before the clock stepped back", func(t *testing.T) {
		s := openJournalTestStore(t)
		clock(s, 10)
		journalUpsert(t, s, "c/one.flac", 1)
		journalUpsert(t, s, "c/two.flac", 2)
		// A response at the clock's 20 s hands the client this cursor.
		cursor := at(20)
		clock(s, 12)
		if err := s.DeleteTrack(ctx, "c/two.flac"); err != nil {
			t.Fatal(err)
		}
		if covered(t, s, cursor) {
			t.Error("a cursor past the stamp the next write would carry reads as covered")
		}
		if m := delta(t, s, cursor); !m.DeltaIncomplete || len(m.Deleted) != 0 {
			t.Errorf("delta: incomplete %v, deleted %q; want incomplete and no deletions", m.DeltaIncomplete, m.Deleted)
		}
		clock(s, 21)
		if !covered(t, s, cursor) {
			t.Error("the cursor still reads as not covered once the clock has passed it")
		}
	})

	t.Run("a cursor below the next stamp stays covered", func(t *testing.T) {
		s := openJournalTestStore(t)
		clock(s, 10)
		journalUpsert(t, s, "w/one.flac", 1)
		watermark := time.Unix(0, indexedAtOf(t, s, "w/one.flac"))
		clock(s, 30)
		for name, since := range map[string]time.Time{
			"the watermark":            watermark,
			"just below the clock":     at(30).Add(-time.Nanosecond),
			"between the two of them":  at(20),
			"one past the watermark's": watermark.Add(time.Nanosecond),
		} {
			if !covered(t, s, since) {
				t.Errorf("%s (%v) reads as not covered", name, since)
			}
			if m := delta(t, s, since); m.DeltaIncomplete {
				t.Errorf("%s: the delta answers incomplete", name)
			}
		}
		// The stamp the next write would carry is the clock here: a cursor
		// at it is past every stamp this history wrote.
		if covered(t, s, at(30)) {
			t.Error("a cursor at the stamp the next write would carry reads as covered")
		}
	})

	t.Run("a history whose stamps ran ahead of the clock", func(t *testing.T) {
		s := openJournalTestStore(t)
		// Stamps written where the clock read an hour ahead of this one (a
		// data directory carried from such a host).
		clock(s, 3600)
		journalUpsert(t, s, "f/one.flac", 1)
		watermark := time.Unix(0, indexedAtOf(t, s, "f/one.flac"))
		clock(s, 0)
		if !covered(t, s, watermark) {
			t.Error("this history's own watermark, ahead of the clock, reads as not covered")
		}
		past := watermark.Add(time.Minute)
		if covered(t, s, past) {
			t.Error("a cursor past every stamp this history wrote reads as covered")
		}
		if m := delta(t, s, past); !m.DeltaIncomplete || len(m.Deleted) != 0 {
			t.Errorf("delta past the watermark: incomplete %v, deleted %q; want incomplete", m.DeltaIncomplete, m.Deleted)
		}
	})

	t.Run("a cursor the clock has passed again is outside the guard", func(t *testing.T) {
		s := openJournalTestStore(t)
		clock(s, 50)
		journalUpsert(t, s, "b/early.flac", 1)
		// A response at the clock's 100 s, with the watermark at 50 s.
		cursor := at(100)
		clock(s, 60)
		journalUpsert(t, s, "b/late.flac", 2)
		clock(s, 101)
		// Covered as before: backlog B919's, pinned until it is fixed.
		if !covered(t, s, cursor) {
			t.Error("the cursor reads as not covered: the guard's scope changed, so B919's record and this case change with it")
		}
		m := delta(t, s, cursor)
		if m.DeltaIncomplete || slices.ContainsFunc(m.Tracks, func(tr Track) bool { return tr.Path == "b/late.flac" }) {
			t.Errorf("delta: incomplete %v, tracks %q; this case pins the delta as it stands (B919)", m.DeltaIncomplete, pathsOfManifest(m))
		}
	})
}
