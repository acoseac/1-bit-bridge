package manifest

import (
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// ExtractorVersion 12: every derived Duration passes plausibleDuration.
//
// v11 introduced the gate and its docblock already claimed the whole
// set — "every derived duration (MP4 / MP3 / AIFF / WAV, v11) applies
// the SAME ceiling through plausibleDuration" — while three older
// sites computed theirs and stamped it straight onto the Track: FLAC
// (STREAMINFO's 36-bit totalSamples), DSF (a full uint64 sampleCount)
// and SACD (a TOC frame count). All three divide an untrusted header
// field by a declared rate.
//
// A stamped Duration is not re-derived by anything downstream: it goes
// into `tags_json`, onto the wire, and into the phone's track list. So
// a forged header buys a permanent wrong answer, not a transient one.
// The extractors are fuzzed precisely because these headers are
// attacker-shaped; these are the named cases.
//
// Each test asserts NIL rather than a clamped value. Duration is a
// pointer and its absence already means "unknown" to every consumer —
// iOS falls back to the file size, which is what every pre-v11 row
// does — whereas a clamped week would be a number somebody could act
// on.

// forgeFLACTotalSamples rewrites the 36-bit totalSamples field of an
// already-valid FLAC fixture, in place.
//
// STREAMINFO's packed 64 bits sit at a fixed offset: 4 bytes of "fLaC"
// magic, 4 of block header, then 10 bytes of block sizes and frame
// sizes. The low 36 bits of that word are totalSamples; the rest is
// sample rate, channels and bit depth, which must survive untouched or
// the test would be measuring a rejected FORMAT rather than a rejected
// duration.
func forgeFLACTotalSamples(t *testing.T, path string, totalSamples uint64) {
	t.Helper()
	const packedOffset = 4 + 4 + 10
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) < packedOffset+8 {
		t.Fatalf("fixture is %d bytes, too short to hold a STREAMINFO", len(raw))
	}
	packed := binary.BigEndian.Uint64(raw[packedOffset : packedOffset+8])
	packed = (packed &^ 0xFFFFFFFFF) | (totalSamples & 0xFFFFFFFFF)
	binary.BigEndian.PutUint64(raw[packedOffset:packedOffset+8], packed)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestExtractFLACRefusesAnImplausibleTotalSamples(t *testing.T) {
	path := filepath.Join(t.TempDir(), "forged.flac")
	writeMinimalFLAC(t, path, 8000, 16, nil)

	// Control FIRST, on the untouched fixture: the gate must not be
	// rejecting every duration. writeMinimalFLAC declares 5 seconds.
	control := &Track{}
	if err := extractFLACFormat(path, control); err != nil {
		t.Fatalf("extractFLACFormat on the unmodified fixture: %v", err)
	}
	if control.Duration == nil {
		t.Fatal("the unmodified fixture stamps no duration — the gate is refusing everything")
	}

	// 2^36-1 samples at this fixture's 8 kHz is 99 days — and 2,177
	// years at the 1 Hz the format's 20-bit rate field permits, which is
	// the worst a forged STREAMINFO can do. Either way it used to land
	// in tags_json and stay there.
	forgeFLACTotalSamples(t, path, 0xFFFFFFFFF)
	tr := &Track{}
	if err := extractFLACFormat(path, tr); err != nil {
		t.Fatalf("extractFLACFormat: %v", err)
	}
	if tr.Duration != nil {
		t.Errorf("a forged STREAMINFO stamped Duration = %.0f seconds (%.0f days)",
			*tr.Duration, *tr.Duration/86400)
	}
	// Typing still lands — the gate refuses the DURATION, not the file.
	if tr.SampleRate == nil || *tr.SampleRate != 8000 {
		t.Errorf("SampleRate = %v, want 8000 — refusing a duration must not refuse the format", tr.SampleRate)
	}
}

// forgeDSFSampleCount rewrites the fmt chunk's 64-bit sample count.
// Layout from writeMinimalDSF: a 28-byte DSD chunk, then `fmt `, whose
// sampleCount sits 36 bytes into the chunk.
func forgeDSFSampleCount(t *testing.T, path string, sampleCount uint64) {
	t.Helper()
	const offset = 28 + 36
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) < offset+8 {
		t.Fatalf("fixture is %d bytes, too short to hold a DSF fmt chunk", len(raw))
	}
	binary.LittleEndian.PutUint64(raw[offset:offset+8], sampleCount)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestExtractDSFRefusesAnImplausibleSampleCount(t *testing.T) {
	path := filepath.Join(t.TempDir(), "forged.dsf")
	writeMinimalDSF(t, path, 2822400, nil)

	control := &Track{}
	if err := extractDSF(path, control); err != nil {
		t.Fatalf("extractDSF on the unmodified fixture: %v", err)
	}
	if control.Duration == nil {
		t.Fatal("the unmodified fixture stamps no duration — the gate is refusing everything")
	}

	// A full uint64 count at DSD64 is 207,000 years. `sampleCount` is
	// read straight out of the chunk with no ceiling of its own.
	forgeDSFSampleCount(t, path, ^uint64(0))
	tr := &Track{}
	if err := extractDSF(path, tr); err != nil {
		t.Fatalf("extractDSF: %v", err)
	}
	if tr.Duration != nil {
		t.Errorf("a forged fmt chunk stamped Duration = %v seconds", *tr.Duration)
	}
	if tr.IsDSD == nil || !*tr.IsDSD {
		t.Error("refusing a duration must not refuse the DSD typing")
	}
}

// TestIFFUnknownPayloadSizeStampsNoDuration — the streaming writer's
// 0xFFFFFFFF "length unknown" sentinel.
//
// With a known physical size it is already refused by arithmetic: no
// sub-4-GiB file holds 4 GiB of payload. The case that mattered is the
// one where the bound is UNKNOWN — physicalFileSize returns 0 when Stat
// on the open handle fails — because iffPayloadFits fails OPEN there,
// and 0xFFFFFFFF over a CD-rate byte rate is 6.8 hours, comfortably
// inside the week-long ceiling. So the sentinel would have been stamped
// as a real duration on a file that never declared one.
//
// Driven through iffPayloadFits directly: an os.Stat that fails on a
// handle this process just opened is not a state a test can arrange.
func TestIFFUnknownPayloadSizeStampsNoDuration(t *testing.T) {
	span := iffPayloadSpan{seen: true, offset: 44, size: iffUnknownPayloadSize}
	if iffPayloadFits(span, 0) {
		t.Error("the unknown-length sentinel passed the fit check against an unknown bound")
	}
	if iffPayloadFits(span, 1<<33) {
		t.Error("the unknown-length sentinel passed the fit check against an 8 GiB file — " +
			"it is a sentinel, not a length, at any file size")
	}
	// Control: one byte under the sentinel is an ordinary declared size
	// and still fails OPEN against an unknown bound, which is the
	// pre-existing behaviour this must not change.
	ordinary := iffPayloadSpan{seen: true, offset: 44, size: iffUnknownPayloadSize - 1}
	if !iffPayloadFits(ordinary, 0) {
		t.Error("an ordinary declared size no longer fails open against an unknown bound")
	}
	// And a WAV whose data chunk carries the sentinel stamps nothing,
	// end to end.
	path := writeTempWAV(t, buildWAVWithID3(t, nil,
		buildWAVFmtChunk(1, 2, 44100, 16),
		buildWAVDataDeclaring(iffUnknownPayloadSize, 4096)))
	tr := &Track{}
	if err := extractWAVWithContext(path, tr, nil); err != nil {
		t.Fatalf("extractWAVWithContext: %v", err)
	}
	if tr.Duration != nil {
		t.Errorf("a data chunk declaring the unknown-length sentinel stamped Duration = %v", *tr.Duration)
	}
}

// TestAIFFSSNDSentinelSurvivesTheNarrowing is the AIFF half of the rule
// above, which v13 broke and no fixture could show.
//
// ssndSoundSpan subtracts the 8-byte SSND prefix from the declared size
// before anything else looks at it, so the sentinel reached
// iffPayloadFits as 0xFFFFFFF7 — an ordinary number to the arm that
// refuses 0xFFFFFFFF. The narrowing landed one release after the
// sentinel rule, in the function in front of it.
//
// The property is checked on the SPAN rather than end to end, for the
// reason the sentinel's own test gives: the two states it reaches are a
// Stat that fails on a handle this process just opened, and a file of
// 4 GiB or more. Below that, 0xFFFFFFF7 fails the physical-bounds check
// on arithmetic alone — which is exactly why a small fixture stayed
// green while the rule was off.
func TestAIFFSSNDSentinelSurvivesTheNarrowing(t *testing.T) {
	// A real file, so the ReadAt of the prefix succeeds and the span
	// would be returned if the sentinel were not refused. The SSND body
	// holds its 8-byte prefix plus 8 bytes of sound; only the DECLARED
	// size is the sentinel.
	openAt := func(t *testing.T, ssnd []byte) *os.File {
		t.Helper()
		path := writeTempAIFF(t, buildAIFFWithID3(t, nil,
			buildAIFFCOMMChunk(2, 26_460_000, 16, 44100), ssnd))
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = f.Close() })
		// Position the handle where the walker leaves it: just past the
		// SSND header, which is where ssndSoundSpan reads from.
		if _, err := f.Seek(int64(len(buildAIFFWithID3(t, nil,
			buildAIFFCOMMChunk(2, 26_460_000, 16, 44100)))+8), io.SeekStart); err != nil {
			t.Fatal(err)
		}
		return f
	}

	f := openAt(t, buildAIFFSSNDDeclaring(iffUnknownPayloadSize, 16))
	span := ssndSoundSpan(f, iffUnknownPayloadSize)
	if span.seen {
		t.Errorf("an SSND declaring the unknown-length sentinel produced a span of %d bytes at offset %d; "+
			"narrowed to 0x%X it is no longer the sentinel, and fails OPEN against an unknown bound",
			span.size, span.offset, span.size)
	}
	if iffPayloadFits(span, 0) {
		t.Error("the narrowed sentinel passed the fit check against an unknown bound")
	}
	if iffPayloadFits(span, 1<<33) {
		t.Error("the narrowed sentinel passed the fit check against an 8 GiB file")
	}

	// Positive control: one byte under the sentinel is an ordinary
	// declared size and must still produce a span, or "refuse the
	// sentinel" would be indistinguishable from "refuse everything".
	g := openAt(t, buildAIFFSSNDDeclaring(iffUnknownPayloadSize-1, 16))
	if ordinary := ssndSoundSpan(g, iffUnknownPayloadSize-1); !ordinary.seen {
		t.Error("an ordinary declared size no longer produces a span")
	}
}

