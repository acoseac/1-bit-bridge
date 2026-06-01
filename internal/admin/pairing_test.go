package admin

import (
	"net/url"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/config"
)

// TestBuildPairURLOmitsUrlsWhenOnlyPrimary keeps the QR payload small
// and byte-identical to pre-v1.x builds when the bridge only knows
// about one endpoint — older iOS clients that don't handle `urls`
// still see exactly what they've always seen.
func TestBuildPairURLOmitsUrlsWhenOnlyPrimary(t *testing.T) {
	out := buildPairURL("https://host:7788", "tok", "AB:CD", "Home", []string{"https://host:7788"})
	if strings.Contains(out, "urls=") {
		t.Errorf("urls= should be omitted when alternates is just the primary: %s", out)
	}
	if !strings.Contains(out, "url=https") {
		t.Errorf("url= must still be present: %s", out)
	}
}

func TestBuildPairURLEmitsUrlsWhenAlternatesPresent(t *testing.T) {
	alts := []string{
		"https://192.168.1.10:7788",
		"https://homepc.local:7788",
		"https://100.64.5.9:7788",
	}
	out := buildPairURL("https://192.168.1.10:7788", "tok", "AB:CD", "Home", alts)
	if !strings.Contains(out, "urls=") {
		t.Fatalf("urls= missing: %s", out)
	}
	// Parse the query and confirm every alternate round-trips through
	// newline-joined encoding.
	u, err := url.Parse(out)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got := u.Query().Get("urls")
	want := strings.Join(alts, "\n")
	if got != want {
		t.Errorf("urls = %q, want %q", got, want)
	}
}

func TestBuildPairURLPrimaryStaysFirst(t *testing.T) {
	// Even when the caller lists the primary in the middle of
	// alternates, the `url=` field is what we pass explicitly — the
	// iOS fallback path (older builds ignoring `urls`) reads only
	// `url`, so it has to be the operator's chosen primary, not
	// whatever advertise.URLs returned first.
	out := buildPairURL("https://pick-me:7788", "tok", "AB:CD", "Home",
		[]string{"https://otherhost:7788", "https://pick-me:7788"})
	u, err := url.Parse(out)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := u.Query().Get("url"); got != "https://pick-me:7788" {
		t.Errorf("url= = %q, want https://pick-me:7788", got)
	}
}

func TestPairAlternatesPrependsPrimary(t *testing.T) {
	// advertise.URLs doesn't know about the operator's override URL —
	// our pairAlternates helper is what ensures it lands first. Test
	// with a non-default listen address just to exercise the port
	// parse.
	got := pairAlternates("https://user-chose-this:9999", &config.Config{ListenAddress: "127.0.0.1:7788"})
	if len(got) == 0 {
		t.Fatal("expected non-empty alternates")
	}
	if got[0] != "https://user-chose-this:9999" {
		t.Errorf("first alternate = %q, want the operator primary", got[0])
	}
	// And the primary isn't duplicated inside the advertise-derived
	// entries.
	count := 0
	for _, u := range got {
		if u == "https://user-chose-this:9999" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("primary appeared %d times; want exactly once", count)
	}
}

// TestPairAlternatesPublicModeFiltersLANAndTailscale pins the
// PR 5 contract: in public mode the QR contains only the
// operator-declared customEndpoints + the autocert public
// domain — NO LAN addresses, NO mDNS, NO Tailscale. Avoids
// baking VPS-internal hostnames into the iOS pair payload.
func TestPairAlternatesPublicModeFiltersLANAndTailscale(t *testing.T) {
	cfg := &config.Config{
		ListenAddress: ":443",
		Deployment: config.DeploymentConfig{
			Mode:                      "public",
			AdminTLSTerminatedByProxy: true,
		},
		Autocert:        config.AutocertConfig{Domain: "bridge.example.com"},
		CustomEndpoints: []string{"https://alt.example.com:443"},
	}
	got := pairAlternates("https://bridge.example.com:443", cfg)
	if len(got) == 0 {
		t.Fatal("expected non-empty alternates")
	}
	// Every URL must come from customEndpoints OR be the autocert
	// domain. NO LAN / .local / Tailscale URLs.
	for _, u := range got {
		if !strings.Contains(u, "bridge.example.com") && !strings.Contains(u, "alt.example.com") {
			t.Errorf("public-mode alternate %q is neither a customEndpoint nor the autocert domain — leak", u)
		}
		// Defensive: refuse hostnames that look like LAN/.local
		// even if they happened to contain the domain (paranoid).
		if strings.Contains(u, ".local:") || strings.Contains(u, "192.168.") || strings.Contains(u, "10.") {
			t.Errorf("public-mode alternate %q includes a LAN/mDNS hint", u)
		}
	}
}

