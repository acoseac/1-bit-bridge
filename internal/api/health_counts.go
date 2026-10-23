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
	// first and sampleFormat are the two library-fact keys whose readiness
	// a count decides (stickyReady): firstIndexedAt and sampleFormat.
	first        readyFlag
	sampleFormat readyFlag
	group        singleflight.Group
}

// readyFlag is one readiness answer: ready once a count said so, and
// otherwise the time of the last miss, which stands for healthCountsTTL.
type readyFlag struct {
	ready bool
	at    time.Time
	known bool
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
		tracks := p.TracksIndexed(fetchCtx)
		pending := p.PendingDeletions(fetchCtx)

		snap := healthCountsSnapshot{
			tracksIndexed:    tracks,
			pendingDeletions: pending,
			fetchedAt:        time.Now(),
		}

		c.mu.Lock()
		defer c.mu.Unlock()
		// Never CACHE a timed-out fetch. Both providers return 0 on a
		// failed query, so a fetchCtx deadline (SQLite write-lock
		// contention, a wedged handle) would otherwise publish
		// "0 tracks indexed" — which reads to a client exactly like an
		// emptied library — and the TTL would keep serving it for the
		// whole window. publicServersCache.servers already takes this
		// posture; this is the sibling that didn't.
		//
		// Two sub-cases, and the cold one matters: with a good snapshot
		// we serve it stale; with NO snapshot yet (first call, database
		// already busy) we return the zeros uncached, so the very next
		// request retries instead of being answered from a cached
		// failure.
		if fetchCtx.Err() != nil {
			if c.hasSnap {
				return c.snap, nil
			}
			return snap, nil
		}
		c.snap = snap
		c.hasSnap = true
		return snap, nil
	})
	snap := v.(healthCountsSnapshot)
	return snap.tracksIndexed, snap.pendingDeletions
}

// firstIndexedAtReady reports whether every track row has a date. Ready
// sticks for this process: an insert writes a date, so a later row cannot
// reopen the count. A miss is rechecked on the health-count TTL. A
// timeout is not stored. A nil cache asks the manifest directly.
func (c *healthCountsCache) firstIndexedAtReady(ctx context.Context, manifest any) bool {
	src, ok := manifest.(interface{ FirstIndexedAtReady(context.Context) bool })
	if !ok {
		return false
	}
	if c == nil {
		return src.FirstIndexedAtReady(ctx)
	}
	return c.stickyReady(ctx, "firstIndexed", &c.first, src.FirstIndexedAtReady)
}

// sampleFormatReady reports whether the store's sample-format backfill is
// complete (manifest.Store.SampleFormatReady, backlog B513), with
// firstIndexedAtReady's caching: ready sticks for this process, since the
// store's completion mark is never removed; a miss is rechecked on the
// health-count TTL; a timeout is not stored.
func (c *healthCountsCache) sampleFormatReady(ctx context.Context, manifest any) bool {
	src, ok := manifest.(interface{ SampleFormatReady(context.Context) bool })
	if !ok {
		return false
	}
	if c == nil {
		return src.SampleFormatReady(ctx)
	}
	return c.stickyReady(ctx, "sampleFormat", &c.sampleFormat, src.SampleFormatReady)
}

// stickyReady answers flag from probe: true for good once probe says so, a
// miss reused for healthCountsTTL, and a probe that timed out not stored.
// Concurrent callers share one probe (key names it in the singleflight
// group), and the probe detaches from the caller's context so one client
// hanging up cannot store a miss for the rest.
func (c *healthCountsCache) stickyReady(ctx context.Context, key string, flag *readyFlag, probe func(context.Context) bool) bool {
	c.mu.Lock()
	if flag.ready {
		c.mu.Unlock()
		return true
	}
	if flag.known && time.Since(flag.at) < healthCountsTTL {
		c.mu.Unlock()
		return false
	}
	c.mu.Unlock()

	v, _, _ := c.group.Do(key, func() (any, error) {
		c.mu.Lock()
		if flag.ready {
			c.mu.Unlock()
			return true, nil
		}
		if flag.known && time.Since(flag.at) < healthCountsTTL {
			c.mu.Unlock()
			return false, nil
		}
		c.mu.Unlock()

		fetchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), healthCountsFetchTimeout)
		defer cancel()
		ready := probe(fetchCtx)
		if ready {
			c.mu.Lock()
			flag.ready = true
			c.mu.Unlock()
			return true, nil
		}
		if fetchCtx.Err() != nil {
			return false, nil
		}
		c.mu.Lock()
		flag.known = true
		flag.at = time.Now()
		c.mu.Unlock()
		return false, nil
	})
	ready, _ := v.(bool)
	return ready
}
