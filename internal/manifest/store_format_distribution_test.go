package manifest

import (
	"context"
	"encoding/json"
	"testing"
)

// insertFormatTrack writes one track row whose tags_json carries the
// given format fields, so FormatDistribution (which reads them via
// json_extract) has shaped rows to group. isDSD is stored as a JSON
// boolean to exercise the CAST(... AS INTEGER) round-trip on the real
// wire shape the scanner writes.
func insertFormatTrack(t *testing.T, s *Store, path, codec string, rate, bits int, isDSD bool) {
	t.Helper()
	tags, err := json.Marshal(map[string]any{
		"codec":         codec,
		"sampleRate":    rate,
		"bitsPerSample": bits,
		"isDSD":         isDSD,
	})
	if err != nil {
		t.Fatalf("marshal tags: %v", err)
	}
	if _, err := s.db.Exec(`
		INSERT INTO tracks(path, size, mtime_ns, tags_json, indexed_at)
		VALUES (?,?,?,?,?)
	`, path, int64(100), int64(1), tags, int64(1)); err != nil {
		t.Fatalf("insert track %q: %v", path, err)
	}
}

type fmtKey struct {
	codec string
	rate  int
	bits  int
	dsd   bool
}

// TestFormatDistribution verifies the GROUP BY aggregates tracks by
// (codec, sampleRate, bitsPerSample, isDSD); that the per-group counts
// sum to the full table (the dashboard's bars-reconcile-to-total
// contract); that the JSON-boolean isDSD round-trips through the CAST to
// the Go bool; and that a track with no extractable format collapses to
// the empty/zero "Unknown" group rather than being dropped.
func TestFormatDistribution(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })

	insertFormatTrack(t, s, "A/01.flac", "FLAC", 44100, 16, false)
	insertFormatTrack(t, s, "A/02.flac", "FLAC", 44100, 16, false)
	insertFormatTrack(t, s, "B/01.flac", "FLAC", 96000, 24, false)
	insertFormatTrack(t, s, "B/02.flac", "FLAC", 192000, 24, false)
	insertFormatTrack(t, s, "C/01.m4a", "ALAC", 44100, 16, false)
	insertFormatTrack(t, s, "D/01.dsf", "DSF", 2822400, 1, true)
	// No extractable format — empty tags map → codec "", rate/bits 0.
	if _, err := s.db.Exec(`
		INSERT INTO tracks(path, size, mtime_ns, tags_json, indexed_at)
		VALUES (?,?,?,?,?)
	`, "E/01.bin", int64(100), int64(1), []byte(`{}`), int64(1)); err != nil {
		t.Fatalf("insert empty-format track: %v", err)
	}

	groups, err := s.FormatDistribution(context.Background())
	if err != nil {
		t.Fatalf("FormatDistribution: %v", err)
	}

	byKey := map[fmtKey]int{}
	total := 0
	for _, g := range groups {
		byKey[fmtKey{g.Codec, g.SampleRate, g.BitsPerSample, g.IsDSD}] += g.Count
		total += g.Count
	}

	if total != 7 {
		t.Errorf("counts sum to %d, want 7 (must reconcile to the full table)", total)
	}
	if n := byKey[fmtKey{"FLAC", 44100, 16, false}]; n != 2 {
		t.Errorf("FLAC/44100/16 = %d, want 2", n)
	}
	if n := byKey[fmtKey{"FLAC", 96000, 24, false}]; n != 1 {
		t.Errorf("FLAC/96000/24 = %d, want 1", n)
	}
	if n := byKey[fmtKey{"FLAC", 192000, 24, false}]; n != 1 {
		t.Errorf("FLAC/192000/24 = %d, want 1", n)
	}
	if n := byKey[fmtKey{"ALAC", 44100, 16, false}]; n != 1 {
		t.Errorf("ALAC/44100/16 = %d, want 1", n)
	}
	// The DSD row MUST surface with IsDSD true — the CAST(json bool)
	// round-trip is the thing under test (a regression here would
	// misclassify every DSD track as PCM, the production bug class the
	// shared ListTrackProjectionsUnderPrefix pattern guards against).
	if n := byKey[fmtKey{"DSF", 2822400, 1, true}]; n != 1 {
		t.Errorf("DSF/2822400/1/dsd = %d, want 1 (isDSD must round-trip true)", n)
	}
	if n := byKey[fmtKey{"", 0, 0, false}]; n != 1 {
		t.Errorf("empty-format Unknown group = %d, want 1", n)
	}
}

// TestFormatDistributionEmptyTable asserts an empty library returns no
// groups (not an error) — the admin snapshot then renders a hidden
// "Master quality" block rather than empty bars.
func TestFormatDistributionEmptyTable(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })

	groups, err := s.FormatDistribution(context.Background())
	if err != nil {
		t.Fatalf("FormatDistribution: %v", err)
	}
	if len(groups) != 0 {
		t.Errorf("got %d groups on empty table, want 0", len(groups))
	}
}
