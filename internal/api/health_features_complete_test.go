package api

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/auth"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// wantAllHealthFeatures is the set the complete-set fixture advertises, in
// the alpha order the builder emits. search is a key the builder appends
// and this fixture does not turn on: its manifest does not implement
// SearchAvailable. TestHealthFeatureCapacityCoversEveryKey is what holds
// the capacity to every key the builder appends, search included.
//
// `trackQuality` was appended by the builder but missing from the capacity
// comment's enumeration — and therefore from the count — from the flag's
// introduction until 2026-08-16, because every existing health test
// asserted either a MINIMUM set or one flag at a time. Nothing exercised
// all the gates at once, so nothing could notice.
var wantAllHealthFeatures = []string{
	"atlasEnrichment",
	"booklets",
	"carPlayOptimize",
	"deleteVariants",
	"demoMode",
	"diagnosticsSummary",
	"dlnaArtwork",
	"dlnaServer",
	"dsdRender",
	"dsdSilence",
	"favorites",
	"favoritesRevisions",
	"firstIndexedAt",
	"keyTempo",
	"loudness",
	"lyrics",
	"operatorDrivenUpscale",
	"pairingEventsSupported",
	"playbackHistory",
	"playbackHistoryRead",
	"playlistBackup",
	"playlistListRevision",
	"playlistsCrossDevice",
	"pushEventsSupported",
	"rendererDiscovery",
	"smartPlaylists",
	"spectrum",
	"syncEvents",
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

	srv := New(cfg, authStore, &firstIndexedReadyManifest{}, "fp").
		WithAtlasMeta(true, time.Hour, mstore).
		WithBooklets(mstore, t.TempDir(), func(string) {}).
		WithUpscale(func() bool { return true }, newStubVariantStore()).
		WithCarPlayOptimize(func() bool { return true }).
		WithVariantDeleter(&stubVariantDeleter{}).
		WithBatchCoordinator(&stubBatchCoordinator{}).
		WithDLNA(true).
		WithArtworkDirs(fakeArtworkDirs{dir: t.TempDir()}). // dlnaArtwork = dlnaServer AND an artwork dir
		WithDSDRender(func() bool { return true }).
		WithRendererDiscovery(&stubRendererDiscovery{}).
		WithFavoritesStore(mstore).
		WithAnalysis(func() bool { return true }, stubAnalysisStore{}).
		WithLyrics(stubLyricsStore{}).
		WithHistoryStore(mstore).
		WithPlaylistStore(mstore).
		WithSmartPlaylistStore(mstore).
		WithDemoMode(true).
		WithPairing(newPairingStoreForFeaturesTest(t, authStore))
	srv.EnableSyncEvents()
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

// firstIndexedReadyManifest answers the optional readiness query without
// implementing search, so the complete set gains firstIndexedAt and
// nothing else.
type firstIndexedReadyManifest struct{ fakeManifestProvider }

func (firstIndexedReadyManifest) FirstIndexedAtReady(context.Context) bool { return true }

// firstIndexedPendingManifest implements the optional readiness query and
// answers that the backfill has not finished. Embedding the ordinary fake
// keeps every other manifest method, so a builder that advertises the key
// whenever the method exists fails this test.
type firstIndexedPendingManifest struct{ fakeManifestProvider }

func (firstIndexedPendingManifest) FirstIndexedAtReady(context.Context) bool { return false }

func TestHealthOmitsFirstIndexedAtWhileTheBackfillIsUnfinished(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{
		LibraryRoots:  []string{dir},
		ListenAddress: ":7788",
		LibraryName:   "T",
	}
	authStore, err := auth.OpenStore(filepath.Join(dir, "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv := New(cfg, authStore, &firstIndexedPendingManifest{}, "fp")
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)

	resp := authGet(t, hs, "/v1/health", "")
	body := readAllOrFail(t, resp)
	resp.Body.Close()
	var got HealthResponse
	if err := jsonUnmarshalForTest(body, &got); err != nil {
		t.Fatal(err)
	}
	for _, feature := range got.Features {
		if feature == "firstIndexedAt" {
			t.Fatalf("advertised firstIndexedAt while the backfill is unfinished: %v", got.Features)
		}
	}
}

// TestHealthFeatureCapacityCoversEveryKey reads the builder, not the
// complete-set fixture. That fixture omits search: its manifest does not
// implement SearchAvailable. The capacity has to cover every key the
// builder can append, search included.
func TestHealthFeatureCapacityCoversEveryKey(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "api.go", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	var found int
	var capacity, keys int
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		capN, keyN, ok := healthFeatureList(fn.Body)
		if !ok {
			continue
		}
		found++
		capacity, keys = capN, keyN
	}
	if found != 1 {
		t.Fatalf("feature lists in api.go: %d, want 1", found)
	}
	if capacity < keys {
		t.Fatalf("feature-list capacity %d is below the %d keys the builder appends", capacity, keys)
	}
}

func healthFeatureList(body *ast.BlockStmt) (capacity, keys int, ok bool) {
	ast.Inspect(body, func(n ast.Node) bool {
		as, isAs := n.(*ast.AssignStmt)
		if !isAs || as.Tok != token.DEFINE || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
			return true
		}
		id, isID := as.Lhs[0].(*ast.Ident)
		if !isID || id.Name != "feats" {
			return true
		}
		call, isCall := as.Rhs[0].(*ast.CallExpr)
		if !isCall || len(call.Args) != 3 {
			return true
		}
		fn, isFn := call.Fun.(*ast.Ident)
		if !isFn || fn.Name != "make" {
			return true
		}
		lit, isLit := call.Args[2].(*ast.BasicLit)
		if !isLit || lit.Kind != token.INT {
			return true
		}
		parsed, err := strconv.Atoi(lit.Value)
		if err != nil {
			return true
		}
		capacity = parsed
		ok = true
		return true
	})
	if !ok {
		return 0, 0, false
	}
	var bad bool
	ast.Inspect(body, func(n ast.Node) bool {
		call, isCall := n.(*ast.CallExpr)
		if !isCall || len(call.Args) < 2 {
			return true
		}
		fn, isFn := call.Fun.(*ast.Ident)
		if !isFn || fn.Name != "append" {
			return true
		}
		id, isID := call.Args[0].(*ast.Ident)
		if !isID || id.Name != "feats" {
			return true
		}
		for _, arg := range call.Args[1:] {
			lit, isLit := arg.(*ast.BasicLit)
			if !isLit || lit.Kind != token.STRING {
				bad = true
				return false
			}
			keys++
		}
		return true
	})
	if bad {
		return 0, 0, false
	}
	return capacity, keys, true
}
