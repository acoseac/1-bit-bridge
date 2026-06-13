package manifest

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func readIndexedAt(t *testing.T, s *Store, path string) int64 {
	t.Helper()
	var v int64
	if err := s.db.QueryRow(`SELECT indexed_at FROM tracks WHERE path = ?`, path).Scan(&v); err != nil {
		t.Fatalf("read indexed_at(%q): %v", path, err)
	}
	return v
}

// TestUpsertAnalysisBumpsParentIndexedAt: a fresh analysis write must
// advance the parent track's indexed_at so iOS delta-sync surfaces the
// new waveformTag. Mirrors the variant bump contract.
func TestUpsertAnalysisBumpsParentIndexedAt(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })
	upsertParent(t, s, "A/1.flac")

	t1 := readIndexedAt(t, s, "A/1.flac") + (1 * time.Hour).Nanoseconds()
	s.now = func() time.Time { return time.Unix(0, t1) }

	if err := s.UpsertAnalysis(context.Background(), AnalysisRow{
		SourcePath: "A/1.flac", WaveformPath: "/w/a.bin", WaveformTag: "tag1",
		WaveformSize: 10, SourceMTimeNS: 1, SourceSize: 2,
		SchemaVersion: "wf1", CreatedAt: 100,
	}); err != nil {
		t.Fatalf("UpsertAnalysis: %v", err)
	}
	if got := readIndexedAt(t, s, "A/1.flac"); got != t1 {
		t.Fatalf("indexed_at = %d, want %d", got, t1)
	}
}

// TestUpsertAnalysisNoOpOnIdenticalRecompute: re-running analysis that
// produces byte-identical results must NOT bump indexed_at (no manifest
// churn), but a changed waveform tag MUST. CreatedAt is not part of the
// equality so a fresh timestamp alone is still a no-op.
func TestUpsertAnalysisNoOpOnIdenticalRecompute(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })
	upsertParent(t, s, "A/1.flac")
	ctx := context.Background()

	base := AnalysisRow{
		SourcePath: "A/1.flac", WaveformPath: "/w/a.bin", WaveformTag: "tag1",
		WaveformSize: 10, SourceMTimeNS: 1, SourceSize: 2,
		SchemaVersion: "wf1", CreatedAt: 100,
	}
	t1 := readIndexedAt(t, s, "A/1.flac") + (1 * time.Hour).Nanoseconds()
	s.now = func() time.Time { return time.Unix(0, t1) }
	if err := s.UpsertAnalysis(ctx, base); err != nil {
		t.Fatalf("first UpsertAnalysis: %v", err)
	}
	after1 := readIndexedAt(t, s, "A/1.flac")
	if after1 != t1 {
		t.Fatalf("first bump: got %d want %d", after1, t1)
	}

	// Identical recompute (only CreatedAt differs) → no bump.
	s.now = func() time.Time { return time.Unix(0, t1+(1*time.Hour).Nanoseconds()) }
	identical := base
	identical.CreatedAt = 999
	if err := s.UpsertAnalysis(ctx, identical); err != nil {
		t.Fatalf("identical UpsertAnalysis: %v", err)
	}
	if got := readIndexedAt(t, s, "A/1.flac"); got != after1 {
		t.Fatalf("identical recompute bumped indexed_at: got %d want %d", got, after1)
	}

	// Changed waveform tag → must bump.
	t3 := after1 + (2 * time.Hour).Nanoseconds()
	s.now = func() time.Time { return time.Unix(0, t3) }
	changed := base
	changed.WaveformTag = "tag2"
	if err := s.UpsertAnalysis(ctx, changed); err != nil {
		t.Fatalf("changed UpsertAnalysis: %v", err)
	}
	if got := readIndexedAt(t, s, "A/1.flac"); got != t3 {
		t.Fatalf("changed-tag bump: got %d want %d", got, t3)
	}
}

// TestManifestSplicesWaveformTag: ListTracks must splice the
// track_analysis.waveform_tag onto Track.WaveformTag, and leave it
// empty for tracks with no analysis row.
func TestManifestSplicesWaveformTag(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	upsertParent(t, s, "A/1.flac")
	upsertParent(t, s, "A/2.flac") // no analysis row

	if err := s.UpsertAnalysis(ctx, AnalysisRow{
		SourcePath: "A/1.flac", WaveformPath: "/w/a.bin", WaveformTag: "abcd1234",
		WaveformSize: 10, SourceMTimeNS: 1, SourceSize: 2,
		SchemaVersion: "wf1", CreatedAt: 1,
	}); err != nil {
		t.Fatalf("UpsertAnalysis: %v", err)
	}

	tracks, err := s.ListTracks(ctx, nil)
	if err != nil {
		t.Fatalf("ListTracks: %v", err)
	}
	got := map[string]string{}
	for _, tr := range tracks {
		got[tr.Path] = tr.WaveformTag
	}
	if got["A/1.flac"] != "abcd1234" {
		t.Fatalf("A/1 waveformTag = %q, want abcd1234", got["A/1.flac"])
	}
	if got["A/2.flac"] != "" {
		t.Fatalf("A/2 waveformTag = %q, want empty", got["A/2.flac"])
	}
}

// TestDeleteTrackRemovesWaveformSidecar: deleting a track unlinks its
// waveform sidecar file and cascades the analysis row.
func TestDeleteTrackRemovesWaveformSidecar(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	upsertParent(t, s, "A/1.flac")

	wf := filepath.Join(t.TempDir(), "a.waveform.bin")
	if err := os.WriteFile(wf, []byte("data"), 0o600); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}
	if err := s.UpsertAnalysis(ctx, AnalysisRow{
		SourcePath: "A/1.flac", WaveformPath: wf, WaveformTag: "t",
		WaveformSize: 4, SourceMTimeNS: 1, SourceSize: 2,
		SchemaVersion: "wf1", CreatedAt: 1,
	}); err != nil {
		t.Fatalf("UpsertAnalysis: %v", err)
	}
	if err := s.DeleteTrack(ctx, "A/1.flac"); err != nil {
		t.Fatalf("DeleteTrack: %v", err)
	}
	if _, err := os.Stat(wf); !os.IsNotExist(err) {
		t.Fatalf("waveform sidecar not removed: stat err = %v", err)
	}
	if row, _ := s.GetAnalysis(ctx, "A/1.flac"); row != nil {
		t.Fatalf("analysis row not cascaded: %+v", *row)
	}
}

// TestLookupAnalysisCaseInsensitive: the /v1/waveform handler hands in
// an iOS-shaped path (leading slash + lowercased); LookupAnalysis must
// resolve it against the case-preserved manifest source_path.
func TestLookupAnalysisCaseInsensitive(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	upsertParent(t, s, "Artist/Album/01.flac")
	if err := s.UpsertAnalysis(ctx, AnalysisRow{
		SourcePath: "Artist/Album/01.flac", WaveformPath: "/w/x.bin", WaveformTag: "t",
		SourceMTimeNS: 1, SourceSize: 1, SchemaVersion: "wf1", CreatedAt: 1,
	}); err != nil {
		t.Fatalf("UpsertAnalysis: %v", err)
	}
	row, err := s.LookupAnalysis(ctx, "/artist/album/01.flac")
	if err != nil {
		t.Fatalf("LookupAnalysis: %v", err)
	}
	if row == nil || row.SourcePath != "Artist/Album/01.flac" {
		t.Fatalf("lookup mismatch: %+v", row)
	}
}
