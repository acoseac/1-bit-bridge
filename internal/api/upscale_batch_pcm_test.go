package api

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/auth"
	"github.com/acoseac/1-bit-bridge/internal/config"
)

// batchFixtureWithDSDRender is batchFixture with upscaling active and the
// DSD-render predicate explicit — the `pcm` batch kind's 503 gate reads
// it, so a fixture needs both states.
func batchFixtureWithDSDRender(t *testing.T, dsdRender bool) (*httptest.Server, string, *stubBatchCoordinator) {
	t.Helper()
	tmp := t.TempDir()
	cfg := &config.Config{LibraryRoots: []string{tmp}, ListenAddress: ":7788", LibraryName: "T"}
	store, _ := auth.OpenStore(filepath.Join(tmp, "tokens.json"))
	raw, _, _ := store.Mint("batch")
	stub := &stubBatchCoordinator{}
	srv := New(cfg, store, nil, "fp").WithBatchCoordinator(stub).
		WithUpscale(func() bool { return true }, nil).
		WithDSDRender(func() bool { return dsdRender })
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	return hs, raw, stub
}

// TestUpscaleBatchPCMKind: `kind: "pcm"` dispatches to SubmitPCMRender
// when the renditions are on, and is the kind's own 503 when they are
// off — decided before the coordinator walks anything.
func TestUpscaleBatchPCMKind(t *testing.T) {
	t.Parallel()
	t.Run("on dispatches to SubmitPCMRender", func(t *testing.T) {
		hs, tok, coord := batchFixtureWithDSDRender(t, true)
		resp := postJSON(t, hs, "/v1/upscale/batch", tok, BatchRequest{Path: "Artist/Album", Kind: "pcm"})
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("status: got %d, want 202", resp.StatusCode)
		}
		if coord.pcms != 1 || coord.optimizes != 0 || coord.submits != 0 {
			t.Errorf("coordinator calls: pcm=%d optimize=%d upscale=%d, want exactly one pcm", coord.pcms, coord.optimizes, coord.submits)
		}
	})
	t.Run("off is the kind's 503", func(t *testing.T) {
		hs, tok, coord := batchFixtureWithDSDRender(t, false)
		resp := postJSON(t, hs, "/v1/upscale/batch", tok, BatchRequest{Path: "Artist/Album", Kind: "pcm"})
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("status: got %d, want 503", resp.StatusCode)
		}
		if coord.pcms != 0 {
			t.Errorf("SubmitPCMRender was called %d times with the renditions off", coord.pcms)
		}
	})
}
