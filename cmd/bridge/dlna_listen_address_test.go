package main

import (
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// The two lines B687 adds, as the MediaServer's wiring and Start write them.
const (
	publicLeftOutLine = "DLNA leaves out an IPv4 address of a LAN interface that is neither private nor link-local: " +
		"it announces and serves only this host's private, link-local and loopback addresses"
	publicPinLine = "dlna.listenAddress names a public address: DLNA serves it, and anyone who can reach that address " +
		"can browse and stream the library without signing in"
)

// linesCarrying returns every string a handler could write for each record
// logged with the message msg that holds one of values.
func (r *mediaServerRig) linesCarrying(msg string, values ...string) []string {
	var out []string
	for _, rec := range r.log.Records() {
		if rec.Message != msg {
			continue
		}
		for _, s := range loggingtest.Strings(rec) {
			for _, v := range values {
				if strings.Contains(s, v) {
					out = append(out, s)
				}
			}
		}
	}
	return out
}

// TestTheMediaServerServesThePublicAddressItsListenAddressNames is B687's
// opt-in through the real wiring: a public address dlna.listenAddress names
// as an IP literal is served as named, and that address alone, on a host
// whose only network is public (its interface carries no private address, so
// it is not LAN-eligible, and the wildcard refuses to start there), beside a
// private address on its interface, and on an interface of its own. Its one
// advertiser runs on the interface that holds it, and Start says once,
// without the address, what serving it means. Until B687 the public-only
// host did not start at all (no LAN-eligible interface), and the address on
// an interface of its own was announced and refused every connection.
func TestTheMediaServerServesThePublicAddressItsListenAddressNames(t *testing.T) {
	const pin, lanPhone, outsider = "203.0.113.5", "192.168.1.50", "198.51.100.9"
	for _, tc := range []struct {
		name  string
		ifs   []hostIface
		iface string // the interface that holds the pin, where SSDP runs
		index int
	}{
		{"a_public_only_host", []hostIface{
			{"eth0", mediaServerLANFlags, []string{pin + "/24", "fe80::5/64"}},
		}, "eth0", 2},
		{"beside_a_private_address", []hostIface{
			{"eth0", mediaServerLANFlags, []string{"192.168.1.20/24", pin + "/24"}},
		}, "eth0", 2},
		{"on_an_interface_of_its_own", []hostIface{
			{"eth0", mediaServerLANFlags, []string{"192.168.1.20/24"}},
			{"eth1", mediaServerLANFlags, []string{pin + "/24"}},
		}, "eth1", 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := startMediaServerOn(t, pin+":7790", []string{pin}, tc.ifs...)
			if !rig.enabled {
				t.Fatalf("DLNA did not start on the public address dlna.listenAddress names:\n%s", strings.Join(rig.log.All(), "\n"))
			}
			srv := rig.lc.server
			for _, w := range rig.aliveNotifies(t, 1) {
				if got, want := ssdpHeader(w.Pkt, "LOCATION"), "http://"+pin+":7790/dlna/description.xml"; got != want || w.Iface != tc.iface {
					t.Errorf("an alive NOTIFY on %s announces %s, want %s on %s", w.Iface, got, want, tc.iface)
				}
			}
			if !srv.AcceptsOn(netip.MustParseAddr(pin)) {
				t.Errorf("the listener does not take a connection on the address it is pinned to")
			}
			if srv.AcceptsOn(netip.MustParseAddr("192.168.1.20")) {
				t.Errorf("a listener pinned to %s takes a connection on another address", pin)
			}
			for _, from := range []string{lanPhone, outsider} {
				status, res := rig.browse(t, from, pin, pin+":7790")
				if want := "http://" + pin + ":7790/dlna/file/" + rig.id; status != http.StatusOK || !slices.Contains(res, want) {
					t.Errorf("Browse from %s on the pinned address: status %d, <res> %v, want 200 and %s", from, status, res, want)
				}
			}
			if status, got := rig.request(t, outsider, pin, pin+":7790", http.MethodGet, "/dlna/file/"+rig.id, "", ""); status != http.StatusOK || got != string(rig.body) {
				t.Errorf("GET of the track on the pinned address: status %d, %d bytes, want 200 and its %d bytes", status, len(got), len(rig.body))
			}
			rig.search(t, tc.iface, tc.index, outsider+":50000", pin)
			if got := ssdpHeader(rig.awaitReplyTo(t, outsider+":50000").Pkt, "LOCATION"); got != "http://"+pin+":7790/dlna/description.xml" {
				t.Errorf("the answer to a search of the pinned address names %s", got)
			}
			if n := len(rig.log.Lines(publicPinLine)); n != 1 {
				t.Errorf("Start said %d times what serving the public address means, want once", n)
			}
			if got := rig.linesCarrying(publicPinLine, pin); len(got) != 0 {
				t.Errorf("the line names the address: %q", got)
			}
			if n := len(rig.log.Lines(publicLeftOutLine)); n != 0 {
				t.Errorf("a pinned listener said %d times that it left a public address out", n)
			}
		})
	}
}

