package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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

// TestSweeperRateLimitStallsTheWholePool pins the consumer that
// acoustid.RateLimitError did not have.
//
// The type shipped with a docblock saying it exists "so the sweeper can pause
// its WHOLE pool", and nothing ever read it: the lookup-error branch classified
// a 429 as transient (it is) and returned, so the worker took the next
// candidate and ran a full fpcalc decode — about a CPU second, plus a
// whole-object read on a network-backed root — before pacing at 350ms and
// hitting the same wall. On a sustained limit that is up to maxPerRun (500)
// rejected requests and 500 wasted decodes against a service that asked us to
// stop for a minute.
//
// The assertion that matters is on the EARLIEST of the two follow-up requests.
// A fix that only stalled the worker which saw the 429 would still let its
// sibling straight through, and a test that looked at one worker — or at the
// last request — would pass anyway.
func TestSweeperRateLimitStallsTheWholePool(t *testing.T) {
	var mu sync.Mutex
	var hits []time.Time
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		first := len(hits) == 0
		hits = append(hits, time.Now())
		mu.Unlock()
		if first {
			// 1s is the shortest honourable advice — Retry-After's
			// delta-seconds form is an integer.
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"status":"error","error":{"code":14,"message":"rate limit exceeded"}}`)
			return
		}
		fmt.Fprint(w, `{"status":"ok","results":[]}`)
	}))
	defer srv.Close()

	// A duration inside the gate's window, matching the canned fingerprint's,
	// so the pre-lookup screen passes and the request actually goes out.
	const dur = 243.55
	s := &fingerprintSweeper{
		// A local base URL resolves to the self-hosted interval (150ms), which
		// is what the pause has to be distinguishable FROM.
		client: acoustid.NewClient(srv.URL, "k", "ua", srv.Client()),
		cache:  acoustid.NewCache(16),
		length: time.Minute,
		compute: func(context.Context, string, time.Duration) (acoustid.Fingerprint, error) {
			return acoustid.Fingerprint{Value: "AQABz0mUaEkSRZEG", Duration: dur, DistinctB64: 40}, nil
		},
	}
	base := candidate{absPath: "/nonexistent/a.flac", durationS: dur, artist: "Some Artist"}

	// One worker takes the 429.
	first := base
	first.path = "a.flac"
	if s.fingerprintOne(context.Background(), first) {
		t.Fatal("a rate-limited lookup must not report a match")
	}

	// Two MORE workers, concurrently. Both must be held.
	var wg sync.WaitGroup
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := base
			c.path = fmt.Sprintf("b-%d.flac", i)
			s.fingerprintOne(context.Background(), c)
		}()
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(hits) != 3 {
		t.Fatalf("server saw %d requests, want 3", len(hits))
	}
	// Well inside the 1s advice, and well outside the 150ms pacing interval the
	// unfixed code would have produced — separated enough not to be fragile.
	const wantPause = 900 * time.Millisecond
	for i, h := range hits[1:] {
		if gap := h.Sub(hits[0]); gap < wantPause {
			t.Errorf("follow-up request %d reached AcoustID %v after the 429, want at least %v — "+
				"Retry-After is not stalling the whole pool", i+1, gap, wantPause)
		}
	}
}

// TestSweeperNoteLookupErrOnlyPausesForARateLimit — the pause is a real stall
// of every worker, so it must fire on exactly the error that asks for it.
func TestSweeperNoteLookupErrOnlyPausesForARateLimit(t *testing.T) {
	s := &fingerprintSweeper{}

	for _, err := range []error{
		errors.New("acoustid: decoding response: unexpected EOF"),
		context.Canceled,
		// A rate limit with no usable advice says nothing about how long to
		// wait; the ordinary pacer already covers the next request.
		&acoustid.RateLimitError{},
	} {
		s.noteLookupErr(err)
		if !s.notBefore.IsZero() {
			t.Fatalf("%v paused the pool; only a Retry-After may", err)
		}
	}

	// Wrapped, which is the shape a caller that annotates the error produces.
	s.noteLookupErr(fmt.Errorf("lookup: %w", &acoustid.RateLimitError{RetryAfter: 30 * time.Second}))
	if d := time.Until(s.notBefore); d < 25*time.Second {
		t.Fatalf("pause remaining = %v, want ~30s from a wrapped RateLimitError", d)
	}

	// Later, shorter advice must not cut an in-force pause short: several
	// workers can be in flight when the limit is hit.
	held := s.notBefore
	s.noteLookupErr(&acoustid.RateLimitError{RetryAfter: time.Second})
	if !s.notBefore.Equal(held) {
		t.Errorf("a 1s Retry-After shortened a 30s pause to %v", time.Until(s.notBefore))
	}

	// Clamped to the client's own bound rather than honoured verbatim —
	// RetryAfter is exported and the type is constructible outside that
	// package, so the parse-time cap is not a guarantee about what arrives.
	s.noteLookupErr(&acoustid.RateLimitError{RetryAfter: 999 * time.Hour})
	if d := time.Until(s.notBefore); d > acoustid.MaxRetryAfter {
		t.Errorf("pause remaining = %v, want no more than %v", d, acoustid.MaxRetryAfter)
	}
}

// nilCarrier stages a typed nil inside an error chain. fmt.Errorf("…: %w",
// (*T)(nil)) cannot: it formats its operand at CONSTRUCTION time, so the nil
// receiver's Error() panics there rather than at the site under test. Twin of
// the helper in internal/acoustid's tests.
type nilCarrier struct{ inner error }

func (nilCarrier) Error() string   { return "wrapped" }
func (w nilCarrier) Unwrap() error { return w.inner }

// TestSweeperNoteLookupErrSurvivesATypedNil pins the guard.
//
// errors.As matches on the CONCRETE TYPE, so a chain carrying a typed-nil
// *acoustid.RateLimitError makes it report true while leaving the target nil,
// and reading RetryAfter off that panics — inside a sweep worker, where the
// per-iteration recover the SCANNER has no equivalent of. Not reachable from
// this repo today (get() only ever builds &RateLimitError{...}), so what this
// pins is the guard against a future wrapper.
func TestSweeperNoteLookupErrSurvivesATypedNil(t *testing.T) {
	err := nilCarrier{inner: (*acoustid.RateLimitError)(nil)}

	// Fixture self-check: if this stops staging a typed nil, the assertion
	// below passes for the wrong reason.
	var probe *acoustid.RateLimitError
	if !errors.As(err, &probe) || probe != nil {
		t.Fatal("fixture does not stage a typed nil, so the guard is not being exercised")
	}

	s := &fingerprintSweeper{}
	s.noteLookupErr(err)
	if !s.notBefore.IsZero() {
		t.Error("a typed nil carries no Retry-After, so it must not pause the pool")
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

// TestSweeperPauseHonoursCancellation — an hour-long Retry-After must not
// become an hour-long shutdown. The sweeper is joined to bgWriters, so a pause
// that ignored cancellation would hold process exit open behind it.
func TestSweeperPauseHonoursCancellation(t *testing.T) {
	s := &fingerprintSweeper{client: acoustid.NewClient("https://api.acoustid.org/v2", "k", "ua", nil)}
	s.noteLookupErr(&acoustid.RateLimitError{RetryAfter: time.Hour})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	s.awaitPause(ctx) // gates the decode
	s.wait(ctx)       // gates the request
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("a paused sweeper took %v to unwind on a cancelled context", elapsed)
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

// TestCollectCandidatesIsNotStarvedByUnresolvableRows.
//
// Moving resolution out of the stream moved the per-run cap to the wrong side
// of it. With one bound, rows pointing at missing files — a folder removed but
// not yet reaped, a root half-mounted — fill the budget, phase two drops every
// one, and the sweep does nothing. StreamTracks order is stable, so the same
// rows win the race on every sweep and the tracks behind them are never
// reached: permanent starvation, not a slow pass.
//
// The old single-cap code did not have this problem, because it counted
// candidates AFTER resolving them. Two bounds restore that property.
func TestCollectCandidatesIsNotStarvedByUnresolvableRows(t *testing.T) {
	root := t.TempDir()
	ctx := context.Background()

	store, err := manifest.OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	dur := 240.0
	add := func(name string, onDisk bool) {
		if onDisk {
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
	// Six rows with no file, then two real ones. Sorted order puts the ghosts
	// first, so a stream that stops at maxPerRun=2 collects only ghosts.
	for i := range 6 {
		add(fmt.Sprintf("aghost-%d.flac", i), false)
	}
	add("zreal-1.flac", true)
	add("zreal-2.flac", true)

	s := &fingerprintSweeper{
		store:     store,
		resolver:  bridgefs.New([]string{root}),
		cache:     acoustid.NewCache(16),
		maxPerRun: 2,
	}
	got, err := s.collectCandidates(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != 2 {
		t.Fatalf("candidates = %d, want 2 — the sweep must reach past rows that "+
			"cannot resolve, or the same ones starve it on every pass", len(got))
	}
	for _, c := range got {
		if !strings.HasPrefix(c.path, "zreal-") {
			t.Errorf("candidate %q is one of the unresolvable rows", c.path)
		}
	}
}

// TestResolveCandidatesStopsAtTheWorkCap pins the other half: the extra rows
// scanCap allows are a memory allowance, not extra work. A healthy library
// resolves the first maxPerRun and never stats the rest.
func TestResolveCandidatesStopsAtTheWorkCap(t *testing.T) {
	root := t.TempDir()
	var in []candidate
	for i := range 10 {
		name := fmt.Sprintf("t-%d.flac", i)
		if err := os.WriteFile(filepath.Join(root, name), []byte("audio"), 0o644); err != nil {
			t.Fatal(err)
		}
		in = append(in, candidate{path: name})
	}

	res := &countingResolver{inner: bridgefs.New([]string{root})}
	s := &fingerprintSweeper{resolver: res, maxPerRun: 3}

	got := s.resolveCandidates(in)
	if len(got) != 3 {
		t.Errorf("resolved %d, want the work cap of 3", len(got))
	}
	if n := res.calls.Load(); n != 3 {
		t.Errorf("resolver called %d times, want 3 — rows past the cap must not be "+
			"stat'd at all", n)
	}
}
