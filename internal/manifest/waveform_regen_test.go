package manifest

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// seedRepairRow stores a track and an analysis row for path whose
// measurements are values no decode of the track would produce, and returns
// the row as read back.
func seedRepairRow(t *testing.T, s *Store, path string) AnalysisRow {
	t.Helper()
	ctx := context.Background()
	if err := s.UpsertTrack(ctx, &Track{Path: path, Size: 4321, ModTime: time.Unix(0, 9_000_000_000)}); err != nil {
		t.Fatal(err)
	}
	rg, peak := -3.25, 0.75
	key, bpm, dr, bw := 9, 171, 3, 15001
	if err := s.UpsertAnalysis(ctx, AnalysisRow{
		SourcePath: path, WaveformPath: "/old/waveforms/" + path + ".waveform.bin", WaveformTag: "0badc0de",
		WaveformSize: 99, SourceMTimeNS: 9_000_000_000, SourceSize: 4321, SchemaVersion: "wf7",
		CreatedAt: 1234, ReplayGainTrackDB: &rg, KeyRoot: &key, KeyMode: "minor", BPM: &bpm,
		TruePeakDB: &peak, DRScore: &dr, AudioMD5State: "", AudioMD5Retryable: true, BandwidthHz: &bw,
		Spectrum: []byte("1BSP-seeded"),
	}); err != nil {
		t.Fatal(err)
	}
	row, err := s.GetAnalysis(ctx, path)
	if err != nil || row == nil {
		t.Fatalf("seeded row: %v %v", row, err)
	}
	if row.AudioMD5Attempts != 1 {
		t.Fatalf("fixture: audio_md5_attempts = %d, want 1 (a retry counter a whole analysis would move)", row.AudioMD5Attempts)
	}
	return *row
}

// sameButTheWaveform compares two rows column by column, but for the three
// waveform columns.
func sameButTheWaveform(a, b AnalysisRow) bool {
	return a.SourcePath == b.SourcePath && a.SourceMTimeNS == b.SourceMTimeNS && a.SourceSize == b.SourceSize &&
		a.SchemaVersion == b.SchemaVersion && a.CreatedAt == b.CreatedAt &&
		float64PtrEqual(a.ReplayGainTrackDB, b.ReplayGainTrackDB) && intPtrEqual(a.KeyRoot, b.KeyRoot) &&
		a.KeyMode == b.KeyMode && intPtrEqual(a.BPM, b.BPM) && float64PtrEqual(a.TruePeakDB, b.TruePeakDB) &&
		intPtrEqual(a.DRScore, b.DRScore) && a.AudioMD5State == b.AudioMD5State &&
		a.AudioMD5Attempts == b.AudioMD5Attempts && intPtrEqual(a.BandwidthHz, b.BandwidthHz) &&
		bytes.Equal(a.Spectrum, b.Spectrum)
}

// TestARepairsCommitWritesTheWaveformAlone: CommitWaveformRepair writes the
// three waveform columns, and every measurement the row carries (loudness,
// true peak, key, tempo, dynamic range, the spectrum and its bandwidth, the
// audio-MD5 verdict and its retry counter, the schema and created_at) stays
// as it was. Red on 3d0a0d05, which has no such write.
func TestARepairsCommitWritesTheWaveformAlone(t *testing.T) {
	s := openTestStore(t)
	base := seedRepairRow(t, s, "Artist/Album/01.flac")
	bumped, err := s.CommitWaveformRepair(context.Background(), base, "/new/waveforms/01.flac.waveform.bin", "feedface", 2048)
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if !bumped {
		t.Error("a curve whose tag changed did not advance indexed_at")
	}
	row, err := s.GetAnalysis(context.Background(), base.SourcePath)
	if err != nil || row == nil {
		t.Fatalf("row: %v %v", row, err)
	}
	if row.WaveformPath != "/new/waveforms/01.flac.waveform.bin" || row.WaveformTag != "feedface" || row.WaveformSize != 2048 {
		t.Errorf("waveform columns = %s %s %d, want the repair's", row.WaveformPath, row.WaveformTag, row.WaveformSize)
	}
	if !sameButTheWaveform(*row, base) {
		t.Errorf("the repair's commit moved a measurement:\n got %+v\nwant %+v", *row, base)
	}
}

