package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/advertise"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/doctor"
)

func TestAContainerHostnameLeavesTheWantSetAndTheDoctor(t *testing.T) {
	dir := t.TempDir()
	lib := filepath.Join(dir, "Music")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "bridge.yaml")
	body := "libraryRoots:\n  - " + lib + "\nlistenAddress: \":7788\"\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("generated", func(t *testing.T) {
		probeAdvertisedHost(t, cfg, "0123456789ab", "", true)
		c := findCheck(t, doctor.Run(context.Background(), buildDoctorDeps(cfgPath)), "pairing-url")
		if c.Status != doctor.Warn || c.Hint != advertise.PairingNeedsEndpoint {
			t.Fatalf("pairing-url: status %v summary %q hint %q", c.Status, c.Summary, c.Hint)
		}
	})
	t.Run("chosen", func(t *testing.T) {
		probeAdvertisedHost(t, cfg, "musicbox", "musicbox", true)
		c := findCheck(t, doctor.Run(context.Background(), buildDoctorDeps(cfgPath)), "pairing-url")
		if c.Status != doctor.OK || c.Summary != "pairing QR dials https://musicbox.local:7788" {
			t.Fatalf("pairing-url: status %v summary %q hint %q", c.Status, c.Summary, c.Hint)
		}
	})
	t.Run("outside", func(t *testing.T) {
		probeAdvertisedHost(t, cfg, "0123456789ab", "0123456789ab", false)
	})
}

func probeAdvertisedHost(t *testing.T, cfg *config.Config, host, want string, inside bool) {
	t.Helper()
	restore := advertise.SetHostProbeForTest(
		func() (string, error) { return host, nil },
		func() bool { return inside },
	)
	t.Cleanup(restore)
	if opts := certSANOptions(cfg); opts.Hostname != want {
		t.Fatalf("Hostname = %q", opts.Hostname)
	}
}
