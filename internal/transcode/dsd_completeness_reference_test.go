package transcode

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/analyze"
	"github.com/acoseac/1-bit-bridge/internal/dsdtone"
)

// TestACompleteDSFWithATrailingID3PassesTheRenderAndTheSurvey is a DSF
// whose fmt sample count matches its data chunk and whose trailing ID3
// chunk holds a large picture. ffprobe counts those bytes as audio. The
// render and the survey take the fmt sample count.
func TestACompleteDSFWithATrailingID3PassesTheRenderAndTheSurvey(t *testing.T) {
	requireDSDToolchain(t)
	const secs = 10
	path := filepath.Join(t.TempDir(), "art.dsf")
	samples := int64(secs) * dsfSilenceRate
	writeSilenceDSF(t, path, samples, samples)
	appendDSFTrailer(t, path, id3WithBody(1_500_000))

	probed, produced, surveyErr := surveyDecodeLengths(t, path, 99)
	if surveyErr != nil {
		t.Fatalf("survey: %v (ffprobe %.3fs, produced %.3fs)", surveyErr, probed, produced)
	}
	if math.Abs(produced-float64(secs)) > 0.05 {
		t.Fatalf("produced %.3fs, fmt %.3fs (ffprobe %.3fs)", produced, float64(secs), probed)
	}
	if err := renderDSDFile(t, path, 99); err != nil {
		t.Fatalf("render: %v", err)
	}
	if _, err := analyze.RunAnalysis(context.Background(), analyze.AnalyzeSpec{
		SourceAbsPath:    path,
		SourceLibraryRel: filepath.Base(path),
		OutputDir:        t.TempDir(),
	}); err != nil {
		t.Fatalf("analysis: %v", err)
	}
}

// TestADSFTruncatedMidDataIsRefused cuts a complete DSF inside its data
// chunk and leaves the fmt sample count and the data-chunk size claiming
// the whole file. ffprobe then reports the bytes that remain.
func TestADSFTruncatedMidDataIsRefused(t *testing.T) {
	requireDSDToolchain(t)
	const secs = 10
	path := filepath.Join(t.TempDir(), "trunc.dsf")
	samples := int64(secs) * dsfSilenceRate
	writeSilenceDSF(t, path, samples, samples)
	truncateFile(t, path, 28+52+12+4*1024*1024)

	_, _, surveyErr := surveyDecodeLengths(t, path, float64(secs))
	if !errors.Is(surveyErr, ErrFFmpegDecodeIncomplete) {
		t.Fatalf("survey: %v", surveyErr)
	}
	if err := renderDSDFile(t, path, float64(secs)); !errors.Is(err, ErrFFmpegDecodeIncomplete) {
		t.Fatalf("render: %v", err)
	}
}

// TestADSFHeaderThatDisagreesWithItsDataChunkIsMalformed is a header whose
// sample count and data chunk differ by more than one 4096-byte block.
func TestADSFHeaderThatDisagreesWithItsDataChunkIsMalformed(t *testing.T) {
	requireDSDToolchain(t)
	path := filepath.Join(t.TempDir(), "bad.dsf")
	samples := int64(dsfSilenceRate)
	declared := samples + int64(8*(4096*2+1))
	writeSilenceDSF(t, path, samples, declared)

	_, _, surveyErr := surveyDecodeLengths(t, path, float64(declared)/dsfSilenceRate)
	if !dsfHeaderMalformed(surveyErr) {
		t.Fatalf("survey: %v", surveyErr)
	}
	if err := renderDSDFile(t, path, float64(declared)/dsfSilenceRate); !dsfHeaderMalformed(err) {
		t.Fatalf("render: %v", err)
	}
}

