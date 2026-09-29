package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

// The upscale and analysis switches stop the bridge MAKING renditions and
// curves; they withdraw none it has made (backlog B108, PROTOCOL.md's two
// "Feature gate semantics" lists). The reads below refuse only on a missing
// STORE, and production wires both stores on every bridge (#781), so with a
// switch off they answer exactly as with it on. PROTOCOL.md said 404 there
// until 2026-09-29: a client drops a rendition or a curve on a 404, and an
// operator stopping new work has not asked for that. Each test first checks
// that the gate IS closed on the server it asks, through /v1/health, or it
// would pass on an open gate.

// healthOf decodes GET /v1/health from hs.
func healthOf(t *testing.T, hs *httptest.Server) HealthResponse {
	t.Helper()
	resp := authGet(t, hs, "/v1/health", "")
	defer resp.Body.Close()
	var h HealthResponse
	if err := jsonUnmarshalForTest(readAllOrFail(t, resp), &h); err != nil {
		t.Fatalf("decode /v1/health: %v", err)
	}
	return h
}

// analysisFlags are the /v1/health features the analysis gate controls.
var analysisFlags = []string{"keyTempo", "loudness", "spectrum", "trackQuality", "waveform"}

func requireAnalysisOff(t *testing.T, hs *httptest.Server) {
	t.Helper()
	feats := healthOf(t, hs).Features
	for _, f := range analysisFlags {
		if slices.Contains(feats, f) {
			t.Fatalf("/v1/health advertises %q: the test needs the analysis gate closed (features %v)", f, feats)
		}
	}
}

func TestARenditionOnDiskIsServedWithUpscalingOff(t *testing.T) {
	hs, tok, root, vs, sidecar := fileVariantFixtureGated(t, false)
	if h := healthOf(t, hs); h.UpscaleEnabled == nil || *h.UpscaleEnabled {
		t.Fatal("/v1/health does not report upscaleEnabled false: the test needs the gate closed")
	}
	const rel = "Artist/Album/01.flac"
	src := statSourceOrFail(t, root, rel)
	vs.records[rel+"|upscaled-v2-176400-24"] = &VariantRecord{
		SidecarPath:   sidecar,
		SourceMTimeNS: src.ModTime().UnixNano(),
		SourceSize:    src.Size(),
	}

	resp := authGet(t, hs, "/v1/download?path="+rel+"&variant=upscaled-v2-176400-24", tok)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: switching upscaling off must not withdraw a rendition on disk", resp.StatusCode)
	}
	if body := readAllOrFail(t, resp); len(body) != 512 || body[0] != 0xCC {
		t.Errorf("body is %d bytes starting %#x, want the sidecar's 512 bytes of 0xCC", len(body), body[0])
	}

	// A pair with no row, and a rendition gone stale, answer as they do
	// with the feature on.
	missing := authGet(t, hs, "/v1/download?path="+rel+"&variant=optimized-v2-48000-16", tok)
	defer missing.Body.Close()
	if missing.StatusCode != http.StatusNotFound {
		t.Errorf("no row: status = %d, want 404", missing.StatusCode)
	}
	assertWireErrorCode(t, missing, "variant_not_found")
	vs.records[rel+"|pcm-v2-176400-24"] = &VariantRecord{
		SidecarPath:   sidecar,
		SourceMTimeNS: src.ModTime().UnixNano() - 60_000_000_000,
		SourceSize:    src.Size(),
	}
	stale := authGet(t, hs, "/v1/download?path="+rel+"&variant=pcm-v2-176400-24", tok)
	defer stale.Body.Close()
	if stale.StatusCode != http.StatusGone {
		t.Errorf("stale: status = %d, want 410", stale.StatusCode)
	}
	assertWireErrorCode(t, stale, "variant_stale")
}

func TestACachedWaveformIsServedWithAnalysisOff(t *testing.T) {
	hs, tok, rel := waveformFixtureGated(t, false)
	requireAnalysisOff(t, hs)

	resp := authGet(t, hs, "/v1/waveform?path="+rel, tok)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: switching analysis off must not withdraw a cached curve", resp.StatusCode)
	}
	if body := readAllOrFail(t, resp); len(body) != 4 {
		t.Errorf("body is %d bytes, want the sidecar's 4", len(body))
	}
	etag := resp.Header.Get("ETag")
	if etag == "" {
		t.Fatal("no ETag: a client holding the curve could not revalidate it")
	}

	// A client that already holds the curve revalidates it, as with the
	// feature on.
	req, _ := http.NewRequest(http.MethodGet, hs.URL+"/v1/waveform?path="+rel, nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("If-None-Match", etag)
	again, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Body.Close()
	if again.StatusCode != http.StatusNotModified {
		t.Errorf("revalidation: status = %d, want 304", again.StatusCode)
	}
}

func TestACachedSpectrumIsServedWithAnalysisOff(t *testing.T) {
	hs, tok, rel, curve := spectrumFixtureGated(t, false, nil)
	requireAnalysisOff(t, hs)

	resp := authGet(t, hs, "/v1/spectrum?path="+rel, tok)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: switching analysis off must not withdraw a cached curve", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != string(curve) {
		t.Errorf("body is %d bytes, want the %d-byte curve verbatim", len(body), len(curve))
	}
}

// The batch list is a read of past work too: it answers with upscaling off
// and on a demo bridge, where the two routes that change a batch refuse
// (503 upscale_disabled, 403 demo_read_only). PROTOCOL.md said all three
// refused until 2026-09-29.
func TestTheBatchListAnswersWithUpscalingOffAndOnADemoBridge(t *testing.T) {
	for _, c := range []struct {
		name       string
		decorate   func(*Server) *Server
		wantCancel int
	}{
		{"upscaling off", func(s *Server) *Server { return s.WithUpscale(func() bool { return false }, nil) }, http.StatusServiceUnavailable},
		{"a demo bridge", func(s *Server) *Server { return s.WithDemoMode(true) }, http.StatusForbidden},
	} {
		t.Run(c.name, func(t *testing.T) {
			hs, tok, _ := batchFixtureWith(t, c.decorate)
			list := authGet(t, hs, "/v1/upscale/batches", tok)
			defer list.Body.Close()
			if list.StatusCode != http.StatusOK {
				t.Errorf("GET /v1/upscale/batches: status = %d, want 200", list.StatusCode)
			}
			cancel := authDelete(t, hs, "/v1/upscale/batches/00000000-0000-4000-8000-000000000001", tok)
			defer cancel.Body.Close()
			if cancel.StatusCode != c.wantCancel {
				t.Errorf("DELETE /v1/upscale/batches/{id}: status = %d, want %d (the server must refuse a change here, or the list's 200 proves nothing)",
					cancel.StatusCode, c.wantCancel)
			}
		})
	}
}