// TestTheMediaServerTakesNoPublicAddressItsListenAddressDoesNotName is the
// opt-in's edges: a host name is no opt-in whatever it resolves to (here the
// listener stands for the public address the name would resolve to), and
// neither is an address in 100.64/10, a tailnet's or an ISP's: each starts,
// warns that DLNA serves no one, and closes a connection on that address
// unread. A pin to a 100.64/10 address on a member was served until B687, as
// an address the server advertised.
func TestTheMediaServerTakesNoPublicAddressItsListenAddressDoesNotName(t *testing.T) {
	const pinnedOffTheLAN = "dlna.listenAddress pins the listener to an address DLNA does not serve: with it, DLNA serves no one"
	for _, tc := range []struct {
		name, listen, at string
		cidrs            []string
	}{
		{"a_name", "music.example:7790", "203.0.113.5", []string{"192.168.1.20/24", "203.0.113.5/24"}},
		{"shared_address_space_on_a_member", "100.70.0.1:7790", "100.70.0.1", []string{"192.168.1.20/24", "100.70.0.1/10"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := startMediaServerOn(t, tc.listen, []string{tc.at}, hostIface{"eth0", mediaServerLANFlags, tc.cidrs})
			if !rig.enabled {
				t.Fatalf("DLNA did not start: %s", strings.Join(rig.log.All(), "\n"))
			}
			if rig.lc.server.AcceptsOn(netip.MustParseAddr(tc.at)) {
				t.Errorf("the listener takes a connection on %s", tc.at)
			}
			if n := rig.bytesBack(t, "198.51.100.9", tc.at, tc.at+":7790"); n != 0 {
				t.Errorf("a connection on %s got %d bytes back, want none", tc.at, n)
			}
			if n := len(rig.log.Lines(pinnedOffTheLAN)); n != 1 {
				t.Errorf("Start warned %d times that DLNA serves no one, want once", n)
			}
			if n := len(rig.log.Lines(publicPinLine)); n != 0 {
				t.Errorf("Start said %d times that it serves a public address", n)
			}
		})
	}
}

// TestTheMediaServerRefusesAPinNoInterfaceHolds is the opt-in's last edge: a
// public address no interface of this host holds has nowhere to advertise
// from, and DLNA does not start (the bind would fail too).
func TestTheMediaServerRefusesAPinNoInterfaceHolds(t *testing.T) {
	rig := startMediaServerOn(t, "203.0.113.77:7790", []string{"203.0.113.77"},
		hostIface{"eth0", mediaServerLANFlags, []string{"192.168.1.20/24", "203.0.113.5/24"}})
	if rig.enabled {
		t.Error("DLNA started on a public address no interface holds")
	}
	const why = "DLNA disabled — no interface of this host holds the address dlna.listenAddress names"
	if n := len(rig.log.Lines(why)); n != 1 {
		t.Errorf("%d lines say why DLNA did not start, want one:\n%s", n, strings.Join(rig.log.All(), "\n"))
	}
}

