package manifest

import (
	"context"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

// TestUpsertTrackEqualClockStillAdvances: when the injected clock returns
// EXACTLY the existing row's indexed_at (rapid back-to-back UpsertTrack
// at the same nanosecond — fake clocks, low-resolution wall clocks, an
// mtime-changed-but-clock-stable scan tick), the CASE WHEN form must
// produce a strict advance. Without it, a client that synced at the
// equal timestamp would miss the second mutation under
// `WHERE indexed_at > since`. Mirrors TestUpsertVariantEqualClockStillAdvances
// for the track-write path.
func TestUpsertTrackEqualClockStillAdvances(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })

	upsertParent(t, s, "Music/A/1.flac")
	var initialIndexedAt int64
	if err := s.db.QueryRow(
		`SELECT indexed_at FROM tracks WHERE path = ?`, "Music/A/1.flac",
	).Scan(&initialIndexedAt); err != nil {
		t.Fatalf("read indexed_at: %v", err)
	}

	// Pin the clock to EXACTLY the existing indexed_at — equality case.
	s.now = func() time.Time { return time.Unix(0, initialIndexedAt) }

	if err := s.UpsertTrack(context.Background(), &Track{
		Path:    "Music/A/1.flac",
		Size:    200, // a real change so the upsert isn't a no-op
		ModTime: time.Now(),
	}); err != nil {
		t.Fatalf("UpsertTrack: %v", err)
	}

	var afterIndexedAt int64
	if err := s.db.QueryRow(
		`SELECT indexed_at FROM tracks WHERE path = ?`, "Music/A/1.flac",
	).Scan(&afterIndexedAt); err != nil {
		t.Fatalf("read indexed_at after UpsertTrack: %v", err)
	}
	if afterIndexedAt != initialIndexedAt+1 {
		t.Errorf("equal-clock should produce existing+1 strict bump: expected %d, got %d (initial=%d)",
			initialIndexedAt+1, afterIndexedAt, initialIndexedAt)
	}
}

// TestUpsertTrackMonotonicGuard: an injected clock that returns a
// timestamp in the PAST must NOT regress indexed_at. The CASE WHEN
// form takes existing+1 in that case (past-clock branch lands in the
// `tracks.indexed_at >= excluded.indexed_at` arm). Mirrors
// TestUpsertVariantMonotonicGuard for the track-write path.
func TestUpsertTrackMonotonicGuard(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })

	upsertParent(t, s, "Music/A/1.flac")
	var initialIndexedAt int64
	if err := s.db.QueryRow(
		`SELECT indexed_at FROM tracks WHERE path = ?`, "Music/A/1.flac",
	).Scan(&initialIndexedAt); err != nil {
		t.Fatalf("read indexed_at: %v", err)
	}

	pastTimestamp := initialIndexedAt - (1 * time.Hour).Nanoseconds()
	s.now = func() time.Time { return time.Unix(0, pastTimestamp) }

	if err := s.UpsertTrack(context.Background(), &Track{
		Path:    "Music/A/1.flac",
		Size:    300,
		ModTime: time.Now(),
	}); err != nil {
		t.Fatalf("UpsertTrack: %v", err)
	}

	var afterIndexedAt int64
	if err := s.db.QueryRow(
		`SELECT indexed_at FROM tracks WHERE path = ?`, "Music/A/1.flac",
	).Scan(&afterIndexedAt); err != nil {
		t.Fatalf("read indexed_at after UpsertTrack: %v", err)
	}
	if afterIndexedAt < initialIndexedAt {
		t.Errorf("indexed_at regressed under past-clock injection: before=%d after=%d (clock returned %d)",
			initialIndexedAt, afterIndexedAt, pastTimestamp)
	}
	if afterIndexedAt != initialIndexedAt+1 {
		t.Errorf("past-clock should produce existing+1 strict bump: expected %d, got %d",
			initialIndexedAt+1, afterIndexedAt)
	}
}

