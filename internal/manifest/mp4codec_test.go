package manifest

import (
	"bytes"
	"encoding/binary"
	"errors"
	"strconv"
	"strings"
	"testing"
)

// failingReadSeeker is a deterministic I/O failure source for the
// hard-error-propagation test below. Both Read and Seek return a
// sentinel error so the test can assert it propagates through
// extractALACBitDepth without being swallowed by the
// errMP4StructureNotFound short-circuit.
type failingReadSeeker struct {
	err error
}

func (f failingReadSeeker) Read(p []byte) (int, error)     { return 0, f.err }
func (f failingReadSeeker) Seek(int64, int) (int64, error) { return 0, f.err }

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

// TestExtractALACBitDepth_PropagatesIOFailure — locks the contract
// that genuine I/O failures (Seek / Read / atom-walk budget
// exhaustion) surface as non-nil errors instead of being swallowed
// by the errMP4StructureNotFound short-circuit. A regression that
// broadened the short-circuit to suppress ALL findSTSD errors
// (round-1 of this PR) would mask real disk / NAS faults; the
// caller's `scanLogger.Warn` would never fire on legitimate
// problems. Per CodeRabbit Trivial round-2 on PR #237.
func TestExtractALACBitDepth_PropagatesIOFailure(t *testing.T) {
	sentinel := errors.New("simulated NAS read failure")
	_, err := extractALACBitDepth(failingReadSeeker{err: sentinel})
	if err == nil {
		t.Fatal("expected I/O error to propagate, got nil")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("expected error to wrap sentinel %v, got %v", sentinel, err)
	}
}

// TestExtractALACBitDepth_SuppressesStructuralNotFound — symmetric
// contract: a non-MP4 file (the sentinel hits during structural
// walk, not I/O) is honestly suppressed so the caller doesn't log
// Warn for "this isn't ALAC anyway". An empty-bytes reader walks
// fine but finds no moov → errMP4StructureNotFound → swallowed.
func TestExtractALACBitDepth_SuppressesStructuralNotFound(t *testing.T) {
	// Empty 8-byte non-MP4 input — Seek/Read succeed but findAtom
	// returns size==0 for "moov", which findSTSD wraps as
	// errMP4StructureNotFound.
	got, err := extractALACBitDepth(bytes.NewReader(make([]byte, 8)))
	if err != nil {
		t.Errorf("unexpected error for structural-not-found case: %v", err)
	}
	if got != 0 {
		t.Errorf("got %d, want 0 for non-MP4 input", got)
	}
}

