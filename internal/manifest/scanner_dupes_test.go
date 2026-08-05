package manifest

import (
	"context"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/dupes"
)

// dupeScanner builds a Scanner over the store with an injectable policy.
func dupeScanner(t *testing.T, s *Store, mode *dupes.FilterMode) *Scanner {
	t.Helper()
	sc := NewScanner([]string{t.TempDir()}, s, "")
	sc.SetDupePolicy(func() dupes.Policy { return dupes.Policy{Mode: *mode} })
	return sc
}

// seedDupePair seeds two same-format copies (larger wins) plus one
// unrelated track.
func seedDupePair(t *testing.T, s *Store) {
	t.Helper()
	mk := func(path string, size int64, rate float64, bits int, codec string, isDSD bool) {
		tr := &Track{
			Path: path, Size: size, ModTime: time.Unix(0, 0).UTC(),
			Title: "Song", Artist: "Artist", AlbumArtist: "Artist", Album: "Album",
			TrackNumber: intptr(1), DiscNumber: intptr(1), Year: intptr(2020),
			Duration: f64ptr(200), SampleRate: f64ptr(rate),
			BitsPerSample: intptr(bits), IsDSD: boolPtr(isDSD), Codec: codec,
		}
		if err := s.UpsertTrack(context.Background(), tr); err != nil {
			t.Fatalf("seed %q: %v", path, err)
		}
	}
	mk("CopyA/Album/01 Song.flac", 900, 44100, 16, "FLAC", false)
	mk("CopyB/Album/01 Song.flac", 1000, 44100, 16, "FLAC", false) // larger → winner
	solo := &Track{Path: "Other/Album/02 Other.flac", Size: 5, ModTime: time.Unix(0, 0).UTC(),
		Title: "Other", Artist: "Artist", Album: "Album", TrackNumber: intptr(2)}
	if err := s.UpsertTrack(context.Background(), solo); err != nil {
		t.Fatal(err)
	}
}

func TestRestampDuplicates_EndToEnd(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	ctx := context.Background()
	mode := dupes.FilterHighestQuality
	sc := dupeScanner(t, s, &mode)
	seedDupePair(t, s)

	n, err := sc.RestampDuplicates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("first pass changed %d rows, want 2 (both group members stamped)", n)
	}
	if st := stampOf(t, s, "CopyA/Album/01 Song.flac"); !st.Suppressed || st.Tier != string(dupes.TierSameFormat) {
		t.Fatalf("smaller copy must be suppressed: %+v", st)
	}
	winner := stampOf(t, s, "CopyB/Album/01 Song.flac")
	if winner.Suppressed || winner.GroupID == "" {
		t.Fatalf("winner must be stamped served: %+v", winner)
	}
	if st := stampOf(t, s, "Other/Album/02 Other.flac"); st.GroupID != "" {
		t.Fatalf("non-duplicate row must stay unstamped: %+v", st)
	}
	// Served manifest excludes exactly the loser; count matches.
	served, err := s.ListServedTracks(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(served) != 2 {
		t.Fatalf("served rows = %d, want 2", len(served))
	}
	total, _ := s.CountServedTracks(ctx)
	if total != 2 {
		t.Fatalf("CountServedTracks = %d, want 2", total)
	}

	// Stable second pass: zero writes (the reconciliation-idle contract).
	n, err = sc.RestampDuplicates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("stable second pass changed %d rows, want 0", n)
	}

	// Summary persisted and consistent.
	sum, err := s.LoadDupeSummary(ctx)
	if err != nil || sum == nil {
		t.Fatalf("summary: %v %v", sum, err)
	}
	if sum.Groups != 1 || sum.Suppressed != 1 || sum.Served != sum.Scanned-1 ||
		sum.Policy != string(dupes.FilterHighestQuality) {
		t.Fatalf("summary mismatch: %+v", sum)
	}
}

