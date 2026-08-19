package manifest

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The threshold reap is the ONLY track-deletion path that fires in normal
// operation — a user deleting, moving, renaming or re-ripping a file ends
// here. `DeleteTrack` has no production caller at all, and
// `DeleteTracksBatch` serves only case-only renames and the UPnP reap.
//
// It therefore has to honour the same "no orphans on disk" contract as its
// five siblings: CASCADE removes the `track_variants` / `track_analysis`
// rows along with the parent, so a sidecar file that wasn't enumerated
// BEFORE the DELETE is unreachable by path from that moment on. The
// forward orphan sweeper that would otherwise reclaim it is opt-in and
// off by default, so an un-enumerated file leaks until the operator runs
// `bridge upscale --gc` by hand.
//
// PR #193 skipped the cleanup here deliberately, on a cost model that
// doesn't hold for the set-scoped form (see the docblock on
// IncrementMissingTracksAndDeleteAtThreshold). These tests pin the
// reversal.

// seedReapFixture writes one track with a variant sidecar and a waveform
// sidecar, both as real files on disk, and returns their paths.
func seedReapFixture(t *testing.T, ctx context.Context, s *Store, dir, path string) (variant, waveform string) {
	t.Helper()
	rate, bits, isDSD := 44100.0, 16, false
	if err := s.UpsertTrack(ctx, &Track{
		Path: path, Size: 1_000_000, SampleRate: &rate,
		BitsPerSample: &bits, Codec: "FLAC", IsDSD: &isDSD,
	}); err != nil {
		t.Fatalf("UpsertTrack %q: %v", path, err)
	}

	// The sidecar FILENAME is independent of the track path, and must stay
	// filesystem-legal: APFS rejects invalid UTF-8 in filenames, while the
	// stored track path is free to carry it (that is the whole point of the
	// ill-formed-path case below). Derive an ASCII-safe name rather than
	// echoing the path's bytes.
	var b strings.Builder
	for _, r := range path {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	safe := b.String()
	variant = filepath.Join(dir, safe+".variant.flac")
	waveform = filepath.Join(dir, safe+".waveform.bin")
	for _, p := range []string{variant, waveform} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatalf("write sidecar %q: %v", p, err)
		}
	}

	if err := s.UpsertVariant(ctx, VariantRow{
		SourcePath: path, VariantID: "upscaled-v2-192000-24",
		SidecarPath: variant, Format: "flac",
		SampleRate: 192000, BitsPerSample: 24, SizeBytes: 1_500_000,
		SourceMTimeNS: 1, SourceSize: 1_000_000,
		SoxSettings: "{}", CreatedAt: 1,
	}); err != nil {
		t.Fatalf("UpsertVariant %q: %v", path, err)
	}
	if err := s.UpsertAnalysis(ctx, AnalysisRow{
		SourcePath: path, WaveformPath: waveform, WaveformTag: "deadbeef",
		WaveformSize: 1, SourceMTimeNS: 1, SourceSize: 1_000_000,
		SchemaVersion: "wf7", CreatedAt: 1,
	}); err != nil {
		t.Fatalf("UpsertAnalysis %q: %v", path, err)
	}
	return variant, waveform
}

func mustGone(t *testing.T, label, p string) {
	t.Helper()
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Errorf("%s sidecar %q still on disk after the reap (err=%v) — "+
			"CASCADE dropped its DB row, so nothing can find it by path again", label, p, err)
	}
}

func mustPresent(t *testing.T, label, p string) {
	t.Helper()
	if _, err := os.Stat(p); err != nil {
		t.Errorf("%s sidecar %q was removed but its track was NOT reaped: %v", label, p, err)
	}
}

// reapToThreshold runs the missing-count pass `threshold` times so the rows
// cross the reap line, and returns the deleted count from the final pass.
func reapToThreshold(t *testing.T, ctx context.Context, s *Store, paths []string, threshold int) int64 {
	t.Helper()
	var deleted int64
	for i := 0; i < threshold; i++ {
		n, err := s.IncrementMissingTracksAndDeleteAtThreshold(ctx, paths, threshold)
		if err != nil {
			t.Fatalf("IncrementMissingTracksAndDeleteAtThreshold pass %d: %v", i+1, err)
		}
		deleted = n
	}
	return deleted
}

func TestThresholdReapRemovesSidecarFiles(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := openTestStore(t)

	variant, waveform := seedReapFixture(t, ctx, s, dir, "Artist/Album/gone.flac")
	// A second track that stays present — its sidecars must survive, so the
	// test can't pass by unlinking everything.
	keepVariant, keepWaveform := seedReapFixture(t, ctx, s, dir, "Artist/Album/stays.flac")

	if n := reapToThreshold(t, ctx, s, []string{"Artist/Album/gone.flac"}, 3); n != 1 {
		t.Fatalf("deleted = %d, want 1 (the reap itself didn't happen — the rest of this test proves nothing)", n)
	}

	mustGone(t, "variant", variant)
	mustGone(t, "waveform", waveform)
	mustPresent(t, "untouched variant", keepVariant)
	mustPresent(t, "untouched waveform", keepWaveform)
}

