package manifest

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// minimalJPEG is a 12-byte JPEG-ish blob (SOI + APP0 marker prefix +
// EOI). Real enough that strings.HasPrefix(MIMEType, "image/") matches
// after dhowden/tag echoes back the APIC frame's stored MIME type;
// we don't decode the image, just hash the bytes — content-correctness
// of the image isn't load-bearing for any test in this file.
var minimalJPEG = []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00, 0xD9}

// buildID3v2_3WithAPIC builds an ID3v2.3 tag block that carries the
// given text frames AND an APIC (attached picture) frame holding the
// supplied image bytes. Mirrors the existing buildID3v2_3 helper in
// testdata_test.go but for the picture frame; kept here because only
// the artwork-extraction tests need it.
//
// APIC frame body shape per ID3v2.3 §4.15:
//
//	encoding (1 byte)             = 0x00 ISO-8859-1
//	MIME type, NUL-terminated     = "image/jpeg\x00"
//	picture type (1 byte)         = 0x03 cover (front)
//	description, NUL-terminated   = "\x00" (empty)
//	picture data                  = raw bytes
func buildID3v2_3WithAPIC(textFrames map[string]string, mimeType string, picData []byte) []byte {
	tag := buildID3v2_3(textFrames)
	// Strip the existing 10-byte header — we'll re-emit one with the
	// new total size after appending the APIC frame.
	body := tag[10:]

	// APIC frame body.
	var apicBody bytes.Buffer
	apicBody.WriteByte(0x00) // encoding: ISO-8859-1
	apicBody.WriteString(mimeType)
	apicBody.WriteByte(0x00) // MIME terminator
	apicBody.WriteByte(0x03) // picture type: cover (front)
	apicBody.WriteByte(0x00) // description terminator (empty)
	apicBody.Write(picData)

	// APIC frame header (ID3v2.3 — uint32 big-endian, not synchsafe).
	var apic bytes.Buffer
	apic.WriteString("APIC")
	var sz [4]byte
	binary.BigEndian.PutUint32(sz[:], uint32(apicBody.Len()))
	apic.Write(sz[:])
	apic.Write([]byte{0x00, 0x00}) // flags
	apic.Write(apicBody.Bytes())

	merged := append([]byte{}, body...)
	merged = append(merged, apic.Bytes()...)

	// Rebuild header with updated synchsafe size.
	var header [10]byte
	copy(header[0:3], []byte("ID3"))
	header[3] = 0x03 // v2.3
	header[4] = 0x00
	header[5] = 0x00 // flags
	writeSyncSafeSize(header[6:10], uint32(len(merged)))

	return append(header[:], merged...)
}

// writeMP3WithAPIC writes a minimal ID3v2.3-tagged MP3 with an embedded
// APIC frame to path. dhowden/tag parses the resulting file and
// surfaces the picture via m.Picture().
func writeMP3WithAPIC(t *testing.T, path string, textFrames map[string]string, mimeType string, picData []byte) {
	t.Helper()
	id3 := buildID3v2_3WithAPIC(textFrames, mimeType, picData)
	// A single dummy MPEG-1 Layer III frame so dhowden treats this as
	// a valid MP3 (matches writeMinimalMP3's framing).
	frame := []byte{0xFF, 0xFB, 0x90, 0x64}
	frame = append(frame, bytes.Repeat([]byte{0x00}, 144-4)...)
	if err := os.WriteFile(path, append(id3, frame...), 0o644); err != nil {
		t.Fatalf("write MP3 fixture: %v", err)
	}
}

// expectedLocalMBID computes the same `local-<hash>` sentinel the
// extractor would produce for the given bytes. Test-side mirror of
// stampLocalArtwork's hash logic.
func expectedLocalMBID(data []byte) string {
	sum := sha256.Sum256(data)
	return "local-" + hex.EncodeToString(sum[:])
}

func TestExtractLocalArtwork_Embedded(t *testing.T) {
	dir := t.TempDir()
	cacheDir := filepath.Join(dir, "artwork")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "track.mp3")
	writeMP3WithAPIC(t, path, map[string]string{
		"title":  "Test",
		"artist": "ArtistA",
		"album":  "AlbumA",
	}, "image/jpeg", minimalJPEG)

	tr := &Track{Path: "track.mp3", Size: 1, ModTime: time.Now()}
	folderCache := &sync.Map{}
	if err := ExtractWithContext(path, tr, &ExtractContext{
		ArtworkCacheDir: cacheDir,
		FolderArtCache:  folderCache,
	}); err != nil {
		t.Fatalf("ExtractWithContext: %v", err)
	}

	want := expectedLocalMBID(minimalJPEG)
	if tr.ArtworkMBID != want {
		t.Errorf("ArtworkMBID = %q, want %q", tr.ArtworkMBID, want)
	}
	cachePath := filepath.Join(cacheDir, want+"-500.jpg")
	got, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatalf("read cache file: %v", err)
	}
	if !bytes.Equal(got, minimalJPEG) {
		t.Errorf("cache bytes mismatch: got %x, want %x", got, minimalJPEG)
	}
}

