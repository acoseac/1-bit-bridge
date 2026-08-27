package manifest

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// buildDFF synthesises a minimally-valid DSDIFF file with the FRM8
// outer + a PROP/SND chunk carrying FS (sample rate) and CMPR
// (compression). The DSD audio chunk is emitted as a 1-byte stub so
// the file is a complete, navigable DSDIFF — the extractor doesn't
// read the audio bytes, but a corrupt-by-truncation file would test a
// different code path than what we want to exercise here.
//
// `compression` is the 4-char CMPR FOURCC ("DSD " for uncompressed,
// "DST " for the DST-compressed variant — typed since the
// docs/DSTFeasibility.md §5 reversal).
func buildDFF(t *testing.T, sampleRate uint32, compression string) []byte {
	t.Helper()
	return buildDFFWithOpts(t, dffFixtureOpts{
		sampleRate:  sampleRate,
		compression: compression,
	})
}

// dffFixtureOpts extends the fixture for the DST-typing tests: an
// optional CHNL chunk, an optional declared-DSD-size override (the
// walker SEEKS past the declared size, so a fixture can declare a
// multi-MB payload while writing a stub — the seek lands past EOF and
// the next read terminates the walk cleanly), and an optional `DST `
// sound chunk opening with FRTE (which REPLACES the DSD chunk, the
// real DST file shape).
type dffFixtureOpts struct {
	sampleRate      uint32
	compression     string
	channels        uint16 // 0 = omit CHNL
	declaredDSDSize uint64 // 0 = the stub's real 4 bytes
	dstFrames       uint32 // with dstFrameRate: emit DST/FRTE instead of DSD
	dstFrameRate    uint16
	emitDSTChunk    bool
}

func buildDFFWithOpts(t *testing.T, opts dffFixtureOpts) []byte {
	t.Helper()
	if len(opts.compression) != 4 {
		t.Fatalf("compression FOURCC must be 4 bytes, got %q", opts.compression)
	}
	compression := opts.compression

	// PROP body: "SND " form-type, then FS chunk, then CMPR chunk.
	prop := []byte{}
	prop = append(prop, []byte("SND ")...)

	// FS chunk: 4-byte FOURCC + 8-byte BE size + 4-byte BE rate.
	fs := []byte{}
	fs = append(fs, []byte("FS  ")...)
	var fsSize [8]byte
	binary.BigEndian.PutUint64(fsSize[:], 4)
	fs = append(fs, fsSize[:]...)
	var rate [4]byte
	binary.BigEndian.PutUint32(rate[:], opts.sampleRate)
	fs = append(fs, rate[:]...)
	prop = append(prop, fs...)

	// Optional CHNL chunk: u16 BE count + one 4-byte ID per channel.
	if opts.channels > 0 {
		chnl := []byte{}
		chnl = append(chnl, []byte("CHNL")...)
		var chnlSize [8]byte
		binary.BigEndian.PutUint64(chnlSize[:], uint64(2+4*int(opts.channels)))
		chnl = append(chnl, chnlSize[:]...)
		var count [2]byte
		binary.BigEndian.PutUint16(count[:], opts.channels)
		chnl = append(chnl, count[:]...)
		for i := uint16(0); i < opts.channels; i++ {
			chnl = append(chnl, []byte("SLFT")...)
		}
		prop = append(prop, chnl...)
	}

	// CMPR chunk: 4-byte FOURCC + 8-byte BE size + 4-byte FOURCC + 1
	// byte name length + zero-length name. Real DSDIFF files include
	// a Pascal-style compression-name string; an empty name is valid
	// per the DSDIFF spec and exercises just the FOURCC parse.
	cmpr := []byte{}
	cmpr = append(cmpr, []byte("CMPR")...)
	var cmprSize [8]byte
	binary.BigEndian.PutUint64(cmprSize[:], 5) // 4 FOURCC + 1 name-length
	cmpr = append(cmpr, cmprSize[:]...)
	cmpr = append(cmpr, []byte(compression)...)
	cmpr = append(cmpr, 0x00) // empty compression name
	// Pad to even byte boundary (5 is odd).
	cmpr = append(cmpr, 0x00)
	prop = append(prop, cmpr...)

	// PROP chunk header: "PROP" + size + body.
	propWithHeader := []byte{}
	propWithHeader = append(propWithHeader, []byte("PROP")...)
	var propSize [8]byte
	binary.BigEndian.PutUint64(propSize[:], uint64(len(prop)))
	propWithHeader = append(propWithHeader, propSize[:]...)
	propWithHeader = append(propWithHeader, prop...)

	// Sound chunk. Uncompressed fixtures carry a `DSD ` chunk (a
	// 4-byte stub; `declaredDSDSize` can declare a bigger payload —
	// the walker seeks past the declaration, landing at EOF, which is
	// the clean terminator). DST fixtures carry a `DST ` chunk whose
	// first nested chunk is FRTE, the real shape.
	dsd := []byte{}
	if opts.emitDSTChunk {
		frte := []byte{}
		frte = append(frte, []byte("FRTE")...)
		var frteSize [8]byte
		binary.BigEndian.PutUint64(frteSize[:], 6)
		frte = append(frte, frteSize[:]...)
		var frames [4]byte
		binary.BigEndian.PutUint32(frames[:], opts.dstFrames)
		frte = append(frte, frames[:]...)
		var frameRate [2]byte
		binary.BigEndian.PutUint16(frameRate[:], opts.dstFrameRate)
		frte = append(frte, frameRate[:]...)
		// A stub DSTF marker after FRTE so the chunk resembles a real
		// compressed stream.
		body := append(frte, []byte("DSTF")...)
		dsd = append(dsd, []byte("DST ")...)
		var dstSize [8]byte
		binary.BigEndian.PutUint64(dstSize[:], uint64(len(body)))
		dsd = append(dsd, dstSize[:]...)
		dsd = append(dsd, body...)
		if len(body)%2 == 1 {
			dsd = append(dsd, 0x00)
		}
	} else {
		dsd = append(dsd, []byte("DSD ")...)
		declared := uint64(4)
		if opts.declaredDSDSize > 0 {
			declared = opts.declaredDSDSize
		}
		var dsdSize [8]byte
		binary.BigEndian.PutUint64(dsdSize[:], declared)
		dsd = append(dsd, dsdSize[:]...)
		dsd = append(dsd, 0x00, 0x00, 0x00, 0x00)
	}

	// FRM8 outer: 4 bytes magic + 8 bytes BE size + 4 bytes form
	// type + body. Size covers form-type + body.
	body := []byte{}
	body = append(body, []byte("DSD ")...)
	body = append(body, propWithHeader...)
	body = append(body, dsd...)

	out := []byte{}
	out = append(out, []byte("FRM8")...)
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(body)))
	out = append(out, size[:]...)
	out = append(out, body...)
	return out
}

