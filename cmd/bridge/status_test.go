package main

import (
	"bytes"
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestStatusServiceNotRunning pins: `bridge status` against an
// admin address that nothing is bound to surfaces the "not
// running" hint and returns exit code 1. The CLI should NOT
// crash or hang — the 5s probe timeout caps the wait.
func TestStatusServiceNotRunning(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "bridge.yaml")
	// Bind a listener to claim a port, then close it so the port
	// is free for the test (avoids accidentally hitting another
	// process). Use the closed port in cfg.AdminAddress so the
	// connect attempt deterministically fails.
	lis := httptest.NewServer(nil)
	addr := lis.Listener.Addr().String()
	lis.Close()

	yaml := "libraryRoots:\n  - " + dir + "\n" +
		"adminAddress: \"" + addr + "\"\n" +
		"listenAddress: \":7788\"\n" +
		"dataDir: " + yamlStr(filepath.Join(dir, "data")) + "\n"
	if err := os.WriteFile(cfg, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "data"), 0o755); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr strings.Builder
	code := statusCmd(context.Background(), []string{"-config", cfg}, &stdout, &stderr)
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "not running") {
		t.Errorf("missing 'not running' hint, stderr = %q", stderr.String())
	}
}

// TestStatusJSONFlag pins: --json route gates on the JSON branch
// (we can't easily exercise a fake admin server here without
// duplicating handlers; the test scope is "the flag is wired and
// not-running still surfaces").
func TestStatusJSONFlagSurfacesNotRunning(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "bridge.yaml")
	lis := httptest.NewServer(nil)
	addr := lis.Listener.Addr().String()
	lis.Close()
	yaml := "libraryRoots:\n  - " + dir + "\n" +
		"adminAddress: \"" + addr + "\"\n" +
		"listenAddress: \":7788\"\n" +
		"dataDir: " + yamlStr(filepath.Join(dir, "data")) + "\n"
	if err := os.WriteFile(cfg, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "data"), 0o755); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr strings.Builder
	code := statusCmd(context.Background(), []string{"-config", cfg, "-json"}, &stdout, &stderr)
	if code != 1 {
		t.Errorf("exit = %d, want 1 (not running, no JSON to emit)", code)
	}
}

// TestStatusNamesUnreadableTracksOnlyWhenThereAreSome — the CLI's one
// view of the decode-refusal count.
//
// `/api/stats` has carried `tracksUnreadable` since #947 and the human
// `bridge status` never printed it, so a headless operator — which is
// every VPS install — had no surface at all for "some of my files are
// broken" short of opening an SSH tunnel to the console.
//
// Hidden at zero, deliberately: a permanent `Unreadable: 0` is a row
// that trains the reader to skip the block, which is the same reason
// the dashboard banner has a `hidden` attribute.
func TestStatusNamesUnreadableTracksOnlyWhenThereAreSome(t *testing.T) {
	base := map[string]any{
		"libraryName":   "Fixture",
		"serverVersion": "0.0.0-test",
		"tracksIndexed": float64(10),
	}
	var quiet bytes.Buffer
	writeStatusHuman(&quiet, base, nil)
	if strings.Contains(quiet.String(), "Unreadable") {
		t.Errorf("a bridge with no decode refusals prints an Unreadable row:\n%s", quiet.String())
	}

	// Absent, as an older bridge serves it — also silent, never "0".
	delete(base, "tracksUnreadable")
	var absent bytes.Buffer
	writeStatusHuman(&absent, base, nil)
	if strings.Contains(absent.String(), "Unreadable") {
		t.Errorf("a payload with no tracksUnreadable key prints the row:\n%s", absent.String())
	}

	// The number arrives as a float64 — every JSON number does, through
	// the map[string]any the probe decodes into. An int type switch here
	// would match nothing and the row would never appear at all.
	base["tracksUnreadable"] = float64(3)
	var loud bytes.Buffer
	writeStatusHuman(&loud, base, nil)
	out := loud.String()
	if !strings.Contains(out, "Unreadable:") || !strings.Contains(out, "3 tracks") {
		t.Errorf("status does not name the 3 unreadable tracks:\n%s", out)
	}
	if strings.Contains(out, "3.0") || strings.Contains(out, "+e") {
		t.Errorf("the count rendered as a float:\n%s", out)
	}

	base["tracksUnreadable"] = float64(1)
	var one bytes.Buffer
	writeStatusHuman(&one, base, nil)
	if !strings.Contains(one.String(), "1 track the decoder") {
		t.Errorf("the singular is not used for one track:\n%s", one.String())
	}
}
