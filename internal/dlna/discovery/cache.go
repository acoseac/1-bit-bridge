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
	if len(fresh.SinkProtocolInfos) > 0 {
		out.SinkProtocolInfos = fresh.SinkProtocolInfos
	}
	if !fresh.LastSeenAt.IsZero() {
		out.LastSeenAt = fresh.LastSeenAt
	}
	return out
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
		out = append(out, info)
	}
	sort.SliceStable(out, func(i, j int) bool {
		li := lowercase(out[i].FriendlyName)
		lj := lowercase(out[j].FriendlyName)
		if li != lj {
			return li < lj
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

// lowercase is a tiny ASCII-only helper for stable case-insensitive
// sort. `strings.ToLower` would work but adds a Unicode normalization
// layer that's unnecessary for the FriendlyName comparison.
func lowercase(s string) string {
	b := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 32
		}
		b[i] = c
	}
	return string(b)
}
