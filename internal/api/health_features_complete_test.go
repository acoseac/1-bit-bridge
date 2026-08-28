package api

import (
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/auth"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// wantAllHealthFeatures is the COMPLETE set the feats builder can emit,
// in the alpha order it emits them. It is the executable twin of the
// capacity comment on `feats := make([]string, 0, N)` in api.go.
//
// Keep this list, that comment, and that N in step. The list is what makes
// the number checkable: `trackQuality` was appended by the builder but
// missing from the comment's enumeration — and therefore from the count —
// from the flag's introduction until 2026-08-16, because every existing
// health test asserted either a MINIMUM set or one flag at a time. Nothing
// exercised all the gates at once, so nothing could notice.
var wantAllHealthFeatures = []string{
	"atlasEnrichment",
	"booklets",
	"carPlayOptimize",
	"deleteVariants",
	"demoMode",
	"diagnosticsSummary",
	"dlnaServer",
	"favorites",
	"keyTempo",
	"loudness",
	"operatorDrivenUpscale",
	"pairingEventsSupported",
	"playbackHistory",
	"playbackHistoryRead",
	"playlistBackup",
	"playlistsCrossDevice",
	"pushEventsSupported",
	"rendererDiscovery",
	"smartPlaylists",
	"spectrum",
	"trackQuality",
	"upscaleCompleteEvents",
	"variantBumpsIndex",
	"waveform",
}

// newAllFeaturesServer wires EVERY optional surface the feats builder
// gates on, so /v1/health advertises the maximum set.
//
// One real manifest.Store backs the store-shaped gates (it satisfies all
// of them); the rest take the package's existing stubs. A future flag whose
// gate is not wired here will make TestHealthFeaturesCompleteSet fail with
// the missing name rather than silently shrink the assertion — which is the
// point of asserting the exact set instead of a subset.
func newAllFeaturesServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{
		LibraryRoots:  []string{t.TempDir()},
		ListenAddress: ":7788",
		LibraryName:   "T",
	}
	authStore, err := auth.OpenStore(filepath.Join(dir, "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := authStore.Mint("test"); err != nil {
		t.Fatal(err)
	}
	mstore, err := manifest.OpenStore(filepath.Join(dir, "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mstore.Close() })

	srv := New(cfg, authStore, nil, "fp").
		WithAtlasMeta(true, time.Hour, mstore).
		WithBooklets(mstore, t.TempDir(), func(string) {}).
		WithUpscale(func() bool { return true }, newStubVariantStore()).
		WithCarPlayOptimize(func() bool { return true }).
		WithVariantDeleter(&stubVariantDeleter{}).
		WithBatchCoordinator(&stubBatchCoordinator{}).
		WithDLNA(true).
		WithRendererDiscovery(&stubRendererDiscovery{}).
		WithFavoritesStore(mstore).
		WithAnalysis(func() bool { return true }, stubAnalysisStore{}).
		WithHistoryStore(mstore).
		WithPlaylistStore(mstore).
		WithSmartPlaylistStore(mstore).
		WithDemoMode(true).
		WithPairing(newPairingStoreForFeaturesTest(t, authStore))
	t.Cleanup(srv.StartEventBroker())
	return srv
}

// TestHealthFeaturesCompleteSet pins the exact maximum feature set.
//
// The sibling tests each assert one flag, or a minimum set plus alpha
// ordering. Neither shape can catch a flag that is emitted but uncounted,
// or one silently dropped from the builder — only an exact-set assertion
// with every gate open can.
func TestHealthFeaturesCompleteSet(t *testing.T) {
	srv := newAllFeaturesServer(t)
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)

	resp := authGet(t, hs, "/v1/health", "")
	body := readAllOrFail(t, resp)
	resp.Body.Close()

	var got HealthResponse
	if err := jsonUnmarshalForTest(body, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Exact set, in order. Compared element-wise so a failure names the
	// first divergence rather than dumping two slices.
	if len(got.Features) != len(wantAllHealthFeatures) {
		t.Errorf("advertised %d features, want %d\n got: %v\nwant: %v",
			len(got.Features), len(wantAllHealthFeatures), got.Features, wantAllHealthFeatures)
	}
	for i := 0; i < len(got.Features) && i < len(wantAllHealthFeatures); i++ {
		if got.Features[i] != wantAllHealthFeatures[i] {
			t.Fatalf("feature[%d] = %q, want %q\n got: %v\nwant: %v",
				i, got.Features[i], wantAllHealthFeatures[i], got.Features, wantAllHealthFeatures)
		}
	}
	// Redundant with the exact match, but names the invariant the builder's
	// comment actually claims ("each conditional appends in lex order"), so a
	// reordering failure reads as a reordering rather than a set change.
	assertAlphaSorted(t, got.Features)
}
