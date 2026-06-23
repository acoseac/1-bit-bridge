package manifest

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// buildAIFFWithID3 synthesises a minimal AIFF FORM carrying an
// embedded "ID3 " sub-chunk with the supplied id3 bytes. Used to
// verify the extractor walks the FORM tree, finds the ID3 chunk,
// and feeds it to dhowden's ReadID3v2Tags.
//
// Layout: "FORM" + BE32 form-size + "AIFF" + sub-chunks (where each
// sub-chunk is FOURCC + BE32 size + payload + pad-if-odd).
func buildAIFFWithID3(t *testing.T, id3 []byte, extraChunks ...[]byte) []byte {
	t.Helper()
	body := []byte("AIFF")

	// Optional "fake COMM" sub-chunk to mirror real-world AIFF files
	// (FORM/AIFF requires a COMM chunk per spec, but our extractor
	// doesn't care — fixtures still parse without it). Caller can
	// pass extra chunks via extraChunks if needed.
	for _, c := range extraChunks {
		body = append(body, c...)
	}

	if len(id3) > 0 {
		var sub [8]byte
		copy(sub[0:4], []byte("ID3 "))
		binary.BigEndian.PutUint32(sub[4:8], uint32(len(id3)))
		body = append(body, sub[:]...)
		body = append(body, id3...)
		if len(id3)%2 == 1 {
			body = append(body, 0x00)
		}
	}

	out := []byte("FORM")
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], uint32(len(body)))
	out = append(out, size[:]...)
	out = append(out, body...)
	return out
}

func writeTempAIFF(t *testing.T, contents []byte) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fixture.aiff")
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

// buildWAVWithID3 synthesises a minimal RIFF/WAVE carrying an
// embedded "id3 " (lowercase, per ID3v2-in-RIFF spec) sub-chunk.
// Layout: "RIFF" + LE32 form-size + "WAVE" + sub-chunks (where each
// sub-chunk is FOURCC + LE32 size + payload + pad-if-odd).
func buildWAVWithID3(t *testing.T, id3 []byte, extraChunks ...[]byte) []byte {
	t.Helper()
	body := []byte("WAVE")
	for _, c := range extraChunks {
		body = append(body, c...)
	}
	if len(id3) > 0 {
		var sub [8]byte
		copy(sub[0:4], []byte("id3 "))
		binary.LittleEndian.PutUint32(sub[4:8], uint32(len(id3)))
		body = append(body, sub[:]...)
		body = append(body, id3...)
		if len(id3)%2 == 1 {
			body = append(body, 0x00)
		}
	}
	out := []byte("RIFF")
	var size [4]byte
	binary.LittleEndian.PutUint32(size[:], uint32(len(body)))
	out = append(out, size[:]...)
	out = append(out, body...)
	return out
}

// buildWAVWithListInfo synthesises a minimal RIFF/WAVE carrying a
// LIST/INFO sub-chunk with the supplied INFO sub-chunks. Used to
// verify the LIST/INFO branch surfaces INAM/IART/IPRD/IGNR.
func buildWAVWithListInfo(t *testing.T, info map[string]string) []byte {
	t.Helper()
	infoBody := []byte("INFO")
	for id, text := range info {
		if len(id) != 4 {
			t.Fatalf("INFO sub-chunk ID must be 4 chars, got %q", id)
		}
		textBytes := append([]byte(text), 0x00) // null-terminate
		infoBody = append(infoBody, []byte(id)...)
		var sz [4]byte
		binary.LittleEndian.PutUint32(sz[:], uint32(len(textBytes)))
		infoBody = append(infoBody, sz[:]...)
		infoBody = append(infoBody, textBytes...)
		if len(textBytes)%2 == 1 {
			infoBody = append(infoBody, 0x00)
		}
	}

	body := []byte("WAVE")
	var sub [8]byte
	copy(sub[0:4], []byte("LIST"))
	binary.LittleEndian.PutUint32(sub[4:8], uint32(len(infoBody)))
	body = append(body, sub[:]...)
	body = append(body, infoBody...)
	if len(infoBody)%2 == 1 {
		body = append(body, 0x00)
	}

	out := []byte("RIFF")
	var size [4]byte
	binary.LittleEndian.PutUint32(size[:], uint32(len(body)))
	out = append(out, size[:]...)
	out = append(out, body...)
	return out
}

func writeTempWAV(t *testing.T, contents []byte) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fixture.wav")
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

func TestExtractAIFF_ID3TagsSurfaceViaDhowden(t *testing.T) {
	id3 := buildID3v2_3(map[string]string{
		"title":  "Aria",
		"artist": "Glenn Gould",
		"album":  "Goldberg Variations",
	})
	path := writeTempAIFF(t, buildAIFFWithID3(t, id3))

	track := &Track{}
	if err := extractAIFFWithContext(path, track, nil); err != nil {
		t.Fatalf("extractAIFFWithContext: %v", err)
	}
	if track.Codec != "AIFF" {
		t.Errorf("Codec = %q, want %q", track.Codec, "AIFF")
	}
	if track.Title != "Aria" {
		t.Errorf("Title = %q, want %q", track.Title, "Aria")
	}
	if track.Artist != "Glenn Gould" {
		t.Errorf("Artist = %q, want %q", track.Artist, "Glenn Gould")
	}
	if track.Album != "Goldberg Variations" {
		t.Errorf("Album = %q, want %q", track.Album, "Goldberg Variations")
	}
}

