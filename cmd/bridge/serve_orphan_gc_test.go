package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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
			variantsDir := requireServeReportsARefusal(t, "orphanSidecarSweepIntervalSec: 3600",
				func(dataDir, variantsDir string) { seedVariantCatalog(t, dataDir, variantsDir, c.rows, 40) },
				"orphanSidecarGC", "orphanSidecarGCRefusal", c.want)
			for i := 0; i < 40; i++ {
				if _, err := os.Stat(transcode.VariantSidecarPath(variantsDir, fmt.Sprintf("Artist/Stranded %d/%02d.flac", i%3, i), "upscaled-v2-176400-24")); err != nil {
					t.Fatalf("a refusing sweep unlinked a stranded file: %v", err)
				}
			}
		})
	}
}

// TestServeReportsTheVariantWatcherRefusalOnTheJobsCard boots the real
// serve with the variant integrity watcher on and reads the Jobs card's
// payload over its two refusals (backlog B131): a variants directory that
// reads as unmounted (12 rows, the directory empty), and a relocation (12
// rows whose sidecars are gone, while the directory still holds one that no
// row names). The watcher refuses its boot tick in each; /api/jobs must say
// which. Like the orphan sweep's test, it pins the wiring line no package
// test can see: `VariantSweepStatus: variantSweepStatus` in runServe.
func TestServeReportsTheVariantWatcherRefusalOnTheJobsCard(t *testing.T) {
	for _, c := range []struct {
		name     string
		stranded int
		want     string
	}{
		{"an unmounted variants directory", 0, "variantsDirUnavailable"},
		{"a relocation", 1, "relocation"},
	} {
		t.Run(c.name, func(t *testing.T) {
			requireServeReportsARefusal(t, "variantSweepIntervalSec: 3600",
				func(dataDir, variantsDir string) {
					seedVariantCatalog(t, dataDir, variantsDir, 12, c.stranded)
					// The rows' sidecars go, the stranded ones stay.
					for i := 0; i < 12; i++ {
						p := transcode.VariantSidecarPath(variantsDir, fmt.Sprintf("Artist/Kept/%02d.flac", i), "upscaled-v2-176400-24")
						if err := os.Remove(p); err != nil {
							t.Fatal(err)
						}
					}
					if c.stranded == 0 {
						// Nothing left but empty folders: the mountpoint an
						// unmount leaves is an empty directory.
						if err := os.RemoveAll(variantsDir); err != nil {
							t.Fatal(err)
						}
						if err := os.MkdirAll(variantsDir, 0o755); err != nil {
							t.Fatal(err)
						}
					}
				},
				"variantIntegrityActive", "variantIntegrityRefusal", c.want)
		})
	}
}

// requireServeReportsARefusal boots serve over the catalog seed makes, with
// the integrity setting integrityYAML, and requires /api/jobs to carry the
// refusal kind want under kindKey, beside runningKey true, with a start. It
// returns the variants directory.
func requireServeReportsARefusal(t *testing.T, integrityYAML string, seed func(dataDir, variantsDir string),
	runningKey, kindKey, want string) string {
	t.Helper()
	dir := t.TempDir()
	lib := filepath.Join(dir, "Music")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	dataDir, variantsDir := filepath.Join(dir, "data"), filepath.Join(dir, "variants")
	seed(dataDir, variantsDir)

	admin := holdLoopback(t)
	cfgPath := filepath.Join(dir, "bridge.yaml")
	body := fmt.Sprintf("libraryRoots:\n  - %s\ndataDir: %s\nadminAddress: %s\n"+
		"upscale:\n    variantsDir: %s\nintegrity:\n    %s\n",
		lib, dataDir, admin.addr, variantsDir, integrityYAML)
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	served := launchServe(t, func(ctx context.Context, stdout, stderr io.Writer) int {
		return runServe(ctx, serveOpts{
			configPath: cfgPath, addrOverride: "127.0.0.1:0", adminListener: admin.ln,
		}, stdout, stderr)
	})
	adminAddr := admin.addr
	waitForAdminReady(t, adminAddr, served.done, served.stderr)

	client := &http.Client{Timeout: 10 * time.Second}
	var last map[string]any
	// Until serve's waits give up (serveGiveUp), not a fixed bound: the
	// sweep's boot tick walks the tree and lists the catalog, which a
	// starved runner can take longer than any number chosen here (B63).
	giveUp := serveGiveUpTime(t)
	for {
		last = jobsMaintenanceOf(t, client, "http://"+adminAddr+"/api/jobs")
		if last[kindKey] != nil || (!giveUp.IsZero() && time.Now().After(giveUp)) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if last[runningKey] != true || last[kindKey] != want {
		t.Fatalf("the Jobs payload does not carry the sweep's %s refusal: %v\nstderr: %s\nserve's goroutines:\n%s",
			want, last, served.stderr.String(), serveStacks())
	}
	sinceKey := kindKey[:len(kindKey)-len("Refusal")] + "RefusingSince"
	if since, _ := last[sinceKey].(string); since == "" {
		t.Errorf("the refusal carries no start (%s): %v", sinceKey, last)
	}
	return variantsDir
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
