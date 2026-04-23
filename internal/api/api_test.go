package api

import (
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/auth"
	"github.com/acoseac/1-bit-bridge/internal/config"
	servertls "github.com/acoseac/1-bit-bridge/internal/tls"
	"github.com/acoseac/1-bit-bridge/internal/version"
)

// newTestServer spins up an httptest.Server with the api handler and a
// populated auth store. Returns the server plus one valid raw token for
// driving the authed() middleware.
func newTestServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	dir := t.TempDir()
	lib := filepath.Join(dir, "Music")
	os.MkdirAll(lib, 0o755)
	cfg := &config.Config{
		LibraryRoots:  []string{lib},
		ListenAddress: ":7788",
		LibraryName:   "Test Library",
	}
	store, err := auth.OpenStore(filepath.Join(dir, "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	raw, _, err := store.Mint("test-client")
	if err != nil {
		t.Fatal(err)
	}
	srv := New(cfg, store, "AB:CD:EF:01:02:03:...:FF")
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	return hs, raw
}

func TestHealthReturns200WithExpectedShape(t *testing.T) {
	hs, _ := newTestServer(t)
	resp, err := http.Get(hs.URL + "/v1/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q", ct)
	}

	var got HealthResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ProtocolVersion != version.ProtocolVersion {
		t.Errorf("protocolVersion = %d, want %d", got.ProtocolVersion, version.ProtocolVersion)
	}
	if got.ServerVersion != version.ServerVersion {
		t.Errorf("serverVersion = %q, want %q", got.ServerVersion, version.ServerVersion)
	}
	if got.LibraryName != "Test Library" {
		t.Errorf("libraryName = %q", got.LibraryName)
	}
	if len(got.LibraryRoots) != 1 || got.LibraryRoots[0] != "Music" {
		t.Errorf("libraryRoots = %v, want [Music]", got.LibraryRoots)
	}
	if got.CertFingerprint == "" {
		t.Error("certFingerprint missing")
	}
	if got.StartedAt.IsZero() {
		t.Error("startedAt missing")
	}
	if got.ScanState.IsScanning {
		t.Error("scanState.isScanning should be false on fresh server")
	}
}

func TestHealthNoAuthRequired(t *testing.T) {
	hs, _ := newTestServer(t)
	// No Authorization header — must still return 200.
	resp, err := http.Get(hs.URL + "/v1/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("health without auth returned %d, want 200", resp.StatusCode)
	}
}

