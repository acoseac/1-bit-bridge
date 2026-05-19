package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// fakeBatchCoordinator records every Submit / SubmitOptimize call
// so the admin DTO's kind dispatch can be exercised without a
// real transcode pipeline. Implements admin.AdminBatchCoordinator.
type fakeBatchCoordinator struct {
	submitCalls         []fakeBatchCall
	submitOptimizeCalls []fakeBatchCall
}

type fakeBatchCall struct {
	path string
	rate int
	bits int
}

func (f *fakeBatchCoordinator) Submit(_ context.Context, path string, rate, bits int) (AdminBatchSubmitResult, error) {
	f.submitCalls = append(f.submitCalls, fakeBatchCall{path: path, rate: rate, bits: bits})
	return AdminBatchSubmitResult{Path: path, TargetRate: rate, TargetBits: bits, EnqueuedCount: 1}, nil
}

func (f *fakeBatchCoordinator) SubmitOptimize(_ context.Context, path string) (AdminBatchSubmitResult, error) {
	f.submitOptimizeCalls = append(f.submitOptimizeCalls, fakeBatchCall{path: path})
	return AdminBatchSubmitResult{Path: path, EnqueuedCount: 1}, nil
}

func (f *fakeBatchCoordinator) Cancel(string) error { return nil }
func (f *fakeBatchCoordinator) ListBatches(int) ([]AdminBatchRow, error) {
	return nil, nil
}
func (f *fakeBatchCoordinator) Throughput() AdminBatchThroughput { return AdminBatchThroughput{} }

// TestApiUpscaleBatchSubmit_KindDispatch asserts the admin handler
// routes on the request body's `kind` field: empty/"upscale" →
// Submit, "optimize" → SubmitOptimize, anything else → 400. The
// senior-review fold-in of the optimize-batch surface.
func TestApiUpscaleBatchSubmit_KindDispatch(t *testing.T) {
	srv, _, _ := newTestServer(t)
	stub := &fakeBatchCoordinator{}
	srv.deps.BatchCoordinator = stub

	t.Run("empty kind dispatches to upscale Submit", func(t *testing.T) {
		stub.submitCalls = nil
		stub.submitOptimizeCalls = nil
		var res AdminBatchSubmitResult
		code := doJSON(t, srv.Handler(), "POST", "/api/upscale/batch",
			map[string]any{"path": "MusicA", "targetRate": 192000, "targetBits": 24}, &res)
		if code != http.StatusAccepted {
			t.Errorf("empty kind: got %d, want 202", code)
		}
		if len(stub.submitCalls) != 1 {
			t.Errorf("Submit calls: %d, want 1", len(stub.submitCalls))
		}
		if len(stub.submitOptimizeCalls) != 0 {
			t.Errorf("SubmitOptimize calls: %d, want 0", len(stub.submitOptimizeCalls))
		}
	})

	t.Run("kind=upscale dispatches to Submit", func(t *testing.T) {
		stub.submitCalls = nil
		stub.submitOptimizeCalls = nil
		var res AdminBatchSubmitResult
		code := doJSON(t, srv.Handler(), "POST", "/api/upscale/batch",
			map[string]any{"path": "MusicA", "kind": "upscale", "targetRate": 192000, "targetBits": 24}, &res)
		if code != http.StatusAccepted {
			t.Errorf("kind=upscale: got %d, want 202", code)
		}
		if len(stub.submitCalls) != 1 {
			t.Errorf("Submit calls: %d, want 1", len(stub.submitCalls))
		}
	})

	t.Run("kind=optimize dispatches to SubmitOptimize", func(t *testing.T) {
		stub.submitCalls = nil
		stub.submitOptimizeCalls = nil
		var res AdminBatchSubmitResult
		code := doJSON(t, srv.Handler(), "POST", "/api/upscale/batch",
			map[string]any{"path": "MusicA", "kind": "optimize"}, &res)
		if code != http.StatusAccepted {
			t.Errorf("kind=optimize: got %d, want 202", code)
		}
		if len(stub.submitCalls) != 0 {
			t.Errorf("Submit calls: %d, want 0 (optimize dispatch)", len(stub.submitCalls))
		}
		if len(stub.submitOptimizeCalls) != 1 {
			t.Errorf("SubmitOptimize calls: %d, want 1", len(stub.submitOptimizeCalls))
		}
		if stub.submitOptimizeCalls[0].path != "MusicA" {
			t.Errorf("SubmitOptimize path: %q, want MusicA", stub.submitOptimizeCalls[0].path)
		}
	})

	t.Run("kind=junk rejected with 400", func(t *testing.T) {
		stub.submitCalls = nil
		stub.submitOptimizeCalls = nil
		req := httptest.NewRequest("POST", "/api/upscale/batch",
			strings.NewReader(`{"path":"MusicA","kind":"junk"}`))
		req.RemoteAddr = "127.0.0.1:54321"
		req.Header.Set("content-type", "application/json")
		rw := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rw, req)
		if rw.Code != http.StatusBadRequest {
			t.Errorf("kind=junk: got %d, want 400", rw.Code)
		}
		if !strings.Contains(rw.Body.String(), "invalid-kind") {
			t.Errorf("body missing invalid-kind code: %s", rw.Body.String())
		}
		if len(stub.submitCalls)+len(stub.submitOptimizeCalls) > 0 {
			t.Errorf("dispatched on invalid kind: submit=%d optimize=%d",
				len(stub.submitCalls), len(stub.submitOptimizeCalls))
		}
	})

	t.Run("kind=OPTIMIZE (case-insensitive) dispatches", func(t *testing.T) {
		stub.submitCalls = nil
		stub.submitOptimizeCalls = nil
		var res AdminBatchSubmitResult
		code := doJSON(t, srv.Handler(), "POST", "/api/upscale/batch",
			map[string]any{"path": "MusicA", "kind": "OPTIMIZE"}, &res)
		if code != http.StatusAccepted {
			t.Errorf("kind=OPTIMIZE: got %d, want 202", code)
		}
		if len(stub.submitOptimizeCalls) != 1 {
			t.Errorf("SubmitOptimize calls: %d, want 1 (case folded)", len(stub.submitOptimizeCalls))
		}
	})
}

