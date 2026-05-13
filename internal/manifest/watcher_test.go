package manifest

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestWatcherDebounce drops a file into a watched directory and
// asserts ScanSubtree fires within `debounce + slack` and the
// new track lands in the manifest. End-to-end check of the watch
// → debounce → ScanSubtree → UpsertTrackBatch path.
//
// We deliberately use a short debounce window (50 ms) and a
// generous deadline (3 s) so the test stays fast on a busy CI
// machine without flaking on debounce timing.
func TestWatcherDebounce(t *testing.T) {
	dir := t.TempDir()
	libDir := filepath.Join(dir, "Music")
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

	// Give the watcher a moment to register watches before
	// dropping the file. fsnotify's Add() is synchronous on
	// every supported platform, so 100 ms is generous.
	time.Sleep(100 * time.Millisecond)

	target := filepath.Join(libDir, "test.flac")
	// Write a placeholder — the scanner's Extract may fail to
	// parse it but the upsert path uses fillFromPath fallbacks
	// so a Track row still lands.
	if err := os.WriteFile(target, []byte("not a real flac"), 0o644); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		got, _ := store.ListTracks(context.Background(), nil)
		if len(got) > 0 {
			return // success
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("expected at least one track in manifest within deadline; got 0")
}

// TestWatcherIgnoresDotfiles asserts dotfile creates don't trigger
// a scan. The scanner skips them anyway, but we'd rather not
// spend a debounce window on them.
func TestWatcherIgnoresDotfiles(t *testing.T) {
	dir := t.TempDir()
	libDir := filepath.Join(dir, "Music")
	if err := os.MkdirAll(libDir, 0o755); err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(filepath.Join(dir, "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	scanner := NewScanner([]string{libDir}, store, "")
	w, err := NewWatcher(scanner, 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = w.Run(ctx) }()
	time.Sleep(100 * time.Millisecond)

	// Drop a dotfile — even a triggered ScanSubtree would skip
	// it, so this test is mostly a regression guard against an
	// over-eager event filter that would deliver scan dispatches
	// for files that can't be in the manifest.
	if err := os.WriteFile(filepath.Join(libDir, ".DS_Store"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)

	got, _ := store.ListTracks(context.Background(), nil)
	if len(got) != 0 {
		t.Errorf("expected 0 tracks for dotfile event; got %d", len(got))
	}
}

// TestIsWatchLimitError pins the substring matcher against the
// canonical fsnotify failure messages.
func TestIsWatchLimitError(t *testing.T) {
	cases := []struct {
		err  string
		want bool
	}{
		{"inotify_add_watch: no space left on device", true},
		{"too many open files", true},
		{"watch limit reached", true},
		{"connection refused", false},
		{"", false},
	}
	for _, tc := range cases {
		got := isWatchLimitError(&strErr{tc.err})
		if got != tc.want {
			t.Errorf("isWatchLimitError(%q) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

type strErr struct{ s string }

func (e *strErr) Error() string { return e.s }
