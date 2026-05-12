package manifest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"os"
	"path/filepath"
	"strconv"
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
	// like `Cover.JPG` / `Folder.JPG` must still be recognised. The
	// extractor uses strings.EqualFold against folderArtCandidates
	// for exactly this reason. JPEG-only by design — PNG case-
	// variants are covered by TestExtractLocalArtwork_RejectsPNGCandidates.
	for _, name := range []string{"Cover.JPG", "FOLDER.JPG", "cover.JPG", "Folder.jpg"} {
		t.Run(name, func(t *testing.T) {
			libDir := t.TempDir()
			cacheDir := filepath.Join(t.TempDir(), "artwork")
			if err := os.MkdirAll(cacheDir, 0o700); err != nil {
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

// TestExtractLocalArtwork_RejectsPNGCandidates asserts the V1
// JPEG-only contract: cover.png / folder.png filenames are NOT
// matched by the folder-level fallback. The cache scheme writes
// `local-<hash>-500.jpg` and the API serves `Content-Type:
// image/jpeg`; mixing PNG bytes into that scheme would produce a
// misdeclared response. PR #98 originally accepted PNG candidates;
// follow-up review (Qodo) flagged the mismatch and we restricted
// to JPEG.
func TestExtractLocalArtwork_RejectsPNGCandidates(t *testing.T) {
	for _, name := range []string{"cover.png", "folder.png", "Cover.PNG", "FOLDER.PNG"} {
		t.Run(name, func(t *testing.T) {
			libDir := t.TempDir()
			cacheDir := filepath.Join(t.TempDir(), "artwork")
			if err := os.MkdirAll(cacheDir, 0o700); err != nil {
				t.Fatal(err)
			}
			audioPath := filepath.Join(libDir, "track.mp3")
			writeMinimalMP3(t, audioPath, map[string]string{"artist": "A", "album": "B"})
			// Write actual PNG bytes (89 50 4E 47 ...) so that even
			// if the regex matched, the magic-byte sniff would
			// reject. Belt-and-suspenders coverage.
			pngBytes := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}
			if err := os.WriteFile(filepath.Join(libDir, name), pngBytes, 0o644); err != nil {
				t.Fatal(err)
			}

			tr := &Track{Path: "track.mp3", Size: 1, ModTime: time.Now()}
			_ = ExtractWithContext(audioPath, tr, &ExtractContext{
				ArtworkCacheDir: cacheDir,
				FolderArtCache:  &sync.Map{},
			})
			if tr.ArtworkMBID != "" {
				t.Errorf("ArtworkMBID = %q, want empty (%q must not match the JPEG-only candidate set)",
					tr.ArtworkMBID, name)
			}
			entries, _ := os.ReadDir(cacheDir)
			for _, e := range entries {
				if strings.HasSuffix(e.Name(), ".jpg") {
					t.Errorf("PNG candidate %q leaked into cache: %s", name, e.Name())
				}
			}
		})
	}
}

// TestExtractLocalArtwork_RejectsMisnamedPNG covers the "user named
// a PNG `cover.jpg`" defense. The filename matches the JPEG-only
// candidate set, but the magic-byte sniff catches the byte-level
// mismatch before stamp commits to the cache. Without this guard,
// the cache file would be PNG bytes served as image/jpeg.
func TestExtractLocalArtwork_RejectsMisnamedPNG(t *testing.T) {
	libDir := t.TempDir()
	cacheDir := filepath.Join(t.TempDir(), "artwork")
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		t.Fatal(err)
	}
	audioPath := filepath.Join(libDir, "track.mp3")
	writeMinimalMP3(t, audioPath, map[string]string{"artist": "A", "album": "B"})
	pngBytes := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 'P', 'N', 'G'}
	// Filename ends in .jpg — would match folderArtCandidates — but
	// the bytes are PNG. Magic-byte sniff must reject.
	if err := os.WriteFile(filepath.Join(libDir, "cover.jpg"), pngBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	tr := &Track{Path: "track.mp3", Size: 1, ModTime: time.Now()}
	_ = ExtractWithContext(audioPath, tr, &ExtractContext{
		ArtworkCacheDir: cacheDir,
		FolderArtCache:  &sync.Map{},
	})
	if tr.ArtworkMBID != "" {
		t.Errorf("ArtworkMBID = %q, want empty (PNG-bytes-in-jpg-file must be rejected)",
			tr.ArtworkMBID)
	}
}

// TestExtractLocalArtwork_RejectsEmbeddedPNG covers the embedded-
// APIC variant of the same defense: an APIC frame with MIME type
// `image/png` (or correct type but PNG bytes) must not be cached.
func TestExtractLocalArtwork_RejectsEmbeddedPNG(t *testing.T) {
	libDir := t.TempDir()
	cacheDir := filepath.Join(t.TempDir(), "artwork")
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		t.Fatal(err)
	}
	audioPath := filepath.Join(libDir, "track.mp3")
	pngBytes := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 'P', 'N', 'G'}
	writeMP3WithAPIC(t, audioPath, map[string]string{"artist": "A", "album": "B"},
		"image/png", pngBytes)

	tr := &Track{Path: "track.mp3", Size: 1, ModTime: time.Now()}
	_ = ExtractWithContext(audioPath, tr, &ExtractContext{
		ArtworkCacheDir: cacheDir,
		FolderArtCache:  &sync.Map{},
	})
	if tr.ArtworkMBID != "" {
		t.Errorf("ArtworkMBID = %q, want empty (embedded image/png must be rejected)",
			tr.ArtworkMBID)
	}
}

