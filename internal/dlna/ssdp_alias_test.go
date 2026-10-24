package dlna

import (
	"net/netip"
	"testing"
)

// TestStrictMulticastChoosesAnAdmittedAlias pins selection before slot acquisition.
func TestStrictMulticastChoosesAnAdmittedAlias(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		n := newFakeNet()
		sub := fakeSubsystem(t, n)
		a := SSDPEndpoint{Interface: en0, Local: netip.MustParsePrefix("192.168.1.20/24"), Location: srcOnEn0}
		b := SSDPEndpoint{Interface: en0, Local: netip.MustParsePrefix("10.2.0.20/24"), Location: "http://10.2.0.20:1234/openhome/description.xml"}
		eps := []SSDPEndpoint{a, b}
		if reverse {
			eps[0], eps[1] = eps[1], eps[0]
		}
		reg := strictReg(srcUDN, 1, eps...)
		reg.Admission = SSDPStrictAdmission{Source: func(s SSDPSearch) bool { return s.Endpoint.Local.Masked().Contains(s.Datagram.Source.Addr()) }}
		mustRegister(t, sub, reg)
		for _, ep := range []SSDPEndpoint{a, b} {
			src := netip.AddrPortFrom(ep.Local.Addr().Next(), 50000).String()
			got := sendTo(t, sub, n, "en0", search("upnp:rootdevice"), src, groupMeta(2))
			if len(got) != 1 || got[0].location != ep.Location {
				t.Fatalf("reverse=%v source=%s: got %v, want %s", reverse, src, got, ep.Location)
			}
		}
		got := sendTo(t, sub, n, "en0", search("upnp:rootdevice"), "172.16.0.1:50000", groupMeta(2))
		if len(got) != 0 {
			t.Fatalf("off-prefix reply: %v", got)
		}
	}
}
