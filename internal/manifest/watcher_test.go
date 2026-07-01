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

// TestWatcherScheduleScanCoalescesAndCleans drives repeated scheduleScan
// calls for one dir and asserts they collapse to a single pending entry
// (the reschedule path) that the fired callback then removes — the map
// self-cleans, no leaked timers. ctx is cancelled so the callback returns
// before ScanSubtree; only the debounce-map bookkeeping is exercised.
func TestWatcherScheduleScanCoalescesAndCleans(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(filepath.Join(dir, "bridge.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()
	scanner := NewScanner([]string{dir}, store, "")
	w, err := NewWatcher(scanner, 30*time.Millisecond)
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // callback skips the scan; we only check map bookkeeping.

	const target = "/watched/dir"
	for i := 0; i < 5; i++ {
		w.scheduleScan(ctx, target)
	}
	w.mu.Lock()
	n := len(w.pending)
	w.mu.Unlock()
	if n != 1 {
		t.Fatalf("pending entries = %d, want 1 (five events for one dir must coalesce)", n)
	}

	// After the debounce window the callback fires and removes the entry.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		w.mu.Lock()
		n = len(w.pending)
		w.mu.Unlock()
		if n == 0 {
			return // self-cleaned
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("pending map not cleaned after debounce; still %d entries", n)
}

// TestWatcherStaleTimerDoesNotEvictFreshEntry reproduces the debounce
// identity race: a timer fires, its callback parks on wt.mu, and while it
// is parked a FRESH entry is installed for the same dir. The stale
// callback must NOT delete the fresh entry. Pre-fix (unconditional
// delete) it did — which under an event storm let the map lose track of
// the live timer and spawn overlapping scans.
func TestWatcherStaleTimerDoesNotEvictFreshEntry(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(filepath.Join(dir, "bridge.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()
	scanner := NewScanner([]string{dir}, store, "")
	w, err := NewWatcher(scanner, 20*time.Millisecond)
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // stale callback returns before ScanSubtree.

	const target = "/watched/dir"
	w.scheduleScan(ctx, target) // arms ps1 (fires in 20ms)

	// Hold the lock across the debounce window so ps1's callback fires but
	// parks on wt.mu before it can run its delete check.
	w.mu.Lock()
	ps1 := w.pending[target]
	time.Sleep(60 * time.Millisecond) // > debounce: ps1 fires, callback parks on wt.mu.

	// Install a FRESH entry for the same dir (a long timer that won't fire
	// during the test), mimicking scheduleScan's Stop()==false path.
	ps2 := &pendingScan{}
	ps2.timer = time.AfterFunc(time.Hour, func() {})
	defer ps2.timer.Stop()
	w.pending[target] = ps2
	w.mu.Unlock() // release: ps1's parked callback now proceeds.

	if ps1 == ps2 {
		t.Fatal("test setup error: ps1 and ps2 must be distinct")
	}

	// Give the stale ps1 callback time to run its delete check + return.
	time.Sleep(150 * time.Millisecond)

	w.mu.Lock()
	got := w.pending[target]
	w.mu.Unlock()
	if got != ps2 {
		t.Fatalf("fresh entry evicted by the stale timer callback (got %p, want ps2 %p); identity guard failed", got, ps2)
	}
}