func TestExtractAIFF_NoID3StillStampsCodec(t *testing.T) {
	// AIFF without embedded ID3 — extractor must not error, must
	// still stamp Codec, and must leave tag fields untouched.
	path := writeTempAIFF(t, buildAIFFWithID3(t, nil))
	track := &Track{}
	if err := extractAIFFWithContext(path, track, nil); err != nil {
		t.Fatalf("extractAIFFWithContext: %v", err)
	}
	if track.Codec != "AIFF" {
		t.Errorf("Codec = %q, want %q", track.Codec, "AIFF")
	}
	if track.Title != "" || track.Artist != "" {
		t.Errorf("expected empty tag fields, got title=%q artist=%q", track.Title, track.Artist)
	}
}

func TestExtractAIFF_BadFORMMagic(t *testing.T) {
	path := writeTempAIFF(t, []byte("NOTAIFF junk data here"))
	track := &Track{}
	err := extractAIFFWithContext(path, track, nil)
	if err == nil {
		t.Fatalf("extractAIFFWithContext: want error on bad magic, got nil")
	}
	if track.Codec != "AIFF" {
		t.Errorf("Codec = %q, want %q (must stamp before magic check)", track.Codec, "AIFF")
	}
}

func TestExtractAIFF_AIFCFormTypeAccepted(t *testing.T) {
	// AIFC is the compressed AIFF variant; same chunk-walker shape.
	// Manually rebuild the outer wrap with form-type "AIFC".
	id3 := buildID3v2_3(map[string]string{"title": "Compressed"})
	body := []byte("AIFC")
	var sub [8]byte
	copy(sub[0:4], []byte("ID3 "))
	binary.BigEndian.PutUint32(sub[4:8], uint32(len(id3)))
	body = append(body, sub[:]...)
	body = append(body, id3...)
	if len(id3)%2 == 1 {
		body = append(body, 0x00)
	}
	out := []byte("FORM")
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], uint32(len(body)))
	out = append(out, size[:]...)
	out = append(out, body...)

	path := writeTempAIFF(t, out)
	track := &Track{}
	if err := extractAIFFWithContext(path, track, nil); err != nil {
		t.Fatalf("extractAIFFWithContext: %v", err)
	}
	if track.Title != "Compressed" {
		t.Errorf("Title = %q, want %q", track.Title, "Compressed")
	}
}

func TestExtractWAV_ID3TagsSurfaceViaDhowden(t *testing.T) {
	id3 := buildID3v2_3(map[string]string{
		"title":  "Take Five",
		"artist": "Dave Brubeck",
		"album":  "Time Out",
	})
	path := writeTempWAV(t, buildWAVWithID3(t, id3))

	track := &Track{}
	if err := extractWAVWithContext(path, track, nil); err != nil {
		t.Fatalf("extractWAVWithContext: %v", err)
	}
	if track.Codec != "WAV" {
		t.Errorf("Codec = %q, want %q", track.Codec, "WAV")
	}
	if track.Title != "Take Five" {
		t.Errorf("Title = %q, want %q", track.Title, "Take Five")
	}
	if track.Artist != "Dave Brubeck" {
		t.Errorf("Artist = %q, want %q", track.Artist, "Dave Brubeck")
	}
	if track.Album != "Time Out" {
		t.Errorf("Album = %q, want %q", track.Album, "Time Out")
	}
}

func TestExtractWAV_ListInfoBlockSurfacesFields(t *testing.T) {
	// LIST/INFO chunk only — no ID3. Expect INAM/IART/IPRD/IGNR
	// to surface as Title/Artist/Album/Genre.
	path := writeTempWAV(t, buildWAVWithListInfo(t, map[string]string{
		"INAM": "Birdland",
		"IART": "Weather Report",
		"IPRD": "Heavy Weather",
		"IGNR": "Jazz Fusion",
	}))

	track := &Track{}
	if err := extractWAVWithContext(path, track, nil); err != nil {
		t.Fatalf("extractWAVWithContext: %v", err)
	}
	if track.Title != "Birdland" {
		t.Errorf("Title = %q, want %q", track.Title, "Birdland")
	}
	if track.Artist != "Weather Report" {
		t.Errorf("Artist = %q, want %q", track.Artist, "Weather Report")
	}
	if track.Album != "Heavy Weather" {
		t.Errorf("Album = %q, want %q", track.Album, "Heavy Weather")
	}
	if track.Genre != "Jazz Fusion" {
		t.Errorf("Genre = %q, want %q", track.Genre, "Jazz Fusion")
	}
}

