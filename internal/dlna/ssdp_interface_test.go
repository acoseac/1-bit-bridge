package dlna

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"math/big"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/ipv4"
)

// The helpers here serve the tests of what an SSDP advertiser does on its
// own interface (backlog B71): the listener it hears M-SEARCHes on, and the
// socket it writes its NOTIFYs from. They are untagged so the tagged test
// files beside them can all reach them, whichever platform builds them.

// lanInterfaceForTest returns the interface the bridge would pick for its
// DLNA server (PickLANEligibleInterface), which must carry an IPv4 address
// and take a multicast pin; it skips where there is none. A test hands it
// only to a LISTENER and to datagrams sent at TTL 0: B38's rule keeps every
// advertiser a test starts on the loopback interface, and a TTL-0 datagram is
// looped back to this host's sockets and never transmitted. Joining a group
// there does send an IGMP report on that network.
func lanInterfaceForTest(t *testing.T) *net.Interface {
	t.Helper()
	iface, err := PickLANEligibleInterface(EligibilityOpts{})
	if err != nil {
		t.Skipf("no LAN-eligible interface on this host: %v", err)
	}
	if !hasIPv4(iface) {
		t.Skipf("the LAN-eligible interface %s carries no IPv4 address", iface.Name)
	}
	skipUnlessMulticastPins(t, iface)
	return iface
}

// hasIPv4 reports whether iface carries an IPv4 address.
func hasIPv4(iface *net.Interface) bool {
	addrs, err := iface.Addrs()
	if err != nil {
		return false
	}
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil {
			return true
		}
	}
	return false
}

// testMulticastGroup returns a group and port no SSDP device uses, drawn at
// random (a group in 239.255.71.0/24, a port in 20000 to 32767, below every
// platform's ephemeral range), so two runs of a test, or a real device on
// the LAN, never meet in it.
func testMulticastGroup(t *testing.T) *net.UDPAddr {
	t.Helper()
	host, err := rand.Int(rand.Reader, big.NewInt(254))
	if err != nil {
		t.Fatal(err)
	}
	port, err := rand.Int(rand.Reader, big.NewInt(12768))
	if err != nil {
		t.Fatal(err)
	}
	return &net.UDPAddr{IP: net.IPv4(239, 255, 71, byte(host.Int64()+1)), Port: 20000 + int(port.Int64())}
}

// testNonce returns a random string a test puts in what it sends, so what it
// reads back is its own and nothing else on the host counts.
func testNonce(t *testing.T) string {
	t.Helper()
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// sendHostLocal writes payload to group out of iface at TTL 0 with loopback
// on, from a socket pinned to iface before it sends: the kernel loops a copy
// back to this host's sockets as a datagram that ARRIVED on iface, and
// transmits nothing (a multicast with TTL 0 must not go beyond the host, on
// Linux, macOS and the BSDs alike). A host that refuses the send skips the
// test: the send is how the test delivers a datagram, not what it measures,
// and GitHub's macOS runner answers `sendto: no route to host` for en0
// (measured on its CI leg, 2026-09-29), where the dev Mac sends.
func sendHostLocal(t *testing.T, iface *net.Interface, group *net.UDPAddr, payload string) {
	t.Helper()
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero})
	if err != nil {
		t.Fatalf("open a sender: %v", err)
	}
	defer c.Close()
	p := ipv4.NewPacketConn(c)
	if err := p.SetMulticastInterface(iface); err != nil {
		t.Fatalf("pin the sender to %s: %v", iface.Name, err)
	}
	if err := p.SetMulticastTTL(0); err != nil {
		t.Fatalf("set TTL 0: %v", err)
	}
	if err := p.SetMulticastLoopback(true); err != nil {
		t.Fatalf("turn loopback on: %v", err)
	}
	if _, err := c.WriteToUDP([]byte(payload), group); err != nil {
		t.Skipf("this host will not send a multicast datagram out of %s to loop it back, "+
			"so the test cannot deliver one to its listeners: %v", iface.Name, err)
	}
}

// heardCarrying reads l until it has been quiet for half a second (a second
// at most), and returns every datagram that carries nonce, sorted.
func heardCarrying(l *net.UDPConn, nonce string) []string {
	var got []string
	buf := make([]byte, 2048)
	end := time.Now().Add(time.Second)
	for time.Now().Before(end) {
		_ = l.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		n, _, err := l.ReadFromUDP(buf)
		if err != nil {
			break
		}
		if s := string(buf[:n]); strings.Contains(s, nonce) {
			got = append(got, s)
		}
	}
	slices.Sort(got)
	return got
}

// TestAStartedAdvertiserWritesItsNotifiesFromAnUnconnectedSocket pins the
// sender half of backlog B71, the wiring of it that every platform's CI can
// see. The advertiser opened its NOTIFY sender with net.DialUDP to the SSDP
// group and pinned it to its interface afterwards, and a connect fixes the
// socket's source address along the group's route at that moment: measured
// on a multi-homed Linux host, the NOTIFYs pinned to the second interface
// went out from the FIRST interface's address, and a renderer with no route
// back to that subnet dropped every one; on a host with no route to the
// group at all (no default route) the connect failed and DLNA did not start.
// An unconnected socket takes its source address from the pinned interface
// on every send, and needs no route to open. The behaviour itself is pinned
// where each platform lets a test see it: the loopback source test on macOS
// and Windows, and the network-namespace test on Linux.
func TestAStartedAdvertiserWritesItsNotifiesFromAnUnconnectedSocket(t *testing.T) {
	a := NewSSDPAdvertiser(SSDPConfig{
		UDN:         "uuid:f1b3a5c2-8e7d-4f3b-9c1a-0d2e3f4a5b6c",
		Location:    "http://127.0.0.1:7790/dlna/description.xml",
		ServerToken: "test",
		Interface:   loopbackInterface(t),
	})
	if err := a.Start(context.Background()); err != nil {
		t.Skipf("multicast unavailable on the loopback interface: %v", err)
	}
	t.Cleanup(a.Stop)
	a.mu.Lock()
	sender := a.sender
	a.mu.Unlock()
	if ra := sender.RemoteAddr(); ra != nil {
		t.Errorf("the NOTIFY sender is connected to %v: its source address was fixed when it connected, "+
			"along the group's route, whatever interface it is pinned to", ra)
	}
}
