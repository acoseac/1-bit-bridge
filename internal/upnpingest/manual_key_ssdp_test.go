package upnpingest

import (
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/dlna/discovery"
)

// Backlog B163: the key a manual upstream is cached under is one no SSDP
// packet can name, so a LAN peer that guesses the manual URL (and with it
// the key, a hash of the URL) cannot land on that server's cache entry. A
// configured UDN is the device's own, and SSDP still names it.
func TestAManualServersKeyIsOneNoSSDPPacketCanName(t *testing.T) {
	for _, srv := range []config.UPnPUpstreamServerConfig{
		{Name: "NAS", ManualDescriptionURL: "http://192.0.2.7:8200/desc.xml"},
		{Name: "NAS"}, // the no-URL fallback only a test can reach
	} {
		key := StableServerKey(srv)
		if got := discovery.UDNFromUSN(key + "::urn:schemas-upnp-org:device:MediaServer:1"); got != "" {
			t.Errorf("an SSDP USN of %q names %q: a packet can land on that server's cache entry", key, got)
		}
	}
	key := StableServerKey(config.UPnPUpstreamServerConfig{Name: "X", UDN: "uuid:ABC"})
	if got := discovery.UDNFromUSN(key + "::urn:schemas-upnp-org:device:MediaServer:1"); got != key {
		t.Errorf("an SSDP USN of the configured UDN %q names %q, want the UDN", key, got)
	}
}
