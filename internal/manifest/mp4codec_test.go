package manifest

import (
	"bytes"
	"encoding/binary"
	"strings"
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

// TestExtractMP4CodecLargeMoovContainer pads the moov box with a 5 MiB
// `free` atom BEFORE the trak, simulating a long M4A (audiobook / DJ
// mix) whose moov legitimately exceeds the 4 MiB byte-span the walker
// previously enforced. Pre-fix: findAtom errored with "search range …
// exceeds budget" on the first nested call. Post-fix: the iteration-
// count budget walks the atoms cleanly, finds trak, returns "ALAC".
func TestExtractMP4CodecLargeMoovContainer(t *testing.T) {
	const padding = 5 << 20 // 5 MiB free atom
	mp4 := buildMP4WithMoovPadding("alac", padding)
	if len(mp4) < padding {
		t.Fatalf("padded MP4 too small: %d bytes", len(mp4))
	}
	got, err := extractMP4Codec(bytes.NewReader(mp4))
	if err != nil {
		t.Fatalf("extractMP4Codec: %v", err)
	}
	if got != "ALAC" {
		t.Errorf("got %q, want %q", got, "ALAC")
	}
}

// TestExtractMP4CodecLargesize64Bit constructs MP4s with the 64-bit
// `largesize` header form (size==1 + 8-byte uint64 real size, total
// 16-byte header) at each level of the moov→trak→mdia→minf→stbl
// descent chain and asserts the codec walker still resolves to the
// inner ALAC sample entry. Pre-fix: extractMP4Codec used
// `parentStart + mp4HeaderSize` (= 8) for every descent, so a
// 64-bit container would be re-entered 8 bytes into its actual
// header — landing inside the largesize uint64 — and the next
// findAtom call would scan garbage. Each table entry locks one
// descent site; a future regression at any single site (say,
// reverting the trak→mdia descent to + mp4HeaderSize) fails one
// row without masking the others.
func TestExtractMP4CodecLargesize64Bit(t *testing.T) {
	for _, which := range []string{"moov", "trak", "mdia", "minf", "stbl"} {
		t.Run(which, func(t *testing.T) {
			mp4 := buildMP4WithLargesizeBox("alac", which)
			got, err := extractMP4Codec(bytes.NewReader(mp4))
			if err != nil {
				t.Fatalf("extractMP4Codec with 64-bit %s: %v", which, err)
			}
			if got != "ALAC" {
				t.Errorf("64-bit %s box: got %q, want %q", which, got, "ALAC")
			}
		})
	}
}

// TestFindAtomIterationBudget asserts the atom-walk count budget
// trips on a pathological input — a moov stuffed with 5000 tiny
// `free` atoms before the trak. Locks the safety net so a future
// regression that drops the budget can't silently allow unbounded
// header reads.
func TestFindAtomIterationBudget(t *testing.T) {
	moov := &bytes.Buffer{}
	// Write enough tiny `free` atoms to exceed mp4MaxAtomsPerSearch.
	tiny := make([]byte, 0) // zero-byte payload → 8-byte atom
	for i := 0; i < mp4MaxAtomsPerSearch+10; i++ {
		writeAtom(moov, "free", tiny)
	}
	// Then a trak — should never be reached.
	writeAtom(moov, "trak", []byte{})

	out := &bytes.Buffer{}
	writeAtom(out, "ftyp", []byte("M4A mp42M4A "))
	writeAtom(out, "moov", moov.Bytes())

	_, err := extractMP4Codec(bytes.NewReader(out.Bytes()))
	if err == nil {
		t.Fatal("expected iteration-budget error, got nil")
	}
	if !strings.Contains(err.Error(), "iteration budget") {
		t.Errorf("expected iteration-budget error, got %v", err)
	}
}

// TestExtractALACBitDepth_16BitFixture builds a minimal MP4 with an
// outer `alac` sample entry containing an inner `alac` config atom
// whose ALACSpecificConfig declares 16-bit source data. Matches the
// shape afinfo reports for the project's reference fixture
// (`01 Espina.m4a` — bit-exact byte layout validated 2026-05-14).
func TestExtractALACBitDepth_16BitFixture(t *testing.T) {
	mp4 := buildMP4WithALACConfig(16)
	got, err := extractALACBitDepth(bytes.NewReader(mp4))
	if err != nil {
		t.Fatal(err)
	}
	if got != 16 {
		t.Errorf("got %d, want 16", got)
	}
}

// TestExtractALACBitDepth_20BitFixture — defensive coverage for the
// rare 20-bit ALAC source (no known fixture in the project's test
// library; pinned by construction).
func TestExtractALACBitDepth_20BitFixture(t *testing.T) {
	mp4 := buildMP4WithALACConfig(20)
	got, err := extractALACBitDepth(bytes.NewReader(mp4))
	if err != nil {
		t.Fatal(err)
	}
	if got != 20 {
		t.Errorf("got %d, want 20", got)
	}
}

// TestExtractALACBitDepth_24BitFixture — common HD ALAC source depth.
func TestExtractALACBitDepth_24BitFixture(t *testing.T) {
	mp4 := buildMP4WithALACConfig(24)
	got, err := extractALACBitDepth(bytes.NewReader(mp4))
	if err != nil {
		t.Fatal(err)
	}
	if got != 24 {
		t.Errorf("got %d, want 24", got)
	}
}

// TestExtractALACBitDepth_32BitFixture — defends against the
// (legitimate) 32-bit ALAC source case, distinct from the
// (illegitimate) decoder-width 32 that prompted PR-pending.
func TestExtractALACBitDepth_32BitFixture(t *testing.T) {
	mp4 := buildMP4WithALACConfig(32)
	got, err := extractALACBitDepth(bytes.NewReader(mp4))
	if err != nil {
		t.Fatal(err)
	}
	if got != 32 {
		t.Errorf("got %d, want 32", got)
	}
}

// TestExtractALACBitDepth_NonALACReturnsZero — an MP4 whose outer
// sample-entry FourCC is `mp4a` (AAC) must return (0, nil), NOT
// error. The caller short-circuits on bits==0 and leaves
// `t.BitsPerSample` nil, which is the correct AAC contract.
func TestExtractALACBitDepth_NonALACReturnsZero(t *testing.T) {
	mp4 := buildMinimalMP4("mp4a")
	got, err := extractALACBitDepth(bytes.NewReader(mp4))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 0 {
		t.Errorf("got %d, want 0 for non-ALAC sample entry", got)
	}
}

// TestExtractALACBitDepth_MissingInnerAtomReturnsZero — an outer
// `alac` sample entry whose 28-byte audio header is followed by NO
// inner `alac` config atom (atypical / corrupt encoder). The walker
// must return (0, nil) so the caller falls through gracefully
// without erroring. Honest-suppression contract, per PR #376
// precedent.
func TestExtractALACBitDepth_MissingInnerAtomReturnsZero(t *testing.T) {
	mp4 := buildMP4WithALACSampleEntryNoInnerConfig()
	got, err := extractALACBitDepth(bytes.NewReader(mp4))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 0 {
		t.Errorf("got %d, want 0 for missing inner alac config", got)
	}
}

// TestExtractALACBitDepth_TruncatedInnerAtomReturnsZero — inner
// `alac` config atom whose payload is too short to carry the
// ALACSpecificConfig (only 4 bytes of ver+flags, no bitDepth byte).
// Returns (0, nil) — honest suppression rather than erroring.
func TestExtractALACBitDepth_TruncatedInnerAtomReturnsZero(t *testing.T) {
	mp4 := buildMP4WithALACSampleEntryTruncatedInner()
	got, err := extractALACBitDepth(bytes.NewReader(mp4))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 0 {
		t.Errorf("got %d, want 0 for truncated inner alac config", got)
	}
}

// TestExtractALACBitDepth_LargesizeOuterBox — defensive: every box
// in the moov→trak→mdia→minf→stbl descent chain encoded in the
// 64-bit largesize form. Mirrors the codec-walker's
// TestExtractMP4CodecLargesize64Bit regression at each site for the
// bit-depth walker too.
func TestExtractALACBitDepth_LargesizeOuterBox(t *testing.T) {
	for _, which := range []string{"moov", "trak", "mdia", "minf", "stbl"} {
		t.Run(which, func(t *testing.T) {
			mp4 := buildMP4WithALACConfigLargesize(16, which)
			got, err := extractALACBitDepth(bytes.NewReader(mp4))
			if err != nil {
				t.Fatalf("extractALACBitDepth with 64-bit %s: %v", which, err)
			}
			if got != 16 {
				t.Errorf("64-bit %s box: got %d, want 16", which, got)
			}
		})
	}
}

// buildMinimalMP4 returns a byte slice containing the smallest
// MP4-shaped tree the codec walker needs: ftyp + moov containing one
// trak/mdia/minf/stbl/stsd with one sample entry whose format is the
// given codec FourCC. The other fields inside each atom are
// zero-padded — the walker doesn't read them.
func buildMinimalMP4(codec string) []byte {
	return buildMP4WithMoovPadding(codec, 0)
}

// buildMP4WithLargesizeBox returns a minimal MP4 in which exactly
// ONE atom in the moov→trak→mdia→minf→stbl→stsd chain is encoded
// with the 64-bit `largesize` form. `which` selects the box ("moov"
// / "trak" / "mdia" / "minf" / "stbl"). Stsd is skipped (the codec
// walker reads inside it directly; a 64-bit stsd would still parse
// fine because findAtom returns the actual headerSize for the seek
// at line 115, but the test set above doesn't need that case).
//
// Used by the 64-bit largesize regression tests to verify each
// descent in extractMP4Codec correctly skips a 16-byte extended
// header on the targeted box and an 8-byte normal header on the
// other boxes.
func buildMP4WithLargesizeBox(codec, which string) []byte {
	if len(codec) != 4 {
		panic("buildMP4WithLargesizeBox: codec must be 4 chars")
	}

	entry := &bytes.Buffer{}
	binary.Write(entry, binary.BigEndian, uint32(16))
	entry.WriteString(codec)
	entry.Write(make([]byte, 8))

	stsdPayload := &bytes.Buffer{}
	stsdPayload.Write(make([]byte, 4))
	binary.Write(stsdPayload, binary.BigEndian, uint32(1))
	stsdPayload.Write(entry.Bytes())

	// writeWith picks 8-byte vs 16-byte header based on `which`.
	writeWith := func(w *bytes.Buffer, atomType string, payload []byte) {
		if atomType == which {
			writeAtom64(w, atomType, payload)
			return
		}
		writeAtom(w, atomType, payload)
	}

	stbl := &bytes.Buffer{}
	writeAtom(stbl, "stsd", stsdPayload.Bytes())

	minf := &bytes.Buffer{}
	writeWith(minf, "stbl", stbl.Bytes())

	mdia := &bytes.Buffer{}
	writeWith(mdia, "minf", minf.Bytes())

	trak := &bytes.Buffer{}
	writeWith(trak, "mdia", mdia.Bytes())

	moov := &bytes.Buffer{}
	writeWith(moov, "trak", trak.Bytes())

	out := &bytes.Buffer{}
	writeAtom(out, "ftyp", []byte("M4A mp42M4A "))
	writeWith(out, "moov", moov.Bytes())
	return out.Bytes()
}

// buildMP4WithMoovPadding is buildMinimalMP4 with an optional `free`
// padding atom inserted into moov BEFORE the trak. Used to fabricate
// large-moov scenarios for the iteration-budget regression test.
func buildMP4WithMoovPadding(codec string, freePaddingBytes int) []byte {
	if len(codec) != 4 {
		panic("buildMP4WithMoovPadding: codec must be 4 chars")
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
	if freePaddingBytes > 0 {
		// Insert a `free` atom whose payload is `freePaddingBytes` bytes.
		// Atom header (8 bytes) + payload pads moov before the trak,
		// forcing the walker to traverse the padding to find the trak.
		writeAtom(moov, "free", make([]byte, freePaddingBytes))
	}
	writeAtom(moov, "trak", trak.Bytes())

	out := &bytes.Buffer{}
	writeAtom(out, "ftyp", []byte("M4A mp42M4A ")) // 12-byte body, valid-shaped
	writeAtom(out, "moov", moov.Bytes())
	return out.Bytes()
}

// buildALACSampleEntryPayload returns the 28-byte audio sample-entry
// header followed by an inner `alac` config atom carrying the given
// source bit depth in ALACSpecificConfig. Matches the real-file layout
// validated 2026-05-14 against `/Users/arsenie/medialibtest/Amestoy
// Trio/Le Fil/01 Espina.m4a`:
//   - 28-byte AudioSampleEntry header (6 reserved, 2 dri, 8 reserved,
//     2 channels, 2 sample_size, 2 pre_defined, 2 reserved, 4 sr)
//   - inner `alac` atom: 8-byte header + 4-byte FullBox ver+flags +
//     24-byte ALACSpecificConfig
func buildALACSampleEntryPayload(bitDepth byte) []byte {
	out := &bytes.Buffer{}
	// AudioSampleEntry header (28 bytes, zeros are fine for the walker).
	out.Write(make([]byte, 28))

	// Inner `alac` config atom payload: 4-byte FullBox ver+flags,
	// then 24-byte ALACSpecificConfig. Only bitDepth at offset 5
	// matters for the walker; other fields filled with shape-valid
	// defaults so a future read-more-than-bitDepth test stays
	// realistic.
	innerPayload := &bytes.Buffer{}
	innerPayload.Write(make([]byte, 4))                         // version+flags
	binary.Write(innerPayload, binary.BigEndian, uint32(4096))  // frameLength
	innerPayload.WriteByte(0)                                   // compatibleVersion
	innerPayload.WriteByte(bitDepth)                            // BIT_DEPTH ← what we read
	innerPayload.WriteByte(40)                                  // pb
	innerPayload.WriteByte(10)                                  // mb
	innerPayload.WriteByte(14)                                  // kb
	innerPayload.WriteByte(2)                                   // numChannels
	binary.Write(innerPayload, binary.BigEndian, uint16(255))   // maxRun
	binary.Write(innerPayload, binary.BigEndian, uint32(0))     // maxFrameBytes
	binary.Write(innerPayload, binary.BigEndian, uint32(0))     // avgBitRate
	binary.Write(innerPayload, binary.BigEndian, uint32(44100)) // sampleRate

	writeAtom(out, "alac", innerPayload.Bytes())
	return out.Bytes()
}

// buildMP4WithALACConfig builds a minimal MP4 in which the single
// sample entry is an outer `alac` FourCC whose payload carries the
// inner `alac` config atom with the chosen source bit depth. Used by
// the extractALACBitDepth tests above.
func buildMP4WithALACConfig(bitDepth byte) []byte {
	return buildMP4WithSampleEntryPayload("alac", buildALACSampleEntryPayload(bitDepth))
}

// buildMP4WithSampleEntryPayload builds a minimal MP4 tree
// (ftyp + moov/trak/mdia/minf/stbl/stsd) with one sample entry whose
// FourCC is `codec` and whose payload (post-FourCC) is `payload`.
// Generalised form of buildMinimalMP4 — the older helper writes a
// fixed 8-byte payload that suffices for the codec walker but not for
// the bit-depth walker, which needs to read into the sample-entry
// payload itself.
func buildMP4WithSampleEntryPayload(codec string, payload []byte) []byte {
	if len(codec) != 4 {
		panic("buildMP4WithSampleEntryPayload: codec must be 4 chars")
	}

	entry := &bytes.Buffer{}
	binary.Write(entry, binary.BigEndian, uint32(8+len(payload))) // entry size = header + payload
	entry.WriteString(codec)
	entry.Write(payload)

	stsdPayload := &bytes.Buffer{}
	stsdPayload.Write(make([]byte, 4))                     // version+flags
	binary.Write(stsdPayload, binary.BigEndian, uint32(1)) // entry_count
	stsdPayload.Write(entry.Bytes())

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
	writeAtom(out, "ftyp", []byte("M4A mp42M4A "))
	writeAtom(out, "moov", moov.Bytes())
	return out.Bytes()
}

// buildMP4WithALACSampleEntryNoInnerConfig — outer `alac` sample entry
// with the 28-byte audio header but NO inner `alac` config atom. The
// walker must return (0, nil), not error.
func buildMP4WithALACSampleEntryNoInnerConfig() []byte {
	return buildMP4WithSampleEntryPayload("alac", make([]byte, 28))
}

// buildMP4WithALACSampleEntryTruncatedInner — inner `alac` atom whose
// payload is only 4 bytes (FullBox ver+flags) and stops there, with
// no ALACSpecificConfig. The walker must return (0, nil).
func buildMP4WithALACSampleEntryTruncatedInner() []byte {
	out := &bytes.Buffer{}
	out.Write(make([]byte, 28))
	writeAtom(out, "alac", make([]byte, 4)) // inner alac w/ payload < 6
	return buildMP4WithSampleEntryPayload("alac", out.Bytes())
}

// buildMP4WithALACConfigLargesize — variant of buildMP4WithALACConfig
// where exactly one box in the moov→trak→mdia→minf→stbl chain is
// encoded in 64-bit largesize form. Mirrors the codec-walker's
// buildMP4WithLargesizeBox helper for the bit-depth walker.
func buildMP4WithALACConfigLargesize(bitDepth byte, which string) []byte {
	payload := buildALACSampleEntryPayload(bitDepth)

	entry := &bytes.Buffer{}
	binary.Write(entry, binary.BigEndian, uint32(8+len(payload)))
	entry.WriteString("alac")
	entry.Write(payload)

	stsdPayload := &bytes.Buffer{}
	stsdPayload.Write(make([]byte, 4))
	binary.Write(stsdPayload, binary.BigEndian, uint32(1))
	stsdPayload.Write(entry.Bytes())

	writeWith := func(w *bytes.Buffer, atomType string, p []byte) {
		if atomType == which {
			writeAtom64(w, atomType, p)
			return
		}
		writeAtom(w, atomType, p)
	}

	stbl := &bytes.Buffer{}
	writeAtom(stbl, "stsd", stsdPayload.Bytes())
	minf := &bytes.Buffer{}
	writeWith(minf, "stbl", stbl.Bytes())
	mdia := &bytes.Buffer{}
	writeWith(mdia, "minf", minf.Bytes())
	trak := &bytes.Buffer{}
	writeWith(trak, "mdia", mdia.Bytes())
	moov := &bytes.Buffer{}
	writeWith(moov, "trak", trak.Bytes())

	out := &bytes.Buffer{}
	writeAtom(out, "ftyp", []byte("M4A mp42M4A "))
	writeWith(out, "moov", moov.Bytes())
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

// writeAtom64 writes a single MP4 atom in the 64-bit `largesize`
// form: 16-byte header (size==1 sentinel, 4-byte type, uint64 real
// size) + payload. Used by the regression tests below to fabricate
// containers that exercise the extended-header descent path.
func writeAtom64(w *bytes.Buffer, atomType string, payload []byte) {
	if len(atomType) != 4 {
		panic("writeAtom64: type must be 4 chars")
	}
	binary.Write(w, binary.BigEndian, uint32(1)) // size==1 — largesize sentinel
	w.WriteString(atomType)
	realSize := uint64(16 + len(payload)) // 16-byte header + payload
	binary.Write(w, binary.BigEndian, realSize)
	w.Write(payload)
}
