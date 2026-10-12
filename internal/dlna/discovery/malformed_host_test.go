package discovery

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"net/url"
	"testing"

	"golang.org/x/net/idna"
)

// The four non-ASCII marks Go's url.Parse keeps in a host. ASCII controls
// and a raw space never get there.
var malformedHostMarks = []struct {
	name string
	esc  string
	r    rune
}{
	{"next line", "%C2%85", '\u0085'},
	{"no-break space", "%C2%A0", '\u00A0'},
	{"line separator", "%E2%80%A8", '\u2028'},
	{"ideographic space", "%E3%80%80", '\u3000'},
}

func TestAHostWithAControlOrASpaceIsMalformed(t *testing.T) {
	for _, mark := range malformedHostMarks {
		raw := "http://192.168.1.20" + mark.esc + ":8200/d.xml"
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("%s: parse: %v", mark.name, err)
		}
		host := u.Hostname()
		if !hostIsMalformed(host) {
			t.Fatalf("%s: host %q is not malformed", mark.name, host)
		}
		kind, addr := classifyHost(host)
		if kind != hostMalformed || addr.IsValid() {
			t.Fatalf("%s: classify = %v %v, want malformed", mark.name, kind, addr)
		}
		if hostKindAllowed(kind, hostMalformed) || hostKindAllowed(kind, hostThisMachine) || hostKindAllowed(kind, hostElsewhere) {
			t.Fatalf("%s: a malformed host was allowed", mark.name)
		}
	}
	// A real host is unchanged.
	kind, _ := classifyHost("192.168.1.20")
	if kind != hostElsewhere {
		t.Fatalf("192.168.1.20 classified %v", kind)
	}
	kind, _ = classifyHost("localhost")
	if kind != hostThisMachine {
		t.Fatalf("localhost classified %v", kind)
	}
	if _, err := url.Parse("http://127.0.0.1\x00/d.xml"); err == nil {
		t.Fatal("url.Parse accepted an ASCII NUL in a host")
	}
	if _, err := url.Parse("http://127.0.0.1 /d.xml"); err == nil {
		t.Fatal("url.Parse accepted a raw space in a host")
	}
}

func TestAMalformedHostIsNeverStrippedBeforeItIsClassified(t *testing.T) {
	// Stripping U+0085 would leave 127.0.0.1, which is this machine.
	u, err := url.Parse("http://127.0.0.1%C2%85/d.xml")
	if err != nil {
		t.Fatal(err)
	}
	kind, _ := classifyHost(u.Hostname())
	if kind != hostMalformed {
		t.Fatalf("classify = %v, want malformed (stripping the mark would say this machine)", kind)
	}
}

func TestAMalformedHostIsRefusedAtEverySiteThatJudgesOne(t *testing.T) {
	raw := "http://192.168.1.20%C2%85:8200/d.xml"
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if isFetchableURL(u) {
		t.Fatal("a malformed host is fetchable")
	}
	if got := fetchableLocation(raw); got != "" {
		t.Fatalf("fetchableLocation = %q, want empty", got)
	}
	if NamesCloudMetadataAddr("http://169.254.169.254%C2%85/") {
		t.Fatal("a malformed prefix of a metadata address was named as one")
	}
	if !NamesCloudMetadataAddr("http://169.254.169.254/") {
		t.Fatal("a real metadata address was not named")
	}

	base, err := url.Parse("http://192.168.1.20:8200/desc.xml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolveServiceURL(base, "http://192.168.1.20%C2%A0:8200/ctl", SourceDiscovered); !errors.Is(err, errServiceURLMalformedHost) {
		t.Fatalf("discovered service: %v, want malformed host", err)
	}
	badBase, err := url.Parse("http://192.168.1.20%E2%80%A8:8200/desc.xml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolveServiceURL(badBase, "/ctl", SourceUserChosen); !errors.Is(err, errServiceURLMalformedHost) {
		t.Fatalf("relative service on a malformed base: %v, want malformed host", err)
	}

	// A localhost name from a loopback source is kept. A malformed host
	// takes that same arm if it is not refused first: its address does
	// not parse, so the loopback-source exception would return it.
	loop := AnnouncedFrom(&net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 1900})
	if got := LocationPermittedBy("http://localhost/d.xml", loop); got == "" {
		t.Fatal("a localhost LOCATION from a loopback source was refused")
	}
	if got := LocationPermittedBy("http://127.0.0.1%E3%80%80/d.xml", loop); got != "" {
		t.Fatalf("malformed LOCATION from a loopback source = %q", got)
	}

	loopback, err := netip.ParseAddr("127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if !OperatorChose("http://127.0.0.1/").Permits(loopback) {
		t.Fatal("a loopback manual URL no longer approves this machine")
	}
	if OperatorChose("http://127.0.0.1%C2%85/").Permits(loopback) {
		t.Fatal("a malformed manual URL approved a connect to this machine")
	}
}

