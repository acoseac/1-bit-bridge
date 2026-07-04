package main

import (
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/config"
)

// TestBanneradminURLShapes pins the operator-facing URL the
// public-mode startup banner prints. CodeRabbit Major review
// post-PR-#295: pre-fix the banner printed cfg.AdminAddress
// (a bind target like 0.0.0.0:7789 or 127.0.0.1:7789), which
// is meaningless to anyone browsing from elsewhere. Now the
// URL is derived from cfg.Autocert.Domain + the admin port.
func TestBanneradminURLShapes(t *testing.T) {
	cases := []struct {
		name         string
		domain       string
		adminAddress string
		scheme       string
		want         string
	}{
		{
			name:         "https + non-:443 admin port",
			domain:       "bridge.example.com",
			adminAddress: "0.0.0.0:7789",
			scheme:       "https",
			want:         "https://bridge.example.com:7789/",
		},
		{
			name:         "https + :443 admin port omits port",
			domain:       "bridge.example.com",
			adminAddress: "0.0.0.0:443",
			scheme:       "https",
			want:         "https://bridge.example.com/",
		},
		{
			name:         "loopback bind doesn't leak into URL — domain wins",
			domain:       "bridge.example.com",
			adminAddress: "127.0.0.1:7789",
			scheme:       "https",
			want:         "https://bridge.example.com:7789/",
		},
		{
			name:         "specific-iface bind doesn't leak",
			domain:       "bridge.example.com",
			adminAddress: "192.168.1.5:7789",
			scheme:       "https",
			want:         "https://bridge.example.com:7789/",
		},
		{
			name:         "trailing dot stripped from domain (non-:443 path)",
			domain:       "bridge.example.com.",
			adminAddress: "0.0.0.0:7789",
			scheme:       "https",
			want:         "https://bridge.example.com:7789/",
		},
		{
			name:         "trailing dot stripped from domain (:443 path — bare-host shape)",
			domain:       "bridge.example.com.",
			adminAddress: "0.0.0.0:443",
			scheme:       "https",
			want:         "https://bridge.example.com/",
		},
		{
			name:         "trailing dot stripped from domain (malformed addr fallback path)",
			domain:       "bridge.example.com.",
			adminAddress: "not-an-addr",
			scheme:       "https",
			want:         "https://bridge.example.com/",
		},
		{
			name:         "empty domain falls back to bind address (test wiring)",
			domain:       "",
			adminAddress: "127.0.0.1:7789",
			scheme:       "https",
			want:         "https://127.0.0.1:7789/",
		},
		{
			name:         "empty domain + empty scheme still gets https default in fallback",
			domain:       "",
			adminAddress: "127.0.0.1:7789",
			scheme:       "",
			want:         "https://127.0.0.1:7789/",
		},
		{
			name:         "dot-only domain normalizes to empty + falls back",
			domain:       ".",
			adminAddress: "127.0.0.1:7789",
			scheme:       "https",
			want:         "https://127.0.0.1:7789/",
		},
		{
			name:         "empty scheme defaults to https",
			domain:       "bridge.example.com",
			adminAddress: "0.0.0.0:7789",
			scheme:       "",
			want:         "https://bridge.example.com:7789/",
		},
		{
			name:         "malformed adminAddress falls back to bare domain",
			domain:       "bridge.example.com",
			adminAddress: "not-an-addr",
			scheme:       "https",
			want:         "https://bridge.example.com/",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := banneradminURL(tc.domain, tc.adminAddress, tc.scheme)
			if got != tc.want {
				t.Errorf("banneradminURL(%q, %q, %q) = %q, want %q",
					tc.domain, tc.adminAddress, tc.scheme, got, tc.want)
			}
		})
	}
}