func TestExtractLocalArtwork_FolderJPG(t *testing.T) {
	libDir := t.TempDir()
	cacheDir := filepath.Join(t.TempDir(), "artwork")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Audio file with NO embedded art — path-derived metadata only.
	audioPath := filepath.Join(libDir, "track.mp3")
	writeMinimalMP3(t, audioPath, map[string]string{"artist": "A", "album": "B"})
	// folder.jpg next to it — the extractor's folder-level fallback.
	coverBytes := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'F', 'O', 'L'}
	if err := os.WriteFile(filepath.Join(libDir, "folder.jpg"), coverBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	tr := &Track{Path: "track.mp3", Size: 1, ModTime: time.Now()}
	folderCache := &sync.Map{}
	if err := ExtractWithContext(audioPath, tr, &ExtractContext{
		ArtworkCacheDir: cacheDir,
		FolderArtCache:  folderCache,
	}); err != nil {
		t.Fatalf("ExtractWithContext: %v", err)
	}
	want := expectedLocalMBID(coverBytes)
	if tr.ArtworkMBID != want {
		t.Errorf("ArtworkMBID = %q, want %q (folder.jpg fallback)", tr.ArtworkMBID, want)
	}
	if _, err := os.Stat(filepath.Join(cacheDir, want+"-500.jpg")); err != nil {
		t.Errorf("cache file missing: %v", err)
	}
}

func TestExtractLocalArtwork_FolderCaseInsensitive(t *testing.T) {
	// Linux filesystems are case-sensitive; Windows-tagger output
	// like `Cover.JPG` / `FOLDER.PNG` must still be recognised. The
	// extractor uses strings.EqualFold against folderArtCandidates
	// for exactly this reason.
	for _, name := range []string{"Cover.JPG", "FOLDER.PNG", "cover.PNG", "Folder.jpg"} {
		t.Run(name, func(t *testing.T) {
			libDir := t.TempDir()
			cacheDir := filepath.Join(t.TempDir(), "artwork")
			if err := os.MkdirAll(cacheDir, 0o755); err != nil {
				t.Fatal(err)
			}
			audioPath := filepath.Join(libDir, "track.mp3")
			writeMinimalMP3(t, audioPath, map[string]string{"artist": "A", "album": "B"})
			cover := []byte{0xFF, 0xD8, 0xFF, 0xE0, 'X'}
			if err := os.WriteFile(filepath.Join(libDir, name), cover, 0o644); err != nil {
				t.Fatal(err)
			}

			tr := &Track{Path: "track.mp3", Size: 1, ModTime: time.Now()}
			folderCache := &sync.Map{}
			_ = ExtractWithContext(audioPath, tr, &ExtractContext{
				ArtworkCacheDir: cacheDir,
				FolderArtCache:  folderCache,
			})
			if !strings.HasPrefix(tr.ArtworkMBID, "local-") {
				t.Errorf("ArtworkMBID = %q, want local- prefix (case-insensitive %q match)",
					tr.ArtworkMBID, name)
			}
		})
	}
}

func TestExtractLocalArtwork_FolderCacheDedup(t *testing.T) {
	// Two tracks in the same directory + one folder.jpg → exactly
	// one ReadDir + one hash + one atomic write under the
	// single-flight folderArtPromise. Counter-asserted by checking
	// the post-run cache only contains a single .jpg file (the
	// stat-before-write path also short-circuits any second writer
	// even if the once.Do contract were broken).
	libDir := t.TempDir()
	cacheDir := filepath.Join(t.TempDir(), "artwork")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"track1.mp3", "track2.mp3"} {
		writeMinimalMP3(t, filepath.Join(libDir, name),
			map[string]string{"artist": "A", "album": "B"})
	}
	cover := []byte{0xFF, 0xD8, 0xFF, 0xE0, 'D', 'E', 'D', 'U', 'P'}
	if err := os.WriteFile(filepath.Join(libDir, "cover.jpg"), cover, 0o644); err != nil {
		t.Fatal(err)
	}

	folderCache := &sync.Map{}
	for _, name := range []string{"track1.mp3", "track2.mp3"} {
		tr := &Track{Path: name, Size: 1, ModTime: time.Now()}
		_ = ExtractWithContext(filepath.Join(libDir, name), tr, &ExtractContext{
			ArtworkCacheDir: cacheDir,
			FolderArtCache:  folderCache,
		})
		want := expectedLocalMBID(cover)
		if tr.ArtworkMBID != want {
			t.Errorf("track %s ArtworkMBID = %q, want %q", name, tr.ArtworkMBID, want)
		}
	}

	// One audio-album worth of folder fallback should produce exactly
	// one cached jpg in the artwork dir.
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	jpgCount := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".jpg") {
			jpgCount++
		}
	}
	if jpgCount != 1 {
		t.Errorf("cache contains %d jpg files, want 1 (dedup expected)", jpgCount)
	}
}

