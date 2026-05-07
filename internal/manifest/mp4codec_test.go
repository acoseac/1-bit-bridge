package manifest

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// TestExtractMP4CodecAlac builds a minimal MP4 atom hierarchy by hand
// (ftyp + moov/trak/mdia/minf/stbl/stsd with an `alac` sample entry)
// and asserts the walker returns "ALAC". Per Gemini A1 / iOS bug
// review #1.
func TestExtractMP4CodecAlac(t *testing.T) {
	mp4 := buildMinimalMP4("alac")
	got, err := extractMP4Codec(bytes.NewReader(mp4))
	if err != nil {
		t.Fatal(err)
	}
	if got != "ALAC" {
		t.Errorf("got %q, want %q", got, "ALAC")
	}
}

// TestExtractMP4CodecAac — same shape with `mp4a` (the canonical
// AAC FourCC) returns "AAC".
func TestExtractMP4CodecAac(t *testing.T) {
	mp4 := buildMinimalMP4("mp4a")
	got, err := extractMP4Codec(bytes.NewReader(mp4))
	if err != nil {
		t.Fatal(err)
	}
	if got != "AAC" {
		t.Errorf("got %q, want %q", got, "AAC")
	}
}

// TestExtractMP4CodecUnknownReturnsEmpty — an unknown codec FourCC
// returns "" rather than erroring; the caller falls through to the
// extension-derived classification.
func TestExtractMP4CodecUnknownReturnsEmpty(t *testing.T) {
	mp4 := buildMinimalMP4("xxxx")
	got, err := extractMP4Codec(bytes.NewReader(mp4))
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Errorf("got %q, want empty for unknown codec", got)
	}
}

// TestExtractMP4CodecMissingMoovErrors — a non-MP4 file (or a
// truncated one) returns an error rather than silently returning ""
// (which would be ambiguous with an unknown codec).
func TestExtractMP4CodecMissingMoovErrors(t *testing.T) {
	// Just an ftyp atom, no moov.
	buf := &bytes.Buffer{}
	writeAtom(buf, "ftyp", []byte("M4A mp42M4A "))
	if _, err := extractMP4Codec(bytes.NewReader(buf.Bytes())); err == nil {
		t.Error("expected error for missing moov, got nil")
	}
}

// buildMinimalMP4 returns a byte slice containing the smallest
// MP4-shaped tree the codec walker needs: ftyp + moov containing one
// trak/mdia/minf/stbl/stsd with one sample entry whose format is the
// given codec FourCC. The other fields inside each atom are
// zero-padded — the walker doesn't read them.
func buildMinimalMP4(codec string) []byte {
	if len(codec) != 4 {
		panic("buildMinimalMP4: codec must be 4 chars")
	}

	// Build innermost first: stsd payload (1 entry of 16 bytes:
	// size+fourcc+8 padding). The sample entry's "size" field
	// itself is not consumed by the parser, but we set it to 16
	// for shape-correctness.
	entry := &bytes.Buffer{}
	binary.Write(entry, binary.BigEndian, uint32(16)) // entry size
	entry.WriteString(codec)                          // entry format FourCC
	entry.Write(make([]byte, 8))                      // payload padding

	// stsd payload: 1 byte version + 3 bytes flags + 4 bytes count
	// + the entry above.
	stsdPayload := &bytes.Buffer{}
	stsdPayload.Write(make([]byte, 4))                     // version+flags
	binary.Write(stsdPayload, binary.BigEndian, uint32(1)) // entry_count
	stsdPayload.Write(entry.Bytes())                       // 1 entry

	stbl := &bytes.Buffer{}
	writeAtom(stbl, "stsd", stsdPayload.Bytes())

	minf := &bytes.Buffer{}
	writeAtom(minf, "stbl", stbl.Bytes())

	mdia := &bytes.Buffer{}
	writeAtom(mdia, "minf", minf.Bytes())

	trak := &bytes.Buffer{}
	writeAtom(trak, "mdia", mdia.Bytes())

	moov := &bytes.Buffer{}
	writeAtom(moov, "trak", trak.Bytes())

	out := &bytes.Buffer{}
	writeAtom(out, "ftyp", []byte("M4A mp42M4A ")) // 12-byte body, valid-shaped
	writeAtom(out, "moov", moov.Bytes())
	return out.Bytes()
}

// writeAtom writes a single MP4 atom with header (8 bytes: size,
// type) + payload to w.
func writeAtom(w *bytes.Buffer, atomType string, payload []byte) {
	if len(atomType) != 4 {
		panic("writeAtom: type must be 4 chars")
	}
	size := uint32(8 + len(payload))
	binary.Write(w, binary.BigEndian, size)
	w.WriteString(atomType)
	w.Write(payload)
}
