package admin

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// TestDSDRenderPatchIsLiveAndNudgesTheSweeper: `dsdRenderEnabled` is a
// class-A live field — every DSD gate reads the caps per call — and a
// flip nudges the auto-optimize sweeper (a newly-admitted DSD library
// should start rendering now). A same-value submit is `unchanged` and
// nudges nothing; a toolchain the doctor rejects rides along as the
// `+reason`, never a restart.
func TestDSDRenderPatchIsLiveAndNudgesTheSweeper(t *testing.T) {
	// patchDSDRender flips the field and returns the per-field outcome.
	patchDSDRender := func(t *testing.T, srv *Server, on bool) fieldApply {
		t.Helper()
		var resp settingsPatchResponse
		if code := doJSON(t, srv.Handler(), "PATCH", "/api/settings",
			map[string]any{"dsdRenderEnabled": on}, &resp); code != 200 {
			t.Fatalf("patch dsdRenderEnabled=%v: %d", on, code)
		}
		if resp.RestartRequired {
			t.Error("RestartRequired = true, want false (every DSD gate reads the caps live)")
		}
		return resp.Fields["dsdRenderEnabled"]
	}

	t.Run("flip on is live, persisted, and nudges the sweeper", func(t *testing.T) {
		srv, _, _ := newTestServer(t)
		nudges := 0
		srv.deps.TriggerAutoOptimizeSweep = func() bool { nudges++; return true }

		got := patchDSDRender(t, srv, true)
		if got.Status != applyLive || got.Reason != "" {
			t.Fatalf("status=%q reason=%q, want live with no reason", got.Status, got.Reason)
		}
		if nudges != 1 {
			t.Errorf("sweeper nudged %d times, want 1", nudges)
		}
		if !srv.deps.CfgHolder.Load().Upscale.DSDRender.Enabled {
			t.Error("the flag did not persist into the live config")
		}
		var read settingsResponse
		if code := doJSON(t, srv.Handler(), "GET", "/api/settings", nil, &read); code != 200 {
			t.Fatalf("get: %d", code)
		}
		if !read.DSDRenderEnabled {
			t.Error("GET /api/settings does not report the flag on")
		}
	})

	t.Run("the same value again is unchanged and nudges nothing", func(t *testing.T) {
		srv, _, _ := newTestServer(t)
		nudges := 0
		srv.deps.TriggerAutoOptimizeSweep = func() bool { nudges++; return true }

		patchDSDRender(t, srv, true)
		got := patchDSDRender(t, srv, true)
		if got.Status != applyUnchanged {
			t.Errorf("status=%q, want unchanged", got.Status)
		}
		if nudges != 1 {
			t.Errorf("sweeper nudged %d times, want 1 (only the real flip)", nudges)
		}
	})

	// A toolchain the doctor rejects is still LIVE — a restart would not
	// install ffmpeg — carrying the verdict as the reason so the switch
	// does not look inert.
	t.Run("a rejected toolchain rides along as the reason, never a restart", func(t *testing.T) {
		srv, _, _ := newTestServer(t)
		srv.deps.TriggerAutoOptimizeSweep = func() bool { return true }
		srv.deps.DSDRenderToolchain = func() (bool, string) { return false, "this ffmpeg build lacks the dsd_* decoders" }

		got := patchDSDRender(t, srv, true)
		if got.Status != applyLive || !strings.Contains(got.Reason, "dsd_*") {
			t.Errorf("status=%q reason=%q, want live with the doctor's reason", got.Status, got.Reason)
		}
	})
}

// TestEligibilityOptsFollowTheCaps: the admin's SQL mirrors bind the live
// caps (absent → zero → byte-identical to pre-v43), and DST only counts
// while DSD itself does.
func TestEligibilityOptsFollowTheCaps(t *testing.T) {
	srv, _, _ := newTestServer(t)
	if got := srv.eligibilityOpts(); got != (manifest.EligibilityOpts{}) {
		t.Fatalf("unwired caps: opts = %+v, want the zero value", got)
	}
	srv.deps.DSDRenderCaps = func() (bool, bool) { return true, true }
	if got := srv.eligibilityOpts(); !got.DSDRender || !got.DST {
		t.Errorf("caps (true,true): opts = %+v", got)
	}
	srv.deps.DSDRenderCaps = func() (bool, bool) { return false, true }
	if got := srv.eligibilityOpts(); got.DSDRender || got.DST {
		t.Errorf("caps (false,true): opts = %+v, want DST folded off with DSD", got)
	}
}

