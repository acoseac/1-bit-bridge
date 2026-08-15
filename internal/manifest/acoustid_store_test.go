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

// mustResetEnrichedByPaths re-queues paths and requires exactly wantN rows to
// have been affected, reporting why that number is the expected one.
//
// Extracted because every edge case below otherwise repeats the same
// error-then-count pair inside its own subtest closure, where each branch also
// pays a nesting penalty — eight of them are what put the caller over the
// cognitive-complexity budget while saying nothing a reader did not already
// know. Takes no ctx, matching seedEnrichedTrack and enrichedAt above.
func mustResetEnrichedByPaths(t *testing.T, s *Store, paths []string, wantN int64, why string) {
	t.Helper()
	n, err := s.ResetEnrichedByPaths(context.Background(), paths)
	if err != nil {
		t.Fatalf("ResetEnrichedByPaths(%q): %v", paths, err)
	}
	if n != wantN {
		t.Fatalf("RowsAffected = %d, want %d — %s", n, wantN, why)
	}
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
		mustResetEnrichedByPaths(t, s, nil, 0, "an empty set must reach no row")
	})

	t.Run("unknown paths affect nothing", func(t *testing.T) {
		mustResetEnrichedByPaths(t, s, []string{"nope.flac"}, 0, "no such row exists")
	})

	t.Run("an already-unenriched row is not counted", func(t *testing.T) {
		// The WHERE enriched_at > 0 guard: a row the enricher has already
		// queued must not be disturbed, and must not inflate the count.
		if err := s.UpsertTrack(ctx, &Track{Path: "fresh.flac", Size: 1, ModTime: time.Unix(1, 0)}); err != nil {
			t.Fatal(err)
		}
		mustResetEnrichedByPaths(t, s, []string{"fresh.flac"}, 0, "the row was already queued")
	})

	t.Run("a path containing SQL-ish characters is handled as data", func(t *testing.T) {
		// The set travels as one bound JSON array through json_each, so
		// quoting is never constructed. Real library paths carry apostrophes.
		const odd = `Don't Stop/01 - "Quoted" & ; DROP.flac`
		seedEnrichedTrack(t, s, odd)
		mustResetEnrichedByPaths(t, s, []string{odd}, 1, "the seeded row must be re-queued")
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

// TestAcoustIDNoMatchRecordsVersionAndRespectsTTL covers the persisted
// no-match verdict.
//
// Two properties, and the feature is unsafe without either. The verdict must
// carry the file version it was computed from, or a re-encode would never be
// re-checked; and it must fall out of the fresh set once the TTL cutoff moves
// past it, because AcoustID's database grows and a permanent negative would
// freeze the library's ceiling at whatever it knew that day.
func TestAcoustIDNoMatchRecordsVersionAndRespectsTTL(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	ctx := context.Background()

	seedEnrichedTrack(t, s, "a.flac")
	seedEnrichedTrack(t, s, "b.flac")

	beforeEnr := enrichedAt(t, s, "a.flac")
	beforeIdx := indexedAt(t, s, "a.flac")

	const size, mtime = int64(4242), int64(1_700_000_000_000_000_000)
	if err := s.SetAcoustIDNoMatch(ctx, "a.flac", size, mtime); err != nil {
		t.Fatal(err)
	}

	// Recording what AcoustID does not know is not a change to what the row
	// says — a bump would push a delta for a column no client receives.
	if now := enrichedAt(t, s, "a.flac"); now != beforeEnr {
		t.Errorf("enriched_at moved %d -> %d", beforeEnr, now)
	}
	if now := indexedAt(t, s, "a.flac"); now != beforeIdx {
		t.Errorf("indexed_at moved %d -> %d — a no-match is not a wire change", beforeIdx, now)
	}

	// Well inside the TTL: present, carrying the exact version it was
	// computed from.
	fresh, err := s.FreshAcoustIDNoMatches(ctx, time.Now().Add(-time.Hour).UnixNano())
	if err != nil {
		t.Fatal(err)
	}
	rec, ok := fresh["a.flac"]
	if !ok {
		t.Fatalf("a.flac absent from the fresh set = %v", fresh)
	}
	if rec.Size != size || rec.MTimeNS != mtime {
		t.Errorf("recorded version = (%d, %d), want (%d, %d) — without the exact "+
			"pair a re-encode could never re-open the row", rec.Size, rec.MTimeNS, size, mtime)
	}
	if _, ok := fresh["b.flac"]; ok {
		t.Error("b.flac in the fresh set despite no verdict — the all-zero default must " +
			"not read as a recorded no-match")
	}

	// Cutoff moved past the stamp: the row must fall out, so the sweep
	// re-asks AcoustID about it.
	stale, err := s.FreshAcoustIDNoMatches(ctx, time.Now().Add(time.Hour).UnixNano())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := stale["a.flac"]; ok {
		t.Error("a.flac still fresh past the cutoff — an expired verdict would suppress " +
			"re-checking forever, which is the objection the TTL answers")
	}
}

// TestClearAcoustIDNoMatchesUnderPrefixIsByteRanged pins the scope of the
// folder-scoped clear.
//
// This is a WRITE keyed on a path prefix, so it must use the byte range rather
// than LIKE: nothing sets case_sensitive_like, so `path LIKE 'Album/%'` also
// matches `album/…`, which on a case-sensitive filesystem is a different
// directory. Clearing a neighbour's verdicts is not corruption, but it
// silently re-decodes an unrelated folder — and the same predicate shape is
// what deletes rows elsewhere, so the habit is the thing being pinned.
func TestClearAcoustIDNoMatchesUnderPrefixIsByteRanged(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	ctx := context.Background()

	for _, p := range []string{"Album/x.flac", "album/y.flac", "Other/z.flac"} {
		seedEnrichedTrack(t, s, p)
		if err := s.SetAcoustIDNoMatch(ctx, p, 1, 1); err != nil {
			t.Fatal(err)
		}
	}

	n, err := s.ClearAcoustIDNoMatchesUnderPrefix(ctx, "Album")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("cleared %d rows, want exactly 1 — the case-twin sibling and the "+
			"unrelated folder must be untouched", n)
	}

	fresh, err := s.FreshAcoustIDNoMatches(ctx, time.Now().Add(-time.Hour).UnixNano())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := fresh["Album/x.flac"]; ok {
		t.Error("Album/x.flac still carries a verdict — the scoped clear missed its target")
	}
	for _, p := range []string{"album/y.flac", "Other/z.flac"} {
		if _, ok := fresh[p]; !ok {
			t.Errorf("%s lost its verdict — the prefix write reached outside its folder", p)
		}
	}

	// An UNSCOPED prefix must delegate to the library-wide clear. The admin
	// helper routes both retries through the prefix form and passes "" for the
	// global one, so if this stopped delegating, "Retry missing" would clear
	// nothing at all while still reporting success.
	if n, err := s.ClearAcoustIDNoMatchesUnderPrefix(ctx, ""); err != nil {
		t.Fatal(err)
	} else if n != 2 {
		t.Fatalf("unscoped clear affected %d rows, want the 2 survivors — an empty "+
			"prefix must mean the whole library, not an empty range", n)
	}
	after, err := s.FreshAcoustIDNoMatches(ctx, time.Now().Add(-time.Hour).UnixNano())
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 0 {
		t.Errorf("verdicts survived the library-wide clear: %v", after)
	}
}
