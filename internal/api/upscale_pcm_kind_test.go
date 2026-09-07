package api

import (
	"net/http"
	"testing"
)

// TestUpscaleRequest_PCMKindRouting: `kind: "pcm"` routes every candidate
// to EnqueuePCMRender — never to the upscale or optimize enqueuers — and
// the match is case-insensitive like the other kinds.
func TestUpscaleRequest_PCMKindRouting(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"pcm", "PCM", " Pcm "} {
		t.Run(kind, func(t *testing.T) {
			hs, tok, _, stub := upscaleFixture(t, true)
			resp := postJSON(t, hs, "/v1/upscale", tok, UpscaleRequest{Path: "Artist/Album/01.flac", Kind: kind})
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusAccepted {
				t.Fatalf("status: got %d, want 202", resp.StatusCode)
			}
			if len(stub.pcmCalls) != 1 || len(stub.calls) != 0 || len(stub.optimizeCalls) != 0 {
				t.Errorf("calls: pcm=%v upscale=%v optimize=%v, want exactly one pcm call",
					stub.pcmCalls, stub.calls, stub.optimizeCalls)
			}
		})
	}
}

// TestUpscalePCMKindRefusedWhenDSDRenderOff pins the gate's PLACE as much
// as its verdict: with the renditions off, `kind: "pcm"` is 503
// upscale_disabled BEFORE path resolution — a nonexistent path gets the
// same 503, never the 404 that would say whether the path exists — and
// nothing is enqueued. With them on, the same nonexistent path is the
// ordinary 404.
func TestUpscalePCMKindRefusedWhenDSDRenderOff(t *testing.T) {
	t.Parallel()
	hs, tok, _, stub := upscaleFixtureOpts(t, true, false)
	for _, path := range []string{"Artist/Album/01.flac", "Artist/Nope/missing.dsf"} {
		resp := postJSON(t, hs, "/v1/upscale", tok, UpscaleRequest{Path: path, Kind: "pcm"})
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("%s: status = %d, want 503 (renditions off, decided before the path is resolved)", path, resp.StatusCode)
		}
		resp.Body.Close()
	}
	if len(stub.pcmCalls) != 0 {
		t.Errorf("enqueued %v with the renditions off", stub.pcmCalls)
	}
	// Control: the optimize kind is untouched by the DSD-render gate.
	resp := postJSON(t, hs, "/v1/upscale", tok, UpscaleRequest{Path: "Artist/Album/01.flac", Kind: "optimize"})
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("optimize with renditions off: status = %d, want 202", resp.StatusCode)
	}
	resp.Body.Close()

	on, tokOn, _, _ := upscaleFixtureOpts(t, true, true)
	resp = postJSON(t, on, "/v1/upscale", tokOn, UpscaleRequest{Path: "Artist/Nope/missing.dsf", Kind: "pcm"})
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("renditions on, missing path: status = %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestDSDRenderFlagAndPCMKindMoveTogether: the health flag and the kind
// gate read ONE predicate, so a bridge never advertises `dsdRender` while
// answering 503 to `pcm`, nor the reverse. Pinned in both states.
func TestDSDRenderFlagAndPCMKindMoveTogether(t *testing.T) {
	t.Parallel()
	for _, on := range []bool{true, false} {
		hs, tok, _, _ := upscaleFixtureOpts(t, true, on)
		advertised := healthFeaturesOf(t, hs, tok)["dsdRender"]
		resp := postJSON(t, hs, "/v1/upscale", tok, UpscaleRequest{Path: "Artist/Album/01.flac", Kind: "pcm"})
		accepted := resp.StatusCode == http.StatusAccepted
		resp.Body.Close()
		if advertised != on || accepted != on {
			t.Errorf("dsdRender predicate %v: health advertises=%v, pcm accepted=%v — the two must move together",
				on, advertised, accepted)
		}
	}
}
