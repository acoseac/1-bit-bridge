package admin

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/advertise"
	"github.com/acoseac/1-bit-bridge/internal/config"
)

// The pairing QR and the Settings "Reachable endpoints" panel read
// Deps.Endpoints — the api layer's /v1/health enumeration — rather than
// running their own advertise.Endpoints walk. That walk stopped emitting
// anything Tailscale-classed in PR #269 (the append moved to the api
// layer), so both consumers lost every Tailscale entry: observed
// 2026-09-20 on a loopback bridge whose health advertised
// `nuc.sable-eagle.ts.net`, `100.102.105.89` and an `fd7a:…` address
// while `POST /api/tokens` answered `alternates: [nuc.local,
// 192.168.0.24]`. A phone paired on Wi-Fi had no Tailscale fallback
// recorded, which is exactly what buildPairURL's docblock promises it has.

// healthStyleEndpoints is the classed list a loopback bridge with
// Tailscale up advertises on /v1/health, in the api layer's class-stable
// order: LAN, mDNS, Tailscale DNS, Tailscale v4, Tailscale v6, custom.
// The URLs are the shape of the 2026-09-20 report.
func healthStyleEndpoints() []advertise.Endpoint {
	return []advertise.Endpoint{
		{URL: "https://192.168.0.24:7788", Class: advertise.ClassLANv4},
		{URL: "https://nuc.local:7788", Class: advertise.ClassMDNSHost},
		{URL: "https://nuc.sable-eagle.ts.net:7788", Class: advertise.ClassTailscaleDNS},
		{URL: "https://100.102.105.89:7788", Class: advertise.ClassTailscaleV4},
		{URL: "https://[fd7a:115c:a1e0::1]:7788", Class: advertise.ClassTailscaleV6},
		{URL: "https://custom.example.test:7788", Class: advertise.ClassCustom},
	}
}

// tailscaleURLs are the three entries only the api layer can supply.
var tailscaleURLs = []string{
	"https://nuc.sable-eagle.ts.net:7788",
	"https://100.102.105.89:7788",
	"https://[fd7a:115c:a1e0::1]:7788",
}

func TestPairAlternatesLoopbackBakesTheAdvertisedTailscaleEndpoints(t *testing.T) {
	cfg := &config.Config{ListenAddress: "0.0.0.0:7788"}
	const primary = "https://nuc.local:7788"
	got := pairAlternates(primary, cfg, healthStyleEndpoints)

	if got[0] != primary {
		t.Fatalf("alternates[0] = %q, want the operator primary %q (full=%v)", got[0], primary, got)
	}
	// Every advertised URL is present, in the api layer's class order,
	// with the primary lifted out of its mDNS slot rather than repeated.
	want := []string{
		primary,
		"https://192.168.0.24:7788",
		"https://nuc.sable-eagle.ts.net:7788",
		"https://100.102.105.89:7788",
		"https://[fd7a:115c:a1e0::1]:7788",
		"https://custom.example.test:7788",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("alternates =\n  %v\nwant\n  %v", got, want)
	}
}

// The nil-provider fallback is the pre-fix host walk PLUS customEndpoints
// — the second half of the defect was that loopback pairing dropped them
// even though health advertised them. The LAN/mDNS rows depend on the
// runner's interfaces, so only the deterministic entry is asserted.
func TestPairAlternatesLoopbackUnwiredStillCarriesCustomEndpoints(t *testing.T) {
	cfg := &config.Config{
		ListenAddress:   "0.0.0.0:7788",
		CustomEndpoints: []string{"https://custom.example.test:7788"},
	}
	got := pairAlternates("https://primary.example.test:7788", cfg, nil)
	if !containsURL(got, "https://custom.example.test:7788") {
		t.Errorf("unwired loopback alternates dropped cfg.CustomEndpoints: %v", got)
	}
}

// Public mode keeps its own synthesis (explicit-port autocert URL +
// customEndpoints, PR 5) and never consults the provider — a VPS's
// Tailscale identity must not reach the QR even when the api layer
// would happily report it.
func TestPairAlternatesPublicModeNeverConsultsTheProvider(t *testing.T) {
	cfg := &config.Config{
		ListenAddress:   ":443",
		Deployment:      config.DeploymentConfig{Mode: "public", AdminTLSTerminatedByProxy: true},
		Autocert:        config.AutocertConfig{Domain: "bridge.example.com"},
		CustomEndpoints: []string{"https://alt.example.com:443"},
	}
	calls := 0
	provider := func() []advertise.Endpoint {
		calls++
		return healthStyleEndpoints()
	}
	got := pairAlternates("https://bridge.example.com:443", cfg, provider)
	if calls != 0 {
		t.Errorf("public mode consulted the endpoint provider %d time(s); want 0", calls)
	}
	for _, u := range got {
		if strings.Contains(u, ".ts.net") || strings.Contains(u, "100.102.") || strings.Contains(u, "fd7a:") {
			t.Errorf("public-mode alternate %q is a Tailscale address — leak", u)
		}
	}
	want := []string{"https://bridge.example.com:443", "https://alt.example.com:443"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("public-mode alternates = %v, want %v", got, want)
	}
}

