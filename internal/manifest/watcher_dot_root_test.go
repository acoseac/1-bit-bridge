package manifest

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// addTree must register watches for a library root whose own basename
// starts with a dot. `filepath.WalkDir` calls the callback for the root
// itself, so an unguarded `shouldSkipDir(d.Name())` returned SkipDir on
// entry and the whole root got ZERO watches — and because addTree then
// returns nil, the caller's "initial watch add failed (partial
// coverage)" warning never fired either. The library silently lost
// instant-update coverage with no operator signal at all.
//
// End-to-end rather than a unit test on addTree: what matters is that a
// file dropped into the root actually reaches the manifest.
func TestWatcherWatchesDotNamedLibraryRoot(t *testing.T) {
	dir := t.TempDir()
	libDir := filepath.Join(dir, ".music")
	if err := os.MkdirAll(libDir, 0o755); err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(filepath.Join(dir, "bridge.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	scanner := NewScanner([]string{libDir}, store, "")
	w, err := NewWatcher(scanner, 50*time.Millisecond)
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = w.Run(ctx) }()

	// fsnotify's Add() is synchronous on every supported platform, so
	// this is generous headroom for the initial addTree pass.
	time.Sleep(100 * time.Millisecond)

	if err := os.WriteFile(filepath.Join(libDir, "dropped.flac"),
		[]byte("not a real flac"), 0o644); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if got, _ := store.GetTrack(context.Background(), "dropped.flac"); got != nil {
			return // watch fired, subtree scanned, row landed
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("no watch registered for a dot-named library root: " +
		"the dropped file never reached the manifest")
}
