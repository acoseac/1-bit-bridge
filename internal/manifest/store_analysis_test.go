package manifest

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"math"
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

// TestLookupAnalysisCaseFoldCarriesSpectrum: the unicode_lower fallback
// SELECT must mirror the exact-path column list. Pre-fix it stopped at
// audio_md5_attempts — a row that resolved ONLY via the case fold (the
// iOS-shaped lowercase path PROTOCOL.md promises to resolve) came back
// with Spectrum == nil / BandwidthHz == nil, so /v1/spectrum answered
// 404 spectrum_not_found while /v1/waveform worked for the same track.
func TestLookupAnalysisCaseFoldCarriesSpectrum(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	upsertParent(t, s, "Artist/Album/01.flac")
	blob := []byte("1BSP-test-spectrum-blob")
	if err := s.UpsertAnalysis(ctx, AnalysisRow{
		SourcePath: "Artist/Album/01.flac", WaveformPath: "/w/x.bin", WaveformTag: "t",
		SourceMTimeNS: 1, SourceSize: 1, SchemaVersion: "wf7", CreatedAt: 1,
		BandwidthHz: intptr(21500), Spectrum: blob,
	}); err != nil {
		t.Fatalf("UpsertAnalysis: %v", err)
	}

	// Control: the exact-path read carries both fields.
	exact, err := s.GetAnalysis(ctx, "Artist/Album/01.flac")
	if err != nil || exact == nil {
		t.Fatalf("GetAnalysis: row=%v err=%v", exact, err)
	}
	if !bytes.Equal(exact.Spectrum, blob) || exact.BandwidthHz == nil {
		t.Fatalf("exact-path read lost spectrum fields: %+v", exact)
	}

	// The case-differing iOS shape resolves only via the folded fallback
	// — it must carry the same spectrum fields as the exact-path read.
	row, err := s.LookupAnalysis(ctx, "/artist/album/01.flac")
	if err != nil {
		t.Fatalf("LookupAnalysis: %v", err)
	}
	if row == nil || row.SourcePath != "Artist/Album/01.flac" {
		t.Fatalf("lookup mismatch: %+v", row)
	}
	if !bytes.Equal(row.Spectrum, blob) {
		t.Fatalf("case-folded lookup Spectrum = %q, want the seeded blob", row.Spectrum)
	}
	if row.BandwidthHz == nil || *row.BandwidthHz != 21500 {
		t.Fatalf("case-folded lookup BandwidthHz = %v, want 21500", row.BandwidthHz)
	}
}