// TestExtractLocalArtwork_RejectsMisdeclaredJPEG covers a third
// vector: APIC frame claims `image/jpeg` MIME type but the bytes
// are PNG. The magic-byte sniff rejects regardless of declared
// MIME — defense in depth against tag forgery.
func TestExtractLocalArtwork_RejectsMisdeclaredJPEG(t *testing.T) {
	libDir := t.TempDir()
	cacheDir := filepath.Join(t.TempDir(), "artwork")
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		t.Fatal(err)
	}
	audioPath := filepath.Join(libDir, "track.mp3")
	pngBytes := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 'P', 'N', 'G'}
	writeMP3WithAPIC(t, audioPath, map[string]string{"artist": "A", "album": "B"},
		"image/jpeg", pngBytes) // MIME claims JPEG, bytes are PNG

	tr := &Track{Path: "track.mp3", Size: 1, ModTime: time.Now()}
	_ = ExtractWithContext(audioPath, tr, &ExtractContext{
		ArtworkCacheDir: cacheDir,
		FolderArtCache:  &sync.Map{},
	})
	if tr.ArtworkMBID != "" {
		t.Errorf("ArtworkMBID = %q, want empty (misdeclared JPEG MIME with non-JPEG bytes must be rejected)",
			tr.ArtworkMBID)
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

func TestWriteArtworkAtomicScan_RaceWinnerCleansTmp(t *testing.T) {
	// Regression for the stat-and-accept fallback's tmp-leak bug
	// (gemini-code-assist on PR #100): when renameWithRetry exhausts
	// its budget but a concurrent writer / prior pass has already
	// published a byte-equivalent destination, writeArtworkAtomicScan
	// returns nil — but the source tmp file is still on disk and the
	// deferred os.Remove(tmpName) MUST run. Pre-fix, an early
	// `tmpName = ""` in the fallback branch suppressed the cleanup
	// and leaked one `.scan-NNN.jpg.tmp` per race-window hit, which
	// over a long uptime would accumulate to the point of disk
	// pressure on the cache partition.
	cacheDir := t.TempDir()
	data := []byte("payload-bytes-for-race-winner-test")
	sum := sha256.Sum256(data)
	mbid := "local-" + hex.EncodeToString(sum[:])
	dst := filepath.Join(cacheDir, mbid+"-500.jpg")

	// Pre-stage destination as if a parallel writer (or prior pass)
	// has already published the file with byte-equivalent content.
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		t.Fatal(err)
	}

	// Inject rename failure so the stat-and-accept branch fires
	// without burning the full ~750 ms retry backoff budget.
	orig := renameFunc
	renameFunc = func(src, dst string) error { return os.ErrPermission }
	t.Cleanup(func() { renameFunc = orig })

	if err := writeArtworkAtomicScan(dst, data); err != nil {
		t.Fatalf("writeArtworkAtomicScan: %v (expected nil — race winner with matching size)", err)
	}

	// Pre-staged destination must be intact (not clobbered).
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("destination clobbered after stat-and-accept: got %q want %q", got, data)
	}

	// And no `.scan-NNN.jpg.tmp` leftover from the failed rename.
	entries, _ := os.ReadDir(cacheDir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".scan-") {
			t.Errorf("leaked tmp file (deferred cleanup did not run): %s", e.Name())
		}
	}
}