// A path that isn't valid UTF-8 can't travel through the JSON array, so it
// takes the per-path fallback arm. That arm needs its own enumeration —
// without one it is the single reaped population whose sidecars still leak,
// and it is exactly the population that already needs special handling.
func TestThresholdReapRemovesSidecarFilesForIllFormedUTF8Paths(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := openTestStore(t)

	bad := "Artist/Album/\xff\xfe.flac"
	variant, waveform := seedReapFixture(t, ctx, s, dir, bad)

	if n := reapToThreshold(t, ctx, s, []string{bad}, 3); n != 1 {
		t.Fatalf("deleted = %d, want 1 — the ill-formed path never reached the fallback DELETE", n)
	}

	mustGone(t, "variant", variant)
	mustGone(t, "waveform", waveform)
}

// A row below the threshold must keep its files: the enumeration has to
// share the DELETE's predicate, not merely its path set. Binding only the
// paths would unlink the sidecars of every still-live track that happened
// to be missing this pass — silent data loss of generated content.
func TestThresholdReapKeepsSidecarsBelowThreshold(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := openTestStore(t)

	variant, waveform := seedReapFixture(t, ctx, s, dir, "Artist/Album/flapping.flac")

	// One pass only, against a threshold of 3 — missing, but nowhere near reapable.
	n, err := s.IncrementMissingTracksAndDeleteAtThreshold(ctx, []string{"Artist/Album/flapping.flac"}, 3)
	if err != nil {
		t.Fatalf("IncrementMissingTracksAndDeleteAtThreshold: %v", err)
	}
	if n != 0 {
		t.Fatalf("deleted = %d, want 0 — a single missed scan must not reap", n)
	}

	mustPresent(t, "variant", variant)
	mustPresent(t, "waveform", waveform)
}

// UPnP-routed rows are never reapable (their lifecycle belongs to the
// ingest's last_seen_at reconcile), so the enumeration must not unlink
// their sidecars either. The routed anti-join is in the shared predicate
// precisely so both consumers inherit it.
func TestThresholdReapSparesRoutedRowSidecars(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := openTestStore(t)

	routed := "Upstream/Album/routed.flac"
	variant, waveform := seedReapFixture(t, ctx, s, dir, routed)
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO upnp_track_routing (source_path, server_udn, object_id, res_url, last_seen_at)
		 VALUES (?, 'udn-1', 'obj-1', 'http://x/1', 1)`, routed); err != nil {
		t.Fatalf("seed routing row: %v", err)
	}

	if n := reapToThreshold(t, ctx, s, []string{routed}, 3); n != 0 {
		t.Fatalf("deleted = %d, want 0 — a routed row must never be threshold-reaped", n)
	}

	mustPresent(t, "routed variant", variant)
	mustPresent(t, "routed waveform", waveform)
}

// TestThresholdReapPredicatesAreShared is what keeps the enumeration and the
// DELETE selecting the same rows.
//
// The predicates are written out at both consumers rather than assembled per
// call site, so nothing at the language level stops one copy drifting. Drift
// is not cosmetic in either direction: a DELETE that reaps rows the
// enumeration missed leaks their files forever, and an enumeration broader
// than the DELETE unlinks sidecars belonging to tracks that are still served.
//
// Mirrors TestIndexedAtAdvanceIsShared / TestEnrichmentMissPredicateIsShared,
// which exist for the same reason.
func TestThresholdReapPredicatesAreShared(t *testing.T) {
	// Each DELETE and its two sidecar enumerations are DERIVED from one
	// predicate const at compile time, so drift is a compile-time
	// impossibility rather than a review question. This asserts the derivation
	// still holds — i.e. nobody re-spelled a statement inline.
	//
	// Drift is not cosmetic in either direction: a DELETE that reaps rows the
	// enumeration missed leaks their files forever, and an enumeration broader
	// than the DELETE unlinks sidecars belonging to tracks still being served.
	for _, tc := range []struct{ name, stmt, want string }{
		{"deleteTracksAtThresholdBatchSQL", deleteTracksAtThresholdBatchSQL, thresholdReapBatchWhereSQL},
		{"reapVariantSidecarsBatchSQL", reapVariantSidecarsBatchSQL, thresholdReapBatchWhereSQL},
		{"reapWaveformWhereBatchSQL", reapWaveformWhereBatchSQL, thresholdReapBatchWhereSQL},
		{"deleteTracksAtThresholdOneSQL", deleteTracksAtThresholdOneSQL, thresholdReapOneWhereSQL},
		{"reapVariantSidecarsOneSQL", reapVariantSidecarsOneSQL, thresholdReapOneWhereSQL},
		{"reapWaveformWhereOneSQL", reapWaveformWhereOneSQL, thresholdReapOneWhereSQL},
	} {
		if !strings.Contains(squashSpace(tc.stmt), squashSpace(tc.want)) {
			t.Errorf("%s no longer embeds its reap predicate — the unlink set and the row set can now diverge.\nStatement:\n%s\n\nwant it to contain:\n%s",
				tc.name, tc.stmt, tc.want)
		}
	}

	// The routed anti-join is the guard no caller may bypass; it must live
	// INSIDE the shared predicates so every derived statement inherits it,
	// rather than being re-stated per statement.
	for _, want := range []string{
		squashSpace(thresholdReapBatchWhereSQL),
		squashSpace(thresholdReapOneWhereSQL),
	} {
		if !strings.Contains(want, "NOT IN (SELECT source_path FROM upnp_track_routing)") {
			t.Errorf("reap predicate lost its UPnP-routed anti-join:\n%s", want)
		}
	}
}
