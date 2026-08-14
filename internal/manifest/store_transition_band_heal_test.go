package manifest

import (
	"context"
	"encoding/binary"
	"testing"
	"time"
)

// Migration v34's healer: wf7 rows whose bandwidth is a decode-filter
// artifact (≥ 22800 Hz — inside sox's transition band) become "no
// measurement" in BOTH stores of the value (column + served blob), with
// the parent track's indexed_at strictly advanced so delta-syncs carry
// the correction; rows below the band are untouched byte-for-byte.
//
// The blob assertions matter as much as the column ones: /v1/spectrum
// serves the blob VERBATIM (one parser on both sides), so a healed
// column over an unhealed blob would have the manifest and the curve
// disagreeing about the same measurement.

// heal1BSPBlob builds an 84-byte 1BSP v2 blob the way the wf7 encoder
// laid it out. Hand-built because package manifest cannot import
// internal/analyze (analyze imports manifest); offsets pinned by the
// byte assertions below, and the healed form is additionally asserted
// against the live encoder from internal/api's tests, which CAN import
// both.
func heal1BSPBlob(bandwidthHz uint32, cliffTenths uint16) []byte {
	out := make([]byte, 0, 84)
	out = append(out, '1', 'B', 'S', 'P', 2, 0)
	out = binary.LittleEndian.AppendUint32(out, 48000)
	out = binary.LittleEndian.AppendUint32(out, 500) // windows
	out = binary.LittleEndian.AppendUint32(out, 60)
	out = binary.LittleEndian.AppendUint32(out, bandwidthHz)
	out = binary.LittleEndian.AppendUint16(out, cliffTenths)
	for i := 0; i < 60; i++ {
		out = append(out, byte(30+i%7)) // arbitrary, recognisable bands
	}
	return out
}

func seedWF7SpectrumRow(t *testing.T, s *Store, path string, bandwidthHz int, blob []byte) {
	t.Helper()
	ctx := context.Background()
	if err := s.UpsertTrack(ctx, &Track{
		Path: path, Size: 1, ModTime: time.Unix(0, 0).UTC(),
	}); err != nil {
		t.Fatalf("seed track %q: %v", path, err)
	}
	bw := bandwidthHz
	if err := s.UpsertAnalysis(ctx, AnalysisRow{
		SourcePath:    path,
		SourceMTimeNS: 1,
		SourceSize:    1,
		SchemaVersion: "wf7",
		CreatedAt:     time.Unix(1_700_000_000, 0).Unix(),
		BandwidthHz:   &bw,
		Spectrum:      blob,
	}); err != nil {
		t.Fatalf("seed analysis %q: %v", path, err)
	}
}

func readHealState(t *testing.T, s *Store, path string) (bw *int, blob []byte, indexedAt int64) {
	t.Helper()
	if err := s.db.QueryRow(
		`SELECT ta.bandwidth_hz, ta.spectrum, t.indexed_at
		   FROM track_analysis ta JOIN tracks t ON t.path = ta.source_path
		  WHERE ta.source_path = ?`, path,
	).Scan(&bw, &blob, &indexedAt); err != nil {
		t.Fatalf("read heal state for %q: %v", path, err)
	}
	return bw, blob, indexedAt
}

func TestHealTransitionBandBandwidths(t *testing.T) {
	s := openTestStore(t)

	// The Blue Train shape: an all-analog master whose "wall" at 23.4 kHz
	// was sox's filter, cliff 63.8 dB (the filter's slope).
	artifactBlob := heal1BSPBlob(23414, 638)
	seedWF7SpectrumRow(t, s, "Jazz/Blue Train/01.flac", 23414, artifactBlob)
	// A REAL wall, safely below the transition band — the Aerosmith case.
	realBlob := heal1BSPBlob(21891, 864)
	seedWF7SpectrumRow(t, s, "Rock/Get A Grip/03.flac", 21891, realBlob)

	_, _, artIdx0 := readHealState(t, s, "Jazz/Blue Train/01.flac")
	_, _, realIdx0 := readHealState(t, s, "Rock/Get A Grip/03.flac")

	if err := healTransitionBandBandwidths(s.db); err != nil {
		t.Fatalf("heal: %v", err)
	}

	bw, blob, artIdx := readHealState(t, s, "Jazz/Blue Train/01.flac")
	// Length FIRST — a heal that truncated the blob must fail with this
	// message, not panic in the field reads below (Gemini on PR #687).
	if len(blob) != len(artifactBlob) {
		t.Fatalf("blob length changed: %d -> %d", len(artifactBlob), len(blob))
	}
	if bw != nil {
		t.Errorf("artifact row still carries bandwidth %d — the column was not healed", *bw)
	}
	if got := binary.LittleEndian.Uint32(blob[18:22]); got != 0 {
		t.Errorf("blob bandwidth field = %d, want 0 (absent) — the served curve "+
			"would contradict the manifest", got)
	}
	if got := binary.LittleEndian.Uint16(blob[22:24]); got != 0xFFFF {
		t.Errorf("blob cliff field = %#x, want 0xFFFF (absent) — a filter-slope "+
			"cliff would survive on the wire", got)
	}
	for i := 24; i < len(blob); i++ {
		if blob[i] != artifactBlob[i] {
			t.Fatalf("display band byte %d changed %d -> %d — the heal must "+
				"touch only the measurement fields", i, artifactBlob[i], blob[i])
		}
	}
	if artIdx <= artIdx0 {
		t.Errorf("indexed_at did not advance (%d -> %d) — synced clients would "+
			"keep the wrong readout forever", artIdx0, artIdx)
	}

	// The control row: untouched in every particular.
	bw, blob, realIdx := readHealState(t, s, "Rock/Get A Grip/03.flac")
	if bw == nil || *bw != 21891 {
		t.Errorf("real-wall row's bandwidth was disturbed: %v", bw)
	}
	if string(blob) != string(realBlob) {
		t.Error("real-wall row's blob was rewritten")
	}
	if realIdx != realIdx0 {
		t.Errorf("real-wall row's indexed_at moved %d -> %d — a no-op heal must "+
			"not churn deltas", realIdx0, realIdx)
	}

	// Idempotency: a second run finds nothing and moves nothing.
	if err := healTransitionBandBandwidths(s.db); err != nil {
		t.Fatalf("second heal: %v", err)
	}
	if _, _, again := readHealState(t, s, "Jazz/Blue Train/01.flac"); again != artIdx {
		t.Errorf("re-run advanced indexed_at %d -> %d — the heal is not idempotent", artIdx, again)
	}
}

// The exact boundary: 22800 is IN the transition band (sox's passband edge
// is exclusive of it), 22799 is out.
func TestHealTransitionBandBoundary(t *testing.T) {
	s := openTestStore(t)
	seedWF7SpectrumRow(t, s, "A/edge-in.flac", 22800, heal1BSPBlob(22800, 400))
	seedWF7SpectrumRow(t, s, "A/edge-out.flac", 22799, heal1BSPBlob(22799, 400))
	if err := healTransitionBandBandwidths(s.db); err != nil {
		t.Fatalf("heal: %v", err)
	}
	if bw, _, _ := readHealState(t, s, "A/edge-in.flac"); bw != nil {
		t.Errorf("22800 survived (= %d) — the passband edge itself is filtered territory", *bw)
	}
	if bw, _, _ := readHealState(t, s, "A/edge-out.flac"); bw == nil || *bw != 22799 {
		t.Errorf("22799 was healed away — below the edge is a real measurement, got %v", bw)
	}
}