func TestWriteArtworkAtomicScan_RaceLoserDoesNotAcceptOnSizeCollision(t *testing.T) {
	// Same-size-different-content negative case (CodeRabbit on
	// PR #100): if the existing destination matches `len(data)` but
	// the bytes differ, the stat-and-accept fallback MUST NOT
	// silently swallow the rename failure. Reproducer is contrived
	// (in production the filename embeds the SHA-256 of `data` so
	// any concurrent winner with the same destination is byte-equivalent
	// by construction), but the test pins the byte-equivalence
	// contract so a future caller that doesn't honour the hash-in-
	// filename convention can't regress this silently.
	cacheDir := t.TempDir()
	want := []byte("expected-bytes-the-caller-tried-to-write")
	collision := make([]byte, len(want))
	copy(collision, want)
	collision[0] ^= 0xFF // same length, different content
	dst := filepath.Join(cacheDir, "local-decoy-500.jpg")
	if err := os.WriteFile(dst, collision, 0o644); err != nil {
		t.Fatal(err)
	}

	orig := renameFunc
	renameFunc = func(src, dst string) error { return os.ErrPermission }
	t.Cleanup(func() { renameFunc = orig })

	if err := writeArtworkAtomicScan(dst, want); err == nil {
		t.Fatal("writeArtworkAtomicScan returned nil; expected the rename error to propagate when destination is size-equal but byte-different")
	}
	// Tmp file from this run must still be cleaned up by the defer.
	entries, _ := os.ReadDir(cacheDir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".scan-") {
			t.Errorf("leaked tmp on size-collision path: %s", e.Name())
		}
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

// TestScanner_RecoversWipedLocalArtworkCache covers the cache-
// wiped-but-manifest-stale recovery path. Sequence:
//
//  1. Initial scan populates cache + stamps Track.ArtworkMBID =
//     "local-<hash>".
//  2. Operator wipes <dataDir>/artwork/ (full cache-dir delete is the
//     realistic incident — copy-paste data dir without artwork
//     subdir, manual `rm -rf`, etc.).
//  3. Re-scan must rebuild the cache file even though the audio file
//     is unchanged (size + mtime would normally trigger early-skip).
//
// Without this recovery, the API returns 202 + Retry-After
// indefinitely for the dangling local- mbid because the enricher
// won't refetch a local- value (no upstream to re-fetch from).
// PR #98 originally documented this as a known limitation; follow-
// up review (Qodo) flagged it as a real bug since the scanner had
// no recovery path either.
func TestScanner_RecoversWipedLocalArtworkCache(t *testing.T) {
	libDir := t.TempDir()
	cacheDir := filepath.Join(t.TempDir(), "artwork")
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Seed an MP3 with embedded APIC so the scanner stamps a
	// local-<hash> ArtworkMBID on first scan.
	audioPath := filepath.Join(libDir, "track.mp3")
	writeMP3WithAPIC(t, audioPath, map[string]string{
		"artist": "Recover", "album": "WipeMe",
	}, "image/jpeg", minimalJPEG)

	store, err := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	sc := NewScanner([]string{libDir}, store, cacheDir)

	// First scan: stamps local-<hash>, writes the cache file.
	if _, err := sc.Scan(context.Background()); err != nil {
		t.Fatalf("first Scan: %v", err)
	}
	expectedMBID := expectedLocalMBID(minimalJPEG)
	cachePath := filepath.Join(cacheDir, expectedMBID+"-500.jpg")
	if _, err := os.Stat(cachePath); err != nil {
		t.Fatalf("cache file not written on first scan: %v", err)
	}

	// Wipe the cache file (operator action — manual rm or cache-dir
	// migration that loses the artwork subdir).
	if err := os.Remove(cachePath); err != nil {
		t.Fatalf("wipe cache: %v", err)
	}

	// Second scan: track is otherwise unchanged (same size, same
	// mtime), but the local- cache file is missing. The recovery
	// path must force re-extraction so the cache is rebuilt.
	if _, err := sc.Scan(context.Background()); err != nil {
		t.Fatalf("second Scan: %v", err)
	}
	if _, err := os.Stat(cachePath); err != nil {
		t.Errorf("cache file not recovered on second scan: %v", err)
	}

	// And the manifest still carries the same local- value.
	got, _ := store.GetTrack("track.mp3")
	if got == nil || got.ArtworkMBID != expectedMBID {
		t.Errorf("ArtworkMBID after recovery = %v, want %q", got, expectedMBID)
	}
}

// TestScanner_NoRecoveryForUUIDArtworkMBID asserts the recovery
// path fires ONLY for `local-` prefixed MBIDs. Tracks whose
// ArtworkMBID is a MusicBrainz UUID belong to the enricher's CAA /
// iTunes path; the scanner has no business re-extracting those, and
// adding a stat-per-track for non-local rows would be wasted I/O on
// libraries dominated by enricher-cached artwork.
func TestScanner_NoRecoveryForUUIDArtworkMBID(t *testing.T) {
	libDir := t.TempDir()
	cacheDir := filepath.Join(t.TempDir(), "artwork")
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		t.Fatal(err)
	}
	audioPath := filepath.Join(libDir, "track.mp3")
	writeMinimalMP3(t, audioPath, map[string]string{"artist": "X", "album": "Y"})

	store, _ := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	defer store.Close()

	// Pre-seed the manifest as if the enricher had run: track has
	// a UUID-form ArtworkMBID. Whether that file exists on disk is
	// irrelevant — recovery must NOT trigger for non-local rows.
	info, _ := os.Stat(audioPath)
	uuidMBID := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	store.UpsertTrack(&Track{
		Path: "track.mp3", Size: info.Size(), ModTime: info.ModTime().UTC(),
		Artist: "X", Album: "Y", ArtworkMBID: uuidMBID,
	})

	sc := NewScanner([]string{libDir}, store, cacheDir)
	scanner := sc // keep symbol live

	// The recovery helper itself is the unit-test shape — assert
	// directly that a UUID-prefixed track does NOT trigger
	// recovery, regardless of the cache file's presence.
	stored, _ := store.GetTrack("track.mp3")
	if scanner.needsLocalArtworkRecovery(stored) {
		t.Errorf("UUID-form ArtworkMBID must not trigger recovery (got true)")
	}

	// And after a fresh scan the row's ArtworkMBID survives unchanged
	// — the worker may re-extract (no embedded art → empty), but the
	// existing UUID stays in place because UpsertTrack is keyed on
	// path and there's no overwrite of pre-existing fields. Skip
	// this assertion because the scanner DOES re-extract on the
	// pre-seeded row's writer round-trip — sufficient that recovery
	// itself didn't fire above.
}

