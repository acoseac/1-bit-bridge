package admin

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The catalog cache answers two different questions with two different
// urgencies, and these tests pin that split. Both were one synchronous
// rebuild before: a person opening the console occasionally paid a full
// library fold on essentially every visit, because a 5-minute TTL always
// expired between visits.

// TestCatalogTTLExpiryServesStaleAndRefreshesBehind pins the load-time
// fix. A clock-expired snapshot is a GUESS that something moved — an
// unnudged writer like enrichment — so the request is answered from the
// copy we already have and the rebuild happens out of band.
func TestCatalogTTLExpiryServesStaleAndRefreshesBehind(t *testing.T) {
	srv, _, _ := newTestServer(t)
	ctx := context.Background()

	first, err := srv.libraryCatalog(ctx)
	if err != nil {
		t.Fatalf("initial build: %v", err)
	}

	// Count rebuilds rather than comparing builtAt stamps. Two
	// time.Now() readings milliseconds apart are NOT reliably ordered:
	// Windows' timer granularity is ~15.6 ms, so a refresh that ran
	// correctly can stamp a value EQUAL to the one before it and a
	// `.After()` assertion fails on CI while passing on every
	// nanosecond-clock host. Same coarse-clock trap as the indexed_at
	// bump. The counter answers the question this test actually asks.
	var builds atomic.Int64
	catalogBuiltHookForTests = func() { builds.Add(1) }
	t.Cleanup(func() { catalogBuiltHookForTests = nil })

	// Age the snapshot past the TTL without touching the epoch: nothing
	// has told us the library changed.
	cached := srv.catalog.Load()
	if cached == nil {
		t.Fatal("snapshot was not cached")
	}
	srv.catalog.Store(&cachedCatalog{
		cat:     cached.cat,
		epoch:   cached.epoch,
		builtAt: time.Now().Add(-2 * catalogCacheTTL),
	})

	// The request must be answered IMMEDIATELY from the stale copy —
	// same pointer, no fold on this goroutine.
	got, err := srv.libraryCatalog(ctx)
	if err != nil {
		t.Fatalf("stale read: %v", err)
	}
	if got != first {
		t.Error("a clock-stale read rebuilt synchronously instead of serving the cached snapshot")
	}

	// ...and a refresh must actually happen behind it.
	srv.WaitForCatalogRefresh()
	if srv.catalog.Load() == nil {
		t.Fatal("snapshot vanished after the background refresh")
	}
	if got := builds.Load(); got != 1 {
		t.Errorf("background rebuilds = %d, want exactly 1; a stale snapshot would never refresh", got)
	}
}

// TestCatalogEpochChangeRebuildsSynchronously is the other half, and the
// behaviour that must NOT regress: when a scan has actually happened the
// snapshot is known-wrong, and "I scanned, I refreshed, my new album
// isn't there" is a real complaint. That case still blocks.
func TestCatalogEpochChangeRebuildsSynchronously(t *testing.T) {
	srv, _, _ := newTestServer(t)
	ctx := context.Background()

	if _, err := srv.libraryCatalog(ctx); err != nil {
		t.Fatalf("initial build: %v", err)
	}
	before := srv.catalog.Load()
	if before == nil {
		t.Fatal("snapshot was not cached")
	}

	srv.InvalidateLibraryCatalog()
	if _, err := srv.libraryCatalog(ctx); err != nil {
		t.Fatalf("post-invalidation read: %v", err)
	}

	// By the time the call RETURNS the new snapshot must already be
	// stored — no waiting on a background goroutine.
	after := srv.catalog.Load()
	if after == nil {
		t.Fatal("snapshot vanished")
	}
	if after.epoch == before.epoch {
		t.Error("epoch change did not rebuild synchronously; a scan's result would not be visible on refresh")
	}
}

