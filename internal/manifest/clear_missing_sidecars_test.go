package manifest

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// TestClearMissingCountsRemovesTheSidecarsTheDeleteDrops: clearing a
// missing track drops its rendition and waveform files with the rows.
// dsd_peaks is a row, not a file, and leaves by the tracks cascade.
// A present track and a routed track keep their files. A missing track
// with no sidecars is still deleted, and it is still in the count.
func TestClearMissingCountsRemovesTheSidecarsTheDeleteDrops(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	dir := t.TempDir()
	peak := fpk(-1.2)

	presentVar, presentWave := seedReapFixture(t, ctx, s, dir, "keep/01.flac")
	goneVar, goneWave := seedReapFixture(t, ctx, s, dir, "gone/01.flac")
	bareVar, bareWave := seedReapFixture(t, ctx, s, dir, "bare/01.flac")
	routedVar, routedWave := seedReapFixture(t, ctx, s, dir, "routed/01.flac")
	seedRoutedTrack(t, s, "routed/01.flac")
	seedClearMissingPeak(t, ctx, s, "keep/01.flac", peak)
	seedClearMissingPeak(t, ctx, s, "gone/01.flac", peak)
	// The bare track is the no-sidecar case. Its fixture files are removed
	// and its rows dropped before the clear, so the command has nothing
	// on disk to unlink for it.
	dropSidecarRows(t, ctx, s, "bare/01.flac")
	os.Remove(bareVar)
	os.Remove(bareWave)
	markTrackMissing(t, ctx, s, "gone/01.flac")
	markTrackMissing(t, ctx, s, "bare/01.flac")
	markTrackMissing(t, ctx, s, "routed/01.flac")

	n, err := s.ClearMissingCounts(ctx)
	if err != nil {
		t.Fatalf("ClearMissingCounts: %v", err)
	}
	if n != 2 {
		t.Errorf("ClearMissingCounts returned %d, want 2 (the missing filesystem tracks, not the present one and not the routed one)", n)
	}

	mustGone(t, "cleared rendition", goneVar)
	mustGone(t, "cleared waveform", goneWave)
	mustPresent(t, "present rendition", presentVar)
	mustPresent(t, "present waveform", presentWave)
	mustPresent(t, "routed rendition", routedVar)
	mustPresent(t, "routed waveform", routedWave)

	if tr, _ := s.GetTrack(ctx, "gone/01.flac"); tr != nil {
		t.Error("missing track survived clear-missing")
	}
	if tr, _ := s.GetTrack(ctx, "bare/01.flac"); tr != nil {
		t.Error("missing track with no sidecars survived clear-missing")
	}
	if tr, _ := s.GetTrack(ctx, "keep/01.flac"); tr == nil {
		t.Error("present track was cleared")
	}
	if tr, _ := s.GetTrack(ctx, "routed/01.flac"); tr == nil {
		t.Error("routed track was cleared")
	}
	if row, err := s.GetAnalysis(ctx, "gone/01.flac"); err != nil || row != nil {
		t.Errorf("cleared analysis row = %v, %v", row, err)
	}
	if got := countDSDPeaks(t, s); got != 1 {
		t.Errorf("dsd_peaks = %d, want 1 (the present track; the cleared peak row has no file)", got)
	}
}

