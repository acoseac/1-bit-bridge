package manifest

import (
	"encoding/binary"
	"math"
	"testing"
)

// TestSafeSeekSkip_TruthTable pins the uint64 → int64 overflow guard
// used by the DFF chunk walker. A real DSD audio chunk (single-digit
// GB at DSD512+) is well below int64 max, but a malformed header
// claiming a giant size would silently convert to a negative int64
// and cause a backward seek, locking the walker in a loop.
// (CodeRabbit Major on PR #223.)
func TestSafeSeekSkip_TruthTable(t *testing.T) {
	cases := []struct {
		name    string
		in      uint64
		want    int64
		wantErr bool
	}{
		{"zero", 0, 0, false},
		{"small", 4, 4, false},
		{"realistic DSD audio chunk (~4 GiB)", 4 << 30, 4 << 30, false},
		{"int64 max", math.MaxInt64, math.MaxInt64, false},
		{"int64 max + 1 (uint64-only)", uint64(math.MaxInt64) + 1, 0, true},
		{"uint64 max (malformed)", math.MaxUint64, 0, true},
	}
	for _, c := range cases {
		got, err := safeSeekSkip(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("%s: safeSeekSkip(%d) = (%d, nil), want error", c.name, c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: safeSeekSkip(%d) unexpected error: %v", c.name, c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s: safeSeekSkip(%d) = %d, want %d", c.name, c.in, got, c.want)
		}
	}
}

// TestExtractDFF_OversizedChunkRejected verifies the overflow guard
// fires end-to-end: a DSDIFF file with a chunk declaring a uint64
// size beyond int64 max must error rather than seek backward and
// loop. The DSDIFF chunk-size field is BE64 (full uint64 range), so
// a malicious file can trigger this.
func TestExtractDFF_OversizedChunkRejected(t *testing.T) {
	// Build a minimal valid PROP/SND chunk, then append a hostile
	// chunk with size = 0xFFFFFFFFFFFFFFFF after it.
	prop := []byte("SND ")
	fs := []byte("FS  ")
	var fsSize [8]byte
	binary.BigEndian.PutUint64(fsSize[:], 4)
	fs = append(fs, fsSize[:]...)
	var rate [4]byte
	binary.BigEndian.PutUint32(rate[:], 2_822_400)
	fs = append(fs, rate[:]...)
	prop = append(prop, fs...)
	cmpr := []byte("CMPR")
	var cmprSize [8]byte
	binary.BigEndian.PutUint64(cmprSize[:], 5)
	cmpr = append(cmpr, cmprSize[:]...)
	cmpr = append(cmpr, []byte("DSD ")...)
	cmpr = append(cmpr, 0x00, 0x00)
	prop = append(prop, cmpr...)

	propWithHeader := []byte("PROP")
	var propSize [8]byte
	binary.BigEndian.PutUint64(propSize[:], uint64(len(prop)))
	propWithHeader = append(propWithHeader, propSize[:]...)
	propWithHeader = append(propWithHeader, prop...)

	// Hostile chunk: FOURCC + uint64-max size.
	bad := []byte("XXXX")
	var badSize [8]byte
	binary.BigEndian.PutUint64(badSize[:], math.MaxUint64)
	bad = append(bad, badSize[:]...)

	body := []byte("DSD ")
	body = append(body, propWithHeader...)
	body = append(body, bad...)

	out := []byte("FRM8")
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(body)))
	out = append(out, size[:]...)
	out = append(out, body...)

	path := writeTempDFF(t, out)
	track := &Track{}
	err := extractDFFWithContext(path, track, nil)
	if err == nil {
		t.Fatalf("extractDFFWithContext: want error on uint64-max chunk size, got nil")
	}
	// Codec was stamped before the walk so the manifest row still
	// classifies as DFF even on a malformed file.
	if track.Codec != "DFF" {
		t.Errorf("Codec = %q, want %q (must stamp before walk)", track.Codec, "DFF")
	}
}