// TestTheMediaServerServesAPrivateOnlyHostAsBefore pins that a host with
// nothing public is served as it was: each member announced on its private
// IPv4 address, the library served on each, and no line about an address
// left out.
func TestTheMediaServerServesAPrivateOnlyHostAsBefore(t *testing.T) {
	rig := startMediaServerOn(t, ":7790", []string{"192.168.1.20", "10.0.0.5"},
		hostIface{"eth0", mediaServerLANFlags, []string{"192.168.1.20/24", "fe80::20/64"}},
		hostIface{"eth1", mediaServerLANFlags, []string{"10.0.0.5/8"}})
	if !rig.enabled {
		t.Fatalf("DLNA did not start: %s", strings.Join(rig.log.All(), "\n"))
	}
	var got []string
	for _, w := range rig.aliveNotifies(t, 2) {
		got = append(got, w.Iface+" "+ssdpHeader(w.Pkt, "LOCATION"))
	}
	for _, want := range []string{"eth0 http://192.168.1.20:7790/dlna/description.xml", "eth1 http://10.0.0.5:7790/dlna/description.xml"} {
		if !slices.Contains(got, want) {
			t.Errorf("no alive NOTIFY %s among %v", want, got)
		}
	}
	for _, at := range []string{"192.168.1.20", "10.0.0.5"} {
		status, res := rig.browse(t, "192.168.1.50", at, at+":7790")
		if want := "http://" + at + ":7790/dlna/file/" + rig.id; status != http.StatusOK || !slices.Contains(res, want) {
			t.Errorf("Browse on %s: status %d, <res> %v, want 200 and %s", at, status, res, want)
		}
	}
	if n := len(rig.log.Lines(publicLeftOutLine)); n != 0 {
		t.Errorf("a private-only host said %d times that it left a public address out", n)
	}
	if got := rig.log.Failures(); len(got) != 0 {
		t.Errorf("warnings on a private-only host:\n%s", strings.Join(got, "\n"))
	}
}

// TestTheMediaServerServesAULAAndNotAGlobalIPv6Address is B687 in IPv6: on a
// dual-stack listener, a member's ULA is served and named in Host, and its
// global (SLAAC) address is not, as a member's public IPv4 address is not,
// though no IPv6 address is ever announced (SSDP is IPv4). A global IPv6
// address dlna.listenAddress names is the opt-in, as a public IPv4 one is.
func TestTheMediaServerServesAULAAndNotAGlobalIPv6Address(t *testing.T) {
	const ula, global, lanPhone = "fd12:3456::20", "2001:db8::20", "fd12:3456::50"
	cidrs := []string{global + "/64", "192.168.1.20/24", ula + "/64", "fe80::20/64"}
	t.Run("a_wildcard", func(t *testing.T) {
		rig := startMediaServerOn(t, "[::]:7790", []string{ula, global}, hostIface{"eth0", mediaServerLANFlags, cidrs})
		if !rig.enabled {
			t.Fatalf("DLNA did not start: %s", strings.Join(rig.log.All(), "\n"))
		}
		srv := rig.lc.server
		if !srv.AcceptsOn(netip.MustParseAddr(ula)) || srv.AcceptsOn(netip.MustParseAddr(global)) {
			t.Errorf("the listener takes the ULA %v and the global address %v, want true and false",
				srv.AcceptsOn(netip.MustParseAddr(ula)), srv.AcceptsOn(netip.MustParseAddr(global)))
		}
		status, res := rig.browse(t, lanPhone, ula, "["+ula+"]:7790")
		if want := "http://[" + ula + "]:7790/dlna/file/" + rig.id; status != http.StatusOK || !slices.Contains(res, want) {
			t.Errorf("Browse on the ULA: status %d, <res> %v, want 200 and %s", status, res, want)
		}
		if status, _ := rig.browse(t, lanPhone, ula, "["+global+"]:7790"); status != http.StatusMisdirectedRequest {
			t.Errorf("Browse over the ULA naming the global address: status %d, want 421", status)
		}
		if n := rig.bytesBack(t, "2001:db8:ffff::9", global, "["+ula+"]:7790"); n != 0 {
			t.Errorf("a connection on the global address got %d bytes back, want none", n)
		}
		for _, location := range rig.aliveLocations(t) {
			if location != "http://192.168.1.20:7790/dlna/description.xml" {
				t.Errorf("an alive NOTIFY announces %s, want the private IPv4 address", location)
			}
		}
	})
	t.Run("the_global_address_pinned", func(t *testing.T) {
		rig := startMediaServerOn(t, "["+global+"]:7790", []string{global}, hostIface{"eth0", mediaServerLANFlags, cidrs})
		if !rig.enabled {
			t.Fatalf("DLNA did not start: %s", strings.Join(rig.log.All(), "\n"))
		}
		status, res := rig.browse(t, "2001:db8:ffff::9", global, "["+global+"]:7790")
		if want := "http://[" + global + "]:7790/dlna/file/" + rig.id; status != http.StatusOK || !slices.Contains(res, want) {
			t.Errorf("Browse on the pinned global address: status %d, <res> %v, want 200 and %s", status, res, want)
		}
		if n := len(rig.log.Lines(publicPinLine)); n != 1 {
			t.Errorf("Start said %d times what serving the public address means, want once", n)
		}
	})
}