func TestFullwidthDigitsAndAPercent25StayOnTheDialCheck(t *testing.T) {
	u, err := url.Parse("http://１２７．０．０．１/d.xml")
	if err != nil {
		t.Fatal(err)
	}
	if hostIsMalformed(u.Hostname()) {
		t.Fatal("fullwidth digits are not a control or a space")
	}
	kind, _ := classifyHost(u.Hostname())
	if kind != hostElsewhere {
		t.Fatalf("fullwidth host classified %v, want elsewhere", kind)
	}
	ascii, err := idna.Lookup.ToASCII(u.Hostname())
	if err != nil {
		t.Fatal(err)
	}
	if ascii != "127.0.0.1" {
		t.Fatalf("IDNA mapped the fullwidth host to %q", ascii)
	}
	lan := AnnouncedFrom(&net.UDPAddr{IP: net.ParseIP("192.168.1.20"), Port: 1900})
	if got := LocationPermittedBy(u.String(), lan); got == "" {
		t.Fatal("the string check refused a host it classifies as elsewhere")
	}
	ctx := WithDialApproval(context.Background(), lan)
	if err := refuseUnapprovedHostLocal(ctx, "tcp", "127.0.0.1:80", nil); err == nil {
		t.Fatal("the dial check let a LAN announcement connect to 127.0.0.1")
	}

	pct, err := url.Parse("http://127.0.0.1%25.example.com/d.xml")
	if err != nil {
		t.Fatal(err)
	}
	if kind, _ := classifyHost(pct.Hostname()); hostIsMalformed(pct.Hostname()) || kind != hostElsewhere {
		t.Fatalf("%q classified malformed or not elsewhere", pct.Hostname())
	}
	if _, err := idna.Lookup.ToASCII(pct.Hostname()); err == nil {
		t.Fatal("IDNA accepted a host holding %")
	}
	if got := LocationPermittedBy(pct.String(), lan); got == "" {
		t.Fatal("the string check refused a %25 host it cannot place")
	}

	zone, err := url.Parse("http://[fe80::1%25en0]/d.xml")
	if err != nil {
		t.Fatal(err)
	}
	if kind, _ := classifyHost(zone.Hostname()); kind != hostLinkLocal || hostIsMalformed(zone.Hostname()) {
		t.Fatalf("a %%25 zone classified %v (malformed %v)", kind, hostIsMalformed(zone.Hostname()))
	}

	win, err := url.Parse("http://[fe80::1%25Wi-Fi%204]:8080/d.xml")
	if err != nil {
		t.Fatal(err)
	}
	if hostIsMalformed(win.Hostname()) {
		t.Fatalf("a Windows zone %q read as malformed", win.Hostname())
	}
	if _, err := url.Parse("http://[fe80::1%25en0%C2%A0]:8080/d.xml"); err == nil {
		t.Fatal("url.Parse kept a non-ASCII escape in a zone")
	}
	if decoded := "fe80::1%en0\u00a0"; !hostIsMalformed(decoded) {
		t.Fatalf("a decoded zone holding U+00A0 %q was kept", decoded)
	}
}