// TestExtractWAV_INFOInteriorNullTruncatedAtFirstNul pins the C-string
// truncation fix: a RIFF INFO value with junk AFTER its terminating NUL
// (e.g. ['H','i',0x00,0xAA,0xBB]) must be cut at the FIRST NUL. The
// pre-fix TrimRight("\x00") only stripped a trailing run of NULs and
// would leave the interior 0xAA 0xBB embedded in the Track field,
// corrupting downstream JSON / iOS rendering. (r1 review fix.)
func TestExtractWAV_INFOInteriorNullTruncatedAtFirstNul(t *testing.T) {
	infoBody := []byte("INFO")
	payload := []byte{'H', 'i', 0x00, 0xAA, 0xBB} // "Hi" + NUL + junk
	infoBody = append(infoBody, []byte("INAM")...)
	var sz [4]byte
	binary.LittleEndian.PutUint32(sz[:], uint32(len(payload)))
	infoBody = append(infoBody, sz[:]...)
	infoBody = append(infoBody, payload...)
	if len(payload)%2 == 1 {
		infoBody = append(infoBody, 0x00) // IFF odd-byte chunk pad
	}

	body := []byte("WAVE")
	var listSub [8]byte
	copy(listSub[0:4], []byte("LIST"))
	binary.LittleEndian.PutUint32(listSub[4:8], uint32(len(infoBody)))
	body = append(body, listSub[:]...)
	body = append(body, infoBody...)

	out := []byte("RIFF")
	var rsz [4]byte
	binary.LittleEndian.PutUint32(rsz[:], uint32(len(body)))
	out = append(out, rsz[:]...)
	out = append(out, body...)

	path := writeTempWAV(t, out)
	track := &Track{}
	if err := extractWAVWithContext(path, track, nil); err != nil {
		t.Fatalf("extractWAVWithContext: %v", err)
	}
	if track.Title != "Hi" {
		t.Errorf("Title = %q (% x), want %q — must truncate at first NUL, not keep interior junk", track.Title, track.Title, "Hi")
	}
}

func TestExtractWAV_ID3WinsOverLISTInfo(t *testing.T) {
	// Both ID3 and LIST/INFO present — ID3 fields must NOT be
	// overwritten by LIST/INFO. populateFromTagMetadata's empty-
	// field guards (inside parseWAVINFOBlock) enforce this.
	id3 := buildID3v2_3(map[string]string{"title": "FromID3"})

	// Build the file manually so ID3 lands BEFORE LIST/INFO in the
	// chunk stream — order doesn't actually matter for correctness
	// (both branches use empty-field guards), but pinning the order
	// makes the test intent explicit.
	body := []byte("WAVE")
	var sub [8]byte
	copy(sub[0:4], []byte("id3 "))
	binary.LittleEndian.PutUint32(sub[4:8], uint32(len(id3)))
	body = append(body, sub[:]...)
	body = append(body, id3...)
	if len(id3)%2 == 1 {
		body = append(body, 0x00)
	}

	// LIST/INFO with a competing INAM that must NOT win.
	infoBody := []byte("INFO")
	competingTitle := append([]byte("FromLIST"), 0x00)
	infoBody = append(infoBody, []byte("INAM")...)
	var titleSz [4]byte
	binary.LittleEndian.PutUint32(titleSz[:], uint32(len(competingTitle)))
	infoBody = append(infoBody, titleSz[:]...)
	infoBody = append(infoBody, competingTitle...)
	if len(competingTitle)%2 == 1 {
		infoBody = append(infoBody, 0x00)
	}

	var listSub [8]byte
	copy(listSub[0:4], []byte("LIST"))
	binary.LittleEndian.PutUint32(listSub[4:8], uint32(len(infoBody)))
	body = append(body, listSub[:]...)
	body = append(body, infoBody...)

	out := []byte("RIFF")
	var sz [4]byte
	binary.LittleEndian.PutUint32(sz[:], uint32(len(body)))
	out = append(out, sz[:]...)
	out = append(out, body...)

	path := writeTempWAV(t, out)
	track := &Track{}
	if err := extractWAVWithContext(path, track, nil); err != nil {
		t.Fatalf("extractWAVWithContext: %v", err)
	}
	if track.Title != "FromID3" {
		t.Errorf("Title = %q, want %q (ID3 must win over LIST/INFO)", track.Title, "FromID3")
	}
}

