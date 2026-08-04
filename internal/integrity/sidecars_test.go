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

// ageFixtures back-dates every file under dir so the sweeper's
// grace-period check sees them as settled.
//
// The tests used `gracePeriodForTest = 1ns` to mean "nothing is too
// fresh to sweep", which relies on `tickStart.Sub(info.ModTime())` being
// positive for a file written moments earlier. That does not hold on
// Windows: NTFS stamps come from the system clock, whose granularity is
// ~15ms, while Go's time.Now() reads a high-resolution source — so a
// just-written file can carry a stamp AHEAD of a tickStart sampled
// afterwards. The difference goes negative, every candidate looks
// too-fresh, and the sweeper correctly skips all of them
// (`examined=1 unlinked=0`).
//
// Back-dating states the precondition directly instead of inferring it
// from a clock race, which is both portable and a better description of
// what these tests mean: a sidecar older than the grace period is
// sweepable. Production grace is minutes, so the skew never mattered
// there.
func ageFixtures(t *testing.T, dir string) {
	t.Helper()
	old := time.Now().Add(-1 * time.Hour)
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		return os.Chtimes(path, old, old)
	})
	if err != nil {
		t.Fatalf("age fixtures under %q: %v", dir, err)
	}
}

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
	ageFixtures(t, outputDir)
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

