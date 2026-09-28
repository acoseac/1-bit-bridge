package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/transcode"
)

// TestServeReportsTheOrphanSweepRefusalOnTheJobsCard boots the real serve
// with the background orphan sweep on, over the lost-index shape (2 rows
// with their files, 40 stranded files no row names), and reads the Jobs
// card's payload. The sweep refuses its boot tick; /api/jobs must say so.
//
// It pins the one line no package test can see, `OrphanSweepStatus:
// orphanSweepStatus` in runServe's admin.Deps: without it every test in
// internal/admin and internal/integrity stays green while the chip reads
// "on" over a sweep that refuses every tick, which is the defect.
func TestServeReportsTheOrphanSweepRefusalOnTheJobsCard(t *testing.T) {
	dir := t.TempDir()
	lib := filepath.Join(dir, "Music")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	dataDir, variantsDir := filepath.Join(dir, "data"), filepath.Join(dir, "variants")
	seedLostIndex(t, dataDir, variantsDir, 2, 40)

	adminPort := freeLoopbackPort(t)
	cfgPath := filepath.Join(dir, "bridge.yaml")
	body := fmt.Sprintf("libraryRoots:\n  - %s\ndataDir: %s\nadminAddress: 127.0.0.1:%d\n"+
		"upscale:\n    variantsDir: %s\nintegrity:\n    orphanSidecarSweepIntervalSec: 3600\n",
		lib, dataDir, adminPort, variantsDir)
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	stdout, stderr := &safeBuffer{}, &safeBuffer{}
	done := make(chan int, 1)
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		done <- run(ctx, []string{"serve", "--config", cfgPath, "--addr", "127.0.0.1:0"}, stdout, stderr)
	}()
	drainServeOnCleanup(t, cancel, exited, done, stderr)
	waitForListening(t, stdout, 30*time.Second)
	adminAddr := fmt.Sprintf("127.0.0.1:%d", adminPort)
	waitForAdminReady(t, adminAddr, done, stderr)

	client := &http.Client{Timeout: 10 * time.Second}
	var last map[string]any
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		last = jobsMaintenanceOf(t, ctx, client, "http://"+adminAddr+"/api/jobs")
		if last["orphanSidecarGCRefusal"] != nil {
			break
		}
	}
	if last["orphanSidecarGC"] != true || last["orphanSidecarGCRefusal"] != "massOrphans" {
		t.Fatalf("the Jobs payload does not carry the sweep's refusal: %v\nstderr: %s", last, stderr.String())
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

// seedLostIndex writes, under dataDir's manifest, `rows` variant rows each
// with its file under variantsDir, and `stranded` sidecar files no row
// references: the shape a lost index leaves.
func seedLostIndex(t *testing.T, dataDir, variantsDir string, rows, stranded int) {
	t.Helper()
	store, err := manifest.OpenStore(manifest.DefaultDBPath(dataDir))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	ctx := context.Background()
	const variant = "upscaled-v2-176400-24"
	for i := 0; i < rows; i++ {
		source := fmt.Sprintf("Artist/Kept/%02d.flac", i)
		if err := store.UpsertTrack(ctx, &manifest.Track{Path: source, Size: 100, ModTime: time.Now()}); err != nil {
			t.Fatal(err)
		}
		p := transcode.VariantSidecarPath(variantsDir, source, variant)
		writeFixtureFile(t, p, 50)
		if err := store.UpsertVariant(ctx, manifest.VariantRow{
			SourcePath: source, VariantID: variant, SidecarPath: p, Format: "flac",
			SampleRate: 176400, BitsPerSample: 24, SizeBytes: 50, SourceMTimeNS: 1, SourceSize: 100,
		}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < stranded; i++ {
		writeFixtureFile(t, transcode.VariantSidecarPath(variantsDir, fmt.Sprintf("Artist/Stranded %d/%02d.flac", i%3, i), variant), 1000)
	}
	// Older than the sweep's grace, as a stranded tree is.
	old := time.Now().Add(-time.Hour)
	if err := filepath.WalkDir(variantsDir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		return os.Chtimes(p, old, old)
	}); err != nil {
		t.Fatal(err)
	}
}

// jobsMaintenanceOf GETs the admin's /api/jobs and returns its maintenance
// object as decoded JSON, so a key's absence stays visible.
func jobsMaintenanceOf(t *testing.T, ctx context.Context, client *http.Client, url string) map[string]any {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
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
