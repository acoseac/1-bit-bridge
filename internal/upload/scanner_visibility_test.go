package upload

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// scanFolderPaths runs a REAL scanner over root and returns the folder rows it
// created. Folder rows are the right observable for "did the walker descend
// here": a track row additionally depends on tag extraction succeeding, which
// a fixture file will not do, whereas the folder row is written by the walker
// itself.
func scanFolderPaths(t *testing.T, root string) []string {
	t.Helper()
	store, err := manifest.OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	sc := manifest.NewScanner([]string{root}, store, t.TempDir())
	ctx := context.Background()
	if _, err := sc.Scan(ctx); err != nil {
		t.Fatalf("scan: %v", err)
	}
	paths, err := store.FolderPaths(ctx)
	if err != nil {
		t.Fatalf("folder paths: %v", err)
	}
	return paths
}
