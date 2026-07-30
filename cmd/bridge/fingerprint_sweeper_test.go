package main

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/acoustid"
	bridgefs "github.com/acoseac/1-bit-bridge/internal/fs"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// TestSweeperPacerSerialisesAcrossWorkers pins the fix for a bug that made the
// pacer look like it worked while doing nothing.
//
// The earlier version released the mutex before sleeping, so every worker
// acquired it in turn, read the SAME stale timestamp, computed the same delay,
// and they all woke and fired together — a burst of exactly worker-count
// requests, which is precisely what the pacer exists to prevent. Nothing about
// the code's shape gave that away; only the timing does.
//
// Asserting on elapsed time rather than on call ordering is what makes this a
// real test: serialisation that does not actually space the calls out would
// still pass an ordering check.
func TestSweeperPacerSerialisesAcrossWorkers(t *testing.T) {
	// A local base URL resolves to the self-hosted interval, keeping the test
	// quick while still exercising real pacing.
	c := acoustid.NewClient("http://127.0.0.1:1/v2", "k", "ua", nil)
	interval := c.MinInterval()
	s := &fingerprintSweeper{client: c}

	const workers = 4
	var wg sync.WaitGroup
	start := time.Now()
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.wait(context.Background())
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)

	// The first call returns immediately (no prior timestamp); each of the
	// remaining three must wait its own interval.
	min := time.Duration(workers-2) * interval
	if elapsed < min {
		t.Fatalf("%d workers paced in %v, want at least %v — they are not being "+
			"serialised, so they would burst against AcoustID", workers, elapsed, min)
	}
}

// TestSweeperPacerHonoursCancellation — a shutting-down sweep must not sit out
// the full interval.
func TestSweeperPacerHonoursCancellation(t *testing.T) {
	c := acoustid.NewClient("https://api.acoustid.org/v2", "k", "ua", nil) // public: 350ms
	s := &fingerprintSweeper{client: c, last: time.Now()}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	s.wait(ctx)
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("wait took %v on a cancelled context, want a prompt return", elapsed)
	}
}

// TestWaitForWorkersLetsHealthyWorkersFinish pins the distinction that a
// production run exposed.
//
// The first version applied the shutdown grace UNCONDITIONALLY, so every sweep
// was capped at it: on a real host, 500 candidates on one worker hit the cap
// after 60s, the sweep reported a truncated result, and the workers kept
// running past it into the next tick. A healthy sweep and a wedged filesystem
// look alike in the code and are not alike at all — one is normal work that
// takes minutes, the other is a mount that will never answer.
func TestWaitForWorkersLetsHealthyWorkersFinish(t *testing.T) {
	// The worker must OUTLAST the grace, or the test passes under the buggy
	// unconditional form too and pins nothing. A short grace keeps the suite
	// fast; passing it in means no shared state is mutated to get it.
	const grace = 40 * time.Millisecond

	done := make(chan struct{})
	go func() { time.Sleep(200 * time.Millisecond); close(done) }()

	start := time.Now()
	waitForWorkers(context.Background(), grace, done)
	elapsed := time.Since(start)

	if elapsed < 180*time.Millisecond {
		t.Fatalf("returned after %v, before the worker finished at ~200ms — "+
			"the shutdown grace is bounding healthy work, which caps every sweep", elapsed)
	}
	select {
	case <-done:
	default:
		t.Fatal("returned before the workers finished")
	}
}

// TestWaitForWorkersGivesUpOnACancelledSweep — the case the grace exists for.
// A worker wedged in an uninterruptible FUSE syscall will not take SIGKILL, so
// the wait must not be unbounded once shutdown has begun.
func TestWaitForWorkersGivesUpOnACancelledSweep(t *testing.T) {
	never := make(chan struct{}) // a worker that never finishes
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// A short grace rather than sleeping the real one out.
	const grace = 80 * time.Millisecond

	start := time.Now()
	waitForWorkers(ctx, grace, never)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("waited %v on a cancelled sweep — a wedged worker must not hang shutdown", elapsed)
	}
}