func TestExtractLocalArtwork_PreferEmbedded(t *testing.T) {
	// Embedded APIC bytes win over folder.jpg when both are present.
	libDir := t.TempDir()
	cacheDir := filepath.Join(t.TempDir(), "artwork")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	embedded := []byte{0xFF, 0xD8, 0xFF, 0xE0, 'E', 'M', 'B'}
	folder := []byte{0xFF, 0xD8, 0xFF, 0xE0, 'F', 'O', 'L'}
	audioPath := filepath.Join(libDir, "track.mp3")
	writeMP3WithAPIC(t, audioPath, map[string]string{"artist": "A", "album": "B"},
		"image/jpeg", embedded)
	if err := os.WriteFile(filepath.Join(libDir, "cover.jpg"), folder, 0o644); err != nil {
		t.Fatal(err)
	}

	tr := &Track{Path: "track.mp3", Size: 1, ModTime: time.Now()}
	folderCache := &sync.Map{}
	_ = ExtractWithContext(audioPath, tr, &ExtractContext{
		ArtworkCacheDir: cacheDir,
		FolderArtCache:  folderCache,
	})
	wantEmbedded := expectedLocalMBID(embedded)
	if tr.ArtworkMBID != wantEmbedded {
		t.Errorf("ArtworkMBID = %q, want embedded hash %q (embedded must win)",
			tr.ArtworkMBID, wantEmbedded)
	}
}

func TestExtractLocalArtwork_NoCacheDir(t *testing.T) {
	// ec with empty ArtworkCacheDir is a documented no-op for legacy
	// callers (the scanner passes "" when the bridge is configured
	// without an artwork dir). Track must still get its tag fields,
	// just no ArtworkMBID stamping.
	libDir := t.TempDir()
	audioPath := filepath.Join(libDir, "track.mp3")
	writeMP3WithAPIC(t, audioPath, map[string]string{
		"artist": "A", "album": "B", "title": "T",
	}, "image/jpeg", minimalJPEG)
	// folder.jpg is also present — must NOT be picked up either.
	if err := os.WriteFile(filepath.Join(libDir, "folder.jpg"), minimalJPEG, 0o644); err != nil {
		t.Fatal(err)
	}

	tr := &Track{Path: "track.mp3", Size: 1, ModTime: time.Now()}
	if err := ExtractWithContext(audioPath, tr, &ExtractContext{
		ArtworkCacheDir: "", // disabled
		FolderArtCache:  &sync.Map{},
	}); err != nil {
		t.Fatalf("ExtractWithContext: %v", err)
	}
	if tr.ArtworkMBID != "" {
		t.Errorf("ArtworkMBID = %q, want empty (no cacheDir)", tr.ArtworkMBID)
	}
	if tr.Title != "T" {
		t.Errorf("Title = %q, want T (tag extraction must still run)", tr.Title)
	}
}

func TestExtractLocalArtwork_OverSizeBytes(t *testing.T) {
	// Embedded APIC larger than maxArtworkBytes is logged + skipped.
	// Track gets indexed but ArtworkMBID stays empty so the enricher
	// can still try the MusicBrainz path.
	libDir := t.TempDir()
	cacheDir := filepath.Join(t.TempDir(), "artwork")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	huge := make([]byte, maxArtworkBytes+1) // one byte over the cap
	huge[0] = 0xFF
	huge[1] = 0xD8
	audioPath := filepath.Join(libDir, "track.mp3")
	writeMP3WithAPIC(t, audioPath, map[string]string{"artist": "A", "album": "B"},
		"image/jpeg", huge)

	tr := &Track{Path: "track.mp3", Size: 1, ModTime: time.Now()}
	_ = ExtractWithContext(audioPath, tr, &ExtractContext{
		ArtworkCacheDir: cacheDir,
		FolderArtCache:  &sync.Map{},
	})
	if tr.ArtworkMBID != "" {
		t.Errorf("ArtworkMBID = %q, want empty (oversize embedded picture must be skipped)",
			tr.ArtworkMBID)
	}
	entries, _ := os.ReadDir(cacheDir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".jpg") {
			t.Errorf("oversize picture leaked into cache: %s", e.Name())
		}
	}
}

