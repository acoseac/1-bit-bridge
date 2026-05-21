package integrity

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"
)

// fakeSidecarLister is the test-side SidecarLister implementation.
// Returns a snapshot of the configured `known` set; thread-safe
// via the mutex so a test that mutates the snapshot mid-tick (to
// model concurrent UpsertVariant writes) doesn't trip the race
// detector.
type fakeSidecarLister struct {
	mu    sync.Mutex
	known map[string]struct{}
}

func (f *fakeSidecarLister) AllSidecarPaths(ctx context.Context) (map[string]struct{}, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]struct{}, len(f.known))
	for k := range f.known {
		out[k] = struct{}{}
	}
	return out, nil
}

// seedTestSidecarTree writes `n` `.flac` files into outputDir, named
// `<prefix><i>.flac` with `i` left-padded to 4 digits for lexical
// ordering predictability. Returns the absolute paths in lexical
// order. Test helper, not a sweeper concern.
func seedTestSidecarTree(t *testing.T, outputDir, prefix string, n int) []string {
	t.Helper()
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		t.Fatalf("mkdir output: %v", err)
	}
	paths := make([]string, n)
	for i := 0; i < n; i++ {
		// Pad to 4 digits so 100+ entries sort lexically correctly
		// (test files use up to 250 entries for the chunk-cap case).
		name := prefix
		istr := strconv.Itoa(i)
		for len(istr) < 4 {
			istr = "0" + istr
		}
		name += istr + ".flac"
		full := filepath.Join(outputDir, name)
		if err := os.WriteFile(full, []byte{0}, 0o644); err != nil {
			t.Fatalf("write %s: %v", full, err)
		}
		paths[i] = full
	}
	sort.Strings(paths)
	return paths
}

// TestShouldConsiderSidecarFile pins the pure file-type predicate.
// `.flac` is the only extension considered today; mismatched
// extensions (incl. case variation) are skipped.
func TestShouldConsiderSidecarFile(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/tmp/transcoded/Album/01.flac", true},
		{"/tmp/transcoded/Album/01.FLAC", false}, // case-sensitive ext match
		{"/tmp/transcoded/Album/01.flac.tmp", false},
		{"/tmp/transcoded/Album/01.mp3", false},
		{"/tmp/transcoded/Album/01.wav", false},
		{"/tmp/transcoded/Album/01", false},
		{"", false},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			if got := shouldConsiderSidecarFile(c.path); got != c.want {
				t.Errorf("shouldConsiderSidecarFile(%q) = %v, want %v",
					c.path, got, c.want)
			}
		})
	}
}

// TestOrphanSidecarSweeperTickUnlinksOrphans is the headline contract:
// files on disk that have no matching `track_variants.sidecar_path`
// entry get unlinked; files that ARE in the snapshot stay put.
func TestOrphanSidecarSweeperTickUnlinksOrphans(t *testing.T) {
	outputDir := t.TempDir()
	paths := seedTestSidecarTree(t, outputDir, "t", 6)

	// Half are "known" to the DB; the other half are orphans.
	known := map[string]struct{}{
		paths[0]: {},
		paths[1]: {},
		paths[2]: {},
	}
	lister := &fakeSidecarLister{known: known}

	s := NewOrphanSidecarSweeper(lister, outputDir, 1*time.Hour)
	unlinked := s.tick(context.Background())
	if unlinked != 3 {
		t.Errorf("unlinked = %d, want 3 (paths[3..5] are orphans)", unlinked)
	}
	// Verify the known files survived and the orphans are gone.
	for _, p := range paths[:3] {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("known sidecar %q was unlinked (should have survived): %v", p, err)
		}
	}
	for _, p := range paths[3:] {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("orphan %q was NOT unlinked: stat err=%v", p, err)
		}
	}
}

// TestOrphanSidecarSweeperRespectsChunkCap pins the chunked-walk
// contract: one tick processes at most `gcChunkSize` entries; the
// remainder is left for subsequent ticks. The cursor advances so
// the next tick doesn't re-walk the same prefix.
func TestOrphanSidecarSweeperRespectsChunkCap(t *testing.T) {
	outputDir := t.TempDir()
	// Seed gcChunkSize + 50 = 150 entries so we observably split
	// across two ticks. All orphan (empty known set).
	totalEntries := gcChunkSize + 50
	paths := seedTestSidecarTree(t, outputDir, "x", totalEntries)
	lister := &fakeSidecarLister{known: map[string]struct{}{}}

	s := NewOrphanSidecarSweeper(lister, outputDir, 1*time.Hour)

	tick1 := s.tick(context.Background())
	if tick1 != gcChunkSize {
		t.Errorf("tick1 unlinked = %d, want %d (chunk cap)", tick1, gcChunkSize)
	}
	if s.lastProcessedPath == "" {
		t.Errorf("cursor should be set after hitting chunk cap; got empty")
	}
	// The cursor should be the gcChunkSize-th path (the last one
	// processed this tick).
	wantCursor := paths[gcChunkSize-1]
	if s.lastProcessedPath != wantCursor {
		t.Errorf("cursor = %q, want %q (last processed path of tick1)",
			s.lastProcessedPath, wantCursor)
	}

	tick2 := s.tick(context.Background())
	want2 := totalEntries - gcChunkSize
	if tick2 != want2 {
		t.Errorf("tick2 unlinked = %d, want %d (remainder after chunk cap)",
			tick2, want2)
	}
	if s.lastProcessedPath != "" {
		t.Errorf("cursor should reset to empty after a full-tree pass; got %q",
			s.lastProcessedPath)
	}
}

