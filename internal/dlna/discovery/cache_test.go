package discovery

import (
	"sync"
	"testing"
	"time"
)

func TestRendererCache_UpsertAndSnapshot(t *testing.T) {
	c := NewRendererCache()
	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	c.Upsert(RendererInfo{
		UDN:          "uuid:bbb",
		FriendlyName: "Bluesound Node",
		ControlURL:   "http://192.0.2.2/ctrl",
		LastSeenAt:   now,
	})
	c.Upsert(RendererInfo{
		UDN:          "uuid:aaa",
		FriendlyName: "Apple TV",
		ControlURL:   "http://192.0.2.1/ctrl",
		LastSeenAt:   now,
	})
	snap := c.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("snap len = %d, want 2", len(snap))
	}
	// Sort by FriendlyName (case-insensitive) → Apple TV first.
	if snap[0].UDN != "uuid:aaa" {
		t.Errorf("snap[0] = %q, want uuid:aaa (Apple TV sorts first)", snap[0].UDN)
	}
}

func TestRendererCache_Snapshot_SkipsIncompleteStubs(t *testing.T) {
	// A stub with no AVTransport ControlURL (residue of a failed or
	// in-flight detail fetch) must NOT surface in /v1/renderers — it's an
	// undrivable, nameless row. Only Snapshot hides it; the entry stays in
	// the cache so the discovery loop's exists-branch can manage it.
	// (bridge-12.)
	c := NewRendererCache()
	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	c.Upsert(RendererInfo{UDN: "uuid:stub", LastSeenAt: now}) // no ControlURL
	c.Upsert(RendererInfo{
		UDN: "uuid:real", FriendlyName: "Real Renderer",
		ControlURL: "http://192.0.2.5/ctrl", LastSeenAt: now,
	})
	snap := c.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("snap len = %d, want 1 (stub filtered)", len(snap))
	}
	if snap[0].UDN != "uuid:real" {
		t.Errorf("snap[0] = %q, want uuid:real", snap[0].UDN)
	}
	if _, ok := c.Get("uuid:stub"); !ok {
		t.Error("stub should remain in the cache (Get), only filtered from Snapshot")
	}
}

func TestRendererCache_Snapshot_StableSortAcrossInsertOrder(t *testing.T) {
	// Insert in two different orders, snapshot order must agree.
	build := func(order []string) []RendererInfo {
		c := NewRendererCache()
		now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
		nameByUDN := map[string]string{
			"uuid:1": "Zappa",
			"uuid:2": "Aretha",
			"uuid:3": "Monk",
		}
		for _, udn := range order {
			c.Upsert(RendererInfo{
				UDN: udn, FriendlyName: nameByUDN[udn],
				ControlURL: "http://192.0.2.9/ctrl", LastSeenAt: now,
			})
		}
		return c.Snapshot()
	}
	a := build([]string{"uuid:1", "uuid:2", "uuid:3"})
	b := build([]string{"uuid:3", "uuid:2", "uuid:1"})
	if len(a) != len(b) {
		t.Fatalf("len mismatch %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i].UDN != b[i].UDN {
			t.Errorf("order diverges at i=%d: %q vs %q", i, a[i].UDN, b[i].UDN)
		}
	}
}

func TestRendererCache_UpsertMergesNonZeroFields(t *testing.T) {
	c := NewRendererCache()
	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	// Initial entry with full metadata (post-fetchDeviceDescription).
	c.Upsert(RendererInfo{
		UDN:               "uuid:full",
		FriendlyName:      "2go",
		Manufacturer:      "Chord",
		ControlURL:        "http://x/control",
		SinkProtocolInfos: []string{"http-get:*:audio/x-dsf:*"},
		LastSeenAt:        now,
	})
	// Refresh via ssdp:alive — only UDN + new lastSeenAt available.
	later := now.Add(30 * time.Second)
	c.Upsert(RendererInfo{
		UDN:        "uuid:full",
		LastSeenAt: later,
	})
	info, ok := c.Get("uuid:full")
	if !ok {
		t.Fatal("entry lost")
	}
	if info.FriendlyName != "2go" {
		t.Errorf("FriendlyName lost on merge: %q", info.FriendlyName)
	}
	if info.Manufacturer != "Chord" {
		t.Errorf("Manufacturer lost on merge: %q", info.Manufacturer)
	}
	if len(info.SinkProtocolInfos) != 1 {
		t.Errorf("SinkProtocolInfos lost on merge: %v", info.SinkProtocolInfos)
	}
	if !info.LastSeenAt.Equal(later) {
		t.Errorf("LastSeenAt should advance to fresh: got %v", info.LastSeenAt)
	}
}