// TestSACDTimecodeCannotExpressAnImplausibleDuration checks the claim
// the SACD gate's docblock rests on, rather than restating it.
//
// The other three sites here divide an unbounded header field by a
// declared rate, and a forged one stamps years. SACD's does not: its
// durations arrive as an M:S:F timecode with ONE byte of minutes,
// seconds < 60 and frames < 75, so the largest value the format can
// express is about four and a quarter hours. The gate on that site is
// structural — uniformity, and cover for the day the decoder widens —
// and writing a forged-TOC test there would have been a test that
// cannot fail, which is worse than none.
//
// So the property to pin is the BOUND. Sweep every byte triple the
// decoder accepts and require the worst case to clear the ceiling with
// room to spare.
func TestSACDTimecodeCannotExpressAnImplausibleDuration(t *testing.T) {
	worst := 0
	for m := 0; m < 256; m++ {
		f, ok := sacdTimecodeFrames(byte(m), 59, sacdFramesPerSecond-1)
		if !ok {
			t.Fatalf("sacdTimecodeFrames(%d, 59, %d) refused a triple it accepts by construction",
				m, sacdFramesPerSecond-1)
		}
		if f > worst {
			worst = f
		}
	}
	seconds := float64(worst) / float64(sacdFramesPerSecond)
	if !plausibleDuration(seconds) {
		t.Fatalf("the widest SACD timecode is %.0f s, which the gate REFUSES — "+
			"the structural note on the stamp site is wrong and a real image could "+
			"lose its duration", seconds)
	}
	// And it is not merely inside: it is an order of magnitude inside,
	// which is what makes "no SACD image can forge this" a claim rather
	// than a near miss.
	if seconds > dffMaxPlausibleDurationSeconds/10 {
		t.Errorf("the widest SACD timecode is %.0f s against a %.0f s ceiling — "+
			"too close to call the gate structural", seconds, dffMaxPlausibleDurationSeconds)
	}
}