// TestUpsertTrackBatchEqualClockEachRowAdvances: when every row in a
// batch flush is already at the injected clock's `now`, each one must
// independently advance to `now+1`. The shared batch-level `now` is
// bound once per row's `excluded.indexed_at`; the CASE WHEN evaluates
// per-row against ITS own existing `tracks.indexed_at`. A regression
// that switched to a single-row check (e.g. a global comparison) would
// flatten the post-batch state and reveal here.
func TestUpsertTrackBatchEqualClockEachRowAdvances(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })

	// Seed three rows via UpsertTrack at a known clock value.
	const t0 int64 = 1_000_000_000
	s.now = func() time.Time { return time.Unix(0, t0) }
	paths := []string{"Music/A/1.flac", "Music/A/2.flac", "Music/A/3.flac"}
	for _, p := range paths {
		if err := s.UpsertTrack(context.Background(), &Track{
			Path: p, Size: 100, ModTime: time.Unix(0, t0),
		}); err != nil {
			t.Fatalf("seed UpsertTrack(%q): %v", p, err)
		}
	}

	// Verify seed state — every row at t0.
	for _, p := range paths {
		var got int64
		if err := s.db.QueryRow(
			`SELECT indexed_at FROM tracks WHERE path = ?`, p,
		).Scan(&got); err != nil {
			t.Fatalf("read seed indexed_at(%q): %v", p, err)
		}
		if got != t0 {
			t.Fatalf("seed indexed_at(%q) = %d, want %d", p, got, t0)
		}
	}

	// Pin the clock to t0 again — the batch's shared `now` will equal
	// every row's existing indexed_at, exercising the CASE WHEN per-row.
	batch := make([]*Track, len(paths))
	for i, p := range paths {
		batch[i] = &Track{Path: p, Size: 200, ModTime: time.Unix(0, t0)}
	}
	if err := s.UpsertTrackBatch(context.Background(), batch); err != nil {
		t.Fatalf("UpsertTrackBatch: %v", err)
	}

	// Each row should have strictly advanced to t0+1.
	for _, p := range paths {
		var got int64
		if err := s.db.QueryRow(
			`SELECT indexed_at FROM tracks WHERE path = ?`, p,
		).Scan(&got); err != nil {
			t.Fatalf("read post-batch indexed_at(%q): %v", p, err)
		}
		if got != t0+1 {
			t.Errorf("post-batch indexed_at(%q) = %d, want %d (t0=%d)", p, got, t0+1, t0)
		}
	}
}

// TestUpsertTrackBatchFreshInsertUsesClock: a batch flush that inserts
// NEW rows (no conflict) must stamp indexed_at = `now`, not `now+1`.
// The CASE WHEN only fires on the ON CONFLICT branch; brand-new rows
// take the unconditional VALUES path. Locks the no-conflict contract.
func TestUpsertTrackBatchFreshInsertUsesClock(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })

	const t0 int64 = 2_000_000_000
	s.now = func() time.Time { return time.Unix(0, t0) }
	batch := []*Track{
		{Path: "Music/B/1.flac", Size: 100, ModTime: time.Unix(0, t0)},
		{Path: "Music/B/2.flac", Size: 100, ModTime: time.Unix(0, t0)},
	}
	if err := s.UpsertTrackBatch(context.Background(), batch); err != nil {
		t.Fatalf("UpsertTrackBatch: %v", err)
	}
	for _, b := range batch {
		var got int64
		if err := s.db.QueryRow(
			`SELECT indexed_at FROM tracks WHERE path = ?`, b.Path,
		).Scan(&got); err != nil {
			t.Fatalf("read indexed_at(%q): %v", b.Path, err)
		}
		if got != t0 {
			t.Errorf("fresh-insert indexed_at(%q) = %d, want %d", b.Path, got, t0)
		}
	}
}