// buildDIINSubChunk constructs one DSDIFF DIIN sub-chunk (DITI / DIAR /
// DIAL / DIGN) carrying a pstring payload. Layout:
//
//	[4 bytes FOURCC][8 bytes BE size][1 byte length][N bytes text][pad if (1+N) odd]
//
// `size` in the chunk header is `1 + N` (length byte + data) — the
// pad byte is OUTSIDE the chunk's declared size and belongs to the
// outer walker's alignment rule.
func buildDIINSubChunk(fourcc, text string) []byte {
	if len(fourcc) != 4 {
		panic("DIIN sub-chunk FOURCC must be 4 bytes")
	}
	n := len(text)
	if n > 255 {
		panic("DIIN pstring length exceeds 1-byte max (255)")
	}
	out := []byte{}
	out = append(out, []byte(fourcc)...)
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(1+n)) // length byte + data
	out = append(out, size[:]...)
	out = append(out, byte(n))
	out = append(out, []byte(text)...)
	// Pad byte if total chunk payload (1 + n) is odd.
	if (1+n)%2 == 1 {
		out = append(out, 0x00)
	}
	return out
}

// buildDIINContainer wraps a slice of DIIN sub-chunks in the outer
// DIIN container chunk header.
func buildDIINContainer(subChunks []byte) []byte {
	out := []byte{}
	out = append(out, []byte("DIIN")...)
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(subChunks)))
	out = append(out, size[:]...)
	out = append(out, subChunks...)
	// Pad byte if DIIN container payload is odd.
	if len(subChunks)%2 == 1 {
		out = append(out, 0x00)
	}
	return out
}

// buildDFFWithDIIN extends buildDFF to embed a DIIN container chunk
// after the DSD audio stub. DSDIFF spec allows DIIN AFTER the audio
// payload — this exercises the walker's continue-past-DSD path.
func buildDFFWithDIIN(t *testing.T, sampleRate uint32, compression string, diinSubChunks []byte) []byte {
	t.Helper()
	if len(compression) != 4 {
		t.Fatalf("compression FOURCC must be 4 bytes, got %q", compression)
	}
	// Reuse the existing buildDFF skeleton up to the FRM8 outer wrap,
	// then re-construct the body to inject DIIN after DSD.
	// Easiest: build the inner body from scratch (we don't need any
	// of buildDFF's wrap logic for this).
	prop := []byte{}
	prop = append(prop, []byte("SND ")...)
	fs := []byte{}
	fs = append(fs, []byte("FS  ")...)
	var fsSize [8]byte
	binary.BigEndian.PutUint64(fsSize[:], 4)
	fs = append(fs, fsSize[:]...)
	var rate [4]byte
	binary.BigEndian.PutUint32(rate[:], sampleRate)
	fs = append(fs, rate[:]...)
	prop = append(prop, fs...)

	cmpr := []byte{}
	cmpr = append(cmpr, []byte("CMPR")...)
	var cmprSize [8]byte
	binary.BigEndian.PutUint64(cmprSize[:], 5)
	cmpr = append(cmpr, cmprSize[:]...)
	cmpr = append(cmpr, []byte(compression)...)
	cmpr = append(cmpr, 0x00)
	cmpr = append(cmpr, 0x00) // pad
	prop = append(prop, cmpr...)

	propWithHeader := []byte{}
	propWithHeader = append(propWithHeader, []byte("PROP")...)
	var propSize [8]byte
	binary.BigEndian.PutUint64(propSize[:], uint64(len(prop)))
	propWithHeader = append(propWithHeader, propSize[:]...)
	propWithHeader = append(propWithHeader, prop...)

	dsd := []byte{}
	dsd = append(dsd, []byte("DSD ")...)
	var dsdSize [8]byte
	binary.BigEndian.PutUint64(dsdSize[:], 4)
	dsd = append(dsd, dsdSize[:]...)
	dsd = append(dsd, 0x00, 0x00, 0x00, 0x00)

	diin := buildDIINContainer(diinSubChunks)

	body := []byte{}
	body = append(body, []byte("DSD ")...) // FRM8 form type
	body = append(body, propWithHeader...)
	body = append(body, dsd...)
	body = append(body, diin...)

	out := []byte{}
	out = append(out, []byte("FRM8")...)
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(body)))
	out = append(out, size[:]...)
	out = append(out, body...)
	return out
}