// TestARepairMovesIndexedAtOnlyWhenTheTagChanges: a client keys its cached
// curve on the tag, so a curve rendered again byte for byte moves nothing a
// delta carries, and one that changed moves `indexed_at` past every stamp
// held (bumpIndexedAtByPathSQL). Red on 3d0a0d05.
func TestARepairMovesIndexedAtOnlyWhenTheTagChanges(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	base := seedRepairRow(t, s, "Artist/Album/01.flac")
	before := indexedAtOf(t, s, base.SourcePath)

	bumped, err := s.CommitWaveformRepair(ctx, base, "/new/01.flac.waveform.bin", base.WaveformTag, base.WaveformSize)
	if err != nil {
		t.Fatal(err)
	}
	if bumped || indexedAtOf(t, s, base.SourcePath) != before {
		t.Fatalf("a repair with the row's own tag moved indexed_at (%d to %d, bumped %t): the curve a client "+
			"holds is the curve rendered again", before, indexedAtOf(t, s, base.SourcePath), bumped)
	}
	again, err := s.GetAnalysis(ctx, base.SourcePath)
	if err != nil || again == nil {
		t.Fatal(err)
	}
	bumped, err = s.CommitWaveformRepair(ctx, *again, "/new/01.flac.waveform.bin", "feedface", 2048)
	if err != nil {
		t.Fatal(err)
	}
	if after := indexedAtOf(t, s, base.SourcePath); !bumped || after <= before {
		t.Fatalf("a repair whose tag changed left indexed_at at %d (was %d, bumped %t): clients would keep "+
			"a curve that is not the one served", after, before, bumped)
	}
}

// TestARepairsCommitWritesNothingOverARowThatChanged: the commit is a
// compare-and-swap on the row the repair started from. An analysis that
// rewrote the row meanwhile (a new created_at), an adoption (a new
// waveform_path), a changed source version, and a deleted row each leave the
// row as the other writer left it, ErrAnalysisChanged.
func TestARepairsCommitWritesNothingOverARowThatChanged(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(t *testing.T, s *Store, base AnalysisRow)
	}{
		{"an analysis rewrote it", func(t *testing.T, s *Store, base AnalysisRow) {
			next := base
			next.CreatedAt = base.CreatedAt + 1
			next.WaveformTag = "c0ffee00"
			if err := s.UpsertAnalysis(context.Background(), next); err != nil {
				t.Fatal(err)
			}
		}},
		{"an adoption moved its path", func(t *testing.T, s *Store, base AnalysisRow) {
			if err := s.UpdateAnalysisWaveformPath(context.Background(), base.SourcePath, "/adopted/01.waveform.bin"); err != nil {
				t.Fatal(err)
			}
		}},
		{"its source changed", func(t *testing.T, s *Store, base AnalysisRow) {
			next := base
			next.SourceMTimeNS++
			if err := s.UpsertAnalysis(context.Background(), next); err != nil {
				t.Fatal(err)
			}
		}},
		{"it was deleted", func(t *testing.T, s *Store, base AnalysisRow) {
			if err := s.DeleteAnalysis(context.Background(), base.SourcePath); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := openTestStore(t)
			base := seedRepairRow(t, s, "Artist/Album/01.flac")
			tc.change(t, s, base)
			want, err := s.GetAnalysis(context.Background(), base.SourcePath)
			if err != nil {
				t.Fatal(err)
			}
			mark := indexedAtOf(t, s, base.SourcePath)
			_, err = s.CommitWaveformRepair(context.Background(), base, "/repair/01.waveform.bin", "feedface", 2048)
			if !errors.Is(err, ErrAnalysisChanged) {
				t.Fatalf("commit over a row that changed = %v, want ErrAnalysisChanged", err)
			}
			got, err := s.GetAnalysis(context.Background(), base.SourcePath)
			if err != nil {
				t.Fatal(err)
			}
			if (got == nil) != (want == nil) || (got != nil && (*got).WaveformPath != (*want).WaveformPath) ||
				(got != nil && (*got).WaveformTag != (*want).WaveformTag) {
				t.Errorf("the refused commit wrote: %+v, want %+v", got, want)
			}
			if indexedAtOf(t, s, base.SourcePath) != mark {
				t.Error("the refused commit moved indexed_at")
			}
		})
	}
}