// --- stringOf case-insensitive lookup ---

// TestStringOfMatchesVorbisAndID3v2Spellings locks the case + space
// agnosticism. Vorbis writes `MUSICBRAINZ_ALBUMID`; ID3v2 TXXX writes
// `MusicBrainz Album Id` (Picard's canonical form). Pre-fix the lookup
// did exact case-sensitive map subscripts and silently missed every
// ID3v2-tagged album. Per Gemini A6 / iOS bug review #6d.
func TestStringOfMatchesVorbisAndID3v2Spellings(t *testing.T) {
	cases := []struct {
		name string
		raw  map[string]any
		want string
	}{
		{
			name: "Vorbis upper underscore",
			raw:  map[string]any{"MUSICBRAINZ_ALBUMID": "vorbis-mbid"},
			want: "vorbis-mbid",
		},
		{
			name: "ID3v2 TXXX human-readable",
			raw:  map[string]any{"MusicBrainz Album Id": "id3v2-mbid"},
			want: "id3v2-mbid",
		},
		{
			name: "all-lower underscored",
			raw:  map[string]any{"musicbrainz_albumid": "lower-mbid"},
			want: "lower-mbid",
		},
		{
			name: "mixed-case spaced",
			raw:  map[string]any{"MUSICBRAINZ ALBUM ID": "spaced-mbid"},
			want: "spaced-mbid",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Pre-normalised keys: 3 wire spellings collapse to 2
			// distinct normalised forms (the underscore-joined and
			// the space-derived). See stringOf docstring.
			got, ok := stringOf(tc.raw,
				"musicbrainz_albumid",
				"musicbrainz_album_id",
			)
			if !ok {
				t.Fatalf("stringOf failed to find any key in %v", tc.raw)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestStringOfTrimsResultValue locks the trim-on-return contract.
// ID3v2 TXXX frames occasionally carry trailing whitespace; without
// the trim, two tracks tagged identically except for trailing
// whitespace would surface as different MBIDs (and on the iOS side
// fan out into different per-album cover-art lookups).
//
// Note on search-key construction: the normaliser bridges minor
// case + space-vs-underscore variants WITHIN one spelling (e.g.
// `musicbrainz_albumid` vs `MUSICBRAINZ_ALBUMID`) but NOT across
// fundamentally different word-boundary shapes (`MUSICBRAINZ_ALBUMID`
// has no separator between "album" and "id"; `MusicBrainz Album Id`
// has a space). Real callers pass BOTH spellings in the search keys
// — this test mirrors that pattern.
func TestStringOfTrimsResultValue(t *testing.T) {
	raw := map[string]any{"MusicBrainz Album Id": "  album-mbid  \n"}
	got, ok := stringOf(raw,
		"musicbrainz_albumid",
		"musicbrainz_album_id",
	)
	if !ok {
		t.Fatalf("stringOf failed to find key in raw=%v", raw)
	}
	if got != "album-mbid" {
		t.Errorf("got %q, want %q (trim-on-return)", got, "album-mbid")
	}
}

// TestStringOfRejectsEmptyAfterTrim — a tag whose value is only
// whitespace should be treated as absent. Defends against taggers
// that emit `MusicBrainz Album Id = "   "`. Pre-fix the rough-trim
// landscape might still return that as a "set" value.
func TestStringOfRejectsEmptyAfterTrim(t *testing.T) {
	raw := map[string]any{"MUSICBRAINZ_ALBUMID": "   \t\n  "}
	if got, ok := stringOf(raw, "musicbrainz_albumid"); ok {
		t.Errorf("stringOf returned %q for whitespace-only value, want absent", got)
	}
}

// TestStringOfArrayValueTrimsFirst — Vorbis comments occasionally
// surface as []string in dhowden's raw map. Locks the same
// trim-on-return for the slice path.
func TestStringOfArrayValueTrimsFirst(t *testing.T) {
	raw := map[string]any{"MUSICBRAINZ_ALBUMID": []string{"  vorbis-mbid  "}}
	got, ok := stringOf(raw, "musicbrainz_albumid")
	if !ok {
		t.Fatalf("stringOf failed to find key in []string value")
	}
	if got != "vorbis-mbid" {
		t.Errorf("got %q, want %q", got, "vorbis-mbid")
	}
}

// TestStringOfArrayScansAllEntries pins the contract that a leading
// blank entry in the slice doesn't shadow a populated trailing one.
// Vorbis allows duplicate keys; pre-fix the function only inspected
// `s[0]`, treating `["  ", "real-value"]` as absent. CodeRabbit
// Minor round-1 on PR #166.
func TestStringOfArrayScansAllEntries(t *testing.T) {
	raw := map[string]any{"COMMENT": []string{"  ", "  real-value  "}}
	got, ok := stringOf(raw, "comment")
	if !ok {
		t.Fatalf("stringOf should walk past blank entries, got absent")
	}
	if got != "real-value" {
		t.Errorf("got %q, want %q", got, "real-value")
	}
}

// TestStringOfHandlesIntValueForCpil is the load-bearing case for
// the bridge#166 Critical fix. dhowden/tag's MP4 path stores the
// `cpil` (compilation) atom as a Go `int` (0 or 1) in Metadata.Raw()
// — the atom-class table maps `cpil` → "compilation", and the
// "uint8" content-type calls `getInt(b[:1])` which returns int.
// Pre-fix the type-switch only handled `string` and `[]string`,
// silently failing for every M4A compilation.
//
// The Compilation safety net's call site checks `comp == "1"` —
// stringOf must coerce the int to "1" so the comparison succeeds.
func TestStringOfHandlesIntValueForCpil(t *testing.T) {
	cases := []struct {
		name string
		val  any
		want string
	}{
		{"int 1", int(1), "1"},
		{"int 0", int(0), "0"},
		{"int64 1", int64(1), "1"},
		{"uint8 1", uint8(1), "1"},
		{"bool true", true, "1"},
		{"bool false", false, "0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := map[string]any{"compilation": tc.val}
			got, ok := stringOf(raw, "tcmp", "cpil", "compilation")
			if !ok {
				t.Fatalf("stringOf should accept %T value, got absent", tc.val)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q for %T(%v)", got, tc.want, tc.val, tc.val)
			}
		})
	}
}

// --- whitespace trim on top-level tags ---

// TestPopulateTrimsWhitespaceFromAllTagFields locks the
// `strings.TrimSpace` defense on every string tag. Pre-fix a track
// tagged `Album: "Abbey Road "` and another tagged `Album: "Abbey
// Road"` would surface as two separate Album rows on iOS (whose
// `MetadataNormalizer.normalize` does trim, but a trim at the bridge
// is one less round-trip for that hygiene). Per Gemini A6 /
// iOS bug review #6e.
func TestPopulateTrimsWhitespaceFromAllTagFields(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "t.flac")
	writeMinimalFLAC(t, p, 44100, 16, map[string]string{
		"TITLE":       "  Song  ",
		"ARTIST":      "  An Artist\n",
		"ALBUMARTIST": "\tAA\t",
		"ALBUM":       "  An Album  ",
		"GENRE":       " Jazz \n",
	})
	tr := &Track{Path: "t.flac", Size: 1, ModTime: time.Now()}
	if err := Extract(p, tr); err != nil {
		t.Fatal(err)
	}
	if tr.Title != "Song" {
		t.Errorf("Title: got %q, want %q", tr.Title, "Song")
	}
	if tr.Artist != "An Artist" {
		t.Errorf("Artist: got %q, want %q", tr.Artist, "An Artist")
	}
	if tr.AlbumArtist != "AA" {
		t.Errorf("AlbumArtist: got %q, want %q", tr.AlbumArtist, "AA")
	}
	if tr.Album != "An Album" {
		t.Errorf("Album: got %q, want %q", tr.Album, "An Album")
	}
	if tr.Genre != "Jazz" {
		t.Errorf("Genre: got %q, want %q", tr.Genre, "Jazz")
	}
}

// --- Compilation safety net ---

// TestCompilationSynthesizesVariousArtistsWhenAlbumArtistMissing locks
// the safety-net synthesis: a track tagged with COMPILATION=1 (Vorbis
// flavour) and no AlbumArtist gets `AlbumArtist = "Various Artists"`.
// Pre-fix iOS fell back to per-track Artist for the missing
// albumArtist, fragmenting compilations into one Album row per
// artist. Per Gemini A6 / iOS bug review #6b.
func TestCompilationSynthesizesVariousArtistsWhenAlbumArtistMissing(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "comp.flac")
	writeMinimalFLAC(t, p, 44100, 16, map[string]string{
		"TITLE":       "Track 1",
		"ARTIST":      "Various Performer A",
		"ALBUM":       "Greatest Hits",
		"COMPILATION": "1",
	})
	tr := &Track{Path: "comp.flac", Size: 1, ModTime: time.Now()}
	if err := Extract(p, tr); err != nil {
		t.Fatal(err)
	}
	if tr.AlbumArtist != "Various Artists" {
		t.Errorf("AlbumArtist: got %q, want %q (synthesized from COMPILATION=1)",
			tr.AlbumArtist, "Various Artists")
	}
}

// TestCompilationDoesNotOverrideExplicitAlbumArtist — a tagger that
// SET the AlbumArtist explicitly (e.g. "Marc-André Hamelin" on a
// classical recital that's "compilation:1" because it spans labels)
// must keep that explicit value. The synth fires only on missing /
// empty AlbumArtist.
func TestCompilationDoesNotOverrideExplicitAlbumArtist(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "explicit.flac")
	writeMinimalFLAC(t, p, 44100, 16, map[string]string{
		"TITLE":       "Track 1",
		"ARTIST":      "Hamelin",
		"ALBUMARTIST": "Marc-André Hamelin",
		"ALBUM":       "Recital",
		"COMPILATION": "1",
	})
	tr := &Track{Path: "explicit.flac", Size: 1, ModTime: time.Now()}
	if err := Extract(p, tr); err != nil {
		t.Fatal(err)
	}
	if tr.AlbumArtist != "Marc-André Hamelin" {
		t.Errorf("explicit AlbumArtist clobbered: got %q, want %q",
			tr.AlbumArtist, "Marc-André Hamelin")
	}
}

