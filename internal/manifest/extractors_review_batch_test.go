package manifest

import (
	"bytes"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestSkipID3v2 pins the offset math of the ID3v2-skip helper, including
// the ID3v2.4-only footer gate: a non-conforming v2.2/v2.3 tag with the
// footer bit set must NOT make us over-skip into the payload.
func TestSkipID3v2(t *testing.T) {
	id3Header := func(major, flags byte, payloadSize int) []byte {
		h := make([]byte, 10)
		copy(h[0:3], "ID3")
		h[3] = major
		h[5] = flags
		n := uint32(payloadSize)
		h[6] = byte((n >> 21) & 0x7f)
		h[7] = byte((n >> 14) & 0x7f)
		h[8] = byte((n >> 7) & 0x7f)
		h[9] = byte(n & 0x7f)
		return h
	}
	const payloadSize = 20
	tag := func(major, flags byte) []byte {
		return append(id3Header(major, flags, payloadSize), bytes.Repeat([]byte{0x11}, payloadSize)...)
	}
	marker := []byte("fLaC and the rest of the stream")

	cases := []struct {
		name    string
		data    []byte
		wantPos int64
	}{
		{"no id3 rewinds to start", append([]byte("fLaC"), marker...), 0},
		{"v2.3 no footer", append(tag(3, 0x00), marker...), 10 + payloadSize},
		{"v2.3 footer-bit-set is ignored", append(tag(3, 0x10), marker...), 10 + payloadSize},
		{"v2.4 footer skips extra 10", append(tag(4, 0x10), marker...), 10 + payloadSize + 10},
		{"too short rewinds to start", []byte("ID3"), 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := bytes.NewReader(tc.data)
			if err := skipID3v2(r); err != nil {
				t.Fatalf("skipID3v2: %v", err)
			}
			pos, _ := r.Seek(0, io.SeekCurrent)
			if pos != tc.wantPos {
				t.Errorf("cursor at %d, want %d", pos, tc.wantPos)
			}
		})
	}
}

// TestExtCoversDispatcher pins the F1 invariant: every extension the
// format dispatcher (extractByFormat) routes to a dedicated parser must
// also be registered in the Ext discovery allowlist. Pre-fix, .m4b /
// .m4p / .oga were parseable by the dispatcher but missing from Ext, so
// the scanner's WalkDir gate skipped them before they ever reached the
// dispatcher (the cases were unreachable dead code).
//
// Go can't reflect over switch-case string literals at runtime, so this
// list is dual-maintained with extractByFormat's switch — keep the two
// in sync (both carry a lockstep comment saying so).
func TestExtCoversDispatcher(t *testing.T) {
	dispatched := []string{
		".iso",
		".dsf", ".dff",
		".aif", ".aiff", ".aifc",
		".wav",
		".m4a", ".mp4", ".m4b", ".m4p",
		".mp3",
		".ogg", ".oga",
		".flac",
	}
	for _, ext := range dispatched {
		if !Ext[ext] {
			t.Errorf("extractByFormat handles %q but it is missing from Ext — "+
				"the scanner skips these files at discovery (scanner.go: if !Ext[ext])", ext)
		}
	}
}

