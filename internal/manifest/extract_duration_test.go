package manifest

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// Duration for the four PCM containers that never carried one (v11):
// MP4 via `mvhd`, MP3 via Xing / Info / VBRI or the CBR estimate, AIFF
// via COMM `numSampleFrames`, WAV via `data` bytes over the fmt byte
// rate — each gated by the shared plausibility ceiling, the two IFF
// containers additionally by the audio payload fitting the file.
//
// Every fixture here is a hand-built container; the numbers are chosen
// so the expected seconds are exact in float64 (or asserted within a
// tolerance the arithmetic's rounding cannot cross).

const durationTolerance = 1e-9

func assertDuration(t *testing.T, got *float64, want float64) {
	t.Helper()
	if got == nil {
		t.Fatalf("Duration = nil, want %v", want)
	}
	if math.Abs(*got-want) > durationTolerance {
		t.Fatalf("Duration = %v, want %v", *got, want)
	}
}

func assertNoDuration(t *testing.T, got *float64) {
	t.Helper()
	if got != nil {
		t.Fatalf("Duration = %v, want nil", *got)
	}
}

// ---------------------------------------------------------------- shared gate

func TestPlausibleDuration_TruthTable(t *testing.T) {
	cases := []struct {
		name string
		d    float64
		want bool
	}{
		{"zero", 0, false},
		{"negative", -1, false},
		{"NaN", math.NaN(), false},
		{"+Inf", math.Inf(1), false},
		{"-Inf", math.Inf(-1), false},
		{"tiny positive", 1e-9, true},
		{"typical track", 240.5, true},
		{"just under the ceiling", dffMaxPlausibleDurationSeconds - 1e-3, true},
		{"the ceiling itself", dffMaxPlausibleDurationSeconds, false},
		{"a forged multi-year value", 1e12, false},
	}
	for _, tc := range cases {
		if got := plausibleDuration(tc.d); got != tc.want {
			t.Errorf("%s: plausibleDuration(%v) = %v, want %v", tc.name, tc.d, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------- MP4

// buildMVHDPayload builds a full-size `mvhd` payload (100 bytes for
// version 0, 112 for version 1 — the real box sizes) carrying only the
// two fields the walker reads: timescale + duration at their
// version-dependent offsets. Everything else (creation / modification
// time, rate, volume, matrix, next_track_ID) stays zero.
func buildMVHDPayload(version byte, timescale uint32, duration uint64) []byte {
	switch version {
	case 0:
		p := make([]byte, 100)
		p[0] = 0
		binary.BigEndian.PutUint32(p[12:16], timescale)
		binary.BigEndian.PutUint32(p[16:20], uint32(duration))
		return p
	case 1:
		p := make([]byte, 112)
		p[0] = 1
		binary.BigEndian.PutUint32(p[20:24], timescale)
		binary.BigEndian.PutUint64(p[24:32], duration)
		return p
	default:
		p := make([]byte, 100)
		p[0] = version
		return p
	}
}

// buildMP4WithMVHD is an AAC-shaped MP4 whose moov opens with the given
// mvhd payload, followed by the usual trak chain — the layout every
// encoder writes.
func buildMP4WithMVHD(mvhdPayload []byte) []byte {
	mvhd := &bytes.Buffer{}
	writeAtom(mvhd, "mvhd", mvhdPayload)
	return buildMP4WithMoovChildren("mp4a", buildAACSampleEntryPayload(44100), [][]byte{mvhd.Bytes()})
}

func TestExtractMP4Duration_Version0(t *testing.T) {
	// 240.5 s in the QuickTime 600 Hz movie timescale.
	got, err := extractMP4Duration(bytes.NewReader(buildMP4WithMVHD(buildMVHDPayload(0, 600, 144300))))
	if err != nil {
		t.Fatalf("extractMP4Duration: %v", err)
	}
	assertDuration(t, &got, 240.5)
}

func TestExtractMP4Duration_Version1(t *testing.T) {
	// A version-1 box (64-bit duration) at a sample-rate timescale: exactly
	// four minutes of 44.1 kHz.
	got, err := extractMP4Duration(bytes.NewReader(buildMP4WithMVHD(buildMVHDPayload(1, 44100, 10_584_000))))
	if err != nil {
		t.Fatalf("extractMP4Duration: %v", err)
	}
	assertDuration(t, &got, 240)
}

func TestExtractMP4Duration_LargesizeMoovStillReachesMVHD(t *testing.T) {
	// The 64-bit `largesize` moov form from the codec walker's regression
	// set: the mvhd search must start at moov's 16-byte header, not 8.
	mvhd := &bytes.Buffer{}
	writeAtom(mvhd, "mvhd", buildMVHDPayload(0, 1000, 5_000))
	trakChain := buildMP4WithSampleEntryPayload("mp4a", buildAACSampleEntryPayload(44100))
	// Lift the moov payload out of the standard fixture and re-wrap it.
	moovPayload := extractMoovPayload(t, trakChain)
	moov := &bytes.Buffer{}
	writeAtom64(moov, "moov", append(mvhd.Bytes(), moovPayload...))
	out := &bytes.Buffer{}
	writeAtom(out, "ftyp", []byte("M4A mp42M4A "))
	out.Write(moov.Bytes())
	got, err := extractMP4Duration(bytes.NewReader(out.Bytes()))
	if err != nil {
		t.Fatalf("extractMP4Duration: %v", err)
	}
	assertDuration(t, &got, 5)
}

// extractMoovPayload returns the payload of the top-level moov in a
// fixture built by the standard builders (ftyp first, then moov).
func extractMoovPayload(t *testing.T, mp4 []byte) []byte {
	t.Helper()
	ftypSize := binary.BigEndian.Uint32(mp4[0:4])
	moov := mp4[ftypSize:]
	if string(moov[4:8]) != "moov" {
		t.Fatalf("fixture: expected moov after ftyp, got %q", moov[4:8])
	}
	return moov[8:]
}

func TestExtractMP4Duration_MissingMVHDReturnsZero(t *testing.T) {
	// The minimal codec-walker fixture carries no mvhd — honest zero, no
	// error, so the caller leaves Duration nil.
	got, err := extractMP4Duration(bytes.NewReader(buildMinimalMP4("alac")))
	if err != nil {
		t.Fatalf("extractMP4Duration: %v", err)
	}
	if got != 0 {
		t.Fatalf("got %v, want 0 for a moov without mvhd", got)
	}
}

func TestExtractMP4Duration_DegenerateBoxesReturnZero(t *testing.T) {
	cases := []struct {
		name string
		mvhd []byte
	}{
		{"v0 unknown-duration sentinel", buildMVHDPayload(0, 600, uint64(mvhdUnknownDuration32))},
		{"v1 unknown-duration sentinel", buildMVHDPayload(1, 600, mvhdUnknownDuration64)},
		{"zero timescale", buildMVHDPayload(0, 0, 144300)},
		{"zero duration (fragmented movie)", buildMVHDPayload(0, 600, 0)},
		{"undeclared version 2", buildMVHDPayload(2, 600, 144300)},
		{"truncated v0 payload", buildMVHDPayload(0, 600, 144300)[:8]},
		{"truncated v1 payload", buildMVHDPayload(1, 600, 144300)[:24]},
		{"empty payload", []byte{}},
	}
	for _, tc := range cases {
		got, err := extractMP4Duration(bytes.NewReader(buildMP4WithMVHD(tc.mvhd)))
		if err != nil {
			t.Fatalf("%s: extractMP4Duration: %v", tc.name, err)
		}
		if got != 0 {
			t.Errorf("%s: got %v, want 0", tc.name, got)
		}
	}
}

func TestExtractMP4Duration_ForgedLargesizeMVHDDoesNotPanic(t *testing.T) {
	// The fuzzer's first find on this walk (testdata/fuzz/FuzzExtractM4A/
	// bd63bb3c1e1eae42): an mvhd in the 64-bit `largesize` form declaring
	// ~2^63 bytes. Its payload length converted to a NEGATIVE int and the
	// bounded read sliced `head[:n]` on it. A forged size is an absent
	// box — zero, no error, no panic.
	mvhd := &bytes.Buffer{}
	binary.Write(mvhd, binary.BigEndian, uint32(1)) // largesize sentinel
	mvhd.WriteString("mvhd")
	binary.Write(mvhd, binary.BigEndian, uint64(0xC530303030303030))
	mvhd.Write(make([]byte, 8))
	fixture := buildMP4WithMoovChildren("mp4a", buildAACSampleEntryPayload(44100), [][]byte{mvhd.Bytes()})
	got, err := extractMP4Duration(bytes.NewReader(fixture))
	if err != nil {
		t.Fatalf("extractMP4Duration: %v", err)
	}
	if got != 0 {
		t.Fatalf("got %v, want 0 for a forged largesize mvhd", got)
	}
	// Size 0 ("to end of enclosing box") on a box declaring more than the
	// walker reads is the other way a payload length exceeds 32 bytes.
	zeroSize := &bytes.Buffer{}
	binary.Write(zeroSize, binary.BigEndian, uint32(0))
	zeroSize.WriteString("mvhd")
	zeroSize.Write(buildMVHDPayload(0, 600, 144300))
	fixture = buildMP4WithMoovChildren("mp4a", buildAACSampleEntryPayload(44100), [][]byte{zeroSize.Bytes()})
	got, err = extractMP4Duration(bytes.NewReader(fixture))
	if err != nil {
		t.Fatalf("extractMP4Duration (size 0): %v", err)
	}
	assertDuration(t, &got, 240.5)
}

func TestParseMVHDHead_TruthTable(t *testing.T) {
	v0 := buildMVHDPayload(0, 600, 144300)
	v1 := buildMVHDPayload(1, 44100, 10_584_000)
	cases := []struct {
		name         string
		head         []byte
		wantTS       uint32
		wantDuration uint64
		wantOK       bool
	}{
		{"v0 full payload", v0, 600, 144300, true},
		{"v0 exactly 20 bytes", v0[:20], 600, 144300, true},
		{"v0 19 bytes", v0[:19], 0, 0, false},
		{"v1 full payload", v1, 44100, 10_584_000, true},
		{"v1 exactly 32 bytes", v1[:32], 44100, 10_584_000, true},
		{"v1 31 bytes", v1[:31], 0, 0, false},
		{"empty", nil, 0, 0, false},
		{"version 2", buildMVHDPayload(2, 600, 144300), 0, 0, false},
		{"v0 zero timescale", buildMVHDPayload(0, 0, 144300), 0, 0, false},
		{"v0 zero duration", buildMVHDPayload(0, 600, 0), 0, 0, false},
		{"v0 unknown sentinel", buildMVHDPayload(0, 600, uint64(mvhdUnknownDuration32)), 0, 0, false},
		{"v1 unknown sentinel", buildMVHDPayload(1, 600, mvhdUnknownDuration64), 0, 0, false},
	}
	for _, tc := range cases {
		ts, d, ok := parseMVHDHead(tc.head)
		if ok != tc.wantOK || ts != tc.wantTS || d != tc.wantDuration {
			t.Errorf("%s: got (%d, %d, %v), want (%d, %d, %v)", tc.name, ts, d, ok, tc.wantTS, tc.wantDuration, tc.wantOK)
		}
	}
}

func TestExtractMP4Duration_SuppressesStructuralNotFound(t *testing.T) {
	// ftyp only, no moov: the same honest-suppression contract the codec /
	// bits / rate walkers share.
	out := &bytes.Buffer{}
	writeAtom(out, "ftyp", []byte("M4A mp42M4A "))
	got, err := extractMP4Duration(bytes.NewReader(out.Bytes()))
	if err != nil {
		t.Fatalf("expected suppression, got error %v", err)
	}
	if got != 0 {
		t.Fatalf("got %v, want 0", got)
	}
}

func TestExtractMP4Duration_PropagatesIOFailure(t *testing.T) {
	sentinel := errors.New("disk on fire")
	if _, err := extractMP4Duration(failingReadSeeker{err: sentinel}); !errors.Is(err, sentinel) {
		t.Fatalf("expected the I/O error to propagate, got %v", err)
	}
}

func TestExtractMP4Duration_DoesNotDisturbTheOtherWalks(t *testing.T) {
	// An mvhd ahead of the trak must be skipped by the stsd descent, so
	// codec + sample rate still resolve on the same fixture.
	fixture := buildMP4WithMVHD(buildMVHDPayload(0, 600, 144300))
	codec, err := extractMP4Codec(bytes.NewReader(fixture))
	if err != nil || codec != "AAC" {
		t.Fatalf("codec = %q, %v; want AAC", codec, err)
	}
	rate, err := extractMP4SampleRate(bytes.NewReader(fixture))
	if err != nil || rate != 44100 {
		t.Fatalf("rate = %v, %v; want 44100", rate, err)
	}
}

// ---------------------------------------------------------------------- MP3

// mp3FrameHeaderWithMode is mp3FrameHeader with the channel-mode bits
// (byte 3, top two bits) set — mono changes the Layer III side-info size
// and therefore where a Xing header sits.
func mp3FrameHeaderWithMode(version, layer, bitrateIdx, srIdx, channelMode byte) []byte {
	hdr := mp3FrameHeader(version, layer, bitrateIdx, srIdx)
	hdr[3] = (channelMode & 0x3) << 6
	return hdr
}

// buildMP3XingFrame lays a Xing (or Info) header into a frame of
// `frameLen` bytes: header, zeroed Layer III side info, the tag, the
// flags word, and the frame count when `withFrames`.
func buildMP3XingFrame(hdr []byte, sideInfo int, tagName string, withFrames bool, frames uint32, frameLen int) []byte {
	frame := make([]byte, frameLen)
	copy(frame, hdr)
	at := 4 + sideInfo
	copy(frame[at:], tagName)
	var flags uint32
	if withFrames {
		flags = 0x1
	}
	binary.BigEndian.PutUint32(frame[at+4:at+8], flags)
	if withFrames {
		binary.BigEndian.PutUint32(frame[at+8:at+12], frames)
	}
	return frame
}

// buildMP3VBRIFrame lays a Fraunhofer VBRI header at its fixed offset
// (32 bytes after the 4-byte frame header).
func buildMP3VBRIFrame(hdr []byte, frames uint32, frameLen int) []byte {
	frame := make([]byte, frameLen)
	copy(frame, hdr)
	at := 4 + 32
	copy(frame[at:], "VBRI")
	binary.BigEndian.PutUint16(frame[at+4:at+6], 1)        // version
	binary.BigEndian.PutUint16(frame[at+6:at+8], 0)        // delay
	binary.BigEndian.PutUint16(frame[at+8:at+10], 50)      // quality
	binary.BigEndian.PutUint32(frame[at+10:at+14], 0)      // bytes (unused)
	binary.BigEndian.PutUint32(frame[at+14:at+18], frames) // frames
	return frame
}

// MPEG 1 Layer III, 128 kbit/s, 44.1 kHz, no padding: 144 × 128000 /
// 44100 = 417 bytes per frame, 1152 samples per frame.
const (
	mp3TestFrameLen             = 417
	mp3TestBitrateKbps          = 128
	mp3TestSamplesPerFrame      = 1152
	mp3TestSampleRate           = 44100
	mp3TestBitrateIdx      byte = 9 // 128 kbit/s in the MPEG 1 Layer III table
)

func mp3TestHeader(channelMode byte) []byte {
	return mp3FrameHeaderWithMode(3, 1, mp3TestBitrateIdx, 0, channelMode)
}

// buildCBRStream is `n` identical CBR frames (header + zero bytes),
// optionally after an ID3v2 tag and optionally before an ID3v1 tail.
func buildCBRStream(id3v2 []byte, n int, id3v1Tail bool) []byte {
	var out []byte
	out = append(out, id3v2...)
	frame := make([]byte, mp3TestFrameLen)
	copy(frame, mp3TestHeader(0))
	for i := 0; i < n; i++ {
		out = append(out, frame...)
	}
	if id3v1Tail {
		tail := make([]byte, mp3ID3v1TagSize)
		copy(tail, "TAG")
		out = append(out, tail...)
	}
	return out
}

func TestExtractMP3Format_XingFramesWins(t *testing.T) {
	const frames = 9188 // ≈ 240 s at 1152 / 44100
	first := buildMP3XingFrame(mp3TestHeader(0), 32, "Xing", true, frames, mp3TestFrameLen)
	// Follow it with a CBR tail whose byte-based estimate would be far
	// shorter — the declared count must win.
	stream := append(first, buildCBRStream(nil, 10, false)...)
	info, err := extractMP3Format(bytes.NewReader(stream))
	if err != nil {
		t.Fatalf("extractMP3Format: %v", err)
	}
	assertDuration(t, &info.sampleRate, mp3TestSampleRate)
	assertDuration(t, &info.duration, float64(frames)*mp3TestSamplesPerFrame/mp3TestSampleRate)
}

func TestExtractMP3Format_InfoTagIsReadLikeXing(t *testing.T) {
	// LAME writes "Info" for a CBR encode — same layout, same frame count.
	const frames = 100
	first := buildMP3XingFrame(mp3TestHeader(0), 32, "Info", true, frames, mp3TestFrameLen)
	info, err := extractMP3Format(bytes.NewReader(first))
	if err != nil {
		t.Fatalf("extractMP3Format: %v", err)
	}
	assertDuration(t, &info.duration, float64(frames)*mp3TestSamplesPerFrame/mp3TestSampleRate)
}

func TestExtractMP3Format_MonoXingOffset(t *testing.T) {
	// Mono MPEG 1 Layer III carries 17 bytes of side info, so the Xing
	// tag sits at offset 21, not 36.
	const frames = 2000
	first := buildMP3XingFrame(mp3TestHeader(3), 17, "Xing", true, frames, mp3TestFrameLen)
	info, err := extractMP3Format(bytes.NewReader(first))
	if err != nil {
		t.Fatalf("extractMP3Format: %v", err)
	}
	assertDuration(t, &info.duration, float64(frames)*mp3TestSamplesPerFrame/mp3TestSampleRate)
}

func TestExtractMP3Format_MPEG2LayerIIIHas576SamplesPerFrame(t *testing.T) {
	// MPEG 2 (22.05 kHz), stereo: 17 bytes of side info, 576 samples per
	// frame. Bitrate index 8 = 64 kbit/s in the MPEG 2 table.
	const frames = 4000
	hdr := mp3FrameHeaderWithMode(2, 1, 8, 0, 0)
	first := buildMP3XingFrame(hdr, 17, "Xing", true, frames, 417)
	info, err := extractMP3Format(bytes.NewReader(first))
	if err != nil {
		t.Fatalf("extractMP3Format: %v", err)
	}
	assertDuration(t, &info.sampleRate, 22050)
	assertDuration(t, &info.duration, float64(frames)*576/22050)
}

func TestExtractMP3Format_VBRIFrames(t *testing.T) {
	const frames = 7000
	first := buildMP3VBRIFrame(mp3TestHeader(0), frames, mp3TestFrameLen)
	info, err := extractMP3Format(bytes.NewReader(first))
	if err != nil {
		t.Fatalf("extractMP3Format: %v", err)
	}
	assertDuration(t, &info.duration, float64(frames)*mp3TestSamplesPerFrame/mp3TestSampleRate)
}

func TestExtractMP3Format_CBREstimateExcludesID3v1Tail(t *testing.T) {
	const n = 100
	stream := buildCBRStream(nil, n, true)
	info, err := extractMP3Format(bytes.NewReader(stream))
	if err != nil {
		t.Fatalf("extractMP3Format: %v", err)
	}
	// 100 × 417 bytes × 8 bits / 128 000 bit/s — the 128-byte "TAG" tail
	// is not audio. (Counting it would read 2.61425 s.)
	assertDuration(t, &info.duration, float64(n*mp3TestFrameLen)*8/(mp3TestBitrateKbps*1000))
}

func TestExtractMP3Format_CBREstimateExcludesLeadingID3v2(t *testing.T) {
	const n = 50
	id3 := buildID3v2_3(map[string]string{"title": "Leading tag", "artist": "ID3"})
	stream := buildCBRStream(id3, n, false)
	info, err := extractMP3Format(bytes.NewReader(stream))
	if err != nil {
		t.Fatalf("extractMP3Format: %v", err)
	}
	assertDuration(t, &info.duration, float64(n*mp3TestFrameLen)*8/(mp3TestBitrateKbps*1000))
}

func TestExtractMP3Format_XingWithoutFramesFallsToEstimate(t *testing.T) {
	// A Xing header whose flags omit the frame count buys nothing; the
	// estimate covers the whole audio span including that frame.
	first := buildMP3XingFrame(mp3TestHeader(0), 32, "Xing", false, 0, mp3TestFrameLen)
	stream := append(first, buildCBRStream(nil, 9, false)...)
	info, err := extractMP3Format(bytes.NewReader(stream))
	if err != nil {
		t.Fatalf("extractMP3Format: %v", err)
	}
	assertDuration(t, &info.duration, float64(10*mp3TestFrameLen)*8/(mp3TestBitrateKbps*1000))
}

func TestExtractMP3Format_LayerIIIgnoresXingAndEstimates(t *testing.T) {
	// Layer II frames carry no Xing header; a "Xing" string in the audio
	// bytes is just audio. MPEG 1 Layer II, index 10 = 192 kbit/s, 44.1 kHz:
	// 144 × 192000 / 44100 = 626 bytes per frame.
	hdr := mp3FrameHeaderWithMode(3, 2, 10, 0, 0)
	frame := buildMP3XingFrame(hdr, 32, "Xing", true, 999_999, 626)
	info, err := extractMP3Format(bytes.NewReader(frame))
	if err != nil {
		t.Fatalf("extractMP3Format: %v", err)
	}
	assertDuration(t, &info.duration, float64(626)*8/(192*1000))
}

func TestExtractMP3Format_NoFrameYieldsNothing(t *testing.T) {
	info, err := extractMP3Format(bytes.NewReader(bytes.Repeat([]byte{0x00, 0x11, 0x22}, 100)))
	if err != nil {
		t.Fatalf("extractMP3Format: %v", err)
	}
	if info.sampleRate != 0 || info.duration != 0 {
		t.Fatalf("got %+v, want zero for frame-less data", info)
	}
}

func TestExtractMP3Format_HeaderOnlyStreamHasNoAudioBytes(t *testing.T) {
	// Exactly one 4-byte header and nothing after it: the estimate has
	// the header's own 4 bytes to work with. Whatever it computes must be
	// positive and tiny — and the rate still resolves.
	info, err := extractMP3Format(bytes.NewReader(mp3TestHeader(0)))
	if err != nil {
		t.Fatalf("extractMP3Format: %v", err)
	}
	if info.sampleRate != mp3TestSampleRate {
		t.Fatalf("sampleRate = %v, want %v", info.sampleRate, mp3TestSampleRate)
	}
	if info.duration <= 0 || info.duration > 0.001 {
		t.Fatalf("duration = %v, want a positive sub-millisecond estimate", info.duration)
	}
}

func TestParseMPEGFrameHeader_Geometry(t *testing.T) {
	cases := []struct {
		name            string
		hdr             []byte
		bitrate         int
		samplesPerFrame int
		sideInfo        int
	}{
		{"MPEG1 L3 128k stereo", mp3FrameHeaderWithMode(3, 1, 9, 0, 0), 128, 1152, 32},
		{"MPEG1 L3 128k mono", mp3FrameHeaderWithMode(3, 1, 9, 0, 3), 128, 1152, 17},
		{"MPEG1 L2 160k", mp3FrameHeaderWithMode(3, 2, 9, 0, 0), 160, 1152, 0},
		{"MPEG1 L2 192k", mp3FrameHeaderWithMode(3, 2, 10, 0, 0), 192, 1152, 0},
		{"MPEG1 L1 288k", mp3FrameHeaderWithMode(3, 3, 9, 0, 0), 288, 384, 0},
		{"MPEG2 L3 64k stereo", mp3FrameHeaderWithMode(2, 1, 8, 0, 0), 64, 576, 17},
		{"MPEG2 L3 64k mono", mp3FrameHeaderWithMode(2, 1, 8, 0, 3), 64, 576, 9},
		{"MPEG2.5 L3 8k", mp3FrameHeaderWithMode(0, 1, 1, 0, 0), 8, 576, 17},
		{"MPEG2 L1 144k", mp3FrameHeaderWithMode(2, 3, 9, 0, 0), 144, 384, 0},
	}
	for _, tc := range cases {
		frame, ok := parseMPEGFrameHeader(tc.hdr)
		if !ok {
			t.Errorf("%s: rejected", tc.name)
			continue
		}
		if frame.bitrateKbps != tc.bitrate || frame.samplesPerFrame != tc.samplesPerFrame || frame.sideInfoSize() != tc.sideInfo {
			t.Errorf("%s: got bitrate %d / spf %d / sideInfo %d, want %d / %d / %d",
				tc.name, frame.bitrateKbps, frame.samplesPerFrame, frame.sideInfoSize(),
				tc.bitrate, tc.samplesPerFrame, tc.sideInfo)
		}
	}
	if _, ok := parseMPEGFrameHeader([]byte{0xFF, 0xFB, 0x90}); ok {
		t.Errorf("a 3-byte header must be rejected (the channel mode lives in byte 4)")
	}
}

func TestExtractMP3_DurationViaExtract(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "t.mp3")
	id3 := buildID3v2_3(map[string]string{"title": "Timed", "artist": "MP3"})
	const frames = 3000
	first := buildMP3XingFrame(mp3TestHeader(0), 32, "Xing", true, frames, mp3TestFrameLen)
	if err := os.WriteFile(p, append(id3, first...), 0o600); err != nil {
		t.Fatal(err)
	}
	tr := &Track{}
	if err := Extract(p, tr); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if tr.Title != "Timed" {
		t.Errorf("Title = %q, want Timed (tags lost?)", tr.Title)
	}
	assertDuration(t, tr.Duration, float64(frames)*mp3TestSamplesPerFrame/mp3TestSampleRate)
}

// --------------------------------------------------------------------- AIFF

// buildAIFFSSNDChunk is an SSND chunk: 8-byte header (offset, blockSize)
// + `payloadBytes` of audio, declared honestly.
func buildAIFFSSNDChunk(payloadBytes int) []byte {
	body := make([]byte, 8+payloadBytes)
	return wrapChunkBE("SSND", body)
}

// buildAIFFSSNDDeclaring is an SSND chunk whose header DECLARES
// `declared` bytes but whose body holds only `actual` — the truncated
// file shape (the declared size is written straight into the header).
func buildAIFFSSNDDeclaring(declared uint32, actual int) []byte {
	out := []byte("SSND")
	var sz [4]byte
	binary.BigEndian.PutUint32(sz[:], declared)
	out = append(out, sz[:]...)
	out = append(out, make([]byte, actual)...)
	return out
}

func TestExtractAIFF_DurationFromCOMMFrames(t *testing.T) {
	// 44 100 frames at 44.1 kHz = exactly 1 s; SSND fits.
	path := writeTempAIFF(t, buildAIFFWithID3(t, nil,
		buildAIFFCOMMChunk(2, 44100, 16, 44100), buildAIFFSSNDChunk(64)))
	tr := &Track{}
	if err := extractAIFFWithContext(path, tr, nil); err != nil {
		t.Fatalf("extractAIFFWithContext: %v", err)
	}
	assertDuration(t, tr.Duration, 1)
}

func TestExtractAIFF_DurationIsPerChannelFrames(t *testing.T) {
	// Six channels, 96 kHz, 240 000 frames = 2.5 s regardless of channels.
	path := writeTempAIFF(t, buildAIFFWithID3(t, nil,
		buildAIFFCOMMChunk(6, 240000, 24, 96000), buildAIFFSSNDChunk(64)))
	tr := &Track{}
	if err := extractAIFFWithContext(path, tr, nil); err != nil {
		t.Fatalf("extractAIFFWithContext: %v", err)
	}
	assertDuration(t, tr.Duration, 2.5)
}

func TestExtractAIFF_COMMAfterSSNDStillLands(t *testing.T) {
	path := writeTempAIFF(t, buildAIFFWithID3(t, nil,
		buildAIFFSSNDChunk(64), buildAIFFCOMMChunk(2, 22050, 16, 44100)))
	tr := &Track{}
	if err := extractAIFFWithContext(path, tr, nil); err != nil {
		t.Fatalf("extractAIFFWithContext: %v", err)
	}
	assertDuration(t, tr.Duration, 0.5)
}

func TestExtractAIFF_TruncatedSSNDReportsNoDuration(t *testing.T) {
	// SSND declares a megabyte the file does not hold: the frame count
	// is right there in COMM, and it is deliberately NOT stamped.
	path := writeTempAIFF(t, buildAIFFWithID3(t, nil,
		buildAIFFCOMMChunk(2, 44100, 16, 44100), buildAIFFSSNDDeclaring(1<<20, 64)))
	tr := &Track{}
	if err := extractAIFFWithContext(path, tr, nil); err != nil {
		t.Fatalf("extractAIFFWithContext: %v", err)
	}
	if tr.SampleRate == nil || *tr.SampleRate != 44100 {
		t.Fatalf("SampleRate = %v, want 44100 (typing must still land)", tr.SampleRate)
	}
	assertNoDuration(t, tr.Duration)
}

func TestExtractAIFF_NoSSNDReportsNoDuration(t *testing.T) {
	path := writeTempAIFF(t, buildAIFFWithID3(t, nil, buildAIFFCOMMChunk(2, 44100, 16, 44100)))
	tr := &Track{}
	if err := extractAIFFWithContext(path, tr, nil); err != nil {
		t.Fatalf("extractAIFFWithContext: %v", err)
	}
	assertNoDuration(t, tr.Duration)
}

func TestExtractAIFF_ZeroFramesReportsNoDuration(t *testing.T) {
	path := writeTempAIFF(t, buildAIFFWithID3(t, nil,
		buildAIFFCOMMChunk(2, 0, 16, 44100), buildAIFFSSNDChunk(0)))
	tr := &Track{}
	if err := extractAIFFWithContext(path, tr, nil); err != nil {
		t.Fatalf("extractAIFFWithContext: %v", err)
	}
	assertNoDuration(t, tr.Duration)
}

func TestExtractAIFF_DurationCoexistsWithID3(t *testing.T) {
	id3 := buildID3v2_3(map[string]string{"title": "Timed AIFF"})
	path := writeTempAIFF(t, buildAIFFWithID3(t, id3,
		buildAIFFCOMMChunk(2, 88200, 24, 88200), buildAIFFSSNDChunk(32)))
	tr := &Track{}
	if err := extractAIFFWithContext(path, tr, nil); err != nil {
		t.Fatalf("extractAIFFWithContext: %v", err)
	}
	if tr.Title != "Timed AIFF" {
		t.Errorf("Title = %q, want Timed AIFF", tr.Title)
	}
	assertDuration(t, tr.Duration, 1)
}

// ---------------------------------------------------------------------- WAV

// buildWAVFmtChunkRaw writes every WAVEFORMAT field explicitly — the
// sibling buildWAVFmtChunk derives the byte rate and block align, and
// the duration tests need to control them.
func buildWAVFmtChunkRaw(formatTag, channels uint16, sampleRate, avgBytesPerSec uint32, blockAlign, bits uint16) []byte {
	payload := make([]byte, 16)
	binary.LittleEndian.PutUint16(payload[0:2], formatTag)
	binary.LittleEndian.PutUint16(payload[2:4], channels)
	binary.LittleEndian.PutUint32(payload[4:8], sampleRate)
	binary.LittleEndian.PutUint32(payload[8:12], avgBytesPerSec)
	binary.LittleEndian.PutUint16(payload[12:14], blockAlign)
	binary.LittleEndian.PutUint16(payload[14:16], bits)
	return wrapChunkLE("fmt ", payload)
}

// buildWAVDataChunk is a `data` chunk holding `payloadBytes` of zeros.
func buildWAVDataChunk(payloadBytes int) []byte {
	return wrapChunkLE("data", make([]byte, payloadBytes))
}

// buildWAVDataDeclaring is a `data` chunk whose header DECLARES
// `declared` bytes over a body of only `actual` — the truncated or
// never-fixed-up streaming-writer shape.
func buildWAVDataDeclaring(declared uint32, actual int) []byte {
	out := []byte("data")
	var sz [4]byte
	binary.LittleEndian.PutUint32(sz[:], declared)
	out = append(out, sz[:]...)
	out = append(out, make([]byte, actual)...)
	return out
}

func TestExtractWAV_DurationFromDataOverByteRate(t *testing.T) {
	// PCM stereo 16-bit 44.1 kHz: 176 400 bytes/s; 17 640 bytes = 0.1 s.
	path := writeTempWAV(t, buildWAVWithID3(t, nil, buildWAVFmtChunk(1, 2, 44100, 16), buildWAVDataChunk(17640)))
	tr := &Track{}
	if err := extractWAVWithContext(path, tr, nil); err != nil {
		t.Fatalf("extractWAVWithContext: %v", err)
	}
	assertDuration(t, tr.Duration, 0.1)
}

func TestExtractWAV_CompressedFormatUsesAvgBytesPerSec(t *testing.T) {
	// MP3-in-WAV (format tag 0x55): nAvgBytesPerSec is 16 000 (128 kbit/s)
	// while nBlockAlign is a 1-byte compressed unit — rate × blockAlign
	// would read 44 100 bytes/s and under-report by ~2.8×.
	path := writeTempWAV(t, buildWAVWithID3(t, nil,
		buildWAVFmtChunkRaw(0x55, 2, 44100, 16000, 1, 0), buildWAVDataChunk(32000)))
	tr := &Track{}
	if err := extractWAVWithContext(path, tr, nil); err != nil {
		t.Fatalf("extractWAVWithContext: %v", err)
	}
	assertDuration(t, tr.Duration, 2)
}

func TestExtractWAV_PCMWithZeroByteRateFallsBackToRateTimesBlockAlign(t *testing.T) {
	// A writer that left nAvgBytesPerSec at 0 on a plain PCM file:
	// 48 000 × 4 = 192 000 bytes/s; 96 000 bytes = 0.5 s.
	path := writeTempWAV(t, buildWAVWithID3(t, nil,
		buildWAVFmtChunkRaw(1, 2, 48000, 0, 4, 16), buildWAVDataChunk(96000)))
	tr := &Track{}
	if err := extractWAVWithContext(path, tr, nil); err != nil {
		t.Fatalf("extractWAVWithContext: %v", err)
	}
	assertDuration(t, tr.Duration, 0.5)
}

func TestExtractWAV_CompressedWithZeroByteRateReportsNoDuration(t *testing.T) {
	// The PCM fallback must not reach a compressed format, whose block
	// align is not a per-sample-frame byte count.
	path := writeTempWAV(t, buildWAVWithID3(t, nil,
		buildWAVFmtChunkRaw(0x55, 2, 44100, 0, 1, 0), buildWAVDataChunk(32000)))
	tr := &Track{}
	if err := extractWAVWithContext(path, tr, nil); err != nil {
		t.Fatalf("extractWAVWithContext: %v", err)
	}
	assertNoDuration(t, tr.Duration)
}

func TestExtractWAV_FmtAfterDataStillLands(t *testing.T) {
	path := writeTempWAV(t, buildWAVWithID3(t, nil, buildWAVDataChunk(17640), buildWAVFmtChunk(1, 2, 44100, 16)))
	tr := &Track{}
	if err := extractWAVWithContext(path, tr, nil); err != nil {
		t.Fatalf("extractWAVWithContext: %v", err)
	}
	assertDuration(t, tr.Duration, 0.1)
}

func TestExtractWAV_TruncatedDataReportsNoDuration(t *testing.T) {
	cases := []struct {
		name string
		data []byte
	}{
		{"declares a megabyte over 100 bytes", buildWAVDataDeclaring(1<<20, 100)},
		{"streaming writer's 0xFFFFFFFF", buildWAVDataDeclaring(0xFFFFFFFF, 100)},
		{"streaming writer's 0", buildWAVDataDeclaring(0, 100)},
	}
	for _, tc := range cases {
		path := writeTempWAV(t, buildWAVWithID3(t, nil, buildWAVFmtChunk(1, 2, 44100, 16), tc.data))
		tr := &Track{}
		if err := extractWAVWithContext(path, tr, nil); err != nil {
			t.Fatalf("%s: extractWAVWithContext: %v", tc.name, err)
		}
		if tr.SampleRate == nil || *tr.SampleRate != 44100 {
			t.Errorf("%s: SampleRate = %v, want 44100 (typing must still land)", tc.name, tr.SampleRate)
		}
		if tr.Duration != nil {
			t.Errorf("%s: Duration = %v, want nil", tc.name, *tr.Duration)
		}
	}
}

func TestExtractWAV_NoFmtReportsNoDuration(t *testing.T) {
	path := writeTempWAV(t, buildWAVWithID3(t, nil, buildWAVDataChunk(17640)))
	tr := &Track{}
	if err := extractWAVWithContext(path, tr, nil); err != nil {
		t.Fatalf("extractWAVWithContext: %v", err)
	}
	assertNoDuration(t, tr.Duration)
}

func TestExtractWAV_DurationCoexistsWithID3AndINFO(t *testing.T) {
	id3 := buildID3v2_3(map[string]string{"title": "Timed WAV"})
	path := writeTempWAV(t, buildWAVWithID3(t, id3, buildWAVFmtChunk(1, 2, 44100, 16), buildWAVDataChunk(176400)))
	tr := &Track{}
	if err := extractWAVWithContext(path, tr, nil); err != nil {
		t.Fatalf("extractWAVWithContext: %v", err)
	}
	if tr.Title != "Timed WAV" {
		t.Errorf("Title = %q, want Timed WAV", tr.Title)
	}
	assertDuration(t, tr.Duration, 1)
}

func TestIFFPayloadFits_TruthTable(t *testing.T) {
	cases := []struct {
		name     string
		span     iffPayloadSpan
		physical uint64
		want     bool
	}{
		{"unseen payload fails closed", iffPayloadSpan{}, 1000, false},
		{"unknown physical size fails open", iffPayloadSpan{seen: true, offset: 100, size: 1 << 40}, 0, true},
		{"fits exactly", iffPayloadSpan{seen: true, offset: 100, size: 900}, 1000, true},
		{"one byte over", iffPayloadSpan{seen: true, offset: 100, size: 901}, 1000, false},
		{"offset past the end", iffPayloadSpan{seen: true, offset: 1001, size: 0}, 1000, false},
		{"size that would wrap a naive sum", iffPayloadSpan{seen: true, offset: 100, size: math.MaxUint64}, 1000, false},
	}
	for _, tc := range cases {
		if got := iffPayloadFits(tc.span, tc.physical); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

// ------------------------------------------------------------- capstone

// TestDurationReachesEveryPCMContainer is the end-to-end capstone: each
// of WAV / AIFF / ALAC / MP3 — the formats the field report showed
// falling back to a file size — must surface a Duration after Extract,
// the sibling of TestPCMGeometryReachesFormatDistribution's rate pin.
func TestDurationReachesEveryPCMContainer(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, b []byte) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	wavPath := write("track.wav", buildWAVWithID3(t, nil, buildWAVFmtChunk(1, 2, 96000, 24), buildWAVDataChunk(576000)))
	aiffPath := write("track.aiff", buildAIFFWithID3(t, nil, buildAIFFCOMMChunk(2, 44100*5, 16, 44100), buildAIFFSSNDChunk(64)))
	alacPath := write("track.m4a", buildMP4WithMoovChildren("alac", buildALACSampleEntryPayloadRate(24, 192000),
		[][]byte{func() []byte {
			b := &bytes.Buffer{}
			writeAtom(b, "mvhd", buildMVHDPayload(0, 600, 144300))
			return b.Bytes()
		}()}))
	mp3Path := write("track.mp3", buildMP3XingFrame(mp3TestHeader(0), 32, "Xing", true, 9188, mp3TestFrameLen))

	cases := []struct {
		abs   string
		codec string
		want  float64
	}{
		{wavPath, "WAV", 1},
		{aiffPath, "AIFF", 5},
		{alacPath, "ALAC", 240.5},
		{mp3Path, "MP3", 9188.0 * mp3TestSamplesPerFrame / mp3TestSampleRate},
	}
	for _, tc := range cases {
		tr := &Track{}
		if err := Extract(tc.abs, tr); err != nil {
			t.Fatalf("Extract %s: %v", tc.abs, err)
		}
		if tr.Codec != tc.codec {
			t.Errorf("%s: Codec = %q, want %q", tc.abs, tr.Codec, tc.codec)
		}
		assertDuration(t, tr.Duration, tc.want)
	}
}
