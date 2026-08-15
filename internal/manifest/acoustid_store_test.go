package manifest

import (
	"context"
	"testing"
	"time"
)

func seedEnrichedTrack(t *testing.T, s *Store, path string) {
	t.Helper()
	ctx := context.Background()
	if err := s.UpsertTrack(ctx, &Track{Path: path, Size: 1, ModTime: time.Unix(1, 0)}); err != nil {
		t.Fatal(err)
	}
	// UpsertTrack resets enriched_at to 0; MarkEnriched is what stamps it.
	if err := s.MarkEnriched(ctx, &Track{Path: path, Size: 1, ModTime: time.Unix(1, 0)}); err != nil {
		t.Fatal(err)
	}
}

func enrichedAt(t *testing.T, s *Store, path string) int64 {
	t.Helper()
	var v int64
	if err := s.db.QueryRow(`SELECT enriched_at FROM tracks WHERE path = ?`, path).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func indexedAt(t *testing.T, s *Store, path string) int64 {
	t.Helper()
	var v int64
	if err := s.db.QueryRow(`SELECT indexed_at FROM tracks WHERE path = ?`, path).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// TestResetEnrichedByPathsIsScoped is the contract that justifies adding a new
// enriched_at writer at all.
//
// The closed writer list exists because `WHERE enriched_at = 0` drives the
// enrichment worker. This one is the narrowest of them — an explicit path set
// rather than a predicate — and the test that matters is the negative one: a
// track NOT in the set must be left completely alone. The alternative the
// sweeper could have used, the library-wide ResetEnrichedMisses, touches
// roughly half the library and would push a ~9,000-track delta to every paired
// device on every sweep.
func TestResetEnrichedByPathsIsScoped(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	ctx := context.Background()

	seedEnrichedTrack(t, s, "a.flac")
	seedEnrichedTrack(t, s, "b.flac")
	seedEnrichedTrack(t, s, "c.flac")

	beforeIdxA := indexedAt(t, s, "a.flac")
	beforeEnrB := enrichedAt(t, s, "b.flac")

	n, err := s.ResetEnrichedByPaths(ctx, []string{"a.flac", "c.flac"})
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("RowsAffected = %d, want 2", n)
	}
	if got := enrichedAt(t, s, "a.flac"); got != 0 {
		t.Errorf("a.flac enriched_at = %d, want 0", got)
	}
	if got := enrichedAt(t, s, "c.flac"); got != 0 {
		t.Errorf("c.flac enriched_at = %d, want 0", got)
	}
	// The load-bearing assertion: an untargeted row is untouched.
	if got := enrichedAt(t, s, "b.flac"); got != beforeEnrB {
		t.Errorf("b.flac enriched_at changed to %d — the reset must be scoped to the given paths", got)
	}
	// indexed_at is NOT bumped here: this only re-queues. The bump comes
	// later from MarkEnriched, when the re-enrichment commits real data.
	// Bumping now would push a delta for tracks that have not changed yet.
	if got := indexedAt(t, s, "a.flac"); got != beforeIdxA {
		t.Errorf("indexed_at moved from %d to %d — a re-queue is not a change", beforeIdxA, got)
	}
}

func TestResetEnrichedByPathsEdgeCases(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	ctx := context.Background()

	t.Run("empty set is a no-op", func(t *testing.T) {
		n, err := s.ResetEnrichedByPaths(ctx, nil)
		if err != nil || n != 0 {
			t.Fatalf("got (%d, %v), want (0, nil)", n, err)
		}
	})

	t.Run("unknown paths affect nothing", func(t *testing.T) {
		n, err := s.ResetEnrichedByPaths(ctx, []string{"nope.flac"})
		if err != nil || n != 0 {
			t.Fatalf("got (%d, %v), want (0, nil)", n, err)
		}
	})

	t.Run("an already-unenriched row is not counted", func(t *testing.T) {
		// The WHERE enriched_at > 0 guard: a row the enricher has already
		// queued must not be disturbed, and must not inflate the count.
		if err := s.UpsertTrack(ctx, &Track{Path: "fresh.flac", Size: 1, ModTime: time.Unix(1, 0)}); err != nil {
			t.Fatal(err)
		}
		n, err := s.ResetEnrichedByPaths(ctx, []string{"fresh.flac"})
		if err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("RowsAffected = %d, want 0 — the row was already queued", n)
		}
	})

	t.Run("a path containing SQL-ish characters is handled as data", func(t *testing.T) {
		// The set travels as one bound JSON array through json_each, so
		// quoting is never constructed. Real library paths carry apostrophes.
		const odd = `Don't Stop/01 - "Quoted" & ; DROP.flac`
		seedEnrichedTrack(t, s, odd)
		n, err := s.ResetEnrichedByPaths(ctx, []string{odd})
		if err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("RowsAffected = %d, want 1", n)
		}
		if got := enrichedAt(t, s, odd); got != 0 {
			t.Errorf("enriched_at = %d, want 0", got)
		}
	})
}

