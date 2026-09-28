package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/api"
)

// TestServeProjectionFollowsTheLiveUpscaleGate boots the real serve and
// switches upscaling on and off through PATCH /api/settings, with no
// restart, asking two console surfaces after every step.
//
// runServe built admin.Deps.ProjectedSize and AvailableDiskSpace as
// closures called once, at construction, that answered nil when
// `upscale.enabled` was false at that moment, and the projection handler
// reads nil as "feature off". So a bridge booted with upscaling off
// answered 503 `upscale-disabled` to GET /api/library/browse-projection
// after the PATCH that switched it on and reported `live`, and one booted
// with it on went on projecting after the PATCH that switched it off,
// while /v1/health said the opposite each time. No internal/admin test
// can see it: every one of them builds its own Deps.
//
// After every step the projection must answer as /v1/health's
// upscaleEnabled says, 200 when true and 503 `upscale-disabled` when
// false. The live gate also needs a usable sox, and on a host without one
// health reads false after the PATCH that switches the feature on, so the
// boot-off leg could not tell a live gate from a boot snapshot. A
// stand-in sox therefore goes first on PATH (POSIX only), and health must
// then read what was PATCHed, which is checked before the comparison.
//
// The variants-dir readout reads the same AvailableDiskSpace closure, so
// on a bridge booted with upscaling off the console's "Free on that
// volume" read 0 bytes whatever the disk held. Free space is a fact
// about the disk, not about upscaling, so it must be reported in every
// state.
func TestServeProjectionFollowsTheLiveUpscaleGate(t *testing.T) {
	stubbedSox := putUsableSoxOnPath(t)
	for _, bootOn := range []bool{false, true} {
		t.Run(fmt.Sprintf("booted with upscale.enabled=%t", bootOn), func(t *testing.T) {
			b := startConsoleBridge(t, fmt.Sprintf("upscale:\n  enabled: %t\n", bootOn), nil)
			for step, on := range []bool{bootOn, !bootOn, bootOn} {
				if step > 0 {
					patchUpscaleEnabled(t, b.console, b.adminBase, on, b.stderr)
				}
				healthOn := healthUpscaleEnabled(t, b.phone, b.apiBase)
				if stubbedSox && healthOn != on {
					t.Fatalf("step %d: /v1/health says upscaleEnabled=%t with the flag at %t and a usable "+
						"sox on PATH, so the comparison below would not show what it is meant to; stderr=%s",
						step, healthOn, on, b.stderr.String())
				}
				code, errCode := projectionVerdict(t, b.console, b.adminBase)
				switch {
				case healthOn && code != http.StatusOK:
					t.Errorf("step %d (upscale.enabled=%t): /v1/health says upscaling is on, and the "+
						"console's projection answered %d %q, not 200", step, on, code, errCode)
				case !healthOn && (code != http.StatusServiceUnavailable || errCode != "upscale-disabled"):
					t.Errorf("step %d (upscale.enabled=%t): /v1/health says upscaling is off, and the "+
						"console's projection answered %d %q, not 503 \"upscale-disabled\"", step, on, code, errCode)
				}
				if free := variantsDirFreeBytes(t, b.console, b.adminBase); free <= 0 {
					t.Errorf("step %d (upscale.enabled=%t): GET /api/upscale/variants-dir reports "+
						"freeBytes=%d for a volume this test is writing to", step, on, free)
				}
			}
		})
	}
}

// putUsableSoxOnPath puts a stand-in `sox` first on PATH, one that
// answers `sox --help` with a format list naming flac, and reports
// whether it did. Nothing in the test above decodes anything (its library
// is empty), so the stand-in is asked only the probe's question. Not on
// Windows, where a shell script is no command; the test runs there with
// whatever PATH holds.
func putUsableSoxOnPath(t *testing.T) bool {
	t.Helper()
	if runtime.GOOS == "windows" {
		return false
	}
	bin := t.TempDir()
	script := "#!/bin/sh\nprintf 'sox:      SoX v14.4.2\\n\\nAUDIO FILE FORMATS: flac wav\\n'\n"
	if err := os.WriteFile(filepath.Join(bin, "sox"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return true
}

// patchUpscaleEnabled switches upscaling through the console's settings
// PATCH and requires the report to call the change live.
func patchUpscaleEnabled(t *testing.T, client *http.Client, adminBase string, on bool, stderr *safeBuffer) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPatch, adminBase+"/api/settings",
		strings.NewReader(fmt.Sprintf(`{"upscaleEnabled":%t}`, on)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("PATCH /api/settings: %v; stderr=%s", err, stderr.String())
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH /api/settings {upscaleEnabled:%t} = %d: %s", on, resp.StatusCode, raw)
	}
	var report struct {
		Fields map[string]struct {
			Status string `json:"status"`
		} `json:"fields"`
	}
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatalf("decode the settings report: %v: %s", err, raw)
	}
	if got := report.Fields["upscaleEnabled"].Status; got != "live" {
		t.Fatalf("the settings report calls upscaleEnabled %q, want \"live\": %s", got, raw)
	}
}

// healthUpscaleEnabled is /v1/health's upscaleEnabled, as a phone reads
// it (no bearer).
func healthUpscaleEnabled(t *testing.T, client *http.Client, apiBase string) bool {
	t.Helper()
	resp, err := client.Get(apiBase + "/v1/health")
	if err != nil {
		t.Fatalf("GET /v1/health: %v", err)
	}
	defer resp.Body.Close()
	var health api.HealthResponse
	if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
		t.Fatalf("decode /v1/health: %v", err)
	}
	if health.UpscaleEnabled == nil {
		t.Fatal("/v1/health carries no upscaleEnabled")
	}
	return *health.UpscaleEnabled
}

// projectionVerdict asks the console's projection endpoint about the whole
// library and returns its status and, for a refusal, the error code.
func projectionVerdict(t *testing.T, client *http.Client, adminBase string) (int, string) {
	t.Helper()
	resp, err := client.Get(adminBase + "/api/library/browse-projection?path=")
	if err != nil {
		t.Fatalf("GET /api/library/browse-projection: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusOK {
		return resp.StatusCode, ""
	}
	var envelope struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("projection refusal %d is not the JSON error envelope: %v: %s", resp.StatusCode, err, raw)
	}
	return resp.StatusCode, envelope.Error
}

// variantsDirFreeBytes is the free space GET /api/upscale/variants-dir
// reports for the variants volume.
func variantsDirFreeBytes(t *testing.T, client *http.Client, adminBase string) int64 {
	t.Helper()
	resp, err := client.Get(adminBase + "/api/upscale/variants-dir")
	if err != nil {
		t.Fatalf("GET /api/upscale/variants-dir: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/upscale/variants-dir = %d: %s", resp.StatusCode, raw)
	}
	var out struct {
		FreeBytes int64 `json:"freeBytes"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode /api/upscale/variants-dir: %v: %s", err, raw)
	}
	return out.FreeBytes
}
