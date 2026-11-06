package doctor

import (
	"net"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/advertise"
	servertls "github.com/acoseac/1-bit-bridge/internal/tls"
)

func TestAWantSetThatDroppedAContainerNameIsCovered(t *testing.T) {
	// A LAN IPv4 address on both sides, as a customEndpoints entry gives a
	// container, so the line is about the name alone: a loopback bridge
	// advertising none is warned about that
	// (TestCheckTLSCertSANs_NoLANIPv4AdvertisedWarns).
	lan := []net.IP{net.ParseIP("192.168.1.20")}
	minted := servertls.GenerateOptions{Hostname: "0123456789ab", ExtraIPs: lan}
	c := checkTLSCertSANs(t.Context(), certFixture(t, minted, servertls.GenerateOptions{ExtraIPs: lan}))
	if c.Status != OK {
		t.Fatalf("status = %q, want ok\nsummary %s\nhint %s", c.Status, c.Summary, c.Hint)
	}
	if strings.Contains(c.Hint, "0123456789ab") {
		t.Fatalf("hint still names the container id: %s", c.Hint)
	}
}

func TestAMissingCustomHostStillWarnsAfterTheContainerNameLeft(t *testing.T) {
	minted := servertls.GenerateOptions{
		Hostname: "0123456789ab",
		ExtraIPs: []net.IP{net.ParseIP("192.0.2.10")},
	}
	want := servertls.GenerateOptions{
		ExtraDNSNames: []string{"library.example.test"},
		ExtraIPs:      []net.IP{net.ParseIP("192.0.2.10")},
	}
	c := checkTLSCertSANs(t.Context(), certFixture(t, minted, want))
	if c.Status != Warn {
		t.Fatalf("status = %q, want warn\nsummary %s", c.Status, c.Summary)
	}
	if !strings.Contains(c.Hint, "library.example.test") {
		t.Fatalf("hint does not name the custom host: %s", c.Hint)
	}
	if strings.Contains(c.Hint, "0123456789ab") {
		t.Fatalf("hint names the container id that left the want-set: %s", c.Hint)
	}
}

func TestPairingURLWarnsWhenTheContainerHasNoDialAddress(t *testing.T) {
	absent := checkPairingURL(t.Context(), Deps{})
	if absent.Status != OK || absent.Summary != "not checked" {
		t.Fatalf("nil primary: %+v", absent)
	}
	warned := checkPairingURL(t.Context(), Deps{
		PairingPrimary: func() (string, string) { return "", "" },
	})
	if warned.Status != Warn || warned.Hint != advertise.PairingNeedsEndpoint {
		t.Fatalf("empty primary: %+v", warned)
	}
	okd := checkPairingURL(t.Context(), Deps{
		PairingPrimary: func() (string, string) { return "https://192.0.2.10:7788", "" },
	})
	if okd.Status != OK || okd.Summary != "pairing QR dials https://192.0.2.10:7788" {
		t.Fatalf("dialable primary: %+v", okd)
	}
}

// TestThePairingURLLineSaysWhyItIsNotTheHostsName is B739's second round: a
// host name only the bridge's own mDNS answers, with mDNS off, is paired at
// an address, and the pairing-url line says why beside the URL, or beside
// "no address a phone can dial" when there is none.
func TestThePairingURLLineSaysWhyItIsNotTheHostsName(t *testing.T) {
	const why = "mdns.enabled is false, and only the bridge's own mDNS answers xn--f1ai0a.local, " +
		"the host's name in its ASCII form"
	okd := checkPairingURL(t.Context(), Deps{
		PairingPrimary: func() (string, string) { return "https://192.0.2.10:7788", why },
	})
	if okd.Status != OK || okd.Summary != "pairing QR dials https://192.0.2.10:7788: "+why {
		t.Fatalf("a primary with a reason: %+v", okd)
	}
	warned := checkPairingURL(t.Context(), Deps{
		PairingPrimary: func() (string, string) { return "", why },
	})
	if warned.Status != Warn || warned.Summary != "no address a phone can dial: "+why ||
		warned.Hint != advertise.PairingNeedsEndpoint {
		t.Fatalf("no primary, with a reason: %+v", warned)
	}
}