// TestSweeperDrainGraceIsAShutdownBound documents what the constant is for, so
// it is not repurposed as a per-sweep budget again. A sweep of 500 candidates
// on one worker legitimately runs for minutes.
func TestSweeperDrainGraceIsAShutdownBound(t *testing.T) {
	if sweeperDrainGrace > time.Minute {
		t.Errorf("sweeperDrainGrace = %v; it bounds SHUTDOWN, so it should stay small — "+
			"a long value here delays process exit on a wedged mount", sweeperDrainGrace)
	}
}

// TestCollectCandidatesSkipsTracksTheEnricherHasNotTriedYet pins the gate that
// keeps the sweeper behind the text ladder rather than racing it.
//
// Found in production, not in review: home-pc logged "resolved=1 requeued=0",
// and the zero is the tell — ResetEnrichedByPaths only advances rows at
// enriched_at > 0, so every path it had just fingerprinted was already queued
// for its first text attempt. An ExtractorVersion bump had re-extracted the
// library and reset the whole thing to 0, and the sweeper followed it in.
//
// On a steady-state library the two populations are identical, which is why no
// fixture caught this; the difference only appears while a re-extraction is in
// flight, and it points the wrong way — the sweeper spends a decode (whole-object
// egress on a network-backed root) to answer what text is about to answer free.
func TestCollectCandidatesSkipsTracksTheEnricherHasNotTriedYet(t *testing.T) {
	root := t.TempDir()
	ctx := context.Background()

	store, err := manifest.OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// Both are eligible in every other respect: real file, PCM, a duration
	// inside the gate's window, and missing exactly what fingerprinting supplies.
	dur := 240.0
	for _, name := range []string{"tried.flac", "untried.flac"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("audio"), 0o644); err != nil {
			t.Fatal(err)
		}
		tr := &manifest.Track{Path: name, Size: 5, ModTime: time.Now(), Duration: &dur}
		if err := store.UpsertTrack(ctx, tr); err != nil {
			t.Fatalf("UpsertTrack %q: %v", name, err)
		}
	}
	// UpsertTrack resets enriched_at to 0, so both rows now read "never tried".
	// Stamp one of them the way the enricher does when it gives up.
	if err := store.MarkEnriched(ctx, &manifest.Track{
		Path: "tried.flac", Size: 5, ModTime: time.Now(), Duration: &dur,
	}); err != nil {
		t.Fatal(err)
	}

	s := &fingerprintSweeper{
		store:     store,
		resolver:  bridgefs.New([]string{root}),
		cache:     acoustid.NewCache(16),
		maxPerRun: 100,
	}
	got, err := s.collectCandidates(ctx)
	if err != nil {
		t.Fatal(err)
	}

	var paths []string
	for _, c := range got {
		paths = append(paths, c.path)
	}
	if len(paths) != 1 || paths[0] != "tried.flac" {
		t.Errorf("candidates = %v, want exactly [tried.flac]; the untried row belongs to the "+
			"text ladder until it stamps enriched_at", paths)
	}
}

// countingResolver wraps a real resolver and counts the calls.
type countingResolver struct {
	inner pathResolver
	calls atomic.Int32
}

func (c *countingResolver) ResolveChecked(p string) (string, os.FileInfo, error) {
	c.calls.Add(1)
	return c.inner.ResolveChecked(p)
}

