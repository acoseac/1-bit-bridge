package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/auth"
	"github.com/acoseac/1-bit-bridge/internal/config"
)

// inactiveUpscaleFixture builds the state production spent a release in and
// no fixture could previously express: the enqueuer and the variant deleter
// are WIRED, while the live feature predicate says the feature is off.
//
// That combination is not exotic — it is the default. The wiring layer builds
// both unconditionally ("always construct, never stop", so the flag can be
// hot), and `upscale.enabled` defaults to false, so every stock bridge runs
// exactly this shape.
func gateFixture(t *testing.T, upscaleOn, optimizeOn bool) (*httptest.Server, string, *stubEnqueuer) {
	t.Helper()
	tmp := t.TempDir()
	root := filepath.Join(tmp, "Music")
	if err := os.MkdirAll(filepath.Join(root, "Artist/Album"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash("Artist/Album/01.flac")),
		[]byte("not really a flac"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{LibraryRoots: []string{root}, ListenAddress: ":7788", LibraryName: "Test"}
	store, _ := auth.OpenStore(filepath.Join(tmp, "tokens.json"))
	raw, _, _ := store.Mint("test")

	stub := newStubEnqueuer()
	srv := New(cfg, store, nil, "fp").
		WithUpscaleEnqueuer(stub).
		WithVariantDeleter(&stubVariantDeleter{all: []VariantSummary{}, byPath: map[string][]VariantSummary{}}).
		WithUpscale(func() bool { return upscaleOn }, nil).
		WithCarPlayOptimize(func() bool { return optimizeOn })

	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	return hs, raw, stub
}

// inactiveUpscaleFixture is the both-flags-off case, named because that is the
// state the defect lived in and most tests here want it.
func inactiveUpscaleFixture(t *testing.T) (*httptest.Server, string, *stubEnqueuer) {
	t.Helper()
	return gateFixture(t, false, false)
}

// TestUpscaleRefusedWhenFeatureInactive pins the gate that the
// always-construct-never-stop conversion (PR #781) silently removed.
//
// Both mutation handlers used to gate on their adapter being nil, and that
// WAS a real gate while the wiring only built the adapter when the feature
// was active. Once the wiring became unconditional, nil-ness stopped meaning
// anything: `POST /v1/upscale` enqueued real sox jobs — writing
// `track_variants` rows and FLAC sidecars, with no disk floor on this path —
// on a bridge whose own `/v1/health` reported `upscaleEnabled: false`.
//
// The assertion is on BOTH the status code and the enqueuer, deliberately.
// A 503 alone would still pass if the handler refused only after dispatching,
// and the enqueue is the half with the side effects.
func TestUpscaleRefusedWhenFeatureInactive(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"", "upscale", "optimize"} {
		t.Run("kind="+kind, func(t *testing.T) {
			hs, tok, stub := inactiveUpscaleFixture(t)
			resp := postJSON(t, hs, "/v1/upscale", tok,
				UpscaleRequest{Path: "Artist/Album/01.flac", Kind: kind})
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusServiceUnavailable {
				t.Errorf("status: got %d, want 503 — an inactive feature must refuse its own mutation path",
					resp.StatusCode)
			}
			var env ErrorResponse
			if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
				t.Fatalf("decode error envelope: %v", err)
			}
			if env.Error != errCodeUpscaleDisabled {
				t.Errorf("wire error code: got %q, want %q", env.Error, errCodeUpscaleDisabled)
			}
			if len(stub.calls) != 0 || len(stub.optimizeCalls) != 0 {
				t.Errorf("an inactive feature enqueued work: upscale=%v optimize=%v",
					stub.calls, stub.optimizeCalls)
			}
		})
	}
}

// TestUpscaleDeleteRefusedWhenFeatureInactive is the delete half of the same
// gate. It answers 404 rather than 503 because that is what this endpoint has
// always returned for "no variant surface here" — the point is that a wired
// deleter no longer implies an active feature.
func TestUpscaleDeleteRefusedWhenFeatureInactive(t *testing.T) {
	t.Parallel()
	hs, tok, _ := inactiveUpscaleFixture(t)
	resp := authDelete(t, hs, "/v1/upscale/variants?confirm=all", tok)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status: got %d, want 404 — an inactive feature must refuse the delete path", resp.StatusCode)
	}
}

// TestUpscaleGateRunsBeforePathResolution pins the ORDER, not just the answer.
//
// The kind routing used to sit after `ResolveChecked` and the recursive
// folder walk, so a request that was going to be refused first cost a full
// WalkDir over whatever directory it named. (CodeRabbit, PR #852.)
//
// A nonexistent path is the cheap way to observe the order without timing
// anything: if the gate runs first the answer is 503, and if path resolution
// runs first it is 404. Both are "refused", which is exactly why asserting on
// the status alone in the other tests could not have caught this.
func TestUpscaleGateRunsBeforePathResolution(t *testing.T) {
	t.Parallel()
	hs, tok, stub := inactiveUpscaleFixture(t)
	resp := postJSON(t, hs, "/v1/upscale", tok,
		UpscaleRequest{Path: "No/Such/Directory", Kind: "optimize"})
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		t.Error("got 404: the path was resolved before the feature gate ran, so a refused " +
			"request still pays for a resolve and, on a real directory, a full recursive walk")
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status: got %d, want 503", resp.StatusCode)
	}
	if len(stub.calls) != 0 || len(stub.optimizeCalls) != 0 {
		t.Errorf("enqueued work: upscale=%v optimize=%v", stub.calls, stub.optimizeCalls)
	}
}

// TestUpscaleOptimizeKindRefusedWhenOnlyOptimizeIsOff pins the narrower arm:
// the master toggle is on, but the CarPlay optimize KIND is off. The upscale
// kind must still be accepted, or the two flags have collapsed into one.
func TestUpscaleOptimizeKindRefusedWhenOnlyOptimizeIsOff(t *testing.T) {
	t.Parallel()
	hs, raw, stub := gateFixture(t, true /* upscale on */, false /* optimize off */)

	resp := postJSON(t, hs, "/v1/upscale", raw,
		UpscaleRequest{Path: "Artist/Album/01.flac", Kind: "optimize"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("optimize status: got %d, want 503", resp.StatusCode)
	}
	if len(stub.optimizeCalls) != 0 {
		t.Errorf("optimize enqueued while the kind is disabled: %v", stub.optimizeCalls)
	}

	resp2 := postJSON(t, hs, "/v1/upscale", raw,
		UpscaleRequest{Path: "Artist/Album/01.flac", Kind: "upscale"})
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusAccepted {
		t.Errorf("upscale status: got %d, want 202 — the optimize flag must not gate the upscale kind", resp2.StatusCode)
	}
}
