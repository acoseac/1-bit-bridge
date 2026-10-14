package manifest

import (
	"os"
	"path/filepath"
	"testing"
)

// TestScanner_AVersionStaleRereadOfID3v1AndWindows1252Stamps is backlog B181.
// An ID3v1 field is ISO-8859-1 and a RIFF INFO field is Windows-1252. Under
// go1.26.6, json.Marshal writes a byte that is not UTF-8 as the \ufffd escape
// and the decoded copy marshals that character raw, so the version-stale diff
// never matches and every later re-read takes the full upsert. A clean FLAC
// only stamps. The two tagged files must come back as the characters the
// formats define, and two re-reads after the version is rewound must stamp.
func TestScanner_AVersionStaleRereadOfID3v1AndWindows1252Stamps(t *testing.T) {
	root := t.TempDir()
	writeID3v1OnlyMP3(t, filepath.Join(root, "id3v1.mp3"), "Caf\xe9")
	wav := buildWAVWithListInfo(t, map[string]string{"INAM": "Caf\x80"})
	if err := os.WriteFile(filepath.Join(root, "info.wav"), wav, 0o644); err != nil {
		t.Fatal(err)
	}
	utf8WAV := buildWAVWithListInfo(t, map[string]string{"INAM": "Café"})
	if err := os.WriteFile(filepath.Join(root, "utf8.wav"), utf8WAV, 0o644); err != nil {
		t.Fatal(err)
	}
	writeMinimalFLAC(t, filepath.Join(root, "clean.flac"), 44100, 16, map[string]string{
		"TITLE": "Clean",
	})

	store, sc := newScanFixture(t, root)
	scanOnce(t, sc, "initial")

	want := map[string]string{
		"id3v1.mp3":  "Caf\u00e9",
		"info.wav":   "Caf\u20ac",
		"utf8.wav":   "Café",
		"clean.flac": "Clean",
	}
	for rel, title := range want {
		if got := storedTrack(t, store, rel).Title; got != title {
			t.Errorf("initial %s: title = %q, want %q", rel, got, title)
		}
	}

	before := map[string]int64{}
	for rel := range want {
		if _, err := store.db.Exec(
			`UPDATE tracks SET extractor_version = 0, enriched_at = 1 WHERE path = ?`, rel,
		); err != nil {
			t.Fatalf("rewind %s: %v", rel, err)
		}
		before[rel] = trackIndexedAt(t, store, rel)
	}
	scanOnce(t, sc, "stale-1")
	scanOnce(t, sc, "stale-2")
	for rel, title := range want {
		got := storedTrack(t, store, rel)
		if got.Title != title {
			t.Errorf("re-read %s: title = %q, want %q", rel, got.Title, title)
		}
		if v := trackColumn(t, store, rel, "extractor_version"); v != int64(ExtractorVersion) {
			t.Errorf("%s: extractor_version = %d, want %d", rel, v, ExtractorVersion)
		}
		if after := trackIndexedAt(t, store, rel); after != before[rel] {
			t.Errorf("%s: indexed_at moved %d -> %d", rel, before[rel], after)
		}
		if v := trackColumn(t, store, rel, "enriched_at"); v != 1 {
			t.Errorf("%s: enriched_at = %d, want 1", rel, v)
		}
	}
}

func TestDeclaredCharsetsAndTheUTF8Fallback(t *testing.T) {
	if got := latin1String("Caf\xe9"); got != "Café" {
		t.Fatalf("latin1 = %q", got)
	}
	if got := latin1String("Café"); got != "Café" {
		t.Fatalf("latin1 of UTF-8 = %q", got)
	}
	if got := windows1252Text([]byte("Caf\x80")); got != "Caf€" {
		t.Fatalf("windows-1252 = %q", got)
	}
	if got := windows1252Text([]byte("Café")); got != "Café" {
		t.Fatalf("windows-1252 of UTF-8 = %q", got)
	}
	if got := diinText([]byte("Caf\xe9")); got != "Café" {
		t.Fatalf("diin = %q", got)
	}
	tr := &Track{Title: "Caf\xff", Path: "Caf\xff.flac"}
	coerceStoredUTF8(tr)
	if tr.Title != "Caf\uFFFD" || tr.Path != "Caf\xff.flac" {
		t.Fatalf("fallback title=%q path=%q", tr.Title, tr.Path)
	}
}

// writeID3v1OnlyMP3 writes one MPEG frame and an ID3v1 tag, with no ID3v2
// header, so the title is read from the 30-byte ID3v1 field.
func writeID3v1OnlyMP3(t *testing.T, path, title string) {
	t.Helper()
	if len(title) > 30 {
		t.Fatalf("ID3v1 title is %d bytes, the field holds 30", len(title))
	}
	frame := []byte{0xFF, 0xFB, 0x90, 0x64}
	frame = append(frame, make([]byte, 140)...)
	tag := make([]byte, 128)
	copy(tag[0:3], "TAG")
	copy(tag[3:33], title)
	tag[127] = 255
	if err := os.WriteFile(path, append(frame, tag...), 0o644); err != nil {
		t.Fatal(err)
	}
}
