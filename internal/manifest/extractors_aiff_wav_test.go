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