func TestExtractLocalArtwork_NonImageMIMEType(t *testing.T) {
	// Spec-allowed but rare: an APIC frame with non-image MIME type.
	// Refuse to hash + cache; track stays empty.
	libDir := t.TempDir()
	cacheDir := filepath.Join(t.TempDir(), "artwork")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	audioPath := filepath.Join(libDir, "track.mp3")
	writeMP3WithAPIC(t, audioPath, map[string]string{"artist": "A", "album": "B"},
		"application/octet-stream", []byte{0x00, 0x01, 0x02, 0x03})

	tr := &Track{Path: "track.mp3", Size: 1, ModTime: time.Now()}
	_ = ExtractWithContext(audioPath, tr, &ExtractContext{
		ArtworkCacheDir: cacheDir,
		FolderArtCache:  &sync.Map{},
	})
	if tr.ArtworkMBID != "" {
		t.Errorf("ArtworkMBID = %q, want empty (non-image MIME must be skipped)",
			tr.ArtworkMBID)
	}
}

func TestExtractLocalArtwork_StatBeforeWriteIdempotent(t *testing.T) {
	// Stat-before-write makes the extractor idempotent across re-
	// scans AND recovers transparently from a cache-dir wipe on the
	// next pass. Pre-stage the target cache file with sentinel bytes;
	// run the extractor; the cache file's contents must be preserved
	// (no overwrite) AND ArtworkMBID must match the pre-stage hash.
	libDir := t.TempDir()
	cacheDir := filepath.Join(t.TempDir(), "artwork")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	embedded := []byte{0xFF, 0xD8, 0xFF, 0xE0, 'I', 'D', 'M'}
	want := expectedLocalMBID(embedded)
	cachePath := filepath.Join(cacheDir, want+"-500.jpg")
	sentinel := []byte("DO_NOT_OVERWRITE")
	if err := os.WriteFile(cachePath, sentinel, 0o644); err != nil {
		t.Fatal(err)
	}

	audioPath := filepath.Join(libDir, "track.mp3")
	writeMP3WithAPIC(t, audioPath, map[string]string{"artist": "A", "album": "B"},
		"image/jpeg", embedded)
	tr := &Track{Path: "track.mp3", Size: 1, ModTime: time.Now()}
	_ = ExtractWithContext(audioPath, tr, &ExtractContext{
		ArtworkCacheDir: cacheDir,
		FolderArtCache:  &sync.Map{},
	})
	if tr.ArtworkMBID != want {
		t.Errorf("ArtworkMBID = %q, want %q", tr.ArtworkMBID, want)
	}
	got, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, sentinel) {
		t.Errorf("cache file overwritten: got %q, want sentinel %q", got, sentinel)
	}
}

func TestExtractLocalArtwork_FolderMissingNoEffect(t *testing.T) {
	// Empty directory + audio with no embedded art → ArtworkMBID
	// stays empty, no spurious cache writes, no error.
	libDir := t.TempDir()
	cacheDir := filepath.Join(t.TempDir(), "artwork")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	audioPath := filepath.Join(libDir, "track.mp3")
	writeMinimalMP3(t, audioPath, map[string]string{"artist": "A", "album": "B"})

	tr := &Track{Path: "track.mp3", Size: 1, ModTime: time.Now()}
	if err := ExtractWithContext(audioPath, tr, &ExtractContext{
		ArtworkCacheDir: cacheDir,
		FolderArtCache:  &sync.Map{},
	}); err != nil {
		t.Fatalf("ExtractWithContext: %v", err)
	}
	if tr.ArtworkMBID != "" {
		t.Errorf("ArtworkMBID = %q, want empty (no embedded, no folder.jpg)", tr.ArtworkMBID)
	}
	entries, _ := os.ReadDir(cacheDir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".jpg") {
			t.Errorf("unexpected cache write: %s", e.Name())
		}
	}
}

// Suppresses unused-import warning when running individual tests
// (atomic / time are used but the package's other test files cover
// some of these — keeping explicit references here makes the file
// self-contained without depending on testdata_test.go's ordering).
var _ = atomic.Bool{}
var _ = time.Now
