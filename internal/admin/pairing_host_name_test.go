package admin

import (
	"html"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/advertise"
	"github.com/acoseac/1-bit-bridge/internal/config"
)

// pairingWalk makes the interface walk the pairing URL reads hold addrs on
// one physical interface, for the rest of the test.
func pairingWalk(t *testing.T, addrs ...string) {
	t.Helper()
	t.Cleanup(advertise.SetInterfacesForTest([]advertise.InterfaceForTest{{
		Name:  "en0",
		Flags: net.FlagUp,
		Addrs: addrs,
	}}))
}

// TestThePairingURLOnAHostWhoseNameIsNotASCII is backlog B739. The pairing QR's
// primary URL named the host as the system reported it, `https://<name>.local`,
// and the certificate cannot carry a name that is not ASCII. A name with
// non-ASCII characters is dialled in its IDNA ASCII form, which the certificate
// carries. A name with no valid form (a test Mac's kern.hostname) is no host
// name, and the fallback for no host name, localhost, is right only on the
// same machine: the phone would dial itself. Such a host is paired at a LAN
// address of its own, and with none there is no URL to offer. An ASCII name is
// dialled as it is, and a host name that cannot be read still falls back to
// localhost, the documented same-machine path.
func TestThePairingURLOnAHostWhoseNameIsNotASCII(t *testing.T) {
	cfg := &config.Config{ListenAddress: ":7788"}
	for _, tc := range []struct {
		name, host string
		walk       []string
		want       string
	}{
		{"a Cyrillic name", "Жук", []string{"192.168.1.20"}, "https://xn--f1ai0a.local:7788"},
		{"no valid form: a LAN address", "Ð\u0091Ä¯U", []string{"192.168.1.20"}, "https://192.168.1.20:7788"},
		{"no valid form, no LAN address", "Ð\u0091Ä¯U", []string{"203.0.113.7"}, ""},
		{"mixed case and an underscore", "MacBook-Pro_2", []string{"192.168.1.20"}, "https://MacBook-Pro_2.local:7788"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			containerProbe(t, tc.host, false)
			pairingWalk(t, tc.walk...)
			got := defaultBridgeURL(cfg)
			if got != tc.want {
				t.Errorf("the pairing URL is %q, want %q", got, tc.want)
			}
			if strings.Contains(got, "localhost") {
				t.Errorf("the pairing URL %q sends the phone to itself", got)
			}
		})
	}
	t.Run("a host name that cannot be read", func(t *testing.T) {
		t.Cleanup(advertise.SetHostProbeForTest(
			func() (string, error) { return "", errHostname },
			func() bool { return false },
		))
		pairingWalk(t, "192.168.1.20")
		if got := defaultBridgeURL(cfg); got != "https://localhost:7788" {
			t.Errorf("the pairing URL is %q, want the same-machine https://localhost:7788", got)
		}
	})
}

// TestAConvertedNameIsPairedAtItsLANAddressWithMDNSOff is B739's second
// round. A host name that is not ASCII is advertised in its IDNA ASCII form,
// and nothing on the LAN but the bridge's own mDNS responder answers that
// form: the system's own responder publishes the name as the system reported
// it, or nothing. With mdns.enabled false, `https://xn--f1ai0a.local` names
// nothing a phone can resolve, so the pairing QR takes what a host with no
// name takes: the address the listener is bound to or the LAN address, and in
// a detected container a dialable customEndpoints entry or none, never the
// container's own address. With mDNS on it is the `.local` name, and an ASCII
// name is its `.local` name either way, as on main, since the system's own
// responder answers it.
func TestAConvertedNameIsPairedAtItsLANAddressWithMDNSOff(t *testing.T) {
	for _, tc := range []struct {
		name, host   string
		docker, mdns bool
		listen       string
		custom       []string
		want         string
	}{
		{"a Cyrillic name, mDNS off", "Жук", false, false, ":7788", nil, "https://192.168.1.20:7788"},
		{"a Cyrillic name, mDNS off, bound to an address", "Жук", false, false, "192.168.2.20:7788", nil,
			"https://192.168.2.20:7788"},
		{"a Cyrillic name, mDNS on", "Жук", false, true, ":7788", nil, "https://xn--f1ai0a.local:7788"},
		{"an ASCII name, mDNS off", "MacBook-Pro_2", false, false, ":7788", nil, "https://MacBook-Pro_2.local:7788"},
		{"a Cyrillic name in a container, mDNS off", "Жук", true, false, ":7788", nil, ""},
		{"a Cyrillic name in a container, mDNS off, with a custom endpoint", "Жук", true, false, ":7788",
			[]string{"https://music.example.test:8443"}, "https://music.example.test:8443"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			containerProbe(t, tc.host, tc.docker)
			twoLANWalk(t)
			cfg := &config.Config{ListenAddress: tc.listen, CustomEndpoints: tc.custom}
			cfg.MDNS.Enabled = &tc.mdns
			if got := defaultBridgeURL(cfg); got != tc.want {
				t.Errorf("the pairing URL is %q, want %q", got, tc.want)
			}
		})
	}
}

