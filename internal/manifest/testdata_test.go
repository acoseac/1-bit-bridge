package manifest

import (
	"bytes"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"testing"

	flac "github.com/mewkiz/flac"
	meta "github.com/mewkiz/flac/meta"
)

// writeMinimalFLAC produces a valid (if silent) FLAC file with STREAMINFO
// and a Vorbis comment block carrying the provided tags. The mewkiz/flac
// library writes actual valid FLAC bytes, which means our extractors can
// round-trip them exactly like a real file from ffmpeg / dBpoweramp.
func writeMinimalFLAC(t *testing.T, path string, sampleRate, bitsPerSample int, vorbisTags map[string]string) {
	t.Helper()
	pairs := make([][2]string, 0, len(vorbisTags))
	for k, v := range vorbisTags {
		pairs = append(pairs, [2]string{k, v})
	}
	writeMinimalFLACPairs(t, path, sampleRate, bitsPerSample, pairs)
}

// writeMinimalFLACPairs is the multi-value-aware variant — accepts
// `[][2]string` so duplicate keys (e.g. multiple `ARTIST=` Vorbis
// Comments on a collaboration FLAC) are preserved in source order
// rather than collapsed by Go's map semantics.
func writeMinimalFLACPairs(t *testing.T, path string, sampleRate, bitsPerSample int, vorbisTags [][2]string) {
	t.Helper()

	// STREAMINFO block. Most fields can be placeholder; only SampleRate,
	// BitsPerSample, and NSamples are material to our extractor.
	info := &meta.StreamInfo{
		BlockSizeMin:  4096,
		BlockSizeMax:  4096,
		FrameSizeMin:  0,
		FrameSizeMax:  0,
		SampleRate:    uint32(sampleRate),
		NChannels:     2,
		BitsPerSample: uint8(bitsPerSample),
		NSamples:      uint64(sampleRate * 5), // 5 seconds
	}

	// Vorbis comment block.
	vc := meta.VorbisComment{
		Vendor: "1-bit-bridge test fixture",
	}
	vc.Tags = append(vc.Tags, vorbisTags...)

	// Build the file bytes manually. mewkiz/flac's Encoder is geared at
	// audio frames; for a tag/format round-trip the raw metadata-block
	// layout is simpler and faster.
	var buf bytes.Buffer
	buf.WriteString("fLaC")

	// STREAMINFO block header: last=0, type=0, len=34.
	writeMetaBlockHeader(&buf, meta.TypeStreamInfo, false, 34)
	writeStreamInfo(&buf, info)

	// Vorbis Comment block (last).
	vcBody := encodeVorbisComment(&vc)
	writeMetaBlockHeader(&buf, meta.TypeVorbisComment, true, uint32(len(vcBody)))
	buf.Write(vcBody)

	// No audio frames — the STREAMINFO is enough for tag + format reads.
	// mewkiz/flac.ParseFile only consumes the metadata if we never read
	// frames, so this is fine for our tests.

	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write FLAC fixture: %v", err)
	}

	// Sanity: our own mewkiz/flac wrapper can parse what we just wrote.
	s, err := flac.ParseFile(path)
	if err != nil {
		t.Fatalf("fixture didn't round-trip through mewkiz/flac: %v", err)
	}
	_ = s.Close()
}

func writeMetaBlockHeader(w io.Writer, t meta.Type, isLast bool, length uint32) {
	first := byte(t) & 0x7F
	if isLast {
		first |= 0x80
	}
	w.Write([]byte{first, byte(length >> 16), byte(length >> 8), byte(length)})
}

func writeStreamInfo(w io.Writer, si *meta.StreamInfo) {
	// 34-byte STREAMINFO per FLAC spec.
	var b [34]byte
	binary.BigEndian.PutUint16(b[0:2], si.BlockSizeMin)
	binary.BigEndian.PutUint16(b[2:4], si.BlockSizeMax)
	// FrameSizeMin/Max: 24-bit ints.
	b[4], b[5], b[6] = 0, 0, 0
	b[7], b[8], b[9] = 0, 0, 0
	// Packed: sampleRate (20 bits), channels-1 (3), bps-1 (5), totalSamples (36) → 64 bits total.
	// Byte 10-17.
	var packed uint64
	packed |= uint64(si.SampleRate&0xFFFFF) << 44
	packed |= uint64(uint8(si.NChannels-1)&0x7) << 41
	packed |= uint64((uint8(si.BitsPerSample)-1)&0x1F) << 36
	packed |= si.NSamples & 0xFFFFFFFFF
	binary.BigEndian.PutUint64(b[10:18], packed)
	// MD5 of unencoded audio (16 bytes) — all zeros.
	w.Write(b[:])
}

