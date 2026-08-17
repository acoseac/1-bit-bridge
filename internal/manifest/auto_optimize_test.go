package manifest

import (
	"context"
	"testing"
	"time"
)

// seedOptimizeTrack inserts a track row with the v25 format columns
// populated (the auto-optimize predicate is plain-column, so the
// pointers must be set for the row to be selectable at all).
func seedOptimizeTrack(t *testing.T, s *Store, path string, rate float64, bits int, codec string, isDSD bool) {
	t.Helper()
	r, b, d := rate, bits, isDSD
	if err := s.UpsertTrack(context.Background(), &Track{
		Path:          path,
		Size:          1_000_000,
		ModTime:       time.Unix(1700000000, 0),
		SampleRate:    &r,
		BitsPerSample: &b,
		Codec:         codec,
		IsDSD:         &d,
	}); err != nil {
		t.Fatalf("UpsertTrack(%q): %v", path, err)
	}
}

// trackRowMTimeAndSize reads back what the scanner actually stamped, so
// a "fresh variant" fixture records the same values the predicate
// compares against (rather than the test's own guess at them).
func trackRowMTimeAndSize(t *testing.T, s *Store, path string) (int64, int64) {
	t.Helper()
	var mtime, size int64
	if err := s.db.QueryRow(`SELECT mtime_ns, size FROM tracks WHERE path = ?`, path).
		Scan(&mtime, &size); err != nil {
		t.Fatalf("read track row %q: %v", path, err)
	}
	return mtime, size
}

func seedOptimizeVariant(t *testing.T, s *Store, path, variantID string, srcMTime, srcSize int64) {
	t.Helper()
	if err := s.UpsertVariant(context.Background(), VariantRow{
		SourcePath:    path,
		VariantID:     variantID,
		SidecarPath:   "/variants/" + variantID + ".flac",
		Format:        "flac",
		SampleRate:    44100,
		BitsPerSample: 16,
		SizeBytes:     300_000,
		SourceMTimeNS: srcMTime,
		SourceSize:    srcSize,
		SoxSettings:   "test",
		CreatedAt:     time.Now().UnixNano(),
	}); err != nil {
		t.Fatalf("UpsertVariant(%q): %v", path, err)
	}
}

func candidatePaths(t *testing.T, s *Store, limit int) []string {
	t.Helper()
	cands, err := s.ListAutoOptimizeCandidates(context.Background(), limit)
	if err != nil {
		t.Fatalf("ListAutoOptimizeCandidates: %v", err)
	}
	out := make([]string, 0, len(cands))
	for _, c := range cands {
		out = append(out, c.Path)
	}
	return out
}