// TestCollectCandidatesDoesNoFilesystemWorkForCachedRows.
//
// The cache key used to be built from an os.Stat, so every row paid a
// filesystem round-trip before the cache could say it had already been
// answered. Steady state is the worst case for that, not the best: once the
// backlog is done, each sweep walks the whole eligible set, stats every row,
// and collects nothing. On a network-backed library that is the entire cost of
// the pass, repeating every sweep forever.
//
// The discriminator is a row whose recorded size disagrees with the file's.
// Keyed from the stat, the key misses the cache and the row becomes a
// candidate; keyed from the row, it hits and is skipped. That difference is
// also the honest statement of what changed: the key now tracks the file
// version the SCANNER recorded, not the bytes on disk this instant.
func TestCollectCandidatesDoesNoFilesystemWorkForCachedRows(t *testing.T) {
	root := t.TempDir()
	ctx := context.Background()

	store, err := manifest.OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// On disk: 9 bytes. Recorded in the row: 5. A real library diverges this
	// way whenever a file is touched between scans.
	if err := os.WriteFile(filepath.Join(root, "a.flac"), []byte("realaudio"), 0o644); err != nil {
		t.Fatal(err)
	}
	dur := 240.0
	mtime := time.Now().Truncate(time.Second)
	tr := &manifest.Track{Path: "a.flac", Size: 5, ModTime: mtime, Duration: &dur}
	if err := store.UpsertTrack(ctx, tr); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkEnriched(ctx, tr); err != nil {
		t.Fatal(err)
	}

	cache := acoustid.NewCache(16)
	cache.Set(acoustid.Key{Path: "a.flac", Size: 5, MTimeNS: mtime.UnixNano()}, acoustid.Outcome{})

	res := &countingResolver{inner: bridgefs.New([]string{root})}
	s := &fingerprintSweeper{store: store, resolver: res, cache: cache, maxPerRun: 100}

	got, err := s.collectCandidates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("candidates = %+v, want none — the row's version is already answered", got)
	}
	if n := res.calls.Load(); n != 0 {
		t.Errorf("resolver called %d times for a fully cached sweep, want 0 — the cache "+
			"check must not be gated behind a filesystem round-trip", n)
	}
}

// TestCollectCandidatesResolvesEveryCandidateExactlyOnce covers phase two.
//
// Resolution moved out of the StreamTracks callback so no filesystem call
// happens with a SQLite cursor open: WAL mode cannot reset the log while a
// reader holds a snapshot, so a read transaction spanning thousands of stats
// pins the WAL for the whole sweep while enrichment writes append behind it.
//
// That ordering is structural — resolveCandidates is called on the result of
// StreamTracks, after it returns — and not observable from here without a seam
// on the store. What IS observable, and what a move back inside the callback
// would disturb, is that every returned candidate carries the absPath phase
// two assigns, at a cost of exactly one resolve each.
func TestCollectCandidatesResolvesEveryCandidateExactlyOnce(t *testing.T) {
	root := t.TempDir()
	ctx := context.Background()

	store, err := manifest.OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	dur := 240.0
	// "gone.flac" has a row but no file — the mount-outage shape. It must be
	// dropped silently rather than persisted as unfingerprintable.
	for _, name := range []string{"a.flac", "b.flac", "gone.flac"} {
		if name != "gone.flac" {
			if err := os.WriteFile(filepath.Join(root, name), []byte("audio"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		tr := &manifest.Track{Path: name, Size: 5, ModTime: time.Now(), Duration: &dur}
		if err := store.UpsertTrack(ctx, tr); err != nil {
			t.Fatal(err)
		}
		if err := store.MarkEnriched(ctx, tr); err != nil {
			t.Fatal(err)
		}
	}

	res := &countingResolver{inner: bridgefs.New([]string{root})}
	s := &fingerprintSweeper{
		store:     store,
		resolver:  res,
		cache:     acoustid.NewCache(16),
		maxPerRun: 100,
	}
	got, err := s.collectCandidates(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != 2 {
		t.Fatalf("candidates = %d, want 2 — the row with no file must be dropped", len(got))
	}
	for _, c := range got {
		if c.absPath == "" {
			t.Errorf("candidate %q has no absPath — phase two must fill it in", c.path)
		}
		if c.path == "gone.flac" {
			t.Error("an unresolvable row reached the worker pool")
		}
	}
	// Three eligible rows, three resolves: one per row that got past the cheap
	// screens, none repeated.
	if n := res.calls.Load(); n != 3 {
		t.Errorf("resolver called %d times, want 3", n)
	}
}
