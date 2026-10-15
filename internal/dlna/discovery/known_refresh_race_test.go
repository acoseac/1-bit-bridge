package discovery

import (
	"context"
	"testing"
	"time"
)

// Backlog B165: the handler looks a known renderer up and refreshes its
// LastSeenAt later, an Upsert of `{UDN, LastSeenAt}`. An EvictStale between
// the two came back as a stub with no ControlURL, which Snapshot hides and
// the stub branch never refreshes or fetches again, so a live renderer was
// missing from /v1/renderers until the stub aged out (RendererTTL). The
// upstream client closed the same race with ServerCache.Touch on 2026-09-28.

func cacheKnownRenderer(c *SSDPDiscoveryClient, seen time.Time) {
	c.cache.Upsert(RendererInfo{
		UDN:          movedUDN,
		FriendlyName: "Chord 2go",
		ControlURL:   "http://192.0.2.7:8080/avtransport/control",
		LastSeenAt:   seen,
	})
}

// inRefreshWindow runs during between the handler's lookup of a known
// renderer and its refresh, on every packet until the test ends.
func inRefreshWindow(t *testing.T, during func(udn string)) {
	t.Helper()
	beforeKnownRefreshHook = during
	t.Cleanup(func() { beforeKnownRefreshHook = nil })
}

func TestHandlePacket_ARendererEvictedBeforeItsRefreshIsFoundAgainOnItsNextAnnouncement(t *testing.T) {
	disp := &countingDispatcher{handler: descriptionOnlyHandler}
	c := newTestClient(t, disp)
	cacheKnownRenderer(c, time.Date(2026, 5, 26, 11, 0, 0, 0, time.UTC))
	evicting := true
	inRefreshWindow(t, func(udn string) {
		if evicting {
			c.cache.Remove(udn) // what EvictStale does to an entry past RendererTTL
		}
	})
	alive := alivePacket(movedUDN, "http://192.0.2.7:8080/description.xml")

	c.handlePacket(context.Background(), alive, nil)
	c.wg.Wait()
	if got, ok := c.cache.Get(movedUDN); ok {
		t.Errorf("the refresh stored %+v for a renderer evicted before it", got)
	}

	evicting = false
	c.handlePacket(context.Background(), alive, nil)
	c.wg.Wait()
	if got, ok := c.cache.Get(movedUDN); !ok || got.ControlURL == "" {
		t.Errorf("the next announcement left (%+v, %v), want the renderer fetched again with its ControlURL "+
			"(%d fetches)", got, ok, disp.calls.Load())
	}
	if n := len(c.cache.Snapshot()); n != 1 {
		t.Errorf("/v1/renderers lists %d renderers, want the one that is announcing", n)
	}
}

// The stub rule holds through the window too: a stub that replaced the entry
// meanwhile (a failed re-fetch replaces, it never merges) keeps its
// LastSeenAt, so it still ages out and is retried.
func TestHandlePacket_AStubThatReplacedARendererBeforeItsRefreshKeepsItsLastSeenAt(t *testing.T) {
	c := newTestClient(t, &countingDispatcher{handler: descriptionOnlyHandler})
	earlier := time.Date(2026, 5, 26, 11, 0, 0, 0, time.UTC)
	cacheKnownRenderer(c, earlier)
	inRefreshWindow(t, func(udn string) {
		c.cache.Replace(RendererInfo{UDN: udn, LastSeenAt: earlier})
	})
	c.handlePacket(context.Background(), alivePacket(movedUDN, "http://192.0.2.7:8080/description.xml"), nil)
	c.wg.Wait()
	got, ok := c.cache.Get(movedUDN)
	if !ok {
		t.Fatal("the stub is gone")
	}
	if !got.LastSeenAt.Equal(earlier) || got.ControlURL != "" {
		t.Errorf("the stub became %+v, want it as the replace left it (LastSeenAt %v, no ControlURL)", got, earlier)
	}
}
