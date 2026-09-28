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
// (backlog B54). The config goes through Normalize, as every config the
// bridge serves from does (Load, and every writer), and nothing on the way
// out strips anything: what keeps the secret off the wire is that the list
// every enumeration reads is published without those parts. Each endpoint
// is still advertised, to the host and port it names; the phone uses none
// of those parts, and dropping the endpoint would cost it a route. The
// body is searched whole, not just the endpoints field.
func TestHealthPublishesNoCustomEndpointCredential(t *testing.T) {
	const secret = "s3cret-Pw"
	declared := []string{
		"https://user:" + secret + "@a.example.test:7788",
		"https://" + secret + "@b.example.test:7788",
		"https://c.example.test:7788/?token=" + secret,
		"https://d.example.test:7788/#" + secret,
	}
	published := []string{
		"https://a.example.test:7788",
		"https://b.example.test:7788",
		"https://c.example.test:7788/",
		"https://d.example.test:7788/",
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