// TestIndexedAtBumpsClearTheLibraryWideMax pins the property that
// distinguishes indexedAtAdvanceSQL from the CASE WHEN form it replaced:
// a bump must land STRICTLY ABOVE every row, not merely above the bumped
// row's own prior value.
//
// Shape (the coarse-clock collision, which is why this failed only on the
// windows-latest CI leg — ~15.6 ms granularity makes it routine, and
// nanosecond clocks hide it): the target row is OLD, a sibling row holds
// the library max, and the write clock reads exactly that max. The old
// form's `ELSE ?` arm then assigned the raw clock, landing the bumped row
// EXACTLY ON a cursor equal to the sibling's value, where
// `indexed_at > since` filters it out.
//
// Covers every writer sharing the expression, because the guarantee
// belongs to the SQL and not to any one caller. Deliberately excludes the
// UpsertTrack / UpsertTrackBatch conflict arms (they write new content at
// wall-clock time — see indexedAtAdvanceSQL) and migration v34's post()
// (shipped migrations are not rewritten).
func TestIndexedAtBumpsClearTheLibraryWideMax(t *testing.T) {
	const (
		target  = "Music/A/target.flac"
		sibling = "Music/B/sibling.flac"
	)
	bumps := []struct {
		name string
		run  func(t *testing.T, s *Store)
	}{
		{"MarkEnriched", func(t *testing.T, s *Store) {
			if err := s.MarkEnriched(context.Background(), &Track{
				Path: target, Size: 100, ModTime: time.Unix(0, 0).UTC(),
			}); err != nil {
				t.Fatal(err)
			}
		}},
		{"applyReconciledTracks", func(t *testing.T, s *Store) {
			if _, err := s.applyReconciledTracks(context.Background(), []Track{{
				Path: target, Size: 100, ModTime: time.Unix(0, 0).UTC(), Album: "Reconciled",
			}}); err != nil {
				t.Fatal(err)
			}
		}},
		{"UpsertVariant", func(t *testing.T, s *Store) {
			if err := s.UpsertVariant(context.Background(), VariantRow{
				SourcePath: target, VariantID: "upscaled-v1-176400-24",
				SidecarPath: "/tmp/x.flac", Format: "flac",
				SampleRate: 176400, BitsPerSample: 24,
			}); err != nil {
				t.Fatal(err)
			}
		}},
		{"DeleteVariant", func(t *testing.T, s *Store) {
			ctx := context.Background()
			if err := s.UpsertVariant(ctx, VariantRow{
				SourcePath: target, VariantID: "upscaled-v1-176400-24",
				SidecarPath: "/tmp/x.flac", Format: "flac",
				SampleRate: 176400, BitsPerSample: 24,
			}); err != nil {
				t.Fatal(err)
			}
			// The UpsertVariant above advanced the target past the sibling;
			// re-flatten so the DELETE is measured from the same shape.
			flattenIndexedAt(t, s, target, indexedAtOf(t, s, sibling)-100)
			if err := s.DeleteVariant(ctx, target, "upscaled-v1-176400-24"); err != nil {
				t.Fatal(err)
			}
		}},
		{"UpsertAnalysis", func(t *testing.T, s *Store) {
			if err := s.UpsertAnalysis(context.Background(), AnalysisRow{
				SourcePath: target, WaveformPath: "/tmp/x.wf", WaveformTag: "abcd1234",
				WaveformSize: 10, SchemaVersion: "wf7",
			}); err != nil {
				t.Fatal(err)
			}
		}},
		{"DeleteAnalysis", func(t *testing.T, s *Store) {
			ctx := context.Background()
			if err := s.UpsertAnalysis(ctx, AnalysisRow{
				SourcePath: target, WaveformPath: "/tmp/x.wf", WaveformTag: "abcd1234",
				WaveformSize: 10, SchemaVersion: "wf7",
			}); err != nil {
				t.Fatal(err)
			}
			flattenIndexedAt(t, s, target, indexedAtOf(t, s, sibling)-100)
			if err := s.DeleteAnalysis(ctx, target); err != nil {
				t.Fatal(err)
			}
		}},
		{"SetArtworkVersionAndBumpIndex", func(t *testing.T, s *Store) {
			if _, err := s.SetArtworkVersionAndBumpIndex(context.Background(), "art-mbid", "v2"); err != nil {
				t.Fatal(err)
			}
		}},
		{"SetBookletTagAndBumpIndex", func(t *testing.T, s *Store) {
			if _, err := s.SetBookletTagAndBumpIndex(context.Background(), "album-mbid", "tag2"); err != nil {
				t.Fatal(err)
			}
		}},
		{"ApplyDupeStamps", func(t *testing.T, s *Store) {
			if _, err := s.ApplyDupeStamps(context.Background(), []DupeStamp{{
				Path: target, GroupID: "g1", Tier: "same-format", BumpIndexed: true,
			}}); err != nil {
				t.Fatal(err)
			}
		}},
	}

	for _, b := range bumps {
		t.Run(b.name, func(t *testing.T) {
			s := openTempStore(t)
			t.Cleanup(func() { _ = s.Close() })
			ctx := context.Background()

			// The target carries the MBIDs the album-wide writers key on.
			if err := s.UpsertTrack(ctx, &Track{
				Path: target, Size: 100, ModTime: time.Unix(0, 0).UTC(),
				ArtworkMBID: "art-mbid", MusicBrainzAlbumID: "album-mbid",
			}); err != nil {
				t.Fatal(err)
			}
			upsertParent(t, s, sibling)

			// The collision shape: target OLD, sibling holds the max, and
			// the clock reads EXACTLY that max.
			max := indexedAtOf(t, s, sibling)
			flattenIndexedAt(t, s, target, max-100)
			s.now = func() time.Time { return time.Unix(0, max) }

			b.run(t, s)

			requireBumpClearedMax(t, s, b.name, target, max)
		})
	}
}