// TestCompilationFlagAcceptsDifferentSpellings — TCMP (ID3v2),
// CPIL (iTunes / MP4), and COMPILATION (Vorbis) all carry the same
// semantic. Locks the case-agnostic stringOf lookup against every
// spelling.
func TestCompilationFlagAcceptsDifferentSpellings(t *testing.T) {
	cases := []struct {
		key, val string
	}{
		{"TCMP", "1"},
		{"CPIL", "1"},
		{"COMPILATION", "1"},
		{"compilation", "1"}, // lowercase Vorbis
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			raw := map[string]any{
				tc.key: tc.val,
			}
			tr := &Track{}
			if t1, ok := stringOf(raw, "tcmp", "cpil", "compilation"); !ok || t1 != "1" {
				t.Fatalf("stringOf with %s=%q: got (%q, %v)", tc.key, tc.val, t1, ok)
			}
			if tr.AlbumArtist == "Various Artists" {
				t.Errorf("setup error — AlbumArtist already set")
			}
		})
	}
}

// --- parseReplayGain ---

// TestParseReplayGain locks the suffix-strip + whitespace-trim
// behaviour around the float parse. Pre-fix the four sequential
// TrimSuffix calls (" dB" / " db" / "dB" / "db") only handled
// those exact case combinations; mixed case ("Db", "DB") fell
// through to ParseFloat and silently returned nil. The new form
// uses ToLower + a single TrimSuffix("db"), which handles every
// case combination AND removes redundant operations.
func TestParseReplayGain(t *testing.T) {
	cases := []struct {
		in   string
		want *float64
	}{
		{"-7.32 dB", floatPtr(-7.32)},
		{"-7.32 db", floatPtr(-7.32)},
		{"-7.32dB", floatPtr(-7.32)},
		{"-7.32db", floatPtr(-7.32)},
		{"-7.32 Db", floatPtr(-7.32)}, // pre-fix returned nil (silent miss)
		{"-7.32 DB", floatPtr(-7.32)}, // pre-fix returned nil
		{"  -7.32  dB  ", floatPtr(-7.32)},
		{"-7.32 dB ", floatPtr(-7.32)}, // trailing space after suffix
		{"-7.32", floatPtr(-7.32)},
		{"+5.5 dB", floatPtr(5.5)},
		{"0 dB", floatPtr(0)},
		{"", nil},
		{"   ", nil},
		{"abc", nil},
		{"dB", nil},
		{" dB", nil},
	}
	for _, tc := range cases {
		t.Run(strconv.Quote(tc.in), func(t *testing.T) {
			got := parseReplayGain(tc.in)
			switch {
			case got == nil && tc.want == nil:
				return
			case got == nil:
				t.Fatalf("got nil, want %v", *tc.want)
			case tc.want == nil:
				t.Fatalf("got %v, want nil", *got)
			case *got != *tc.want:
				t.Errorf("got %v, want %v", *got, *tc.want)
			}
		})
	}
}