// TestAcoustIDMatchProvenance covers the column that makes fingerprint writes
// auditable and reversible. Without it, an MBID written from audio is
// indistinguishable from one written from tags, forever.
func TestAcoustIDMatchProvenance(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	ctx := context.Background()

	seedEnrichedTrack(t, s, "a.flac")
	seedEnrichedTrack(t, s, "b.flac")

	if got, err := s.AcoustIDMatch(ctx, "a.flac"); err != nil || got != "" {
		t.Fatalf("unset provenance = (%q, %v), want empty", got, err)
	}

	beforeEnr := enrichedAt(t, s, "a.flac")
	beforeIdx := indexedAt(t, s, "a.flac")

	const acoustID = "9ff43b6a-4f16-427c-93c2-92307ca505e0"
	if err := s.SetAcoustIDMatch(ctx, "a.flac", acoustID); err != nil {
		t.Fatal(err)
	}
	got, err := s.AcoustIDMatch(ctx, "a.flac")
	if err != nil {
		t.Fatal(err)
	}
	if got != acoustID {
		t.Errorf("provenance = %q, want %q", got, acoustID)
	}

	// Recording HOW a row was resolved is not a change to WHAT it says, so
	// neither timestamp moves. Bumping indexed_at here would push a delta to
	// every paired device for a column they never receive.
	if now := enrichedAt(t, s, "a.flac"); now != beforeEnr {
		t.Errorf("enriched_at moved %d -> %d", beforeEnr, now)
	}
	if now := indexedAt(t, s, "a.flac"); now != beforeIdx {
		t.Errorf("indexed_at moved %d -> %d — provenance is not a wire change", beforeIdx, now)
	}

	n, err := s.CountAcoustIDMatches(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("CountAcoustIDMatches = %d, want 1", n)
	}
}

// TestAcoustIDMatchSurvivesReEnrichment pins that provenance is column-only.
//
// MarkEnriched rewrites tags_json wholesale. If the provenance had been stored
// in that blob it would be silently erased on the next enrichment pass, which
// is exactly when it is most wanted — the row is being rewritten and you want
// to know where its MBIDs came from.
func TestAcoustIDMatchSurvivesReEnrichment(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	ctx := context.Background()

	seedEnrichedTrack(t, s, "a.flac")
	const acoustID = "9ff43b6a-4f16-427c-93c2-92307ca505e0"
	if err := s.SetAcoustIDMatch(ctx, "a.flac", acoustID); err != nil {
		t.Fatal(err)
	}

	if err := s.MarkEnriched(ctx, &Track{
		Path: "a.flac", Size: 1, ModTime: time.Unix(1, 0),
		ArtistMBID: "6d7b7cd4-254b-4c25-83f6-dd20f98ceacd",
	}); err != nil {
		t.Fatal(err)
	}

	got, err := s.AcoustIDMatch(ctx, "a.flac")
	if err != nil {
		t.Fatal(err)
	}
	if got != acoustID {
		t.Fatalf("provenance = %q after re-enrichment, want it preserved — "+
			"a tags_json field would have been erased here", got)
	}
}

// TestAcoustIDMatchedPaths pins the set the sweeper's candidate pass consumes.
//
// acoustid_match is column-only, so the Track rows StreamTracks yields cannot
// carry it; this set is how the column reaches the sweep without the field
// leaking toward tags_json or the wire type.
func TestAcoustIDMatchedPaths(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	ctx := context.Background()

	if got, err := s.AcoustIDMatchedPaths(ctx); err != nil || len(got) != 0 {
		t.Fatalf("empty store = (%v, %v), want an empty set", got, err)
	}

	seedEnrichedTrack(t, s, "a.flac")
	seedEnrichedTrack(t, s, "b.flac")
	seedEnrichedTrack(t, s, "c.flac")
	if err := s.SetAcoustIDMatch(ctx, "a.flac", "9ff43b6a-4f16-427c-93c2-92307ca505e0"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAcoustIDMatch(ctx, "c.flac", "1c0eee38-6dd2-4b8d-a5a4-ea0b441c30c6"); err != nil {
		t.Fatal(err)
	}

	got, err := s.AcoustIDMatchedPaths(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("set = %v, want exactly {a.flac, c.flac}", got)
	}
	for _, p := range []string{"a.flac", "c.flac"} {
		if _, ok := got[p]; !ok {
			t.Errorf("matched path %q missing from the set", p)
		}
	}
	if _, ok := got["b.flac"]; ok {
		t.Errorf("b.flac in the set despite carrying no provenance — the empty-string " +
			"default must not read as matched")
	}
}
