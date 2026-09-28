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
//  4. makeRoomLocked — a new UDN arriving at MaxCachedDevices evicts the
//     stub that would expire first.
//
// The cache is the SINGLE source of truth for `/v1/renderers`. The
// HTTP handler calls `Snapshot()` once per request + serializes the
// slice; the read is cheap (RLock + copy of a typically <10-entry
// map).
//
// It holds at most MaxCachedDevices entries (see there): a new UDN evicts
// the stub that expires first when the cache is full, and is refused when
// no stub is left to evict.
type RendererCache struct {
	mu      sync.RWMutex
	entries map[string]RendererInfo // keyed on UDN
}

// MaxCachedDevices is how many devices one discovery cache holds: this
// package's RendererCache, and internal/upnp's ServerCache for the servers
// it finds through SSDP.
//
// A LAN peer can announce as many distinct UDNs as it likes, and both
// caches kept one entry per UDN for as long as that UDN was refreshed. A
// renderer whose LOCATION answered 4xx was worse: its stub carried a
// year-2999 LastSeenAt that no eviction pass reached, so a flood of such
// UDNs grew the cache and the client's location records for the life of
// the process (measured on 2026-09-28: 5,000 stubs and 5,000 records
// survived an eviction pass an hour later, about 490 bytes a UDN, and
// 100,000 fake MediaServers with a valid description held 45 MB for their
// ServerTTL; backlog B47).
//
// 256 is far above any real LAN, so a full cache means a flood, and a full
// cache never makes room by dropping a device it serves: the renderer
// cache evicts only stubs (the residue of a failed fetch, which Snapshot
// hides anyway), soonest to expire first, and refuses a new UDN once none
// is left, and the server cache refuses a new server that the operator did
// not configure. A device already cached therefore stays for as long as it
// keeps announcing, whatever a flood does, and a new device waits until the
// flood's entries expire.
const MaxCachedDevices = 256

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
//
// A NEW UDN is admitted as makeRoomLocked allows; the result reports
// whether info is stored.
func (c *RendererCache) Upsert(info RendererInfo) bool {
	if info.UDN == "" {
		return false // defensive — every legitimate entry has a UDN
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	existing, ok := c.entries[info.UDN]
	if !ok {
		if !c.makeRoomLocked() {
			return false
		}
		c.entries[info.UDN] = info
		return true
	}
	c.entries[info.UDN] = mergeRendererInfo(existing, info)
	return true
}

// makeRoomLocked reports whether the cache can take one more UDN. Below
// MaxCachedDevices it can; at the bound it evicts the stub (an entry with no
// ControlURL) whose LastSeenAt is earliest, the one EvictStale would drop
// first, and without a stub to evict it cannot. Caller holds c.mu.
//
// A renderer with a ControlURL is never evicted to make room: it is what
// /v1/renderers serves and what a phone may be driving, and the new UDN is
// one any LAN peer can announce.
func (c *RendererCache) makeRoomLocked() bool {
	if len(c.entries) < MaxCachedDevices {
		return true
	}
	victim := ""
	var soonest time.Time
	for udn, info := range c.entries {
		if info.ControlURL != "" {
			continue
		}
		if victim == "" || info.LastSeenAt.Before(soonest) {
			victim, soonest = udn, info.LastSeenAt
		}
	}
	if victim == "" {
		return false
	}
	delete(c.entries, victim)
	return true
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
//
// A NEW UDN is admitted as makeRoomLocked allows; the result reports whether
// info is stored. Replacing a UDN already cached is never refused.
func (c *RendererCache) Replace(info RendererInfo) bool {
	if info.UDN == "" {
		return false // defensive — every legitimate entry has a UDN
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.entries[info.UDN]; !ok && !c.makeRoomLocked() {
		return false
	}
	c.entries[info.UDN] = info
	return true
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
