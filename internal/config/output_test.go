package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOutputOnlyConfigHasNoRoots(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bridge.yaml")
	body := "listenAddress: \":7788\"\nscanIntervalSec: 3600\ndataDir: " + filepath.Join(dir, "data") + "\noutput:\n  mpd: on\n  endpoint: 127.0.0.1:6600\n  password: \"s3cret-value\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Output.Mode() != "on" || cfg.Output.Endpoint != "127.0.0.1:6600" || cfg.Output.Password != "s3cret-value" {
		t.Fatalf("output = %#v", cfg.Output)
	}
	if len(cfg.LibraryRoots) != 0 {
		t.Fatalf("roots = %v", cfg.LibraryRoots)
	}
	if errs := cfg.CheckLibraryRootsAccessible(); len(errs) != 0 {
		t.Fatalf("accessibility: %v", errs)
	}
	if strings.Contains(errString(cfg.Validate()), "s3cret-value") {
		t.Fatal("validate named the password")
	}
}

func TestOutputModeRejectsAnythingElse(t *testing.T) {
	cfg := &Config{ListenAddress: ":7788", AdminAddress: "127.0.0.1:7789", ScanIntervalSec: 3600, LibraryRoots: []string{"/music"}, Output: OutputConfig{MPD: "yes", Password: "s3cret-value"}}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "output.mpd") {
		t.Fatalf("validate: %v", err)
	}
	if strings.Contains(err.Error(), "s3cret-value") {
		t.Fatal("the error names the password")
	}
	for _, mode := range []string{"", "auto", "AUTO", " on ", "on", "off"} {
		cfg.Output.MPD = mode
		want := "auto"
		if mode == " on " || mode == "on" {
			want = "on"
		} else if mode == "off" {
			want = "off"
		}
		if cfg.Output.Mode() != want {
			t.Fatalf("mode %q -> %q", mode, cfg.Output.Mode())
		}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("validate %q: %v", mode, err)
		}
	}
}

func TestAutoStillRequiresARoot(t *testing.T) {
	cfg := &Config{ListenAddress: ":7788", ScanIntervalSec: 3600, Output: OutputConfig{MPD: "auto"}}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "libraryRoots") {
		t.Fatalf("auto with no roots: %v", err)
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