// requireBumpClearedMax asserts both halves of the contract: the bumped row
// sits strictly above the pre-bump library max, AND it actually surfaces in
// a since-delta taken at that max (the value check alone would pass for a
// row the read path filters out for some other reason).
func requireBumpClearedMax(t *testing.T, s *Store, writer, target string, max int64) {
	t.Helper()
	if got := indexedAtOf(t, s, target); got <= max {
		t.Fatalf("%s left indexed_at at %d, not past the library max %d — "+
			"a client whose cursor is %d never sees the change", writer, got, max, max)
	}
	since := time.Unix(0, max).UTC()
	delta, err := s.ListTracks(context.Background(), &since)
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range delta {
		if tr.Path == target {
			return
		}
	}
	t.Fatalf("%s: target absent from the since-delta (got %d rows)", writer, len(delta))
}

// flattenIndexedAt forces a row's indexed_at to an exact value so a bump's
// starting point is deterministic. Test-only: production writes always go
// through indexedAtAdvanceSQL.
func flattenIndexedAt(t *testing.T, s *Store, path string, v int64) {
	t.Helper()
	if _, err := s.db.Exec(`UPDATE tracks SET indexed_at = ? WHERE path = ?`, v, path); err != nil {
		t.Fatalf("flattenIndexedAt(%q): %v", path, err)
	}
}

// TestIndexedAtAdvanceIsShared is what keeps the six delta-visibility bump
// statements in step.
//
// The advance expression is written out verbatim in each rather than
// concatenated in (see indexedAtAdvanceSQL — a concatenated form trips
// SonarCloud go:S2077 and reads as an assembled query), so nothing at the
// language level stops them drifting. This test does. Mirrors
// TestEnrichmentMissPredicateIsShared, which exists for the same reason.
//
// Drift here is not cosmetic: a statement that silently reverts to the old
// `CASE WHEN indexed_at >= ? THEN indexed_at + 1 ELSE ? END` advances only
// past its OWN row and can land exactly on a client's cursor.
func TestIndexedAtAdvanceIsShared(t *testing.T) {
	for name, stmt := range map[string]string{
		"bumpIndexedAtByPathSQL":  bumpIndexedAtByPathSQL,
		"markEnrichedSQL":         markEnrichedSQL,
		"applyReconciledTrackSQL": applyReconciledTrackSQL,
		"setArtworkVersionSQL":    setArtworkVersionSQL,
		"setBookletTagSQL":        setBookletTagSQL,
		"applyDupeStampBumpSQL":   applyDupeStampBumpSQL,
	} {
		// Whitespace-normalised: the statements nest the expression at
		// different depths and align their SET columns differently, so
		// forcing identical layout would couple the guard to formatting
		// rather than to meaning. Any SEMANTIC edit still fails.
		if !strings.Contains(squashSpace(stmt), squashSpace(indexedAtAdvanceSQL)) {
			t.Errorf("%s no longer embeds the shared indexed_at advance —\n"+
				"this writer's bump may no longer clear the library-wide max.\nStatement:\n%s\n\nwant it to contain:\n%s",
				name, stmt, indexedAtAdvanceSQL)
		}
	}
}

