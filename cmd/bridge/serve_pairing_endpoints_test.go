package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/api"
)

// TestServeBakesHealthEndpointsIntoThePairingQR pins the one line no
// package test can see: `admin.Deps.Endpoints: apiSrv.ReachableEndpoints`
// in runServe. Without it the admin has no endpoint list at all — the QR
// carries the primary alone, the panel renders empty — and every test in
// internal/admin stays green. (The first draft fell back to the admin's
// old host-network walk there, which on a host without Tailscale is
// byte-identical to health: this test could not see the line missing.
// That walk is the shape the QR shipped in from PR #269 until
// 2026-09-20, with no Tailscale entry and no customEndpoints.)
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
	// Both ports must be real (bootPairingServe holds them; backlog B313).
	s := bootPairingServe(t, "customEndpoints:\n  - "+custom+"\n")
	ctx, stderr := s.ctx, s.stderr

	// What the phone sees.
	tlsClient := pairingPhone()
	resp, err := tlsClient.Get(s.base + "/v1/health")
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
	adminBase := "http://" + s.admin
	client := &http.Client{Timeout: 10 * time.Second}
	mint := pairViaAdmin(t, ctx, client, adminBase+"/api/tokens",
		`{"name":"boot test","url":"`+primary+`"}`, http.StatusCreated, stderr)
	assertQRMatchesHealth(t, "mint", mint, primary, health.Endpoints)

	// Rotate shares the builder; the same wiring must reach it.
	rotated := pairViaAdmin(t, ctx, client, adminBase+"/api/tokens/"+mint.ID+"/rotate",
		`{"url":"`+primary+`"}`, http.StatusOK, stderr)
	assertQRMatchesHealth(t, "rotate", rotated, primary, health.Endpoints)
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
			"admin.Deps.Endpoints is not wired to apiSrv.ReachableEndpoints, so the QR has no "+
			"endpoint list to bake (primary alone) instead of what the phone sees.",
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
