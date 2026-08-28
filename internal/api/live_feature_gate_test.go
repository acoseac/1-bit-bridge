package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/auth"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// stubSmartPlaylistStore is wired but empty — enough to answer.
type stubSmartPlaylistStore struct{}

func (stubSmartPlaylistStore) LoadSmartPlaylists(context.Context) ([]manifest.StoredSmartPlaylist, error) {
	return nil, nil
}

// TestSmartPlaylistsHealthFlagAndEndpointMoveTogether is the
// never-split-a-field's-halves rule, enforced where it would actually
// break.
//
// The health flag and the endpoint are two consumers of one setting. If
// only one of them reads it live, a bridge advertises `smartPlaylists`
// while the endpoint 404s (or the reverse: hides a working feature).
// Both keying off smartPlaylistsActive() is what makes that impossible;
// this asserts they agree in all four combinations of wired × enabled.
func TestSmartPlaylistsHealthFlagAndEndpointMoveTogether(t *testing.T) {
	cases := []struct {
		name    string
		wired   bool
		enabled *bool // nil = no live predicate at all
		want    bool
	}{
		{"wired, no predicate", true, nil, true},
		{"wired and enabled", true, boolPtr(true), true},
		{"wired but disabled", true, boolPtr(false), false},
		{"not wired", false, boolPtr(true), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newTestServerForFeatureGate(t)
			if tc.wired {
				srv.WithSmartPlaylistStore(stubSmartPlaylistStore{})
			}
			if tc.enabled != nil {
				v := *tc.enabled
				srv.WithSmartPlaylistEnabled(func() bool { return v })
			}

			advertised := healthAdvertises(t, srv, "smartPlaylists")
			if advertised != tc.want {
				t.Errorf("health advertises smartPlaylists = %v, want %v", advertised, tc.want)
			}

			rec := httptest.NewRecorder()
			req := httptest.NewRequest("GET", "/v1/smart-playlists", nil)
			srv.smartPlaylists(rec, req)
			answered := rec.Code != 404
			if answered != tc.want {
				t.Errorf("endpoint answered = %v (code %d), want %v", answered, rec.Code, tc.want)
			}
			if advertised != answered {
				t.Errorf("health says %v but the endpoint says %v — a client is being told "+
					"one thing and served another", advertised, answered)
			}
		})
	}
}

// TestSmartPlaylistsGateIsReadLive — the predicate is consulted per
// request, not captured. A gate read once is exactly as restart-bound as
// the boot boolean it replaced, and the type signature would not say so.
func TestSmartPlaylistsGateIsReadLive(t *testing.T) {
	on := true
	srv := newTestServerForFeatureGate(t)
	srv.WithSmartPlaylistStore(stubSmartPlaylistStore{}).
		WithSmartPlaylistEnabled(func() bool { return on })

	if !healthAdvertises(t, srv, "smartPlaylists") {
		t.Fatal("expected the flag while on")
	}
	on = false
	if healthAdvertises(t, srv, "smartPlaylists") {
		t.Error("flag survived the predicate flipping — it is being cached somewhere")
	}
	on = true
	if !healthAdvertises(t, srv, "smartPlaylists") {
		t.Error("flag did not come back; the gate has to work in both directions")
	}
}

// TestCarPlayOptimizeGateIsReadLive — same contract for the other feature
// converted in this PR. The pre-generation sweeper reads the setting live,
// so a captured health flag would keep advertising a capability the
// operator switched off while the sweeper had already stopped.
func TestCarPlayOptimizeGateIsReadLive(t *testing.T) {
	on := false
	srv := newTestServerForFeatureGate(t)
	srv.WithUpscale(func() bool { return true }, nil).WithCarPlayOptimize(func() bool { return on })

	if healthAdvertises(t, srv, "carPlayOptimize") {
		t.Fatal("advertised while off")
	}
	on = true
	if !healthAdvertises(t, srv, "carPlayOptimize") {
		t.Error("flipping the predicate on did not reach the health response")
	}
}

