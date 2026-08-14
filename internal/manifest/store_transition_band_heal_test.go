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

// wf7MeasuredBlob / wf7AbsentBlob are CAPTURED from the real encoder —
// `analyze.EncodeSpectrum` run 2026-08-14 with bands[i] = -30-(i%7),
// windows 500, and (bandwidth 23414 Hz, cliff 63.8 dB) vs no measurement
// — NOT hand-derived (the iOS band-map fixture pattern: an earlier
// hand-built blob here pinned only its author's beliefs about the
// layout, which is how #686's 80-byte fixture happened; CodeRabbit on
// #687). Package manifest cannot import internal/analyze (analyze
// imports manifest), so captured bytes are the only way to pin the
// migration against the ENCODER rather than against a second copy of
// the offsets. Deliberately frozen: v34 heals rows written by the
// wf7-era encoder, so these bytes are the frozen contract even if the
// encoder later changes — while the api-side
// TestMigrationV34PatchMatchesTheEncoder fails if the live encoder's
// layout drifts, flagging that a NEW migration (not an edit to v34)
// would be needed for post-drift rows.
var wf7MeasuredBlob = []byte{
	0x31, 0x42, 0x53, 0x50, 0x02, 0x00, 0x80, 0xbb, 0x00, 0x00, 0xf4, 0x01,
	0x00, 0x00, 0x3c, 0x00, 0x00, 0x00, 0x76, 0x5b, 0x00, 0x00, 0x7e, 0x02,
	0x1e, 0x1f, 0x20, 0x21, 0x22, 0x23, 0x24, 0x1e, 0x1f, 0x20, 0x21, 0x22,
	0x23, 0x24, 0x1e, 0x1f, 0x20, 0x21, 0x22, 0x23, 0x24, 0x1e, 0x1f, 0x20,
	0x21, 0x22, 0x23, 0x24, 0x1e, 0x1f, 0x20, 0x21, 0x22, 0x23, 0x24, 0x1e,
	0x1f, 0x20, 0x21, 0x22, 0x23, 0x24, 0x1e, 0x1f, 0x20, 0x21, 0x22, 0x23,
	0x24, 0x1e, 0x1f, 0x20, 0x21, 0x22, 0x23, 0x24, 0x1e, 0x1f, 0x20, 0x21,
}
var wf7AbsentBlob = []byte{
	0x31, 0x42, 0x53, 0x50, 0x02, 0x00, 0x80, 0xbb, 0x00, 0x00, 0xf4, 0x01,
	0x00, 0x00, 0x3c, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0xff, 0xff,
	0x1e, 0x1f, 0x20, 0x21, 0x22, 0x23, 0x24, 0x1e, 0x1f, 0x20, 0x21, 0x22,
	0x23, 0x24, 0x1e, 0x1f, 0x20, 0x21, 0x22, 0x23, 0x24, 0x1e, 0x1f, 0x20,
	0x21, 0x22, 0x23, 0x24, 0x1e, 0x1f, 0x20, 0x21, 0x22, 0x23, 0x24, 0x1e,
	0x1f, 0x20, 0x21, 0x22, 0x23, 0x24, 0x1e, 0x1f, 0x20, 0x21, 0x22, 0x23,
	0x24, 0x1e, 0x1f, 0x20, 0x21, 0x22, 0x23, 0x24, 0x1e, 0x1f, 0x20, 0x21,
}

// heal1BSPBlob builds an 84-byte 1BSP v2 blob for rows where only the
// VALUES matter (the boundary cases and the untouched control row) —
// the captured fixtures above are the layout authority.
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
	// was sox's filter, cliff 63.8 dB (the filter's slope) — seeded with
	// the REAL encoder's bytes for that measurement.
	artifactBlob := wf7MeasuredBlob
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
	// THE pin: the healed blob must equal what the real encoder emits for
	// the same result with no measurement — migration and encoder agree
	// through captured bytes, not through two copies of the offsets
	// (CodeRabbit Major on PR #687). The per-field checks below only
	// localise a failure.
	if string(blob) != string(wf7AbsentBlob) {
		t.Errorf("healed blob != the encoder's measurement-absent form:\n got: % x\nwant: % x",
			blob[:24], wf7AbsentBlob[:24])
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

// The defensive branch: a row whose bandwidth is in the band but whose blob
// is too short to carry the fields (unreachable from the real analyzer —
// every wf7 row with a bandwidth has an 84-byte blob — but the migration
// guards it, so the guard is pinned). The COLUMN is still healed and
// indexed_at still advances; the blob is left verbatim rather than sliced
// out of range. (CodeRabbit on PR #687.)
func TestHealTransitionBandShortBlob(t *testing.T) {
	s := openTestStore(t)
	short := []byte("1BSP-too-short")
	seedWF7SpectrumRow(t, s, "A/short.flac", 23000, short)
	_, _, idx0 := readHealState(t, s, "A/short.flac")

	if err := healTransitionBandBandwidths(s.db); err != nil {
		t.Fatalf("heal errored on a short blob: %v", err)
	}
	bw, blob, idx := readHealState(t, s, "A/short.flac")
	if bw != nil {
		t.Errorf("column not healed for the short-blob row: %d", *bw)
	}
	if string(blob) != string(short) {
		t.Errorf("short blob was rewritten: % x", blob)
	}
	if idx <= idx0 {
		t.Errorf("indexed_at did not advance (%d -> %d)", idx0, idx)
	}
}
