package manifest

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// newScanStore opens a throwaway manifest store for a scanner test.
func newScanStore(t *testing.T) *Store {
	t.Helper()
	store, err := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// scanRootOnce runs one full scan over `root` and returns the store
// plus the indexed-track count.
func scanRootOnce(t *testing.T, root string) (*Store, int) {
	t.Helper()
	store := newScanStore(t)
	n, err := NewScanner([]string{root}, store, "").Scan(context.Background())
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	return store, n
}

// A library root whose OWN basename starts with a dot must still be
// scanned. `filepath.WalkDir` invokes the walk callback for the root
// itself, so an unguarded `shouldSkipDir(d.Name())` returned SkipDir on
// the very first callback — which terminates the walk and returns nil.
// The result was 0 files indexed with NO error surfaced anywhere, and
// on a fresh install not even the `observed == 0` sentinel fires
// (that one only arms when the DB already holds rows).
//
// `/mnt/storage/.music`, `~/.library` and friends are legitimate
// operator choices; `internal/config` places no basename restriction on
// a root. The skip heuristic exists to prune DISCOVERED descendants
// (.Trash, .Spotlight-V100, $RECYCLE.BIN), never an explicitly
// configured path.
func TestScannerIndexesDotNamedLibraryRoot(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, ".music")
	album := filepath.Join(root, "Artist", "Album")
	if err := os.MkdirAll(album, 0o755); err != nil {
		t.Fatal(err)
	}
	writeMinimalAudio(t, filepath.Join(album, "01.flac"))
	writeMinimalAudio(t, filepath.Join(root, "loose.flac"))

	store, n := scanRootOnce(t, root)
	if n != 2 {
		t.Fatalf("indexed %d tracks, want 2 — a dot-named root must not skip its own walk", n)
	}
	for _, p := range []string{"Artist/Album/01.flac", "loose.flac"} {
		got, err := store.GetTrack(context.Background(), p)
		if err != nil {
			t.Fatalf("GetTrack %q: %v", p, err)
		}
		if got == nil {
			t.Fatalf("track %q missing from the manifest", p)
		}
	}
}

// The root exemption must NOT leak into descendants: a dot-directory
// found *inside* a root is still pruned. Without this the fix would
// start indexing .Trash / .Spotlight-V100 contents.
func TestScannerStillSkipsDotDirsBelowTheRoot(t *testing.T) {
	root := t.TempDir()
	trash := filepath.Join(root, ".Trash")
	if err := os.MkdirAll(trash, 0o755); err != nil {
		t.Fatal(err)
	}
	writeMinimalAudio(t, filepath.Join(trash, "deleted.flac"))
	writeMinimalAudio(t, filepath.Join(root, "kept.flac"))

	store, n := scanRootOnce(t, root)
	if n != 1 {
		t.Fatalf("indexed %d tracks, want 1 — dot-dirs BELOW the root must stay pruned", n)
	}
	if got, _ := store.GetTrack(context.Background(), ".Trash/deleted.flac"); got != nil {
		t.Fatal("a dot-directory below the root was indexed")
	}
}

// ScanSubtree carries the same walk-entry exemption. Reachable when a
// dot-named root is itself the subtree target (the watcher dispatches
// `filepath.Dir(ev.Name)`, which is the root for a file dropped
// directly into it).
func TestScanSubtreeIndexesDotNamedRoot(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, ".library")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	writeMinimalAudio(t, filepath.Join(root, "dropped.flac"))

	store := newScanStore(t)
	sc := NewScanner([]string{root}, store, "")
	if _, err := sc.ScanSubtree(context.Background(), root); err != nil {
		t.Fatalf("ScanSubtree: %v", err)
	}
	if got, _ := store.GetTrack(context.Background(), "dropped.flac"); got == nil {
		t.Fatal("ScanSubtree skipped a dot-named root instead of walking it")
	}
}
