package api

import (
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/auth"
	"github.com/acoseac/1-bit-bridge/internal/config"
)

// fetchHealthFeaturesWithDLNA builds a minimal /v1/health-only server
// with the requested dlna wiring and returns the decoded Features
// slice. Parallel to fetchHealthFeatures (carplay) — kept separate so
// the carplay test fixture isn't churned by the additional dlna arg.
func fetchHealthFeaturesWithDLNA(t *testing.T, dlnaOn bool) []string {
	t.Helper()
	tmp := t.TempDir()
	cfg := &config.Config{
		LibraryRoots:  []string{tmp},
		ListenAddress: ":7788",
		LibraryName:   "Test",
	}
	store, _ := auth.OpenStore(filepath.Join(tmp, "tokens.json"))
	srv := New(cfg, store, nil, "fp").WithDLNA(dlnaOn)
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)

	resp := authGet(t, hs, "/v1/health", "")
	body := readAllOrFail(t, resp)
	resp.Body.Close()

	var got HealthResponse
	if err := jsonUnmarshalForTest(body, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return got.Features
}

// TestHealthAdvertisesDLNAServerWhenEnabled — iOS gates discovery-
// surface UI on this flag. Bridges that wire `WithDLNA(true)`
// advertise; iOS then offers the bridge as a DLNA-capable source in
// the OutputPickerSheet. Locked alongside the alpha-sort invariant —
// clients compare /v1/health fingerprints byte-for-byte for content-
// equality short-circuit caches.
func TestHealthAdvertisesDLNAServerWhenEnabled(t *testing.T) {
	features := fetchHealthFeaturesWithDLNA(t, true)
	found := false
	for _, f := range features {
		if f == "dlnaServer" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("Features did not contain \"dlnaServer\"; got %v", features)
	}
	// Alpha-sort invariant — same check shape as the other
	// health-features tests. dlnaServer (d-l) lands lexically
	// between diagnosticsSummary (d-i) and operatorDrivenUpscale.
	for i := 1; i < len(features); i++ {
		if features[i-1] > features[i] {
			t.Errorf("Features not alpha-sorted at index %d: %q > %q (got %v)",
				i, features[i-1], features[i], features)
		}
	}
}

// TestHealthOmitsDLNAServerWhenDisabled — default (no WithDLNA call,
// or WithDLNA(false)) must NOT advertise the capability. Operators
// who didn't opt into DLNA shouldn't have iOS clients trying to
// drive a non-running listener.
func TestHealthOmitsDLNAServerWhenDisabled(t *testing.T) {
	features := fetchHealthFeaturesWithDLNA(t, false)
	for _, f := range features {
		if f == "dlnaServer" {
			t.Errorf("dlna disabled but Features advertised dlnaServer; got %v", features)
		}
	}
}

// TestHealthOmitsDLNAServerByDefault — a bridge constructed without
// any WithDLNA call must NOT advertise the capability. Defends
// against an accidental flip of the zero-value's meaning.
func TestHealthOmitsDLNAServerByDefault(t *testing.T) {
	tmp := t.TempDir()
	cfg := &config.Config{
		LibraryRoots:  []string{tmp},
		ListenAddress: ":7788",
		LibraryName:   "Test",
	}
	store, _ := auth.OpenStore(filepath.Join(tmp, "tokens.json"))
	srv := New(cfg, store, nil, "fp") // NO WithDLNA call
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)

	resp := authGet(t, hs, "/v1/health", "")
	body := readAllOrFail(t, resp)
	resp.Body.Close()

	var got HealthResponse
	if err := jsonUnmarshalForTest(body, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, f := range got.Features {
		if f == "dlnaServer" {
			t.Errorf("default-constructed Server advertised dlnaServer; got %v", got.Features)
		}
	}
}
