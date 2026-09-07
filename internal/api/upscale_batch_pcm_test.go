package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// batchFixtureWithDSDRender is batchFixture with upscaling active and the
// DSD-render predicate explicit — the `pcm` batch kind's 503 gate reads
// it, so a fixture needs both states. Built by DECORATING the sibling
// rather than copying its wiring: a second copy would drift the moment
// batchFixture gains a dependency, and that drift is silent (the pcm
// tests would keep passing against a server the other tests no longer
// describe).
func batchFixtureWithDSDRender(t *testing.T, dsdRender bool) (*httptest.Server, string, *stubBatchCoordinator) {
	t.Helper()
	return batchFixtureWith(t, func(s *Server) *Server {
		return s.WithUpscale(func() bool { return true }, nil).
			WithDSDRender(func() bool { return dsdRender })
	})
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
