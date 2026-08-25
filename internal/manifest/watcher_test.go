package manifest

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
)

// newWatcherFixture stands up a library directory, a store, and a
// Watcher over it. Extracted because four tests in this file (plus the
// dot-named-root case) were carrying byte-identical setup.
//
// `libName` is the library root's BASENAME — the dot-named-root test
// needs to control it, since that is precisely what it exercises.
func newWatcherFixture(t *testing.T, libName string, debounce time.Duration) (libDir string, store *Store, w *Watcher) {
	t.Helper()
	dir := t.TempDir()
	libDir = filepath.Join(dir, libName)
	if err := os.MkdirAll(libDir, 0o755); err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(filepath.Join(dir, "bridge.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	w, err = NewWatcher(NewScanner([]string{libDir}, store, ""), debounce)
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	return libDir, store, w
}

// waitForTrack polls until a track appears in the manifest, or fails
// with `msg` at the deadline.
func waitForTrack(t *testing.T, store *Store, msg string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		got, _ := store.ListTracks(context.Background(), nil)
		if len(got) > 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal(msg)
}

// TestWatcherDebounce drops a file into a watched directory and
// asserts ScanSubtree fires within `debounce + slack` and the
// new track lands in the manifest. End-to-end check of the watch
// → debounce → ScanSubtree → UpsertTrackBatch path.
//
// We deliberately use a short debounce window (50 ms) and a
// generous deadline (3 s) so the test stays fast on a busy CI
// machine without flaking on debounce timing.
func TestWatcherDebounce(t *testing.T) {
	libDir, store, w := newWatcherFixture(t, "Music", 50*time.Millisecond)
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

	waitForTrack(t, store, "expected at least one track in manifest within deadline; got 0")
}

// TestWatcherShutdownDrainsInflightScan is the B8 regression guard: Run's
// shutdown path (cancel → deferred cancelAllPending + waitForInflightScans)
// must return cleanly and NOT deadlock, so the caller can safely close the
// store the instant Run returns. A file drop arms + fires a debounced
// ScanSubtree; cancelling mid-flight exercises the new wait. Pre-fix, Run
// returned without waiting for the fired dispatch (which could then race the
// store close); the fix makes Run block on the in-flight scan — this test
// pins that the block terminates (no deadlock) and the scan's write landed.
func TestWatcherShutdownDrainsInflightScan(t *testing.T) {
	// Held open at the dispatch's tail so the cancel below provably lands
	// while a scan is still counted in flight. The previous shape slept
	// 100ms for the watches to register and 60ms for a 20ms debounce to
	// fire — which was wrong in both directions. Too short (routinely, on
	// Windows: slower fsnotify registration and ~15.6ms clock granularity)
	// and nothing had been dispatched, so the run failed with "expected
	// the in-flight scan's track to have landed". Too long — the ordinary
	// case — and the one-file scan had already FINISHED, scanWG was back
	// to zero, and the assertion passed without the drain doing anything
	// at all. So it was flaky and weak at once; this version is neither.
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once

	libDir, store, w := newWatcherFixture(t, "Music", 20*time.Millisecond)
	// Set BEFORE Run's goroutine starts: goroutine creation is the
	// happens-before edge that makes this write visible to the dispatch
	// goroutines without a race. (A package-level var could not offer
	// that — see the field's docblock.)
	w.afterDispatchHookForTests = func() {
		once.Do(func() { close(entered) })
		<-release
	}
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() { _ = w.Run(ctx); close(runDone) }()

	// Write until a dispatch actually happens, rather than sleeping a
	// guess at how long watch registration takes. Re-writing the same
	// path is harmless and re-arms the debounce, so a write that lands
	// before the watch exists simply costs one more iteration.
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for {
			select {
			case <-entered:
				return
			default:
			}
			_ = os.WriteFile(filepath.Join(libDir, "x.flac"), []byte("x"), 0o644)
			select {
			case <-entered:
				return
			case <-time.After(150 * time.Millisecond):
			}
		}
	}()
	t.Cleanup(func() { <-writerDone })

	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		close(release)
		t.Fatal("no subtree scan was ever dispatched — the watch never registered")
	}

	// A dispatch is now parked at its tail with scanWG at 1, and the scan
	// it just ran has already written. Cancel here is what the drain has
	// to survive.
	cancel()

	// The assertion the whole test exists for: Run MUST NOT return while
	// that dispatch is outstanding. The window is a heuristic but it fails
	// SAFE — a broken drain returns in microseconds, and a slow machine
	// only makes "has not returned yet" more true, never less.
	select {
	case <-runDone:
		close(release)
		t.Fatal("Run returned while a dispatch was still in flight — " +
			"waitForInflightScans did not wait")
	case <-time.After(150 * time.Millisecond):
	}

	close(release)
	select {
	case <-runDone: // Run's defer (cancelAllPending + waitForInflightScans) completed
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return once the in-flight scan finished — " +
			"waitForInflightScans deadlocked")
	}
	// The dispatched scan completed before Run returned (Run waited for it),
	// so its UpsertTrackBatch landed against the still-open store.
	got, _ := store.ListTracks(context.Background(), nil)
	if len(got) == 0 {
		t.Error("expected the in-flight scan's track to have landed before Run returned")
	}
}