// TestListAutoOptimizeCandidatesSelectionContract pins every arm of the
// candidate predicate in one fixture, because the arms interact: an
// over-broad predicate spends disk and CPU on tracks that can never be
// served, and an over-narrow one silently pre-generates nothing.
//
// Negative-control-verified: removing the UPnP anti-join, the
// dupe_suppressed arm, or the staleness clause each turns a distinct
// sub-assertion below red.
func TestListAutoOptimizeCandidatesSelectionContract(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	const (
		eligible     = "Music/A/Album/eligible.flac"   // 96/24 FLAC, no variant → WANT
		atFloor      = "Music/A/Album/at-floor.flac"   // 44.1/16 → already CarPlay-ready
		lossy        = "Music/A/Album/lossy.mp3"       // not PCM
		dsd          = "Music/A/Album/dsd.dsf"         // DSD
		routed       = "Music/Upstream/routed.flac"    // UPnP-routed → no local file
		suppressed   = "Music/A/Album/suppressed.flac" // duplicate-suppressed → never served
		covered      = "Music/A/Album/covered.flac"    // fresh variant → nothing to do
		staleVariant = "Music/A/Album/stale.flac"      // variant recorded against an older file
		zeroByte     = "Music/A/Album/zero.flac"       // truncated upload
	)

	seedOptimizeTrack(t, s, eligible, 96000, 24, "FLAC", false)
	seedOptimizeTrack(t, s, atFloor, 44100, 16, "FLAC", false)
	seedOptimizeTrack(t, s, lossy, 96000, 24, "MP3", false)
	seedOptimizeTrack(t, s, dsd, 2822400, 1, "DSF", true)
	seedOptimizeTrack(t, s, routed, 96000, 24, "FLAC", false)
	seedOptimizeTrack(t, s, suppressed, 96000, 24, "FLAC", false)
	seedOptimizeTrack(t, s, covered, 96000, 24, "FLAC", false)
	seedOptimizeTrack(t, s, staleVariant, 96000, 24, "FLAC", false)

	// Zero-byte source: UpsertTrack with Size 0.
	zr, zb, zd := 96000.0, 24, false
	if err := s.UpsertTrack(ctx, &Track{
		Path: zeroByte, Size: 0, ModTime: time.Unix(1700000000, 0),
		SampleRate: &zr, BitsPerSample: &zb, Codec: "FLAC", IsDSD: &zd,
	}); err != nil {
		t.Fatalf("UpsertTrack(zero-byte): %v", err)
	}

	// Route one track from a fake upstream.
	if _, err := s.db.Exec(`
		INSERT INTO upnp_track_routing (source_path, server_udn, object_id, res_url, last_seen_at)
		VALUES (?, 'udn:test', 'obj-1', 'http://upstream/1.flac', ?)`,
		routed, time.Now().UnixNano()); err != nil {
		t.Fatalf("seed upnp routing: %v", err)
	}

	// Suppress one as a duplicate.
	if _, err := s.ApplyDupeStamps(ctx, []DupeStamp{{
		Path: suppressed, GroupID: "g1", Tier: "identical-audio", Suppressed: true,
	}}); err != nil {
		t.Fatalf("ApplyDupeStamps: %v", err)
	}

	// A FRESH variant recorded against the current track row.
	cm, cs := trackRowMTimeAndSize(t, s, covered)
	seedOptimizeVariant(t, s, covered, "optimized-v2-48000-16", cm, cs)

	// A STALE variant: same variant ID, but recorded against an older
	// version of the file (source has since been re-encoded).
	sm, ss := trackRowMTimeAndSize(t, s, staleVariant)
	seedOptimizeVariant(t, s, staleVariant, "optimized-v2-48000-16", sm-1, ss-42)

	cands, err := s.ListAutoOptimizeCandidates(ctx, 100)
	if err != nil {
		t.Fatalf("ListAutoOptimizeCandidates: %v", err)
	}
	got := make([]string, 0, len(cands))
	byPath := make(map[string]AutoOptimizeCandidate, len(cands))
	for _, c := range cands {
		got = append(got, c.Path)
		byPath[c.Path] = c
	}

	// One row per fixture, each naming WHY it belongs (or doesn't) — the
	// reason is the assertion's whole value when a predicate arm regresses.
	selection := []struct {
		path string
		want bool
		why  string
	}{
		{eligible, true, "hi-res PCM with no variant"},
		{staleVariant, true, "variant recorded against an older version of the file"},
		{atFloor, false, "already at the CarPlay floor (44.1/16)"},
		{lossy, false, "lossy source — not PCM"},
		{dsd, false, "DSD is structurally excluded"},
		{routed, false, "UPnP-routed: no local file to decode"},
		{suppressed, false, "duplicate-suppressed: never served, so the variant could never be requested"},
		{covered, false, "a fresh variant already covers it"},
		{zeroByte, false, "zero-byte source: sox cannot probe it"},
	}
	wantCount := 0
	for _, c := range selection {
		if _, present := byPath[c.path]; present != c.want {
			verb := "should be selected"
			if !c.want {
				verb = "should be excluded"
			}
			t.Errorf("%q %s (%s); candidates = %v", c.path, verb, c.why, got)
		}
		if c.want {
			wantCount++
		}
	}
	if len(cands) != wantCount {
		t.Errorf("candidate count = %d (%v), want exactly %d", len(cands), got, wantCount)
	}

	// The stale row must announce itself as a regeneration so the sweeper
	// can log it as such; the first-generation row must not.
	if c := byPath[staleVariant]; c.StaleVariantID != "optimized-v2-48000-16" {
		t.Errorf("stale candidate StaleVariantID = %q, want the existing variant id", c.StaleVariantID)
	}
	if c := byPath[eligible]; c.StaleVariantID != "" {
		t.Errorf("first-generation candidate StaleVariantID = %q, want empty", c.StaleVariantID)
	}
	if c := byPath[eligible]; c.SampleRate != 96000 || c.BitsPerSample != 24 || c.Codec != "FLAC" {
		t.Errorf("candidate geometry = %d/%d %q, want 96000/24 FLAC",
			c.SampleRate, c.BitsPerSample, c.Codec)
	}

	// Count agrees with the (uncapped) listing — the card and the work
	// must not be able to disagree.
	n, cerr := s.CountAutoOptimizeCandidates(ctx)
	if cerr != nil {
		t.Fatalf("CountAutoOptimizeCandidates: %v", cerr)
	}
	if n != wantCount {
		t.Errorf("CountAutoOptimizeCandidates = %d, want %d", n, wantCount)
	}
}

