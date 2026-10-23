package main

import (
	"errors"
	"io"
	"log/slog"
	"net"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/dlna"
)

// syntheticAddrs is an addrsOf over interfaces of a test's own, by name: no
// host is read. An interface it does not list fails, as an address listing
// can.
func syntheticAddrs(t *testing.T, byName map[string][]string) func(*net.Interface) ([]net.Addr, error) {
	t.Helper()
	parsed := map[string][]net.Addr{}
	for name, cidrs := range byName {
		for _, c := range cidrs {
			ip, n, err := net.ParseCIDR(c)
			if err != nil {
				t.Fatalf("parse %s: %v", c, err)
			}
			n.IP = ip
			parsed[name] = append(parsed[name], n)
		}
	}
	return func(ifi *net.Interface) ([]net.Addr, error) {
		addrs, ok := parsed[ifi.Name]
		if !ok {
			return nil, errors.New("no such interface in the test's host")
		}
		return addrs, nil
	}
}

// TestTheMediaServersEndpointsAreOnePerMemberOnItsFirstIPv4 pins the wildcard
// wiring's advertise set (B470 PR 5a1 keeps it as it was): one endpoint per
// member of the multicast set, in the set's order, on the member's first
// IPv4 address that is neither loopback nor unspecified. A member's aliases
// add no endpoint, a self-assigned 169.254 address counts when it comes
// first, and a member with no IPv4 address, or whose addresses cannot be
// read, is skipped.
func TestTheMediaServersEndpointsAreOnePerMemberOnItsFirstIPv4(t *testing.T) {
	members := []*net.Interface{
		{Index: 2, Name: "en0"},
		{Index: 3, Name: "en1"},
		{Index: 4, Name: "en2"},
		{Index: 5, Name: "bridge0"},
		{Index: 6, Name: "gone0"},
		{Index: 7, Name: "en3"},
	}
	addrsOf := syntheticAddrs(t, map[string][]string{
		"en0":     {"fe80::1/64", "192.168.1.20/24", "192.168.1.21/24", "10.1.1.1/8"},
		"en1":     {"169.254.7.8/16", "10.0.0.5/8"},
		"en2":     {"fe80::2/64", "2001:db8::2/64"},
		"bridge0": {"0.0.0.0/0", "127.0.0.9/8", "172.17.0.1/16"},
		"en3":     {"192.168.2.30/24"},
	})
	got := advertiseEndpointsOf(members, addrsOf, 7790, slog.New(slog.NewTextHandler(io.Discard, nil)))
	want := []dlna.AdvertiseEndpoint{
		{Interface: members[0], ServerURL: "http://192.168.1.20:7790"},
		{Interface: members[1], ServerURL: "http://169.254.7.8:7790"},
		{Interface: members[3], ServerURL: "http://172.17.0.1:7790"},
		{Interface: members[5], ServerURL: "http://192.168.2.30:7790"},
	}
	if len(got) != len(want) {
		t.Fatalf("%d endpoints, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].Interface != want[i].Interface || got[i].ServerURL != want[i].ServerURL {
			t.Errorf("endpoint %d is %s on %s, want %s on %s", i,
				got[i].ServerURL, got[i].Interface.Name, want[i].ServerURL, want[i].Interface.Name)
		}
	}
	if eps := gatherAdvertiseEndpoints(false, 7790, slog.New(slog.NewTextHandler(io.Discard, nil))); eps != nil {
		t.Errorf("a pinned bind gathered %d endpoints, want none (the single-endpoint fallback)", len(eps))
	}
}

// TestTheMediaServersServerURLKeepsAPinnedHostAsWritten pins deriveServerURL:
// a pinned bind's host goes into the URL exactly as configured, an IPv4
// literal, a bracketed IPv6 literal or a name; a wildcard bind takes the
// first gathered endpoint, and with none the picked interface's first IPv4.
func TestTheMediaServersServerURLKeepsAPinnedHostAsWritten(t *testing.T) {
	picked := &net.Interface{Index: 2, Name: "en0"}
	bare := &net.Interface{Index: 3, Name: "en1"}
	addrsOf := syntheticAddrs(t, map[string][]string{
		"en0": {"fe80::1/64", "192.168.1.20/24"},
		"en1": {"fe80::3/64"},
	})
	gathered := []dlna.AdvertiseEndpoint{{Interface: picked, ServerURL: "http://192.168.1.20:7790"}, {ServerURL: "http://10.0.0.5:7790"}}
	for _, tc := range []struct {
		name     string
		rawHost  string
		wildcard bool
		eps      []dlna.AdvertiseEndpoint
		iface    *net.Interface
		want     string
		wantErr  bool
	}{
		{"pinned_ipv4", "192.168.1.42", false, nil, picked, "http://192.168.1.42:7790", false},
		{"pinned_ipv6", "2001:db8::1", false, nil, picked, "http://[2001:db8::1]:7790", false},
		{"pinned_name", "nas.local", false, nil, picked, "http://nas.local:7790", false},
		{"wildcard_first_endpoint", "", true, gathered, picked, "http://192.168.1.20:7790", false},
		{"wildcard_no_endpoint", "", true, nil, picked, "http://192.168.1.20:7790", false},
		{"wildcard_nothing_usable", "", true, nil, bare, "", true},
	} {
		got, err := deriveServerURL(tc.rawHost, tc.wildcard, 7790, tc.eps, tc.iface, addrsOf)
		switch {
		case tc.wantErr && err == nil:
			t.Errorf("%s: %q, want an error", tc.name, got)
		case !tc.wantErr && (err != nil || got != tc.want):
			t.Errorf("%s: %q (%v), want %q", tc.name, got, err, tc.want)
		}
	}
}