// TestWatcherIgnoresDotfiles asserts dotfile creates don't trigger
// a scan. The scanner skips them anyway, but we'd rather not
// spend a debounce window on them.
func TestWatcherIgnoresDotfiles(t *testing.T) {
	libDir, store, w := newWatcherFixture(t, "Music", 50*time.Millisecond)
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
// canonical fsnotify WATCH-budget messages. fd-exhaustion ("too many
// open files") is deliberately NOT a watch-limit error — it routes to
// isOpenFileLimitError so addTree emits the ulimit-oriented hint
// instead of the max_user_watches one.
func TestIsWatchLimitError(t *testing.T) {
	cases := []struct {
		err  string
		want bool
	}{
		{"inotify_add_watch: no space left on device", true},
		{"too many open files", false}, // fd-exhaustion, not a watch-limit — see isOpenFileLimitError
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

// TestIsOpenFileLimitError pins the fd-exhaustion classifier that B9
// split out of isWatchLimitError. Only "too many open files"
// (EMFILE/ENFILE) matches; the watch-budget messages must NOT, so the
// two paths emit distinct operator hints.
func TestIsOpenFileLimitError(t *testing.T) {
	cases := []struct {
		err  string
		want bool
	}{
		{"pipe: too many open files", true},
		{"too many open files", true},
		{"inotify_add_watch: no space left on device", false},
		{"watch limit reached", false},
		{"connection refused", false},
		{"", false},
	}
	for _, tc := range cases {
		got := isOpenFileLimitError(&strErr{tc.err})
		if got != tc.want {
			t.Errorf("isOpenFileLimitError(%q) = %v, want %v", tc.err, got, tc.want)
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
	defer w.w.Close() // NewWatcher allocates an fsnotify.Watcher; these tests don't Run() it.

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
	defer w.w.Close() // NewWatcher allocates an fsnotify.Watcher; these tests don't Run() it.

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

// addTree must register watches for a library root whose OWN basename
// starts with a dot.
//
// filepath.WalkDir calls the callback for the root itself, so an
// unguarded shouldSkipDir(d.Name()) returned SkipDir on entry and the
// whole root got ZERO watches. Worse, addTree then returns nil, so the
// caller's "initial watch add failed (partial coverage)" warning never
// fired either — the library silently lost instant-update coverage with
// no operator signal at all.
//
// End-to-end rather than a unit test on addTree: what matters is that a
// file dropped into the root actually reaches the manifest.
func TestWatcherWatchesDotNamedLibraryRoot(t *testing.T) {
	libDir, store, w := newWatcherFixture(t, ".music", 50*time.Millisecond)
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

	waitForTrack(t, store, "no watch registered for a dot-named library root: "+
		"the dropped file never reached the manifest")
}

// A dot-directory that appears at RUNTIME gets no carve-out — the
// exemption belongs to operator-configured roots only.
//
// handleEvent's Create branch calls addTree with the just-created
// directory as its `root`, so the old `path != root` form exempted every
// such directory from itself: a `.Trashes` / `.stversions` that showed up
// under a watched library got a watch, a later event under it dispatched
// ScanSubtree INSIDE it (whose own walker exempts the directory it was
// pointed at), and its files were indexed as
// `.Trashes/501/Album/track.flac`. The full Scan never sees those paths —
// shouldSkipDir prunes them as descendants — so they accrued
// missing_count, were reaped three scans later, and reappeared on the
// next drop: deleted albums cycling in and out of /v1/manifest.
//
// Driven through handleEvent rather than a raw addTree call so the
// wiring (which caller passes which intent) is what's pinned. WatchList
// is the assertion because "is a watch registered here" is exactly the
// decision under test; everything downstream follows from it.
func TestWatcherRuntimeDotDirGetsNoRootExemption(t *testing.T) {
	libDir, _, w := newWatcherFixture(t, "Music", time.Hour)
	t.Cleanup(func() {
		w.cancelAllPending()
		_ = w.w.Close()
	})

	trash := filepath.Join(libDir, ".Trashes")
	if err := os.MkdirAll(filepath.Join(trash, "501"), 0o755); err != nil {
		t.Fatal(err)
	}

	// The watcher sees the directory appear under an already-watched root.
	w.handleEvent(context.Background(), fsnotify.Event{Name: trash, Op: fsnotify.Create})

	for _, p := range w.w.WatchList() {
		if p == trash || strings.HasPrefix(p, trash+string(os.PathSeparator)) {
			t.Fatalf("watch registered inside a runtime-created dot-directory: %q "+
				"— events from there dispatch ScanSubtree into it and index its files", p)
		}
	}

	// Contrast, and the reason the parameter exists rather than a blanket
	// skip: the same directory AS a configured root is still watched.
	if err := w.addTree(trash, true); err != nil {
		t.Fatalf("addTree(configured root): %v", err)
	}
	var watched bool
	for _, p := range w.w.WatchList() {
		if p == trash {
			watched = true
		}
	}
	if !watched {
		t.Error("a dot-named CONFIGURED root must still be watched")
	}
}
