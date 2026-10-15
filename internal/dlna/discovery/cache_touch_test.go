package discovery

import (
	"testing"
	"time"
)

// TestRendererCacheTouch pins the refresh the SSDP handler makes for a known
// renderer (backlog B165): one step under the lock, nothing stored for a UDN
// the cache does not hold, a stub left as it is, and every field but
// LastSeenAt kept.
func TestRendererCacheTouch(t *testing.T) {
	cache := NewRendererCache()
	at := time.Unix(100, 0)
	later := at.Add(time.Minute)

	if cache.Touch("uuid:gone", later) {
		t.Error("Touch reported a renderer the cache does not hold")
	}
	if _, ok := cache.Get("uuid:gone"); ok {
		t.Fatal("Touch stored a renderer the cache did not hold")
	}

	cache.Upsert(RendererInfo{UDN: "uuid:stub", LastSeenAt: at})
	if cache.Touch("uuid:stub", later) {
		t.Error("Touch refreshed a stub")
	}
	if stub, _ := cache.Get("uuid:stub"); !stub.LastSeenAt.Equal(at) {
		t.Errorf("a stub's LastSeenAt moved to %v, want %v: it must age out", stub.LastSeenAt, at)
	}

	live := RendererInfo{
		UDN: "uuid:live", FriendlyName: "Chord 2go", Manufacturer: "Chord",
		ControlURL: "http://192.0.2.7:8080/avt", EventURL: "http://192.0.2.7:8080/evt",
		RenderingControlURL: "http://192.0.2.7:8080/rc", SinkProtocolInfos: []string{"http-get:*:audio/flac:*"},
		LastSeenAt: at,
	}
	cache.Upsert(live)
	if !cache.Touch("uuid:live", later) {
		t.Fatal("Touch did not refresh a renderer the cache holds")
	}
	got, _ := cache.Get("uuid:live")
	if !got.LastSeenAt.Equal(later) {
		t.Errorf("LastSeenAt = %v, want %v", got.LastSeenAt, later)
	}
	got.LastSeenAt = live.LastSeenAt
	if got.FriendlyName != live.FriendlyName || got.Manufacturer != live.Manufacturer ||
		got.ControlURL != live.ControlURL || got.EventURL != live.EventURL ||
		got.RenderingControlURL != live.RenderingControlURL || len(got.SinkProtocolInfos) != 1 {
		t.Errorf("after Touch: %+v, want every field but LastSeenAt as it was: %+v", got, live)
	}
}