// TestApiLibraryBrowseProjection_KindPCM: the faithful tier projects DSD
// rows only (a PCM source is at-target for this kind), at 24-bit, 503
// while the caps are off; and the optimize kind admits the same DSD row
// as its compact tier once the caps are on, folding it into
// "variants not possible" when they are not.
func TestApiLibraryBrowseProjection_KindPCM(t *testing.T) {
	srv, _, _ := newTestServer(t)
	browseTestSeed(t, srv)
	wireOptimizeTestDeps(t, srv)
	rate, bits, isDSD := 2822400.0, 1, true
	if err := srv.deps.Manifest.UpsertTrack(context.Background(), &manifest.Track{
		Path: "MusicA/Album2/02.dsf", Size: 4000, SampleRate: &rate, BitsPerSample: &bits,
		Codec: "DSF", IsDSD: &isDSD,
	}); err != nil {
		t.Fatal(err)
	}
	// The production closure folds the LIVE caps into the per-track
	// answer (transcode.DSDRenderEligible takes DSDRenderCaps), so the
	// stub reads the same switch the caps dep reports — flipping capsOn
	// is what a real operator toggle does to both.
	capsOn := false
	srv.deps.DSDRenderCaps = func() (bool, bool) { return capsOn, false }
	srv.deps.DSDRenderEligible = func(_, codec string, dsd bool, sourceRate int, compression string) bool {
		return capsOn && dsd && codec == "DSF" && sourceRate%44100 == 0 && compression != "DST"
	}
	srv.deps.TargetRateForPCMRender = func(sourceRate int) int {
		if sourceRate%44100 == 0 {
			return 176400
		}
		return 0
	}

	// Caps OFF: pcm is 503, optimize folds the DSD row into unknownFormat.
	var errBody map[string]any
	if code := doJSON(t, srv.Handler(), "GET", "/api/library/browse-projection?path=MusicA&kind=pcm", nil, &errBody); code != http.StatusServiceUnavailable || errBody["error"] != "dsd-render-disabled" {
		t.Fatalf("caps off: pcm projection = %d %v, want 503 dsd-render-disabled", code, errBody)
	}
	var opt browseProjectionResponse
	if code := doJSON(t, srv.Handler(), "GET", "/api/library/browse-projection?path=MusicA&kind=optimize", nil, &opt); code != 200 {
		t.Fatalf("caps off optimize: %d", code)
	}
	if opt.ProjectedFiles != 1 || opt.UnknownFormatFiles != 1 {
		t.Errorf("caps off optimize: projected=%d unknown=%d, want 1 (the 96/24 FLAC) and 1 (the DSF)", opt.ProjectedFiles, opt.UnknownFormatFiles)
	}

	// Caps ON.
	capsOn = true
	var pcm browseProjectionResponse
	if code := doJSON(t, srv.Handler(), "GET", "/api/library/browse-projection?path=MusicA&kind=pcm", nil, &pcm); code != 200 {
		t.Fatalf("caps on pcm: %d", code)
	}
	if pcm.Kind != "pcm" || pcm.TargetBits != 24 || pcm.TargetRate != 0 {
		t.Errorf("pcm echo: kind=%q bits=%d rate=%d, want pcm/24/0", pcm.Kind, pcm.TargetBits, pcm.TargetRate)
	}
	if pcm.ProjectedFiles != 1 {
		t.Errorf("pcm ProjectedFiles = %d, want 1 (the DSF)", pcm.ProjectedFiles)
	}
	if pcm.AlreadyAtTargetFiles != 3 {
		t.Errorf("pcm AlreadyAtTargetFiles = %d, want 3 (every PCM source needs nothing from this kind)", pcm.AlreadyAtTargetFiles)
	}
	if pcm.UnknownFormatFiles != 0 || pcm.AlreadyCoveredFiles != 0 {
		t.Errorf("pcm unknown=%d covered=%d, want 0/0", pcm.UnknownFormatFiles, pcm.AlreadyCoveredFiles)
	}
	if code := doJSON(t, srv.Handler(), "GET", "/api/library/browse-projection?path=MusicA&kind=optimize", nil, &opt); code != 200 {
		t.Fatalf("caps on optimize: %d", code)
	}
	if opt.ProjectedFiles != 2 || opt.UnknownFormatFiles != 0 {
		t.Errorf("caps on optimize: projected=%d unknown=%d, want 2 (FLAC 96/24 + the DSF's compact tier) and 0", opt.ProjectedFiles, opt.UnknownFormatFiles)
	}
	// The upscale kind never sees a DSD row as anything but "not possible".
	var up browseProjectionResponse
	if code := doJSON(t, srv.Handler(), "GET", "/api/library/browse-projection?path=MusicA&kind=upscale", nil, &up); code != 200 {
		t.Fatalf("upscale: %d", code)
	}
	if up.UnknownFormatFiles != 1 {
		t.Errorf("upscale UnknownFormatFiles = %d, want 1 (the DSF, whatever the caps)", up.UnknownFormatFiles)
	}
}