func writeTempDFF(t *testing.T, contents []byte) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fixture.dff")
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

func TestExtractDFF_PopulatesCodecIsDSDSampleRate(t *testing.T) {
	// 2_822_400 Hz = DSD64. Most common rate; the extractor should
	// stamp Codec="DFF" and IsDSD=true and surface the rate as a
	// float64 pointer.
	path := writeTempDFF(t, buildDFF(t, 2_822_400, "DSD "))
	track := &Track{}
	if err := extractDFFWithContext(path, track, nil); err != nil {
		t.Fatalf("extractDFFWithContext: %v", err)
	}
	if track.Codec != "DFF" {
		t.Errorf("Codec = %q, want %q", track.Codec, "DFF")
	}
	if track.IsDSD == nil || !*track.IsDSD {
		t.Errorf("IsDSD = %v, want non-nil true", track.IsDSD)
	}
	if track.SampleRate == nil || *track.SampleRate != 2_822_400 {
		t.Errorf("SampleRate = %v, want pointer to 2822400", track.SampleRate)
	}
	if track.BitsPerSample == nil || *track.BitsPerSample != 1 {
		t.Errorf("BitsPerSample = %v, want pointer to 1", track.BitsPerSample)
	}
}

func TestExtractDFF_UnknownCompressionRejection(t *testing.T) {
	// Any CMPR FOURCC that is neither "DSD " (uncompressed) nor
	// "DST " (typed since the docs/DSTFeasibility.md §5 reversal —
	// see TestExtractDFF_DSTCompressionTyped) must leave the DSD
	// stamps nil: a corrupt encoder or future variant must classify
	// as unknown audio, never as playable DSD (the PR #186
	// default-deny, now scoped to genuinely-unknown FOURCCs).
	for _, compression := range []string{"XXXX", "RLE ", "FUTR"} {
		t.Run(compression, func(t *testing.T) {
			path := writeTempDFF(t, buildDFF(t, 2_822_400, compression))
			track := &Track{}
			if err := extractDFFWithContext(path, track, nil); err != nil {
				t.Fatalf("extractDFFWithContext: %v", err)
			}
			if track.Codec != "DFF" {
				t.Errorf("Codec = %q, want %q", track.Codec, "DFF")
			}
			if track.IsDSD != nil {
				t.Errorf("CMPR=%q: IsDSD = %v, want nil", compression, *track.IsDSD)
			}
			if track.SampleRate != nil {
				t.Errorf("CMPR=%q: SampleRate = %v, want nil", compression, *track.SampleRate)
			}
			if track.BitsPerSample != nil {
				t.Errorf("CMPR=%q: BitsPerSample = %v, want nil", compression, *track.BitsPerSample)
			}
			if track.Compression != "" {
				t.Errorf("CMPR=%q: Compression = %q, want empty (denied rows carry no wire discriminator)", compression, track.Compression)
			}
			if track.Duration != nil {
				t.Errorf("CMPR=%q: Duration = %v, want nil", compression, *track.Duration)
			}
		})
	}
}