func encodeVorbisComment(vc *meta.VorbisComment) []byte {
	var buf bytes.Buffer
	// Vendor length + vendor string (little-endian length).
	vendor := []byte(vc.Vendor)
	binary.Write(&buf, binary.LittleEndian, uint32(len(vendor)))
	buf.Write(vendor)
	// Comment count.
	binary.Write(&buf, binary.LittleEndian, uint32(len(vc.Tags)))
	for _, tagPair := range vc.Tags {
		s := tagPair[0] + "=" + tagPair[1]
		binary.Write(&buf, binary.LittleEndian, uint32(len(s)))
		buf.Write([]byte(s))
	}
	return buf.Bytes()
}

// writeMinimalDSF produces a valid DSF container with:
//   - DSD chunk header (28 bytes) pointing at an ID3v2 block at the end
//   - fmt chunk (52 bytes) carrying sampleRate / bitsPerSample / sampleCount
//   - data chunk header (12 bytes) with no audio data
//   - an ID3v2.3 tag with the requested frames
//
// This mirrors exactly what our extractDSF walks, so positive tests
// exercise the real parser, not a simplified one.
func writeMinimalDSF(t *testing.T, path string, sampleRate uint32, frames map[string]string) {
	t.Helper()
	id3 := buildID3v2_3(frames)

	var buf bytes.Buffer
	// Build fmt and data first so we know offsets.
	var fmtChunk [52]byte
	copy(fmtChunk[0:4], []byte("fmt "))
	binary.LittleEndian.PutUint64(fmtChunk[4:12], 52) // chunk size
	binary.LittleEndian.PutUint32(fmtChunk[12:16], 1) // format version
	binary.LittleEndian.PutUint32(fmtChunk[16:20], 0) // format ID: DSD
	binary.LittleEndian.PutUint32(fmtChunk[20:24], 2) // channel type (stereo)
	binary.LittleEndian.PutUint32(fmtChunk[24:28], 2) // channel num
	binary.LittleEndian.PutUint32(fmtChunk[28:32], sampleRate)
	binary.LittleEndian.PutUint32(fmtChunk[32:36], 1) // bits/sample = 1 for DSD
	sampleCount := uint64(sampleRate * 5)             // ~5 seconds
	binary.LittleEndian.PutUint64(fmtChunk[36:44], sampleCount)
	// blockSizePerChannel lives at [44:48]; a common value is 4096.
	binary.LittleEndian.PutUint32(fmtChunk[44:48], 4096)

	var dataHeader [12]byte
	copy(dataHeader[0:4], []byte("data"))
	binary.LittleEndian.PutUint64(dataHeader[4:12], 12) // no audio bytes

	// Total file size = 28 + 52 + 12 + len(id3)
	totalSize := uint64(28 + 52 + 12 + len(id3))
	metadataPointer := uint64(28 + 52 + 12) // offset of ID3

	// DSD chunk header.
	var dsd [28]byte
	copy(dsd[0:4], []byte("DSD "))
	binary.LittleEndian.PutUint64(dsd[4:12], 28)
	binary.LittleEndian.PutUint64(dsd[12:20], totalSize)
	binary.LittleEndian.PutUint64(dsd[20:28], metadataPointer)

	buf.Write(dsd[:])
	buf.Write(fmtChunk[:])
	buf.Write(dataHeader[:])
	buf.Write(id3)

	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write DSF fixture: %v", err)
	}
}

