package upnp

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/dlna/discovery"
)

// Backlog B163: the manual poller caches its server under
// upnpingest.StableServerKey's "manual:<sha256 of the manual URL>", and an
// SSDP packet names its cache entry by whatever its USN carries. One packet
// whose USN was that key landed on the manual server's entry: an alive from
// another host re-fetched the description there and replaced the control URL,
// the description URL and the dial approval with the sender's (the ingest and
// LiveHost then went there), and refreshed LastSeenAt, so a manual server that
// was down showed online; a byebye removed the entry. The key is a hash of the
// manual URL, not published, but a NAS URL is guessable.

// manualServerKey has the shape StableServerKey gives a manual upstream.
var manualServerKey = "manual:" + strings.Repeat("ab", 32)

func cachedManualServer(cache *ServerCache, seen time.Time) ServerInfo {
	manual := ServerInfo{
		UDN:                        manualServerKey,
		FriendlyName:               "NAS",
		ContentDirectoryControlURL: "http://192.0.2.7:8200/ctl/ContentDir",
		DescriptionURL:             "http://192.0.2.7:8200/desc.xml",
		DialApproval:               discovery.OperatorChose("http://192.0.2.7:8200/desc.xml"),
		LastSeenAt:                 seen,
	}
	cache.UpsertConfigured(manual)
	return manual
}

func spoofer() *net.UDPAddr { return &net.UDPAddr{IP: net.IPv4(192, 0, 2, 66), Port: 1900} }

func TestHandlePacket_AManualKeyAliveLeavesTheManualServersEntryAlone(t *testing.T) {
	for _, usnUDN := range []string{manualServerKey, strings.ToUpper(manualServerKey)} {
		t.Run(usnUDN[:8], func(t *testing.T) {
			disp := &recordingDispatcher{}
			cache := NewServerCache()
			earlier := time.Now().Add(-time.Hour)
			manual := cachedManualServer(cache, earlier)
			c := newServerDiscoveryTestClient(t, disp, cache)

			c.handlePacket(context.Background(), alivePacket(usnUDN, "http://192.0.2.66:9001/desc.xml"), spoofer())
			c.wg.Wait() // any description fetch the packet started

			if n := disp.fetchCount(); n != 0 {
				t.Errorf("the packet fetched a description %d times, want 0", n)
			}
			got, ok := cache.Get(manualServerKey)
			if !ok {
				t.Fatal("the manual server's entry is gone")
			}
			if got.ContentDirectoryControlURL != manual.ContentDirectoryControlURL ||
				got.DescriptionURL != manual.DescriptionURL || got.DialApproval != manual.DialApproval {
				t.Errorf("the manual server's entry now leads to (%q, %q, %+v), want its own (%q, %q, %+v)",
					got.ContentDirectoryControlURL, got.DescriptionURL, got.DialApproval,
					manual.ContentDirectoryControlURL, manual.DescriptionURL, manual.DialApproval)
			}
			if !got.LastSeenAt.Equal(earlier) {
				t.Errorf("the packet refreshed the manual server's LastSeenAt to %v: a down server would show online",
					got.LastSeenAt)
			}
			if _, ok := cache.Get(usnUDN); ok && usnUDN != manualServerKey {
				t.Errorf("the packet cached a server under %q", usnUDN)
			}
		})
	}
}

func TestHandlePacket_AManualKeyByebyeLeavesTheManualServersEntryAlone(t *testing.T) {
	cache := NewServerCache()
	cachedManualServer(cache, time.Now().Add(-time.Hour))
	c := newServerDiscoveryTestClient(t, &recordingDispatcher{}, cache)
	bye := []byte("NOTIFY * HTTP/1.1\r\n" +
		"HOST: 239.255.255.250:1900\r\n" +
		"NT: urn:schemas-upnp-org:device:MediaServer:1\r\n" +
		"NTS: ssdp:byebye\r\n" +
		"USN: " + manualServerKey + "::urn:schemas-upnp-org:device:MediaServer:1\r\n" +
		"\r\n")
	c.handlePacket(context.Background(), bye, spoofer())
	if _, ok := cache.Get(manualServerKey); !ok {
		t.Error("a byebye naming the manual server's key removed its entry")
	}
}

// A real device's UDN, and an odd one outside both namespaces, still work:
// the refusal is the bridge's own key space, not everything that is not
// uuid: (backlog B163's constraint).
func TestHandlePacket_AnOddUDNOutsideTheManualKeySpaceIsStillCached(t *testing.T) {
	for _, udn := range []string{"uuid:4d696e69-444c-164e-9d41-001122334455", "UUID:odd-firmware-1", "nas-without-uuid"} {
		t.Run(udn, func(t *testing.T) {
			disp := &recordingDispatcher{}
			cache := NewServerCache()
			c := newServerDiscoveryTestClient(t, disp, cache)
			c.handlePacket(context.Background(), alivePacket(udn, "http://192.0.2.9:8200/desc.xml"), nil)
			c.wg.Wait()
			if got, ok := cache.Get(udn); !ok || got.ContentDirectoryControlURL == "" {
				t.Errorf("a first sighting of %q cached (%+v, %v), want an entry with a control URL", udn, got, ok)
			}
		})
	}
}
