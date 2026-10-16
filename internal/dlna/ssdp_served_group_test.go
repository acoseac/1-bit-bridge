//go:build linux || darwin

package dlna

import (
	"context"
	"net"
	"testing"
	"time"

	"golang.org/x/net/ipv4"
)

// TestTheSSDPAdvertiserAnswersASearchSentToTheGroup pins, through the
// advertiser's real read loop, that a search sent to the SSDP group is still
// answered now that every other search is judged by the address it was sent
// to (backlog B48): every unicast is judged as the tailnet address here, and
// refused, and the group search is answered all the same. Without the
// group's exception every multicast M-SEARCH, how a control point finds a
// server, would be refused.
//
// The listener joins the SSDP group on the loopback interface at a port no
// device uses, and the search goes out of the loopback interface at TTL 0, so
// nothing leaves the host (the B38 rule). Windows is left out:
// ListenMulticastUDP turns IP_MULTICAST_LOOP off on the listener, which
// Winsock applies to what a socket receives (backlog B120), and
// TestSearchedForAnswersTheGroupAndJudgesEveryOtherDestination pins the rule
// there.
func TestTheSSDPAdvertiserAnswersASearchSentToTheGroup(t *testing.T) {
	lo := loopbackInterface(t)
	group := &net.UDPAddr{IP: net.IPv4(239, 255, 255, 250), Port: testMulticastGroup(t).Port}
	l, err := net.ListenMulticastUDP("udp4", lo, group)
	if err != nil {
		t.Skipf("join %v on %s: %v", group, lo.Name, err)
	}
	var judged judgedAs
	judged.set("100.64.0.7")
	sink := newLogSink()
	const location = "http://192.168.1.20:7790/dlna/description.xml"
	adv := NewSSDPAdvertiser(SSDPConfig{
		UDN:         "uuid:b48b48b4-8b48-4b48-8b48-b48b48b48b48",
		Location:    location,
		ServerToken: "test",
		served:      macServedJudgedBy(&judged),
	})
	adv.log = sink.logger()
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

	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero})
	if err != nil {
		t.Fatalf("open a searcher: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	p := ipv4.NewPacketConn(c)
	if err := p.SetMulticastInterface(lo); err != nil {
		t.Fatalf("pin the searcher to %s: %v", lo.Name, err)
	}
	if err := p.SetMulticastTTL(0); err != nil {
		t.Fatalf("set TTL 0: %v", err)
	}
	if err := p.SetMulticastLoopback(true); err != nil {
		t.Fatalf("turn loopback on: %v", err)
	}
	msg := "M-SEARCH * HTTP/1.1\r\nHOST: " + SSDPMulticastAddr + "\r\nMAN: \"ssdp:discover\"\r\nST: upnp:rootdevice\r\n\r\n"
	if _, err := c.WriteToUDP([]byte(msg), group); err != nil {
		t.Skipf("this host will not send a multicast datagram out of %s to loop it back: %v", lo.Name, err)
	}
	if got := firstAnswer(c, 5*time.Second); got != location {
		t.Errorf("a search sent to the group was answered %q, want an answer naming %s; the log:\n%s", got, location, sink.String())
	}
}