// TestClearMissingCountsLogsASidecarItCannotRemove: a remove that the
// filesystem refuses is a warning. The command still succeeds, the row
// is still gone, and the file that could not be removed stays for the
// orphan sweep. A waveform beside it, which can be removed, is removed.
func TestClearMissingCountsLogsASidecarItCannotRemove(t *testing.T) {
	rec := loggingtest.Record(t)
	s := openTestStore(t)
	ctx := context.Background()
	dir := t.TempDir()
	blocked := filepath.Join(dir, "blocked.flac")
	if err := os.Mkdir(blocked, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blocked, "child"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	wave := filepath.Join(dir, "gone.waveform.bin")
	if err := os.WriteFile(wave, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertTrack(ctx, &Track{Path: "gone/01.flac", Size: 1_000_000, Codec: "FLAC"}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertVariant(ctx, VariantRow{
		SourcePath: "gone/01.flac", VariantID: "upscaled-v2-192000-24",
		SidecarPath: blocked, Format: "flac",
		SampleRate: 192000, BitsPerSample: 24, SizeBytes: 1,
		SourceMTimeNS: 1, SourceSize: 1_000_000, SoxSettings: "{}", CreatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertAnalysis(ctx, AnalysisRow{
		SourcePath: "gone/01.flac", WaveformPath: wave, WaveformTag: "deadbeef",
		WaveformSize: 1, SourceMTimeNS: 1, SourceSize: 1_000_000,
		SchemaVersion: "wf7", CreatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	markTrackMissing(t, ctx, s, "gone/01.flac")

	n, err := s.ClearMissingCounts(ctx)
	if err != nil {
		t.Fatalf("ClearMissingCounts: %v", err)
	}
	if n != 1 {
		t.Errorf("returned %d, want 1", n)
	}
	if tr, _ := s.GetTrack(ctx, "gone/01.flac"); tr != nil {
		t.Error("row survived a remove the filesystem refused")
	}
	if _, err := os.Stat(blocked); err != nil {
		t.Errorf("blocked directory gone: %v", err)
	}
	mustGone(t, "waveform beside a blocked rendition", wave)
	if got := rec.Failures("remove sidecar"); len(got) != 1 {
		t.Errorf("remove warnings = %v, want one", got)
	}
}

func seedClearMissingPeak(t *testing.T, ctx context.Context, s *Store, path string, peak *float64) {
	t.Helper()
	if err := s.UpsertDSDPeak(ctx, DSDPeak{
		SourcePath: path, Profile: "a1|compact|44100|-v",
		TruePeakDBTP: peak, SourceMTimeNS: 1, SourceSize: 1_000_000, MeasuredAt: 1,
	}); err != nil {
		t.Fatalf("UpsertDSDPeak %s: %v", path, err)
	}
}

func dropSidecarRows(t *testing.T, ctx context.Context, s *Store, path string) {
	t.Helper()
	if _, err := s.db.ExecContext(ctx, `DELETE FROM track_variants WHERE source_path = ?`, path); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM track_analysis WHERE source_path = ?`, path); err != nil {
		t.Fatal(err)
	}
}

func markTrackMissing(t *testing.T, ctx context.Context, s *Store, path string) {
	t.Helper()
	if _, err := s.db.ExecContext(ctx, `UPDATE tracks SET missing_count = 1 WHERE path = ?`, path); err != nil {
		t.Fatal(err)
	}
}

// TestClearMissingSidecarQueriesShareTheDeletePredicate: the rendition
// list, the waveform list and the DELETE are derived from one predicate,
// so a routed row the DELETE spares is a row whose files stay too.
func TestClearMissingSidecarQueriesShareTheDeletePredicate(t *testing.T) {
	want := squashSpace(clearMissingTracksWhereSQL)
	for _, tc := range []struct{ name, stmt string }{
		{"clearMissingTracksDeleteSQL", clearMissingTracksDeleteSQL},
		{"clearMissingVariantSidecarsSQL", clearMissingVariantSidecarsSQL},
		{"clearMissingWaveformWhereSQL", clearMissingWaveformWhereSQL},
		{"journalClearMissingSQL", journalClearMissingSQL},
	} {
		if !strings.Contains(squashSpace(tc.stmt), want) {
			t.Errorf("%s no longer embeds the clear-missing predicate.\n%s", tc.name, tc.stmt)
		}
	}
	if !strings.Contains(want, "NOT EXISTS") || !strings.Contains(want, "upnp_track_routing") {
		t.Errorf("clear-missing predicate lost its routed anti-join:\n%s", want)
	}
}
