package dlna

import (
	"bufio"
	"bytes"
	"context"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"
)

// rootDeviceSearch sends a unicast M-SEARCH for upnp:rootdevice with no MX,
// which an advertiser answers within defaultMSearchJitter with one datagram,
// to addr from a loopback socket closed at the test's end.
func rootDeviceSearch(t *testing.T, addr *net.UDPAddr) *net.UDPConn {
	t.Helper()
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("open a searcher: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	msg := "M-SEARCH * HTTP/1.1\r\nHOST: " + SSDPMulticastAddr + "\r\nMAN: \"ssdp:discover\"\r\nST: upnp:rootdevice\r\n\r\n"
	if _, err := c.WriteToUDP([]byte(msg), addr); err != nil {
		t.Fatalf("send the M-SEARCH: %v", err)
	}
	return c
}

// firstAnswer returns the LOCATION of the first answer the searcher c reads
// within wait, and "" when none arrives.
func firstAnswer(c *net.UDPConn, wait time.Duration) string {
	buf := make([]byte, 4096)
	_ = c.SetReadDeadline(time.Now().Add(wait))
	for {
		n, _, err := c.ReadFromUDP(buf)
		if err != nil {
			return ""
		}
		resp, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(buf[:n])), nil)
		if err != nil {
			continue
		}
		_ = resp.Body.Close()
		return resp.Header.Get("LOCATION")
	}
}

// startArmedMSearchListener runs adv's M-SEARCH read loop on a loopback
// socket of the test's own, armed as Start arms the real listener, until the
// test ends, and returns the socket's address. A unicast to it reaches the
// loop as a search sent to 127.0.0.1 would reach an advertiser's
// 0.0.0.0:1900.
func startArmedMSearchListener(t *testing.T, adv *SSDPAdvertiser) *net.UDPAddr {
	t.Helper()
	l, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("bind a loopback listener: %v", err)
	}
	if !adv.armListener(l) {
		_ = l.Close()
		t.Fatal("the listener could not be armed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	adv.wg.Add(1)
	go adv.runMSearchListener(ctx, l, true)
	t.Cleanup(func() {
		cancel()
		_ = l.Close()
		adv.wg.Wait()
	})
	return l.LocalAddr().(*net.UDPAddr)
}

// TestTheSSDPAdvertiserAnswersOnlyASearchSentToAnAddressItServes pins the
// SSDP half of backlog B48 through the advertiser's real read loop. Its
// listener is bound to 0.0.0.0:1900, so a unicast M-SEARCH sent to any
// address of the host reached it: measured from a tailnet peer, one sent to
// the host's tailnet address on 1900 was answered, with the LAN LOCATION.
// Each case judges one search sent to 127.0.0.1 as sent to one of the Mac's
// addresses (servedAddrs.judgeAs). A served one is answered. A refused one is
// logged, which says the loop has read and judged it, and is not answered in
// five times the longest delay an answer waits.
func TestTheSSDPAdvertiserAnswersOnlyASearchSentToAnAddressItServes(t *testing.T) {
	cases := []struct {
		name   string
		as     string
		served bool
	}{
		{"loopback", "", true},
		{"lan_ipv4", "192.168.1.20", true},
		{"zero_config_link", "169.254.7.8", true},
		{"tailnet_ipv4", "100.64.0.7", false},
		{"an_address_no_interface_holds", "192.168.9.9", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var judged judgedAs
			judged.set(tc.as)
			sink := newLogSink()
			const location = "http://192.168.1.20:7790/dlna/description.xml"
			adv := NewSSDPAdvertiser(SSDPConfig{
				UDN:         "uuid:b48b48b4-8b48-4b48-8b48-b48b48b48b48",
				Location:    location,
				ServerToken: "test",
				Logger:      sink.logger(),
				served:      macServedJudgedBy(&judged),
			})
			adv.log = sink.logger()
			search := rootDeviceSearch(t, startArmedMSearchListener(t, adv))
			if tc.served {
				if got := firstAnswer(search, 5*time.Second); got != location {
					t.Errorf("a search judged as sent to %q was answered %q, want an answer naming %s", tc.as, got, location)
				}
				return
			}
			if want := "refused an M-SEARCH to an address it does not serve"; !sink.waitFor(want, 5*time.Second) {
				t.Errorf("no refusal in the log:\n%s", sink.String())
			}
			if got := firstAnswer(search, 5*defaultMSearchJitter); got != "" {
				t.Errorf("a search judged as sent to %s was answered %q, want no answer", tc.as, got)
			}
		})
	}
}

