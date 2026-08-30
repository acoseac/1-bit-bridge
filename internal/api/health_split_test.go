package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/auth"
	"github.com/acoseac/1-bit-bridge/internal/config"
)

func healthSplitServer(t *testing.T) (*httptest.Server, *auth.Store) {
	t.Helper()
	dir := t.TempDir()
	lib := filepath.Join(dir, "Music")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{LibraryRoots: []string{lib}, ListenAddress: ":7788", LibraryName: "Ars Music"}
	store, err := auth.OpenStore(filepath.Join(dir, "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv := New(cfg, store, &fakeManifestProvider{tracksIndexed: 19930}, "fp").
		WithUpdater(fakeUpdater{info: UpdateInfo{
			LatestVersion:    "0.1.9",
			UpdateAvailable:  true,
			ReleaseNotesURL:  "https://example.test/r",
			MinClientVersion: "1.2.0",
		}})
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	return hs, store
}

func healthKeys(t *testing.T, hs *httptest.Server, token string) map[string]json.RawMessage {
	t.Helper()
	req, _ := http.NewRequest("GET", hs.URL+"/v1/health", nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
	var out map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

// TestHealthUnauthenticatedOmitsInventoryFields is the split.
//
// /v1/health answers without a bearer token by design — iOS reads it
// before a pairing exists. What it answered WITH was an inventory: the
// library's size and, sharpest of all, whether this host is behind on
// patches. An unauthenticated internet-wide scan could enumerate every
// bridge and sort them by how far behind they are.
//
// Key ABSENCE is asserted by decoding into map[string]json.RawMessage
// and checking the key, never by a substring probe on the raw body — a
// substring match cannot distinguish `"scanState":{}` from absent.
func TestHealthUnauthenticatedOmitsInventoryFields(t *testing.T) {
	hs, _ := healthSplitServer(t)
	keys := healthKeys(t, hs, "")

	for _, k := range []string{"scanState", "latestServerVersion", "updateAvailable", "updateReleaseNotesURL"} {
		if _, present := keys[k]; present {
			t.Errorf("unauthenticated /v1/health still carries %q — this is the field "+
				"set that turns a scan into a patch-level targeting index", k)
		}
	}
}

// TestHealthUnauthenticatedKeepsTheHandshake is the other half, and the
// one that matters for not breaking anything.
//
// These fields ARE the pre-pairing handshake, or are non-optional in
// the shipped iOS HealthResponse — removing either kind fails Codable
// decoding outright on every app in the field rather than degrading.
// Checked against BridgeSourceClient.swift: libraryName, libraryRoots,
// certFingerprint, protocolVersion, serverVersion and startedAt are all
// declared non-optional.
func TestHealthUnauthenticatedKeepsTheHandshake(t *testing.T) {
	hs, _ := healthSplitServer(t)
	keys := healthKeys(t, hs, "")

	for _, k := range []string{
		"protocolVersion", "serverVersion", "libraryName", "libraryRoots",
		"certFingerprint", "startedAt", "endpoints",
		// The client-compat floor. It shares a source struct with the
		// three withheld update fields and is NOT disclosure: iOS reads
		// it pre-pairing to decide whether to nudge an App Store update,
		// and it says nothing about this host's patch level. It was
		// dropped by accident in the first cut of this change.
		"minClientVersion",
	} {
		if _, present := keys[k]; !present {
			t.Errorf("unauthenticated /v1/health is missing %q — iOS declares the "+
				"non-optional ones as non-optional, so absence is a decode failure "+
				"on every shipped app, not a degraded screen", k)
		}
	}
}

// TestHealthAuthenticatedIsComplete: the fields moved behind the token
// are still reachable WITH one. This is a split, not a removal — an
// operator with a token, and a future client, still get the full answer.
func TestHealthAuthenticatedIsComplete(t *testing.T) {
	hs, store := healthSplitServer(t)
	raw, _, err := store.Mint("probe")
	if err != nil {
		t.Fatal(err)
	}
	keys := healthKeys(t, hs, raw)

	for _, k := range []string{"scanState", "latestServerVersion", "updateAvailable", "minClientVersion"} {
		if _, present := keys[k]; !present {
			t.Errorf("authenticated /v1/health is missing %q — the fields were meant "+
				"to move behind the token, not disappear", k)
		}
	}
	var ss ScanState
	if err := json.Unmarshal(keys["scanState"], &ss); err != nil {
		t.Fatalf("scanState: %v", err)
	}
	if ss.TracksIndexed != 19930 {
		t.Errorf("tracksIndexed = %d, want 19930", ss.TracksIndexed)
	}
}

// TestHealthWithABadTokenIsUnauthenticatedNot401: an invalid or stale
// token must degrade to the unauthenticated payload, never to an error.
// Answering 401 here would break every pre-pairing probe — and a client
// holding a revoked token would lose the endpoint list it needs to find
// the bridge again.
func TestHealthWithABadTokenIsUnauthenticatedNot401(t *testing.T) {
	hs, _ := healthSplitServer(t)
	keys := healthKeys(t, hs, "not-a-real-token")
	if _, present := keys["scanState"]; present {
		t.Error("a bogus token was treated as authenticated")
	}
	if _, present := keys["certFingerprint"]; !present {
		t.Error("a bogus token cost the caller the handshake fields")
	}
}

// TestUnauthenticatedHealthDoesNotQueryTheManifest.
//
// /v1/health is the one endpoint reachable without a credential, so it
// is the one that can be flooded. Computing scanState for a caller who
// will never see it is work done on the cheapest-to-abuse path — and
// the counts, TTL-cached or not, mean touching the store.
//
// Counting provider calls rather than timing anything: a timing
// assertion here would be a flake generator, and the question is
// whether the code path runs at all.
func TestUnauthenticatedHealthDoesNotQueryTheManifest(t *testing.T) {
	mp := &countingManifestProvider{}
	dir := t.TempDir()
	lib := filepath.Join(dir, "Music")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{LibraryRoots: []string{lib}, ListenAddress: ":7788", LibraryName: "T"}
	store, err := auth.OpenStore(filepath.Join(dir, "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(New(cfg, store, mp, "fp").Handler())
	t.Cleanup(hs.Close)

	res, err := http.Get(hs.URL + "/v1/health")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if n := mp.scanCalls(); n != 0 {
		t.Errorf("an unauthenticated /v1/health made %d manifest call(s) for a field "+
			"it then drops", n)
	}

	raw, _, err := store.Mint("probe")
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("GET", hs.URL+"/v1/health", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	res2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res2.Body.Close()
	if n := mp.scanCalls(); n == 0 {
		t.Error("an AUTHENTICATED /v1/health made no manifest call — the field is " +
			"supposed to move behind the token, not disappear")
	}
}

// countingManifestProvider records whether the scan-state path ran.
type countingManifestProvider struct {
	fakeManifestProvider
	calls atomic.Int64
}

func (c *countingManifestProvider) IsScanning() bool {
	c.calls.Add(1)
	return c.fakeManifestProvider.IsScanning()
}

func (c *countingManifestProvider) scanCalls() int64 { return c.calls.Load() }
