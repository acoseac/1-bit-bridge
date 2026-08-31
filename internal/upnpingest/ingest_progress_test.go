package upnpingest

import (
	"context"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/upnp"
)

// A walk of a 15,000-track upstream took minutes with nothing on screen —
// no counter, no in-flight marker — and the admin page does not re-fetch
// after load, so even the after-the-fact "Last walk" line waited for a
// manual reload. The filesystem scanner has published progress since it
// was written; these pin its twin for the ingest.

// TestWalkProgressIsVisibleDuringTheWalkAndClearsAfter is the whole
// point: the counter has to be readable WHILE the walk holds runMu, or it
// reports only on a walk that has already finished.
func TestWalkProgressIsVisibleDuringTheWalkAndClearsAfter(t *testing.T) {
	stub := newStubSOAP()
	stub.addRoute("GetSystemUpdateID", wrapSystemUpdateID("0"))
	stub.addRoute("Browse", wrapBrowse(
		`<DIDL-Lite xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:upnp="urn:schemas-upnp-org:metadata-1-0/upnp/">`+
			`<container id="64$0" parentID="64"><dc:title>Music</dc:title><upnp:class>object.container.storageFolder</upnp:class></container>`+
			`</DIDL-Lite>`, 1, 1))
	stub.addRoute("Browse", wrapBrowse(
		`<DIDL-Lite xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:upnp="urn:schemas-upnp-org:metadata-1-0/upnp/">`+
			`<item id="64$0$0" parentID="64$0">`+
			`<dc:title>One</dc:title><upnp:class>object.item.audioItem.musicTrack</upnp:class>`+
			`<upnp:artist>A</upnp:artist><upnp:album>B</upnp:album>`+
			`<res protocolInfo="http-get:*:audio/x-flac:*" size="1">http://h:8200/1.flac</res>`+
			`</item>`+
			`<item id="64$0$1" parentID="64$0">`+
			`<dc:title>Two</dc:title><upnp:class>object.item.audioItem.musicTrack</upnp:class>`+
			`<upnp:artist>A</upnp:artist><upnp:album>B</upnp:album>`+
			`<res protocolInfo="http-get:*:audio/x-flac:*" size="1">http://h:8200/2.flac</res>`+
			`</item></DIDL-Lite>`, 2, 2))

	client := upnp.NewContentDirectoryClient(stub)
	store := openIngestTestStore(t)
	cfg := config.UPnPUpstreamConfig{
		Enabled: true,
		Servers: []config.UPnPUpstreamServerConfig{
			{Name: "2Go", UDN: "uuid:test", PathPrefix: "Chord 2Go"},
		},
	}
	ing, err := NewIngester(cfg, client, &stubResolver{controlURL: "http://h:8200/ctl/CD"}, store, nil)
	if err != nil {
		t.Fatalf("NewIngester: %v", err)
	}

	// Before anything runs: not walking, and nothing to report.
	if got := ing.WalkProgress(); got.Walking || got.Key != "" || got.Items != 0 {
		t.Fatalf("idle ingester reports %+v, want a zero status", got)
	}

	// Sampled from INSIDE the walk. The stub dispatcher is called on the
	// walk's own goroutine while runMu is held, so reading here proves the
	// status is reachable during the walk rather than only after it — a
	// counter published under the same lock would deadlock or, worse,
	// silently report nothing until the work was already done.
	var sawWalking bool
	var sawKey string
	stub.onCall = func() {
		if st := ing.WalkProgress(); st.Walking {
			sawWalking = true
			sawKey = st.Key
		}
	}

	if _, err := ing.Run(context.Background(), Options{ForceWalk: true}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if !sawWalking {
		t.Error("WalkProgress never reported a walk in flight; the progress line " +
			"would stay hidden for the whole walk, which is the case it exists for")
	}
	if want := StableServerKey(cfg.Servers[0]); sawKey != want {
		t.Errorf("in-flight key = %q, want the StableServerKey %q — matching on "+
			"anything else cannot be joined to a configured row", sawKey, want)
	}
	// Cleared afterwards, or the UI shows a walk that has finished.
	if got := ing.WalkProgress(); got.Walking || got.Key != "" {
		t.Errorf("after the walk WalkProgress reports %+v, want cleared", got)
	}
}

// TestWalkProgressClearsAfterAFailedWalk pins the defer.
//
// The in-flight marker is what the SSE publisher gates on, so a walk that
// errors — an unreachable upstream, a SOAP fault — must clear it too, or
// the page shows a walk in progress forever and the event keeps
// publishing on the fast tick.
func TestWalkProgressClearsAfterAFailedWalk(t *testing.T) {
	stub := newStubSOAP()
	stub.addRoute("GetSystemUpdateID", wrapSystemUpdateID("0"))
	// No Browse route: the walk fails at its first request.

	client := upnp.NewContentDirectoryClient(stub)
	store := openIngestTestStore(t)
	cfg := config.UPnPUpstreamConfig{
		Enabled: true,
		Servers: []config.UPnPUpstreamServerConfig{
			{Name: "2Go", UDN: "uuid:test", PathPrefix: "Chord 2Go"},
		},
	}
	ing, err := NewIngester(cfg, client, &stubResolver{controlURL: "http://h:8200/ctl/CD"}, store, nil)
	if err != nil {
		t.Fatalf("NewIngester: %v", err)
	}
	if _, err := ing.Run(context.Background(), Options{ForceWalk: true}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := ing.WalkProgress(); got.Walking {
		t.Errorf("a failed walk left the in-flight marker set (%+v); the page would "+
			"show a walk forever and the event would publish on every fast tick", got)
	}
}
