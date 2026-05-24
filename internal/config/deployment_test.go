package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDeploymentEffectiveMode pins the typed-mode resolution: empty
// or whitespace defaults to "loopback"; "public" returns the typed
// constant; anything else surfaces as an error.
func TestDeploymentEffectiveMode(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want DeploymentMode
		ok   bool
	}{
		{"empty defaults to loopback", "", DeploymentModeLoopback, true},
		{"explicit loopback", "loopback", DeploymentModeLoopback, true},
		{"explicit public", "public", DeploymentModePublic, true},
		{"uppercase tolerated", "PUBLIC", DeploymentModePublic, true},
		{"whitespace trimmed", "  public  ", DeploymentModePublic, true},
		{"typo surfaces error", "puplic", "", false},
		{"unknown values rejected", "private", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DeploymentConfig{Mode: tc.in}.EffectiveMode()
			if tc.ok {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if got != tc.want {
					t.Errorf("got %q, want %q", got, tc.want)
				}
				return
			}
			if err == nil {
				t.Errorf("expected error for %q, got nil", tc.in)
			}
		})
	}
}

// TestConfigIsPublic pins the IsPublic helper: unknown values are
// treated as non-public (typo doesn't silently relax the loopback
// gate — Validate surfaces the typo separately).
func TestConfigIsPublic(t *testing.T) {
	cases := []struct {
		name string
		mode string
		want bool
	}{
		{"empty is loopback", "", false},
		{"loopback explicit", "loopback", false},
		{"public", "public", true},
		{"typo treated as non-public", "puplic", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &Config{Deployment: DeploymentConfig{Mode: tc.mode}}
			if got := c.IsPublic(); got != tc.want {
				t.Errorf("IsPublic() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestValidatePublicModeAllowsNonLoopbackAdmin pins the relaxation:
// in public mode the admin listener can bind a non-loopback address
// (the new adminauth layer becomes the trust boundary; auth is gated
// on IsPublic so loopback installs stay unauthenticated).
func TestValidatePublicModeAllowsNonLoopbackAdmin(t *testing.T) {
	dir := t.TempDir()
	for _, addr := range []string{"0.0.0.0:7789", "192.168.1.5:7789", "10.0.0.7:7789"} {
		cfg := &Config{
			LibraryRoots:    []string{dir},
			ListenAddress:   ":7788",
			AdminAddress:    addr,
			ScanIntervalSec: 3600,
			Deployment:      DeploymentConfig{Mode: "public", AdminTLSTerminatedByProxy: true},
			Autocert:        AutocertConfig{Domain: "bridge.example.com"},
		}
		if err := cfg.Validate(); err != nil {
			t.Errorf("public-mode Validate(%q): unexpected error %v", addr, err)
		}
	}
}

// TestValidatePublicModeRejectsMissingAutocertDomain pins the
// pairing-side prerequisite: iOS dials a specific hostname, and
// without a configured Autocert.Domain the Origin allowlist + the
// upcoming ACME wiring have no anchor to validate against.
func TestValidatePublicModeRejectsMissingAutocertDomain(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{
		LibraryRoots:    []string{dir},
		ListenAddress:   ":7788",
		AdminAddress:    "0.0.0.0:7789",
		ScanIntervalSec: 3600,
		Deployment:      DeploymentConfig{Mode: "public", AdminTLSTerminatedByProxy: true},
		// Autocert.Domain intentionally unset.
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "autocert.domain") {
		t.Errorf("error %q should mention autocert.domain", err.Error())
	}
}

// TestValidatePublicModeRejectsWithoutProxyFlag pins the PR 2
// scope: until PR 3 adds native ACME + tls.NewListener wrapping
// for the admin listener, public mode requires an external
// reverse proxy to terminate TLS. The bridge's session cookies
// are Secure-flagged in public mode; serving them over plain
// HTTP would silently break login (browsers refuse Secure
// cookies over HTTP).
func TestValidatePublicModeRejectsWithoutProxyFlag(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{
		LibraryRoots:    []string{dir},
		ListenAddress:   ":7788",
		AdminAddress:    "0.0.0.0:7789",
		ScanIntervalSec: 3600,
		Deployment:      DeploymentConfig{Mode: "public"},
		Autocert:        AutocertConfig{Domain: "bridge.example.com"},
		// AdminTLSTerminatedByProxy intentionally unset.
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "adminTLSTerminatedByProxy") {
		t.Errorf("error %q should mention adminTLSTerminatedByProxy", err.Error())
	}
}

// TestValidatePublicModeRejectsEmptyAdminAddress: a missing
// AdminAddress in public mode is a configuration error — there's
// nothing for the operator's browser to dial.
func TestValidatePublicModeRejectsEmptyAdminAddress(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{
		LibraryRoots:    []string{dir},
		ListenAddress:   ":7788",
		AdminAddress:    "",
		ScanIntervalSec: 3600,
		Deployment:      DeploymentConfig{Mode: "public"},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "adminAddress") {
		t.Errorf("error %q should mention adminAddress", err.Error())
	}
}

// TestValidateLoopbackModeStillRejectsNonLoopback: regression guard
// that the existing loopback-only invariant remains intact for
// loopback-mode installs (the historical default).
func TestValidateLoopbackModeStillRejectsNonLoopback(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{
		LibraryRoots:    []string{dir},
		ListenAddress:   ":7788",
		AdminAddress:    "0.0.0.0:7789",
		ScanIntervalSec: 3600,
		// Deployment.Mode unset → defaults to loopback.
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "loopback") {
		t.Errorf("error %q should mention loopback enforcement", err.Error())
	}
}

// TestValidatePublicModeAllowsEmptyLibraryRoots: operators on a VPS
// often boot before the FUSE mount is up; refusing to start would
// trap them in a chicken-and-egg loop.
func TestValidatePublicModeAllowsEmptyLibraryRoots(t *testing.T) {
	cfg := &Config{
		LibraryRoots:    nil,
		ListenAddress:   ":7788",
		AdminAddress:    "0.0.0.0:7789",
		ScanIntervalSec: 3600,
		Deployment: DeploymentConfig{
			Mode:                      "public",
			AdminTLSTerminatedByProxy: true,
		},
		Autocert: AutocertConfig{Domain: "bridge.example.com"},
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("public-mode empty libraryRoots: unexpected error %v", err)
	}
}

// TestValidateLoopbackModeStillRejectsEmptyLibraryRoots: regression
// guard that the historical default still requires at least one root.
func TestValidateLoopbackModeStillRejectsEmptyLibraryRoots(t *testing.T) {
	cfg := &Config{
		LibraryRoots:    nil,
		ListenAddress:   ":7788",
		AdminAddress:    "127.0.0.1:7789",
		ScanIntervalSec: 3600,
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "libraryRoots") {
		t.Errorf("error %q should mention libraryRoots", err.Error())
	}
}

// TestValidateDeploymentModeTypo: a malformed `mode:` value should
// surface as a Validate error at load time rather than silently
// falling through to loopback (where it would mask public-mode
// intent and lock the operator into a broken posture).
func TestValidateDeploymentModeTypo(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{
		LibraryRoots:    []string{dir},
		ListenAddress:   ":7788",
		AdminAddress:    "127.0.0.1:7789",
		ScanIntervalSec: 3600,
		Deployment:      DeploymentConfig{Mode: "puplic"},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "deployment.mode") {
		t.Errorf("error %q should mention deployment.mode", err.Error())
	}
}

// TestLoadPublicModeRejectsEmptyAdminAddress pins the end-to-end
// behaviour through Load() → applyDefaults() → Validate() for the
// realistic operator misconfiguration: `deployment.mode: public` is
// set in the YAML but `adminAddress` is omitted. CodeRabbit Major
// review on PR #289 caught that the bare Validate() test didn't
// exercise the Load() path — applyDefaults previously set
// AdminAddress to the loopback default unconditionally, so the
// "must not be empty in public mode" branch was unreachable
// through Load(). Fix: applyDefaults now skips the loopback
// default in public mode, surfacing the error here.
func TestLoadPublicModeRejectsEmptyAdminAddress(t *testing.T) {
	dir := t.TempDir()
	libRoot := filepath.Join(dir, "Music")
	if err := os.MkdirAll(libRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "bridge.yaml")
	yaml := "libraryRoots:\n  - " + libRoot + "\n" +
		"listenAddress: \"0.0.0.0:7788\"\n" +
		"deployment:\n  mode: public\n  adminTLSTerminatedByProxy: true\n" +
		"autocert:\n  domain: bridge.example.com\n"
	if err := os.WriteFile(configPath, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(configPath)
	if err == nil {
		t.Fatal("Load: expected error for public mode + omitted adminAddress, got nil")
	}
	if !strings.Contains(err.Error(), "adminAddress") {
		t.Errorf("Load: error %q should mention adminAddress", err.Error())
	}
}

// TestLoadLoopbackModeKeepsAdminAddressDefault: regression guard
// that the loopback-mode applyDefaults path still applies the
// historical "127.0.0.1:7789" default when adminAddress is
// omitted. Without this, a CLAUDE.md "operator just runs the
// bridge with a minimal YAML" flow would break.
func TestLoadLoopbackModeKeepsAdminAddressDefault(t *testing.T) {
	dir := t.TempDir()
	libRoot := filepath.Join(dir, "Music")
	if err := os.MkdirAll(libRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "bridge.yaml")
	yaml := "libraryRoots:\n  - " + libRoot + "\n"
	if err := os.WriteFile(configPath, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.AdminAddress != DefaultAdminAddress {
		t.Errorf("AdminAddress = %q, want default %q", cfg.AdminAddress, DefaultAdminAddress)
	}
}

// TestValidatePublicModeAutocertEnabledRelaxesProxyRequirement pins
// the PR 3 relaxation: in PR 2, public mode hard-required
// adminTLSTerminatedByProxy=true (because the bridge couldn't
// terminate TLS itself for the admin console yet). PR 3 adds the
// autocert path, so an operator with autocert.enabled=true (and
// the required Email + port-443 prerequisites) no longer needs the
// proxy flag — the bridge terminates TLS itself via the same SNI
// switcher that serves the public API.
func TestValidatePublicModeAutocertEnabledRelaxesProxyRequirement(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{
		LibraryRoots:    []string{dir},
		ListenAddress:   ":443",
		AdminAddress:    "0.0.0.0:7789",
		ScanIntervalSec: 3600,
		Deployment:      DeploymentConfig{Mode: "public"},
		Autocert: AutocertConfig{
			Enabled: true,
			Domain:  "bridge.example.com",
			Email:   "ops@example.com",
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("public + autocert.enabled (no proxy): unexpected error %v", err)
	}
}

// TestValidatePublicModeRejectsBothPathsDisabled pins the
// admin-TLS gate: in public mode, EITHER the bridge terminates
// TLS itself (autocert.enabled) OR the operator delegates to a
// reverse proxy. Both off is a misconfiguration that would leak
// session cookies + credentials cleartext.
func TestValidatePublicModeRejectsBothPathsDisabled(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{
		LibraryRoots:    []string{dir},
		ListenAddress:   ":7788",
		AdminAddress:    "0.0.0.0:7789",
		ScanIntervalSec: 3600,
		Deployment:      DeploymentConfig{Mode: "public"},
		Autocert:        AutocertConfig{Domain: "bridge.example.com"},
		// Neither AdminTLSTerminatedByProxy=true NOR Autocert.Enabled=true.
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "plain HTTP") {
		t.Errorf("error %q should mention plain HTTP", err.Error())
	}
}

// TestValidateAutocertEnabledRequiresEmail pins the ACME-side
// prerequisite: LE registers the account key against the
// operator-supplied email. Without it, autocert.Manager construction
// would still work but expiry-warning / revocation notices would
// have nowhere to go.
func TestValidateAutocertEnabledRequiresEmail(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{
		LibraryRoots:    []string{dir},
		ListenAddress:   ":443",
		AdminAddress:    "0.0.0.0:7789",
		ScanIntervalSec: 3600,
		Deployment:      DeploymentConfig{Mode: "public"},
		Autocert: AutocertConfig{
			Enabled: true,
			Domain:  "bridge.example.com",
			// Email intentionally unset.
		},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "autocert.email") {
		t.Errorf("error %q should mention autocert.email", err.Error())
	}
}

// TestValidateAutocertRequiresPort443OrExternalMapping pins the
// LE TLS-ALPN-01 constraint: the challenge validator connects on
// TCP/443 exclusively. The operator must either bind :443 directly
// (e.g. as root or with CAP_NET_BIND_SERVICE) OR run an external
// port-forward and confirm via the external443Mapping flag.
func TestValidateAutocertRequiresPort443OrExternalMapping(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{
		LibraryRoots:    []string{dir},
		ListenAddress:   ":7788", // NOT port 443
		AdminAddress:    "0.0.0.0:7789",
		ScanIntervalSec: 3600,
		Deployment:      DeploymentConfig{Mode: "public"},
		Autocert: AutocertConfig{
			Enabled: true,
			Domain:  "bridge.example.com",
			Email:   "ops@example.com",
			// External443Mapping intentionally unset.
		},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "443") {
		t.Errorf("error %q should mention port 443", err.Error())
	}

	// With external443Mapping=true, the same config passes.
	cfg.Autocert.External443Mapping = true
	if err := cfg.Validate(); err != nil {
		t.Errorf("external443Mapping=true: unexpected error %v", err)
	}
}

// TestListenAddrIsPort443Forms covers the realistic bind shapes.
func TestListenAddrIsPort443Forms(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{":443", true},
		{"0.0.0.0:443", true},
		{"[::]:443", true},
		{"192.0.2.10:443", true},
		{"[2001:db8::1]:443", true},
		{":7788", false},
		{"0.0.0.0:7788", false},
		{"", false},
		{"not-an-addr", false},
		{":4430", false}, // adjacent-port false-positive guard
	}
	for _, tc := range cases {
		if got := listenAddrIsPort443(tc.in); got != tc.want {
			t.Errorf("listenAddrIsPort443(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestEffectiveAutocertCacheDirDefault: when CacheDir is empty,
// defaults to <dataDir>/acme. When set, used verbatim.
//
// Path construction uses filepath.Join so the test is OS-agnostic
// — pre-fix the hardcoded "/srv/..." literals would fail on
// Windows even though the helper itself works correctly there
// (CodeRabbit Minor on PR #293).
func TestEffectiveAutocertCacheDirDefault(t *testing.T) {
	base := t.TempDir()
	cfg := &Config{DataDir: filepath.Join(base, "data")}
	if got, want := cfg.EffectiveAutocertCacheDir(), filepath.Join(cfg.DataDir, "acme"); got != want {
		t.Errorf("default: got %q, want %q", got, want)
	}
	cfg.Autocert.CacheDir = filepath.Join(base, "letsencrypt")
	if got, want := cfg.EffectiveAutocertCacheDir(), cfg.Autocert.CacheDir; got != want {
		t.Errorf("explicit: got %q, want %q", got, want)
	}
}

// TestEffectiveMDNSEnabledDefaultsByPosture pins the posture-
// keyed default: loopback defaults to true (home LAN needs
// auto-discovery), public defaults to false (no LAN to advertise
// on; TXT records would leak library metadata).
func TestEffectiveMDNSEnabledDefaultsByPosture(t *testing.T) {
	// Loopback default: true.
	cfg := &Config{}
	if !cfg.EffectiveMDNSEnabled() {
		t.Error("loopback default: EffectiveMDNSEnabled() = false, want true")
	}
	// Public default: false.
	cfg2 := &Config{Deployment: DeploymentConfig{Mode: "public"}}
	if cfg2.EffectiveMDNSEnabled() {
		t.Error("public default: EffectiveMDNSEnabled() = true, want false")
	}
	// Explicit operator override wins in both directions.
	tval := true
	fval := false
	cfg3 := &Config{MDNS: MDNSConfig{Enabled: &fval}}
	if cfg3.EffectiveMDNSEnabled() {
		t.Error("loopback + explicit false: got true, want false")
	}
	cfg4 := &Config{
		Deployment: DeploymentConfig{Mode: "public"},
		MDNS:       MDNSConfig{Enabled: &tval},
	}
	// Validate refuses the public+true combo separately; here we
	// just pin that EffectiveMDNSEnabled mirrors the explicit
	// value rather than overriding it.
	if !cfg4.EffectiveMDNSEnabled() {
		t.Error("public + explicit true: got false, want true (Validate refuses separately)")
	}
}

// TestValidatePublicModeRejectsExplicitMDNSTrue pins the
// security/correctness gate: explicit `mdns.enabled: true` in
// public mode is a misconfiguration.
func TestValidatePublicModeRejectsExplicitMDNSTrue(t *testing.T) {
	dir := t.TempDir()
	tval := true
	cfg := &Config{
		LibraryRoots:    []string{dir},
		ListenAddress:   ":7788",
		AdminAddress:    "0.0.0.0:7789",
		ScanIntervalSec: 3600,
		Deployment: DeploymentConfig{
			Mode:                      "public",
			AdminTLSTerminatedByProxy: true,
		},
		Autocert: AutocertConfig{Domain: "bridge.example.com"},
		MDNS:     MDNSConfig{Enabled: &tval},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "mdns.enabled") {
		t.Errorf("error %q should mention mdns.enabled", err.Error())
	}
}

// TestApplyDefaultsTailscaleDisabledInPublicMode pins the
// posture-keyed default: empty `tailscale.mode` in public mode
// resolves to "disabled" (not "cli") so the bridge doesn't
// fork-exec a missing `tailscale` CLI every 30 s on the
// /v1/health hot path.
func TestApplyDefaultsTailscaleDisabledInPublicMode(t *testing.T) {
	cfg := &Config{
		Deployment: DeploymentConfig{Mode: "public"},
	}
	cfg.applyDefaults()
	if cfg.Tailscale.Mode != string(TailscaleModeDisabled) {
		t.Errorf("public + empty tailscale.mode: got %q, want %q",
			cfg.Tailscale.Mode, TailscaleModeDisabled)
	}
}

// TestApplyDefaultsTailscaleCLIInLoopbackMode regression-guards
// the historical default: empty tailscale.mode in loopback mode
// must still resolve to "cli" (so home-LAN installs keep their
// implicit Tailscale auto-pilot behaviour).
func TestApplyDefaultsTailscaleCLIInLoopbackMode(t *testing.T) {
	cfg := &Config{}
	cfg.applyDefaults()
	// applyDefaults leaves Mode empty in loopback (the empty value
	// resolves to "cli" via EffectiveMode, NOT via applyDefaults).
	// Pin both shapes — Mode field stays empty AND
	// EffectiveMode returns CLI.
	if cfg.Tailscale.Mode != "" {
		t.Errorf("loopback + empty tailscale.mode: applyDefaults set %q, want empty",
			cfg.Tailscale.Mode)
	}
	mode, err := cfg.Tailscale.EffectiveMode()
	if err != nil {
		t.Fatal(err)
	}
	if mode != TailscaleModeCLI {
		t.Errorf("loopback: EffectiveMode = %q, want %q", mode, TailscaleModeCLI)
	}
}

// TestApplyDefaultsTailscaleExplicitOverrideUnchanged: an
// operator who explicitly sets `tailscale.mode: cli` in a
// public-mode bridge.yaml (legitimate — tailnet-routed public
// bridge) must NOT have it silently flipped to "disabled".
func TestApplyDefaultsTailscaleExplicitOverrideUnchanged(t *testing.T) {
	cfg := &Config{
		Deployment: DeploymentConfig{Mode: "public"},
		Tailscale:  TailscaleConfig{Mode: "cli"},
	}
	cfg.applyDefaults()
	if cfg.Tailscale.Mode != "cli" {
		t.Errorf("explicit tailscale.mode=cli: got %q, want cli", cfg.Tailscale.Mode)
	}
}

// TestLoadResolvesAutocertCacheDirRelative pins the
// resolvePaths contract: a relative `autocert.cacheDir` in YAML
// must resolve against the config file's directory (matching the
// existing behaviour for libraryRoots / dataDir / TLS paths).
// Pre-fix the relative path would have been used verbatim and
// resolved against process CWD — different across operator
// shells (CodeRabbit Major on PR #293).
func TestLoadResolvesAutocertCacheDirRelative(t *testing.T) {
	dir := t.TempDir()
	libRoot := filepath.Join(dir, "Music")
	if err := os.MkdirAll(libRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "bridge.yaml")
	yaml := "libraryRoots:\n  - " + libRoot + "\n" +
		"listenAddress: \":443\"\n" +
		"adminAddress: \"0.0.0.0:7789\"\n" +
		"deployment:\n  mode: public\n" +
		"autocert:\n  enabled: true\n  domain: bridge.example.com\n" +
		"  email: ops@example.com\n  cacheDir: \"./acme-cache\"\n"
	if err := os.WriteFile(configPath, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := filepath.Join(dir, "acme-cache")
	if cfg.Autocert.CacheDir != want {
		t.Errorf("Autocert.CacheDir = %q, want %q (must resolve against config-dir, not CWD)",
			cfg.Autocert.CacheDir, want)
	}
}
