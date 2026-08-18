package config

import (
	"strings"
	"testing"
)

// Demo block validation: a well-formed SHA-256 hex digest passes, a
// malformed one is rejected at load time — regardless of demo.enabled,
// so the typo is caught when it lands in the file, not when the
// operator eventually flips enabled.
func TestValidate_DemoTokenSHA256(t *testing.T) {
	base := func() *Config {
		return &Config{LibraryRoots: []string{"/nonexistent"}, ListenAddress: ":7788", AdminAddress: "127.0.0.1:7789", ScanIntervalSec: 3600}
	}
	if err := base().Validate(); err != nil {
		t.Fatalf("base config should validate, got %v", err)
	}
	valid := strings.Repeat("ab", 32) // 64 hex chars

	t.Run("valid hash, enabled", func(t *testing.T) {
		cfg := base()
		cfg.Demo.Enabled = true
		cfg.Demo.TokenSHA256 = valid
		if err := cfg.Validate(); err != nil {
			t.Fatalf("expected valid, got %v", err)
		}
	})
	t.Run("enabled without a token is allowed", func(t *testing.T) {
		cfg := base()
		cfg.Demo.Enabled = true
		if err := cfg.Validate(); err != nil {
			t.Fatalf("expected valid (pair-minted tokens only), got %v", err)
		}
	})
	for name, bad := range map[string]string{
		"too short":     valid[:63],
		"too long":      valid + "a",
		"non-hex chars": strings.Repeat("zz", 32),
	} {
		t.Run(name, func(t *testing.T) {
			cfg := base()
			cfg.Demo.TokenSHA256 = bad
			if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "demo.tokenSHA256") {
				t.Fatalf("expected demo.tokenSHA256 error, got %v", err)
			}
		})
	}
}
