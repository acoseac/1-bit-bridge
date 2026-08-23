package admin

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// TestApiJobsNilSafeDefaults — a bare test server (no job closures
// wired) still serves a full 200 snapshot: config-derived sections
// populate, closure-backed fields are omitted, nothing panics. This is
// the degradation contract the Jobs page renders against.
func TestApiJobsNilSafeDefaults(t *testing.T) {
	srv, _, _ := newTestServer(t)
	h := srv.Handler()

	var got jobsSnapshotResponse
	if code := doJSON(t, h, "GET", "/api/jobs", nil, &got); code != 200 {
		t.Fatalf("jobs: %d", code)
	}
	if got.Scanner.IntervalSec != 3600 {
		t.Errorf("scanner.intervalSec = %d, want 3600", got.Scanner.IntervalSec)
	}
	if got.Scanner.LastFullScan != nil || got.Scanner.NextScanDue != nil {
		t.Errorf("scanner timestamps should be omitted before any scan: %+v", got.Scanner)
	}
	if got.Analysis.Enabled || got.Analysis.Active {
		t.Errorf("analysis should be off by default: %+v", got.Analysis)
	}
	if got.Analysis.Sweep != nil || got.Analysis.Coverage != nil {
		t.Errorf("analysis sweep/coverage should be omitted when inactive: %+v", got.Analysis)
	}
	if got.Fingerprint != nil {
		t.Errorf("fingerprint should be omitted without a closure: %+v", got.Fingerprint)
	}
	if got.Enrichment.Source == "" {
		t.Error("enrichment.source should always resolve (default = musicbrainz)")
	}
	if got.Enrichment.HarvestActive {
		t.Error("harvestActive should be false without the closure")
	}
	// Enabled tracks the config (which now defaults ON); what this test
	// is about is the RECORDER being absent, so Run must stay nil.
	if got.SmartMixes.Run != nil {
		t.Errorf("smartMixes.Run should be nil with no recorder: %+v", got.SmartMixes)
	}
	if got.Updates.CheckIntervalHours != 6 {
		t.Errorf("updates.checkIntervalHours = %d, want resolved default 6", got.Updates.CheckIntervalHours)
	}
	if got.UPnP.Enabled || got.UPnP.ConfiguredServers != 0 {
		t.Errorf("upnp should be off by default: %+v", got.UPnP)
	}
}

