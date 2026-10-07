package manifest

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// A decomposed album name and the precomposed spelling of the same name
// are one path to the subtree lookup, which folds through unicode_lower.
// A full scan's in-memory fold has to do the same, or the upgrade scan
// dates the walked spelling at the scan clock and the subtree scan
// keeps the old date. Both arms copy the date the row already had.
func TestADecomposedAlbumTakesThePrecomposedDate(t *testing.T) {
	const (
		nfc = "Caf\u00e9"
		nfd = "Cafe\u0301"
	)
	if !volumeKeepsDecomposedName(t) {
		t.Skip("this volume does not store a decomposed directory name")
	}
	ctx := context.Background()
	nfcPath := "Artist/" + nfc + "/song.flac"
	nfdPath := "Artist/" + nfd + "/song.flac"
	for _, arm := range []string{"full", "subtree"} {
		t.Run(arm, func(t *testing.T) {
			root := t.TempDir()
			store, sc := newScanFixture(t, root)
			clockAt(store, reproT1)
			if err := store.UpsertTrack(ctx, &Track{
				Path: nfcPath, Size: 14, ModTime: reproT1,
			}); err != nil {
				t.Fatal(err)
			}
			seedTrackDirs(t, filepath.Join(root, "Artist", nfd))
			clockAt(store, reproT2)
			if arm == "full" {
				scanOnce(t, sc, "decomposed")
			} else if _, err := sc.ScanSubtree(ctx, root); err != nil {
				t.Fatal(err)
			}
			got, err := store.GetTrack(ctx, nfdPath)
			if err != nil {
				t.Fatal(err)
			}
			if got == nil || got.FirstIndexedAt == nil || !got.FirstIndexedAt.Equal(reproT1) {
				gotAt := "<nil>"
				if got != nil && got.FirstIndexedAt != nil {
					gotAt = got.FirstIndexedAt.UTC().Format("2006-01-02T15:04:05Z")
				}
				t.Fatalf("%s scan dated %s %s, want %s", arm, nfdPath, gotAt, reproT1.Format("2006-01-02T15:04:05Z"))
			}
		})
	}
}

func volumeKeepsDecomposedName(t *testing.T) bool {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "Cafe\u0301")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	parent := filepath.Dir(dir)
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() == "Cafe\u0301" {
			return true
		}
	}
	return false
}
