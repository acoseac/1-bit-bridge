package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/api"
)

// TestServeBakesHealthEndpointsIntoThePairingQR pins the one line no
// package test can see: `admin.Deps.Endpoints: apiSrv.ReachableEndpoints`
// in runServe. Without it the admin falls back to its own host-network
// walk and every test in internal/admin stays green — that fallback is
// the exact shape the pairing QR shipped in from PR #269 until
// 2026-09-20, with no Tailscale entry and no customEndpoints.
//
// Booting the real server, the test asks the two surfaces the same
// question and requires the same answer: `POST /api/tokens` must bake
// precisely what `GET /v1/health` advertises, in health's order, behind
// the operator's primary. A Tailscale identity cannot be injected into
// a real `serve` (the provider is process-scoped), so the discriminator
// is a customEndpoint — health has always advertised it, the QR only
// does through the shared enumeration. The fixture asserts health does
// carry it, so the equality cannot hold vacuously.
func TestServeBakesHealthEndpointsIntoThePairingQR(t *testing.T) {
	const custom = "https://custom.example.test:7788"
	dir := t.TempDir()
	lib := filepath.Join(dir, "Music")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	// Both ports must be real: ReachableEndpoints answers nil for a
	// `:0` listen address (no port a URL could carry), which would make
	// health and the QR trivially agree on an empty list. Reserved and
	// released up front — the weaker half of this fixture, for the
	// reasons freeLoopbackPort records.
	apiPort, adminPort := freeLoopbackPort(t), freeLoopbackPort(t)
	cfgPath := filepath.Join(dir, "bridge.yaml")
	body := fmt.Sprintf("libraryRoots:\n  - %s\ndataDir: %s\nadminAddress: 127.0.0.1:%d\ncustomEndpoints:\n  - %s\n",
		lib, filepath.Join(dir, "data"), adminPort, custom)
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stdout, stderr := &safeBuffer{}, &safeBuffer{}
	done := make(chan int, 1)
	go func() {
		done <- run(ctx, []string{"serve", "--config", cfgPath,
			"--addr", fmt.Sprintf("127.0.0.1:%d", apiPort)}, stdout, stderr)
	}()
	addr, _ := waitForListening(t, stdout, 30*time.Second)
	waitForAdminReady(t, fmt.Sprintf("127.0.0.1:%d", adminPort), done, stderr)

	// What the phone sees.
	tlsClient := &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
	}
	resp, err := tlsClient.Get("https://" + addr + "/v1/health")
	if err != nil {
		t.Fatalf("GET /v1/health: %v; stderr=%s", err, stderr.String())
	}
	var health api.HealthResponse
	err = json.NewDecoder(resp.Body).Decode(&health)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("decode /v1/health: %v", err)
	}
	if !containsString(health.Endpoints, custom) {
		t.Fatalf("fixture broken: /v1/health does not advertise the customEndpoint, so the "+
			"comparison below could pass for the wrong reason; endpoints=%v", health.Endpoints)
	}

	// What the QR bakes. The primary is one health does NOT list, so
	// the relation to assert is exact: [primary] + health.endpoints.
	const primary = "https://primary.example.test:7788"
	adminBase := fmt.Sprintf("http://127.0.0.1:%d", adminPort)
	client := &http.Client{Timeout: 10 * time.Second}
	mint := pairViaAdmin(t, ctx, client, adminBase+"/api/tokens",
		`{"name":"boot test","url":"`+primary+`"}`, http.StatusCreated, stderr)
	assertQRMatchesHealth(t, "mint", mint, primary, health.Endpoints)

	// Rotate shares the builder; the same wiring must reach it.
	rotated := pairViaAdmin(t, ctx, client, adminBase+"/api/tokens/"+mint.ID+"/rotate",
		`{"url":"`+primary+`"}`, http.StatusOK, stderr)
	assertQRMatchesHealth(t, "rotate", rotated, primary, health.Endpoints)

	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("serve exit code = %d, want 0; stderr=%s", code, stderr.String())
		}
	case <-time.After(shutdownGrace + 5*time.Second):
		t.Fatalf("serve did not shut down within grace window; stderr=%s", stderr.String())
	}
}

// pairResponse is the slice of the admin's pair result this test reads.
// A private mirror rather than an import: internal/admin's DTO is
// unexported, and the fields read here are the console's own contract.
type pairResponse struct {
	ID         string   `json:"id"`
	URL        string   `json:"url"`
	PairURL    string   `json:"pairURL"`
	Alternates []string `json:"alternates"`
}

func pairViaAdmin(t *testing.T, ctx context.Context, client *http.Client, endpoint, body string, wantCode int, stderr *safeBuffer) pairResponse {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v; stderr=%s", endpoint, err, stderr.String())
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("POST %s: read body: %v", endpoint, err)
	}
	if resp.StatusCode != wantCode {
		t.Fatalf("POST %s = %d, want %d: %s", endpoint, resp.StatusCode, wantCode, raw)
	}
	var out pairResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode %s: %v: %s", endpoint, err, raw)
	}
	return out
}

func assertQRMatchesHealth(t *testing.T, op string, res pairResponse, primary string, health []string) {
	t.Helper()
	if res.URL != primary {
		t.Errorf("%s: url = %q, want the operator's primary %q", op, res.URL, primary)
	}
	want := append([]string{primary}, health...)
	if strings.Join(res.Alternates, "\n") != strings.Join(want, "\n") {
		t.Errorf("%s: alternates are not [primary] + /v1/health.endpoints —\n  got  %v\n  want %v\n"+
			"admin.Deps.Endpoints is not wired to apiSrv.ReachableEndpoints, so the QR bakes the "+
			"admin's own host walk (no Tailscale entry, no customEndpoints) instead of what the phone sees.",
			op, res.Alternates, want)
	}
	// And the QR itself, not just the JSON beside it.
	u, err := url.Parse(res.PairURL)
	if err != nil {
		t.Fatalf("%s: parse pairURL: %v", op, err)
	}
	values := u.Query()
	if got := values.Get("urls"); got != strings.Join(want, "\n") {
		t.Errorf("%s: the QR's urls= differs from alternates:\n  got  %q\n  want %q", op, got, strings.Join(want, "\n"))
	}
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