func TestProtocolVersionHeaderOnEveryResponse(t *testing.T) {
	hs, _ := newTestServer(t)
	resp, err := http.Get(hs.URL + "/v1/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got := resp.Header.Get("X-Bridge-Protocol")
	want := strconv.Itoa(version.ProtocolVersion)
	if got != want {
		t.Errorf("X-Bridge-Protocol = %q, want %q", got, want)
	}
}

func TestProtocolVersionHeaderOn404(t *testing.T) {
	hs, _ := newTestServer(t)
	resp, err := http.Get(hs.URL + "/v1/nothing-here")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
	// Middleware must still fire on unmatched routes.
	if got := resp.Header.Get("X-Bridge-Protocol"); got != strconv.Itoa(version.ProtocolVersion) {
		t.Errorf("X-Bridge-Protocol on 404 = %q", got)
	}
}

func TestLibraryRootsBasenamesOnly(t *testing.T) {
	// The full server-side absolute path must NOT leak to clients.
	hs, _ := newTestServer(t)
	resp, err := http.Get(hs.URL + "/v1/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(body), "/tmp") || strings.Contains(string(body), "/var/folders") {
		t.Errorf("health body leaks absolute server path: %s", body)
	}
}

// ---- authed() middleware tests, using a tiny probe handler ----

func newTestServerWithProbe(t *testing.T) (*httptest.Server, *Server, string) {
	t.Helper()
	dir := t.TempDir()
	lib := filepath.Join(dir, "Music")
	os.MkdirAll(lib, 0o755)
	cfg := &config.Config{LibraryRoots: []string{lib}, ListenAddress: ":7788", LibraryName: "T"}
	store, err := auth.OpenStore(filepath.Join(dir, "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	raw, _, _ := store.Mint("test")
	s := New(cfg, store, "fp")

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", s.health)
	mux.HandleFunc("GET /v1/probe", s.authed(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "probe-ok")
	}))
	hs := httptest.NewServer(protocolHeader(mux))
	t.Cleanup(hs.Close)
	return hs, s, raw
}

func TestAuthedMissingTokenReturns401(t *testing.T) {
	hs, _, _ := newTestServerWithProbe(t)
	resp, err := http.Get(hs.URL + "/v1/probe")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
	var er ErrorResponse
	_ = json.NewDecoder(resp.Body).Decode(&er)
	if er.Error != "unauthorized" {
		t.Errorf("error code = %q", er.Error)
	}
}

func TestAuthedBadTokenReturns401(t *testing.T) {
	hs, _, _ := newTestServerWithProbe(t)
	req, _ := http.NewRequest("GET", hs.URL+"/v1/probe", nil)
	req.Header.Set("Authorization", "Bearer wrong-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestAuthedValidTokenPassesThrough(t *testing.T) {
	hs, _, raw := newTestServerWithProbe(t)
	req, _ := http.NewRequest("GET", hs.URL+"/v1/probe", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Errorf("status = %d; body = %s", resp.StatusCode, body)
	}
	if string(body) != "probe-ok" {
		t.Errorf("body = %q", body)
	}
}

func TestAuthedBearerCaseInsensitive(t *testing.T) {
	// Clients differ: "Bearer X", "bearer X", "BEARER X" must all work.
	hs, _, raw := newTestServerWithProbe(t)
	for _, scheme := range []string{"Bearer", "bearer", "BEARER"} {
		req, _ := http.NewRequest("GET", hs.URL+"/v1/probe", nil)
		req.Header.Set("Authorization", scheme+" "+raw)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("scheme %q: %v", scheme, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Errorf("scheme %q: status = %d, body = %s", scheme, resp.StatusCode, body)
		}
	}
}

func TestAuthedWrongSchemeRejected(t *testing.T) {
	hs, _, raw := newTestServerWithProbe(t)
	req, _ := http.NewRequest("GET", hs.URL+"/v1/probe", nil)
	req.Header.Set("Authorization", "Basic "+raw)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("status = %d, want 401 (non-Bearer scheme must not fall through)", resp.StatusCode)
	}
}

// ---- integration over real TLS + fingerprint pinning ----

// TestHealthOverTLSWithPinnedCert proves the full wire works: a TLS server
// using a cert minted by internal/tls, fingerprint pinning on the client, a
// /v1/health round-trip that matches schema. This is the nearest Go-side
// analogue to what the iOS pairing flow will do.
func TestHealthOverTLSWithPinnedCert(t *testing.T) {
	tmp := t.TempDir()
	lib := filepath.Join(tmp, "Music")
	os.MkdirAll(lib, 0o755)

	certPath, keyPath := servertls.DefaultPaths(tmp)
	cert, fingerprint, err := servertls.LoadOrGenerate(certPath, keyPath, "localhost")
	if err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{LibraryRoots: []string{lib}, ListenAddress: ":7788", LibraryName: "TLS"}
	store, _ := auth.OpenStore(filepath.Join(tmp, "tokens.json"))

	s := New(cfg, store, fingerprint)
	hs := httptest.NewUnstartedServer(s.Handler())
	hs.TLS = &tls.Config{Certificates: []tls.Certificate{*cert}}
	hs.StartTLS()
	defer hs.Close()

	var peerFP string
	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true,
			VerifyConnection: func(state tls.ConnectionState) error {
				peerFP = servertls.FingerprintFromDER(state.PeerCertificates[0].Raw)
				return nil
			},
		},
	}}

	resp, err := client.Get(hs.URL + "/v1/health")
	if err != nil {
		t.Fatalf("health over TLS: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Errorf("status = %d", resp.StatusCode)
	}
	if peerFP != fingerprint {
		t.Errorf("peer fingerprint %q != server fingerprint %q", peerFP, fingerprint)
	}
	var got HealthResponse
	json.NewDecoder(resp.Body).Decode(&got)
	if got.CertFingerprint != fingerprint {
		t.Errorf("health reports fingerprint %q, expected %q", got.CertFingerprint, fingerprint)
	}
}