// TestTheMediaServerSaysOnceAndWithoutTheAddressThatItLeftOutAPublicAddress
// pins the line B687 adds: the advertise set and the fallback ServerURL each
// read every address of what they look at, two members here each carrying
// a public IPv4 address (and the fallback a third time where no member
// yields a LOCATION), and the line is written once per start, naming none of
// the addresses, whether DLNA then starts or not.
func TestTheMediaServerSaysOnceAndWithoutTheAddressThatItLeftOutAPublicAddress(t *testing.T) {
	for _, tc := range []struct {
		name    string
		ifs     []hostIface
		at      []string
		starts  bool
		public  []string
		private []string
	}{
		{"members_with_a_private_address", []hostIface{
			{"eth0", mediaServerLANFlags, []string{"192.168.1.20/24", "93.184.216.34/24"}},
			{"eth1", mediaServerLANFlags, []string{"198.51.100.7/24", "10.0.0.5/8", "100.64.0.9/10"}},
		}, []string{"192.168.1.20", "10.0.0.5"}, true, []string{"93.184.216.34", "198.51.100.7", "100.64.0.9"}, []string{"192.168.1.20", "10.0.0.5"}},
		{"members_with_no_private_ipv4_address", []hostIface{
			{"eth0", mediaServerLANFlags, []string{"203.0.113.5/24", "fd12:3456::5/64"}},
			{"eth1", mediaServerLANFlags, []string{"198.51.100.7/24", "fd34:5678::7/64"}},
		}, []string{"203.0.113.5"}, false, []string{"203.0.113.5", "198.51.100.7"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := startMediaServerOn(t, "0.0.0.0:7790", tc.at, tc.ifs...)
			if rig.enabled != tc.starts {
				t.Fatalf("DLNA started: %v, want %v\n%s", rig.enabled, tc.starts, strings.Join(rig.log.All(), "\n"))
			}
			if n := len(rig.log.Lines(publicLeftOutLine)); n != 1 {
				t.Errorf("said %d times that a public address was left out, want once:\n%s", n, strings.Join(rig.log.All(), "\n"))
			}
			if got := rig.linesCarrying(publicLeftOutLine, append(tc.public, tc.private...)...); len(got) != 0 {
				t.Errorf("the line names an address: %q", got)
			}
		})
	}
}

// TestTheMediaServerTakesEverySpellingOfTheWildcardAsOne pins that writing
// the wildcard out, however it is spelled, names no address (B687): the
// mixed host is announced on its private address and its public one is not
// served, with each spelling. "::0" and "::ffff:0.0.0.0" read as a pin until
// B687, announced as "http://[::0]:7790".
func TestTheMediaServerTakesEverySpellingOfTheWildcardAsOne(t *testing.T) {
	for _, listen := range []string{":7790", "0.0.0.0:7790", "[::]:7790", "[::0]:7790", "[0:0:0:0:0:0:0:0]:7790", "[::ffff:0.0.0.0]:7790"} {
		t.Run(listen, func(t *testing.T) {
			rig := startMediaServerOn(t, listen, []string{"192.168.1.20", "203.0.113.5"},
				hostIface{"eth0", mediaServerLANFlags, []string{"203.0.113.5/24", "192.168.1.20/24"}})
			if !rig.enabled {
				t.Fatalf("DLNA did not start: %s", strings.Join(rig.log.All(), "\n"))
			}
			for _, location := range rig.aliveLocations(t) {
				if location != "http://192.168.1.20:7790/dlna/description.xml" {
					t.Errorf("an alive NOTIFY announces %s", location)
				}
			}
			if rig.lc.server.AcceptsOn(netip.MustParseAddr("203.0.113.5")) {
				t.Error("the listener takes a connection on the public address")
			}
		})
	}
}
