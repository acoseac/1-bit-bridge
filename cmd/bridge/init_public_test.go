package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/adminauth"
	"github.com/acoseac/1-bit-bridge/internal/config"
)

// TestInitPublicHappyPath drives `bridge init --public --yes` end-
// to-end and asserts the generated YAML carries every public-mode
// gate, the admin credentials are minted, and the banner output
// matches the expected shape.
func TestInitPublicHappyPath(t *testing.T) {
	tmp := t.TempDir()
	cfgDir := filepath.Join(tmp, "cfg")

	var stdout, stderr bytes.Buffer
	code := initCmd([]string{
		"--yes",
		"--no-service",
		"--skip-doctor",
		"--public",
		"--domain", "bridge.example.com",
		"--email", "ops@example.com",
		"--dir", cfgDir,
		"--name", "Public Library",
	}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("initCmd: code=%d stderr=%s stdout=%s", code, stderr.String(), stdout.String())
	}

	// Config file must reflect every public-mode default.
	cfgPath := filepath.Join(cfgDir, "bridge.yaml")
	loaded, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !loaded.IsPublic() {
		t.Error("loaded config is not public mode")
	}
	if loaded.Autocert.Domain != "bridge.example.com" {
		t.Errorf("Autocert.Domain = %q", loaded.Autocert.Domain)
	}
	if !loaded.Autocert.Enabled {
		t.Error("Autocert.Enabled should be true (no --admin-tls-proxy)")
	}
	if loaded.Autocert.Email != "ops@example.com" {
		t.Errorf("Autocert.Email = %q", loaded.Autocert.Email)
	}
	if loaded.ListenAddress != ":443" {
		t.Errorf("ListenAddress = %q, want :443", loaded.ListenAddress)
	}
	if loaded.Tailscale.Mode != string(config.TailscaleModeDisabled) {
		t.Errorf("Tailscale.Mode = %q, want disabled", loaded.Tailscale.Mode)
	}
	if loaded.MDNS.Enabled == nil || *loaded.MDNS.Enabled {
		t.Errorf("MDNS.Enabled should be explicit false")
	}
	if len(loaded.LibraryRoots) != 0 {
		t.Errorf("LibraryRoots should be empty in public init without --library; got %v", loaded.LibraryRoots)
	}
	wantEndpoint := "https://bridge.example.com"
	if len(loaded.CustomEndpoints) != 1 || loaded.CustomEndpoints[0] != wantEndpoint {
		t.Errorf("CustomEndpoints = %v, want [%q]", loaded.CustomEndpoints, wantEndpoint)
	}

	// adminauth.json must exist and carry the bcrypt hash.
	dataDir := filepath.Join(cfgDir, "data")
	adminAuthPath := filepath.Join(dataDir, "adminauth.json")
	if _, err := os.Stat(adminAuthPath); err != nil {
		t.Fatalf("adminauth.json: %v", err)
	}
	store, err := adminauth.OpenStore(adminAuthPath)
	if err != nil {
		t.Fatal(err)
	}
	if !store.IsInitialised() {
		t.Error("adminauth store should be initialised")
	}
	if store.Username() != "admin" {
		t.Errorf("Username = %q, want admin", store.Username())
	}

	// Banner output: credentials box must appear; fingerprint box
	// must NOT.
	out := stdout.String()
	if !strings.Contains(out, "Admin credentials") {
		t.Errorf("stdout missing admin credentials banner:\n%s", out)
	}
	if strings.Contains(out, "TLS fingerprint") {
		t.Errorf("public-mode init should suppress fingerprint banner; got:\n%s", out)
	}
}