// buildID3v2_3 builds a minimal ID3v2.3 blob containing TEXT-encoded frames
// for the provided map. Frame IDs follow the standard ID3v2 naming (TIT2
// = title, TPE1 = artist, TALB = album, TRCK = track, TYER = year, etc.).
func buildID3v2_3(fields map[string]string) []byte {
	// Map human keys to ID3v2.3 frame IDs.
	frameKeys := map[string]string{
		"title":       "TIT2",
		"artist":      "TPE1",
		"albumArtist": "TPE2",
		"album":       "TALB",
		"track":       "TRCK",
		"year":        "TYER",
		"genre":       "TCON",
		"disc":        "TPOS",
	}
	var body bytes.Buffer
	for k, v := range fields {
		id, ok := frameKeys[k]
		if !ok {
			continue
		}
		// Frame: 4-byte ID + 4-byte size + 2-byte flags + data.
		// Data = 1-byte encoding (0x00 = ISO-8859-1) + value bytes.
		data := append([]byte{0x00}, []byte(v)...)
		body.WriteString(id)
		// ID3v2.3 size is a regular uint32 big-endian (not synchsafe —
		// that's v2.4).
		var sz [4]byte
		binary.BigEndian.PutUint32(sz[:], uint32(len(data)))
		body.Write(sz[:])
		body.Write([]byte{0x00, 0x00}) // flags
		body.Write(data)
	}
	// ID3v2 header: "ID3" + major(3) + minor(0) + flags + synchsafe-size.
	var header [10]byte
	copy(header[0:3], []byte("ID3"))
	header[3] = 0x03 // v2.3
	header[4] = 0x00
	header[5] = 0x00 // flags
	writeSyncSafeSize(header[6:10], uint32(body.Len()))
	return append(header[:], body.Bytes()...)
}

// writeSyncSafeSize encodes n (which must be < 2^28) as a 4-byte
// synchsafe integer per ID3v2 spec.
func writeSyncSafeSize(out []byte, n uint32) {
	out[0] = byte((n >> 21) & 0x7F)
	out[1] = byte((n >> 14) & 0x7F)
	out[2] = byte((n >> 7) & 0x7F)
	out[3] = byte(n & 0x7F)
}

// writeMinimalMP3 writes a valid ID3v2.3 header followed by a single MP3
// sync frame. dhowden/tag will happily parse tags from this.
func writeMinimalMP3(t *testing.T, path string, frames map[string]string) {
	t.Helper()
	id3 := buildID3v2_3(frames)
	// A single dummy MPEG frame after the tag (dhowden/tag only needs the
	// tag part, but a file with only an ID3 block and no audio is still
	// valid).
	//
	// Frame header: 0xFFFB9064 is a common MPEG-1 Layer III frame.
	frame := []byte{0xFF, 0xFB, 0x90, 0x64}
	frame = append(frame, bytes.Repeat([]byte{0x00}, 144-4)...)

	if err := os.WriteFile(path, append(id3, frame...), 0o644); err != nil {
		t.Fatalf("write MP3 fixture: %v", err)
	}
}

// tempLibrary creates a new temp dir with a few fixture tracks and returns
// the dir plus (for convenience) the number of expected tracks.
func tempLibrary(t *testing.T) (root string, expectedTracks int) {
	t.Helper()
	root = filepath.Join(t.TempDir(), "Music")
	os.MkdirAll(filepath.Join(root, "Artist A", "Album 1"), 0o755)
	os.MkdirAll(filepath.Join(root, "Artist B", "Album 2"), 0o755)
	writeMinimalFLAC(t,
		filepath.Join(root, "Artist A", "Album 1", "01 FlacTrack.flac"),
		96000, 24,
		map[string]string{"TITLE": "Flac Title", "ARTIST": "Artist A", "ALBUM": "Album 1", "TRACKNUMBER": "1", "DATE": "2024"},
	)
	writeMinimalDSF(t,
		filepath.Join(root, "Artist A", "Album 1", "02 DsfTrack.dsf"),
		2822400,
		map[string]string{"title": "DSF Title", "artist": "Artist A", "album": "Album 1", "track": "2"},
	)
	writeMinimalMP3(t,
		filepath.Join(root, "Artist B", "Album 2", "01 Mp3Track.mp3"),
		map[string]string{"title": "Mp3 Title", "artist": "Artist B", "album": "Album 2", "track": "1"},
	)
	// A dot-file and a non-audio file that should be skipped.
	os.WriteFile(filepath.Join(root, "Artist A", "Album 1", ".DS_Store"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(root, "Artist A", "Album 1", "liner.txt"), []byte("x"), 0o644)
	return root, 3
}
