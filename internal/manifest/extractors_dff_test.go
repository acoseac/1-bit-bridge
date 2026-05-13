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
// "DST " for the DST-compressed variant the extractor must reject).
func buildDFF(t *testing.T, sampleRate uint32, compression string) []byte {
	t.Helper()
	if len(compression) != 4 {
		t.Fatalf("compression FOURCC must be 4 bytes, got %q", compression)
	}

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
	binary.BigEndian.PutUint32(rate[:], sampleRate)
	fs = append(fs, rate[:]...)
	prop = append(prop, fs...)

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

	// DSD audio chunk: real files carry the DSD bytes here. We emit a
	// 4-byte stub so the file is parseable but doesn't pretend to
	// hold real audio.
	dsd := []byte{}
	dsd = append(dsd, []byte("DSD ")...)
	var dsdSize [8]byte
	binary.BigEndian.PutUint64(dsdSize[:], 4)
	dsd = append(dsd, dsdSize[:]...)
	dsd = append(dsd, 0x00, 0x00, 0x00, 0x00)

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
	// Any CMPR FOURCC other than "DSD " (uncompressed) — including
	// "DST " AND any future / unknown variant — must leave the DSD
	// stamps nil. Pre-fix, the parser stamped IsDSD/SampleRate as
	// soon as it saw FS and only rolled back for "DST " specifically;
	// a corrupt encoder or future variant was left as playable
	// uncompressed DFF (Greptile + CodeRabbit on PR #186). This test
	// pins the post-fix "default-deny" contract.
	for _, compression := range []string{"DST ", "XXXX", "RLE ", "FUTR"} {
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
		})
	}
}

func TestExtractDFF_DSTCompressionRejection(t *testing.T) {
	// CMPR == "DST " is the lossless-DSD compressed variant. iOS
	// can't decode it; the bridge must surface Codec="DFF" so the
	// quality chip still renders, but leave IsDSD/SampleRate nil
	// so iOS-side surface classifies the row as "an unknown audio
	// file" rather than "a DSD track that fails to load".
	path := writeTempDFF(t, buildDFF(t, 2_822_400, "DST "))
	track := &Track{}
	if err := extractDFFWithContext(path, track, nil); err != nil {
		t.Fatalf("extractDFFWithContext: %v", err)
	}
	if track.Codec != "DFF" {
		t.Errorf("Codec = %q, want %q (codec must stamp regardless of compression)", track.Codec, "DFF")
	}
	if track.IsDSD != nil {
		t.Errorf("IsDSD = %v, want nil (DST must not stamp IsDSD)", *track.IsDSD)
	}
	if track.SampleRate != nil {
		t.Errorf("SampleRate = %v, want nil (DST must not stamp SampleRate)", *track.SampleRate)
	}
	if track.BitsPerSample != nil {
		t.Errorf("BitsPerSample = %v, want nil (DST must not stamp bits)", *track.BitsPerSample)
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
