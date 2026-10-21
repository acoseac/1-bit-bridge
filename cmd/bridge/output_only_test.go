package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/doctor"
)

// TestOutputOnlyBridgeStarts is a bridge with output.mpd on and no library
// root. It has to get past the root gate and answer on the console: that
// is the install that reports its player and serves no tracks.
func TestOutputOnlyBridgeStarts(t *testing.T) {
	root := t.TempDir()
	lan := holdLoopback(t)
	admin := holdLoopback(t)
	dataDir := filepath.Join(root, "data")
	configPath := filepath.Join(root, "bridge.yaml")
	yamlText := "dataDir: " + dataDir + "\nadminAddress: " + admin.addr + "\noutput:\n  mpd: on\n"
	if err := os.WriteFile(configPath, []byte(yamlText), 0o600); err != nil {
		t.Fatal(err)
	}
	opts := serveOpts{
		configPath:    configPath,
		addrOverride:  lan.addr,
		lanListener:   lan.ln,
		adminListener: admin.ln,
	}
	b := launchServe(t, func(ctx context.Context, stdout, stderr io.Writer) int {
		return runServe(ctx, opts, stdout, stderr)
	})
	waitForAdminReady(t, admin.addr, b.done, b.stderr)
}

// TestDoctorPassesWithNoRoots is the same install graded by bridge doctor.
// An empty library is not a failure once output.mpd is on; auto still is,
// because the config does not load. On Linux the report carries the player
// checklist too, whose Music line reads Ready for a box with no library;
// its Player and DAC lines grade this host's MPD and sound cards, which a
// test host need not have, so they are not about the config and a fail
// there is not this test's.
func TestDoctorPassesWithNoRoots(t *testing.T) {
	dir := t.TempDir()
	data := filepath.Join(dir, "data")
	if err := os.MkdirAll(data, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "bridge.yaml")
	body := "listenAddress: \"127.0.0.1:0\"\nadminAddress: \"127.0.0.1:0\"\ndataDir: " + data + "\noutput:\n  mpd: on\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	d := buildDoctorDeps(path)
	if d.ConfigFile == nil || d.ConfigFile.LoadErr != nil {
		t.Fatalf("config did not load: %+v", d.ConfigFile)
	}
	if len(d.LibraryRoots) != 0 {
		t.Fatalf("roots = %v", d.LibraryRoots)
	}
	report := doctor.Run(context.Background(), d)
	for _, c := range report.Checks {
		if c.Name != "config-file" && c.Name != "library-roots" {
			continue
		}
		if c.Status != doctor.OK {
			t.Errorf("%s = %s (%s)", c.Name, c.Status, c.Summary)
		}
	}
	for _, c := range report.Checks {
		if c.Status == doctor.Fail && c.Name != "player" && c.Name != "dac" {
			t.Errorf("fail %s: %s", c.Name, c.Summary)
		}
		if c.Name == "music" && (c.Status != doctor.OK || c.Summary != "Ready: can play music from another bridge or NAS") {
			t.Errorf("music %s %q, want ok and Ready", c.Status, c.Summary)
		}
	}
}