// TestInitPublicFooterUsesDomainURL pins the post-PR-#296
// followup contract: the final "Admin console: ..." line printed
// by `bridge init` MUST use the public-mode-aware URL (derived
// from autocert.domain), NOT the historical
// `http://<adminAddress>/` shape. The bridge.yaml has
// `adminAddress: 0.0.0.0:7789` in default direct-TLS public
// mode — printing `http://0.0.0.0:7789/` to the operator is
// dial-broken from any browser on the VPS (0.0.0.0 isn't a
// dialable host on darwin/linux) AND wrong even from the local
// host (admin serves HTTPS in direct-TLS mode).
//
// Regression-locks the user-reported observation from the
// 2026-05-24 deployment to bridge.ars.md: "bridge init's final
// footer line `Admin console: http://0.0.0.0:7789/` is from
// init.go and missed the PR #296 banner fix".
func TestInitPublicFooterUsesDomainURL(t *testing.T) {
	tmp := t.TempDir()
	cfgDir := filepath.Join(tmp, "cfg")

	var stdout, stderr bytes.Buffer
	code := initCmd([]string{
		"--yes",
		"--no-service",
		"--skip-doctor",
		"--public",
		"--domain", "bridge.example.com",
		"--email", "ops@example.com",
		"--dir", cfgDir,
		"--name", "Public Library",
	}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("initCmd: code=%d stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	// Default direct-TLS public init binds admin on
	// 0.0.0.0:7789; the URL printed at the footer must be
	// https://bridge.example.com:7789/ (domain-derived). The
	// bind-address form must NOT appear in the footer.
	want := "Admin console: https://bridge.example.com:7789/"
	if !strings.Contains(out, want) {
		t.Errorf("init footer missing public-mode URL %q; full stdout:\n%s", want, out)
	}
	if strings.Contains(out, "http://0.0.0.0:7789/") {
		t.Errorf("init footer leaked bind-target URL http://0.0.0.0:7789/; full stdout:\n%s", out)
	}
}

// TestInitPublicProxyFooterUsesBareDomain pins the reverse-proxy
// branch of the same fix. In `--admin-tls-proxy` mode the admin
// listener serves plain HTTP on loopback (127.0.0.1:7789), but
// the FOOTER must print the canonical proxy-fronted URL
// `https://<domain>/` — the proxy maps it externally on its own
// port (often :443, sometimes :8443). Bridge can't know the
// external port; the bare-domain form is the right default.
func TestInitPublicProxyFooterUsesBareDomain(t *testing.T) {
	tmp := t.TempDir()
	cfgDir := filepath.Join(tmp, "cfg")

	var stdout, stderr bytes.Buffer
	code := initCmd([]string{
		"--yes",
		"--no-service",
		"--skip-doctor",
		"--public",
		"--admin-tls-proxy",
		"--domain", "bridge.example.com",
		"--dir", cfgDir,
		"--name", "Proxy Library",
	}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("initCmd: code=%d stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	want := "Admin console: https://bridge.example.com/"
	if !strings.Contains(out, want) {
		t.Errorf("proxy-mode init footer missing canonical URL %q; full stdout:\n%s", want, out)
	}
	if strings.Contains(out, "http://127.0.0.1:7789/") {
		t.Errorf("proxy-mode init footer leaked backend URL http://127.0.0.1:7789/; full stdout:\n%s", out)
	}
}

// TestInitLoopbackFooterUnchanged pins the regression-safety
// guarantee for the loopback path: the historical
// `http://<adminAddress>/` shape MUST survive the public-mode-
// aware refactor. Operators on existing single-host installs
// don't expect any UX change.
func TestInitLoopbackFooterUnchanged(t *testing.T) {
	tmp := t.TempDir()
	cfgDir := filepath.Join(tmp, "cfg")
	libRoot := filepath.Join(tmp, "music")
	if err := os.MkdirAll(libRoot, 0o755); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := initCmd([]string{
		"--yes",
		"--no-service",
		"--skip-doctor",
		"--dir", cfgDir,
		"--library", libRoot,
		"--name", "LAN Library",
	}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("initCmd: code=%d stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	// Default loopback admin address — exact shape preserved.
	wantPrefix := "Admin console: http://127.0.0.1:7789/"
	if !strings.Contains(out, wantPrefix) {
		t.Errorf("loopback init footer changed shape; want substring %q in:\n%s", wantPrefix, out)
	}
	if strings.Contains(out, "https://") {
		t.Errorf("loopback init footer leaked https:// — should be plain http for loopback installs:\n%s", out)
	}
}

// TestInitPublicRequiresDomain pins the gate: --public without
// --domain refuses with a clear error.
func TestInitPublicRequiresDomain(t *testing.T) {
	tmp := t.TempDir()
	cfgDir := filepath.Join(tmp, "cfg")
	var stdout, stderr bytes.Buffer
	code := initCmd([]string{
		"--yes", "--no-service", "--skip-doctor",
		"--public",
		"--dir", cfgDir,
	}, strings.NewReader(""), &stdout, &stderr)
	if code == 0 {
		t.Errorf("--public without --domain must fail; stdout=%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "--domain") {
		t.Errorf("stderr should mention --domain: %s", stderr.String())
	}
}

// TestInitPublicRequiresEmailUnlessProxy: ACME (autocert.enabled)
// registers an account under the operator-supplied email; --public
// without --email refuses unless --admin-tls-proxy is also set
// (proxy mode delegates ACME to the reverse proxy).
func TestInitPublicRequiresEmailUnlessProxy(t *testing.T) {
	tmp := t.TempDir()
	cfgDir := filepath.Join(tmp, "cfg")
	var stdout, stderr bytes.Buffer
	code := initCmd([]string{
		"--yes", "--no-service", "--skip-doctor",
		"--public",
		"--domain", "bridge.example.com",
		"--dir", cfgDir,
	}, strings.NewReader(""), &stdout, &stderr)
	if code == 0 {
		t.Errorf("--public without --email must fail when autocert handles TLS; stdout=%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "--email") {
		t.Errorf("stderr should mention --email: %s", stderr.String())
	}
}

// TestInitPublicWithProxyDefaultsToLoopback pins the CodeRabbit
// Major fix post-PR-#295: in --admin-tls-proxy mode the bridge
// MUST NOT serve plain-HTTP admin on all interfaces. The
// reverse proxy reaches the admin endpoint on loopback; binding
// 0.0.0.0 in proxy mode is an unsafe default that leaks
// credentials over the open internet if the firewall is misconfigured
// or the proxy is briefly down.
func TestInitPublicWithProxyDefaultsToLoopback(t *testing.T) {
	tmp := t.TempDir()
	cfgDir := filepath.Join(tmp, "cfg")
	var stdout, stderr bytes.Buffer
	code := initCmd([]string{
		"--yes", "--no-service", "--skip-doctor",
		"--public",
		"--domain", "bridge.example.com",
		"--admin-tls-proxy",
		"--dir", cfgDir,
	}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("init: code=%d stderr=%s", code, stderr.String())
	}
	cfg, err := config.Load(filepath.Join(cfgDir, "bridge.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AdminAddress != "127.0.0.1:7789" {
		t.Errorf("AdminAddress = %q, want 127.0.0.1:7789 in --admin-tls-proxy mode (safe loopback default)",
			cfg.AdminAddress)
	}
}

// TestInitPublicDirectTLSDefaultsToAllInterfaces: the inverse —
// autocert-direct-TLS mode wraps the admin listener in
// tls.NewListener via certManager, so binding 0.0.0.0 is safe
// (TLS is the trust boundary). The historical default.
func TestInitPublicDirectTLSDefaultsToAllInterfaces(t *testing.T) {
	tmp := t.TempDir()
	cfgDir := filepath.Join(tmp, "cfg")
	var stdout, stderr bytes.Buffer
	code := initCmd([]string{
		"--yes", "--no-service", "--skip-doctor",
		"--public",
		"--domain", "bridge.example.com",
		"--email", "ops@example.com",
		// No --admin-tls-proxy
		"--dir", cfgDir,
	}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("init: code=%d stderr=%s", code, stderr.String())
	}
	cfg, err := config.Load(filepath.Join(cfgDir, "bridge.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AdminAddress != "0.0.0.0:7789" {
		t.Errorf("AdminAddress = %q, want 0.0.0.0:7789 (direct-TLS mode — TLS is the trust boundary)",
			cfg.AdminAddress)
	}
}

// TestInitPublicAdminAddressOverrideWinsInBothModes: explicit
// --admin-address always wins over the posture-default.
func TestInitPublicAdminAddressOverrideWinsInBothModes(t *testing.T) {
	for _, proxyMode := range []bool{false, true} {
		tmp := t.TempDir()
		cfgDir := filepath.Join(tmp, "cfg")
		var stdout, stderr bytes.Buffer
		args := []string{
			"--yes", "--no-service", "--skip-doctor",
			"--public",
			"--domain", "bridge.example.com",
			"--admin-address", "10.0.0.5:8080",
			"--dir", cfgDir,
		}
		if proxyMode {
			args = append(args, "--admin-tls-proxy")
		} else {
			args = append(args, "--email", "ops@example.com")
		}
		code := initCmd(args, strings.NewReader(""), &stdout, &stderr)
		if code != 0 {
			t.Fatalf("proxy=%v: init code=%d stderr=%s", proxyMode, code, stderr.String())
		}
		cfg, err := config.Load(filepath.Join(cfgDir, "bridge.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.AdminAddress != "10.0.0.5:8080" {
			t.Errorf("proxy=%v: AdminAddress = %q, want 10.0.0.5:8080 (explicit --admin-address must win)",
				proxyMode, cfg.AdminAddress)
		}
	}
}

// TestInitPublicWithProxyAllowsNoEmail: --admin-tls-proxy is the
// escape hatch for operators delegating TLS to Caddy/nginx. The
// proxy handles ACME externally so --email isn't load-bearing.
func TestInitPublicWithProxyAllowsNoEmail(t *testing.T) {
	tmp := t.TempDir()
	cfgDir := filepath.Join(tmp, "cfg")
	var stdout, stderr bytes.Buffer
	code := initCmd([]string{
		"--yes", "--no-service", "--skip-doctor",
		"--public",
		"--domain", "bridge.example.com",
		"--admin-tls-proxy",
		"--dir", cfgDir,
	}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("--public --admin-tls-proxy should succeed without --email; code=%d stderr=%s", code, stderr.String())
	}
	cfg, err := config.Load(filepath.Join(cfgDir, "bridge.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Autocert.Enabled {
		t.Error("--admin-tls-proxy: Autocert.Enabled should be false (proxy owns ACME)")
	}
	if !cfg.Deployment.AdminTLSTerminatedByProxy {
		t.Error("--admin-tls-proxy: Deployment.AdminTLSTerminatedByProxy should be true")
	}
}