func TestExtractDFF_DSTCompressionTyped(t *testing.T) {
	// CMPR == "DST " (the lossless-DSD SACD compressor) is TYPED since
	// the docs/DSTFeasibility.md §5 reversal: SampleRate + IsDSD +
	// BitsPerSample stamped TOGETHER (the DIDL `<res>` `!IsDSD`
	// co-gate must never observe a half-stamped row — the PR #563
	// renderer silent-decline class) plus the additive
	// `Compression = "DST"` wire discriminator. Codec stays "DFF" —
	// iOS maps the compression field onto its own canonical DST
	// marker at its upsert chokepoint (Mirror-PR pair). This fixture
	// carries a legacy DSD-chunk shape with no FRTE, so Duration
	// stays nil (DST duration comes ONLY from FRTE).
	path := writeTempDFF(t, buildDFF(t, 2_822_400, "DST "))
	track := &Track{}
	if err := extractDFFWithContext(path, track, nil); err != nil {
		t.Fatalf("extractDFFWithContext: %v", err)
	}
	if track.Codec != "DFF" {
		t.Errorf("Codec = %q, want %q (codec must stamp regardless of compression)", track.Codec, "DFF")
	}
	if track.Compression != "DST" {
		t.Errorf("Compression = %q, want %q", track.Compression, "DST")
	}
	if track.IsDSD == nil || !*track.IsDSD {
		t.Errorf("IsDSD = %v, want non-nil true (DST rows are typed DSD)", track.IsDSD)
	}
	if track.SampleRate == nil || *track.SampleRate != 2_822_400 {
		t.Errorf("SampleRate = %v, want pointer to 2822400", track.SampleRate)
	}
	if track.BitsPerSample == nil || *track.BitsPerSample != 1 {
		t.Errorf("BitsPerSample = %v, want pointer to 1 (stamped together with IsDSD)", track.BitsPerSample)
	}
	if track.Duration != nil {
		t.Errorf("Duration = %v, want nil (no FRTE in this fixture)", *track.Duration)
	}
}

func TestExtractDFF_DSTWithFRTE_StampsDurationAndChannels(t *testing.T) {
	// The real DST file shape: PROP carries CHNL + CMPR("DST "), and
	// the `DST ` sound chunk opens with FRTE (numFrames + the fixed
	// 75 frames/s rate) — 4500 frames / 75 = exactly 60 s, computed
	// WITHOUT decoding anything.
	path := writeTempDFF(t, buildDFFWithOpts(t, dffFixtureOpts{
		sampleRate:   2_822_400,
		compression:  "DST ",
		channels:     2,
		emitDSTChunk: true,
		dstFrames:    4500,
		dstFrameRate: 75,
	}))
	track := &Track{}
	if err := extractDFFWithContext(path, track, nil); err != nil {
		t.Fatalf("extractDFFWithContext: %v", err)
	}
	if track.Compression != "DST" {
		t.Errorf("Compression = %q, want %q", track.Compression, "DST")
	}
	if track.Duration == nil || *track.Duration != 60 {
		t.Errorf("Duration = %v, want pointer to 60", track.Duration)
	}
	if track.Channels == nil || *track.Channels != 2 {
		t.Errorf("Channels = %v, want pointer to 2", track.Channels)
	}
	if track.IsDSD == nil || !*track.IsDSD {
		t.Errorf("IsDSD = %v, want non-nil true", track.IsDSD)
	}
}

