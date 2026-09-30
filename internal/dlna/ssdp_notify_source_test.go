//go:build darwin || windows

package dlna

import (
	"bufio"
	"bytes"
	"context"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/ipv4"
)

// TestAnAdvertiserOnLoopbackNotifiesFromALoopbackAddress pins the sender half
// of backlog B71 where the platform lets a test see it. The advertiser's
// NOTIFYs must leave from the interface they are pinned to, with that
// interface's address. Its sender was net.DialUDP to the group, pinned
// afterwards, and the connect fixed the source address along the group's
// route: measured on macOS, an advertiser pinned to lo0 announced from en0's
// address (the group's route goes through en0), and on Windows a socket
// connected that way and pinned to the loopback interface sent its datagram
// out of Ethernet, onto the LAN, whenever nothing on the host had joined the
// group on loopback.
//
// A listener of the test's own joins the SSDP group on the loopback
// interface before the advertiser starts, and every NOTIFY carrying this
// advertiser's LOCATION must come from a loopback address. Linux is left
// out: 127.0.0.1 is host-scoped there, so the kernel gives a multicast
// pinned to lo another interface's address whichever way the socket was
// opened (measured); the network-namespace test covers Linux. On Windows
// main's form happened to pass here too, because the advertiser's own
// listener joins on loopback before its sender connects; this pins the
// fixed form there.
func TestAnAdvertiserOnLoopbackNotifiesFromALoopbackAddress(t *testing.T) {
	lo := loopbackInterface(t)
	group, err := net.ResolveUDPAddr("udp4", SSDPMulticastAddr)
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.ListenMulticastUDP("udp4", lo, group)
	if err != nil {
		t.Skipf("cannot join the SSDP group on %s: %v", lo.Name, err)
	}
	t.Cleanup(func() { _ = l.Close() })
	// Windows applies IP_MULTICAST_LOOP to what a socket receives, and
	// ListenMulticastUDP turned it off; elsewhere it governs only sends.
	if err := ipv4.NewPacketConn(l).SetMulticastLoopback(true); err != nil {
		t.Fatalf("turn loopback on: %v", err)
	}

	location := "http://127.0.0.1:7790/" + testNonce(t) + "/description.xml"
	a := NewSSDPAdvertiser(SSDPConfig{
		UDN:         "uuid:f1b3a5c2-8e7d-4f3b-9c1a-0d2e3f4a5b6c",
		Location:    location,
		ServerToken: "test",
		Interface:   lo,
	})
	if err := a.Start(context.Background()); err != nil {
		t.Skipf("multicast unavailable on the loopback interface: %v", err)
	}
	t.Cleanup(a.Stop)

	var sources []string
	buf := make([]byte, 4096)
	end := time.Now().Add(2 * time.Second)
	for time.Now().Before(end) && len(sources) < len(NotifyTargetsFor("uuid:x")) {
		_ = l.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		n, src, err := l.ReadFromUDP(buf)
		if err != nil {
			break
		}
		if !bytes.HasPrefix(buf[:n], []byte("NOTIFY ")) {
			continue
		}
		req, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(buf[:n])))
		if err != nil || req.Header.Get("LOCATION") != location {
			continue
		}
		if !src.IP.IsLoopback() {
			t.Errorf("a NOTIFY pinned to %s came from %s, not a loopback address", lo.Name, src.IP)
		}
		sources = append(sources, src.IP.String())
	}
	if len(sources) == 0 {
		t.Fatalf("heard none of the advertiser's NOTIFYs on %s", lo.Name)
	}
	t.Logf("NOTIFY sources: %s", strings.Join(sources, ", "))
}