// TestPairAlternatesPublicModeOmits443Port pins the Gemini
// medium normalization on PR #295: when the bridge listens on
// :443 (the https default), the synthesized autocert URL is
// `https://host` (no `:443` suffix) so an operator who also
// declared `https://host` in customEndpoints sees one URL in
// the QR, not two near-duplicates.
func TestPairAlternatesPublicModeOmits443Port(t *testing.T) {
	cfg := &config.Config{
		ListenAddress:   ":443",
		Deployment:      config.DeploymentConfig{Mode: "public", AdminTLSTerminatedByProxy: true},
		Autocert:        config.AutocertConfig{Domain: "bridge.example.com"},
		CustomEndpoints: []string{"https://bridge.example.com"},
	}
	got := pairAlternates("https://bridge.example.com", cfg)
	count := 0
	for _, u := range got {
		if u == "https://bridge.example.com" {
			count++
		}
		if u == "https://bridge.example.com:443" {
			t.Errorf("synthesized URL must omit :443; got %q", u)
		}
	}
	if count != 1 {
		t.Errorf("autocert + customEndpoint duplicate should dedupe to exactly 1; got %d alternates=%v", count, got)
	}
}

func TestEnsurePrimaryFirstHappyPathPassthrough(t *testing.T) {
	primary := "https://primary:7788"
	in := []string{primary, "https://b:7788", "https://c:7788"}
	got := ensurePrimaryFirst(primary, in)
	if len(got) != len(in) || got[0] != primary {
		t.Errorf("ensurePrimaryFirst pass-through changed slice: in=%v out=%v", in, got)
	}
}

func TestEnsurePrimaryFirstDedupsPrimaryEvenWhenAlreadyAtHead(t *testing.T) {
	// CodeRabbit round 2 on PR #101: a duplicate primary anywhere in
	// the input must collapse to a single primary at the head. The
	// pre-fix early-return on `alternates[0] == primary` let the
	// duplicate slip through unchanged.
	primary := "https://primary:7788"
	in := []string{primary, "https://b:7788", primary}
	got := ensurePrimaryFirst(primary, in)
	want := []string{primary, "https://b:7788"}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d (got=%v)", len(got), len(want), got)
	}
	for i, u := range want {
		if got[i] != u {
			t.Errorf("got[%d] = %q, want %q (full got=%v)", i, got[i], u, got)
		}
	}
	count := 0
	for _, u := range got {
		if u == primary {
			count++
		}
	}
	if count != 1 {
		t.Errorf("primary appeared %d times after dedup; want exactly once (got=%v)", count, got)
	}
}

func TestEnsurePrimaryFirstReordersWhenPrimaryNotHead(t *testing.T) {
	// CodeRabbit defence-in-depth: if a future helper change ever
	// returns the primary in a non-head position, the response-
	// boundary helper restores the contract.
	primary := "https://primary:7788"
	in := []string{"https://b:7788", primary, "https://c:7788"}
	got := ensurePrimaryFirst(primary, in)
	if got[0] != primary {
		t.Errorf("ensurePrimaryFirst did not move primary to head: %v", got)
	}
	count := 0
	for _, u := range got {
		if u == primary {
			count++
		}
	}
	if count != 1 {
		t.Errorf("primary appeared %d times after normalization; want exactly once", count)
	}
}

func TestEnsurePrimaryFirstPrependsWhenPrimaryMissing(t *testing.T) {
	primary := "https://primary:7788"
	in := []string{"https://b:7788", "https://c:7788"}
	got := ensurePrimaryFirst(primary, in)
	if got[0] != primary {
		t.Errorf("ensurePrimaryFirst did not prepend missing primary: %v", got)
	}
	if len(got) != len(in)+1 {
		t.Errorf("len = %d, want %d (one more than input)", len(got), len(in)+1)
	}
}

func TestEnsurePrimaryFirstEmptyInputYieldsPrimaryOnly(t *testing.T) {
	primary := "https://primary:7788"
	got := ensurePrimaryFirst(primary, nil)
	if len(got) != 1 || got[0] != primary {
		t.Errorf("ensurePrimaryFirst on nil input = %v, want [%q]", got, primary)
	}
}

// --- defaultBridgeURL: public mode prefers the public endpoint ---

func TestDefaultBridgeURLPublicModePrefersAutocertDomain(t *testing.T) {
	// THE fix (half 1): a public-mode bridge must pre-fill the dial URL
	// with the public domain the device can reach off-network, NOT a
	// `<hostname>.local` mDNS name that only resolves on the bridge's LAN.
	cfg := &config.Config{
		ListenAddress: "0.0.0.0:443",
		Deployment:    config.DeploymentConfig{Mode: "public"},
		Autocert:      config.AutocertConfig{Domain: "bridge.ars.md"},
	}
	got := defaultBridgeURL(cfg)
	if got != "https://bridge.ars.md" {
		t.Errorf("public-mode default URL = %q, want https://bridge.ars.md (no :443)", got)
	}
}

