package dlna

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

// endpointURLs are the LOCATION bases of cfg's advertise endpoints.
func endpointURLs(cfg ServerConfig) []string {
	var out []string
	for _, ep := range cfg.AdvertiseEndpoints {
		out = append(out, ep.ServerURL)
	}
	return out
}

// cidrAddrs is the interface addresses cidrs name, each with its prefix.
func cidrAddrs(t *testing.T, cidrs ...string) []net.Addr {
	t.Helper()
	var out []net.Addr
	for _, c := range cidrs {
		ip, n, err := net.ParseCIDR(c)
		if err != nil {
			t.Fatalf("parse %s: %v", c, err)
		}
		n.IP = ip
		out = append(out, n)
	}
	return out
}

// Test_ownHostOnly_RefusesAnAddressOfThisHostItDoesNotServe is B687's half of
// the Host check (B170): a literal Host names this host only when it is an
// address of a class the listener serves, loopback, RFC 1918, a ULA outside
// Tailscale's, link-local, as well as an address of this host. Until B687
// the check took any address of the host's interfaces (isInterfaceAddr), so
// a request over a LAN connection that named the host's public address, its
// SLAAC address or its tailnet address was answered, and a Browse handed
// back the library's URLs on that address. Each refused Host here is an
// address this host holds, so the old check answered it.
func Test_ownHostOnly_RefusesAnAddressOfThisHostItDoesNotServe(t *testing.T) {
	s, err := NewServer(ServerConfig{
		Library:            newTestLib(testTrack("t1", "Test Track")),
		UDN:                "uuid:test-public-host",
		FriendlyName:       "Test Bridge",
		ListenAddress:      ":7790",
		ServerURL:          "http://192.168.1.20:7790",
		AdvertiseEndpoints: []AdvertiseEndpoint{{ServerURL: "http://192.168.1.20:7790"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	s.interfaceAddrs = func() ([]net.Addr, error) {
		return cidrAddrs(t, "192.168.1.20/24", "203.0.113.5/24", "fd12:3456::20/64", "2001:db8::20/64",
			"fe80::20/64", "100.64.0.7/10", "fd7a:115c:a1e0::7/48"), nil
	}
	ts := httptest.NewServer(s.handler())
	t.Cleanup(ts.Close)
	for _, host := range []string{
		"203.0.113.5:7790",          // the public IPv4 address of a LAN interface
		"203.0.113.5",               // no port
		"[::ffff:203.0.113.5]:7790", // mapped
		"[2001:db8::20]:7790",       // the LAN interface's SLAAC address
		"100.64.0.7:7790",           // the tailnet's IPv4 address
		"[fd7a:115c:a1e0::7]:7790",  // the tailnet's ULA
	} {
		if code, _ := sendAs(t, ts, descriptionRequest(t, ts), host); code != http.StatusMisdirectedRequest {
			t.Errorf("Host %q, description.xml: status %d, want 421", host, code)
		}
		code, body := sendAs(t, ts, browseRequest(t, ts), host)
		if code != http.StatusMisdirectedRequest {
			t.Errorf("Host %q, Browse: status %d, want 421", host, code)
		}
		if strings.Contains(body, "/dlna/file/") {
			t.Errorf("Host %q, Browse: the answer carries the library's URLs", host)
		}
	}
	for _, host := range []string{"192.168.1.20:7790", "[fd12:3456::20]:7790", "[fe80::20]:7790", "127.0.0.1:7790"} {
		code, body := sendAs(t, ts, browseRequest(t, ts), host)
		if code != http.StatusOK {
			t.Errorf("Host %q, Browse: status %d, want 200", host, code)
			continue
		}
		if want := "http://" + host + "/dlna/file/"; !strings.Contains(body, want) {
			t.Errorf("Host %q, Browse: no <res> built on %q", host, want)
		}
	}
}

// TestNewServerRefusesToAdvertiseAnAddressItWouldNotServe is B687's last
// link: a server whose ServerURL or an advertise endpoint's LOCATION names an
// address of a class its listener does not serve is refused at NewServer,
// whatever wiring built it, so no renderer is handed a URL on a public
// address that was not pinned. Until B687 the server served any address it
// advertised on an eligible interface, and its wiring advertised a LAN
// interface's public IPv4 address when the interface listed it first. A
// LAN address, loopback, link-local, a name, and the literal
// dlna.listenAddress names are taken: a public one is the operator's opt-in,
// and one of another class (the tailnet's) is the operator's own pin, which
// serves no one and warns at Start, and starts.
func TestNewServerRefusesToAdvertiseAnAddressItWouldNotServe(t *testing.T) {
	base := func(listen, serverURL string, locations ...string) ServerConfig {
		cfg := ServerConfig{Library: newTestLib(), UDN: "uuid:test-advertise", ListenAddress: listen, ServerURL: serverURL}
		for _, l := range locations {
			cfg.AdvertiseEndpoints = append(cfg.AdvertiseEndpoints, AdvertiseEndpoint{ServerURL: l})
		}
		return cfg
	}
	for _, tc := range []struct {
		name string
		cfg  ServerConfig
		ok   bool
	}{
		{"a_public_server_url", base(":7790", "http://203.0.113.5:7790"), false},
		{"a_public_location", base(":7790", "http://192.168.1.20:7790", "http://192.168.1.20:7790", "http://203.0.113.5:7790"), false},
		{"a_mapped_public_location", base(":7790", "http://192.168.1.20:7790", "http://[::ffff:203.0.113.5]:7790"), false},
		{"a_global_ipv6_server_url", base(":7790", "http://[2001:db8::20]:7790"), false},
		{"the_tailnets_ipv4", base(":7790", "http://100.64.0.7:7790"), false},
		{"tailscales_ula", base(":7790", "http://[fd7a:115c:a1e0::7]:7790"), false},
		{"the_unspecified_address", base(":7790", "http://0.0.0.0:7790"), false},
		{"a_public_address_pinned_elsewhere", base("192.168.1.20:7790", "http://203.0.113.5:7790"), false},
		{"a_lan_address", base(":7790", "http://192.168.1.20:7790", "http://192.168.1.20:7790", "http://10.0.0.5:7790"), true},
		{"a_self_assigned_address", base(":7790", "http://169.254.7.8:7790", "http://169.254.7.8:7790"), true},
		{"a_ula", base(":7790", "http://[fd12:3456::20]:7790"), true},
		{"loopback", base("127.0.0.1:7790", "http://127.0.0.1:7790"), true},
		{"a_name", base(":7790", "http://bridge-box.lan:7790"), true},
		{"the_pinned_public_address", base("203.0.113.5:7790", "http://203.0.113.5:7790"), true},
		{"the_pinned_public_address_mapped", base("[::ffff:203.0.113.5]:7790", "http://[::ffff:203.0.113.5]:7790"), true},
		{"the_pinned_public_ipv6_address", base("[2001:db8::20]:7790", "http://[2001:db8::20]:7790"), true},
		{"the_tailnets_ipv4_as_the_listen_address_names_it", base("100.64.0.7:7790", "http://100.64.0.7:7790"), true},
		{"tailscales_ula_as_the_listen_address_names_it", base("[fd7a:115c:a1e0::7]:7790", "http://[fd7a:115c:a1e0::7]:7790"), true},
		{"the_tailnets_ipv4_beside_another_pin", base("100.64.0.7:7790", "http://100.64.0.8:7790"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewServer(tc.cfg)
			switch {
			case tc.ok && err != nil:
				t.Errorf("refused: %v", err)
			case !tc.ok && err == nil:
				t.Errorf("NewServer took a configuration that advertises an address the listener does not serve")
			case err != nil:
				// The refusal names the field, never the value.
				for _, raw := range append([]string{tc.cfg.ServerURL}, endpointURLs(tc.cfg)...) {
					if host := strings.TrimSuffix(strings.TrimPrefix(raw, "http://"), ":7790"); strings.Contains(err.Error(), strings.Trim(host, "[]")) {
						t.Errorf("the refusal names %s: %v", host, err)
					}
				}
			}
		})
	}
}

// TestTheAddressClassesAndTheOptIn pins the two classifiers B687 adds:
// IsLANClassAddress, what the listener serves on a member without an
// opt-in, and PinnedPublicAddress, the one public address it serves at the
// operator's word. A LAN address is never the opt-in and a public address
// never of a LAN class; 100.64/10 and Tailscale's ULA are neither, so no
// tailnet address is served either way; a host name and every spelling of
// the wildcard opt nothing in; an IPv4-mapped address is judged unmapped.
func TestTheAddressClassesAndTheOptIn(t *testing.T) {
	for _, tc := range []struct {
		addr string
		lan  bool
	}{
		{"192.168.1.20", true}, {"10.0.0.5", true}, {"172.16.0.1", true}, {"172.31.255.255", true},
		{"169.254.7.8", true}, {"fe80::1", true}, {"fe80::1%eth0", true}, {"fd12:3456::1", true},
		{"::ffff:192.168.1.20", true},
		{"172.32.0.1", false}, {"203.0.113.5", false}, {"93.184.216.34", false}, {"2001:db8::1", false},
		{"100.64.0.1", false}, {"100.127.255.254", false}, {"fd7a:115c:a1e0::7", false},
		{"::ffff:93.184.216.34", false}, {"127.0.0.1", false}, {"::1", false}, {"0.0.0.0", false},
		{"::", false}, {"224.0.0.1", false}, {"ff02::c", false},
	} {
		if got := IsLANClassAddress(netip.MustParseAddr(tc.addr)); got != tc.lan {
			t.Errorf("IsLANClassAddress(%s) = %v, want %v", tc.addr, got, tc.lan)
		}
	}
	if IsLANClassAddress(netip.Addr{}) {
		t.Error("IsLANClassAddress of no address is true")
	}
	for _, tc := range []struct{ listen, pin string }{
		{"203.0.113.5:7790", "203.0.113.5"},
		{"93.184.216.34:7790", "93.184.216.34"},
		{"[::ffff:203.0.113.5]:7790", "203.0.113.5"},
		{"[2001:db8::20]:7790", "2001:db8::20"},
		{"[2001:db8::20%eth0]:7790", "2001:db8::20"},
		{"100.63.255.255:7790", "100.63.255.255"}, // just below 100.64/10
		{"100.128.0.0:7790", "100.128.0.0"},       // just above it
		{"192.168.1.20:7790", ""}, {"10.0.0.5:7790", ""}, {"169.254.7.8:7790", ""},
		{"[fd12:3456::5]:7790", ""}, {"[fe80::1%eth0]:7790", ""}, {"127.0.0.1:7790", ""}, {"[::1]:7790", ""},
		{"100.64.0.7:7790", ""}, {"100.127.255.254:7790", ""}, {"[fd7a:115c:a1e0::7]:7790", ""},
		{":7790", ""}, {"0.0.0.0:7790", ""}, {"[::]:7790", ""}, {"[::0]:7790", ""}, {"[::ffff:0.0.0.0]:7790", ""},
		{"music.example:7790", ""}, {"203.0.113.5", ""}, {"224.0.0.1:7790", ""}, {"255.255.255.255:7790", ""},
	} {
		got := PinnedPublicAddress(tc.listen)
		switch {
		case tc.pin == "" && got.IsValid():
			t.Errorf("PinnedPublicAddress(%q) = %s, want none", tc.listen, got)
		case tc.pin != "" && got != netip.MustParseAddr(tc.pin):
			t.Errorf("PinnedPublicAddress(%q) = %v, want %s", tc.listen, got, tc.pin)
		}
	}
}

// Test_ownHostOnly_TakesThePinnedPublicAddressAlone pins the Host check of a
// listener dlna.listenAddress pins to a public address: that address is
// named as a Host and the Browse builds its URLs on it, and another public
// address of this host is still refused (B687).
func Test_ownHostOnly_TakesThePinnedPublicAddressAlone(t *testing.T) {
	s, err := NewServer(ServerConfig{
		Library:       newTestLib(testTrack("t1", "Test Track")),
		UDN:           "uuid:test-pinned-host",
		ListenAddress: "203.0.113.5:7790",
		ServerURL:     "http://203.0.113.5:7790",
	})
	if err != nil {
		t.Fatal(err)
	}
	s.interfaceAddrs = func() ([]net.Addr, error) {
		return cidrAddrs(t, "203.0.113.5/24", "203.0.113.6/24", "192.168.1.20/24"), nil
	}
	ts := httptest.NewServer(s.handler())
	t.Cleanup(ts.Close)
	code, body := sendAs(t, ts, browseRequest(t, ts), "203.0.113.5:7790")
	if code != http.StatusOK || !strings.Contains(body, "http://203.0.113.5:7790/dlna/file/") {
		t.Errorf("Browse naming the pinned address: status %d, want 200 and its URLs on that address", code)
	}
	if code, _ := sendAs(t, ts, browseRequest(t, ts), "203.0.113.6:7790"); code != http.StatusMisdirectedRequest {
		t.Errorf("Browse naming another public address of this host: status %d, want 421", code)
	}
}

// TestStartRefusesAListenerNotBoundToThePin pins Start's half of the opt-in:
// the public address dlna.listenAddress names is served only as the address
// the listener bound, so a listener handed over that stands for another one
// is refused at Start, before anything is served or announced.
func TestStartRefusesAListenerNotBoundToThePin(t *testing.T) {
	l := loopbackListener(t)
	s, err := NewServer(ServerConfig{
		Library:       newTestLib(testTrack("t1", "Test Track")),
		UDN:           "uuid:test-pin-unbound",
		ListenAddress: "203.0.113.5:7790",
		Listener:      l,
		ServerURL:     "http://203.0.113.5:7790",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.Start(ctx); err == nil || !strings.Contains(err.Error(), "not bound to the public address") {
		_ = s.Stop(context.Background())
		t.Fatalf("Start of a listener on loopback pinned to a public address: %v, want a refusal", err)
	}
	if s.AcceptsOn(netip.MustParseAddr("127.0.0.1")) || s.AcceptsOn(netip.MustParseAddr("203.0.113.5")) {
		t.Error("a refused Start left a listener accepting")
	}
}