// TestAllAnalysisRowsCarriesSpectrum: AllAnalysisRows is aligned with
// the other two read sites (same column list). Its only caller today
// (`bridge analyze --gc`) reads just waveform_path, but a silently
// narrower SELECT here is how the LookupAnalysis fallback bug happened.
func TestAllAnalysisRowsCarriesSpectrum(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	upsertParent(t, s, "A/1.flac")
	blob := []byte("1BSP-test-spectrum-blob")
	if err := s.UpsertAnalysis(ctx, AnalysisRow{
		SourcePath: "A/1.flac", WaveformPath: "/w/a.bin", WaveformTag: "t",
		SourceMTimeNS: 1, SourceSize: 1, SchemaVersion: "wf7", CreatedAt: 1,
		BandwidthHz: intptr(21500), Spectrum: blob,
	}); err != nil {
		t.Fatalf("UpsertAnalysis: %v", err)
	}
	rows, err := s.AllAnalysisRows(ctx)
	if err != nil {
		t.Fatalf("AllAnalysisRows: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("AllAnalysisRows returned %d rows, want 1", len(rows))
	}
	if !bytes.Equal(rows[0].Spectrum, blob) || rows[0].BandwidthHz == nil || *rows[0].BandwidthHz != 21500 {
		t.Fatalf("AllAnalysisRows lost spectrum fields: %+v", rows[0])
	}
}

func f64ptr(v float64) *float64 { return &v }
func intptr(v int) *int         { return &v }

// TestUpsertAnalysisPersistsKeyTempo: the nullable key_root/key_mode/bpm
// columns round-trip — present values read back, absent read back nil/"".
func TestUpsertAnalysisPersistsKeyTempo(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	upsertParent(t, s, "A/1.flac")
	upsertParent(t, s, "A/2.flac")

	if err := s.UpsertAnalysis(ctx, AnalysisRow{
		SourcePath: "A/1.flac", WaveformPath: "/w/a", WaveformTag: "t1",
		SourceMTimeNS: 1, SourceSize: 2, SchemaVersion: "wf3", CreatedAt: 1,
		KeyRoot: intptr(7), KeyMode: "major", BPM: intptr(128),
	}); err != nil {
		t.Fatalf("UpsertAnalysis (with key/tempo): %v", err)
	}
	if err := s.UpsertAnalysis(ctx, AnalysisRow{
		SourcePath: "A/2.flac", WaveformPath: "/w/b", WaveformTag: "t2",
		SourceMTimeNS: 1, SourceSize: 2, SchemaVersion: "wf3", CreatedAt: 1,
	}); err != nil {
		t.Fatalf("UpsertAnalysis (nil key/tempo): %v", err)
	}

	got, err := s.GetAnalysis(ctx, "A/1.flac")
	if err != nil {
		t.Fatalf("GetAnalysis A/1: %v", err)
	}
	if got.KeyRoot == nil || *got.KeyRoot != 7 || got.KeyMode != "major" || got.BPM == nil || *got.BPM != 128 {
		t.Fatalf("A/1 key/tempo = (%v,%q,%v), want (7, major, 128)", got.KeyRoot, got.KeyMode, got.BPM)
	}
	got2, err := s.GetAnalysis(ctx, "A/2.flac")
	if err != nil {
		t.Fatalf("GetAnalysis A/2: %v", err)
	}
	if got2.KeyRoot != nil || got2.KeyMode != "" || got2.BPM != nil {
		t.Fatalf("A/2 key/tempo = (%v,%q,%v), want all empty", got2.KeyRoot, got2.KeyMode, got2.BPM)
	}
}

// TestManifestSplicesKeyTempo: ListTracks splices KeyRoot/KeyMode always
// (no tag source) and BPM tag-absent-only (a curated BPM tag wins).
func TestManifestSplicesKeyTempo(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	upsertParent(t, s, "A/1.flac")       // no tags
	if err := s.UpsertTrack(ctx, &Track{ // curated BPM tag of 90
		Path: "A/2.flac", Size: 100, ModTime: time.Now(), BPM: intptr(90),
	}); err != nil {
		t.Fatalf("UpsertTrack A/2: %v", err)
	}
	upsertParent(t, s, "A/3.flac") // no analysis row

	for _, p := range []string{"A/1.flac", "A/2.flac"} {
		if err := s.UpsertAnalysis(ctx, AnalysisRow{
			SourcePath: p, WaveformPath: "/w/" + p, WaveformTag: "tag-" + p,
			SourceMTimeNS: 1, SourceSize: 2, SchemaVersion: "wf3", CreatedAt: 1,
			KeyRoot: intptr(2), KeyMode: "minor", BPM: intptr(140),
		}); err != nil {
			t.Fatalf("UpsertAnalysis %s: %v", p, err)
		}
	}

	tracks, err := s.ListTracks(ctx, nil)
	if err != nil {
		t.Fatalf("ListTracks: %v", err)
	}
	by := map[string]Track{}
	for _, tr := range tracks {
		by[tr.Path] = tr
	}
	// A/1: key + tempo both from analysis — and the tempo is POSITIVELY
	// marked estimated on the wire.
	if a1 := by["A/1.flac"]; a1.KeyRoot == nil || *a1.KeyRoot != 2 || a1.KeyMode != "minor" || a1.BPM == nil || *a1.BPM != 140 {
		t.Fatalf("A/1 = (%v,%q,%v), want analysis (2, minor, 140)", a1.KeyRoot, a1.KeyMode, a1.BPM)
	} else if !a1.BPMEstimated {
		t.Fatalf("A/1 spliced an estimated BPM without marking it — the client would have no way to label it")
	}
	// A/2: curated BPM tag wins; key still from analysis (no key tag) —
	// and the curated value MUST NOT carry the estimate marker: labelling
	// a user's own tag "estimated" is the mislabel the field exists to
	// prevent.
	if a2 := by["A/2.flac"]; a2.BPM == nil || *a2.BPM != 90 || a2.KeyRoot == nil || *a2.KeyRoot != 2 {
		t.Fatalf("A/2 = (key=%v, bpm=%v), want curated bpm 90 + analysis key 2", a2.KeyRoot, a2.BPM)
	} else if a2.BPMEstimated {
		t.Fatalf("A/2's curated BPM tag is marked estimated — a curated tag must never carry the marker")
	}
	// A/3: nothing.
	if a3 := by["A/3.flac"]; a3.KeyRoot != nil || a3.KeyMode != "" || a3.BPM != nil || a3.BPMEstimated {
		t.Fatalf("A/3 = (%v,%q,%v,%v), want all empty", a3.KeyRoot, a3.KeyMode, a3.BPM, a3.BPMEstimated)
	}

	// The wire shape itself: `bpmEstimated` is an omitempty additive, so
	// the estimated row carries the key and the curated + absent rows OMIT
	// it entirely (absence makes no claim — pre-feature clients and
	// curated tags both read identically). Key-EXISTENCE via a map, not a
	// substring match: a substring probe for `"bpmEstimated":true` would
	// falsely pass if the field ever serialized as `"bpmEstimated":false`
	// (Gemini on PR #689) — the claim under test is that the KEY is absent.
	for path, wantKey := range map[string]bool{
		"A/1.flac": true, "A/2.flac": false, "A/3.flac": false,
	} {
		blob, err := json.Marshal(by[path])
		if err != nil {
			t.Fatalf("marshal %s: %v", path, err)
		}
		var m map[string]any
		if err := json.Unmarshal(blob, &m); err != nil {
			t.Fatalf("unmarshal %s: %v", path, err)
		}
		if _, got := m["bpmEstimated"]; got != wantKey {
			t.Fatalf("%s wire form bpmEstimated key presence = %v, want %v (blob %s)",
				path, got, wantKey, blob)
		}
		if wantKey && m["bpmEstimated"] != true {
			t.Fatalf("%s bpmEstimated = %v, want true", path, m["bpmEstimated"])
		}
	}
}

// TestSplicedKeyTempoNotPersistedOnRoundTrip: KeyRoot/KeyMode are scrubbed
// unconditionally on write-back (analysis-only); BPM is scrubbed only when
// analysis-derived, so a curated BPM tag survives a round-trip.
func TestSplicedKeyTempoNotPersistedOnRoundTrip(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	upsertParent(t, s, "A/1.flac")       // no tags → all analysis-derived
	if err := s.UpsertTrack(ctx, &Track{ // curated BPM 90
		Path: "A/2.flac", Size: 100, ModTime: time.Now(), BPM: intptr(90),
	}); err != nil {
		t.Fatalf("UpsertTrack A/2: %v", err)
	}
	for _, p := range []string{"A/1.flac", "A/2.flac"} {
		if err := s.UpsertAnalysis(ctx, AnalysisRow{
			SourcePath: p, WaveformPath: "/w/" + p, WaveformTag: "tag-" + p,
			SourceMTimeNS: 1, SourceSize: 2, SchemaVersion: "wf3", CreatedAt: 1,
			KeyRoot: intptr(5), KeyMode: "major", BPM: intptr(140),
		}); err != nil {
			t.Fatalf("UpsertAnalysis %s: %v", p, err)
		}
	}

	tracks, err := s.ListTracks(ctx, nil)
	if err != nil {
		t.Fatalf("ListTracks: %v", err)
	}
	for i := range tracks {
		tr := tracks[i]
		if err := s.UpsertTrack(ctx, &tr); err != nil { // round-trip
			t.Fatalf("round-trip UpsertTrack %s: %v", tr.Path, err)
		}
	}

	// GetTrack reads tags_json only: key always gone; A/1 analysis-BPM gone,
	// A/2 curated BPM preserved. The estimate marker must be gone from BOTH
	// — frozen into tags_json it would label a later curated tag as an
	// estimate.
	a1, _ := s.GetTrack(ctx, "A/1.flac")
	if a1.KeyRoot != nil || a1.KeyMode != "" || a1.BPM != nil || a1.BPMEstimated {
		t.Fatalf("A/1 leaked analysis values: key=(%v,%q) bpm=%v est=%v, want all nil",
			a1.KeyRoot, a1.KeyMode, a1.BPM, a1.BPMEstimated)
	}
	a2, _ := s.GetTrack(ctx, "A/2.flac")
	if a2.KeyRoot != nil || a2.KeyMode != "" || a2.BPMEstimated {
		t.Fatalf("A/2 leaked analysis key/marker: (%v,%q,%v), want nil", a2.KeyRoot, a2.KeyMode, a2.BPMEstimated)
	}
	if a2.BPM == nil || *a2.BPM != 90 {
		t.Fatalf("A/2 curated BPM = %v, want 90 (preserved)", a2.BPM)
	}

	// Live column still re-splices key for A/1.
	tracks2, _ := s.ListTracks(ctx, nil)
	for _, tr := range tracks2 {
		if tr.Path == "A/1.flac" && (tr.KeyRoot == nil || *tr.KeyRoot != 5) {
			t.Fatalf("A/1 key re-splice = %v, want 5", tr.KeyRoot)
		}
	}
}

// TestUpsertAnalysisPersistsLoudness: the nullable replaygain_track_db
// column round-trips — a present value reads back, nil reads back nil.
func TestUpsertAnalysisPersistsLoudness(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	upsertParent(t, s, "A/1.flac")
	upsertParent(t, s, "A/2.flac")

	if err := s.UpsertAnalysis(ctx, AnalysisRow{
		SourcePath: "A/1.flac", WaveformPath: "/w/a.bin", WaveformTag: "t1",
		SourceMTimeNS: 1, SourceSize: 2, SchemaVersion: "wf2", CreatedAt: 1,
		ReplayGainTrackDB: f64ptr(-7.25),
	}); err != nil {
		t.Fatalf("UpsertAnalysis (with loudness): %v", err)
	}
	if err := s.UpsertAnalysis(ctx, AnalysisRow{
		SourcePath: "A/2.flac", WaveformPath: "/w/b.bin", WaveformTag: "t2",
		SourceMTimeNS: 1, SourceSize: 2, SchemaVersion: "wf2", CreatedAt: 1,
		ReplayGainTrackDB: nil,
	}); err != nil {
		t.Fatalf("UpsertAnalysis (nil loudness): %v", err)
	}

	got1, err := s.GetAnalysis(ctx, "A/1.flac")
	if err != nil {
		t.Fatalf("GetAnalysis A/1: %v", err)
	}
	if got1.ReplayGainTrackDB == nil || *got1.ReplayGainTrackDB != -7.25 {
		t.Fatalf("A/1 loudness = %v, want -7.25", got1.ReplayGainTrackDB)
	}
	got2, err := s.GetAnalysis(ctx, "A/2.flac")
	if err != nil {
		t.Fatalf("GetAnalysis A/2: %v", err)
	}
	if got2.ReplayGainTrackDB != nil {
		t.Fatalf("A/2 loudness = %v, want nil", *got2.ReplayGainTrackDB)
	}
}

// TestUpsertAnalysisLoudnessBackfillBumps: a loudness backfill (nil →
// value) on an otherwise-identical waveform-fresh row MUST bump
// indexed_at exactly once (so iOS delta-sync picks up the new
// replayGainTrackDB), and a re-run with the SAME loudness is a no-op.
// This is the v14→v16 backfill path: a wf1-era row gains its scalar.
func TestUpsertAnalysisLoudnessBackfillBumps(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	upsertParent(t, s, "A/1.flac")

	base := AnalysisRow{
		SourcePath: "A/1.flac", WaveformPath: "/w/a.bin", WaveformTag: "t1",
		WaveformSize: 10, SourceMTimeNS: 1, SourceSize: 2,
		SchemaVersion: "wf2", CreatedAt: 1,
	}
	t1 := readIndexedAt(t, s, "A/1.flac") + (1 * time.Hour).Nanoseconds()
	s.now = func() time.Time { return time.Unix(0, t1) }
	if err := s.UpsertAnalysis(ctx, base); err != nil { // loudness nil
		t.Fatalf("first UpsertAnalysis: %v", err)
	}

	// Backfill loudness (nil → -8.0) → must bump.
	t2 := t1 + (1 * time.Hour).Nanoseconds()
	s.now = func() time.Time { return time.Unix(0, t2) }
	withLoud := base
	withLoud.ReplayGainTrackDB = f64ptr(-8.0)
	if err := s.UpsertAnalysis(ctx, withLoud); err != nil {
		t.Fatalf("backfill UpsertAnalysis: %v", err)
	}
	if got := readIndexedAt(t, s, "A/1.flac"); got != t2 {
		t.Fatalf("loudness backfill did not bump: got %d want %d", got, t2)
	}

	// Same loudness again → no-op.
	s.now = func() time.Time { return time.Unix(0, t2+(1*time.Hour).Nanoseconds()) }
	again := base
	again.ReplayGainTrackDB = f64ptr(-8.0)
	again.CreatedAt = 999
	if err := s.UpsertAnalysis(ctx, again); err != nil {
		t.Fatalf("idempotent UpsertAnalysis: %v", err)
	}
	if got := readIndexedAt(t, s, "A/1.flac"); got != t2 {
		t.Fatalf("identical loudness recompute bumped: got %d want %d", got, t2)
	}
}

// TestManifestSplicesReplayGainTagAbsentOnly is the wire contract: the
// analysis loudness fills Track.ReplayGainTrackDB ONLY when the source's
// own tags carry none — a curated tag always wins. Three tracks cover
// the matrix: tag-absent + analysis present (→ analysis), tag present +
// analysis present (→ tag), neither (→ nil).
func TestManifestSplicesReplayGainTagAbsentOnly(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	// A/1: no tag — analysis should fill it.
	upsertParent(t, s, "A/1.flac")
	// A/2: curated ReplayGain tag of -5.0 baked into tags_json.
	if err := s.UpsertTrack(ctx, &Track{
		Path: "A/2.flac", Size: 100, ModTime: time.Now(),
		ReplayGainTrackDB: f64ptr(-5.0),
	}); err != nil {
		t.Fatalf("UpsertTrack A/2: %v", err)
	}
	// A/3: no tag, no analysis row.
	upsertParent(t, s, "A/3.flac")

	for _, p := range []string{"A/1.flac", "A/2.flac"} {
		if err := s.UpsertAnalysis(ctx, AnalysisRow{
			SourcePath: p, WaveformPath: "/w/" + p, WaveformTag: "tag-" + p,
			SourceMTimeNS: 1, SourceSize: 2, SchemaVersion: "wf2", CreatedAt: 1,
			ReplayGainTrackDB: f64ptr(-9.0),
		}); err != nil {
			t.Fatalf("UpsertAnalysis %s: %v", p, err)
		}
	}

	tracks, err := s.ListTracks(ctx, nil)
	if err != nil {
		t.Fatalf("ListTracks: %v", err)
	}
	got := map[string]*float64{}
	for i := range tracks {
		got[tracks[i].Path] = tracks[i].ReplayGainTrackDB
	}
	if got["A/1.flac"] == nil || *got["A/1.flac"] != -9.0 {
		t.Fatalf("A/1 (tag-absent) = %v, want analysis -9.0", got["A/1.flac"])
	}
	if got["A/2.flac"] == nil || *got["A/2.flac"] != -5.0 {
		t.Fatalf("A/2 (tag-present) = %v, want curated tag -5.0", got["A/2.flac"])
	}
	if got["A/3.flac"] != nil {
		t.Fatalf("A/3 (neither) = %v, want nil", *got["A/3.flac"])
	}
}

// TestSplicedReplayGainNotPersistedOnRoundTrip pins the marshalForStorage
// scrub: a Track read with an ANALYSIS-derived replayGainTrackDB, then fed
// back through a write path, must NOT freeze that value into tags_json (it
// would become a faux curated tag that wins over future analysis). A
// genuinely CURATED tag on the same field must survive the round-trip.
// Same class as TestUpsertTrackDoesNotPersistEnrichedField. (CodeRabbit #396.)
func TestSplicedReplayGainNotPersistedOnRoundTrip(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	upsertParent(t, s, "A/1.flac")       // no tag → analysis fills it
	if err := s.UpsertTrack(ctx, &Track{ // curated -5.0 baked into tags_json
		Path: "A/2.flac", Size: 100, ModTime: time.Now(),
		ReplayGainTrackDB: f64ptr(-5.0),
	}); err != nil {
		t.Fatalf("UpsertTrack A/2: %v", err)
	}
	for _, p := range []string{"A/1.flac", "A/2.flac"} {
		if err := s.UpsertAnalysis(ctx, AnalysisRow{
			SourcePath: p, WaveformPath: "/w/" + p, WaveformTag: "tag-" + p,
			SourceMTimeNS: 1, SourceSize: 2, SchemaVersion: "wf2", CreatedAt: 1,
			ReplayGainTrackDB: f64ptr(-9.0),
		}); err != nil {
			t.Fatalf("UpsertAnalysis %s: %v", p, err)
		}
	}

	// Read (splices analysis into A/1), then round-trip BOTH back through
	// the write path — exactly the footgun the marker guards against.
	tracks, err := s.ListTracks(ctx, nil)
	if err != nil {
		t.Fatalf("ListTracks: %v", err)
	}
	for i := range tracks {
		tr := tracks[i]
		if err := s.UpsertTrack(ctx, &tr); err != nil {
			t.Fatalf("round-trip UpsertTrack %s: %v", tr.Path, err)
		}
	}

	// GetTrack reads tags_json ONLY (no splice): the analysis value must be
	// gone for A/1, the curated tag must remain for A/2.
	a1, err := s.GetTrack(ctx, "A/1.flac")
	if err != nil {
		t.Fatalf("GetTrack A/1: %v", err)
	}
	if a1.ReplayGainTrackDB != nil {
		t.Fatalf("A/1 tags_json leaked analysis value %v, want nil (scrubbed)", *a1.ReplayGainTrackDB)
	}
	a2, err := s.GetTrack(ctx, "A/2.flac")
	if err != nil {
		t.Fatalf("GetTrack A/2: %v", err)
	}
	if a2.ReplayGainTrackDB == nil || *a2.ReplayGainTrackDB != -5.0 {
		t.Fatalf("A/2 curated tag = %v, want -5.0 (preserved)", a2.ReplayGainTrackDB)
	}

	// And the live analysis column must still re-splice for A/1 — the
	// scrub removed the frozen copy, not the source of truth.
	tracks2, err := s.ListTracks(ctx, nil)
	if err != nil {
		t.Fatalf("ListTracks (re-read): %v", err)
	}
	for i := range tracks2 {
		if tracks2[i].Path == "A/1.flac" {
			if tracks2[i].ReplayGainTrackDB == nil || *tracks2[i].ReplayGainTrackDB != -9.0 {
				t.Fatalf("A/1 re-splice = %v, want analysis -9.0", tracks2[i].ReplayGainTrackDB)
			}
		}
	}
}

// TestSpliceAnalysisReplayGain_SkipsNonFinite is the defense-in-depth
// twin of the extractor-side guard (2026-07-21 review Low):
// track_analysis is writable by an external sqlite3 CLI, so the REAL
// column is not trusted input — a hand-written NaN/±Inf must NOT reach
// the wire, where json.Marshal rejects non-finite floats and would
// crash /v1/manifest mid-stream at enc.Encode. The splice skips them;
// the track simply surfaces with no loudness.
func TestSpliceAnalysisReplayGain_SkipsNonFinite(t *testing.T) {
	cases := []struct {
		name string
		rg   sql.NullFloat64
		want *float64
	}{
		{"null column", sql.NullFloat64{}, nil},
		{"finite", sql.NullFloat64{Float64: -9.5, Valid: true}, f64ptr(-9.5)},
		{"nan", sql.NullFloat64{Float64: math.NaN(), Valid: true}, nil},
		{"+inf", sql.NullFloat64{Float64: math.Inf(1), Valid: true}, nil},
		{"-inf", sql.NullFloat64{Float64: math.Inf(-1), Valid: true}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var tr Track
			spliceAnalysisReplayGain(&tr, tc.rg)
			switch {
			case tc.want == nil && tr.ReplayGainTrackDB != nil:
				t.Fatalf("ReplayGainTrackDB = %v, want nil (non-finite skipped)", *tr.ReplayGainTrackDB)
			case tc.want != nil && tr.ReplayGainTrackDB == nil:
				t.Fatalf("ReplayGainTrackDB = nil, want %v", *tc.want)
			case tc.want != nil && *tr.ReplayGainTrackDB != *tc.want:
				t.Fatalf("ReplayGainTrackDB = %v, want %v", *tr.ReplayGainTrackDB, *tc.want)
			}
			if tc.want == nil && tr.replayGainFromAnalysis {
				t.Error("replayGainFromAnalysis set for a skipped value — marshalForStorage would scrub a real tag")
			}
		})
	}

	// Tag-present track under the guard: a non-finite analysis value
	// must not touch the curated tag (tag-absent-only contract holds).
	tag := &Track{ReplayGainTrackDB: f64ptr(-5.0)}
	spliceAnalysisReplayGain(tag, sql.NullFloat64{Float64: math.Inf(1), Valid: true})
	if tag.ReplayGainTrackDB == nil || *tag.ReplayGainTrackDB != -5.0 {
		t.Fatalf("curated tag = %v, want -5.0 (untouched)", tag.ReplayGainTrackDB)
	}
}