func TestRendererCache_UpsertFreshFieldsWin(t *testing.T) {
	c := NewRendererCache()
	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	c.Upsert(RendererInfo{UDN: "uuid:x", FriendlyName: "Old Name", LastSeenAt: now})
	c.Upsert(RendererInfo{UDN: "uuid:x", FriendlyName: "New Name", LastSeenAt: now})
	info, _ := c.Get("uuid:x")
	if info.FriendlyName != "New Name" {
		t.Errorf("fresh FriendlyName should win, got %q", info.FriendlyName)
	}
}

func TestRendererCache_UpsertRejectsEmptyUDN(t *testing.T) {
	c := NewRendererCache()
	c.Upsert(RendererInfo{UDN: "", FriendlyName: "no udn"})
	if c.Len() != 0 {
		t.Errorf("empty-UDN entry should be rejected, got %d entries", c.Len())
	}
}

func TestRendererCache_Remove(t *testing.T) {
	c := NewRendererCache()
	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	c.Upsert(RendererInfo{UDN: "uuid:x", LastSeenAt: now})
	c.Remove("uuid:x")
	if c.Len() != 0 {
		t.Errorf("Remove didn't drop entry, got %d entries", c.Len())
	}
	// Idempotent.
	c.Remove("uuid:x")
	c.Remove("uuid:never-existed")
}

func TestRendererCache_EvictStale(t *testing.T) {
	c := NewRendererCache()
	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	c.Upsert(RendererInfo{UDN: "uuid:fresh", LastSeenAt: now.Add(-30 * time.Second)})
	c.Upsert(RendererInfo{UDN: "uuid:stale", LastSeenAt: now.Add(-5 * time.Minute)})
	evicted := c.EvictStale(now, 60*time.Second)
	if evicted != 1 {
		t.Errorf("EvictStale returned %d, want 1", evicted)
	}
	if _, ok := c.Get("uuid:fresh"); !ok {
		t.Error("fresh entry incorrectly evicted")
	}
	if _, ok := c.Get("uuid:stale"); ok {
		t.Error("stale entry NOT evicted")
	}
}

func TestRendererCache_Snapshot_EmptyIsEmptySliceNotNil(t *testing.T) {
	c := NewRendererCache()
	snap := c.Snapshot()
	if snap == nil {
		t.Fatal("Snapshot must return empty slice, not nil")
	}
	if len(snap) != 0 {
		t.Errorf("len = %d, want 0", len(snap))
	}
}

func TestRendererCache_Clear(t *testing.T) {
	c := NewRendererCache()
	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	c.Upsert(RendererInfo{UDN: "uuid:x", LastSeenAt: now})
	c.Upsert(RendererInfo{UDN: "uuid:y", LastSeenAt: now})
	c.Clear()
	if c.Len() != 0 {
		t.Errorf("Clear should empty the cache, got %d entries", c.Len())
	}
}

// Concurrent Upsert + Snapshot under `-race` — pins the
// RWMutex contract.
func TestRendererCache_ConcurrentAccessIsRaceFree(t *testing.T) {
	c := NewRendererCache()
	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	const writers = 16
	const reads = 200
	var wg sync.WaitGroup
	wg.Add(writers + 1)
	for i := 0; i < writers; i++ {
		i := i
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				c.Upsert(RendererInfo{
					UDN:        "uuid:" + string(rune('a'+i)),
					LastSeenAt: now.Add(time.Duration(j) * time.Millisecond),
				})
			}
		}()
	}
	go func() {
		defer wg.Done()
		for r := 0; r < reads; r++ {
			_ = c.Snapshot()
			_ = c.Len()
		}
	}()
	wg.Wait()
}