// TestExtractWAV_MalformedShortLISTChunkSkipped pins the cursor-
// alignment fix: a LIST chunk declaring size < 4 (too short to hold
// even the 4-byte form-type) used to leave the cursor inside the
// truncated payload, mis-aligning every subsequent chunk header.
// The fix routes the short-size branch through seekPastChunk so the
// walker stays aligned and the trailing id3 chunk still parses.
// (CodeRabbit Minor on PR #224.)
func TestExtractWAV_MalformedShortLISTChunkSkipped(t *testing.T) {
	body := []byte("WAVE")
	// Malformed LIST (size=3, junk payload). Without the fix, the
	// next iteration reads the junk as a chunk header and the id3
	// chunk after it is never found.
	body = append(body, []byte("LIST")...)
	var listSize [4]byte
	binary.LittleEndian.PutUint32(listSize[:], 3)
	body = append(body, listSize[:]...)
	body = append(body, 0xAA, 0xBB, 0xCC)
	body = append(body, 0x00) // pad: (3 is odd) → 1 byte

	// Valid id3 chunk with a TIT2 afterwards.
	id3 := buildID3v2_3(map[string]string{"title": "RecoveredAfterBadLIST"})
	var sub [8]byte
	copy(sub[0:4], []byte("id3 "))
	binary.LittleEndian.PutUint32(sub[4:8], uint32(len(id3)))
	body = append(body, sub[:]...)
	body = append(body, id3...)
	if len(id3)%2 == 1 {
		body = append(body, 0x00)
	}

	out := []byte("RIFF")
	var size [4]byte
	binary.LittleEndian.PutUint32(size[:], uint32(len(body)))
	out = append(out, size[:]...)
	out = append(out, body...)

	path := writeTempWAV(t, out)
	track := &Track{}
	if err := extractWAVWithContext(path, track, nil); err != nil {
		t.Fatalf("extractWAVWithContext: %v", err)
	}
	if track.Title != "RecoveredAfterBadLIST" {
		t.Errorf("Title = %q, want %q (malformed LIST left cursor misaligned?)",
			track.Title, "RecoveredAfterBadLIST")
	}
}

// TestExtractAIFC_RoutedFromExtractWithContext locks the .aifc
// dispatcher routing. extractAIFFWithContext already accepted the
// "AIFC" form-type internally, but the dispatcher only routed
// .aif/.aiff. Pre-fix, .aifc files fell through to the dhowden
// default branch (which doesn't support AIFF/AIFC at all) and
// surfaced no metadata. (CodeRabbit Major on PR #224.)
func TestExtractAIFC_RoutedFromExtractWithContext(t *testing.T) {
	id3 := buildID3v2_3(map[string]string{"title": "AIFCTitle"})
	innerBody := []byte("AIFC")
	var sub [8]byte
	copy(sub[0:4], []byte("ID3 "))
	binary.BigEndian.PutUint32(sub[4:8], uint32(len(id3)))
	innerBody = append(innerBody, sub[:]...)
	innerBody = append(innerBody, id3...)
	if len(id3)%2 == 1 {
		innerBody = append(innerBody, 0x00)
	}
	out := []byte("FORM")
	var formSize [4]byte
	binary.BigEndian.PutUint32(formSize[:], uint32(len(innerBody)))
	out = append(out, formSize[:]...)
	out = append(out, innerBody...)

	dir := t.TempDir()
	path := filepath.Join(dir, "fixture.aifc")
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	track := &Track{}
	if err := ExtractWithContext(path, track, nil); err != nil {
		t.Fatalf("ExtractWithContext: %v", err)
	}
	if track.Codec != "AIFF" {
		t.Errorf("Codec = %q, want %q", track.Codec, "AIFF")
	}
	if track.Title != "AIFCTitle" {
		t.Errorf("Title = %q, want %q (.aifc routing missing?)",
			track.Title, "AIFCTitle")
	}
}

func TestExtractWAV_BadRIFFMagic(t *testing.T) {
	path := writeTempWAV(t, []byte("NOTWAV not a real wav file"))
	track := &Track{}
	err := extractWAVWithContext(path, track, nil)
	if err == nil {
		t.Fatalf("extractWAVWithContext: want error on bad magic, got nil")
	}
	if track.Codec != "WAV" {
		t.Errorf("Codec = %q, want %q (must stamp before magic check)", track.Codec, "WAV")
	}
}

func TestExtractWAV_NotWAVEFormTypeRejected(t *testing.T) {
	// RIFF magic but form-type != "WAVE" (could be AVI, RMID, etc.).
	bad := []byte("RIFF")
	var size [4]byte
	binary.LittleEndian.PutUint32(size[:], 4)
	bad = append(bad, size[:]...)
	bad = append(bad, []byte("AVI ")...)
	path := writeTempWAV(t, bad)
	track := &Track{}
	err := extractWAVWithContext(path, track, nil)
	if err == nil {
		t.Fatalf("extractWAVWithContext: want error on non-WAVE form type, got nil")
	}
}

// TestExtractAIFF_RoutedFromExtractWithContext locks the dispatcher
// wiring: ExtractWithContext routes .aif/.aiff through the new
// dedicated extractor (not the default dhowden fall-through).
func TestExtractAIFF_RoutedFromExtractWithContext(t *testing.T) {
	id3 := buildID3v2_3(map[string]string{"title": "RoutedTitle"})
	for _, ext := range []string{".aif", ".aiff"} {
		t.Run(ext, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "fixture"+ext)
			if err := os.WriteFile(path, buildAIFFWithID3(t, id3), 0o600); err != nil {
				t.Fatalf("write fixture: %v", err)
			}
			track := &Track{}
			if err := ExtractWithContext(path, track, nil); err != nil {
				t.Fatalf("ExtractWithContext: %v", err)
			}
			if track.Codec != "AIFF" {
				t.Errorf("Codec = %q, want %q", track.Codec, "AIFF")
			}
			if track.Title != "RoutedTitle" {
				t.Errorf("Title = %q, want %q", track.Title, "RoutedTitle")
			}
		})
	}
}