// TestExtractFLAC_ID3v2PrefixStillReadsFormatAndMultiValue pins F4. Some
// taggers prepend an ID3v2 tag to a FLAC (out of spec but common). Pre-
// fix, extractFLACFormatFromReader + applyFLACMultiValueArtists read the
// fLaC magic at offset 0, saw "ID3", and bailed — so the track lost its
// hi-res format fields (sampleRate / bitsPerSample) AND its multi-value
// artist array. skipID3v2 seeks past the prefix so both passes work.
//
// We assert only the fields those two passes own — dhowden's own read of
// an ID3-prefixed FLAC (Title/Album) is out of scope here.
func TestExtractFLAC_ID3v2PrefixStillReadsFormatAndMultiValue(t *testing.T) {
	dir := t.TempDir()

	base := filepath.Join(dir, "base.flac")
	writeMinimalFLACPairs(t, base, 96000, 24, [][2]string{
		{"TITLE", "Reflections"},
		{"ALBUM", "The Balance"},
		{"ARTIST", "Abdullah Ibrahim"},
		{"ARTIST", "Ekaya"},
	})
	flacBytes, err := os.ReadFile(base)
	if err != nil {
		t.Fatalf("read base flac: %v", err)
	}

	// Prepend a non-trivial ID3v2 tag, as a broken tagger would.
	id3 := buildID3v2_3(map[string]string{"title": "ID3-Prefix-Title"})
	p := filepath.Join(dir, "prefixed.flac")
	if err := os.WriteFile(p, append(id3, flacBytes...), 0o644); err != nil {
		t.Fatalf("write prefixed flac: %v", err)
	}

	tr := &Track{Path: "prefixed.flac", Size: 1, ModTime: time.Now()}
	if err := Extract(p, tr); err != nil {
		t.Fatalf("Extract: %v", err)
	}

	// Format fields recovered past the ID3v2 prefix (extractFLACFormatFromReader).
	if tr.SampleRate == nil || *tr.SampleRate != 96000 {
		t.Errorf("SampleRate = %v, want 96000 (format parse must skip the ID3v2 prefix)", tr.SampleRate)
	}
	if tr.BitsPerSample == nil || *tr.BitsPerSample != 24 {
		t.Errorf("BitsPerSample = %v, want 24 (format parse must skip the ID3v2 prefix)", tr.BitsPerSample)
	}
	// Multi-value ARTIST recovered past the prefix (applyFLACMultiValueArtists).
	if tr.Artist != "Abdullah Ibrahim; Ekaya" {
		t.Errorf("Artist = %q, want %q (multi-value join must skip the ID3v2 prefix)", tr.Artist, "Abdullah Ibrahim; Ekaya")
	}
}

// framedID3Chunk frames an ID3 payload as an IFF/RIFF sub-chunk
// (FOURCC + size + payload + pad-if-odd), so it can be passed as an
// extraChunk to buildAIFFWithID3 / buildWAVWithID3 and thus be walked
// BEFORE the builder's own id3 sub-chunk (controlling chunk order).
func framedID3Chunk(fourcc string, payload []byte, bigEndian bool) []byte {
	out := make([]byte, 0, 8+len(payload)+1)
	out = append(out, fourcc...)
	var sz [4]byte
	if bigEndian {
		binary.BigEndian.PutUint32(sz[:], uint32(len(payload)))
	} else {
		binary.LittleEndian.PutUint32(sz[:], uint32(len(payload)))
	}
	out = append(out, sz[:]...)
	out = append(out, payload...)
	if len(payload)%2 == 1 {
		out = append(out, 0x00)
	}
	return out
}

// TestApplyEmbeddedID3_EarliestChunkWinsEntirely pins F12. A WAV/AIFF
// carrying two ID3 chunks must resolve to the EARLIEST chunk's metadata
// entirely. Pre-fix, populateFromTagMetadata ran unconditionally on
// every chunk (its `if v != ""` guards are last-non-empty-wins), so the
// second chunk's Title overwrote the first while the artwork carrier
// stayed the first — a torn state. The short-circuit makes it first-wins.
func TestApplyEmbeddedID3_EarliestChunkWinsEntirely(t *testing.T) {
	first := buildID3v2_3(map[string]string{"title": "First", "artist": "Alpha"})
	second := buildID3v2_3(map[string]string{"title": "Second", "artist": "Beta"})

	t.Run("aiff", func(t *testing.T) {
		firstChunk := framedID3Chunk("ID3 ", first, true) // walked first
		path := writeTempAIFF(t, buildAIFFWithID3(t, second, firstChunk))
		tr := &Track{}
		if err := extractAIFFWithContext(path, tr, nil); err != nil {
			t.Fatalf("extractAIFFWithContext: %v", err)
		}
		if tr.Title != "First" {
			t.Errorf("Title = %q, want %q (earliest ID3 chunk must win entirely)", tr.Title, "First")
		}
		if tr.Artist != "Alpha" {
			t.Errorf("Artist = %q, want %q (earliest ID3 chunk must win entirely)", tr.Artist, "Alpha")
		}
	})

	t.Run("wav", func(t *testing.T) {
		firstChunk := framedID3Chunk("id3 ", first, false) // RIFF: LE size, lowercase FOURCC
		path := writeTempWAV(t, buildWAVWithID3(t, second, firstChunk))
		tr := &Track{}
		if err := extractWAVWithContext(path, tr, nil); err != nil {
			t.Fatalf("extractWAVWithContext: %v", err)
		}
		if tr.Title != "First" {
			t.Errorf("Title = %q, want %q (earliest ID3 chunk must win entirely)", tr.Title, "First")
		}
		if tr.Artist != "Alpha" {
			t.Errorf("Artist = %q, want %q (earliest ID3 chunk must win entirely)", tr.Artist, "Alpha")
		}
	})
}
