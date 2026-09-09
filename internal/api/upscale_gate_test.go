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
	hs, raw, stub, _ := gateFixtureWithBatch(t, upscaleOn, optimizeOn)
	return hs, raw, stub
}

// gateFixtureWithBatch is gateFixture plus a handle on the batch coordinator,
// for the tests that need to assert the coordinator was never reached.
func gateFixtureWithBatch(t *testing.T, upscaleOn, optimizeOn bool) (*httptest.Server, string, *stubEnqueuer, *stubBatchCoordinator) {
	t.Helper()
	batchStub := &stubBatchCoordinator{}
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
	// The batch coordinator is wired here for the same reason the enqueuer
	// and the deleter are: production wires all three unconditionally, so a
	// fixture that leaves one nil cannot observe the live gate at all. That
	// is exactly why the batch hole survived PR #852 — upscale_batch_test.go
	// builds its server without WithUpscale, so upscaleActive() was false
	// there and its tests asserted 202 on a bridge with the feature off.
	srv := New(cfg, store, nil, "fp").
		WithUpscaleEnqueuer(stub).
		WithVariantDeleter(&stubVariantDeleter{all: []VariantSummary{}, byPath: map[string][]VariantSummary{}}).
		WithBatchCoordinator(batchStub).
		WithUpscale(func() bool { return upscaleOn }, nil).
		WithCarPlayOptimize(func() bool { return optimizeOn })

	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	return hs, raw, stub, batchStub
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

// TestUpscaleBatchRefusedWhenFeatureInactive is the third handler of the same
// class, and the one PR #852 did not enumerate.
//
// A batch walks the WHOLE LIBRARY, so this was the largest-scope way for a
// bearer-token holder to start real sox work on a bridge advertising
// `upscaleEnabled: false` — and each finished job's UpsertVariant
// strict-advances indexed_at, making it a whole-library delta to every paired
// device as well.
//
// Asserting on the coordinator, not only the status: a 503 returned AFTER the
// library walk would still be a walk the request should never have caused.
func TestUpscaleBatchRefusedWhenFeatureInactive(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"", "upscale", "optimize", "pcm"} {
		t.Run("kind="+kind, func(t *testing.T) {
			hs, tok, _, batch := gateFixtureWithBatch(t, false, false)
			resp := postJSON(t, hs, "/v1/upscale/batch", tok,
				BatchRequest{Path: "", Kind: kind})
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusServiceUnavailable {
				t.Errorf("status: got %d, want 503 — an inactive feature must refuse the batch path",
					resp.StatusCode)
			}
			var env ErrorResponse
			if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
				t.Fatalf("decode error envelope: %v", err)
			}
			if env.Error != errCodeUpscaleDisabled {
				t.Errorf("wire error code: got %q, want %q", env.Error, errCodeUpscaleDisabled)
			}
			if batch.submits != 0 || batch.optimizes != 0 || batch.pcms != 0 {
				t.Errorf("an inactive feature walked the library: submits=%d optimizes=%d pcms=%d",
					batch.submits, batch.optimizes, batch.pcms)
			}
		})
	}
}

// TestUpscaleBatchOptimizeKindHonoursItsOwnFlag pins the per-kind half.
//
// With the master flag ON and the CarPlay optimize flag OFF, `POST /v1/upscale`
// answered 503 and `POST /v1/upscale/batch` answered 202 for the same kind on
// the same bridge. The `pcm` arm beside it has always carried its own gate,
// which is what made the omission visible.
func TestUpscaleBatchOptimizeKindHonoursItsOwnFlag(t *testing.T) {
	t.Parallel()
	hs, tok, _, batch := gateFixtureWithBatch(t, true, false)
	resp := postJSON(t, hs, "/v1/upscale/batch", tok, BatchRequest{Path: "", Kind: "optimize"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status: got %d, want 503 — the optimize kind has its own flag", resp.StatusCode)
	}
	if batch.optimizes != 0 {
		t.Errorf("a disabled kind reached the coordinator: optimizes=%d", batch.optimizes)
	}
}

// TestUpscaleBatchAcceptedWhenActive is the negative control for the two tests
// above: with both flags on, the same requests reach the coordinator. Without
// it, a handler that refused unconditionally would pass them both.
func TestUpscaleBatchAcceptedWhenActive(t *testing.T) {
	t.Parallel()
	hs, tok, _, batch := gateFixtureWithBatch(t, true, true)
	for _, kind := range []string{"upscale", "optimize"} {
		resp := postJSON(t, hs, "/v1/upscale/batch", tok, BatchRequest{Path: "", Kind: kind})
		if resp.StatusCode != http.StatusAccepted {
			t.Errorf("kind=%s status: got %d, want 202 — an ACTIVE feature must still work", kind, resp.StatusCode)
		}
		resp.Body.Close()
	}
	if batch.submits != 1 || batch.optimizes != 1 {
		t.Errorf("active feature did not reach the coordinator: submits=%d optimizes=%d",
			batch.submits, batch.optimizes)
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
