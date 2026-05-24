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
