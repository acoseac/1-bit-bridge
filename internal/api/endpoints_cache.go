package api

import (
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// endpointsTTL bounds how long a /v1/health advertised-endpoint snapshot is
// reused. Longer than the sibling healthCountsTTL / publicServersTTL (3s)
// because the underlying data moves far more slowly: the endpoint set only
// changes when an interface gains or loses an address, and the mDNS
// advertiser already accepts a 60s detection latency for exactly that
// transition (internal/mdns rebind loop). 15s is well inside that budget and
// inside iOS's ~15s health-poll cadence, so a client never sees a stale set
// that matters.
const endpointsTTL = 15 * time.Second

// endpointsCache is a tiny single-key TTL cache for the endpoint list
// advertised by /v1/health.
//
// /v1/health is UNAUTHENTICATED and carries no rate limiter (the only
// limiter in this package is per-authed-token, on /v1/manifest), so any LAN
// or tailnet peer can loop it. Uncached, every request ran a full interface
// enumeration: `net.Interfaces()` plus one `iface.Addrs()` per interface. On
// Linux that is 1 + N NetlinkRIB socket-create-and-dump round trips (Go
// re-dumps RTM_GETADDR in full on every Addrs() call); on Windows it is
// 1 + N GetAdaptersAddresses calls, which are notably slow — and Windows is
// a live deployment target. A Docker/Hyper-V host with 15-25 adapters paid
// 16-26 kernel dumps per request, unbounded and unauthenticated.
//
// The two COUNT(*) scans on this same handler were already TTL-capped for
// exactly this reason; the interface walk was the remaining uncapped
// per-request cost. Mirrors the sibling healthCountsCache /
// publicServersCache shape: a mutex guards the snapshot and a
// singleflight.Group collapses a flood at the expiry boundary onto ONE
// recompute.
//
// Unlike those siblings there is no ctx to detach — reachableEndpoints does
// pure syscalls and takes no context, so there is no caller-cancellation
// hazard to guard against.
//
// Public-mode bridges short-circuit inside reachableEndpoints before the
// walk (they advertise only operator-declared endpoints), so on those deploys
// this wrap is a cheap passthrough over an already-trivial computation.
type endpointsCache struct {
	mu      sync.Mutex
	eps     []string
	fetched time.Time
	hasSnap bool
	group   singleflight.Group
	// ttl is per-instance (not a package-level var) so a test can shrink it
	// without racing a parallel test — same convention as
	// publicServersCache.fetchTimeout and transcode.Pool.jobTimeout. A zero
	// value falls back to endpointsTTL so a hand-built &endpointsCache{}
	// still behaves.
	ttl time.Duration
}

func newEndpointsCache() *endpointsCache { return &endpointsCache{ttl: endpointsTTL} }

func (c *endpointsCache) effectiveTTL() time.Duration {
	if c.ttl <= 0 {
		return endpointsTTL
	}
	return c.ttl
}

// endpoints returns the advertised endpoint list, recomputing via `compute`
// at most once per TTL window across all concurrent callers.
//
// The returned slice is shared with the cache and with every other caller in
// the window — treat it as READ-ONLY. Every current consumer assigns it
// straight into a response DTO for marshalling, which never mutates.
func (c *endpointsCache) endpoints(compute func() []string) []string {
	c.mu.Lock()
	if c.hasSnap && time.Since(c.fetched) < c.effectiveTTL() {
		eps := c.eps
		c.mu.Unlock()
		return eps
	}
	c.mu.Unlock()

	v, _, _ := c.group.Do("endpoints", func() (any, error) {
		// Re-check under the lock: a flight that queued behind a just-
		// completed one must not recompute.
		c.mu.Lock()
		if c.hasSnap && time.Since(c.fetched) < c.effectiveTTL() {
			eps := c.eps
			c.mu.Unlock()
			return eps, nil
		}
		c.mu.Unlock()

		eps := compute()

		c.mu.Lock()
		c.eps = eps
		c.fetched = time.Now()
		c.hasSnap = true
		c.mu.Unlock()
		return eps, nil
	})
	eps, _ := v.([]string)
	return eps
}
