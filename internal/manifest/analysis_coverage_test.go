package manifest

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// TestAnalysisCoverage pins the coverage query's bucket semantics
// against a seeded store: DSD (by extension, the sweeper's own rule)
// and zero-byte rows are excluded from eligible in that precedence
// order; UPnP-routed rows are invisible; analysis rows split
// fresh-vs-stale by schema version; and an analysis row attached to an
// excluded (DSD/zero-byte) track never counts as analysed — so
// analysed <= eligible holds by construction.
func TestAnalysisCoverage(t *testing.T) {
	s, err := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	const schema = "v2"

	seed := []struct {
		path string
		size int64
	}{
		{"a/fresh.flac", 100},   // eligible, analysed fresh
		{"a/stale.flac", 100},   // eligible, analysed stale
		{"a/pending.flac", 100}, // eligible, no analysis row
		{"a/disc.dsf", 100},     // DSD-excluded
		{"a/disc.dff", 100},     // DSD-excluded
		{"a/empty.flac", 0},     // zero-byte-excluded
		{"a/emptydsd.dsf", 0},   // DSD wins over zero-byte (precedence)
		{"upnp/routed.flac", 100},
	}
	for _, tr := range seed {
		if err := s.UpsertTrack(ctx, &Track{Path: tr.path, Size: tr.size, ModTime: time.Now()}); err != nil {
			t.Fatalf("UpsertTrack %q: %v", tr.path, err)
		}
	}
	if err := s.UpsertUPnPRouting(ctx, &UPnPRouting{
		SourcePath: "upnp/routed.flac", ServerUDN: "uuid:x", ObjectID: "1",
		ResURL: "http://up/1.flac", LastSeenAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	rows := []AnalysisRow{
		{SourcePath: "a/fresh.flac", WaveformPath: "/w/f", WaveformTag: "aa", WaveformSize: 10, SchemaVersion: schema},
		{SourcePath: "a/stale.flac", WaveformPath: "/w/s", WaveformTag: "bb", WaveformSize: 10, SchemaVersion: "v1"},
		// Excluded tracks with analysis rows: must count in neither
		// analysed bucket (a DSD row could only predate the DSD gate;
		// a zero-byte row predates the truncation).
		{SourcePath: "a/disc.dsf", WaveformPath: "/w/d", WaveformTag: "cc", WaveformSize: 10, SchemaVersion: schema},
		{SourcePath: "a/empty.flac", WaveformPath: "/w/e", WaveformTag: "dd", WaveformSize: 10, SchemaVersion: schema},
	}
	for _, r := range rows {
		r.SourceMTimeNS, r.SourceSize, r.CreatedAt = 1, 100, 1
		if err := s.UpsertAnalysis(ctx, r); err != nil {
			t.Fatalf("UpsertAnalysis %q: %v", r.SourcePath, err)
		}
	}

	cov, err := s.AnalysisCoverage(ctx, schema)
	if err != nil {
		t.Fatal(err)
	}
	want := AnalysisCoverage{
		TotalLocal:       7, // routed row invisible
		DSDExcluded:      3, // .dsf + .dff + zero-byte .dsf (DSD precedence)
		ZeroByteExcluded: 1, // empty.flac only
		AnalysedFresh:    1,
		AnalysedStale:    1,
	}
	if cov != want {
		t.Errorf("AnalysisCoverage = %+v, want %+v", cov, want)
	}
	if eligible := cov.TotalLocal - cov.DSDExcluded - cov.ZeroByteExcluded; eligible != 3 {
		t.Errorf("eligible = %d, want 3", eligible)
	}
}

// TestAnalysisCoverageEmptyLibrary — a fresh store yields all-zero
// coverage, not an error (the Jobs card renders a quiet empty state).
func TestAnalysisCoverageEmptyLibrary(t *testing.T) {
	s, err := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	cov, err := s.AnalysisCoverage(context.Background(), "v2")
	if err != nil {
		t.Fatal(err)
	}
	if cov != (AnalysisCoverage{}) {
		t.Errorf("empty-library coverage = %+v, want zero", cov)
	}
}