func TestExtractDFF_DIIN_PopulatesTitleArtistAlbumGenre(t *testing.T) {
	sub := []byte{}
	sub = append(sub, buildDIINSubChunk("DITI", "Symphony No. 5 in C Minor")...) // even length (25): chunk payload = 26 even, no pad
	sub = append(sub, buildDIINSubChunk("DIAR", "Ludwig van Beethoven")...)      // 20 chars: 1+20=21 odd → pad
	sub = append(sub, buildDIINSubChunk("DIAL", "Beethoven Symphonies")...)      // 20 chars: 1+20=21 odd → pad
	sub = append(sub, buildDIINSubChunk("DIGN", "Classical")...)                 // 9 chars: 1+9=10 even, no pad
	path := writeTempDFF(t, buildDFFWithDIIN(t, 2_822_400, "DSD ", sub))

	track := &Track{}
	if err := extractDFFWithContext(path, track, nil); err != nil {
		t.Fatalf("extractDFFWithContext: %v", err)
	}
	if track.Title != "Symphony No. 5 in C Minor" {
		t.Errorf("Title = %q, want %q", track.Title, "Symphony No. 5 in C Minor")
	}
	if track.Artist != "Ludwig van Beethoven" {
		t.Errorf("Artist = %q, want %q", track.Artist, "Ludwig van Beethoven")
	}
	if track.Album != "Beethoven Symphonies" {
		t.Errorf("Album = %q, want %q", track.Album, "Beethoven Symphonies")
	}
	if track.Genre != "Classical" {
		t.Errorf("Genre = %q, want %q", track.Genre, "Classical")
	}
	// Existing PROP fields must still populate alongside DIIN.
	if track.IsDSD == nil || !*track.IsDSD {
		t.Errorf("IsDSD = %v, want non-nil true", track.IsDSD)
	}
	if track.SampleRate == nil || *track.SampleRate != 2_822_400 {
		t.Errorf("SampleRate = %v, want 2822400", track.SampleRate)
	}
}

// TestExtractDFF_DIIN_OddLengthPStringPadHandling locks the pad-byte
// rule for a pstring whose `1 + length` is odd. Without proper pad
// handling, the next sub-chunk would mis-align and read garbage.
func TestExtractDFF_DIIN_OddLengthPStringPadHandling(t *testing.T) {
	// "AB" is 2 bytes; 1+2 = 3 odd → one pad byte follows the
	// pstring. Then DIAR "CD" (1+2=3 odd → pad). If the walker
	// loses alignment, DIAR's FOURCC would be misread.
	sub := []byte{}
	sub = append(sub, buildDIINSubChunk("DITI", "AB")...)
	sub = append(sub, buildDIINSubChunk("DIAR", "CD")...)
	path := writeTempDFF(t, buildDFFWithDIIN(t, 2_822_400, "DSD ", sub))

	track := &Track{}
	if err := extractDFFWithContext(path, track, nil); err != nil {
		t.Fatalf("extractDFFWithContext: %v", err)
	}
	if track.Title != "AB" {
		t.Errorf("Title = %q, want %q (pstring pad mishandled?)", track.Title, "AB")
	}
	if track.Artist != "CD" {
		t.Errorf("Artist = %q, want %q (pstring pad mishandled?)", track.Artist, "CD")
	}
}