// TestExtractWAV_RoutedFromExtractWithContext locks the dispatcher
// wiring for .wav files.
func TestExtractWAV_RoutedFromExtractWithContext(t *testing.T) {
	path := writeTempWAV(t, buildWAVWithID3(t, buildID3v2_3(map[string]string{"title": "RoutedWAV"})))
	// Rename .wav extension by re-writing — writeTempWAV already uses .wav.
	track := &Track{}
	if err := ExtractWithContext(path, track, nil); err != nil {
		t.Fatalf("ExtractWithContext: %v", err)
	}
	if track.Codec != "WAV" {
		t.Errorf("Codec = %q, want %q", track.Codec, "WAV")
	}
	if track.Title != "RoutedWAV" {
		t.Errorf("Title = %q, want %q", track.Title, "RoutedWAV")
	}
}

// --- PCM geometry (sampleRate / bitsPerSample) ---

// wrapChunkLE / wrapChunkBE wrap a payload as a complete IFF/RIFF
// sub-chunk: FOURCC + 32-bit size + payload + odd-byte pad. LE is RIFF
// (WAV), BE is IFF (AIFF).
func wrapChunkLE(fourcc string, payload []byte) []byte {
	out := []byte(fourcc)
	var sz [4]byte
	binary.LittleEndian.PutUint32(sz[:], uint32(len(payload)))
	out = append(out, sz[:]...)
	out = append(out, payload...)
	if len(payload)%2 == 1 {
		out = append(out, 0x00)
	}
	return out
}

func wrapChunkBE(fourcc string, payload []byte) []byte {
	out := []byte(fourcc)
	var sz [4]byte
	binary.BigEndian.PutUint32(sz[:], uint32(len(payload)))
	out = append(out, sz[:]...)
	out = append(out, payload...)
	if len(payload)%2 == 1 {
		out = append(out, 0x00)
	}
	return out
}

// buildWAVFmtChunk builds a 16-byte WAVEFORMAT `fmt ` chunk for the
// given format tag (1=PCM, 3=IEEE float, 6=A-law, …), channels, sample
// rate, and bit depth.
func buildWAVFmtChunk(formatTag, channels uint16, sampleRate uint32, bits uint16) []byte {
	payload := make([]byte, 16)
	binary.LittleEndian.PutUint16(payload[0:2], formatTag)
	binary.LittleEndian.PutUint16(payload[2:4], channels)
	binary.LittleEndian.PutUint32(payload[4:8], sampleRate)
	binary.LittleEndian.PutUint32(payload[8:12], sampleRate*uint32(channels)*uint32(bits)/8) // byte rate
	binary.LittleEndian.PutUint16(payload[12:14], channels*bits/8)                           // block align
	binary.LittleEndian.PutUint16(payload[14:16], bits)
	return wrapChunkLE("fmt ", payload)
}

// buildWAVFmtChunkExtensible builds a 40-byte WAVE_FORMAT_EXTENSIBLE
// `fmt ` chunk whose SubFormat GUID begins with subFormat (1=PCM,
// 3=float). The extractor reads the container bit depth at [14:16] and
// the real format from the SubFormat's first 2 bytes.
func buildWAVFmtChunkExtensible(channels uint16, sampleRate uint32, containerBits, validBits, subFormat uint16) []byte {
	payload := make([]byte, 40)
	binary.LittleEndian.PutUint16(payload[0:2], 0xFFFE) // WAVE_FORMAT_EXTENSIBLE
	binary.LittleEndian.PutUint16(payload[2:4], channels)
	binary.LittleEndian.PutUint32(payload[4:8], sampleRate)
	binary.LittleEndian.PutUint32(payload[8:12], sampleRate*uint32(channels)*uint32(containerBits)/8)
	binary.LittleEndian.PutUint16(payload[12:14], channels*containerBits/8)
	binary.LittleEndian.PutUint16(payload[14:16], containerBits)
	binary.LittleEndian.PutUint16(payload[16:18], 22) // cbSize
	binary.LittleEndian.PutUint16(payload[18:20], validBits)
	binary.LittleEndian.PutUint32(payload[20:24], 0x3)       // channel mask FL|FR
	binary.LittleEndian.PutUint16(payload[24:26], subFormat) // SubFormat GUID first 2 bytes
	return wrapChunkLE("fmt ", payload)
}

