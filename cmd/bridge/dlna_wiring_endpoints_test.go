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

// discardNote is a publicLeftOutNote whose line goes nowhere.
func discardNote() *publicLeftOutNote {
	return &publicLeftOutNote{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

// TestTheMediaServersEndpointsAreOnePerMemberOnItsFirstIPv4 pins the wildcard
// wiring's advertise set (B470 PR 5a1 keeps it as it was, and B687 keeps it
// to the private and link-local classes): one endpoint per member of the
// multicast set, in the set's order, on the member's first IPv4 address that
// is private or link-local. A member's aliases add no endpoint, a
// self-assigned 169.254 address counts when it comes first, a public or
// 100.64/10 address is passed over wherever the member lists it (and said),
// and a member with no private or link-local IPv4 address, or whose
// addresses cannot be read, is skipped.
func TestTheMediaServersEndpointsAreOnePerMemberOnItsFirstIPv4(t *testing.T) {
	members := []*net.Interface{
		{Index: 2, Name: "en0"},
		{Index: 3, Name: "en1"},
		{Index: 4, Name: "en2"},
		{Index: 5, Name: "bridge0"},
		{Index: 6, Name: "gone0"},
		{Index: 7, Name: "en3"},
		{Index: 8, Name: "en4"},
		{Index: 9, Name: "en5"},
	}
	addrsOf := syntheticAddrs(t, map[string][]string{
		"en0":     {"fe80::1/64", "192.168.1.20/24", "192.168.1.21/24", "10.1.1.1/8"},
		"en1":     {"169.254.7.8/16", "10.0.0.5/8"},
		"en2":     {"fe80::2/64", "2001:db8::2/64"},
		"bridge0": {"0.0.0.0/0", "127.0.0.9/8", "172.17.0.1/16"},
		"en3":     {"192.168.2.30/24"},
		"en4":     {"93.184.216.34/24", "100.64.0.9/10", "192.168.4.4/24"},
		"en5":     {"203.0.113.5/24", "fd12:3456::5/64"},
	})
	note := discardNote()
	got := advertiseEndpointsOf(members, addrsOf, 7790, note, slog.New(slog.NewTextHandler(io.Discard, nil)))
	want := []dlna.AdvertiseEndpoint{
		{Interface: members[0], ServerURL: "http://192.168.1.20:7790"},
		{Interface: members[1], ServerURL: "http://169.254.7.8:7790"},
		{Interface: members[3], ServerURL: "http://172.17.0.1:7790"},
		{Interface: members[5], ServerURL: "http://192.168.2.30:7790"},
		{Interface: members[6], ServerURL: "http://192.168.4.4:7790"},
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
	if !note.said {
		t.Error("the public addresses en4 and en5 carry were left out without a word")
	}
	ifaces := make([]net.Interface, len(members))
	for i, m := range members {
		ifaces[i] = *m
		ifaces[i].Flags = net.FlagUp | net.FlagMulticast
	}
	if eps := gatherAdvertiseEndpoints(false, ifaces, addrsOf, 7790, discardNote(), slog.New(slog.NewTextHandler(io.Discard, nil))); eps != nil {
		t.Errorf("a pinned bind gathered %d endpoints, want none (the single-endpoint fallback)", len(eps))
	}
}

// TestTheMediaServersServerURLKeepsAPinnedHostAsWritten pins deriveServerURL:
// a pinned bind's host goes into the URL exactly as configured, an IPv4
// literal, a bracketed IPv6 literal or a name; a wildcard bind takes the
// first gathered endpoint, and with none the picked interface's first private
// or link-local IPv4, never its public one (B687).
func TestTheMediaServersServerURLKeepsAPinnedHostAsWritten(t *testing.T) {
	picked := &net.Interface{Index: 2, Name: "en0"}
	bare := &net.Interface{Index: 3, Name: "en1"}
	publicFirst := &net.Interface{Index: 4, Name: "en2"}
	publicOnly := &net.Interface{Index: 5, Name: "en3"}
	addrsOf := syntheticAddrs(t, map[string][]string{
		"en0": {"fe80::1/64", "192.168.1.20/24"},
		"en1": {"fe80::3/64"},
		"en2": {"203.0.113.5/24", "10.0.0.7/8"},
		"en3": {"203.0.113.6/24", "fd12:3456::6/64"},
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
		leftOut  bool
	}{
		{"pinned_ipv4", "192.168.1.42", false, nil, picked, "http://192.168.1.42:7790", false, false},
		{"pinned_ipv6", "2001:db8::1", false, nil, picked, "http://[2001:db8::1]:7790", false, false},
		{"pinned_name", "nas.local", false, nil, picked, "http://nas.local:7790", false, false},
		{"pinned_public_ipv4", "203.0.113.5", false, nil, publicFirst, "http://203.0.113.5:7790", false, false},
		{"wildcard_first_endpoint", "", true, gathered, picked, "http://192.168.1.20:7790", false, false},
		{"wildcard_no_endpoint", "", true, nil, picked, "http://192.168.1.20:7790", false, false},
		{"wildcard_nothing_usable", "", true, nil, bare, "", true, false},
		{"wildcard_public_listed_first", "", true, nil, publicFirst, "http://10.0.0.7:7790", false, true},
		{"wildcard_public_alone", "", true, nil, publicOnly, "", true, true},
	} {
		note := discardNote()
		got, err := deriveServerURL(tc.rawHost, tc.wildcard, 7790, tc.eps, tc.iface, addrsOf, note)
		switch {
		case tc.wantErr && err == nil:
			t.Errorf("%s: %q, want an error", tc.name, got)
		case !tc.wantErr && (err != nil || got != tc.want):
			t.Errorf("%s: %q (%v), want %q", tc.name, got, err, tc.want)
		}
		if note.said != tc.leftOut {
			t.Errorf("%s: a public address left out said %v, want %v", tc.name, note.said, tc.leftOut)
		}
	}
}

// TestADLNAWildcardIsAWildcardHoweverItIsSpelled pins isWildcardHost: no host
// and every spelling of the unspecified address bind every address, and
// name no address the operator meant (B687); an address, a name and what
// does not parse are a pin.
func TestADLNAWildcardIsAWildcardHoweverItIsSpelled(t *testing.T) {
	for _, host := range []string{"", "0.0.0.0", "::", "::0", "0:0:0:0:0:0:0:0", "::ffff:0.0.0.0", "0000::"} {
		if !isWildcardHost(host) {
			t.Errorf("%q is not a wildcard, want one", host)
		}
	}
	for _, host := range []string{"192.168.1.20", "203.0.113.5", "::1", "127.0.0.1", "2001:db8::1", "nas.local", "0", "0.0.0.1"} {
		if isWildcardHost(host) {
			t.Errorf("%q is a wildcard, want a pin", host)
		}
	}
}
