package discovery

// MaxPendingDetailFetches is how many detail fetches one discovery client
// holds at once: from the packet that dispatches a fetch until it returns,
// the ones still queued for the client's fetch semaphore included.
//
// A fetch is a goroutine from its dispatch, and the semaphore bounds only
// how many of them RUN (four for the renderer client, two for the upstream
// MediaServer client). So without this bound every announcement that
// dispatched a fetch queued a goroutine, and a flood of them queued one per
// packet: measured on 2026-09-28, 10,000 packets cost 10,000 goroutines and
// 35 to 37 MiB of stack in either client, for one UDN in the upstream client
// (whose fetches were not deduplicated) and for 10,000 distinct UDNs in
// both. A LAN peer sees the M-SEARCH's source port and can send that flood
// to it, and one whose LOCATION never answers holds each fetch for the
// whole DetailFetchTimeout.
//
// 64 is far above any real LAN's burst of new devices: a power cut that
// brings thirty renderers back at once claims thirty. At the bound a
// dispatch is dropped, not queued, and the device's next announcement
// dispatches again, within one M-SEARCH cycle; a flood that keeps the set
// full delays a genuine new device until the flood stops, where the unbounded
// queue put it behind every packet the flood had sent.
const MaxPendingDetailFetches = 64

// DetailFetchClaims is the set of UDNs one discovery client has a detail
// fetch claimed for. Both discovery clients keep one, this package's
// renderer client and internal/upnp's MediaServer client, so a burst of
// announcements for one device dispatches one fetch, and a flood of them
// holds at most MaxPendingDetailFetches goroutines.
//
// It has no lock of its own. Each client guards it with the mutex that
// guards its recorded Locations, which lets the renderer client's
// pruneLocations decide from both under one lock: a concurrent fetch can
// neither record nor release while it runs.
//
// A claim is taken before the fetch's goroutine is spawned and released by
// that goroutine's last deferred call (registered after its WaitGroup Done,
// so it runs first): once a client's Stop has joined its fetches, every
// claim is released, and a restarted client dispatches as a fresh one.
type DetailFetchClaims map[string]struct{}

// Claim reserves udn's one detail fetch. It answers false, and changes
// nothing, when udn already holds one, or when MaxPendingDetailFetches are
// claimed; a caller told false spawns no fetch.
func (f DetailFetchClaims) Claim(udn string) bool {
	if _, held := f[udn]; held {
		return false
	}
	if len(f) >= MaxPendingDetailFetches {
		return false
	}
	f[udn] = struct{}{}
	return true
}

// Release frees udn's claim. Only the claimed fetch's goroutine calls it.
func (f DetailFetchClaims) Release(udn string) { delete(f, udn) }

// Held reports whether udn has a detail fetch claimed.
func (f DetailFetchClaims) Held(udn string) bool {
	_, held := f[udn]
	return held
}