// encodeAIFFExtendedInt encodes an integer sample rate as a 10-byte
// 80-bit IEEE-754 extended float — the inverse of parseAIFFExtended,
// used to synthesise AIFF COMM chunks.
func encodeAIFFExtendedInt(rate uint32) [10]byte {
	var out [10]byte
	if rate == 0 {
		return out
	}
	mantissa := uint64(rate)
	exponent := 16383 + 63
	for mantissa&(1<<63) == 0 {
		mantissa <<= 1
		exponent--
	}
	out[0] = byte(exponent >> 8)
	out[1] = byte(exponent)
	binary.BigEndian.PutUint64(out[2:10], mantissa)
	return out
}

// buildAIFFCOMMChunk builds an 18-byte AIFF COMM chunk. extraTrailer
// (e.g. an AIFC compressionType FOURCC + pstring) is appended after the
// 80-bit sample rate so AIFC-shaped COMM chunks can be exercised too.
func buildAIFFCOMMChunk(channels int16, numFrames uint32, sampleSize int16, sampleRate uint32, extraTrailer ...byte) []byte {
	payload := make([]byte, 18+len(extraTrailer))
	binary.BigEndian.PutUint16(payload[0:2], uint16(channels))
	binary.BigEndian.PutUint32(payload[2:6], numFrames)
	binary.BigEndian.PutUint16(payload[6:8], uint16(sampleSize))
	ext := encodeAIFFExtendedInt(sampleRate)
	copy(payload[8:18], ext[:])
	copy(payload[18:], extraTrailer)
	return wrapChunkBE("COMM", payload)
}

func TestExtractWAV_FmtChunkPCM(t *testing.T) {
	// PCM 96 kHz / 24-bit. fmt chunk only, no tags.
	path := writeTempWAV(t, buildWAVWithID3(t, nil, buildWAVFmtChunk(1, 2, 96000, 24)))
	track := &Track{}
	if err := extractWAVWithContext(path, track, nil); err != nil {
		t.Fatalf("extractWAVWithContext: %v", err)
	}
	if track.SampleRate == nil || *track.SampleRate != 96000 {
		t.Errorf("SampleRate = %v, want 96000", track.SampleRate)
	}
	if track.BitsPerSample == nil || *track.BitsPerSample != 24 {
		t.Errorf("BitsPerSample = %v, want 24", track.BitsPerSample)
	}
}

func TestExtractWAV_FmtChunkIEEEFloat32(t *testing.T) {
	// IEEE float (format tag 3), 192 kHz / 32-bit.
	path := writeTempWAV(t, buildWAVWithID3(t, nil, buildWAVFmtChunk(3, 2, 192000, 32)))
	track := &Track{}
	if err := extractWAVWithContext(path, track, nil); err != nil {
		t.Fatalf("extractWAVWithContext: %v", err)
	}
	if track.SampleRate == nil || *track.SampleRate != 192000 {
		t.Errorf("SampleRate = %v, want 192000", track.SampleRate)
	}
	if track.BitsPerSample == nil || *track.BitsPerSample != 32 {
		t.Errorf("BitsPerSample = %v, want 32 (float is PCM-like)", track.BitsPerSample)
	}
}

func TestExtractWAV_FmtChunkExtensiblePCM(t *testing.T) {
	// WAVE_FORMAT_EXTENSIBLE wrapping PCM — the bits gate must read the
	// real format from the SubFormat GUID, not bail on the 0xFFFE tag.
	path := writeTempWAV(t, buildWAVWithID3(t, nil, buildWAVFmtChunkExtensible(2, 88200, 24, 24, 1)))
	track := &Track{}
	if err := extractWAVWithContext(path, track, nil); err != nil {
		t.Fatalf("extractWAVWithContext: %v", err)
	}
	if track.SampleRate == nil || *track.SampleRate != 88200 {
		t.Errorf("SampleRate = %v, want 88200", track.SampleRate)
	}
	if track.BitsPerSample == nil || *track.BitsPerSample != 24 {
		t.Errorf("BitsPerSample = %v, want 24 (extensible PCM SubFormat)", track.BitsPerSample)
	}
}

func TestExtractWAV_FmtChunkLossyKeepsBitsNil(t *testing.T) {
	// A-law (format tag 6) is lossy/compressed: sampleRate is meaningful
	// but bitsPerSample is a container artefact and must stay nil.
	path := writeTempWAV(t, buildWAVWithID3(t, nil, buildWAVFmtChunk(6, 1, 8000, 8)))
	track := &Track{}
	if err := extractWAVWithContext(path, track, nil); err != nil {
		t.Fatalf("extractWAVWithContext: %v", err)
	}
	if track.SampleRate == nil || *track.SampleRate != 8000 {
		t.Errorf("SampleRate = %v, want 8000", track.SampleRate)
	}
	if track.BitsPerSample != nil {
		t.Errorf("BitsPerSample = %v, want nil for lossy A-law", *track.BitsPerSample)
	}
}