// A live sidecar whose DB SidecarPath differs from its on-disk (WalkDir)
// path only in case must NOT be unlinked on a case-insensitive FS. The
// known-set is case-folded, so the mixed-case on-disk file still matches;
// on the pre-fix raw lookup the file was treated as orphan and deleted.
func TestOrphanSidecarSweeperCaseInsensitive(t *testing.T) {
	outputDir := t.TempDir()
	dir := filepath.Join(outputDir, "Artist", "Album")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	sidecar := filepath.Join(dir, "Track.flac")
	if err := os.WriteFile(sidecar, []byte{0}, 0o644); err != nil {
		t.Fatal(err)
	}
	// DB records the SAME file under a different-cased path (as if written
	// on a case-insensitive FS with mixed casing).
	dbPath := filepath.Join(outputDir, "artist", "album", "track.flac")
	lister := &fakeSidecarLister{known: map[string]struct{}{dbPath: {}}}

	s := NewOrphanSidecarSweeper(lister, outputDir, 1*time.Hour)
	s.gracePeriodForTest = 1 * time.Nanosecond
	ageFixtures(t, outputDir)
	if unlinked := s.tick(context.Background()); unlinked != 0 {
		t.Errorf("unlinked = %d, want 0 (live sidecar with a case-only DB delta)", unlinked)
	}
	if _, err := os.Stat(sidecar); err != nil {
		t.Errorf("live sidecar %q was unlinked over a casing delta: %v", sidecar, err)
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
	ageFixtures(t, outputDir)

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
	ageFixtures(t, outputDir)

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
	ageFixtures(t, outputDir)

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
	ageFixtures(t, outputDir)
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
		// A dir is fully walked BEFORE its sibling file (base names "B" < "B.flac"),
		// so a cursor on "A/B.flac" means "A/B" is already swept → prune. (The prior
		// implementation left this un-pruned on the mistaken belief that "A/B/02.flac"
		// sorts after "A/B.flac" — it's visited before.)
		{"dir A/B fully behind sibling file A/B.flac", "A" + sep + "B", "A" + sep + "B.flac", true},
		{"later sibling not behind", "C", "B" + sep + "c.flac", false},
		{"earlier sibling fully behind", "AlbumA", "AlbumB" + sep + "01.flac", true},
		// The bridge02-04 regression: WalkDir visits all of "A/" before sibling
		// "A-Bonus/" (base names "A" < "A-Bonus"), so "A-Bonus" is still unwalked when
		// the cursor sits inside "A" — must NOT be pruned even though "A-Bonus/…" <
		// "A/…" as a raw string ('-' < '/').
		{"sibling A-Bonus after A must descend (dash<sep)", "A-Bonus", "A" + sep + "track.flac", false},
		{"sibling 'A B' after A must descend (space<sep)", "A B", "A" + sep + "track.flac", false},
		{"A-Bonus genuinely behind when cursor in later sibling", "A-Bonus", "B" + sep + "x.flac", true},
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
	ageFixtures(t, outputDir)

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

// TestOrphanSidecarSweeper_SiblingDashDir_NotPruned is the bridge02-04 regression:
// a sibling album directory whose name sorts BEFORE an earlier-walked sibling as a raw
// string ("A-Bonus" vs "A/…", '-' 0x2D < '/' 0x2F) must NOT be pruned by the chunk-
// resume SkipDir short-circuit — filepath.WalkDir visits "A-Bonus/" AFTER all of "A/",
// so its orphan is still unswept when the cursor sits inside "A". Fails on the pre-fix
// raw-string dirEntirelyBehindCursor (the orphan survives the sweep).
func TestOrphanSidecarSweeper_SiblingDashDir_NotPruned(t *testing.T) {
	outputDir := t.TempDir()
	// Walk order (base-name sort): A/ (whole subtree) THEN A-Bonus/.
	a := seedSidecarAlbum(t, outputDir, "A", 2)           // both known
	bonus := seedSidecarAlbum(t, outputDir, "A-Bonus", 1) // the orphan

	known := map[string]struct{}{a[0]: {}, a[1]: {}}
	lister := &fakeSidecarLister{known: known}
	s := NewOrphanSidecarSweeper(lister, outputDir, 1*time.Hour)
	s.chunkSizeForTest = 2 // tick 1 fills on A/*, forcing a resume that must reach A-Bonus
	s.gracePeriodForTest = 1 * time.Nanosecond
	ageFixtures(t, outputDir)

	total := 0
	for i := 0; i < 10; i++ { // bounded; sweep completes in 2 ticks
		total += s.tick(context.Background())
		if s.lastProcessedPath == "" {
			break // cursor reset → full tree covered
		}
	}

	if total != 1 {
		t.Errorf("total unlinked = %d, want 1 (the A-Bonus orphan)", total)
	}
	if _, err := os.Stat(bonus[0]); !os.IsNotExist(err) {
		t.Errorf("orphan %q survived — SkipDir wrongly pruned the A-Bonus subtree (raw-string collation bug): err=%v", bonus[0], err)
	}
	for _, p := range a {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("known sidecar %q was unlinked (should have survived): %v", p, err)
		}
	}
}

// TestPathWalkCompare_MatchesActualWalkDirOrder empirically locks the helper against
// filepath.WalkDir's real traversal order: it builds a tree seeded with dir/file
// collation traps (sibling names with '-' and ' '; a dir vs a sibling file of the same
// stem; nested subtrees), records the ACTUAL visit order, and asserts pathWalkCompare
// reproduces it (strictly increasing across every consecutive pair).
func TestPathWalkCompare_MatchesActualWalkDirOrder(t *testing.T) {
	root := t.TempDir()
	mk := func(rel string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", p, err)
		}
		if err := os.WriteFile(p, []byte{0}, 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	for _, rel := range []string{
		"A/00.flac", "A/01.flac",
		"A B/00.flac",     // space sibling (' ' < '/')
		"A-Bonus/00.flac", // dash sibling ('-' < '/')
		"B/nested/00.flac",
		"B/nested.flac", // file sibling of the "nested" dir ('.' < '/')
		"B.flac",        // file sibling of the "B" dir
	} {
		mk(rel)
	}

	var order []string
	if err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		order = append(order, p)
		return nil
	}); err != nil {
		t.Fatalf("walk: %v", err)
	}

	for i := 0; i+1 < len(order); i++ {
		if got := pathWalkCompare(order[i], order[i+1]); got >= 0 {
			t.Errorf("pathWalkCompare(%q, %q) = %d, want < 0 (WalkDir visited them in this order)",
				order[i], order[i+1], got)
		}
	}
	// Sanity: equal compares 0; the compare is antisymmetric.
	if got := pathWalkCompare(order[0], order[0]); got != 0 {
		t.Errorf("pathWalkCompare(x, x) = %d, want 0", got)
	}
	if len(order) > 1 {
		if a, b := pathWalkCompare(order[0], order[1]), pathWalkCompare(order[1], order[0]); a != -b {
			t.Errorf("not antisymmetric: cmp(a,b)=%d cmp(b,a)=%d", a, b)
		}
	}
}

// TestPathWalkCompare_ZeroAlloc locks the zero-alloc contract (see the helper doc): the
// function runs on every walk entry until the resume cursor clears, so a future refactor
// to strings.Split would reintroduce per-entry GC pressure on large libraries.
func TestPathWalkCompare_ZeroAlloc(t *testing.T) {
	a := filepath.Join("music", "Diana Krall", "The Look of Love", "01 Love Letters.flac")
	b := filepath.Join("music", "Diana Krall", "The Look of Love", "02 I Remember You.flac")
	if allocs := testing.AllocsPerRun(100, func() {
		_ = pathWalkCompare(a, b)
	}); allocs != 0 {
		t.Errorf("pathWalkCompare allocated %v times/run, want 0", allocs)
	}
}