// TestExtractALACBitDepth_OversizedEntrySizeIsClampedToStsd — a
// malformed sample entry whose declared `entrySize` extends beyond
// the enclosing stsd box could otherwise let the inner walker scan
// adjacent mp4 boxes (stts, stsc, stsz, …) and false-positive on any
// 4-byte stretch spelling "alac". The bounds clamp returns honest 0
// (CodeRabbit Major on PR #237).
func TestExtractALACBitDepth_OversizedEntrySizeIsClampedToStsd(t *testing.T) {
	mp4 := buildMP4WithOversizedALACEntry()
	got, err := extractALACBitDepth(bytes.NewReader(mp4))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 0 {
		t.Errorf("got %d, want 0 (entry claims to extend past stsd — must suppress)", got)
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

// TestExtractMP4SampleRate_ALACFromConfig — ALAC reads the authoritative
// sample rate from ALACSpecificConfig, including hi-res rates the
// AudioSampleEntry 16.16 field can't hold. The 16.16 header field is
// zero in the fixture, so a passing hi-res case proves the config
// override fires.
func TestExtractMP4SampleRate_ALACFromConfig(t *testing.T) {
	for _, rate := range []uint32{44100, 48000, 88200, 96000, 176400, 192000, 352800} {
		t.Run(strconv.Itoa(int(rate)), func(t *testing.T) {
			mp4 := buildMP4WithALACConfigRate(24, rate)
			got, err := extractMP4SampleRate(bytes.NewReader(mp4))
			if err != nil {
				t.Fatalf("extractMP4SampleRate: %v", err)
			}
			if got != float64(rate) {
				t.Errorf("got %v, want %d", got, rate)
			}
		})
	}
}

// TestExtractMP4SampleRate_AACFromAudioSampleEntry — AAC (`mp4a`) has no
// inner config the bridge reads, so the rate comes from the
// AudioSampleEntry 16.16 fixed-point field.
func TestExtractMP4SampleRate_AACFromAudioSampleEntry(t *testing.T) {
	for _, rate := range []uint32{44100, 48000} {
		t.Run(strconv.Itoa(int(rate)), func(t *testing.T) {
			mp4 := buildMP4WithAAC(rate)
			got, err := extractMP4SampleRate(bytes.NewReader(mp4))
			if err != nil {
				t.Fatalf("extractMP4SampleRate: %v", err)
			}
			if got != float64(rate) {
				t.Errorf("got %v, want %d", got, rate)
			}
		})
	}
}

// TestExtractMP4SampleRate_SuppressesStructuralNotFound — a non-MP4
// input returns (0, nil) so the caller leaves SampleRate nil rather than
// failing the scan (mirrors the bit-depth walker's honest-suppression).
func TestExtractMP4SampleRate_SuppressesStructuralNotFound(t *testing.T) {
	got, err := extractMP4SampleRate(bytes.NewReader(make([]byte, 8)))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 0 {
		t.Errorf("got %v, want 0 for non-MP4 input", got)
	}
}

// TestExtractMP4SampleRate_PropagatesIOFailure — genuine I/O failures
// surface as errors, not silent 0, so the caller's scanLogger.Warn fires
// on real disk faults.
func TestExtractMP4SampleRate_PropagatesIOFailure(t *testing.T) {
	sentinel := errors.New("simulated NAS read failure")
	_, err := extractMP4SampleRate(failingReadSeeker{err: sentinel})
	if err == nil {
		t.Fatal("expected I/O error to propagate, got nil")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("expected error to wrap sentinel %v, got %v", sentinel, err)
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
	return buildALACSampleEntryPayloadRate(bitDepth, 44100)
}

// buildALACSampleEntryPayloadRate is buildALACSampleEntryPayload with a
// parameterised ALACSpecificConfig sample rate so the hi-res sample-rate
// path (config rate overriding the AudioSampleEntry 16.16 field) can be
// exercised. The 28-byte AudioSampleEntry header stays all-zero, so the
// 16.16 rate reads as 0 — exactly the hi-res scenario where the config
// rate is authoritative.
func buildALACSampleEntryPayloadRate(bitDepth byte, sampleRate uint32) []byte {
	out := &bytes.Buffer{}
	// AudioSampleEntry header (28 bytes, zeros are fine for the walker).
	out.Write(make([]byte, 28))

	// Inner `alac` config atom payload: 4-byte FullBox ver+flags,
	// then 24-byte ALACSpecificConfig. bitDepth at offset 5 and
	// sampleRate at offset 20 are what the walkers read; other fields
	// filled with shape-valid defaults.
	innerPayload := &bytes.Buffer{}
	innerPayload.Write(make([]byte, 4))                        // version+flags
	binary.Write(innerPayload, binary.BigEndian, uint32(4096)) // frameLength
	innerPayload.WriteByte(0)                                  // compatibleVersion
	innerPayload.WriteByte(bitDepth)                           // BIT_DEPTH ← what we read
	innerPayload.WriteByte(40)                                 // pb
	innerPayload.WriteByte(10)                                 // mb
	innerPayload.WriteByte(14)                                 // kb
	innerPayload.WriteByte(2)                                  // numChannels
	binary.Write(innerPayload, binary.BigEndian, uint16(255))  // maxRun
	binary.Write(innerPayload, binary.BigEndian, uint32(0))    // maxFrameBytes
	binary.Write(innerPayload, binary.BigEndian, uint32(0))    // avgBitRate
	binary.Write(innerPayload, binary.BigEndian, sampleRate)   // sampleRate ← what we read

	writeAtom(out, "alac", innerPayload.Bytes())
	return out.Bytes()
}

// buildMP4WithALACConfigRate builds a minimal MP4 whose ALAC config
// declares the given bit depth + sample rate. Used by the sample-rate
// walker's hi-res tests.
func buildMP4WithALACConfigRate(bitDepth byte, sampleRate uint32) []byte {
	return buildMP4WithSampleEntryPayload("alac", buildALACSampleEntryPayloadRate(bitDepth, sampleRate))
}

// buildAACSampleEntryPayload returns a 28-byte version-0 AudioSampleEntry
// body whose 16.16 fixed-point sampleRate field (offset 24) encodes the
// given rate. `mp4a` (AAC) carries no inner config the bridge reads, so
// this AudioSampleEntry field is the sample-rate source. Rates must be
// ≤ 65535 Hz (the 16.16 integer-part limit) — true for all real AAC.
func buildAACSampleEntryPayload(sampleRate uint32) []byte {
	body := make([]byte, 28)
	binary.BigEndian.PutUint32(body[24:28], sampleRate<<16)
	return body
}

// buildMP4WithAAC builds a minimal MP4 with an `mp4a` sample entry whose
// AudioSampleEntry declares the given sample rate.
func buildMP4WithAAC(sampleRate uint32) []byte {
	return buildMP4WithSampleEntryPayload("mp4a", buildAACSampleEntryPayload(sampleRate))
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

// buildMP4WithOversizedALACEntry — a minimal MP4 whose outer `alac`
// sample-entry box claims `entrySize == 0xFFFFFFFF` (or any value
// exceeding the enclosing stsd size). The actual payload is a
// well-formed 16-bit ALAC config; if the extractor failed to clamp
// `innerSearchEnd` to `stsdEnd`, it would either walk past stsd into
// unrelated atoms OR find the legitimate inner alac and return 16 —
// wrong behaviour either way. With the clamp, returns 0.
func buildMP4WithOversizedALACEntry() []byte {
	payload := buildALACSampleEntryPayload(16)

	// Construct the sample entry with a deliberately-too-large
	// `size` field. The real payload bytes are still well-formed; only
	// the declared size lies.
	entry := &bytes.Buffer{}
	binary.Write(entry, binary.BigEndian, uint32(0xFFFFFFFF)) // oversized
	entry.WriteString("alac")
	entry.Write(payload)

	stsdPayload := &bytes.Buffer{}
	stsdPayload.Write(make([]byte, 4))
	binary.Write(stsdPayload, binary.BigEndian, uint32(1))
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
