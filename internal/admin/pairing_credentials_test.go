package admin

import (
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/config"
)

// TestPublicPairingCarriesNoCustomEndpointCredential builds a public
// bridge's pairing QR, the primary url= and the urls= alternates, from
// customEndpoints that carry a secret in each part of a URL that can carry
// one, after the Normalize every served config goes through (backlog B54).
// A public QR reads customEndpoints directly (pairAlternates' public
// branch), and its PRIMARY is the declared endpoint for the autocert host
// (defaultBridgeURL), the url= every shipped app dials first: the declared
// endpoint must still be that primary, without the part, and no URL in the
// QR may carry the secret. A loopback QR reads /v1/health's enumeration,
// which serve's boot test covers.
func TestPublicPairingCarriesNoCustomEndpointCredential(t *testing.T) {
	const secret = "s3cret-Pw"
	for _, tc := range []struct{ name, declared, primary string }{
		{"a password", "https://user:" + secret + "@tenant.example.test:8443", "https://tenant.example.test:8443"},
		{"a token as the user name", "https://" + secret + "@tenant.example.test", "https://tenant.example.test:443"},
		{"a query", "https://tenant.example.test:8443/?token=" + secret, "https://tenant.example.test:8443/"},
		{"a fragment", "https://tenant.example.test:8443/#" + secret, "https://tenant.example.test:8443/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{
				ListenAddress: "127.0.0.1:20001",
				Deployment:    config.DeploymentConfig{Mode: "public", AdminTLSTerminatedByProxy: true},
				Autocert:      config.AutocertConfig{Domain: "tenant.example.test"},
				CustomEndpoints: []string{
					tc.declared,
					"https://user:" + secret + "@alt.example.test:9443/?k=" + secret,
				},
			}
			if err := cfg.Normalize(); err != nil {
				t.Fatal(err)
			}
			primary := defaultBridgeURL(cfg)
			if primary != tc.primary {
				t.Errorf("pairing primary = %q, want %q: the declared endpoint, without its credential", primary, tc.primary)
			}
			qr := append([]string{primary}, pairAlternates(primary, cfg, nil)...)
			for _, u := range qr {
				if strings.Contains(strings.ToLower(u), strings.ToLower(secret)) {
					t.Errorf("the pairing QR carries the secret in %q (all: %q)", u, qr)
				}
			}
			if !containsURL(qr, "https://alt.example.test:9443/") {
				t.Errorf("the QR lost the alternate endpoint instead of its credential: %q", qr)
			}
		})
	}
}
