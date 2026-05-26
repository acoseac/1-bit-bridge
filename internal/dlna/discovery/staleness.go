package discovery

import "time"

// IsStaleRenderer returns true when the renderer's most recent
// observation (`lastSeenAt`) is older than `ttl` relative to `now`.
// Pure function — pinned by `staleness_test.go` against the
// boundary cases (zero TTL, future lastSeenAt from clock skew,
// negative interval).
//
// Used by `RendererCache.evictStale(now)` (called from the
// `SSDPDiscoveryClient`'s periodic tick) to drop entries for
// renderers that silently disappeared without sending an
// explicit `ssdp:byebye`. The TTL window (default 60s,
// `cfg.DLNA.Discovery.RendererTTLSeconds`) sits comfortably above
// the 30s M-SEARCH cadence — a renderer that's still on the
// network gets refreshed at least once per TTL window, and one
// missed observation cycle is not enough to evict.
//
// **Future lastSeenAt is treated as fresh** — minor clock drift
// (NTP corrections, monotonic vs wall-clock skew on a host that's
// been suspended) shouldn't false-positive on staleness.
// `now.Sub(lastSeenAt)` returning a negative interval falls into
// the `interval < 0` branch and short-circuits to false.
func IsStaleRenderer(lastSeenAt, now time.Time, ttl time.Duration) bool {
	if ttl <= 0 {
		// Zero / negative TTL = "stale immediately" semantically
		// doesn't fit any real use case + would evict everything;
		// treat as "never stale" so a misconfigured ttl doesn't
		// silently wipe the cache.
		return false
	}
	interval := now.Sub(lastSeenAt)
	if interval < 0 {
		// Clock skew / suspended-host catch-up — treat as fresh.
		return false
	}
	return interval > ttl
}