func TestExtractDFF_DSTZeroFrameRate_NoDuration(t *testing.T) {
	// A forged FRTE with frameRate 0 must not stamp a duration (the
	// float math cannot trap, and the plausibility gate rejects it) —
	// the typing itself still lands.
	path := writeTempDFF(t, buildDFFWithOpts(t, dffFixtureOpts{
		sampleRate:   2_822_400,
		compression:  "DST ",
		channels:     2,
		emitDSTChunk: true,
		dstFrames:    4500,
		dstFrameRate: 0,
	}))
	track := &Track{}
	if err := extractDFFWithContext(path, track, nil); err != nil {
		t.Fatalf("extractDFFWithContext: %v", err)
	}
	if track.Duration != nil {
		t.Errorf("Duration = %v, want nil", *track.Duration)
	}
	if track.Compression != "DST" {
		t.Errorf("Compression = %q, want %q (typing must survive the forged FRTE)", track.Compression, "DST")
	}
}

func TestExtractDFF_Uncompressed_StampsDurationFromDSDSize(t *testing.T) {
	// Net-new for ALL DFF (the extractor never computed duration
	// before): DSD64 stereo with a declared 42_336_000-byte payload =
	// 2_822_400/8 bytes-per-channel-per-second × 2 ch × 60 s → 60 s.
	// The fixture is SPARSE-extended to cover the declared payload
	// (os.Truncate — APFS holes cost nothing) so the physical-size
	// truncation bound admits the duration; the walker seeks through
	// the hole and terminates at the real EOF.
	fixture := buildDFFWithOpts(t, dffFixtureOpts{
		sampleRate:      2_822_400,
		compression:     "DSD ",
		channels:        2,
		declaredDSDSize: 42_336_000,
	})
	path := writeTempDFF(t, fixture)
	if err := os.Truncate(path, int64(len(fixture))+42_336_000); err != nil {
		t.Fatalf("sparse-extend fixture: %v", err)
	}
	track := &Track{}
	if err := extractDFFWithContext(path, track, nil); err != nil {
		t.Fatalf("extractDFFWithContext: %v", err)
	}
	if track.Duration == nil || *track.Duration != 60 {
		t.Errorf("Duration = %v, want pointer to 60", track.Duration)
	}
	if track.Channels == nil || *track.Channels != 2 {
		t.Errorf("Channels = %v, want pointer to 2", track.Channels)
	}
	if track.Compression != "" {
		t.Errorf("Compression = %q, want empty for uncompressed", track.Compression)
	}
}

func TestExtractDFF_TruncatedFile_NoDurationForMissingAudio(t *testing.T) {
	// A truncated file whose DSD chunk declares more payload than the
	// file physically holds must not stamp a duration for audio that
	// doesn't exist — `Seek` lands past EOF without error, so the
	// declared size alone can't be trusted (CodeRabbit on this PR;
	// the iOS DFFHeadScan applies the same bound). Typing still lands.
	path := writeTempDFF(t, buildDFFWithOpts(t, dffFixtureOpts{
		sampleRate:      2_822_400,
		compression:     "DSD ",
		channels:        2,
		declaredDSDSize: 42_336_000, // file is only a few hundred bytes
	}))
	track := &Track{}
	if err := extractDFFWithContext(path, track, nil); err != nil {
		t.Fatalf("extractDFFWithContext: %v", err)
	}
	if track.Duration != nil {
		t.Errorf("Duration = %v, want nil (declared payload exceeds the file)", *track.Duration)
	}
	if track.IsDSD == nil || !*track.IsDSD {
		t.Errorf("IsDSD = %v, want non-nil true (typing must survive truncation)", track.IsDSD)
	}
}

