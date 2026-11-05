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
