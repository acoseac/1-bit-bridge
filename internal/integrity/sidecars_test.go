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

// seedSidecarAlbum writes n `.flac` files into outputDir/<album>/, named
// 0i.flac, and returns their absolute paths. Companion to seedTestSidecarTree
// for NESTED-tree tests (the flat helper can't exercise directory SkipDir).
// Extracted from the SkipDir test to keep that test's cognitive complexity
// under the gate.
func seedSidecarAlbum(t *testing.T, outputDir, album string, n int) []string {
	t.Helper()
	dir := filepath.Join(outputDir, album)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	out := make([]string, n)
	for i := 0; i < n; i++ {
		p := filepath.Join(dir, "0"+strconv.Itoa(i)+".flac")
		if err := os.WriteFile(p, []byte{0}, 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
		out[i] = p
	}
	return out
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
	// Bypass the 10-minute production grace floor — the seeded files
	// are brand new (modtime ≈ now), and the race-protection
	// regression has its own dedicated test below.
	s.gracePeriodForTest = 1 * time.Nanosecond
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
	// Test-local chunk size so we don't have to seed gcChunkSize (5000)
	// files for every run. 100 is small enough for sub-second test
	// wall-clock; the chunked-walk + cursor contract is the same shape
	// regardless of the constant. The chunkSizeForTest seam matches
	// the gracePeriodForTest convention.
	const testChunk = 100
	totalEntries := testChunk + 50 // 150 entries → splits into 100 + 50
	paths := seedTestSidecarTree(t, outputDir, "x", totalEntries)
	lister := &fakeSidecarLister{known: map[string]struct{}{}}

	s := NewOrphanSidecarSweeper(lister, outputDir, 1*time.Hour)
	s.chunkSizeForTest = testChunk
	// Same bypass as TestOrphanSidecarSweeperTickUnlinksOrphans —
	// freshly-seeded files would otherwise hit the 10-min grace floor.
	s.gracePeriodForTest = 1 * time.Nanosecond

	tick1 := s.tick(context.Background())
	if tick1 != testChunk {
		t.Errorf("tick1 unlinked = %d, want %d (chunk cap)", tick1, testChunk)
	}
	if s.lastProcessedPath == "" {
		t.Errorf("cursor should be set after hitting chunk cap; got empty")
	}
	// The cursor should be the testChunk-th path (the last one
	// processed this tick).
	wantCursor := paths[testChunk-1]
	if s.lastProcessedPath != wantCursor {
		t.Errorf("cursor = %q, want %q (last processed path of tick1)",
			s.lastProcessedPath, wantCursor)
	}

	tick2 := s.tick(context.Background())
	want2 := totalEntries - testChunk
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
	s.gracePeriodForTest = 1 * time.Nanosecond

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
	s.gracePeriodForTest = 1 * time.Nanosecond

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
	s.gracePeriodForTest = 1 * time.Nanosecond
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

// TestOrphanSidecarSweeperGracePeriodProtectsConcurrentWrites is the
// race-condition regression test Gemini HIGH on PR #282 asked for.
// A concurrent `UpsertVariant` writer lands the sidecar on disk
// BEFORE its row commits to `track_variants`. If the sweeper takes
// its snapshot DURING that window, the file looks orphan; without
// the grace-period gate, the sweeper would unlink it behind the
// writer's in-flight transaction.
//
// Contract: a fresh file (modtime < grace) is skipped REGARDLESS of
// whether the snapshot contains its path. Both branches must respect
// the grace:
//   - Sidecar present in known-set: protected by the known-set check.
//   - Sidecar absent from known-set + modtime < grace: protected by
//     the grace gate (this test).
//   - Sidecar absent + modtime >= grace: ordinary orphan, unlinked.
//
// Test shape: seed one fresh file (modtime ≈ now), set
// gracePeriodForTest to a value longer than test wall-clock (5s is
// safe), assert the file SURVIVES the sweep. Then backdate the file
// past the grace, sweep again, assert it's gone.
func TestOrphanSidecarSweeperGracePeriodProtectsConcurrentWrites(t *testing.T) {
	outputDir := t.TempDir()
	flacFile := filepath.Join(outputDir, "in-flight.flac")
	if err := os.WriteFile(flacFile, []byte{0}, 0o644); err != nil {
		t.Fatalf("write fresh file: %v", err)
	}

	// Empty known-set: the file is NOT in track_variants (writer's
	// row hasn't committed yet). Without the grace gate, the sweeper
	// would unlink immediately.
	lister := &fakeSidecarLister{known: map[string]struct{}{}}
	s := NewOrphanSidecarSweeper(lister, outputDir, 1*time.Hour)
	// Grace period longer than test wall-clock — the fresh file's
	// modtime (just now) is firmly inside the grace window.
	s.gracePeriodForTest = 5 * time.Second

	unlinked := s.tick(context.Background())
	if unlinked != 0 {
		t.Errorf("tick with fresh in-flight file unlinked %d files; want 0 "+
			"(grace period should protect concurrent UpsertVariant writes)",
			unlinked)
	}
	if _, err := os.Stat(flacFile); err != nil {
		t.Errorf("fresh in-flight file %q was unlinked (grace period "+
			"should have protected it): %v", flacFile, err)
	}

	// Now backdate the file past the grace window. Real-world this
	// is the "transaction committed long ago but the row was later
	// deleted, leaving the sidecar truly orphaned" case.
	pastModTime := time.Now().Add(-10 * time.Second)
	if err := os.Chtimes(flacFile, pastModTime, pastModTime); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	unlinked = s.tick(context.Background())
	if unlinked != 1 {
		t.Errorf("tick with backdated orphan unlinked %d files; want 1",
			unlinked)
	}
	if _, err := os.Stat(flacFile); !os.IsNotExist(err) {
		t.Errorf("backdated orphan %q was NOT unlinked: stat err=%v",
			flacFile, err)
	}
}

// TestDirEntirelyBehindCursor pins the chunk-resume SkipDir predicate,
// including the '.'-vs-separator collation gotcha that a bare-name compare
// would get wrong. Native separators throughout (portable across POSIX/Windows
// because '/' and '\' both sort after '.').
func TestDirEntirelyBehindCursor(t *testing.T) {
	sep := string(filepath.Separator)
	cases := []struct {
		name   string
		dir    string
		cursor string
		want   bool
	}{
		{"empty cursor never skips", "A", "", false},
		{"straddling sibling fully behind", "A", "B" + sep + "c.flac", true},
		{"ancestor of cursor must descend", "A" + sep + "B", "A" + sep + "B" + sep + "c.flac", false},
		{"dot-vs-sep: dir A/B with cursor A/B.flac must descend", "A" + sep + "B", "A" + sep + "B.flac", false},
		{"later sibling not behind", "C", "B" + sep + "c.flac", false},
		{"earlier sibling fully behind", "AlbumA", "AlbumB" + sep + "01.flac", true},
		{"root dir is ancestor of in-tree cursor", "out", "out" + sep + "A" + sep + "x.flac", false},
		{"volume/filesystem root is ancestor of in-tree cursor", sep, sep + "A" + sep + "x.flac", false},
		{"trailing-separator dir is ancestor of in-tree cursor", "out" + sep, "out" + sep + "A" + sep + "x.flac", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := dirEntirelyBehindCursor(c.dir, c.cursor); got != c.want {
				t.Errorf("dirEntirelyBehindCursor(%q, %q) = %v, want %v", c.dir, c.cursor, got, c.want)
			}
		})
	}
}

// TestOrphanSidecarSweeperSkipDirResumesWithoutMissingOrphans is the headline
// PR D contract: across a chunk-split sweep of a NESTED tree, (a) an orphan in
// a not-yet-covered subtree is still found and unlinked — proving SkipDir never
// prunes a live branch (under-sweep guard) — and (b) SkipDir actually fires on a
// fully-behind-cursor subtree on a resume tick (the O(N²) short-circuit engaged).
//
// The existing chunk-cap test uses a FLAT tree, where the only directory is the
// root (always an ancestor of the cursor → never pruned), so it can't exercise
// SkipDir at all. This test uses album subdirectories.
func TestOrphanSidecarSweeperSkipDirResumesWithoutMissingOrphans(t *testing.T) {
	outputDir := t.TempDir()
	// Lexical full-path order: AlbumA/00,01  AlbumB/00,01  AlbumC/00
	a := seedSidecarAlbum(t, outputDir, "AlbumA", 2)
	b := seedSidecarAlbum(t, outputDir, "AlbumB", 2)
	c := seedSidecarAlbum(t, outputDir, "AlbumC", 1) // the orphan album

	// AlbumA + AlbumB known; AlbumC's lone file is the orphan.
	known := map[string]struct{}{a[0]: {}, a[1]: {}, b[0]: {}, b[1]: {}}
	lister := &fakeSidecarLister{known: known}
	s := NewOrphanSidecarSweeper(lister, outputDir, 1*time.Hour)
	s.chunkSizeForTest = 2 // forces 3 ticks: A/* | B/* | C/*
	s.gracePeriodForTest = 1 * time.Nanosecond

	total := 0
	for i := 0; i < 10; i++ { // bounded; sweep completes in 3
		total += s.tick(context.Background())
		if s.lastProcessedPath == "" {
			break // cursor reset → full tree covered
		}
	}

	if total != 1 {
		t.Errorf("total unlinked across sweep = %d, want 1 (only the AlbumC orphan)", total)
	}
	if _, err := os.Stat(c[0]); !os.IsNotExist(err) {
		t.Errorf("orphan %q survived the chunked sweep — SkipDir wrongly pruned a live subtree (under-swept): err=%v", c[0], err)
	}
	for _, p := range append(append([]string{}, a...), b...) {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("known sidecar %q was unlinked (should have survived): %v", p, err)
		}
	}
	if got := s.skippedDirsForTest.Load(); got < 1 {
		t.Errorf("skippedDirsForTest = %d, want >= 1 (SkipDir short-circuit never engaged across the resume ticks)", got)
	}
}