// TestNilCarPlayOptimizeReadsAsOff — the field is now a func, and a nil
// func must not panic the health handler.
func TestNilCarPlayOptimizeReadsAsOff(t *testing.T) {
	srv := newTestServerForFeatureGate(t)
	srv.WithUpscale(func() bool { return true }, nil)
	if healthAdvertises(t, srv, "carPlayOptimize") {
		t.Error("a nil predicate must read as off")
	}
}

// --- helpers ---

func boolPtr(b bool) *bool { return &b }

// newTestServerForFeatureGate builds a bare Server; these tests call the
// handlers directly, so no mux or auth is needed.
func newTestServerForFeatureGate(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	lib := filepath.Join(dir, "Music")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	store, err := auth.OpenStore(filepath.Join(dir, "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{LibraryRoots: []string{lib}, ListenAddress: ":7788", LibraryName: "T"}
	return New(cfg, store, nil, "fp")
}

// healthAdvertises reports whether /v1/health lists the named feature.
func healthAdvertises(t *testing.T, srv *Server, feature string) bool {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.health(rec, httptest.NewRequest("GET", "/v1/health", nil))
	if rec.Code != 200 {
		t.Fatalf("health: %d", rec.Code)
	}
	var resp struct {
		Features []string `json:"features"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode health: %v", err)
	}
	for _, f := range resp.Features {
		if f == feature {
			return true
		}
	}
	return false
}

// TestUpscaleAndAnalysisGatesAreReadLive — the pools are constructed
// unconditionally now, so "wired" no longer stands in for "on". A gate
// read once would be exactly as restart-bound as the boot boolean it
// replaced, and the type signature would not say so.
func TestUpscaleAndAnalysisGatesAreReadLive(t *testing.T) {
	up, an := false, false
	srv := newTestServerForFeatureGate(t)
	srv.WithUpscale(func() bool { return up }, nil).
		WithAnalysis(func() bool { return an }, nil)

	if healthAdvertises(t, srv, "waveform") {
		t.Fatal("waveform advertised while analysis is off")
	}
	an = true
	if !healthAdvertises(t, srv, "waveform") {
		t.Error("flipping analysis on did not reach /v1/health")
	}
	an = false
	if healthAdvertises(t, srv, "waveform") {
		t.Error("flipping analysis back off did not reach /v1/health — the gate has to " +
			"work in both directions")
	}

	// Probe the top-level `upscaleEnabled` field rather than a feature
	// flag: every upscale FLAG additionally requires an adapter this bare
	// server has none of (operatorDrivenUpscale needs a batch coordinator,
	// deleteVariants a variant deleter), so a flag would stay absent for a
	// reason unrelated to the gate under test.
	up = true
	if !healthUpscaleEnabled(t, srv) {
		t.Error("flipping upscale on did not reach /v1/health")
	}
	up = false
	if healthUpscaleEnabled(t, srv) {
		t.Error("flipping upscale back off did not reach /v1/health")
	}
}

// TestNilFeatureGatesReadAsOff — the fields are funcs now, and a nil one
// must not panic the health handler.
func TestNilFeatureGatesReadAsOff(t *testing.T) {
	srv := newTestServerForFeatureGate(t)
	if srv.upscaleActive() || srv.analysisActive() {
		t.Error("nil predicates must read as off")
	}
	// And the handler must survive them.
	if healthAdvertises(t, srv, "waveform") {
		t.Error("waveform advertised with a nil analysis gate")
	}
}

// healthUpscaleEnabled reads the top-level `upscaleEnabled` boolean.
func healthUpscaleEnabled(t *testing.T, srv *Server) bool {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.health(rec, httptest.NewRequest("GET", "/v1/health", nil))
	if rec.Code != 200 {
		t.Fatalf("health: %d", rec.Code)
	}
	var resp struct {
		UpscaleEnabled *bool `json:"upscaleEnabled"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode health: %v", err)
	}
	return resp.UpscaleEnabled != nil && *resp.UpscaleEnabled
}
