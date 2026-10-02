package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/api"
	"github.com/acoseac/1-bit-bridge/internal/transcode"
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
// boot-off leg could not tell a live gate from a boot snapshot. So the
// test answers serve's sox probe itself (withUsableSox), and health must
// then read what was PATCHed, which is checked before the comparison.
//
// That answer was a stand-in sox first on PATH until 2026-09-29, and
// POSIX only. ProbeSox runs it with a 2 s timeout, a timed-out probe reads
// as no sox, and the shared cache keeps that for 30 s: a stand-in slower
// than 2 s fails step 1 exactly as the dev Mac did under sibling sessions'
// load (backlog B105), and on a Linux host starved by a CPU hog, under
// -race, the health request that ran the probe took up to 2.08 s. A probe
// that runs no process takes the host's load out of the test, and puts the
// check on Windows too.
//
// The variants-dir readout reads the same AvailableDiskSpace closure, so
// on a bridge booted with upscaling off the console's "Free on that
// volume" read 0 bytes whatever the disk held. Free space is a fact
// about the disk, not about upscaling, so it must be reported in every
// state.
func TestServeProjectionFollowsTheLiveUpscaleGate(t *testing.T) {
	for _, bootOn := range []bool{false, true} {
		t.Run(fmt.Sprintf("booted with upscale.enabled=%t", bootOn), func(t *testing.T) {
			b := startConsoleBridge(t, fmt.Sprintf("upscale:\n  enabled: %t\n", bootOn), nil, withUsableSox)
			for step, on := range []bool{bootOn, !bootOn, bootOn} {
				if step > 0 {
					patchUpscaleEnabled(t, b.console, b.adminBase, on, b.stderr)
				}
				healthOn := healthUpscaleEnabled(t, b.phone, b.apiBase)
				if healthOn != on {
					t.Fatalf("step %d: /v1/health says upscaleEnabled=%t with the flag at %t and serve's "+
						"sox probe answering a usable sox, so the comparison below would not show what "+
						"it is meant to; stderr=%s", step, healthOn, on, b.stderr.String())
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

// TestServeBootLineReadsTheSharedSoxProbe: serve's boot line about a
// feature switched on without a usable sox reads the probe the gates read
// (soxToolchainCache), so it says what that probe says, once per feature,
// and a boot test's stand-in (serveOpts.soxProbe) reaches it. It probed sox
// for itself until 2026-09-29 (backlog B105), so on a host with a usable
// sox it printed nothing while the gates read the stand-in, and on a host
// without one it printed the host's answer.
func TestServeBootLineReadsTheSharedSoxProbe(t *testing.T) {
	const said = "the test's stand-in finds no sox"
	var probes atomic.Int32
	b := startConsoleBridge(t, "upscale:\n  enabled: true\nanalysis:\n  enabled: true\n", nil, func(o *serveOpts) {
		o.soxProbe = func(context.Context) (transcode.SoxInfo, error) {
			probes.Add(1)
			return transcode.SoxInfo{}, fmt.Errorf("%w: %s", transcode.ErrSoxMissing, said)
		}
	})
	out := b.stderr.String()
	for _, feature := range []string{"upscale", "analysis"} {
		lead := feature + ": feature is enabled in bridge.yaml but sox is not available, so it stays off"
		var lines []string
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(line, lead) {
				lines = append(lines, line)
			}
		}
		if len(lines) != 1 || !strings.HasSuffix(lines[0], said) {
			t.Errorf("the boot lines about %s are %q, want one that gives the shared probe's answer (%q); "+
				"stderr=%s", feature, lines, said, out)
		}
	}
	if probes.Load() == 0 {
		t.Error("serve never asked the stand-in probe: its sox answers came from the host's")
	}
}

// withUsableSox answers serve's sox probe (serveOpts.soxProbe) with a sox
// whose build has FLAC, as `sox --help` reports one, without running a
// process. Nothing in the test above decodes anything (its library is
// empty), so the probe's question is the only one sox is asked.
func withUsableSox(o *serveOpts) {
	o.soxProbe = func(context.Context) (transcode.SoxInfo, error) {
		return transcode.SoxInfo{Path: "sox", Version: "v14.4.2", Formats: []string{"flac", "wav"},
			FormatsKnown: true, HasFLAC: true}, nil
	}
}

// patchUpscaleEnabled switches upscaling through the console's settings
// PATCH and requires the report to call the change live.
func patchUpscaleEnabled(t *testing.T, client *http.Client, adminBase string, on bool, stderr *safeBuffer) {
	t.Helper()
	patchSwitchLive(t, client, adminBase, "upscaleEnabled", on, stderr)
}

// patchSwitchLive sets one of the settings PATCH's boolean fields and
// requires the report to call the change live.
func patchSwitchLive(t *testing.T, client *http.Client, adminBase, field string, on bool, stderr *safeBuffer) {
	t.Helper()
	patchSettingLive(t, client, adminBase, field, on, stderr)
}

// patchSettingLive sets one field of the settings PATCH to value and
// requires the report to call the change live.
func patchSettingLive(t *testing.T, client *http.Client, adminBase, field string, value any, stderr *safeBuffer) {
	t.Helper()
	body, err := json.Marshal(map[string]any{field: value})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPatch, adminBase+"/api/settings",
		strings.NewReader(string(body)))
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
		t.Fatalf("PATCH /api/settings %s = %d: %s", body, resp.StatusCode, raw)
	}
	var report struct {
		Fields map[string]struct {
			Status string `json:"status"`
		} `json:"fields"`
	}
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatalf("decode the settings report: %v: %s", err, raw)
	}
	if got := report.Fields[field].Status; got != "live" {
		t.Fatalf("the settings report calls %s %q, want \"live\": %s", field, got, raw)
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
