package api

import (
	"context"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// publicServersTTL bounds how long a /v1/health UPnP-upstream snapshot is
// reused. Matches healthCountsTTL — well within iOS's ~15s health-poll
// cadence, so clients never see a stale server list that matters, while an
// unauthenticated flood can't drive the underlying per-server COUNT(*) scans
// more than ~once per window.
const publicServersTTL = 3 * time.Second

// publicServersFetchTimeout bounds the detached provider call. Mirrors
// healthCountsFetchTimeout — a wedged SQLite handle reports the last known
// (or nil) server list rather than blocking the health probe indefinitely.
const publicServersFetchTimeout = 2 * time.Second

type publicServersSnapshot struct {
	servers   []UPnPUpstreamPublicServer
	fetchedAt time.Time
}

// publicServersCache is a tiny single-key TTL cache for the /v1/health UPnP
// upstream advertisement. /v1/health is unauthenticated and can be hammered
// (iOS polls every ~15s, and nothing rate-limits the route), so calling
// UPnPUpstreamPublicProvider.PublicServers on every request is a hazard on a
// hybrid deployment: the production adapter loops the configured upstreams
// issuing one `SELECT COUNT(*) FROM upnp_track_routing WHERE server_udn = ?`
// per server, and on a 15k-routed-row upstream each of those is a full index
// scan competing with the scanner/writer on the single SQLite handle. Mirrors
// the sibling healthCountsCache / reachabilityCache on this same handler: a
// mutex guards the snapshot, a singleflight.Group collapses a flood at the
// expiry boundary onto ONE recompute, and the fetch DETACHES from the
// caller's ctx (context.WithoutCancel) so a client hang-up mid-fetch can't
// cache a synthesized-nil for the callers that joined the flight.
//
// Public-mode bridges never wire a provider (UPnP upstream is Validate-
// rejected there), so the /v1/health handler's `s.upnpPublicProvider != nil`
// guard keeps this cache off the hot path entirely on those deploys — the
// wrap is a no-op-cost passthrough when there are no servers to count.
type publicServersCache struct {
	mu      sync.Mutex
	snap    publicServersSnapshot
	hasSnap bool
	group   singleflight.Group
	// fetchTimeout bounds a single detached provider call. Per-instance (not a
	// package-level var) so tests can shrink it without racing a parallel test
	// — same convention as transcode.Pool.jobTimeout (PR #162). Defaults to
	// publicServersFetchTimeout via newPublicServersCache; a zero value falls
	// back to that default so a hand-built &publicServersCache{} still works.
	fetchTimeout time.Duration
}

func newPublicServersCache() *publicServersCache {
	return &publicServersCache{fetchTimeout: publicServersFetchTimeout}
}

// servers returns the cached UPnP upstream server list, refreshing on cache
// miss or staleness. Nil receiver or nil provider returns nil so test
// harnesses that construct &Server{} without New() — and pre-feature deploys
// with no provider wired — surface the same "no upstreams" shape as before.
//
// The returned slice is shared read-only across every caller that hits a
// cache hit; callers (only the /v1/health handler) MUST NOT mutate it. The
// provider builds a fresh slice per refresh, and the handler only JSON-
// encodes the result, so concurrent readers are safe.
func (c *publicServersCache) servers(ctx context.Context, p UPnPUpstreamPublicProvider) []UPnPUpstreamPublicServer {
	if c == nil || p == nil {
		return nil
	}
	c.mu.Lock()
	if c.hasSnap && time.Since(c.snap.fetchedAt) < publicServersTTL {
		snap := c.snap
		c.mu.Unlock()
		return snap.servers
	}
	c.mu.Unlock()

	// singleflight collapses concurrent callers onto one fetch; every caller
	// that arrived during the in-flight window shares the result.
	v, _, _ := c.group.Do("servers", func() (interface{}, error) {
		// Re-check under the lock — a flight that resolved while we were
		// queued may already have refreshed the snapshot.
		c.mu.Lock()
		if c.hasSnap && time.Since(c.snap.fetchedAt) < publicServersTTL {
			snap := c.snap
			c.mu.Unlock()
			return snap, nil
		}
		c.mu.Unlock()

		// Detach from the caller's ctx: the singleflight result is shared, so
		// the first caller hanging up mid-fetch must not cache a nil for the
		// rest. The per-server COUNTs are bounded by their own timeout instead.
		timeout := c.fetchTimeout
		if timeout <= 0 {
			timeout = publicServersFetchTimeout
		}
		fetchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
		defer cancel()
		servers := p.PublicServers(fetchCtx)

		c.mu.Lock()
		defer c.mu.Unlock()
		// Serve-stale-on-error: WithoutCancel means only the fetch deadline can
		// fire, so fetchCtx.Err() != nil signals a wedged SQLite handle / slow
		// per-server COUNT fan-out. Don't clobber a good prior snapshot with the
		// degraded/empty result — keep the last-good list and LEAVE its
		// (already-stale) timestamp so the next request retries soon (bounded by
		// singleflight to one in-flight fetch at a time). Only a first fetch with
		// no prior snapshot caches a degraded result (nothing better to serve).
		if fetchCtx.Err() != nil && c.hasSnap {
			return c.snap, nil
		}
		snap := publicServersSnapshot{
			servers:   servers,
			fetchedAt: time.Now(),
		}
		c.snap = snap
		c.hasSnap = true
		return snap, nil
	})
	snap := v.(publicServersSnapshot)
	return snap.servers
}