// TestOrphanSidecarSweeperEffectiveOverrides locks the test-seam
// helpers' contract — production constants when override is zero or
// negative, override value when positive. Defensive against an
// accidental shape change to `effectiveGracePeriod` /
// `effectiveChunkSize` that would silently bypass the production
// floor.
func TestOrphanSidecarSweeperEffectiveOverrides(t *testing.T) {
	cases := []struct {
		name          string
		override      time.Duration
		wantGrace     time.Duration
		chunkOverride int
		wantChunkSize int
	}{
		{"zero-uses-production", 0, gcGracePeriod, 0, gcChunkSize},
		{"negative-uses-production", -1 * time.Second, gcGracePeriod, -5, gcChunkSize},
		{"positive-override-wins", 250 * time.Millisecond, 250 * time.Millisecond, 250, 250},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := &OrphanSidecarSweeper{
				gracePeriodForTest: c.override,
				chunkSizeForTest:   c.chunkOverride,
			}
			if got := s.effectiveGracePeriod(); got != c.wantGrace {
				t.Errorf("effectiveGracePeriod = %v, want %v", got, c.wantGrace)
			}
			if got := s.effectiveChunkSize(); got != c.wantChunkSize {
				t.Errorf("effectiveChunkSize = %d, want %d", got, c.wantChunkSize)
			}
		})
	}
}