// TestLyricsBumpClearsLibraryWideMax pins the delta-visibility contract for the
// lyrics row's bump: it must clear the LIBRARY-WIDE max, not merely advance
// past the bumped row's own prior value.
//
// The regression this catches shipped in PR #840 and is the exact form
// indexedAtAdvanceSQL's docblock calls out as dead: a hand-rolled
// `CASE WHEN indexed_at >= ? THEN indexed_at + 1 ELSE ? END`. Its ELSE arm
// assigns the raw clock, so when the caller's clock sits at or below a value
// another row already holds, the bumped row lands on or below a cursor a
// client already has and `indexed_at > since` drops it — the track that just
// gained lyrics never reaches the phone.
//
// StampExtractorVersionBatch is exactly that caller shape: it computes `now`
// ONCE for the whole batch and loops, while the interleaved UpsertTrackBatch
// leg is pushing the library max ahead via MAX+1. Windows' ~15.6 ms clock
// granularity makes the collision routine; nanosecond clocks hide it, which is
// why this test pins the clock instead of racing it.
func TestLyricsBumpClearsLibraryWideMax(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })

	const t0 int64 = 1_000_000_000
	s.now = func() time.Time { return time.Unix(0, t0) }
	upsertParent(t, s, "Music/A/ahead.flac")
	upsertParent(t, s, "Music/A/lyrical.flac")

	// Another row carries the library-wide max — the state UpsertTrackBatch's
	// MAX+1 arm produces while a stamp batch runs against an older `now`.
	const ahead = t0 + 100_000
	if _, err := s.db.Exec(`UPDATE tracks SET indexed_at = ? WHERE path = ?`,
		ahead, "Music/A/ahead.flac"); err != nil {
		t.Fatalf("seed library max: %v", err)
	}

	var before int64
	if err := s.db.QueryRow(`SELECT indexed_at FROM tracks WHERE path = ?`,
		"Music/A/lyrical.flac").Scan(&before); err != nil {
		t.Fatalf("read indexed_at: %v", err)
	}
	if before >= ahead {
		t.Fatalf("fixture broken: subject row %d already at/above the library max %d", before, ahead)
	}

	// The batch clock is BELOW the library max — a coarse clock, or simply a
	// batch whose `now` was taken before the other leg advanced the max.
	s.now = func() time.Time { return time.Unix(0, t0+10) }
	if err := s.StampExtractorVersionBatch(context.Background(), []*Track{{
		Path: "Music/A/lyrical.flac",
		lyrics: &extractedLyrics{
			Format: "lrc", Synced: true, Body: "[00:01.000]hello",
			Source: "sylt", Tag: "deadbeef",
		},
	}}); err != nil {
		t.Fatalf("StampExtractorVersionBatch: %v", err)
	}

	var after int64
	if err := s.db.QueryRow(`SELECT indexed_at FROM tracks WHERE path = ?`,
		"Music/A/lyrical.flac").Scan(&after); err != nil {
		t.Fatalf("read indexed_at after stamp: %v", err)
	}
	if after <= before {
		t.Errorf("lyrics bump did not advance the row at all: before=%d after=%d", before, after)
	}
	if after <= ahead {
		t.Errorf("lyrics bump did not clear the library-wide max: after=%d, other row holds %d.\n"+
			"A client whose cursor is %d will never receive this track's lyrics.", after, ahead, ahead)
	}
}

// TestNoHandRolledIndexedAtBump sweeps store.go for every `indexed_at =`
// assignment and fails on any that is neither the shared advance nor one of
// the two exclusions indexedAtAdvanceSQL's docblock names.
//
// TestIndexedAtAdvanceIsShared cannot see this class: it walks a map of named
// CONSTS, and PR #840's regression was an inline string literal inside a
// function body, so it was never a candidate. The guard that pins a convention
// has to sweep the same surface a new writer is actually written on.
func TestNoHandRolledIndexedAtBump(t *testing.T) {
	src, err := os.ReadFile("store.go")
	if err != nil {
		t.Fatalf("read store.go: %v", err)
	}
	text := string(src)
	shared := squashSpace(indexedAtAdvanceSQL)

	assign := regexp.MustCompile(`indexed_at\s*=`)
	locs := assign.FindAllStringIndex(text, -1)
	if len(locs) == 0 {
		t.Fatal("no `indexed_at =` assignments found — the sweep is looking at the wrong file")
	}
	checked := 0
	for _, loc := range locs {
		// Skip prose: the docblocks discuss both forms by name.
		lineStart := strings.LastIndexByte(text[:loc[0]], '\n') + 1
		if strings.HasPrefix(strings.TrimSpace(text[lineStart:loc[0]]), "//") {
			continue
		}
		checked++
		end := loc[1] + 320
		if end > len(text) {
			end = len(text)
		}
		window := squashSpace(text[loc[0]:end])
		switch {
		case strings.Contains(window, shared):
			// The shared advance, written out verbatim.
		case strings.Contains(window, "excluded.indexed_at"):
			// The UpsertTrack / UpsertTrackBatch conflict arms (and the
			// track_lyrics insert, which carries a value computed elsewhere).
		case strings.Contains(window, "track_analysis"):
			// healTransitionBandBandwidths, migration v34's post(): frozen,
			// append-only, both live bridges already ran it.
		default:
			line := 1 + strings.Count(text[:loc[0]], "\n")
			t.Errorf("store.go:%d — hand-rolled indexed_at assignment.\n"+
				"Every delta-visibility bump uses indexedAtAdvanceSQL (or\n"+
				"bumpIndexedAtByPathSQL when the bump is the whole statement);\n"+
				"the only exclusions are the upsert conflict arms and migration\n"+
				"v34's post(). See indexedAtAdvanceSQL's docblock.\nSaw: %s",
				line, window[:min(180, len(window))])
		}
	}
	if checked < 5 {
		t.Fatalf("only %d assignments classified — the sweep has gone inert", checked)
	}
}
