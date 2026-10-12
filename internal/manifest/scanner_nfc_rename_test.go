package manifest

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestScannerReapsTheUnseenCompositionOfOneFolder is the scanner pin
// for a folder whose rows exist under two spellings the fold treats
// as one name, of which the walk sees only one. The stale spelling is
// reaped as a rename on that full scan. A path the walk did not see
// at all stays through the missing-count grace (threshold 3, the
// production default). The unset threshold is 1, which would delete
// that control on the same scan and hide a fold that stopped composing.
func TestScannerReapsTheUnseenCompositionOfOneFolder(t *testing.T) {
	pairs := []struct {
		name string
		a, b string
	}{
		{"cafe", "Cafe\u0301", "Caf\u00e9"},
		{"dotted capital i", "I\u0307stanbul", "İstanbul"},
	}
	for _, p := range pairs {
		t.Run(p.name, func(t *testing.T) {
			root := t.TempDir()
			seedTrackDirs(t, filepath.Join(root, p.a))
			stored := soleDirName(t, root)
			staleName, ok := otherSpelling(stored, p.a, p.b)
			if !ok {
				t.Fatalf("volume stored %q, neither %q nor %q", stored, p.a, p.b)
			}
			s, sc := newScanFixture(t, root)
			sc.SetDeleteThreshold(3)
			scanOnce(t, sc, "index")
			live := stored + "/song.flac"
			stale := staleName + "/song.flac"
			mustIndexed(t, s, live)
			plantRow(t, s, stale)
			plantRow(t, s, "Gone/song.flac")

			scanOnce(t, sc, "reap")
			mustAbsent(t, s, stale)
			mustIndexed(t, s, live)
			mustIndexed(t, s, "Gone/song.flac")
		})
	}
}

// TestScannerLeavesAPlainIBesideDottedCapitalI pins the lowercase the
// reap uses. strings.ToLower maps U+0130 to a plain "i", so a fold
// built on it would reap an "istanbul" row because the walk saw
// "İstanbul". Those are two directories where the volume keeps both.
func TestScannerLeavesAPlainIBesideDottedCapitalI(t *testing.T) {
	root := t.TempDir()
	seedTrackDirs(t, filepath.Join(root, "İstanbul"))
	stored := soleDirName(t, root)
	if stored == "istanbul" {
		t.Skip("volume folded dotted capital i onto a plain i")
	}
	s, sc := newScanFixture(t, root)
	sc.SetDeleteThreshold(3)
	scanOnce(t, sc, "index")
	plantRow(t, s, "istanbul/song.flac")

	scanOnce(t, sc, "second")
	mustIndexed(t, s, stored+"/song.flac", "istanbul/song.flac")
}

// TestScannerLeavesBothCompositionSiblings is the case the reap must
// not treat as a rename: two directories whose names differ only by
// composition, both walked. A volume that stores one spelling of the
// name cannot stage the fixture.
func TestScannerLeavesBothCompositionSiblings(t *testing.T) {
	root := t.TempDir()
	const nfd, nfc = "Cafe\u0301", "Caf\u00e9"
	if !stageBothNames(t, root, nfd, nfc) {
		t.Skip("volume stores one spelling of this name")
	}
	seedTrackDirs(t, filepath.Join(root, nfd), filepath.Join(root, nfc))
	s, sc := newScanFixture(t, root)
	sc.SetDeleteThreshold(3)
	scanOnce(t, sc, "index")
	scanOnce(t, sc, "again")
	mustIndexed(t, s, nfd+"/song.flac", nfc+"/song.flac")
}

func soleDirName(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || !entries[0].IsDir() {
		t.Fatalf("%s listed %d entries, want one directory", dir, len(entries))
	}
	return entries[0].Name()
}

func otherSpelling(stored, a, b string) (string, bool) {
	switch stored {
	case a:
		return b, a != b
	case b:
		return a, a != b
	default:
		return "", false
	}
}

func stageBothNames(t *testing.T, root, a, b string) bool {
	t.Helper()
	if err := os.Mkdir(filepath.Join(root, a), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, b), 0o755); err != nil {
		if os.IsExist(err) {
			return false
		}
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() {
			have[e.Name()] = true
		}
	}
	return have[a] && have[b]
}

func plantRow(t *testing.T, s *Store, path string) {
	t.Helper()
	if err := s.UpsertTrack(context.Background(), &Track{
		Path:    path,
		Size:    1,
		ModTime: time.Unix(1, 0).UTC(),
	}); err != nil {
		t.Fatal(err)
	}
}

func mustAbsent(t *testing.T, s *Store, path string) {
	t.Helper()
	got, err := s.GetTrack(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("%q still in the manifest", path)
	}
}