// TestTheRegenerationFailureCountPersistsAndResets: a counted failure is
// durable, so two of them stop the curve across a restart of the store; a
// failure against another build, or another version of the source, starts
// the count again at one; a success clears it; and the record goes with its
// analysis row. Red on 3d0a0d05, which keeps no such count.
func TestTheRegenerationFailureCountPersistsAndResets(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "bridge.db")
	s, err := OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	base := seedRepairRow(t, s, "Artist/Album/01.flac")
	p, mt, sz := base.SourcePath, base.SourceMTimeNS, base.SourceSize

	for want := 1; want <= 2; want++ {
		n, err := s.RecordWaveformRegenFailure(ctx, p, "build-a", mt, sz, "write: no space left on device")
		if err != nil || n != want {
			t.Fatalf("failure %d: count = %d, %v; want %d", want, n, err, want)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	f, err := s.GetWaveformRegenFailure(ctx, p)
	if err != nil || !f.StopsFor("build-a", mt, sz) {
		t.Fatalf("after a restart the record is %+v (%v); want two failures that stop build-a", f, err)
	}
	if f.StopsFor("build-b", mt, sz) || f.StopsFor("build-a", mt+1, sz) || f.StopsFor("build-a", mt, sz+1) {
		t.Error("a record counted against build-a and this version stops another build or version too")
	}
	stopped, err := s.StoppedWaveformRegens(ctx, "build-a")
	if err != nil || len(stopped) != 1 || stopped[0].SourcePath != p {
		t.Errorf("StoppedWaveformRegens(build-a) = %+v, %v; want the one curve", stopped, err)
	}
	if other, err := s.StoppedWaveformRegens(ctx, "build-b"); err != nil || len(other) != 0 {
		t.Errorf("StoppedWaveformRegens(build-b) = %+v, %v; want none", other, err)
	}

	if n, err := s.RecordWaveformRegenFailure(ctx, p, "build-b", mt, sz, "again"); err != nil || n != 1 {
		t.Errorf("a failure under a new build counted %d (%v), want 1", n, err)
	}
	if n, err := s.RecordWaveformRegenFailure(ctx, p, "build-b", mt+1, sz, "again"); err != nil || n != 1 {
		t.Errorf("a failure against a changed source counted %d (%v), want 1", n, err)
	}
	if err := s.ClearWaveformRegenFailure(ctx, p); err != nil {
		t.Fatal(err)
	}
	if f, err := s.GetWaveformRegenFailure(ctx, p); err != nil || f != nil {
		t.Errorf("after the clear the record is %+v (%v), want none", f, err)
	}

	if _, err := s.RecordWaveformRegenFailure(ctx, p, "build-a", mt, sz, "again"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteAnalysis(ctx, p); err != nil {
		t.Fatal(err)
	}
	if f, err := s.GetWaveformRegenFailure(ctx, p); err != nil || f != nil {
		t.Errorf("the record outlived its analysis row: %+v (%v)", f, err)
	}
	if n, err := s.RecordWaveformRegenFailure(ctx, p, "build-a", mt, sz, "no row"); err != nil || n != 0 {
		t.Errorf("a failure for a row that is gone counted %d (%v), want 0 and nothing written", n, err)
	}
}

// TestARegenerationFailureMovesNothingAClientSees: the count lives beside
// the row and touches neither the track nor its delta stamp.
func TestARegenerationFailureMovesNothingAClientSees(t *testing.T) {
	s := openTestStore(t)
	base := seedRepairRow(t, s, "Artist/Album/01.flac")
	mark := indexedAtOf(t, s, base.SourcePath)
	if _, err := s.RecordWaveformRegenFailure(context.Background(), base.SourcePath, "b", base.SourceMTimeNS,
		base.SourceSize, strings.Repeat("x", 2000)); err != nil {
		t.Fatal(err)
	}
	if indexedAtOf(t, s, base.SourcePath) != mark {
		t.Error("a counted failure moved indexed_at")
	}
	f, err := s.GetWaveformRegenFailure(context.Background(), base.SourcePath)
	if err != nil || f == nil || len(f.Reason) > analysisFailureReasonMax {
		t.Errorf("record = %+v (%v): the reason must be bounded like an analysis failure's", f, err)
	}
}

// TestRecordedWaveformsListsTheRowsThatRecordACurve: the guard's catalog is
// every row with a waveform path, and no other.
func TestRecordedWaveformsListsTheRowsThatRecordACurve(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedRepairRow(t, s, "B/02.flac")
	seedRepairRow(t, s, "A/01.flac")
	if err := s.UpsertTrack(ctx, &Track{Path: "C/03.flac", Size: 1, ModTime: time.Unix(1, 0)}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertAnalysis(ctx, AnalysisRow{SourcePath: "C/03.flac", SourceMTimeNS: 1, SourceSize: 1,
		SchemaVersion: "wf7", CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	got, err := s.RecordedWaveforms(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].SourcePath != "A/01.flac" || got[1].SourcePath != "B/02.flac" ||
		got[0].WaveformSize != 99 || !strings.HasSuffix(got[0].WaveformPath, "A/01.flac.waveform.bin") {
		t.Errorf("RecordedWaveforms = %+v, want A and B, in path order, with their recorded path and size", got)
	}
}

// TestSuppressedAnalysisVersionIsTheSetsAnswerForOnePath: one path's answer
// is the suppressed set's, version included.
func TestSuppressedAnalysisVersionIsTheSetsAnswerForOnePath(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	base := seedRepairRow(t, s, "A/01.flac")
	if _, ok, err := s.SuppressedAnalysisVersion(ctx, base.SourcePath); err != nil || ok {
		t.Fatalf("a path with no verdicts reads suppressed (%t, %v)", ok, err)
	}
	for i := 0; i < AnalysisFailureThreshold(); i++ {
		if _, err := s.RecordAnalysisFailure(ctx, base.SourcePath, "sox: truncated"); err != nil {
			t.Fatal(err)
		}
	}
	set, err := s.SuppressedAnalysisPaths(ctx)
	if err != nil {
		t.Fatal(err)
	}
	v, ok, err := s.SuppressedAnalysisVersion(ctx, base.SourcePath)
	if err != nil || !ok || v != set[base.SourcePath] {
		t.Errorf("SuppressedAnalysisVersion = %+v, %t, %v; want the set's %+v", v, ok, err, set[base.SourcePath])
	}
}
