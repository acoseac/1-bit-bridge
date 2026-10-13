package doctor

import (
	"net"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/advertise"
	servertls "github.com/acoseac/1-bit-bridge/internal/tls"
)

func TestAWantSetThatDroppedAContainerNameIsCovered(t *testing.T) {
	minted := servertls.GenerateOptions{Hostname: "0123456789ab"}
	c := checkTLSCertSANs(t.Context(), certFixture(t, minted, servertls.GenerateOptions{}))
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
		PairingPrimary: func() (string, bool) { return "", false },
	})
	if warned.Status != Warn || warned.Hint != advertise.PairingNeedsEndpoint {
		t.Fatalf("empty primary: %+v", warned)
	}
	okd := checkPairingURL(t.Context(), Deps{
		PairingPrimary: func() (string, bool) { return "https://192.0.2.10:7788", true },
	})
	if okd.Status != OK || !strings.Contains(okd.Summary, "https://192.0.2.10:7788") {
		t.Fatalf("dialable primary: %+v", okd)
	}
}
