package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/auth"
	"github.com/acoseac/1-bit-bridge/internal/config"
)

// TestHealthPublishesNoCustomEndpointCredential asks /v1/health, with no
// token, for the endpoint list of a bridge whose customEndpoints carry a
// secret in each part of a URL that can carry one, in both postures
// (backlog B54; the path since B66). The config goes through Normalize, as
// every config the bridge serves from does (Load, and every writer), and
// nothing on the way out strips anything: what keeps the secret off the
// wire is that the list every enumeration reads is published without those
// parts. Each endpoint is still advertised, to the host and port it names;
// the phone relies on none of those parts, and dropping the endpoint would
// cost it a route. The body is searched whole, not just the endpoints field.
func TestHealthPublishesNoCustomEndpointCredential(t *testing.T) {
	const secret = "s3cret-Pw"
	declared := []string{
		"https://user:" + secret + "@a.example.test:7788",
		"https://" + secret + "@b.example.test:7788",
		"https://c.example.test:7788/?token=" + secret,
		"https://d.example.test:7788/#" + secret,
		"https://e.example.test:7788/" + secret + "/",
	}
	published := []string{
		"https://a.example.test:7788",
		"https://b.example.test:7788",
		"https://c.example.test:7788/",
		"https://d.example.test:7788/",
		"https://e.example.test:7788",
	}
	for _, public := range []bool{false, true} {
		name := "loopback"
		if public {
			name = "public"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			lib := filepath.Join(dir, "Music")
			if err := os.MkdirAll(lib, 0o755); err != nil {
				t.Fatal(err)
			}
			cfg := &config.Config{
				LibraryRoots:    []string{lib},
				ListenAddress:   ":7788",
				LibraryName:     "Test Library",
				CustomEndpoints: slices.Clone(declared),
			}
			cfg.Tailscale.Mode = "disabled"
			if public {
				cfg.Deployment = config.DeploymentConfig{Mode: "public", AdminTLSTerminatedByProxy: true}
				cfg.Autocert = config.AutocertConfig{Domain: "bridge.example.test"}
			}
			if err := cfg.Normalize(); err != nil {
				t.Fatal(err)
			}
			store, err := auth.OpenStore(filepath.Join(dir, "tokens.json"))
			if err != nil {
				t.Fatal(err)
			}
			hs := httptest.NewServer(New(cfg, store, nil, "AB:CD").Handler())
			t.Cleanup(hs.Close)

			resp, err := http.Get(hs.URL + "/v1/health")
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("GET /v1/health = %d: %s", resp.StatusCode, body)
			}
			if strings.Contains(strings.ToLower(string(body)), strings.ToLower(secret)) {
				t.Errorf("/v1/health, asked with no token, carries the secret:\n%s", body)
			}
			for _, want := range published {
				if !strings.Contains(string(body), `"`+want+`"`) {
					t.Errorf("/v1/health does not advertise %s, the endpoint without its credential:\n%s", want, body)
				}
			}
		})
	}
}

// TestHealthPublishesNoAutocertDomainCredential asks /v1/health, with no
// token, for the endpoints of a public bridge whose autocert.domain an
// operator edited by hand (backlog B66). Public mode builds an endpoint from
// the domain as a string, `https://<domain>:<listen port>`, and published
// `https://user:password@host:7788` whole until B66. The config goes through
// Normalize, as every config the bridge serves from does, and the endpoint
// is the host it names; one that names none is the placeholder, which
// resolves nowhere, and the body carries nothing of what was written.
func TestHealthPublishesNoAutocertDomainCredential(t *testing.T) {
	const secret = "s3cret-Pw"
	for _, tc := range []struct{ name, domain, published string }{
		{"a password", "user:" + secret + "@bridge.example.test", "https://bridge.example.test:7788"},
		{"a token as the user name, a scheme, a port and a path",
			"https://" + secret + "@bridge.example.test:8443/" + secret, "https://bridge.example.test:7788"},
		{"a query and a fragment", "bridge.example.test?k=" + secret + "#" + secret, "https://bridge.example.test:7788"},
		{"no host", "user:" + secret + "@", "https://" + config.InvalidAutocertDomain + ":7788"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			lib := filepath.Join(dir, "Music")
			if err := os.MkdirAll(lib, 0o755); err != nil {
				t.Fatal(err)
			}
			cfg := &config.Config{
				LibraryRoots:  []string{lib},
				ListenAddress: ":7788",
				LibraryName:   "Test Library",
				Deployment:    config.DeploymentConfig{Mode: "public", AdminTLSTerminatedByProxy: true},
				Autocert:      config.AutocertConfig{Domain: tc.domain},
			}
			cfg.Tailscale.Mode = "disabled"
			if err := cfg.Normalize(); err != nil {
				t.Fatal(err)
			}
			store, err := auth.OpenStore(filepath.Join(dir, "tokens.json"))
			if err != nil {
				t.Fatal(err)
			}
			hs := httptest.NewServer(New(cfg, store, nil, "AB:CD").Handler())
			t.Cleanup(hs.Close)

			resp, err := http.Get(hs.URL + "/v1/health")
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("GET /v1/health = %d: %s", resp.StatusCode, body)
			}
			if strings.Contains(strings.ToLower(string(body)), strings.ToLower(secret)) {
				t.Errorf("/v1/health, asked with no token, carries the secret:\n%s", body)
			}
			if !strings.Contains(string(body), `"`+tc.published+`"`) {
				t.Errorf("/v1/health does not advertise %s:\n%s", tc.published, body)
			}
		})
	}
}