// TestAnUncompressedDFFWithATrailingID3PassesTheRenderAndTheSurvey is an
// uncompressed DSDIFF whose DSD chunk is 2 s and whose trailing ID3 chunk
// is 1.5e6 bytes. Measured with FFmpeg 9.0.2, ffprobe reports 4.126 s for
// that file whether the ID3 chunk sits inside the form or after it.
func TestAnUncompressedDFFWithATrailingID3PassesTheRenderAndTheSurvey(t *testing.T) {
	requireDSDToolchain(t)
	const secs = 2
	path := filepath.Join(t.TempDir(), "art.dff")
	writeSilenceDFF(t, path, secs)
	appendDFFID3(t, path, 1_500_000)

	probed, produced, surveyErr := surveyDecodeLengths(t, path, 99)
	if surveyErr != nil {
		t.Fatalf("survey: %v (ffprobe %.3fs, produced %.3fs)", surveyErr, probed, produced)
	}
	if math.Abs(produced-float64(secs)) > 0.05 {
		t.Fatalf("produced %.3fs, chunk %.3fs (ffprobe %.3fs)", produced, float64(secs), probed)
	}
	if err := renderDSDFile(t, path, 99); err != nil {
		t.Fatalf("render: %v", err)
	}
	if _, err := analyze.RunAnalysis(context.Background(), analyze.AnalyzeSpec{
		SourceAbsPath:    path,
		SourceLibraryRel: filepath.Base(path),
		OutputDir:        t.TempDir(),
	}); err != nil {
		t.Fatalf("analysis: %v", err)
	}
}

// dsfHeaderMalformed is the refusal for a DSF whose sample count and data
// chunk disagree. It is not a short decode: a short decode is remembered
// for an album mate, and a malformed header is a fact about the file.
func dsfHeaderMalformed(err error) bool {
	return err != nil && !errors.Is(err, ErrFFmpegDecodeIncomplete) &&
		bytes.Contains([]byte(err.Error()), []byte("malformed"))
}

func renderDSDFile(t *testing.T, path string, manifestSec float64) error {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = (JobSpec{
		SourceAbsPath:     path,
		SourceLibraryRel:  filepath.Base(path),
		SourceMTimeNS:     info.ModTime().UnixNano(),
		SourceSize:        info.Size(),
		SourceSampleRate:  dsfSilenceRate,
		SourceIsDSD:       true,
		SourceChannels:    2,
		SourceDurationSec: manifestSec,
		TargetSampleRate:  44100,
		TargetBits:        16,
		Quality:           QualityVeryHigh,
		OutputDir:         t.TempDir(),
		TempDir:           t.TempDir(),
		Kind:              JobKindOptimize,
	}).renderDSD(context.Background())
	return err
}

func truncateFile(t *testing.T, path string, n int64) {
	t.Helper()
	if err := os.Truncate(path, n); err != nil {
		t.Fatal(err)
	}
}

func writeSilenceDFF(t *testing.T, path string, secs int) {
	t.Helper()
	packed := bytes.Repeat([]byte{0x69}, secs*dsfSilenceRate/8)
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	err = dsdtone.WriteDFF(f, dsfSilenceRate, 2, packed)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		t.Fatal(err)
	}
}

// appendDFFID3 writes an ID3 chunk after the DSD chunk, inside the form,
// and extends the FRM8 size to cover it. The DSD chunk's own length stays.
func appendDFFID3(t *testing.T, path string, body int) {
	t.Helper()
	payload := make([]byte, body)
	chunk := make([]byte, 12+len(payload))
	copy(chunk[0:4], "ID3 ")
	binary.BigEndian.PutUint64(chunk[4:12], uint64(len(payload)))
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var size [8]byte
	if _, err := f.ReadAt(size[:], 4); err != nil {
		t.Fatal(err)
	}
	binary.BigEndian.PutUint64(size[:], binary.BigEndian.Uint64(size[:])+uint64(len(chunk)))
	if _, err := f.WriteAt(size[:], 4); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(chunk); err != nil {
		t.Fatal(err)
	}
}

// id3WithBody is an ID3v2.3 APIC frame whose payload is body bytes long.
func id3WithBody(body int) []byte {
	frame := make([]byte, 10+body)
	copy(frame[0:4], "APIC")
	binary.BigEndian.PutUint32(frame[4:8], uint32(body))
	n := len(frame)
	tag := make([]byte, 10+n)
	copy(tag[0:3], "ID3")
	tag[3] = 3
	tag[6] = byte((n >> 21) & 0x7f)
	tag[7] = byte((n >> 14) & 0x7f)
	tag[8] = byte((n >> 7) & 0x7f)
	tag[9] = byte(n & 0x7f)
	copy(tag[10:], frame)
	return tag
}