// TestTheLANFallbackIsTheAddressTheListenerIsBoundTo is B739's second round. A
// host whose name gives a phone nothing to dial is paired at an address of its
// own, and the first LAN address of the interface walk is not where a listener
// bound to another address listens: with 192.168.1.20 listed before
// 192.168.2.20 and `listenAddress: 192.168.2.20:7788`, the QR dialled
// 192.168.1.20, where nothing answers. The address the listener is bound to
// wins, when a phone can dial it, then a customEndpoints entry a phone can
// dial, then a loopback listener's own address, reached from this machine alone,
// and only for a listener on every address, the first LAN IPv4 address. A
// listener bound to any other name or a link-local address isn't there, so
// without a custom endpoint a phone is told to set one.
func TestTheLANFallbackIsTheAddressTheListenerIsBoundTo(t *testing.T) {
	custom := []string{"https://music.example.test:8443"}
	for _, tc := range []struct {
		name, listen string
		custom       []string
		want         string
	}{
		{"bound to the second address", "192.168.2.20:7788", nil, "https://192.168.2.20:7788"},
		{"bound to an address, beside a custom endpoint", "192.168.2.20:7788", custom, "https://192.168.2.20:7788"},
		{"bound to an IPv6 address", "[fd00::20]:7788", nil, "https://[fd00::20]:7788"},
		{"on every address, beside a custom endpoint", ":7788", custom, "https://music.example.test:8443"},
		{"on every address", ":7788", nil, "https://192.168.1.20:7788"},
		{"on every IPv4 address", "0.0.0.0:7788", nil, "https://192.168.1.20:7788"},
		{"on every IPv6 address", "[::]:7788", nil, "https://192.168.1.20:7788"},
		{"bound to loopback", "127.0.0.1:7788", nil, "https://127.0.0.1:7788"},
		{"bound to loopback, beside a custom endpoint", "127.0.0.1:7788", custom, "https://music.example.test:8443"},
		{"bound to loopback by name", "localhost:7788", nil, "https://localhost:7788"},
		{"bound to a name", "music.example.test:7788", nil, ""},
		{"bound to an IPv6 link-local address", "[fe80::20%en1]:7788", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			containerProbe(t, "Ð\u0091Ä¯U", false)
			twoLANWalk(t)
			cfg := &config.Config{ListenAddress: tc.listen, CustomEndpoints: tc.custom}
			if got := defaultBridgeURL(cfg); got != tc.want {
				t.Errorf("the pairing URL is %q, want %q", got, tc.want)
			}
		})
	}
}

// twoLANWalk makes the interface walk hold 192.168.1.20 on en0 and then
// 192.168.2.20 on en1, for the rest of the test.
func twoLANWalk(t *testing.T) {
	t.Helper()
	t.Cleanup(advertise.SetInterfacesForTest([]advertise.InterfaceForTest{
		{Name: "en0", Flags: net.FlagUp, Addrs: []string{"192.168.1.20"}},
		{Name: "en1", Flags: net.FlagUp, Addrs: []string{"192.168.2.20"}},
	}))
}

// TestAHostWithNoNameToPairAtSaysSo: with no valid host name and no LAN
// address, the console's mint and the Devices page say there is no address a
// phone can dial (advertise.PairingNeedsEndpoint), as a detected container
// with none does, and mint no token, rather than offer localhost.
func TestAHostWithNoNameToPairAtSaysSo(t *testing.T) {
	srv, _, _ := newTestServer(t)
	h := srv.Handler()
	containerProbe(t, "Ð\u0091Ä¯U", false)
	pairingWalk(t, "203.0.113.7")

	refuseUndialableMint(t, h, srv, map[string]string{"name": "iPhone"})

	req := httptest.NewRequest(http.MethodGet, "/devices", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	req.Host = testConsoleHost
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	body := w.Body.String()
	start := strings.Index(body, `id="pair-form"`)
	end := strings.Index(body, `id="pair-step-result"`)
	if start < 0 || end < start {
		t.Fatal("pair form missing")
	}
	form := body[start:end]
	if !strings.Contains(form, html.EscapeString(advertise.PairingNeedsEndpoint)) {
		t.Errorf("the pairing form does not say there is no address to offer:\n%s", form)
	}
	if strings.Contains(form, "localhost") || strings.Contains(form, ".local:") {
		t.Errorf("the pairing form offers an address a phone cannot dial:\n%s", form)
	}
}