// TestApiLibraryBrowseProjection_KindOptimize wires the optimize
// deps closures and exercises the kind=optimize branch end-to-end.
// Verifies:
//   - the optimize section gets distinct numbers from the upscale
//     section (no cross-contamination at the projection level)
//   - `TargetRate=0`/`TargetBits=16` echoes back (signals
//     "per-track family-preserved")
//   - the response carries `Kind:"optimize"` echo
//   - tracks failing OptimizeEligible roll into UnknownFormatFiles
//     (a 16/44.1 source is already at the CarPlay floor → skipped)
func TestApiLibraryBrowseProjection_KindOptimize(t *testing.T) {
	srv, _, _ := newTestServer(t)
	browseTestSeed(t, srv)

	// Wire all four projection closures with simple stubs.
	srv.deps.ProjectedSize = func(sourceSize int64, sourceRate, sourceBits, targetRate, targetBits int) int64 {
		if sourceSize <= 0 || sourceRate <= 0 || sourceBits <= 0 ||
			targetRate <= 0 || targetBits <= 0 {
			return 0
		}
		return sourceSize *
			int64(targetRate) / int64(sourceRate) *
			int64(targetBits) / int64(sourceBits)
	}
	srv.deps.AvailableDiskSpace = func(string) (int64, error) {
		return 1 << 40, nil // 1 TB
	}
	// Mirrors transcode.OptimizeEligible: PCM-only AND not-already-
	// at-CarPlay-floor (source rate > 48000 OR source bits > 16).
	srv.deps.OptimizeEligible = func(_, codec string, sourceRate, sourceBits int) bool {
		isPCM := codec == "FLAC" || codec == "ALAC" || codec == "WAV" || codec == "AIFF"
		if !isPCM {
			return false
		}
		return sourceRate > 48000 || sourceBits > 16
	}
	srv.deps.TargetRateForOptimize = func(sourceRate int) int {
		switch {
		case sourceRate%48000 == 0:
			return 48000
		case sourceRate%44100 == 0:
			return 44100
		default:
			return 0
		}
	}
	cfg := srv.deps.CfgHolder.Load()
	next := config.Clone(cfg)
	next.Upscale.Enabled = true
	srv.deps.CfgHolder.Store(next)

	var resp browseProjectionResponse
	code := doJSON(t, srv.Handler(), "GET",
		"/api/library/browse-projection?path=MusicA&kind=optimize", nil, &resp)
	if code != http.StatusOK {
		t.Fatalf("optimize projection: %d", code)
	}
	if resp.Kind != "optimize" {
		t.Errorf("response Kind = %q, want \"optimize\"", resp.Kind)
	}
	if resp.TargetBits != 16 {
		t.Errorf("TargetBits = %d, want 16 (optimize fixed)", resp.TargetBits)
	}
	if resp.TargetRate != 0 {
		t.Errorf("TargetRate = %d, want 0 (signals per-track family-preserved)", resp.TargetRate)
	}
	// MusicA fixture: 3 tracks (Album1/01 44.1/16, Album1/02 44.1/16,
	// Album2/01 96/24).
	// - Two 44.1/16 tracks fail OptimizeEligible (already at floor)
	//   → counted in UnknownFormatFiles, NOT ProjectedFiles.
	// - One 96/24 track is eligible → ProjectedFiles=1.
	// Crucially: the upscale variants on Album1/01 and Album2/01
	// should NOT bleed into the optimize HasVariant count (senior
	// fix) — so AlreadyCoveredFiles must be 0 for optimize.
	if resp.AlreadyCoveredFiles != 0 {
		t.Errorf("AlreadyCoveredFiles = %d, want 0 (no optimize variants seeded; upscale variants must NOT cross-contaminate)", resp.AlreadyCoveredFiles)
	}
	if resp.UnknownFormatFiles < 2 {
		t.Errorf("UnknownFormatFiles = %d, want >= 2 (two 44.1/16 tracks already at floor)", resp.UnknownFormatFiles)
	}
	if resp.ProjectedFiles != 1 {
		t.Errorf("ProjectedFiles = %d, want 1 (only the 96/24 track is eligible)", resp.ProjectedFiles)
	}
}