func TestExtractDFF_BoundAfterPayloadStartButBeforeEnd_NoDuration(t *testing.T) {
	// CodeRabbit round 2: the fit check must be `payload offset +
	// declared size <= physical size`, not size alone — a file
	// truncated AFTER the payload start but BEFORE its declared end
	// passes the size-only compare (declared 42_336_000 <= physical
	// 42_336_010) while the payload's END exceeds the file by roughly
	// the header length. The iOS DFFHeadScan applies the identical
	// offset-aware rule (Mirror parity). Typing still lands.
	fixture := buildDFFWithOpts(t, dffFixtureOpts{
		sampleRate:      2_822_400,
		compression:     "DSD ",
		channels:        2,
		declaredDSDSize: 42_336_000,
	})
	path := writeTempDFF(t, fixture)
	if err := os.Truncate(path, 42_336_010); err != nil {
		t.Fatalf("sparse-extend fixture: %v", err)
	}
	track := &Track{}
	if err := extractDFFWithContext(path, track, nil); err != nil {
		t.Fatalf("extractDFFWithContext: %v", err)
	}
	if track.Duration != nil {
		t.Errorf("Duration = %v, want nil (payload END exceeds the file)", *track.Duration)
	}
	if track.IsDSD == nil || !*track.IsDSD {
		t.Errorf("IsDSD = %v, want non-nil true (typing must survive the failed fit)", track.IsDSD)
	}
}

func TestExtractDFF_UncompressedWithoutCHNL_NoDuration(t *testing.T) {
	// Duration needs the channel count; typing doesn't. A legacy
	// CHNL-less fixture keeps the format stamps and omits Duration —
	// the pre-DST behaviour plus nothing.
	path := writeTempDFF(t, buildDFF(t, 2_822_400, "DSD "))
	track := &Track{}
	if err := extractDFFWithContext(path, track, nil); err != nil {
		t.Fatalf("extractDFFWithContext: %v", err)
	}
	if track.IsDSD == nil || !*track.IsDSD {
		t.Errorf("IsDSD = %v, want non-nil true", track.IsDSD)
	}
	if track.Duration != nil {
		t.Errorf("Duration = %v, want nil (no CHNL in this fixture)", *track.Duration)
	}
	if track.Channels != nil {
		t.Errorf("Channels = %v, want nil", *track.Channels)
	}
}

func TestExtractDFF_BadFRM8Magic(t *testing.T) {
	// A non-DFF file at a .dff extension must return an error so
	// the scanner logs the issue rather than silently mis-stamping.
	path := writeTempDFF(t, []byte("NOT A DFF FILE PROBABLY MP3 OR JUNK"))
	track := &Track{}
	err := extractDFFWithContext(path, track, nil)
	if err == nil {
		t.Fatalf("extractDFFWithContext: want error on bad magic, got nil")
	}
	// Codec is still stamped at the top of the function (extension-
	// derived) so the manifest row at least classifies as DFF.
	if track.Codec != "DFF" {
		t.Errorf("Codec = %q, want %q (codec stamps before magic check)", track.Codec, "DFF")
	}
}

func TestExtractDFF_NotDSDFormType(t *testing.T) {
	// FRM8 magic but form-type != "DSD " (e.g. AIFF would be "AIFF",
	// some other IFF dialect). Must error rather than try to parse
	// alien chunks.
	bad := []byte{}
	bad = append(bad, []byte("FRM8")...)
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], 4)
	bad = append(bad, size[:]...)
	bad = append(bad, []byte("AIFF")...)
	path := writeTempDFF(t, bad)
	track := &Track{}
	err := extractDFFWithContext(path, track, nil)
	if err == nil {
		t.Fatalf("extractDFFWithContext: want error on non-DSD form type, got nil")
	}
}

func TestExtractDFF_CodecStampsBeforeOpen(t *testing.T) {
	// File-not-found case: the function should still return an
	// open-side error AND have stamped Codec="DFF" beforehand. (The
	// stamp-before-open ordering is what makes the scanner robust
	// to unreadable files showing up in the manifest with a usable
	// codec hint.)
	track := &Track{}
	err := extractDFFWithContext("/definitely/not/a/path/fixture.dff", track, nil)
	if err == nil {
		t.Fatalf("extractDFFWithContext: want error on missing file, got nil")
	}
	if track.Codec != "DFF" {
		t.Errorf("Codec = %q, want %q (must stamp before open attempt)", track.Codec, "DFF")
	}
}