// TestApiJobsWiredSections — closure-backed sections surface once
// wired, the analysis coverage tile computes from the store (behind
// its TTL cache), and NextScanDue derives from LastFullScan + interval.
func TestApiJobsWiredSections(t *testing.T) {
	srv, _, _ := newTestServer(t)
	h := srv.Handler()
	ctx := context.Background()

	// Seed a small library: 2 eligible FLACs (1 analysed fresh), 1 DSD.
	for _, tr := range []struct {
		path string
		size int64
	}{{"a/x.flac", 10}, {"a/y.flac", 10}, {"a/z.dsf", 10}} {
		if err := srv.deps.Manifest.UpsertTrack(ctx, &manifest.Track{Path: tr.path, Size: tr.size, ModTime: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	if err := srv.deps.Manifest.UpsertAnalysis(ctx, manifest.AnalysisRow{
		SourcePath: "a/x.flac", WaveformPath: "/w/x", WaveformTag: "aa",
		WaveformSize: 4, SourceMTimeNS: 1, SourceSize: 10, SchemaVersion: "v9", CreatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}

	next := config.Clone(srv.deps.CfgHolder.Load())
	next.Analysis.Enabled = true
	next.SmartPlaylists.Enabled = boolPtrT(true)
	srv.deps.CfgHolder.Store(next)

	now := time.Date(2026, 8, 2, 10, 0, 0, 0, time.UTC)
	srv.deps.AnalysisActive = func() bool { return true }
	srv.deps.AnalysisSchemaVersion = "v9"
	srv.deps.AnalysisSweep = func() *AnalysisSweepState {
		return &AnalysisSweepState{LastFinishedAt: &now, Last: &AnalysisSweepCounts{Total: 3, Enqueued: 1}}
	}
	srv.deps.FingerprintState = func() *FingerprintJobState {
		return &FingerprintJobState{Enabled: true, Active: false, DegradedReason: "no_api_key"}
	}
	srv.deps.SmartMixRun = func() *JobRunState { return &JobRunState{LastFinishedAt: &now} }
	srv.deps.BackupRun = func() *JobRunState { return &JobRunState{NextDueAt: &now} }

	var got jobsSnapshotResponse
	if code := doJSON(t, h, "GET", "/api/jobs", nil, &got); code != 200 {
		t.Fatalf("jobs: %d", code)
	}
	if !got.Analysis.Active || got.Analysis.DegradedReason != "" {
		t.Errorf("analysis should be active with no degraded reason: %+v", got.Analysis)
	}
	cov := got.Analysis.Coverage
	if cov == nil {
		t.Fatal("analysis.coverage missing")
	}
	if cov.TotalLocal != 3 || cov.DSDExcluded != 1 || cov.Eligible != 2 || cov.Analysed != 1 {
		t.Errorf("coverage = %+v, want total 3 / dsd 1 / eligible 2 / analysed 1", cov)
	}
	if got.Analysis.Sweep == nil || got.Analysis.Sweep.Last == nil || got.Analysis.Sweep.Last.Total != 3 {
		t.Errorf("analysis.sweep not surfaced: %+v", got.Analysis.Sweep)
	}
	if got.Fingerprint == nil || got.Fingerprint.DegradedReason != "no_api_key" {
		t.Errorf("fingerprint state not surfaced: %+v", got.Fingerprint)
	}
	if !got.SmartMixes.Enabled || got.SmartMixes.Run == nil || got.SmartMixes.IntervalSec <= 0 {
		t.Errorf("smartMixes not surfaced: %+v", got.SmartMixes)
	}
	if got.Backups.Run == nil {
		t.Errorf("backups.run not surfaced: %+v", got.Backups)
	}
	if !got.SmartMixes.AnalysisAssisted {
		t.Error("smartMixes.analysisAssisted should mirror analysis.active")
	}

	// Enabled-but-inactive analysis reads as degraded sox_missing.
	srv.deps.AnalysisActive = func() bool { return false }
	got = jobsSnapshotResponse{}
	if code := doJSON(t, h, "GET", "/api/jobs", nil, &got); code != 200 {
		t.Fatalf("jobs degraded: %d", code)
	}
	if got.Analysis.DegradedReason != "sox_missing" {
		t.Errorf("degradedReason = %q, want sox_missing", got.Analysis.DegradedReason)
	}
	if got.Analysis.Coverage != nil {
		t.Errorf("coverage should be omitted when inactive: %+v", got.Analysis.Coverage)
	}

	// Scanner next-due derives from a completed scan + interval.
	if _, err := srv.deps.Scanner.Scan(ctx); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	got = jobsSnapshotResponse{}
	if code := doJSON(t, h, "GET", "/api/jobs", nil, &got); code != 200 {
		t.Fatalf("jobs post-scan: %d", code)
	}
	if got.Scanner.LastFullScan == nil || got.Scanner.NextScanDue == nil {
		t.Fatalf("scanner timestamps missing after scan: %+v", got.Scanner)
	}
	if d := got.Scanner.NextScanDue.Sub(*got.Scanner.LastFullScan); d != time.Hour {
		t.Errorf("nextScanDue - lastFullScan = %v, want 1h (intervalSec 3600)", d)
	}
}

// TestApiFingerprintSweep mirrors TestApiAnalysisSweep for the
// fingerprint trigger endpoint.
func TestApiFingerprintSweep(t *testing.T) {
	srv, _, _ := newTestServer(t)
	h := srv.Handler()

	var out map[string]any
	if code := doJSON(t, h, "POST", "/api/fingerprint/sweep", nil, &out); code != http.StatusServiceUnavailable {
		t.Fatalf("unwired trigger: code = %d, want 503", code)
	}
	triggered := 0
	srv.deps.TriggerFingerprintSweep = func() bool { triggered++; return true }
	out = nil
	if code := doJSON(t, h, "POST", "/api/fingerprint/sweep", nil, &out); code != http.StatusAccepted {
		t.Fatalf("wired trigger: code = %d, want 202", code)
	}
	if triggered != 1 {
		t.Errorf("trigger invoked %d times, want 1", triggered)
	}
}

// TestJobsAutoOptimizeCard pins the card's presence contract: absent when
// the sweeper isn't wired (no upscale pool on this bridge, so a card
// explaining a feature that can't run would be noise), present with the
// live state when it is.
func TestJobsAutoOptimizeCard(t *testing.T) {
	srv, _, _ := newTestServer(t)
	h := srv.Handler()

	// Unwired → field omitted entirely.
	var got jobsSnapshotResponse
	if code := doJSON(t, h, "GET", "/api/jobs", nil, &got); code != 200 {
		t.Fatalf("jobs: %d", code)
	}
	if got.AutoOptimize != nil {
		t.Errorf("autoOptimize should be omitted when unwired: %+v", got.AutoOptimize)
	}

	now := time.Date(2026, 8, 17, 9, 0, 0, 0, time.UTC)
	srv.deps.AutoOptimizeState = func() *AutoOptimizeJobState {
		return &AutoOptimizeJobState{
			Enabled: true, Active: true, LastFinishedAt: &now,
			Last: &AutoOptimizeSweepCounts{Enqueued: 12, Regenerated: 2, Remaining: 340},
		}
	}
	got = jobsSnapshotResponse{}
	if code := doJSON(t, h, "GET", "/api/jobs", nil, &got); code != 200 {
		t.Fatalf("jobs wired: %d", code)
	}
	if got.AutoOptimize == nil {
		t.Fatal("autoOptimize missing when wired")
	}
	if !got.AutoOptimize.Active || got.AutoOptimize.Last == nil {
		t.Fatalf("autoOptimize not surfaced: %+v", got.AutoOptimize)
	}
	if got.AutoOptimize.Last.Enqueued != 12 || got.AutoOptimize.Last.Remaining != 340 {
		t.Errorf("autoOptimize.last = %+v, want enqueued 12 / remaining 340", got.AutoOptimize.Last)
	}
}