func TestExtractWAV_FmtChunkCoexistsWithID3(t *testing.T) {
	// Real WAVs carry fmt BEFORE the tags; format + tags both surface.
	id3 := buildID3v2_3(map[string]string{"title": "Both", "artist": "Geometry"})
	path := writeTempWAV(t, buildWAVWithID3(t, id3, buildWAVFmtChunk(1, 2, 44100, 16)))
	track := &Track{}
	if err := extractWAVWithContext(path, track, nil); err != nil {
		t.Fatalf("extractWAVWithContext: %v", err)
	}
	if track.Title != "Both" {
		t.Errorf("Title = %q, want %q", track.Title, "Both")
	}
	if track.SampleRate == nil || *track.SampleRate != 44100 {
		t.Errorf("SampleRate = %v, want 44100", track.SampleRate)
	}
	if track.BitsPerSample == nil || *track.BitsPerSample != 16 {
		t.Errorf("BitsPerSample = %v, want 16", track.BitsPerSample)
	}
}

func TestExtractAIFF_COMMChunkSurfacesSampleRateAndBits(t *testing.T) {
	for _, tc := range []struct {
		name string
		rate uint32
		bits int16
	}{
		{"CD", 44100, 16},
		{"hi-res", 96000, 24},
		{"DXD", 352800, 24},
		{"192/24", 192000, 24},
	} {
		t.Run(tc.name, func(t *testing.T) {
			comm := buildAIFFCOMMChunk(2, tc.rate*5, tc.bits, tc.rate)
			path := writeTempAIFF(t, buildAIFFWithID3(t, nil, comm))
			track := &Track{}
			if err := extractAIFFWithContext(path, track, nil); err != nil {
				t.Fatalf("extractAIFFWithContext: %v", err)
			}
			if track.SampleRate == nil || *track.SampleRate != float64(tc.rate) {
				t.Errorf("SampleRate = %v, want %d", track.SampleRate, tc.rate)
			}
			if track.BitsPerSample == nil || *track.BitsPerSample != int(tc.bits) {
				t.Errorf("BitsPerSample = %v, want %d", track.BitsPerSample, tc.bits)
			}
		})
	}
}

func TestExtractAIFF_COMMCoexistsWithID3(t *testing.T) {
	// COMM (format) + ID3 (tags) both present, like a real tagged AIFF.
	id3 := buildID3v2_3(map[string]string{"title": "Aria", "artist": "Gould"})
	comm := buildAIFFCOMMChunk(2, 44100*5, 24, 88200)
	path := writeTempAIFF(t, buildAIFFWithID3(t, id3, comm))
	track := &Track{}
	if err := extractAIFFWithContext(path, track, nil); err != nil {
		t.Fatalf("extractAIFFWithContext: %v", err)
	}
	if track.Title != "Aria" {
		t.Errorf("Title = %q, want %q", track.Title, "Aria")
	}
	if track.SampleRate == nil || *track.SampleRate != 88200 {
		t.Errorf("SampleRate = %v, want 88200", track.SampleRate)
	}
	if track.BitsPerSample == nil || *track.BitsPerSample != 24 {
		t.Errorf("BitsPerSample = %v, want 24", track.BitsPerSample)
	}
}

