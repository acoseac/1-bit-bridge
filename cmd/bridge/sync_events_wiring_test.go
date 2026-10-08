package main

import (
	"crypto/tls"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/api"
)

// TestSyncEventsFollowTheScanHealthReports pins the production constructor.
// /v1/health reports a scan only while it is running and not stalled, and
// library.changed must suppress on that same value.
func TestSyncEventsFollowTheScanHealthReports(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	if strings.Count(body, "NewSyncEventPublisher(") != 1 {
		t.Fatalf("NewSyncEventPublisher calls = %d, want 1", strings.Count(body, "NewSyncEventPublisher("))
	}
	idx := strings.Index(body, "NewSyncEventPublisher(")
	window := body[max(0, idx-500):idx]
	if !strings.Contains(window, "if !cfg.Demo.Enabled") {
		t.Fatal("the publisher is constructed outside the demo guard")
	}
	call := body[idx:]
	if nl := strings.IndexByte(call, '\n'); nl >= 0 {
		call = call[:nl]
	}
	if !strings.Contains(call, "scanner.AdvertisedScanning(time.Now())") {
		t.Fatalf("publisher wiring %q, want the scan health reports", call)
	}
}

func TestADemoServeAdvertisesNoSyncEvents(t *testing.T) {
	dir := t.TempDir()
	lib := filepath.Join(dir, "Music")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "bridge.yaml")
	body := "libraryRoots:\n  - " + lib + "\nadminAddress: 127.0.0.1:0\ndemo:\n  enabled: true\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	br := bootServe(t, "--config", cfgPath, "--addr", "127.0.0.1:0")
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	resp, err := client.Get("https://" + br.addr + "/v1/health")
	if err != nil {
		t.Fatalf("health: %v\n%s", err, br.stderr.String())
	}
	defer resp.Body.Close()
	var health api.HealthResponse
	if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
		t.Fatal(err)
	}
	for _, f := range health.Features {
		if f == "syncEvents" {
			t.Fatal("a demo serve advertised syncEvents")
		}
	}
}