// Drive the real handlers. Mint and rotate share the builder, so both
// responses — `alternates` and the `urls=` field inside `pairURL` —
// must carry the Tailscale entries with the operator's primary first.
func TestMintAndRotateBakeTailscaleIntoAlternatesAndURLs(t *testing.T) {
	srv, _, _ := newTestServer(t)
	srv.deps.Endpoints = healthStyleEndpoints
	h := srv.Handler()
	const primary = "https://nuc.local:7788"

	var mint pairResult
	code := doJSON(t, h, "POST", "/api/tokens", map[string]string{
		"name": "iPhone 13",
		"url":  primary,
	}, &mint)
	if code != http.StatusCreated {
		t.Fatalf("mint: %d", code)
	}
	assertPairCarriesTailscale(t, "mint", mint, primary)

	var rotated pairResult
	code = doJSON(t, h, "POST", "/api/tokens/"+mint.ID+"/rotate",
		map[string]string{"url": primary}, &rotated)
	if code != http.StatusOK {
		t.Fatalf("rotate: %d", code)
	}
	assertPairCarriesTailscale(t, "rotate", rotated, primary)
}

func assertPairCarriesTailscale(t *testing.T, op string, res pairResult, primary string) {
	t.Helper()
	if len(res.Alternates) == 0 || res.Alternates[0] != primary {
		t.Fatalf("%s: alternates[0] = %v, want the primary %q first", op, res.Alternates, primary)
	}
	for _, u := range tailscaleURLs {
		if !containsURL(res.Alternates, u) {
			t.Errorf("%s: alternates lack the Tailscale endpoint %q: %v", op, u, res.Alternates)
		}
	}
	u, err := url.Parse(res.PairURL)
	if err != nil {
		t.Fatalf("%s: parse pairURL: %v", op, err)
	}
	if got := u.Query().Get("url"); got != primary {
		t.Errorf("%s: url= %q, want the primary %q", op, got, primary)
	}
	baked := strings.Split(u.Query().Get("urls"), "\n")
	if len(baked) == 0 || baked[0] != primary {
		t.Errorf("%s: urls= does not lead with the primary: %v", op, baked)
	}
	for _, want := range tailscaleURLs {
		if !containsURL(baked, want) {
			t.Errorf("%s: the QR's urls= lacks the Tailscale endpoint %q: %v", op, want, baked)
		}
	}
	// One entry per URL — the QR is what a phone parses, and a duplicate
	// there is a duplicate in its failover rotation.
	seen := map[string]int{}
	for _, b := range baked {
		seen[b]++
	}
	for b, n := range seen {
		if n != 1 {
			t.Errorf("%s: %q appears %d times in urls=", op, b, n)
		}
	}
}

// The panel renders the same list with its class tags — the row the
// panel's own prose tells the operator to look for after bringing a
// Tailscale tunnel up.
func TestEndpointsPanelRendersTheSharedEnumerationWithClasses(t *testing.T) {
	srv, _, _ := newTestServer(t)
	srv.deps.Endpoints = healthStyleEndpoints
	var entries []adminEndpointEntry
	if code := doJSON(t, srv.Handler(), "GET", "/api/endpoints", nil, &entries); code != http.StatusOK {
		t.Fatalf("endpoints: %d", code)
	}
	want := []adminEndpointEntry{
		{URL: "https://192.168.0.24:7788", Class: "LAN"},
		{URL: "https://nuc.local:7788", Class: "mDNS"},
		{URL: "https://nuc.sable-eagle.ts.net:7788", Class: "Tailscale DNS"},
		{URL: "https://100.102.105.89:7788", Class: "Tailscale"},
		{URL: "https://[fd7a:115c:a1e0::1]:7788", Class: "Tailscale"},
		{URL: "https://custom.example.test:7788", Class: "Custom"},
	}
	if len(entries) != len(want) {
		t.Fatalf("entries = %v, want %v", entries, want)
	}
	for i := range want {
		if entries[i] != want[i] {
			t.Errorf("entries[%d] = %+v, want %+v", i, entries[i], want[i])
		}
	}
}

// The `:0` short-circuit answers before the provider is asked — the
// configured address names no port a URL could carry, and the panel's
// honest answer there is the empty list (TestEndpointsHandlerHandlesPortZero).
func TestEndpointsPanelPortZeroDoesNotConsultTheProvider(t *testing.T) {
	srv, cfg, _ := newTestServer(t)
	cfg.ListenAddress = ":0"
	calls := 0
	srv.deps.Endpoints = func() []advertise.Endpoint {
		calls++
		return healthStyleEndpoints()
	}
	var entries []adminEndpointEntry
	if code := doJSON(t, srv.Handler(), "GET", "/api/endpoints", nil, &entries); code != http.StatusOK {
		t.Fatalf("endpoints: %d", code)
	}
	if calls != 0 || len(entries) != 0 {
		t.Errorf("port-zero panel: provider calls = %d, entries = %v; want 0 and empty", calls, entries)
	}
}

func containsURL(list []string, u string) bool {
	for _, x := range list {
		if x == u {
			return true
		}
	}
	return false
}