func TestExtractAIFC_COMMWithCompressionTrailer(t *testing.T) {
	// AIFC COMM appends a compressionType FOURCC + pstring after the
	// 80-bit rate; the leading 18 bytes parse identically. "NONE" is
	// uncompressed big-endian PCM.
	trailer := append([]byte("NONE"), 0x00) // 4-byte FOURCC + empty pstring
	comm := buildAIFFCOMMChunk(2, 48000*5, 24, 48000, trailer...)
	// Assemble an AIFC FORM around the COMM chunk.
	form := []byte("FORM")
	var sz [4]byte
	binary.BigEndian.PutUint32(sz[:], uint32(len("AIFC")+len(comm)))
	form = append(form, sz[:]...)
	form = append(form, []byte("AIFC")...)
	form = append(form, comm...)

	dir := t.TempDir()
	path := filepath.Join(dir, "fixture.aifc")
	if err := os.WriteFile(path, form, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	track := &Track{}
	if err := extractAIFFWithContext(path, track, nil); err != nil {
		t.Fatalf("extractAIFFWithContext: %v", err)
	}
	if track.SampleRate == nil || *track.SampleRate != 48000 {
		t.Errorf("SampleRate = %v, want 48000", track.SampleRate)
	}
	if track.BitsPerSample == nil || *track.BitsPerSample != 24 {
		t.Errorf("BitsPerSample = %v, want 24 (NONE is uncompressed PCM)", track.BitsPerSample)
	}
}

// TestExtractAIFC_LossyCompressionKeepsBitsNil — a COMPRESSED AIFC
// (compressionType "ima4", lossy ADPCM) must surface SampleRate but
// leave BitsPerSample nil: its COMM.sampleSize describes the
// pre-compression source, not the stored signal (the AIFF analog of the
// iOS PR #371 lossy-bit-depth regression). CodeRabbit Major on PR #440.
func TestExtractAIFC_LossyCompressionKeepsBitsNil(t *testing.T) {
	trailer := append([]byte("ima4"), 0x00) // lossy ADPCM compression FOURCC + empty pstring
	comm := buildAIFFCOMMChunk(2, 44100*5, 16, 44100, trailer...)
	form := []byte("FORM")
	var sz [4]byte
	binary.BigEndian.PutUint32(sz[:], uint32(len("AIFC")+len(comm)))
	form = append(form, sz[:]...)
	form = append(form, []byte("AIFC")...)
	form = append(form, comm...)

	dir := t.TempDir()
	path := filepath.Join(dir, "fixture.aifc")
	if err := os.WriteFile(path, form, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	track := &Track{}
	if err := extractAIFFWithContext(path, track, nil); err != nil {
		t.Fatalf("extractAIFFWithContext: %v", err)
	}
	if track.SampleRate == nil || *track.SampleRate != 44100 {
		t.Errorf("SampleRate = %v, want 44100 (rate always set)", track.SampleRate)
	}
	if track.BitsPerSample != nil {
		t.Errorf("BitsPerSample = %v, want nil for lossy AIFC compression", *track.BitsPerSample)
	}
}

// TestAIFFCOMMHasPCMDepth pins the AIFF/AIFC bit-depth eligibility gate:
// plain AIFF always, uncompressed AIFC FOURCCs yes, lossy ones no.
func TestAIFFCOMMHasPCMDepth(t *testing.T) {
	// Plain AIFF — body content irrelevant.
	if !aiffCOMMHasPCMDepth(make([]byte, 18), "AIFF") {
		t.Error("AIFF should always be PCM-depth eligible")
	}
	commWith := func(comp string) []byte {
		b := make([]byte, 22)
		copy(b[18:22], comp)
		return b
	}
	for _, comp := range []string{"NONE", "twos", "sowt", "raw ", "fl32", "fl64", "in24", "in32", "23ni"} {
		if !aiffCOMMHasPCMDepth(commWith(comp), "AIFC") {
			t.Errorf("AIFC %q should be PCM-depth eligible", comp)
		}
	}
	for _, comp := range []string{"ima4", "ulaw", "alaw", "MAC3", "MAC6", "QDMC", "agsm"} {
		if aiffCOMMHasPCMDepth(commWith(comp), "AIFC") {
			t.Errorf("AIFC %q (lossy/compressed) must NOT be PCM-depth eligible", comp)
		}
	}
	// AIFC COMM too short to hold the compressionType → not eligible.
	if aiffCOMMHasPCMDepth(make([]byte, 18), "AIFC") {
		t.Error("AIFC with truncated COMM (no compressionType) must not be eligible")
	}
}

// TestParseAIFFExtended_RoundTrip pins the 80-bit IEEE-extended decoder
// against the common sample rates — each must decode to its exact
// integer with no floating-point drift (math.Ldexp is exact here).
func TestParseAIFFExtended_RoundTrip(t *testing.T) {
	for _, rate := range []uint32{8000, 11025, 16000, 22050, 32000, 44100, 48000, 88200, 96000, 176400, 192000, 352800, 384000, 768000} {
		ext := encodeAIFFExtendedInt(rate)
		got := parseAIFFExtended(ext[:])
		if got != float64(rate) {
			t.Errorf("parseAIFFExtended(encode(%d)) = %v, want %d", rate, got, rate)
		}
	}
}

// TestParseAIFFExtended_DegenerateEncodings — zero, Inf, and NaN
// encodings (and a too-short slice) must all decode to 0 so a corrupt
// COMM leaves SampleRate nil rather than emitting a garbage rate.
func TestParseAIFFExtended_DegenerateEncodings(t *testing.T) {
	zero := make([]byte, 10)
	if got := parseAIFFExtended(zero); got != 0 {
		t.Errorf("zero encoding = %v, want 0", got)
	}
	// Exponent all-ones (0x7FFF) = Inf/NaN.
	inf := make([]byte, 10)
	inf[0], inf[1] = 0x7F, 0xFF
	inf[2] = 0x80 // non-zero mantissa MSB
	if got := parseAIFFExtended(inf); got != 0 {
		t.Errorf("Inf/NaN encoding = %v, want 0", got)
	}
	// A huge but non-0x7FFF exponent (0x7FFE here) drives Ldexp to
	// +Inf — which must clamp to 0, because a +Inf SampleRate would
	// fail json.Marshal and break the tags_json write (Gemini HIGH on
	// PR #440). Pre-fix this returned +Inf.
	overflow := make([]byte, 10)
	overflow[0], overflow[1] = 0x7F, 0xFE
	overflow[2] = 0x80 // mantissa MSB set
	if got := parseAIFFExtended(overflow); got != 0 {
		t.Errorf("overflow encoding = %v, want 0 (must not return ±Inf)", got)
	}
	if got := parseAIFFExtended([]byte{0x40, 0x0E}); got != 0 {
		t.Errorf("short slice = %v, want 0", got)
	}
}
