package api

import (
	"context"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// healthCountsTTL bounds how long a /v1/health scan-state count snapshot is
// reused. Well within iOS's ~15s health-poll cadence, so clients never see a
// stale count that matters, while an unauthenticated flood can't drive the
// underlying COUNT(*) scans more than ~once per window.
const healthCountsTTL = 3 * time.Second

// healthCountsFetchTimeout bounds the detached count fetch. Mirrors
// reachabilityProbeTimeout — a wedged SQLite handle reports the last known
// (or zero) counts rather than blocking the health probe indefinitely.
const healthCountsFetchTimeout = 2 * time.Second

// healthCountsProvider is the subset of ManifestProvider the count cache
// needs. Narrow interface so tests can inject a call-counting fake without a
// real store; *manifest.Store (via ManifestProvider) satisfies it.
type healthCountsProvider interface {
	TracksIndexed(ctx context.Context) int
	PendingDeletions(ctx context.Context) int64
}

type healthCountsSnapshot struct {
	tracksIndexed    int
	pendingDeletions int64
	fetchedAt        time.Time
}

// healthCountsCache is a tiny single-key TTL cache for the /v1/health
// scan-state counts. /v1/health is unauthenticated and can be hammered (iOS
// polls every ~15s, and nothing rate-limits the route), so running
// TracksIndexed + PendingDeletions — two COUNT(*) scans, one filtered with no
// missing_count index — on every request competes with the scanner/writer on
// the single SQLite handle. Mirrors the sibling reachabilityCache on this same
// handler: a mutex guards the snapshot, a singleflight.Group collapses a flood
// at the expiry boundary onto ONE recompute, and the fetch DETACHES from the
// caller's ctx (context.WithoutCancel) so a client hang-up mid-count can't
// cache a synthesized-zero for the callers that joined the flight.
type healthCountsCache struct {
	mu      sync.Mutex
	snap    healthCountsSnapshot
	hasSnap bool
	group   singleflight.Group
}

func newHealthCountsCache() *healthCountsCache { return &healthCountsCache{} }

// counts returns the cached (tracksIndexed, pendingDeletions), refreshing on
// cache miss or staleness. Nil receiver or nil provider returns (0, 0) so
// test harnesses that construct &Server{} without New() don't panic.
func (c *healthCountsCache) counts(ctx context.Context, p healthCountsProvider) (int, int64) {
	if c == nil || p == nil {
		return 0, 0
	}
	c.mu.Lock()
	if c.hasSnap && time.Since(c.snap.fetchedAt) < healthCountsTTL {
		snap := c.snap
		c.mu.Unlock()
		return snap.tracksIndexed, snap.pendingDeletions
	}
	c.mu.Unlock()

	// singleflight collapses concurrent callers onto one fetch; every caller
	// that arrived during the in-flight window shares the result.
	v, _, _ := c.group.Do("counts", func() (interface{}, error) {
		// Re-check under the lock — a flight that resolved while we were
		// queued may already have refreshed the snapshot.
		c.mu.Lock()
		if c.hasSnap && time.Since(c.snap.fetchedAt) < healthCountsTTL {
			snap := c.snap
			c.mu.Unlock()
			return snap, nil
		}
		c.mu.Unlock()

		// Detach from the caller's ctx: the singleflight result is shared, so
		// the first caller hanging up mid-count must not cache a zero for the
		// rest. The two COUNTs are bounded by their own timeout instead.
		fetchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), healthCountsFetchTimeout)
		defer cancel()
		snap := healthCountsSnapshot{
			tracksIndexed:    p.TracksIndexed(fetchCtx),
			pendingDeletions: p.PendingDeletions(fetchCtx),
			fetchedAt:        time.Now(),
		}
		c.mu.Lock()
		c.snap = snap
		c.hasSnap = true
		c.mu.Unlock()
		return snap, nil
	})
	snap := v.(healthCountsSnapshot)
	return snap.tracksIndexed, snap.pendingDeletions
}
