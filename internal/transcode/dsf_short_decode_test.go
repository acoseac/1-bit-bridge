package transcode

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/analyze"
	"github.com/acoseac/1-bit-bridge/internal/dsdtone"
)

// dsfSilenceRate is DSD64: 64 × 44.1 kHz. A byte of 0x69 is DSD silence
// (0x00 is full-scale offset), so a long fixture does not need a modulator.
const dsfSilenceRate = 2822400

// TestDSFShortDecodeFixtures measures three synthetic DSF files through the
// album survey's decode (MeasureDSDPeak's Stages A and B) and through
// analysis. The lengths are what decides whether a short decode is the
// file's header or the decoder. A well-formed file that fails here is a
// decoder bug; the completeness tolerance stays where it is.
func TestDSFShortDecodeFixtures(t *testing.T) {
	requireDSDToolchain(t)
	dir := t.TempDir()
	cases := []struct {
		name       string
		samples    int64 // per channel, the bytes the data chunk holds
		declared   int64 // per channel, the fmt chunk's sample count
		id3        bool
		wellFormed bool
	}{
		{
			name:     "header overstates the data by 3s",
			samples:  40 * dsfSilenceRate,
			declared: 43 * dsfSilenceRate,
		},
		{
			name:       "last block zero-padded",
			samples:    dsfSilenceRate, // 1s; not a multiple of the 4096-byte block
			declared:   dsfSilenceRate,
			wellFormed: true,
		},
		{
			name:       "id3 after the data",
			samples:    dsfSilenceRate,
			declared:   dsfSilenceRate,
			id3:        true,
			wellFormed: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, strings.ReplaceAll(tc.name, " ", "-")+".dsf")
			writeSilenceDSF(t, path, tc.samples, tc.declared)
			if tc.id3 {
				appendDSFID3(t, path)
			}
			headerSec := float64(tc.declared) / dsfSilenceRate
			dataSec := float64(tc.samples) / dsfSilenceRate
			probed, produced, surveyErr := surveyDecodeLengths(t, path, headerSec)
			_, analysisErr := analyze.RunAnalysis(context.Background(), analyze.AnalyzeSpec{
				SourceAbsPath:    path,
				SourceLibraryRel: filepath.Base(path),
				OutputDir:        filepath.Join(dir, "waveforms", tc.name),
			})
			t.Logf("header %.3fs data %.3fs ffprobe %.3fs produced %.3fs survey=%v analysis=%v",
				headerSec, dataSec, probed, produced, errText(surveyErr), errText(analysisErr))
			// ffprobe follows the data chunk. An fmt sample count past that
			// chunk does not become the survey's reference, and a zero pad
			// inside the last block, or an ID3 chunk after the data, does
			// not fail the 2% bar. A 2–4s shortfall is a different file.
			if math.Abs(probed-dataSec) > 0.05 {
				t.Errorf("ffprobe %.3fs, data %.3fs", probed, dataSec)
			}
			if surveyErr != nil {
				t.Errorf("survey decode: %v", surveyErr)
			}
			if math.Abs(produced-dataSec) > 0.05 {
				t.Errorf("produced %.3fs, data %.3fs", produced, dataSec)
			}
			if analysisErr != nil {
				t.Errorf("analysis: %v", analysisErr)
			}
			if !tc.wellFormed && math.Abs(probed-headerSec) < 1 {
				t.Errorf("ffprobe %.3fs followed the declared %.3fs", probed, headerSec)
			}
		})
	}
}

func writeSilenceDSF(t *testing.T, path string, samples, declared int64) {
	t.Helper()
	if samples%8 != 0 {
		t.Fatalf("samples %d is not a whole number of bytes", samples)
	}
	packed := bytes.Repeat([]byte{0x69}, int(samples/8))
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	err = dsdtone.WriteDSF(f, dsfSilenceRate, 2, packed, declared)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		t.Fatal(err)
	}
}