// TestListAutoOptimizeCandidatesOrderAndLimit pins newest-indexed-first
// ordering and the cap. Ordering is load-bearing: under a per-sweep cap
// or a disk floor, the head of the queue is what actually gets built, so
// it must be the freshly added music (the literal ask) rather than an
// arbitrary path order.
func TestListAutoOptimizeCandidatesOrderAndLimit(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	// Insert oldest-first so path order and indexed_at order disagree —
	// otherwise the ORDER BY tie-breaker could pass by accident.
	paths := []string{
		"Music/Z/oldest.flac",
		"Music/M/middle.flac",
		"Music/A/newest.flac",
	}
	for i, p := range paths {
		seedOptimizeTrack(t, s, p, 96000, 24, "FLAC", false)
		// Force a distinct, increasing indexed_at per row.
		if _, err := s.db.Exec(`UPDATE tracks SET indexed_at = ? WHERE path = ?`,
			int64(1000+i), p); err != nil {
			t.Fatalf("set indexed_at: %v", err)
		}
	}

	got := candidatePaths(t, s, 100)
	want := []string{"Music/A/newest.flac", "Music/M/middle.flac", "Music/Z/oldest.flac"}
	for i := range want {
		if i >= len(got) || got[i] != want[i] {
			t.Fatalf("candidate order = %v, want %v (newest indexed first)", got, want)
		}
	}

	if capped := candidatePaths(t, s, 2); len(capped) != 2 {
		t.Errorf("limit 2 returned %d rows: %v", len(capped), capped)
	} else if capped[0] != want[0] || capped[1] != want[1] {
		t.Errorf("capped listing = %v, want the two newest %v", capped, want[:2])
	}

	// A non-positive limit must return nothing rather than everything:
	// the cap is a safety property, so an unset one must not read as
	// "unbounded" (see config.AutoOptimizeConfig.MaxPerSweep).
	for _, lim := range []int{0, -1} {
		if got := candidatePaths(t, s, lim); len(got) != 0 {
			t.Errorf("limit %d returned %d rows, want 0", lim, len(got))
		}
	}
	// The COUNT twin is deliberately uncapped.
	n, err := s.CountAutoOptimizeCandidates(ctx)
	if err != nil {
		t.Fatalf("CountAutoOptimizeCandidates: %v", err)
	}
	if n != len(paths) {
		t.Errorf("CountAutoOptimizeCandidates = %d, want %d", n, len(paths))
	}
}