// TestExtractAIFF_EmptySSNDStampsNoDuration is the end-to-end shape the
// unit test above protects: a well-formed COMM claiming ten minutes of
// frames beside an SSND holding no audio. The COMM arithmetic is fine
// and plausibleDuration accepts 600 s, so the payload check is the only
// thing standing between a corrupt file and a confident wrong answer in
// the phone's track list.
func TestExtractAIFF_EmptySSNDStampsNoDuration(t *testing.T) {
	// An SSND body is NOT all audio: it opens with an 8-byte prefix
	// (`offset`, `blockSize`). So "empty" has two spellings, and the
	// second is the one a size==0 check misses — a chunk of exactly 8
	// bytes holds the prefix and no sound at all, yet looked like 8
	// bytes of audio and stamped the COMM duration (CodeRabbit on #966).
	for _, tc := range []struct {
		name             string
		declared, actual int
	}{
		{"declares nothing", 0, 0},
		{"holds only its 8-byte prefix", 8, 8},
		{"cannot hold its own prefix", 4, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTempAIFF(t, buildAIFFWithID3(t, nil,
				buildAIFFCOMMChunk(2, 26_460_000, 16, 44100),
				buildAIFFSSNDDeclaring(uint32(tc.declared), tc.actual)))
			tr := &Track{}
			if err := extractAIFFWithContext(path, tr, nil); err != nil {
				t.Fatalf("extractAIFFWithContext: %v", err)
			}
			if tr.Duration != nil {
				t.Fatalf("Duration = %v, want nil — the SSND holds no audio, so ten minutes "+
					"of COMM frames is an inconsistent file, not a ten-minute track", *tr.Duration)
			}
		})
	}
}

// TestExtractAIFF_SSNDPrefixIsNotCountedAsAudio is the positive control
// for the narrowing above: a chunk that DOES hold sound must still time,
// or "reject the empty ones" would be indistinguishable from "reject
// everything".
//
// It also pins that the prefix is EXCLUDED rather than merely tested —
// the span is narrowed past `offset`, so the physical-bounds check
// measures the audio against the file rather than the audio plus eight
// bytes it is not.
func TestExtractAIFF_SSNDPrefixIsNotCountedAsAudio(t *testing.T) {
	// 44 100 frames at 44.1 kHz = 1 s, with a real 64-byte payload.
	path := writeTempAIFF(t, buildAIFFWithID3(t, nil,
		buildAIFFCOMMChunk(2, 44100, 16, 44100), buildAIFFSSNDChunk(64)))
	tr := &Track{}
	if err := extractAIFFWithContext(path, tr, nil); err != nil {
		t.Fatalf("extractAIFFWithContext: %v", err)
	}
	assertDuration(t, tr.Duration, 1)
}