// TestMultiValueArtistOverridesDhowdenFlattening covers the
// `applyFLACMultiValueArtists` path: a FLAC with two `ARTIST=` Vorbis
// Comments should land on the bridge as `"; "`-joined, NOT as the
// last value (which is what dhowden/tag's map-based parser collapses
// the multi-value tag set down to).
func TestMultiValueArtistOverridesDhowdenFlattening(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "duo.flac")
	writeMinimalFLACPairs(t, p, 44100, 16, [][2]string{
		{"TITLE", "Dreamtime"},
		{"ALBUM", "The Balance"},
		{"ARTIST", "Abdullah Ibrahim"},
		{"ARTIST", "Ekaya"},
	})
	tr := &Track{Path: "duo.flac", Size: 1, ModTime: time.Now()}
	if err := Extract(p, tr); err != nil {
		t.Fatal(err)
	}
	if tr.Artist != "Abdullah Ibrahim; Ekaya" {
		t.Errorf("Artist: got %q, want %q (multi-value ARTIST must join with \"; \" — pre-fix the last ARTIST won)", tr.Artist, "Abdullah Ibrahim; Ekaya")
	}
}

func TestMultiValueAlbumArtistOverridesDhowdenFlattening(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "compilation.flac")
	writeMinimalFLACPairs(t, p, 44100, 16, [][2]string{
		{"TITLE", "Various"},
		{"ALBUM", "Cool Reissue"},
		{"ARTIST", "Track Artist"},
		{"ALBUMARTIST", "Curator A"},
		{"ALBUMARTIST", "Curator B"},
	})
	tr := &Track{Path: "compilation.flac", Size: 1, ModTime: time.Now()}
	if err := Extract(p, tr); err != nil {
		t.Fatal(err)
	}
	if tr.AlbumArtist != "Curator A; Curator B" {
		t.Errorf("AlbumArtist: got %q, want %q", tr.AlbumArtist, "Curator A; Curator B")
	}
}

