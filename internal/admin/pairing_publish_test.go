package admin

import (
	"html"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/advertise"
	"github.com/acoseac/1-bit-bridge/internal/config"
)

func containerProbe(t *testing.T, host string, inside bool) {
	t.Helper()
	restore := advertise.SetHostProbeForTest(
		func() (string, error) { return host, nil },
		func() bool { return inside },
	)
	t.Cleanup(restore)
}

func TestDefaultBridgeURLDoesNotBakeAGeneratedContainerName(t *testing.T) {
	cfg := &config.Config{ListenAddress: ":7788"}

	containerProbe(t, "0123456789ab", true)
	if got := defaultBridgeURL(cfg); got != "" {
		t.Fatalf("container primary = %q, want empty", got)
	}
	if strings.Contains(defaultBridgeURL(cfg), "localhost") {
		t.Fatal("container primary used localhost")
	}

	cfg.CustomEndpoints = []string{
		"https://127.0.0.1:7788",
		"https://192.0.2.10:7788",
	}
	if got := defaultBridgeURL(cfg); got != "https://192.0.2.10:7788" {
		t.Fatalf("custom primary = %q", got)
	}

	cfg.CustomEndpoints = nil
	containerProbe(t, "musicbox", true)
	if got := defaultBridgeURL(cfg); got != "https://musicbox.local:7788" {
		t.Fatalf("chosen hostname primary = %q", got)
	}

	containerProbe(t, "0123456789ab", false)
	if got := defaultBridgeURL(cfg); got != "https://0123456789ab.local:7788" {
		t.Fatalf("12 hex outside a container = %q", got)
	}

	restore := advertise.SetHostProbeForTest(
		func() (string, error) { return "", errHostname },
		func() bool { return false },
	)
	t.Cleanup(restore)
	if got := defaultBridgeURL(cfg); got != "https://localhost:7788" {
		t.Fatalf("hostname error outside a container = %q", got)
	}
	restore = advertise.SetHostProbeForTest(
		func() (string, error) { return "", errHostname },
		func() bool { return true },
	)
	t.Cleanup(restore)
	if got := defaultBridgeURL(cfg); got != "" {
		t.Fatalf("hostname error inside a container = %q", got)
	}
}

// errHostname is the stand-in a hostname read returns when the name
// cannot be read. The value is not shown.
var errHostname = errHost("unread")

type errHost string

func (e errHost) Error() string { return string(e) }

func refuseUndialableMint(t *testing.T, h http.Handler, srv *Server, body map[string]string) {
	t.Helper()
	var apiErr struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	code := doJSON(t, h, "POST", "/api/tokens", body, &apiErr)
	if code != http.StatusBadRequest || apiErr.Error != "pairing-url" {
		t.Fatalf("mint %v: status %d error %q message %q", body, code, apiErr.Error, apiErr.Message)
	}
	if apiErr.Message != advertise.PairingNeedsEndpoint {
		t.Fatalf("message = %q", apiErr.Message)
	}
	if n := len(srv.deps.Auth.List()); n != 0 {
		t.Fatalf("mint wrote %d tokens", n)
	}
}

func TestAContainerMintRefusesAnUndialableURL(t *testing.T) {
	srv, _, _ := newTestServer(t)
	h := srv.Handler()
	containerProbe(t, "0123456789ab", true)

	refuseUndialableMint(t, h, srv, map[string]string{"name": "iPhone"})
	refuseUndialableMint(t, h, srv, map[string]string{"name": "iPhone", "url": "https://localhost:7788"})
	refuseUndialableMint(t, h, srv, map[string]string{"name": "iPhone", "url": "https://0123456789ab.local:7788"})

	var mint pairResult
	code := doJSON(t, h, "POST", "/api/tokens", map[string]string{
		"name": "iPhone",
		"url":  "https://192.0.2.10:7788",
	}, &mint)
	if code != http.StatusCreated {
		t.Fatalf("dialable mint status = %d", code)
	}
	toks := srv.deps.Auth.List()
	if len(toks) != 1 || !toks[0].RotatedAt.IsZero() {
		t.Fatalf("tokens = %+v", toks)
	}

	var apiErr struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	code = doJSON(t, h, "POST", "/api/tokens/"+mint.ID+"/rotate", map[string]string{
		"url": "https://localhost:7788",
	}, &apiErr)
	if code != http.StatusBadRequest || apiErr.Message != advertise.PairingNeedsEndpoint {
		t.Fatalf("rotate localhost: %d %q", code, apiErr.Message)
	}
	if !srv.deps.Auth.List()[0].RotatedAt.IsZero() {
		t.Fatal("refused rotate moved RotatedAt")
	}
	code = doJSON(t, h, "POST", "/api/tokens/"+mint.ID+"/rotate", map[string]string{}, &apiErr)
	if code != http.StatusBadRequest || apiErr.Message != advertise.PairingNeedsEndpoint {
		t.Fatalf("rotate empty: %d %q", code, apiErr.Message)
	}
	if !srv.deps.Auth.List()[0].RotatedAt.IsZero() {
		t.Fatal("empty rotate moved RotatedAt")
	}
}

func TestTheDevicesPageSaysToSetCustomEndpoints(t *testing.T) {
	srv, _, _ := newTestServer(t)
	containerProbe(t, "0123456789ab", true)
	req := httptest.NewRequest(http.MethodGet, "/devices", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	req.Host = testConsoleHost
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	body := w.Body.String()
	start := strings.Index(body, `id="pair-form"`)
	end := strings.Index(body, `id="pair-step-result"`)
	if start < 0 || end < start {
		t.Fatalf("pair form missing")
	}
	form := body[start:end]
	if !strings.Contains(form, html.EscapeString(advertise.PairingNeedsEndpoint)) {
		t.Fatalf("form does not carry the sentence:\n%s", form)
	}
	if strings.Contains(form, "localhost") || strings.Contains(form, "0123456789ab") {
		t.Fatalf("form baked an undialable address:\n%s", form)
	}
}