// TestWarmLibraryCatalogPopulatesTheSnapshot pins that boot warming
// leaves a usable snapshot, so the first real request is a cache hit
// rather than a cold fold.
func TestWarmLibraryCatalogPopulatesTheSnapshot(t *testing.T) {
	srv, _, _ := newTestServer(t)
	if srv.catalog.Load() != nil {
		t.Fatal("precondition: the catalog should start cold")
	}

	srv.WarmLibraryCatalog(context.Background())

	warmed := srv.catalog.Load()
	if warmed == nil {
		t.Fatal("WarmLibraryCatalog left no snapshot")
	}
	got, err := srv.libraryCatalog(context.Background())
	if err != nil {
		t.Fatalf("read after warm: %v", err)
	}
	if got != warmed.cat {
		t.Error("the first read after warming rebuilt instead of reusing the warmed snapshot")
	}
}

// TestConcurrentStaleReadsCoalesceIntoOneRebuild pins that a burst of
// stale reads costs ONE library fold, not one per reader.
//
// The singleflight is what delivers that (verified: this test still
// passes with the catalogRefreshing flag removed). It is pinned anyway
// because it is the property that matters — a dozen tiles loading at
// once must not each re-fold a 20k-track library — and because routing
// the refresher around the flight is an easy and silent regression.
func TestConcurrentStaleReadsCoalesceIntoOneRebuild(t *testing.T) {
	srv, _, _ := newTestServer(t)
	ctx := context.Background()

	if _, err := srv.libraryCatalog(ctx); err != nil {
		t.Fatalf("initial build: %v", err)
	}

	var builds atomic.Int64
	catalogBuiltHookForTests = func() { builds.Add(1) }
	t.Cleanup(func() { catalogBuiltHookForTests = nil })

	// Age the snapshot past the TTL, epoch untouched.
	cached := srv.catalog.Load()
	stale := func() {
		srv.catalog.Store(&cachedCatalog{
			cat:     cached.cat,
			epoch:   cached.epoch,
			builtAt: time.Now().Add(-2 * catalogCacheTTL),
		})
	}
	stale()

	// A burst of readers, each of which finds the snapshot stale.
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := srv.libraryCatalog(ctx); err != nil {
				t.Errorf("stale read: %v", err)
			}
		}()
	}
	wg.Wait()
	srv.WaitForCatalogRefresh()

	if got := builds.Load(); got != 1 {
		t.Errorf("24 concurrent stale reads triggered %d rebuilds, want exactly 1", got)
	}
}

// TestStaleReadDuringAnActiveRefreshSpawnsNothing pins the
// catalogRefreshing flag specifically, and pins the narrow thing it
// really does: bound goroutine spawns while a refresh is already in
// flight. Each spawn is a bgRefresh.Add that shutdown waits out, so an
// unbounded burst of them is a real if minor cost.
//
// The build is held open by the hook, so a spawned refresher is still
// running when the assertion happens — without that, a goroutine that
// finishes in microseconds makes the broken and correct code
// indistinguishable, which is exactly how the first version of this test
// passed with the flag removed.
func TestStaleReadDuringAnActiveRefreshSpawnsNothing(t *testing.T) {
	srv, _, _ := newTestServer(t)
	ctx := context.Background()
	if _, err := srv.libraryCatalog(ctx); err != nil {
		t.Fatalf("initial build: %v", err)
	}

	release := make(chan struct{})
	catalogBuiltHookForTests = func() { <-release }
	t.Cleanup(func() {
		catalogBuiltHookForTests = nil
		select {
		case <-release:
		default:
			close(release)
		}
		srv.WaitForCatalogRefresh()
	})

	// Mark a refresh as already running, then take the stale path.
	srv.catalogRefreshing.Store(true)
	cached := srv.catalog.Load()
	srv.catalog.Store(&cachedCatalog{
		cat:     cached.cat,
		epoch:   cached.epoch,
		builtAt: time.Now().Add(-2 * catalogCacheTTL),
	})
	if _, err := srv.libraryCatalog(ctx); err != nil {
		t.Fatalf("stale read: %v", err)
	}

	// Nothing should have been spawned, so the drain returns at once. A
	// spawned refresher would be parked in the hook and hold this open.
	done := make(chan struct{})
	go func() { srv.WaitForCatalogRefresh(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a refresher was spawned while one was already in flight")
	}
	srv.catalogRefreshing.Store(false)
}