// TestApiLibraryBrowseProjection_KindOptimize_503WhenUnwired covers
// the closure-missing path: optimize-projection MUST surface a clean
// 503 rather than a typed-nil panic when the optimize-specific
// closures aren't wired (upscale feature off OR pre-feature build).
func TestApiLibraryBrowseProjection_KindOptimize_503WhenUnwired(t *testing.T) {
	srv, _, _ := newTestServer(t)
	browseTestSeed(t, srv)
	// Wire the upscale-side closures but NOT the optimize-side ones.
	srv.deps.ProjectedSize = func(int64, int, int, int, int) int64 { return 0 }
	srv.deps.AvailableDiskSpace = func(string) (int64, error) { return 0, nil }

	req := httptest.NewRequest("GET",
		"/api/library/browse-projection?path=MusicA&kind=optimize", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	rw := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rw, req)
	if rw.Code != http.StatusServiceUnavailable {
		t.Errorf("got %d, want 503", rw.Code)
	}
	if !strings.Contains(rw.Body.String(), "optimize-disabled") {
		t.Errorf("body missing optimize-disabled code: %s", rw.Body.String())
	}
}

// TestApiLibraryBrowseProjection_KindInvalid asserts a typo in the
// kind query param surfaces a 400 rather than silently falling
// through to the upscale default.
func TestApiLibraryBrowseProjection_KindInvalid(t *testing.T) {
	srv, _, _ := newTestServer(t)
	browseTestSeed(t, srv)
	srv.deps.ProjectedSize = func(int64, int, int, int, int) int64 { return 0 }
	srv.deps.AvailableDiskSpace = func(string) (int64, error) { return 0, nil }

	req := httptest.NewRequest("GET",
		"/api/library/browse-projection?path=MusicA&kind=junk", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	rw := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rw, req)
	if rw.Code != http.StatusBadRequest {
		t.Errorf("got %d, want 400", rw.Code)
	}
	if !strings.Contains(rw.Body.String(), "invalid-kind") {
		t.Errorf("body missing invalid-kind code: %s", rw.Body.String())
	}
}

// TestApiLibraryBrowse_PerKindFolderRollup pins the JSON shape: the
// /api/library/browse response carries `optimizedCount` /
// `optimizedSizeBytes` alongside the legacy `upscaledCount` /
// `upscaledSizeBytes`. JS-side tile rendering depends on the
// presence of both keys.
func TestApiLibraryBrowse_PerKindFolderRollup(t *testing.T) {
	srv, _, _ := newTestServer(t)
	browseTestSeed(t, srv)
	// Seed an optimize variant on top of the base fixture so the
	// per-kind split surfaces non-zero values for both keys.
	if err := srv.deps.Manifest.UpsertVariant(context.Background(), manifest.VariantRow{
		SourcePath: "MusicA/Album1/02.flac", VariantID: "optimized-v2-44100-16",
		SidecarPath: "/tmp/opt.flac", Format: "flac",
		SampleRate: 44100, BitsPerSample: 16, SizeBytes: 333,
		SourceMTimeNS: 1, SourceSize: 300, SoxSettings: "{}", CreatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}

	type folderShape struct {
		Path               string `json:"path"`
		TrackCount         int    `json:"trackCount"`
		UpscaledCount      int    `json:"upscaledCount"`
		OptimizedCount     int    `json:"optimizedCount"`
		UpscaledSizeBytes  int64  `json:"upscaledSizeBytes"`
		OptimizedSizeBytes int64  `json:"optimizedSizeBytes"`
		PathHash           string `json:"pathHash"`
	}
	type resp struct {
		Folders []folderShape `json:"folders"`
	}
	var r resp
	code := doJSON(t, srv.Handler(), "GET", "/api/library/browse?path=", nil, &r)
	if code != http.StatusOK {
		t.Fatalf("browse: %d", code)
	}
	var musicA folderShape
	for _, f := range r.Folders {
		if f.Path == "MusicA" {
			musicA = f
			break
		}
	}
	if musicA.Path == "" {
		t.Fatalf("MusicA missing: %+v", r.Folders)
	}
	if musicA.UpscaledCount != 2 {
		t.Errorf("MusicA upscaledCount = %d, want 2", musicA.UpscaledCount)
	}
	if musicA.OptimizedCount != 1 {
		t.Errorf("MusicA optimizedCount = %d, want 1", musicA.OptimizedCount)
	}
	if musicA.OptimizedSizeBytes != 333 {
		t.Errorf("MusicA optimizedSizeBytes = %d, want 333", musicA.OptimizedSizeBytes)
	}
	if len(musicA.PathHash) != 8 {
		t.Errorf("pathHash length = %d, want 8 (crc32 hex)", len(musicA.PathHash))
	}
}
