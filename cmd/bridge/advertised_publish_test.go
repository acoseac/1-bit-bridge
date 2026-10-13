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
		restore := advertise.SetHostProbeForTest(
			func() (string, error) { return "0123456789ab", nil },
			func() bool { return true },
		)
		t.Cleanup(restore)
		if opts := certSANOptions(cfg); opts.Hostname != "" {
			t.Fatalf("Hostname = %q", opts.Hostname)
		}
		c := findCheck(t, doctor.Run(context.Background(), buildDoctorDeps(cfgPath)), "pairing-url")
		if c.Status != doctor.Warn || c.Hint != advertise.PairingNeedsEndpoint {
			t.Fatalf("pairing-url: status %v summary %q hint %q", c.Status, c.Summary, c.Hint)
		}
	})
	t.Run("chosen", func(t *testing.T) {
		restore := advertise.SetHostProbeForTest(
			func() (string, error) { return "musicbox", nil },
			func() bool { return true },
		)
		t.Cleanup(restore)
		if opts := certSANOptions(cfg); opts.Hostname != "musicbox" {
			t.Fatalf("Hostname = %q", opts.Hostname)
		}
		c := findCheck(t, doctor.Run(context.Background(), buildDoctorDeps(cfgPath)), "pairing-url")
		if c.Status != doctor.OK || c.Summary != "pairing QR dials https://musicbox.local:7788" {
			t.Fatalf("pairing-url: status %v summary %q hint %q", c.Status, c.Summary, c.Hint)
		}
	})
	t.Run("outside", func(t *testing.T) {
		restore := advertise.SetHostProbeForTest(
			func() (string, error) { return "0123456789ab", nil },
			func() bool { return false },
		)
		t.Cleanup(restore)
		if opts := certSANOptions(cfg); opts.Hostname != "0123456789ab" {
			t.Fatalf("Hostname = %q", opts.Hostname)
		}
	})
}
