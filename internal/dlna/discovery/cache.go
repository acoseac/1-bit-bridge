package discovery

import (
	"sort"
	"sync"
	"time"
)

// RendererCache is the in-memory store of currently-known renderers,
// keyed on UDN. Thread-safe via `sync.RWMutex` — readers (HTTP
// handler for `/v1/renderers`) take RLock; writers (SSDP packet
// handlers) take Lock.
//
// **Lifecycle**: entries enter via `Upsert` whenever the discovery
// client observes a renderer (M-SEARCH response OR ssdp:alive
// NOTIFY). Entries leave via three paths:
//
//  1. `Remove(udn)` — called from the `ssdp:byebye` handler when a
//     renderer announces its departure explicitly.
//  2. `EvictStale(now, ttl)` — called from the periodic tick (every
//     M-SEARCH cycle) to drop entries that haven't been observed
//     within `ttl` (default 60s). Covers the silent-disappearance
//     case (renderer power-pulled, network blip past the byebye
//     window).
//  3. `Clear()` — called from `SSDPDiscoveryClient.Stop` for clean
//     teardown. Optional in production (process exit drops everything
//     anyway) but useful for tests.
//
// The cache is the SINGLE source of truth for `/v1/renderers`. The
// HTTP handler calls `Snapshot()` once per request + serializes the
// slice; the read is cheap (RLock + copy of a typically <10-entry
// map).
type RendererCache struct {
	mu      sync.RWMutex
	entries map[string]RendererInfo // keyed on UDN
}

// NewRendererCache constructs an empty cache.
func NewRendererCache() *RendererCache {
	return &RendererCache{
		entries: make(map[string]RendererInfo),
	}
}

// Upsert adds or refreshes an entry in the cache. The `LastSeenAt`
// field on `info` is the authoritative timestamp — callers MUST
// stamp it before calling Upsert. (Stamping inside Upsert would
// hide clock injection for tests + couple the cache to a clock
// source it doesn't otherwise need.)
//
// When the entry exists, fields are MERGED: new non-zero fields
// from `info` override the cached ones; cached non-zero fields
// persist when `info` omits them. This handles the common
// "ssdp:alive only carries UDN + Location" case where a NOTIFY
// refreshes lastSeenAt without re-fetching the full
// DeviceDescription / GetProtocolInfo.
//
// Callers wanting a strict replace (e.g. post-`fetchDeviceDescription`
// rebuild) pass a fully-populated `info` — the merge happens to
// produce the same result.
func (c *RendererCache) Upsert(info RendererInfo) {
	if info.UDN == "" {
		return // defensive — every legitimate entry has a UDN
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	existing, ok := c.entries[info.UDN]
	if !ok {
		c.entries[info.UDN] = info
		return
	}
	c.entries[info.UDN] = mergeRendererInfo(existing, info)
}

// mergeRendererInfo combines a cached entry with a fresh one. Fresh
// non-zero fields win; cached values persist when fresh omits them.
// `LastSeenAt` always advances to the fresh timestamp when fresh is
// non-zero (the lastSeenAt monotonic advance is the cache's purpose).
func mergeRendererInfo(cached, fresh RendererInfo) RendererInfo {
	out := cached
	if fresh.FriendlyName != "" {
		out.FriendlyName = fresh.FriendlyName
	}
	if fresh.Manufacturer != "" {
		out.Manufacturer = fresh.Manufacturer
	}
	if fresh.ModelDescription != "" {
		out.ModelDescription = fresh.ModelDescription
	}
	if fresh.ModelName != "" {
		out.ModelName = fresh.ModelName
	}
	if fresh.ControlURL != "" {
		out.ControlURL = fresh.ControlURL
	}
	if fresh.EventURL != "" {
		out.EventURL = fresh.EventURL
	}
	if fresh.RenderingControlURL != "" {
		out.RenderingControlURL = fresh.RenderingControlURL
	}
	if len(fresh.SinkProtocolInfos) > 0 {
		out.SinkProtocolInfos = fresh.SinkProtocolInfos
	}
	if !fresh.LastSeenAt.IsZero() {
		out.LastSeenAt = fresh.LastSeenAt
	}
	return out
}

// Replace stores info as THE entry for its UDN, discarding whatever was
// cached rather than merging into it — atomically, so no observer ever sees
// the UDN absent.
//
// This is the detail-fetch path's writer: a fetch produces the complete truth
// about a renderer, so merging is not just unnecessary but actively wrong.
// mergeRendererInfo is non-empty-wins, so a FAILED fetch's ControlURL-less
// stub merged into a live entry would KEEP the dead ControlURL while
// refreshing LastSeenAt — pinning an undrivable renderer in the cache forever
// (EvictStale can't reach it: LastSeenAt advances on every announcement).
// That invariant used to be enforced by Removing the entry BEFORE the fetch,
// which left the renderer missing from /v1/renderers for the fetch's whole
// duration; enforcing it at the write instead keeps the entry visible.
//
// Callers holding only a PARTIAL observation — the LastSeenAt refresh on an
// ssdp:alive, which carries no service URLs — MUST use Upsert.
func (c *RendererCache) Replace(info RendererInfo) {
	if info.UDN == "" {
		return // defensive — every legitimate entry has a UDN
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[info.UDN] = info
}

// Remove drops the entry for the given UDN. Idempotent — removing
// a non-existent UDN is a no-op.
func (c *RendererCache) Remove(udn string) {
	if udn == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, udn)
}

// EvictStale removes every entry whose `lastSeenAt` is older than
// `ttl` relative to `now`. Returns the count of evicted entries
// (useful for telemetry / logging). Pure-evict semantics — no
// upsert / refresh path here; the SSDP listeners own that.
func (c *RendererCache) EvictStale(now time.Time, ttl time.Duration) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	var evicted int
	for udn, info := range c.entries {
		if IsStaleRenderer(info.LastSeenAt, now, ttl) {
			delete(c.entries, udn)
			evicted++
		}
	}
	return evicted
}