// TestOperatorAdminURLShapesAcrossPostures pins the three-branch
// helper that drives BOTH the `bridge serve` startup banner AND
// the `bridge init` completion footer. Pre-fix init.go hardcoded
// `http://<adminAddress>/` (e.g. `http://0.0.0.0:7789/`) which
// is dial-broken from any browser on a fresh public-mode VPS
// install. Post-fix both surfaces converge on the same URL for
// the same posture via this helper.
func TestOperatorAdminURLShapesAcrossPostures(t *testing.T) {
	publicAutocert := &config.Config{
		AdminAddress: "0.0.0.0:7789",
		Deployment:   config.DeploymentConfig{Mode: string(config.DeploymentModePublic)},
		Autocert:     config.AutocertConfig{Enabled: true, Domain: "bridge.example.com"},
	}
	publicAutocertOn443 := &config.Config{
		AdminAddress: "0.0.0.0:443",
		Deployment:   config.DeploymentConfig{Mode: string(config.DeploymentModePublic)},
		Autocert:     config.AutocertConfig{Enabled: true, Domain: "bridge.example.com"},
	}
	publicProxy := &config.Config{
		AdminAddress: "127.0.0.1:7789",
		Deployment: config.DeploymentConfig{
			Mode:                      string(config.DeploymentModePublic),
			AdminTLSTerminatedByProxy: true,
		},
		Autocert: config.AutocertConfig{Domain: "bridge.example.com"},
	}
	publicProxyNonStandardBackend := &config.Config{
		// Operator chose an alternative loopback port for the
		// reverse-proxy backend. The PRINTED URL must still be
		// `https://<domain>/` — the bridge can't know how the
		// proxy maps that backend externally.
		AdminAddress: "127.0.0.1:9999",
		Deployment: config.DeploymentConfig{
			Mode:                      string(config.DeploymentModePublic),
			AdminTLSTerminatedByProxy: true,
		},
		Autocert: config.AutocertConfig{Domain: "bridge.example.com"},
	}
	publicProxyTrailingDot := &config.Config{
		AdminAddress: "127.0.0.1:7789",
		Deployment: config.DeploymentConfig{
			Mode:                      string(config.DeploymentModePublic),
			AdminTLSTerminatedByProxy: true,
		},
		Autocert: config.AutocertConfig{Domain: "bridge.example.com."},
	}
	loopback := &config.Config{
		AdminAddress: "127.0.0.1:7789",
	}
	loopbackNoScheme := &config.Config{
		AdminAddress: "127.0.0.1:7789",
	}

	cases := []struct {
		name        string
		cfg         *config.Config
		adminScheme string
		want        string
	}{
		{"public autocert direct-TLS non-:443", publicAutocert, "https", "https://bridge.example.com:7789/"},
		// init.go's footer passes "http" as the loopback default;
		// in public direct-TLS mode the helper must IGNORE that
		// override and force https — otherwise the operator sees
		// `http://bridge.example.com:7789/` and dials plain HTTP
		// against an https-terminating listener.
		{"public autocert direct-TLS: caller-supplied http is ignored", publicAutocert, "http", "https://bridge.example.com:7789/"},
		{"public autocert direct-TLS: caller-supplied empty also forced to https", publicAutocert, "", "https://bridge.example.com:7789/"},
		{"public autocert direct-TLS :443 omits port", publicAutocertOn443, "https", "https://bridge.example.com/"},
		{"public reverse-proxy: scheme-aware https + bare domain", publicProxy, "http", "https://bridge.example.com/"},
		{"public reverse-proxy: non-standard backend doesn't leak port", publicProxyNonStandardBackend, "http", "https://bridge.example.com/"},
		{"public reverse-proxy: trailing dot stripped", publicProxyTrailingDot, "http", "https://bridge.example.com/"},
		{"loopback: historical http://addr/ shape", loopback, "http", "http://127.0.0.1:7789/"},
		{"loopback: empty adminScheme defaults to http", loopbackNoScheme, "", "http://127.0.0.1:7789/"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := operatorAdminURL(tc.cfg, tc.adminScheme)
			if got != tc.want {
				t.Errorf("operatorAdminURL = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestOperatorAdminURLNilConfigReturnsBrowseableFallback — `bridge
// init` calls the helper with a possibly-nil *config.Config
// (config.Load can fail post-write on a malformed YAML); helper
// must not panic AND must return a complete URL the operator can
// type into a browser, NOT a bare `http://` that would land in
// printAdmin's stdout line. CodeRabbit Minor + Gemini Medium on
// PR #297.
func TestOperatorAdminURLNilConfigReturnsBrowseableFallback(t *testing.T) {
	got := operatorAdminURL(nil, "http")
	want := "http://" + config.DefaultAdminAddress + "/"
	if got != want {
		t.Errorf("operatorAdminURL(nil, http) = %q, want %q", got, want)
	}
	// Empty scheme defaults to http on the nil-cfg path: with no cfg we
	// fall back to the loopback DefaultAdminAddress, which is served over
	// plain http. (Matches the docblock and the cfg!=nil loopback branch;
	// an https default here would print an unreachable https://127.0.0.1.)
	got = operatorAdminURL(nil, "")
	wantEmpty := "http://" + config.DefaultAdminAddress + "/"
	if got != wantEmpty {
		t.Errorf("operatorAdminURL(nil, empty) = %q, want %q", got, wantEmpty)
	}
}

// TestOperatorAdminURLDefensiveFallbacks pins the empty-field
// recovery contract Gemini Medium asked for on PR #297:
// validators should reject these inputs at load time, but the
// helper composes a browseable URL anyway so any path that
// bypasses validation (raced init, future caller) doesn't leak
// `https:///` into the operator's terminal.
func TestOperatorAdminURLDefensiveFallbacks(t *testing.T) {
	defaultAddr := config.DefaultAdminAddress

	cases := []struct {
		name string
		cfg  *config.Config
		want string
	}{
		{
			name: "public proxy with empty domain falls back to bind address",
			cfg: &config.Config{
				AdminAddress: "127.0.0.1:7789",
				Deployment: config.DeploymentConfig{
					Mode:                      string(config.DeploymentModePublic),
					AdminTLSTerminatedByProxy: true,
				},
				// Autocert.Domain deliberately empty.
			},
			want: "https://127.0.0.1:7789/",
		},
		{
			name: "public proxy with empty domain AND empty AdminAddress falls back to DefaultAdminAddress",
			cfg: &config.Config{
				Deployment: config.DeploymentConfig{
					Mode:                      string(config.DeploymentModePublic),
					AdminTLSTerminatedByProxy: true,
				},
			},
			want: "https://" + defaultAddr + "/",
		},
		{
			name: "public direct-TLS with empty domain AND empty AdminAddress falls back to DefaultAdminAddress",
			cfg: &config.Config{
				Deployment: config.DeploymentConfig{Mode: string(config.DeploymentModePublic)},
				Autocert:   config.AutocertConfig{Enabled: true},
			},
			want: "https://" + defaultAddr + "/",
		},
		{
			name: "loopback with empty AdminAddress falls back to DefaultAdminAddress",
			cfg:  &config.Config{},
			want: "http://" + defaultAddr + "/",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := operatorAdminURL(tc.cfg, "")
			if got != tc.want {
				t.Errorf("operatorAdminURL = %q, want %q", got, tc.want)
			}
			// Sanity: result must NOT contain the degenerate
			// `:///` shape — that's what the defensive fallback
			// is guarding against.
			if strings.Contains(got, ":///") {
				t.Errorf("degenerate URL with empty host: %q", got)
			}
		})
	}
}