// TestExtractDFF_PROPShortPayloadKeepsCursorAligned regression-tests
// the CodeRabbit Critical post-merge finding on PR #223: a PROP
// chunk with `size < 4` (too short to hold even the "SND " form-
// type) previously bare-`continue`'d without skipping the
// payload, leaving the cursor inside the PROP body and corrupting
// every subsequent chunk-header read. The fix routes the short-
// size branch through safeSeekSkip + seekPastChunk so the walker
// stays aligned, and a trailing DIIN chunk with title metadata
// is recovered correctly.
func TestExtractDFF_PROPShortPayloadKeepsCursorAligned(t *testing.T) {
	// Hand-craft: FRM8 + "DSD " form-type + PROP with size=3 (3
	// bytes of junk, odd → 1 pad byte) + DSD audio stub + DIIN
	// with a DITI title. Without the fix, the post-PROP cursor
	// would be 16 bytes off (12 chunk-header reread + 4 misread
	// payload bytes), so the DSD-chunk header would be misread
	// and DIIN never found.
	body := []byte("DSD ") // FRM8 form type

	// Malformed PROP: size=3, three junk bytes + 1 pad.
	body = append(body, []byte("PROP")...)
	var propSize [8]byte
	binary.BigEndian.PutUint64(propSize[:], 3)
	body = append(body, propSize[:]...)
	body = append(body, 0xAA, 0xBB, 0xCC, 0x00) // 3 junk + 1 pad

	// Valid DSD audio chunk (1-byte stub + 1 pad).
	body = append(body, []byte("DSD ")...)
	var dsdSize [8]byte
	binary.BigEndian.PutUint64(dsdSize[:], 1)
	body = append(body, dsdSize[:]...)
	body = append(body, 0x00, 0x00) // 1 byte + 1 pad

	// DIIN with a DITI title — only reachable if the walker stays
	// aligned after the malformed PROP.
	diin := buildDIINContainer(buildDIINSubChunk("DITI", "RecoveredAfterBadPROP"))
	body = append(body, diin...)

	out := []byte("FRM8")
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(body)))
	out = append(out, size[:]...)
	out = append(out, body...)

	path := writeTempDFF(t, out)
	track := &Track{}
	if err := extractDFFWithContext(path, track, nil); err != nil {
		t.Fatalf("extractDFFWithContext: %v", err)
	}
	if track.Title != "RecoveredAfterBadPROP" {
		t.Errorf("Title = %q, want %q (PROP short-payload mis-aligned walker?)",
			track.Title, "RecoveredAfterBadPROP")
	}
}

// TestExtractDFF_PROPNonSNDFormTypeKeepsCursorAligned regression-
// tests the second half of the PR #223 Critical finding: a PROP
// chunk with an odd size AND a non-"SND " form-type previously
// bare-`continue`'d without consuming the pad byte, shifting
// every subsequent chunk header by one byte. The fix consumes
// the pad before continuing.
func TestExtractDFF_PROPNonSNDFormTypeKeepsCursorAligned(t *testing.T) {
	body := []byte("DSD ")

	// PROP with non-"SND " form-type AND odd size (5 bytes — 4
	// form-type bytes + 1 junk). Pre-fix the walker would skip
	// past 5 bytes of body content but leave the 1 pad byte
	// unread, shifting alignment by 1.
	body = append(body, []byte("PROP")...)
	var propSize [8]byte
	binary.BigEndian.PutUint64(propSize[:], 5)
	body = append(body, propSize[:]...)
	body = append(body, []byte("WRNG")...) // not "SND "
	body = append(body, 0xDD)              // 1 junk byte
	body = append(body, 0x00)              // pad byte (1+5=6 even when wrapped — actually 5 is odd so pad needed)

	body = append(body, []byte("DSD ")...)
	var dsdSize [8]byte
	binary.BigEndian.PutUint64(dsdSize[:], 1)
	body = append(body, dsdSize[:]...)
	body = append(body, 0x00, 0x00)

	diin := buildDIINContainer(buildDIINSubChunk("DITI", "RecoveredAfterNonSNDPROP"))
	body = append(body, diin...)

	out := []byte("FRM8")
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(body)))
	out = append(out, size[:]...)
	out = append(out, body...)

	path := writeTempDFF(t, out)
	track := &Track{}
	if err := extractDFFWithContext(path, track, nil); err != nil {
		t.Fatalf("extractDFFWithContext: %v", err)
	}
	if track.Title != "RecoveredAfterNonSNDPROP" {
		t.Errorf("Title = %q, want %q (PROP non-SND pad mis-handled?)",
			track.Title, "RecoveredAfterNonSNDPROP")
	}
}