func TestSingleArtistFLACUnchanged(t *testing.T) {
	// Single-value ARTIST must pass through dhowden/tag unchanged —
	// the multi-value override is a strict superset that no-ops on
	// the common single-credit case.
	dir := t.TempDir()
	p := filepath.Join(dir, "solo.flac")
	writeMinimalFLAC(t, p, 44100, 16, map[string]string{
		"TITLE":  "Solo Track",
		"ARTIST": "Ekaya",
		"ALBUM":  "The Balance",
	})
	tr := &Track{Path: "solo.flac", Size: 1, ModTime: time.Now()}
	if err := Extract(p, tr); err != nil {
		t.Fatal(err)
	}
	if tr.Artist != "Ekaya" {
		t.Errorf("Artist: got %q, want %q (single-value path must not be touched by the multi-value override)", tr.Artist, "Ekaya")
	}
}

func TestMultiValueArtistTrimsWhitespacePerSegment(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "trimmy.flac")
	writeMinimalFLACPairs(t, p, 44100, 16, [][2]string{
		{"ARTIST", "  Abdullah Ibrahim  "},
		{"ARTIST", "\tEkaya\n"},
	})
	tr := &Track{Path: "trimmy.flac", Size: 1, ModTime: time.Now()}
	if err := Extract(p, tr); err != nil {
		t.Fatal(err)
	}
	if tr.Artist != "Abdullah Ibrahim; Ekaya" {
		t.Errorf("Artist: got %q, want %q (per-segment TrimSpace must run before the join)", tr.Artist, "Abdullah Ibrahim; Ekaya")
	}
}

func floatPtr(v float64) *float64 { return &v }

// Suppresses unused-import warning when running individual tests
// (atomic / time are used but the package's other test files cover
// some of these — keeping explicit references here makes the file
// self-contained without depending on testdata_test.go's ordering).
var _ = atomic.Bool{}
var _ = time.Now
