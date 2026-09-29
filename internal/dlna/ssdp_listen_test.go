//go:build linux || darwin

package dlna

import (
	"context"
	"slices"
	"testing"
)

// TestAnSSDPListenerHearsOnlyTheInterfaceItJoined pins backlog B71 at the
// listener. The DLNA server runs one advertiser per LAN interface, each
// announcing its own interface's LOCATION, so each advertiser must hear only
// the M-SEARCHes that arrive on its own interface. net.ListenMulticastUDP
// binds the listener to the WILDCARD address (net's listenDatagram rewrites
// the group to 0.0.0.0 before the bind), and Linux's default
// IP_MULTICAST_ALL = 1 then hands it the group's datagrams from every
// interface where any socket on the host joined the group. So on a
// multi-homed Linux bridge every advertiser also answered the M-SEARCHes of
// the others, with its own LOCATION: measured with the real binary in three
// network namespaces, every M-SEARCH from either subnet got two answers, one
// naming the other subnet.
//
// Two listeners join one group through listenSSDP, the advertiser's own
// constructor: one on the loopback interface, one on the LAN interface the
// bridge would pick. A datagram goes out of each interface at TTL 0, so the
// kernel loops it back as one that arrived there and puts nothing on any
// network (lanInterfaceForTest says what the join itself does). Each
// listener must hear its own interface's datagram and not the other's.
//
// macOS delivers per joined interface natively, so there it pins that the
// listener stays so. Windows is left out: it applies IP_MULTICAST_LOOP to
// what a socket RECEIVES, and ListenMulticastUDP turns that off on the
// listener, so nothing this host sends can reach it (with the flag turned
// back on it delivers per interface too, measured by hand on 2026-09-29).
func TestAnSSDPListenerHearsOnlyTheInterfaceItJoined(t *testing.T) {
	lo := loopbackInterface(t)
	lan := lanInterfaceForTest(t)
	group := testMulticastGroup(t)
	nonce := testNonce(t)

	onLo, err := listenSSDP(context.Background(), lo, group, ssdpLogger)
	if err != nil {
		t.Skipf("cannot join %s on %s: %v", group, lo.Name, err)
	}
	t.Cleanup(func() { _ = onLo.Close() })
	onLAN, err := listenSSDP(context.Background(), lan, group, ssdpLogger)
	if err != nil {
		t.Skipf("cannot join %s on %s: %v", group, lan.Name, err)
	}
	t.Cleanup(func() { _ = onLAN.Close() })

	sendHostLocal(t, lo, group, "loopback "+nonce)
	sendHostLocal(t, lan, group, "lan "+nonce)

	if got, want := heardCarrying(onLo, nonce), []string{"loopback " + nonce}; !slices.Equal(got, want) {
		t.Errorf("the listener joined on %s heard %q, want only %q, the datagram that arrived on its own interface",
			lo.Name, got, want)
	}
	if got, want := heardCarrying(onLAN, nonce), []string{"lan " + nonce}; !slices.Equal(got, want) {
		t.Errorf("the listener joined on %s heard %q, want only %q, the datagram that arrived on its own interface",
			lan.Name, got, want)
	}
}