func TestDefaultBridgeURLPublicModeKeepsNonDefaultPort(t *testing.T) {
	cfg := &config.Config{
		ListenAddress: "0.0.0.0:8443",
		Deployment:    config.DeploymentConfig{Mode: "public"},
		Autocert:      config.AutocertConfig{Domain: "bridge.ars.md"},
	}
	if got := defaultBridgeURL(cfg); got != "https://bridge.ars.md:8443" {
		t.Errorf("public-mode default URL = %q, want https://bridge.ars.md:8443", got)
	}
}

func TestDefaultBridgeURLPublicModeFallsBackToCustomEndpoint(t *testing.T) {
	// No autocert domain (TLS-terminated by a reverse proxy) → use the
	// first declared customEndpoint rather than .local.
	cfg := &config.Config{
		ListenAddress:   "0.0.0.0:443",
		Deployment:      config.DeploymentConfig{Mode: "public", AdminTLSTerminatedByProxy: true},
		CustomEndpoints: []string{"https://bridge.ars.md", "https://alt.example"},
	}
	if got := defaultBridgeURL(cfg); got != "https://bridge.ars.md" {
		t.Errorf("public-mode default URL = %q, want first customEndpoint https://bridge.ars.md", got)
	}
}

func TestDefaultBridgeURLLoopbackUsesMDNS(t *testing.T) {
	// Loopback mode keeps the historical `https://<host>.local:<port>`
	// shape (exact host depends on os.Hostname, so assert the envelope).
	cfg := &config.Config{ListenAddress: "0.0.0.0:7788"}
	got := defaultBridgeURL(cfg)
	if !strings.HasPrefix(got, "https://") || !strings.HasSuffix(got, ":7788") {
		t.Errorf("loopback default URL = %q, want https://<host>:7788", got)
	}
	if strings.Contains(got, "bridge.ars.md") {
		t.Errorf("loopback default URL leaked a public domain: %q", got)
	}
}

// --- pairFingerprint: bake the cert the device will actually see ---

func TestPairFingerprintUsesResolverForPublicHost(t *testing.T) {
	// THE fix (half 2): the QR must carry the fingerprint the device
	// captures when it dials the URL — here the resolver's value, not
	// the self-signed fallback.
	resolve := func(host string) string {
		if host == "bridge.ars.md" {
			return "7E:E2:40:LE"
		}
		return ""
	}
	got := pairFingerprint("https://bridge.ars.md", "34:7E:SELF", resolve)
	if got != "7E:E2:40:LE" {
		t.Errorf("pairFingerprint = %q, want resolver value 7E:E2:40:LE", got)
	}
}

func TestPairFingerprintStripsPortBeforeResolving(t *testing.T) {
	resolve := func(host string) string {
		if host == "bridge.ars.md" {
			return "LE"
		}
		return "WRONG-" + host
	}
	if got := pairFingerprint("https://bridge.ars.md:8443", "SELF", resolve); got != "LE" {
		t.Errorf("pairFingerprint with port = %q, want LE (port stripped before resolve)", got)
	}
}

func TestPairFingerprintFallsBackWhenResolverNil(t *testing.T) {
	if got := pairFingerprint("https://bridge.ars.md", "SELF", nil); got != "SELF" {
		t.Errorf("pairFingerprint with nil resolver = %q, want self-signed SELF", got)
	}
}

func TestPairFingerprintFallsBackWhenResolverEmpty(t *testing.T) {
	resolve := func(string) string { return "" }
	if got := pairFingerprint("https://bridge.ars.md", "SELF", resolve); got != "SELF" {
		t.Errorf("pairFingerprint with empty resolve = %q, want self-signed SELF", got)
	}
}

func TestPairFingerprintFallsBackWhenURLHasNoHost(t *testing.T) {
	resolve := func(string) string { return "SHOULD-NOT-BE-USED" }
	if got := pairFingerprint("::not a url::", "SELF", resolve); got != "SELF" {
		t.Errorf("pairFingerprint on hostless URL = %q, want self-signed SELF", got)
	}
}

func TestPairURLHost(t *testing.T) {
	cases := map[string]string{
		"https://bridge.ars.md":        "bridge.ars.md",
		"https://bridge.ars.md:8443":   "bridge.ars.md",
		"https://1bitbridge.local:443": "1bitbridge.local",
		"https://192.168.0.14:7788":    "192.168.0.14",
		// Scheme-less operator input (admin dial-URL field) must still
		// resolve a host so the fingerprint doesn't fall back to
		// self-signed. (Gemini MEDIUM on PR #338.)
		"bridge.ars.md":      "bridge.ars.md",
		"bridge.ars.md:8443": "bridge.ars.md",
		"192.168.0.14:7788":  "192.168.0.14",
		"":                   "",
	}
	for in, want := range cases {
		if got := pairURLHost(in); got != want {
			t.Errorf("pairURLHost(%q) = %q, want %q", in, got, want)
		}
	}
}
