package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/transcode"
)

// TestServeReportsTheOrphanSweepRefusalOnTheJobsCard boots the real serve
// with the background orphan sweep on and reads the Jobs card's payload,
// over two shapes: the lost index (2 rows with their files, 40 stranded
// files no row names) and the empty catalog (no row, the same 40 files).
// The sweep refuses its boot tick in each; /api/jobs must say which.
//
// It pins the one line no package test can see, `OrphanSweepStatus:
// orphanSweepStatus` in runServe's admin.Deps: without it every test in
// internal/admin and internal/integrity stays green while the chip reads
// "on" over a sweep that refuses every tick, which is the defect. The
// empty catalog is the refusal that was not on the card at all until
// 2026-09-28: it WARNed on every tick, and the chip said "on".
func TestServeReportsTheOrphanSweepRefusalOnTheJobsCard(t *testing.T) {
	for _, c := range []struct {
		name string
		rows int
		want string
	}{
		{"a lost index", 2, "massOrphans"},
		{"an empty catalog", 0, "emptyCatalog"},
	} {
		t.Run(c.name, func(t *testing.T) {
			requireServeReportsTheOrphanSweepRefusal(t, c.rows, c.want)
		})
	}
}

// requireServeReportsTheOrphanSweepRefusal boots serve over rows variant
// rows with their files and 40 stranded files no row names, and requires
// /api/jobs to carry the refusal kind want, and every stranded file to
// survive.
func requireServeReportsTheOrphanSweepRefusal(t *testing.T, rows int, want string) {
	t.Helper()
	dir := t.TempDir()
	lib := filepath.Join(dir, "Music")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	dataDir, variantsDir := filepath.Join(dir, "data"), filepath.Join(dir, "variants")
	seedVariantCatalog(t, dataDir, variantsDir, rows, 40)

	adminPort := freeLoopbackPort(t)
	cfgPath := filepath.Join(dir, "bridge.yaml")
	body := fmt.Sprintf("libraryRoots:\n  - %s\ndataDir: %s\nadminAddress: 127.0.0.1:%d\n"+
		"upscale:\n    variantsDir: %s\nintegrity:\n    orphanSidecarSweepIntervalSec: 3600\n",
		lib, dataDir, adminPort, variantsDir)
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	served := bootServe(t, "--config", cfgPath, "--addr", "127.0.0.1:0")
	adminAddr := fmt.Sprintf("127.0.0.1:%d", adminPort)
	waitForAdminReady(t, adminAddr, served.done, served.stderr)

	client := &http.Client{Timeout: 10 * time.Second}
	var last map[string]any
	// Until serve's waits give up (serveGiveUp), not a fixed bound: the
	// sweep's boot tick walks the tree and lists the catalog, which a
	// starved runner can take longer than any number chosen here (B63).
	giveUp := serveGiveUpTime(t)
	for {
		last = jobsMaintenanceOf(t, client, "http://"+adminAddr+"/api/jobs")
		if last["orphanSidecarGCRefusal"] != nil || (!giveUp.IsZero() && time.Now().After(giveUp)) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if last["orphanSidecarGC"] != true || last["orphanSidecarGCRefusal"] != want {
		t.Fatalf("the Jobs payload does not carry the sweep's %s refusal: %v\nstderr: %s\nserve's goroutines:\n%s",
			want, last, served.stderr.String(), serveStacks())
	}
	if since, _ := last["orphanSidecarGCRefusingSince"].(string); since == "" {
		t.Errorf("the refusal carries no start: %v", last)
	}
	for i := 0; i < 40; i++ {
		if _, err := os.Stat(transcode.VariantSidecarPath(variantsDir, fmt.Sprintf("Artist/Stranded %d/%02d.flac", i%3, i), "upscaled-v2-176400-24")); err != nil {
			t.Fatalf("a refusing sweep unlinked a stranded file: %v", err)
		}
	}
}

// jobsMaintenanceOf GETs the admin's /api/jobs and returns its maintenance
// object as decoded JSON, so a key's absence stays visible.
func jobsMaintenanceOf(t *testing.T, client *http.Client, url string) map[string]any {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	var got struct {
		Maintenance map[string]any `json:"maintenance"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode %s: %v", url, err)
	}
	return got.Maintenance
}