// Snapshot returns a stable-sorted copy of every entry currently in
// the cache. Sort key: FriendlyName (case-insensitive), then UDN as
// tie-breaker for deterministic test pinning.
//
// Returns an empty slice (NOT nil) for an empty cache so the JSON
// wire shape `{"renderers": []}` is consistent.
func (c *RendererCache) Snapshot() []RendererInfo {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if len(c.entries) == 0 {
		return []RendererInfo{}
	}
	out := make([]RendererInfo, 0, len(c.entries))
	for _, info := range c.entries {
		// Skip incomplete stubs — a cached entry with no AVTransport
		// ControlURL is the residue of a failed (or in-flight) detail
		// fetch and is unusable: iOS can't dispatch SetAVTransportURI to
		// it. Surfacing it would show a nameless, undrivable row in the
		// output picker. (Transient stubs age out + retry; structural
		// ones persist suppressed — see SSDPDiscoveryClient.) (bridge-12.)
		if info.ControlURL == "" {
			continue
		}
		out = append(out, info)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if cmp := lowercaseCompare(out[i].FriendlyName, out[j].FriendlyName); cmp != 0 {
			return cmp < 0
		}
		return out[i].UDN < out[j].UDN
	})
	return out
}

// Len returns the current entry count. Cheap (RLock + map len) so
// callers can branch on emptiness without a Snapshot copy.
func (c *RendererCache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.entries)
}

// Get returns the entry for the given UDN. The second return is
// false when the UDN isn't cached.
func (c *RendererCache) Get(udn string) (RendererInfo, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	info, ok := c.entries[udn]
	return info, ok
}

// Clear drops every entry. Called from `SSDPDiscoveryClient.Stop`
// for clean teardown.
func (c *RendererCache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = make(map[string]RendererInfo)
}

// lowercaseCompare is an allocation-free ASCII case-insensitive
// string comparator. Returns -1 / 0 / 1 like strings.Compare. Used
// in `Snapshot()`'s sort closure — the prior `lowercase(s)` form
// allocated a new string per side per comparison, an O(N log N)
// allocation tax per snapshot call. Per Gemini MEDIUM round-1 on
// PR #305.
func lowercaseCompare(a, b string) int {
	lenA, lenB := len(a), len(b)
	minLen := lenA
	if lenB < minLen {
		minLen = lenB
	}
	for i := 0; i < minLen; i++ {
		ca, cb := a[i], b[i]
		if ca >= 'A' && ca <= 'Z' {
			ca += 32
		}
		if cb >= 'A' && cb <= 'Z' {
			cb += 32
		}
		if ca != cb {
			if ca < cb {
				return -1
			}
			return 1
		}
	}
	if lenA == lenB {
		return 0
	}
	if lenA < lenB {
		return -1
	}
	return 1
}
