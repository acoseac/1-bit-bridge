package dlna

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

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
// LAN address, loopback, link-local, a name, and the public address
// dlna.listenAddress pins (the operator's opt-in) are taken.
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
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewServer(tc.cfg)
			switch {
			case tc.ok && err != nil:
				t.Errorf("refused: %v", err)
			case !tc.ok && err == nil:
				t.Errorf("NewServer took a configuration that advertises an address the listener does not serve")
			}
		})
	}
}
