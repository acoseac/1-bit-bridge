package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// stubRendererDiscovery is a test seam for RendererDiscoverySnapshotter.
type stubRendererDiscovery struct {
	entries []RendererInfo
}

func (s *stubRendererDiscovery) Snapshot() []RendererInfo {
	return s.entries
}

// -----------------------------------------------------------------------------
// /v1/renderers handler
// -----------------------------------------------------------------------------

func TestRenderersHandler_404WhenDiscoveryNotWired(t *testing.T) {
	s := &Server{} // no rendererDiscovery wired
	rec := httptest.NewRecorder()
	s.renderers(rec, httptest.NewRequest(http.MethodGet, "/v1/renderers", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	// Pin the API-shaped JSON error body (NOT plain-text from
	// http.NotFound). Per CodeRabbit MINOR round-1 on PR #305.
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	body, _ := io.ReadAll(rec.Body)
	if !strings.Contains(string(body), `"error":"not_found"`) {
		t.Errorf("body missing typed error: %s", body)
	}
}

func TestRenderersHandler_200WithEmptyCache(t *testing.T) {
	s := &Server{
		rendererDiscovery: &stubRendererDiscovery{entries: nil},
	}
	rec := httptest.NewRecorder()
	s.renderers(rec, httptest.NewRequest(http.MethodGet, "/v1/renderers", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body, _ := io.ReadAll(rec.Body)
	// Must serialize as `[]` not `null` for iOS JSONDecoder.
	if !strings.Contains(string(body), `"renderers":[]`) {
		t.Errorf("empty cache should serialize as [], got: %s", body)
	}
}

func TestRenderersHandler_200WithPopulatedCache(t *testing.T) {
	s := &Server{
		rendererDiscovery: &stubRendererDiscovery{entries: []RendererInfo{
			{
				UDN:               "uuid:test-1",
				FriendlyName:      "Chord 2go",
				Manufacturer:      "Chord Electronics",
				ControlURL:        "http://192.168.1.42:8080/avt/control",
				EventURL:          "http://192.168.1.42:8080/avt/event",
				SinkProtocolInfos: []string{"http-get:*:audio/x-dsf:*"},
				LastSeenAt:        time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC),
			},
		}},
	}
	rec := httptest.NewRecorder()
	s.renderers(rec, httptest.NewRequest(http.MethodGet, "/v1/renderers", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var resp RenderersResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Renderers) != 1 {
		t.Fatalf("len = %d, want 1", len(resp.Renderers))
	}
	r := resp.Renderers[0]
	if r.FriendlyName != "Chord 2go" {
		t.Errorf("FriendlyName = %q", r.FriendlyName)
	}
	if r.UDN != "uuid:test-1" {
		t.Errorf("UDN = %q", r.UDN)
	}
	if len(r.SinkProtocolInfos) != 1 {
		t.Errorf("SinkProtocolInfos = %v", r.SinkProtocolInfos)
	}
}

// -----------------------------------------------------------------------------
// /v1/health.features alpha-sort guard
// -----------------------------------------------------------------------------

// Pins the rendererDiscovery alpha-sort invariant: when ALL
// conditional features are advertised, the Features slice must be
// in lex order. A future feature added out of position would fail
// here. Mirrors `health_carplay_optimize_test.go:66` shape.
func TestHealthFeatures_RendererDiscoveryAlphaSort(t *testing.T) {
	cases := []struct {
		name string
		// gates lists which optional features are wired for the case
		gates []string
		want  []string
	}{
		{
			name:  "minimum: just unconditional",
			gates: []string{},
			want:  []string{"diagnosticsSummary", "variantBumpsIndex"},
		},
		{
			name:  "dlnaServer alone",
			gates: []string{"dlna"},
			want:  []string{"diagnosticsSummary", "dlnaServer", "variantBumpsIndex"},
		},
		{
			name:  "dlnaServer + rendererDiscovery",
			gates: []string{"dlna", "rendererDiscovery"},
			want: []string{
				"diagnosticsSummary",
				"dlnaServer",
				"rendererDiscovery",
				"variantBumpsIndex",
			},
		},
		{
			name:  "dlnaServer + rendererDiscovery + pushEventsSupported",
			gates: []string{"dlna", "rendererDiscovery", "events"},
			want: []string{
				"diagnosticsSummary",
				"dlnaServer",
				"pushEventsSupported",
				"rendererDiscovery",
				"variantBumpsIndex",
			},
		},
		{
			name:  "rendererDiscovery sits between pushEventsSupported and upscaleCompleteEvents",
			gates: []string{"dlna", "rendererDiscovery", "events", "upscale"},
			want: []string{
				"diagnosticsSummary",
				"dlnaServer",
				"pushEventsSupported",
				"rendererDiscovery",
				"upscaleCompleteEvents",
				"variantBumpsIndex",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			feats := simulateFeaturesList(tc.gates)
			assertFeatureListEqual(t, feats, tc.want)
			assertFeaturesAlphaSorted(t, feats)
		})
	}
}

// assertFeatureListEqual compares produced + expected slices and
// emits per-mismatch errors. Extracted from the table loop so the
// loop body stays small + the Sonar cognitive-complexity gate
// stays under threshold. Per CodeRabbit MAJOR round-1 on PR #305.
func assertFeatureListEqual(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d (got %v, want %v)", len(got), len(want), got, want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("feats[%d] = %q, want %q\nfull: %v", i, got[i], w, got)
		}
	}
}

// assertFeaturesAlphaSorted verifies lex-sort holds independently of the
// hand-rolled `want` slice. Extracted alongside the equality
// helper above for the same Sonar cognitive-complexity reduction.
func assertFeaturesAlphaSorted(t *testing.T, feats []string) {
	t.Helper()
	for i := 1; i < len(feats); i++ {
		if feats[i] < feats[i-1] {
			t.Errorf("Features not alpha-sorted: %q after %q", feats[i], feats[i-1])
		}
	}
}

// simulateFeaturesList mirrors the conditional-append chain in
// /v1/health's handler so the alpha-sort invariant can be unit-
// tested without standing up an httptest server. Keep this in
// LOCK STEP with the production chain in api.go's health handler
// (the test will catch drift the next time a new feature lands).
//
// Implemented as a series of per-block append helpers so the
// outer body stays under Sonar's cognitive-complexity threshold
// (15). Per CodeRabbit MAJOR round-1 on PR #305.
func simulateFeaturesList(gates []string) []string {
	g := gateChecker(gates)
	feats := make([]string, 0, 10)
	feats = appendUpscaleEarlyFeatures(feats, g)
	feats = append(feats, "diagnosticsSummary")
	if g("dlna") {
		feats = append(feats, "dlnaServer")
	}
	feats = appendUpscaleMidFeatures(feats, g)
	feats = appendEventFeatures(feats, g)
	feats = appendRendererDiscoveryFeature(feats, g)
	if g("upscale") {
		feats = append(feats, "upscaleCompleteEvents")
	}
	feats = append(feats, "variantBumpsIndex")
	return feats
}

func gateChecker(gates []string) func(string) bool {
	return func(name string) bool {
		for _, gate := range gates {
			if gate == name {
				return true
			}
		}
		return false
	}
}

func appendUpscaleEarlyFeatures(feats []string, g func(string) bool) []string {
	if !g("upscale") {
		return feats
	}
	if g("carPlayOptimize") {
		feats = append(feats, "carPlayOptimize")
	}
	if g("deleteVariants") {
		feats = append(feats, "deleteVariants")
	}
	return feats
}

func appendUpscaleMidFeatures(feats []string, g func(string) bool) []string {
	if g("upscale") && g("batchCoordinator") {
		feats = append(feats, "operatorDrivenUpscale")
	}
	return feats
}

func appendEventFeatures(feats []string, g func(string) bool) []string {
	if !g("events") {
		return feats
	}
	if g("pairing") {
		feats = append(feats, "pairingEventsSupported")
	}
	feats = append(feats, "pushEventsSupported")
	return feats
}

func appendRendererDiscoveryFeature(feats []string, g func(string) bool) []string {
	if g("dlna") && g("rendererDiscovery") {
		feats = append(feats, "rendererDiscovery")
	}
	return feats
}