// appendDSFID3 points the DSF metadata pointer at a minimal ID3v2.3 tag
// written after the data chunk, and extends the header's file-size field
// to cover it. The fmt sample count is left as written.
func appendDSFID3(t *testing.T, path string) {
	t.Helper()
	tag := minimalID3v23()
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	old := info.Size()
	var field [8]byte
	binary.LittleEndian.PutUint64(field[:], uint64(old)+uint64(len(tag)))
	if _, err := f.WriteAt(field[:], 12); err != nil {
		t.Fatal(err)
	}
	binary.LittleEndian.PutUint64(field[:], uint64(old))
	if _, err := f.WriteAt(field[:], 20); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(0, os.SEEK_END); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(tag); err != nil {
		t.Fatal(err)
	}
}

func minimalID3v23() []byte {
	content := []byte{0, 't'} // ISO-8859-1, one character
	frame := make([]byte, 10+len(content))
	copy(frame[0:4], "TIT2")
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(content)))
	copy(frame[10:], content)
	n := len(frame)
	tag := []byte{
		'I', 'D', '3', 3, 0, 0,
		byte((n >> 21) & 0x7f),
		byte((n >> 14) & 0x7f),
		byte((n >> 7) & 0x7f),
		byte(n & 0x7f),
	}
	return append(tag, frame...)
}

// surveyDecodeLengths runs the measurement MeasureDSDPeak runs, and reports
// ffprobe's container duration beside the scratch's duration. On a
// length-incomplete decode the scratch's duration is taken from the error,
// which is the only place the check records it.
func surveyDecodeLengths(t *testing.T, path string, headerSec float64) (probed, produced float64, err error) {
	t.Helper()
	spec := JobSpec{
		SourceAbsPath: path, SourceLibraryRel: filepath.Base(path),
		SourceSampleRate: dsfSilenceRate, SourceIsDSD: true, SourceChannels: 2,
		SourceDurationSec: headerSec,
		TargetSampleRate:  44100, TargetBits: 16, Quality: QualityVeryHigh,
		TempDir: t.TempDir(), Kind: JobKindOptimize,
	}
	ctx := context.Background()
	geo, err := spec.dsdGeometry(ctx)
	if err != nil {
		t.Fatalf("geometry: %v", err)
	}
	scratchDir := renderScratchDir(spec.TempDir)
	if err := spec.mkdirScratch(scratchDir); err != nil {
		t.Fatal(err)
	}
	scratchPath := filepath.Join(scratchDir, nextSidecarTmpToken()+renderScratchSuffix)
	defer os.Remove(scratchPath)
	if err := createOutput(scratchPath, scratchDir); err != nil {
		t.Fatal(err)
	}
	_, samples, err := spec.decodeAndMeasure(ctx, geo, scratchDir, scratchPath)
	produced = float64(samples) / float64(spec.TargetSampleRate)
	if _, prod, ok := lengthsFromIncomplete(err); ok {
		return geo.Duration, prod, err
	}
	if err != nil && !errors.Is(err, ErrFFmpegDecodeIncomplete) {
		return geo.Duration, produced, err
	}
	return geo.Duration, produced, err
}

func lengthsFromIncomplete(err error) (source, produced float64, ok bool) {
	if err == nil {
		return 0, 0, false
	}
	_, rest, found := strings.Cut(err.Error(), "source ")
	if !found {
		return 0, 0, false
	}
	srcText, rest, found := strings.Cut(rest, "s, produced ")
	if !found {
		return 0, 0, false
	}
	prodText, _, _ := strings.Cut(rest, "s")
	source, err1 := strconv.ParseFloat(srcText, 64)
	produced, err2 := strconv.ParseFloat(prodText, 64)
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return source, produced, true
}

func errText(err error) string {
	if err == nil {
		return "pass"
	}
	return fmt.Sprintf("fail (%v)", err)
}