// TestExtractDFF_DIIN_OverrunPStringSkipsField verifies the
// defensive bounds check: a pstring declaring length > available
// bytes must be skipped (field stays empty) rather than crashing or
// over-reading.
func TestExtractDFF_DIIN_OverrunPStringSkipsField(t *testing.T) {
	// Manually craft a malformed DITI sub-chunk: header declares
	// payload-size = 5 bytes (1 length + 4 data), but the length
	// byte inside the payload declares 100. parseDIINChunks's
	// bounds check should refuse the overrun and leave Title empty,
	// while the walker advances past the declared 5-byte payload
	// and resumes scanning. 12-byte header + 5-byte payload = 17
	// bytes total; 17 is odd so one pad byte follows before DIAR.
	sub := []byte{}
	sub = append(sub, []byte("DITI")...)
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], 5)
	sub = append(sub, size[:]...)
	sub = append(sub, byte(100))              // declared length 100 — overrun
	sub = append(sub, 0x41, 0x42, 0x43, 0x44) // 4 bytes of data (size==5 total with length byte)
	sub = append(sub, 0x00)                   // pad: (12+5)=17 odd → 1 pad byte
	sub = append(sub, buildDIINSubChunk("DIAR", "OK")...)

	path := writeTempDFF(t, buildDFFWithDIIN(t, 2_822_400, "DSD ", sub))

	track := &Track{}
	if err := extractDFFWithContext(path, track, nil); err != nil {
		t.Fatalf("extractDFFWithContext: %v", err)
	}
	if track.Title != "" {
		t.Errorf("Title = %q, want empty (overrun pstring should be skipped)", track.Title)
	}
	if track.Artist != "OK" {
		t.Errorf("Artist = %q, want %q (next sub-chunk should still parse)", track.Artist, "OK")
	}
}

// TestExtractDFF_DIIN_EmptyContainerSafe pins the zero-sub-chunk case:
// a present-but-empty DIIN container must not crash and must leave
// all DIIN-sourced fields empty.
func TestExtractDFF_DIIN_EmptyContainerSafe(t *testing.T) {
	path := writeTempDFF(t, buildDFFWithDIIN(t, 2_822_400, "DSD ", nil))
	track := &Track{}
	if err := extractDFFWithContext(path, track, nil); err != nil {
		t.Fatalf("extractDFFWithContext: %v", err)
	}
	if track.Title != "" || track.Artist != "" || track.Album != "" || track.Genre != "" {
		t.Errorf("empty DIIN should leave fields empty: title=%q artist=%q album=%q genre=%q",
			track.Title, track.Artist, track.Album, track.Genre)
	}
	// PROP fields still populate.
	if track.IsDSD == nil || !*track.IsDSD {
		t.Errorf("IsDSD = %v, want non-nil true", track.IsDSD)
	}
}

// TestExtractDFF_DIIN_COMTSkippedSafely pins the COMT sub-chunk
// skip path: a DIIN container with a COMT chunk (which has a
// structured layout we don't decode) must not crash and must
// continue parsing other sub-chunks correctly.
func TestExtractDFF_DIIN_COMTSkippedSafely(t *testing.T) {
	// Build a minimal COMT sub-chunk: 4 bytes "COMT" + 8 bytes size
	// (10) + 10 bytes opaque data. Total payload size 10 even → no
	// pad. The walker should skip it and proceed to DIAR.
	comt := []byte{}
	comt = append(comt, []byte("COMT")...)
	var commentSize [8]byte
	binary.BigEndian.PutUint64(commentSize[:], 10)
	comt = append(comt, commentSize[:]...)
	comt = append(comt, []byte("0123456789")...)

	sub := []byte{}
	sub = append(sub, comt...)
	sub = append(sub, buildDIINSubChunk("DIAR", "After COMT")...)

	path := writeTempDFF(t, buildDFFWithDIIN(t, 2_822_400, "DSD ", sub))

	track := &Track{}
	if err := extractDFFWithContext(path, track, nil); err != nil {
		t.Fatalf("extractDFFWithContext: %v", err)
	}
	if track.Artist != "After COMT" {
		t.Errorf("Artist = %q, want %q (COMT skip mishandled?)", track.Artist, "After COMT")
	}
}
