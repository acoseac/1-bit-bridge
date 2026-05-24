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