// TestRestampDuplicates_PolicyFlipUnsuppressesViaDelta pins the
// hot-apply story end-to-end: flipping to off clears suppression AND
// strict-advances indexed_at so a delta-syncing client recovers the row.
func TestRestampDuplicates_PolicyFlipUnsuppressesViaDelta(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	ctx := context.Background()
	mode := dupes.FilterHighestQuality
	sc := dupeScanner(t, s, &mode)
	seedDupePair(t, s)
	if _, err := sc.RestampDuplicates(ctx); err != nil {
		t.Fatal(err)
	}
	// A fully-synced client's cursor: the max indexed_at across the
	// library after the first stamping pass.
	var watermark int64
	for _, p := range []string{"CopyA/Album/01 Song.flac", "CopyB/Album/01 Song.flac", "Other/Album/02 Other.flac"} {
		if v := indexedAtOf(t, s, p); v > watermark {
			watermark = v
		}
	}
	since := time.Unix(0, watermark).UTC()

	mode = dupes.FilterOff
	n, err := sc.RestampDuplicates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("flip to off changed %d rows, want 1 (the suppressed copy)", n)
	}
	if st := stampOf(t, s, "CopyA/Album/01 Song.flac"); st.Suppressed {
		t.Fatalf("policy off must clear suppression: %+v", st)
	}
	// The group stamp survives (stats stay populated with the filter off).
	if st := stampOf(t, s, "CopyA/Album/01 Song.flac"); st.GroupID == "" {
		t.Fatalf("group stamp must survive policy off: %+v", st)
	}
	delta, err := s.ListServedTracks(ctx, &since)
	if err != nil {
		t.Fatal(err)
	}
	if len(delta) != 1 || delta[0].Path != "CopyA/Album/01 Song.flac" {
		t.Fatalf("un-suppressed row must surface in the served delta, got %+v", delta)
	}
}

// TestRestampDuplicates_StaleStampCleared: a stamped row whose twin
// vanished falls out of every group — the pass clears it and, because it
// was suppressed, pushes it back into the delta stream.
func TestRestampDuplicates_StaleStampCleared(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	ctx := context.Background()
	mode := dupes.FilterHighestQuality
	sc := dupeScanner(t, s, &mode)
	seedDupePair(t, s)
	if _, err := sc.RestampDuplicates(ctx); err != nil {
		t.Fatal(err)
	}
	before := indexedAtOf(t, s, "CopyA/Album/01 Song.flac")
	if err := s.DeleteTrack(ctx, "CopyB/Album/01 Song.flac"); err != nil {
		t.Fatal(err)
	}
	if _, err := sc.RestampDuplicates(ctx); err != nil {
		t.Fatal(err)
	}
	st := stampOf(t, s, "CopyA/Album/01 Song.flac")
	if st.GroupID != "" || st.Suppressed {
		t.Fatalf("orphaned stamp must be cleared: %+v", st)
	}
	if after := indexedAtOf(t, s, "CopyA/Album/01 Song.flac"); after <= before {
		t.Fatalf("previously-suppressed orphan must re-enter the delta (indexed_at %d → %d)", before, after)
	}
}

// TestRestampDuplicates_UnwiredPolicySuppressesNothing pins the
// fail-open default for scanners without SetDupePolicy wiring (tests,
// bare CLI scans): groups are stamped, nothing is hidden.
func TestRestampDuplicates_UnwiredPolicySuppressesNothing(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	ctx := context.Background()
	sc := NewScanner([]string{t.TempDir()}, s, "")
	seedDupePair(t, s)
	if _, err := sc.RestampDuplicates(ctx); err != nil {
		t.Fatal(err)
	}
	if st := stampOf(t, s, "CopyA/Album/01 Song.flac"); st.Suppressed {
		t.Fatal("unwired scanner must never suppress")
	}
	if st := stampOf(t, s, "CopyA/Album/01 Song.flac"); st.GroupID == "" {
		t.Fatal("unwired scanner still stamps groups (stats work with filter off)")
	}
	n, _ := s.CountServedTracks(ctx)
	if n != 3 {
		t.Fatalf("everything stays served, got %d", n)
	}
}

// TestRestampDuplicates_RoutedRowsNeverStamped: UPnP-routed rows are
// excluded from grouping entirely — even a byte-identical routed twin
// neither gets stamped nor drags the filesystem row into a group.
func TestRestampDuplicates_RoutedRowsNeverStamped(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	ctx := context.Background()
	mode := dupes.FilterHighestQuality
	sc := dupeScanner(t, s, &mode)
	seedDupePair(t, s)
	seedRoutedTrack(t, s, "2go/Server/x.flac")
	if _, err := sc.RestampDuplicates(ctx); err != nil {
		t.Fatal(err)
	}
	if st := stampOf(t, s, "2go/Server/x.flac"); st.GroupID != "" || st.Suppressed {
		t.Fatalf("routed row must never be stamped: %+v", st)
	}
}
