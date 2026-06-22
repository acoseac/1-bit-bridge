package manifest

import (
	"context"
	"testing"
)

// TestKeyDistribution verifies the GROUP BY counts analyzed tracks per
// (key_root, key_mode), excludes un-keyed rows, and that the surviving
// pairs are the ones the admin Camelot wheel maps (via ToCamelot) to
// codes. Seeds parent tracks (track_analysis has a FK to tracks).
func TestKeyDistribution(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })

	seed := func(path string, root *int, mode string) {
		t.Helper()
		upsertParent(t, s, path)
		if err := s.UpsertAnalysis(context.Background(), AnalysisRow{
			SourcePath: path, WaveformPath: "/w/" + path, WaveformTag: "t-" + path,
			WaveformSize: 10, SourceMTimeNS: 1, SourceSize: 2,
			SchemaVersion: "wf1", CreatedAt: 100,
			KeyRoot: root, KeyMode: mode,
		}); err != nil {
			t.Fatalf("UpsertAnalysis %q: %v", path, err)
		}
	}

	seed("A/1.flac", intptr(0), "major") // C major  → 8B
	seed("A/2.flac", intptr(0), "major") // C major  → 8B (count 2)
	seed("A/3.flac", intptr(9), "minor") // A minor  → 8A
	seed("A/4.flac", nil, "")            // un-keyed  → excluded

	kd, err := s.KeyDistribution(context.Background())
	if err != nil {
		t.Fatalf("KeyDistribution: %v", err)
	}

	type key struct {
		root int
		mode string
	}
	got := map[key]int{}
	for _, k := range kd {
		got[key{k.KeyRoot, k.KeyMode}] += k.Count
	}
	if len(got) != 2 {
		t.Errorf("got %d groups, want 2 (the un-keyed row must be excluded): %+v", len(got), kd)
	}
	if n := got[key{0, "major"}]; n != 2 {
		t.Errorf("(C, major) = %d, want 2", n)
	}
	if n := got[key{9, "minor"}]; n != 1 {
		t.Errorf("(A, minor) = %d, want 1", n)
	}
}

// TestKeyDistributionEmpty asserts no analyzed keys yields no groups (not
// an error) — the admin wheel then renders its "no analysed keys" state.
func TestKeyDistributionEmpty(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })

	kd, err := s.KeyDistribution(context.Background())
	if err != nil {
		t.Fatalf("KeyDistribution: %v", err)
	}
	if len(kd) != 0 {
		t.Errorf("got %d groups on empty analysis, want 0", len(kd))
	}
}