// TestOrphanSidecarSweeperHonoursCancellation pins the ctx-cancel
// contract: a context cancelled mid-walk returns promptly. Important
// for graceful shutdown on libraries with hundreds of thousands of
// variants where one tick's walk could otherwise hold the process
// up for seconds.
func TestOrphanSidecarSweeperHonoursCancellation(t *testing.T) {
	outputDir := t.TempDir()
	seedTestSidecarTree(t, outputDir, "c", 200) // plenty to walk
	lister := &fakeSidecarLister{known: map[string]struct{}{}}
	s := NewOrphanSidecarSweeper(lister, outputDir, 1*time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel BEFORE the tick starts

	// The tick should not panic and should return quickly. We do
	// NOT assert "zero unlinks" here because between the snapshot
	// fetch and the ctx-check inside filepath.Walk's callback, the
	// walk may have processed a small number of entries — that's
	// fine; the contract is "returns promptly", not "processes
	// zero entries".
	done := make(chan int, 1)
	go func() {
		done <- s.tick(ctx)
	}()
	select {
	case <-done:
		// Good.
	case <-time.After(5 * time.Second):
		t.Fatal("tick did not return within 5s after ctx cancel")
	}
}

// TestOrphanSidecarSweeperStartIntervalZeroIsNoOp pins the disable
// path: interval ≤ 0 → no goroutine spawned. The stopFn is still
// safe to call (no-op).
func TestOrphanSidecarSweeperStartIntervalZeroIsNoOp(t *testing.T) {
	lister := &fakeSidecarLister{known: map[string]struct{}{}}
	s := NewOrphanSidecarSweeper(lister, t.TempDir(), 0)
	stop := s.Start(context.Background())
	// stopFn must be idempotent and safe.
	stop()
	stop()
}

// TestOrphanSidecarSweeperStartTickFires drives the watcher through
// one full Start → tick → stop cycle with the test seam. Confirms
// the goroutine wakes, fires the initial-boot tick, and stops on
// the returned stopFn.
func TestOrphanSidecarSweeperStartTickFires(t *testing.T) {
	outputDir := t.TempDir()
	seedTestSidecarTree(t, outputDir, "s", 3) // all orphan
	lister := &fakeSidecarLister{known: map[string]struct{}{}}
	s := NewOrphanSidecarSweeper(lister, outputDir, 1*time.Hour)

	tickFired := make(chan int, 1)
	s.SetOnTickComplete(func(unlinked int) {
		select {
		case tickFired <- unlinked:
		default:
			// drop subsequent ticks (we only care about the boot tick)
		}
	})

	stop := s.Start(context.Background())
	defer stop()

	select {
	case n := <-tickFired:
		if n != 3 {
			t.Errorf("boot-tick unlinked = %d, want 3", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("boot tick did not fire within 2s")
	}
}

// TestOrphanSidecarSweeperPreservesNonFlacFiles confirms the
// file-type filter: the sweeper does not touch `.tmp`, `.txt`,
// `.flac.partial`, or any non-`.flac` files in the variants
// directory. Defensive guard for operators who use the variants
// directory for related artifacts (less common, but the file-type
// filter shape makes the sweeper safe by construction).
func TestOrphanSidecarSweeperPreservesNonFlacFiles(t *testing.T) {
	outputDir := t.TempDir()
	// Mix .flac (orphan, will be unlinked) with various other
	// extensions (must survive).
	nonFlacFiles := []string{
		"readme.txt",
		"partial.flac.tmp",
		"backup.bak",
		"manifest.json",
	}
	for _, name := range nonFlacFiles {
		full := filepath.Join(outputDir, name)
		if err := os.WriteFile(full, []byte("test"), 0o644); err != nil {
			t.Fatalf("write %s: %v", full, err)
		}
	}
	flacOrphan := filepath.Join(outputDir, "track.flac")
	if err := os.WriteFile(flacOrphan, []byte{0}, 0o644); err != nil {
		t.Fatalf("write flac: %v", err)
	}

	lister := &fakeSidecarLister{known: map[string]struct{}{}}
	s := NewOrphanSidecarSweeper(lister, outputDir, 1*time.Hour)
	unlinked := s.tick(context.Background())

	if unlinked != 1 {
		t.Errorf("unlinked = %d, want 1 (only the .flac orphan)", unlinked)
	}
	for _, name := range nonFlacFiles {
		full := filepath.Join(outputDir, name)
		if _, err := os.Stat(full); err != nil {
			t.Errorf("non-flac file %q was unlinked (should have survived): %v",
				full, err)
		}
	}
	if _, err := os.Stat(flacOrphan); !os.IsNotExist(err) {
		t.Errorf("orphan .flac was NOT unlinked: stat err=%v", err)
	}
}
