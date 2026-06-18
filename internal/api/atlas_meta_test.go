package api

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/auth"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

const (
	atlasTestRelMBID = "11111111-1111-4111-8111-111111111111"
	atlasTestArtMBID = "22222222-2222-4222-8222-222222222222"
)

func newAtlasMetaTestServer(t *testing.T, enabled bool) (token string, srv *Server) {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{LibraryRoots: []string{t.TempDir()}, ListenAddress: ":7788", LibraryName: "T"}
	authStore, _ := auth.OpenStore(filepath.Join(dir, "tokens.json"))
	raw, _, _ := authStore.Mint("test")
	mstore, _ := manifest.OpenStore(filepath.Join(dir, "bridge.db"))
	t.Cleanup(func() { _ = mstore.Close() })
	return raw, New(cfg, authStore, nil, "fp").WithAtlasMeta(enabled, 30*24*time.Hour, mstore)
}

func featuresContain(feats []string, want string) bool {
	for _, f := range feats {
		if f == want {
			return true
		}
	}
	return false
}

func TestAtlasIngestAndMetaRoundTrip(t *testing.T) {
	token, srv := newAtlasMetaTestServer(t, true)
	body := `{"release":{"mbid":"` + atlasTestRelMBID + `","found":true,"description":"D","recordLabel":"L","genres":["Jazz"],"descriptionSource":"bandcamp","descriptionSourceUrl":"https://x.bandcamp.com/album/y"},` +
		`"artist":{"mbid":"` + atlasTestArtMBID + `","found":true,"bio":"B","bioSummary":"S","bioSource":"wiki","bioSourceUrl":"https://en.wikipedia.org/wiki/Z"}}`
	resp := doReq(t, srv, http.MethodPost, "/v1/atlas-ingest", token, "", body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("ingest status = %d, want 200", resp.StatusCode)
	}
	var ir atlasIngestResponse
	json.NewDecoder(resp.Body).Decode(&ir)
	resp.Body.Close()
	if !ir.ReleaseIngested || !ir.ArtistIngested {
		t.Errorf("ingest result = %+v, want both true", ir)
	}

	gresp := doReq(t, srv, http.MethodGet, "/v1/atlas-meta/release/"+atlasTestRelMBID, token, "", "")
	if gresp.StatusCode != http.StatusOK {
		t.Fatalf("meta release status = %d, want 200", gresp.StatusCode)
	}
	var mr atlasMetaResponse
	json.NewDecoder(gresp.Body).Decode(&mr)
	gresp.Body.Close()
	if !mr.Found || mr.Description != "D" || mr.RecordLabel != "L" || len(mr.Genres) != 1 || mr.TTLSeconds <= 0 {
		t.Errorf("meta release = %+v", mr)
	}
	if mr.Source != "bandcamp" || mr.SourceURL != "https://x.bandcamp.com/album/y" {
		t.Errorf("release attribution = (%q, %q), want (bandcamp, …)", mr.Source, mr.SourceURL)
	}

	aresp := doReq(t, srv, http.MethodGet, "/v1/atlas-meta/artist/"+atlasTestArtMBID, token, "", "")
	var ar atlasMetaResponse
	json.NewDecoder(aresp.Body).Decode(&ar)
	aresp.Body.Close()
	if !ar.Found || ar.Bio != "B" || ar.BioSummary != "S" {
		t.Errorf("meta artist = %+v", ar)
	}
	if ar.Source != "wiki" || ar.SourceURL != "https://en.wikipedia.org/wiki/Z" {
		t.Errorf("artist attribution = (%q, %q), want (wiki, …)", ar.Source, ar.SourceURL)
	}
}

func TestAtlasMetaAbsentIs404(t *testing.T) {
	token, srv := newAtlasMetaTestServer(t, true)
	resp := doReq(t, srv, http.MethodGet, "/v1/atlas-meta/release/33333333-3333-4333-8333-333333333333", token, "", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("absent meta = %d, want 404 (never checked)", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestAtlasMetaTombstoneIs200(t *testing.T) {
	token, srv := newAtlasMetaTestServer(t, true)
	doReq(t, srv, http.MethodPost, "/v1/atlas-ingest", token, "",
		`{"release":{"mbid":"`+atlasTestRelMBID+`","found":false}}`).Body.Close()
	resp := doReq(t, srv, http.MethodGet, "/v1/atlas-meta/release/"+atlasTestRelMBID, token, "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("tombstone meta = %d, want 200", resp.StatusCode)
	}
	var mr atlasMetaResponse
	json.NewDecoder(resp.Body).Decode(&mr)
	resp.Body.Close()
	if mr.Found {
		t.Error("tombstone meta has found=true, want false")
	}
}

func TestAtlasIngestValidation(t *testing.T) {
	token, srv := newAtlasMetaTestServer(t, true)
	cases := []struct {
		name string
		body string
		want int
	}{
		{"empty body", `{}`, http.StatusBadRequest},
		{"bad mbid", `{"release":{"mbid":"not-a-uuid","found":true}}`, http.StatusBadRequest},
		{"oversized description", `{"release":{"mbid":"` + atlasTestRelMBID + `","found":true,"description":"` + strings.Repeat("x", 17000) + `"}}`, http.StatusBadRequest},
		{"valid", `{"release":{"mbid":"` + atlasTestRelMBID + `","found":true}}`, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := doReq(t, srv, http.MethodPost, "/v1/atlas-ingest", token, "", tc.body)
			if resp.StatusCode != tc.want {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.want)
			}
			resp.Body.Close()
		})
	}
}

func TestAtlasFeatureOffReturns404(t *testing.T) {
	token, srv := newAtlasMetaTestServer(t, false) // disabled
	resp := doReq(t, srv, http.MethodGet, "/v1/atlas-meta/release/"+atlasTestRelMBID, token, "", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("feature-off meta = %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()
	iresp := doReq(t, srv, http.MethodPost, "/v1/atlas-ingest", token, "",
		`{"release":{"mbid":"`+atlasTestRelMBID+`","found":true}}`)
	if iresp.StatusCode != http.StatusNotFound {
		t.Errorf("feature-off ingest = %d, want 404", iresp.StatusCode)
	}
	iresp.Body.Close()
}

func TestHealthAdvertisesAtlasEnrichment(t *testing.T) {
	token, srvOn := newAtlasMetaTestServer(t, true)
	resp := doReq(t, srvOn, http.MethodGet, "/v1/health", token, "", "")
	var h HealthResponse
	json.NewDecoder(resp.Body).Decode(&h)
	resp.Body.Close()
	if !featuresContain(h.Features, "atlasEnrichment") {
		t.Errorf("features = %v, want atlasEnrichment", h.Features)
	}
	if len(h.Features) > 0 && h.Features[0] != "atlasEnrichment" {
		t.Errorf("atlasEnrichment should sort first; features[0]=%q", h.Features[0])
	}
	for i := 1; i < len(h.Features); i++ {
		if h.Features[i-1] > h.Features[i] {
			t.Errorf("features not alpha-sorted at %d: %q > %q", i, h.Features[i-1], h.Features[i])
		}
	}

	_, srvOff := newAtlasMetaTestServer(t, false)
	r2 := doReq(t, srvOff, http.MethodGet, "/v1/health", "", "", "")
	var h2 HealthResponse
	json.NewDecoder(r2.Body).Decode(&h2)
	r2.Body.Close()
	if featuresContain(h2.Features, "atlasEnrichment") {
		t.Errorf("disabled bridge advertised atlasEnrichment: %v", h2.Features)
	}
}