// TestEveryAdvertiserAnswersByTheServersServedAddrs pins the wiring: Start
// hands each advertiser the servedAddrs its listener judges by, so the two
// halves of B48 cannot disagree, and makes one from the configuration when
// none was set.
func TestEveryAdvertiserAnswersByTheServersServedAddrs(t *testing.T) {
	s := startLoopbackDLNA(t, ServerConfig{}, nil)
	if s.served == nil {
		t.Fatal("Start left the server with no servedAddrs")
	}
	if len(s.ssdps) == 0 {
		t.Fatal("no advertiser started")
	}
	for i, adv := range s.ssdps {
		if adv.cfg.served != s.served {
			t.Errorf("advertiser %d answers by a servedAddrs of its own, not the server's", i)
		}
	}
	if _, ok := s.served.advertised[netipAddrOfURL(t, s.cfg.ServerURL)]; !ok {
		t.Errorf("the server's own address (%s) is not among what it advertises: %v", s.cfg.ServerURL, s.served.advertised)
	}
}

// netipAddrOfURL is the IP literal a URL's host names.
func netipAddrOfURL(t *testing.T, raw string) netip.Addr {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	a, err := netip.ParseAddr(u.Hostname())
	if err != nil {
		t.Fatalf("%q names no IP literal: %v", raw, err)
	}
	return a
}

// TestSearchedForAnswersTheGroupAndJudgesEveryOtherDestination pins what the
// read loop decides each M-SEARCH by: one sent to the SSDP group is answered
// without asking servedAddrs, since the group is no address of the host and
// a group datagram reaches the listener only on the interface it joined
// (B71). Every other destination is judged, and one whose destination was
// asked for and not reported is refused, since it could be a unicast to the
// tailnet.
func TestSearchedForAnswersTheGroupAndJudgesEveryOtherDestination(t *testing.T) {
	sink := newLogSink()
	adv := NewSSDPAdvertiser(SSDPConfig{
		UDN:         "uuid:b48b48b4-8b48-4b48-8b48-b48b48b48b48",
		Location:    "http://192.168.1.20:7790/dlna/description.xml",
		ServerToken: "test",
		served:      newTestHost(macWithTailscale()...).served(nil),
	})
	adv.log = sink.logger()
	src := &net.UDPAddr{IP: net.IPv4(192, 168, 1, 30), Port: 50000}
	cases := []struct {
		name  string
		dst   netip.Addr
		known bool
		want  bool
	}{
		{"the_group", ssdpGroupAddr, true, true},
		{"a_lan_address", netip.MustParseAddr("192.168.1.20"), true, true},
		{"loopback", netip.MustParseAddr("127.0.0.1"), true, true},
		{"the_tailnet_address", netip.MustParseAddr("100.64.0.7"), true, false},
		{"a_broadcast_address", netip.MustParseAddr("192.168.1.255"), true, false},
		{"no_destination_reported", netip.Addr{}, false, false},
		{"the_group_not_reported", ssdpGroupAddr, false, false},
	}
	for _, tc := range cases {
		if got := adv.searchedFor(tc.dst, tc.known, src); got != tc.want {
			t.Errorf("%s: searchedFor(%v, known %v) = %v, want %v", tc.name, tc.dst, tc.known, got, tc.want)
		}
	}
	log := sink.String()
	if strings.Contains(log, "address=239.255.255.250") {
		t.Errorf("a search sent to the group was judged and refused:\n%s", log)
	}
	for _, want := range []string{"address=100.64.0.7 interface=utun7 from=192.168.1.30", `address="(not reported)"`} {
		if !strings.Contains(log, want) {
			t.Errorf("the log has no refusal carrying %s:\n%s", want, log)
		}
	}
}
