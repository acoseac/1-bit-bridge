package manifest

import (
	"bytes"
	"path/filepath"
	"testing"
)

// mp3FrameHeader synthesises a 4-byte MPEG audio frame header for the
// given 2-bit version (3=MPEG1, 2=MPEG2, 0=MPEG2.5), 2-bit layer
// (1=Layer3), 4-bit bitrate index, and 2-bit sample-rate index. The
// 4th byte (channel mode etc.) is irrelevant to the sample-rate read.
func mp3FrameHeader(version, layer, bitrateIdx, srIdx byte) []byte {
	return []byte{
		0xFF,
		0xE0 | (version&0x3)<<3 | (layer&0x3)<<1 | 0x1, // sync top 3 bits + version + layer + protection
		(bitrateIdx&0xF)<<4 | (srIdx&0x3)<<2,           // bitrate index + sample-rate index
		0x00,
	}
}

// padFrame appends quiet bytes after a frame header so the stream looks
// like a real (truncated) MP3 rather than a bare 4-byte header.
func padFrame(hdr []byte) []byte {
	return append(append([]byte{}, hdr...), bytes.Repeat([]byte{0x00}, 64)...)
}

func TestExtractMP3SampleRate_TableOfRates(t *testing.T) {
	const layer3 = 1
	const bitrate128 = 9 // any valid (non-free, non-bad) index
	cases := []struct {
		name    string
		version byte
		srIdx   byte
		want    float64
	}{
		{"MPEG1/44100", 3, 0, 44100},
		{"MPEG1/48000", 3, 1, 48000},
		{"MPEG1/32000", 3, 2, 32000},
		{"MPEG2/22050", 2, 0, 22050},
		{"MPEG2/24000", 2, 1, 24000},
		{"MPEG2/16000", 2, 2, 16000},
		{"MPEG2.5/11025", 0, 0, 11025},
		{"MPEG2.5/12000", 0, 1, 12000},
		{"MPEG2.5/8000", 0, 2, 8000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stream := padFrame(mp3FrameHeader(tc.version, layer3, bitrate128, tc.srIdx))
			got, err := extractMP3SampleRate(bytes.NewReader(stream))
			if err != nil {
				t.Fatalf("extractMP3SampleRate: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// TestExtractMP3SampleRate_SkipsID3v2Tag — a real MP3 prefixes the audio
// with an ID3v2 tag (which can embed a 0xFF byte run inside APIC data);
// the extractor must skip the tag and read the first true frame.
func TestExtractMP3SampleRate_SkipsID3v2Tag(t *testing.T) {
	id3 := buildID3v2_3(map[string]string{"title": "Skip past me", "artist": "ID3"})
	frame := padFrame(mp3FrameHeader(3, 1, 9, 1)) // MPEG1 / 48000
	stream := append(append([]byte{}, id3...), frame...)
	got, err := extractMP3SampleRate(bytes.NewReader(stream))
	if err != nil {
		t.Fatalf("extractMP3SampleRate: %v", err)
	}
	if got != 48000 {
		t.Errorf("got %v, want 48000 (frame after ID3 tag)", got)
	}
}

// TestExtractMP3SampleRate_NoFrameReturnsZero — data with no valid frame
// sync returns (0, nil) so the track is still indexed, just with a nil
// SampleRate.
func TestExtractMP3SampleRate_NoFrameReturnsZero(t *testing.T) {
	got, err := extractMP3SampleRate(bytes.NewReader(bytes.Repeat([]byte{0x00, 0x11, 0x22}, 100)))
	if err != nil {
		t.Fatalf("extractMP3SampleRate: %v", err)
	}
	if got != 0 {
		t.Errorf("got %v, want 0 for frame-less data", got)
	}
}

// TestExtractMP3_SampleRateViaExtract drives the full dispatcher: the
// writeMinimalMP3 fixture embeds a 0xFFFB9064 frame (MPEG1 Layer3,
// 44100 Hz) after its ID3v2 tag. Extract must surface SampleRate=44100,
// leave BitsPerSample nil (lossy), and keep Codec="MP3".
func TestExtractMP3_SampleRateViaExtract(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "t.mp3")
	writeMinimalMP3(t, p, map[string]string{"title": "Geometry", "artist": "MP3"})
	tr := &Track{}
	if err := Extract(p, tr); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if tr.Codec != "MP3" {
		t.Errorf("Codec = %q, want MP3", tr.Codec)
	}
	if tr.SampleRate == nil || *tr.SampleRate != 44100 {
		t.Errorf("SampleRate = %v, want 44100", tr.SampleRate)
	}
	if tr.BitsPerSample != nil {
		t.Errorf("BitsPerSample = %v, want nil for lossy MP3", *tr.BitsPerSample)
	}
	// Tags must still surface through the rewired single-open path.
	if tr.Title != "Geometry" {
		t.Errorf("Title = %q, want Geometry (tags lost in rewire?)", tr.Title)
	}
}

// TestMpegFrameSampleRate_Validation pins the header validation: a real
// frame resolves, every reserved/invalid field is rejected so a stray
// 0xFF 0xEx byte pair in audio data can't masquerade as a frame.
func TestMpegFrameSampleRate_Validation(t *testing.T) {
	if rate, ok := mpegFrameSampleRate(mp3FrameHeader(3, 1, 9, 0)); !ok || rate != 44100 {
		t.Errorf("valid MPEG1 frame: got (%d, %v), want (44100, true)", rate, ok)
	}
	bad := []struct {
		name string
		hdr  []byte
	}{
		{"reserved version", mp3FrameHeader(1, 1, 9, 0)},
		{"reserved layer", mp3FrameHeader(3, 0, 9, 0)},
		{"reserved sample-rate index", mp3FrameHeader(3, 1, 9, 3)},
		{"free-format bitrate", mp3FrameHeader(3, 1, 0, 0)},
		{"bad bitrate", mp3FrameHeader(3, 1, 15, 0)},
		{"no sync", []byte{0x00, 0x00, 0x00, 0x00}},
		{"partial sync", []byte{0xFF, 0x00, 0x00, 0x00}},
	}
	for _, tc := range bad {
		if _, ok := mpegFrameSampleRate(tc.hdr); ok {
			t.Errorf("%s: expected rejection, got ok", tc.name)
		}
	}
}

// TestUnsyncsafe pins the ID3v2 synchsafe-integer decode used to size
// the tag skip.
func TestUnsyncsafe(t *testing.T) {
	cases := []struct {
		in   []byte
		want uint32
	}{
		{[]byte{0x00, 0x00, 0x00, 0x00}, 0},
		{[]byte{0x00, 0x00, 0x00, 0x7F}, 127},
		{[]byte{0x00, 0x00, 0x01, 0x00}, 128},
		{[]byte{0x00, 0x00, 0x02, 0x01}, 257},
		{[]byte{0x7F, 0x7F, 0x7F, 0x7F}, 0x0FFFFFFF}, // max 28-bit value
	}
	for _, tc := range cases {
		if got := unsyncsafe(tc.in); got != tc.want {
			t.Errorf("unsyncsafe(% x) = %d, want %d", tc.in, got, tc.want)
		}
	}
}
