package dlna

import (
	"context"
	"net/netip"
	"slices"
	"testing"
	"time"
)

// TestAcceptsOnIsTheListenersServedSet pins AcceptsOn, which the player's
// Insert asks for the addresses the DLNA listener takes connections on (B470
// PR 5a4), each case with its own expected set, on the Mac B48 was measured
// on: a wildcard listener accepts on every address it serves (loopback, the
// LAN address, the zero-config link's) and on none it does not (the
// tailnet's two, the SLAAC address, an address the host does not hold), an
// IPv4 wildcard on no IPv6 address; a listener pinned to the LAN address
// accepts on that address alone; one pinned to the tailnet's accepts on
// nothing, since it refuses every connection there; and a server whose
// listener is not up accepts on nothing.
func TestAcceptsOnIsTheListenersServedSet(t *testing.T) {
	asked := []string{"127.0.0.1", "::1", "192.168.1.20", "169.254.7.8", "100.64.0.7", "fd7a:115c:a1e0::7",
		"2001:db8::20", "192.168.1.99", "10.0.0.5"}
	for _, tc := range []struct {
		name    string
		bound   string
		accepts []string
	}{
		{"an IPv4 wildcard", "0.0.0.0", []string{"127.0.0.1", "192.168.1.20", "169.254.7.8"}},
		{"a dual-stack wildcard", "::", []string{"127.0.0.1", "::1", "192.168.1.20", "169.254.7.8"}},
		{"pinned to the LAN address", "192.168.1.20", []string{"192.168.1.20"}},
		{"pinned to the tailnet's address", "100.64.0.7", nil},
		{"no listener", "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{served: newTestHost(macWithTailscale()...).served(nil)}
			if tc.bound != "" {
				s.accepting.Store(&acceptState{served: s.served, bound: netip.MustParseAddr(tc.bound)})
			}
			for _, a := range asked {
				if got, want := s.AcceptsOn(netip.MustParseAddr(a)), slices.Contains(tc.accepts, a); got != want {
					t.Errorf("AcceptsOn(%s) = %v, want %v", a, got, want)
				}
			}
			if s.AcceptsOn(netip.Addr{}) {
				t.Error("AcceptsOn of no address is true")
			}
		})
	}
}

// TestAcceptsOnFollowsTheServedSnapshot pins AcceptsOn's freshness, which is
// the listener's own: an address that leaves the host is still accepted on
// until the snapshot that held it is servedMaxAge old, and one that comes up
// is accepted on once a second has passed since the snapshot that refused it
// (servedMissRebuild).
func TestAcceptsOnFollowsTheServedSnapshot(t *testing.T) {
	clk := newTestClock()
	host := newTestHost(lan("en0", "192.168.1.20"), lan("en1", "192.168.2.20"))
	s := &Server{served: host.served(clk.now)}
	s.accepting.Store(&acceptState{served: s.served, bound: netip.IPv6Unspecified()})
	gone, added := netip.MustParseAddr("192.168.2.20"), netip.MustParseAddr("192.168.2.21")
	if !s.AcceptsOn(gone) {
		t.Fatal("an address the host holds is not accepted on")
	}
	host.set(lan("en0", "192.168.1.20"), lan("en1", "192.168.2.21"))
	clk.advance(servedMaxAge - time.Millisecond)
	if !s.AcceptsOn(gone) {
		t.Error("an address that left the host was dropped before its snapshot was servedMaxAge old")
	}
	clk.advance(time.Millisecond)
	if s.AcceptsOn(gone) {
		t.Error("an address that left the host is still accepted on once its snapshot was servedMaxAge old")
	}
	if !s.AcceptsOn(added) {
		t.Error("the address that came up is not accepted on after the rebuild")
	}
}

// TestAcceptsOnIsTheRunningListeners pins AcceptsOn's life through the real
// Start and Stop: a listener pinned to loopback accepts on 127.0.0.1 once
// Start has bound it, and on nothing once Stop has begun.
func TestAcceptsOnIsTheRunningListeners(t *testing.T) {
	s := startLoopbackDLNA(t, ServerConfig{}, nil)
	loopback := netip.MustParseAddr("127.0.0.1")
	if !s.AcceptsOn(loopback) {
		t.Fatal("a listener bound to 127.0.0.1 does not accept on it")
	}
	if s.AcceptsOn(netip.MustParseAddr("192.168.1.20")) {
		t.Error("a listener pinned to loopback accepts on another address")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if s.AcceptsOn(loopback) {
		t.Error("a stopped listener still accepts")
	}
}
