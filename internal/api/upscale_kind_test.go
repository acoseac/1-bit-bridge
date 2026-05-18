package api

import (
	"testing"
)

// TestUpscaleRequest_KindRouting locks the per-candidate dispatch
// branch in the /v1/upscale handler: `kind: "upscale"` (or empty,
// for back-compat with pre-v1.x iOS clients that omit the field)
// routes to EnqueueOne; `kind: "optimize"` routes to EnqueueOptimize.
// A typo'd kind value lands on a 400 — silent downgrade to upscale
// would re-introduce the kind-discriminator-corruption hazard a
// future server-side feature gate is trying to defend against.
func TestUpscaleRequest_KindRouting(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name             string
		body             UpscaleRequest
		wantStatus       int
		wantUpscaleCalls int
		wantOptimizeCals int
	}{
		{
			name:             "empty kind routes to upscale (back-compat)",
			body:             UpscaleRequest{Path: "Artist/Album/01.flac"},
			wantStatus:       202,
			wantUpscaleCalls: 1,
		},
		{
			name:             "explicit upscale kind routes to upscale",
			body:             UpscaleRequest{Path: "Artist/Album/01.flac", Kind: "upscale"},
			wantStatus:       202,
			wantUpscaleCalls: 1,
		},
		{
			name:             "optimize kind routes to EnqueueOptimize",
			body:             UpscaleRequest{Path: "Artist/Album/01.flac", Kind: "optimize"},
			wantStatus:       202,
			wantOptimizeCals: 1,
		},
		{
			name:             "case-insensitive optimize matches",
			body:             UpscaleRequest{Path: "Artist/Album/01.flac", Kind: "OPTIMIZE"},
			wantStatus:       202,
			wantOptimizeCals: 1,
		},
		{
			name:       "unknown kind rejects with 400",
			body:       UpscaleRequest{Path: "Artist/Album/01.flac", Kind: "downscale"},
			wantStatus: 400,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			hs, tok, _, stub := upscaleFixture(t, true)
			resp := postJSON(t, hs, "/v1/upscale", tok, tc.body)
			defer resp.Body.Close()
			if resp.StatusCode != tc.wantStatus {
				t.Errorf("status: got %d, want %d", resp.StatusCode, tc.wantStatus)
			}
			if len(stub.calls) != tc.wantUpscaleCalls {
				t.Errorf("upscale calls: got %d, want %d (calls=%v)",
					len(stub.calls), tc.wantUpscaleCalls, stub.calls)
			}
			if len(stub.optimizeCalls) != tc.wantOptimizeCals {
				t.Errorf("optimize calls: got %d, want %d (calls=%v)",
					len(stub.optimizeCalls), tc.wantOptimizeCals, stub.optimizeCalls)
			}
		})
	}
}
