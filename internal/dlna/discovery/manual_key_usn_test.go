package discovery

import (
	"context"
	"strings"
	"testing"
)

// Backlog B163: both discovery clients key their caches by the UDN a packet's
// USN carries, and "manual:<sha256 of the URL>" is how the upstream ingest
// keys a manual server in its cache (upnpingest.StableServerKey). No device
// announces one (UPnP UDA's UDNs begin "uuid:"), so a USN in that key space
// names nothing; a UDN outside both namespaces still does.
func TestUDNFromUSNAnswersNothingForTheBridgesOwnKeySpace(t *testing.T) {
	key := "manual:" + strings.Repeat("ab", 32)
	for _, tc := range []struct{ usn, want string }{
		{key + "::urn:schemas-upnp-org:device:MediaServer:1", ""},
		{key, ""},
		{strings.ToUpper(key) + "::urn:schemas-upnp-org:device:MediaServer:1", ""},
		{" " + key + "::urn:schemas-upnp-org:device:MediaServer:1", ""},
		{"manual:no-url:nas::urn:schemas-upnp-org:device:MediaServer:1", ""},
		{"uuid:abc::urn:schemas-upnp-org:device:MediaRenderer:1", "uuid:abc"},
		{"uuid:abc", "uuid:abc"},
		{"UUID:odd-firmware::upnp:rootdevice", "UUID:odd-firmware"},
		{"nas-without-uuid::urn:schemas-upnp-org:device:MediaServer:1", "nas-without-uuid"},
		{"uuid:manual:abc", "uuid:manual:abc"},
		// The check reads the prefix's bytes: a UDN shorter than it, and one
		// whose first rune is several bytes, are kept.
		{"man", "man"},
		{"ñmanual:abc::upnp:rootdevice", "ñmanual:abc"},
		{"", ""},
	} {
		if got := UDNFromUSN(tc.usn); got != tc.want {
			t.Errorf("UDNFromUSN(%q) = %q, want %q", tc.usn, got, tc.want)
		}
	}
}

func TestHandlePacket_AManualKeyUSNNamesNoRenderer(t *testing.T) {
	disp := &countingDispatcher{handler: descriptionOnlyHandler}
	c := newTestClient(t, disp)
	udn := "manual:" + strings.Repeat("cd", 32)
	c.handlePacket(context.Background(), alivePacket(udn, "http://192.0.2.9:8080/description.xml"), nil)
	c.wg.Wait() // any description fetch the packet started
	if n := disp.calls.Load(); n != 0 {
		t.Errorf("the packet fetched %d times, want 0", n)
	}
	if _, ok := c.cache.Get(udn); ok {
		t.Errorf("the packet cached a renderer under %q", udn)
	}
}
