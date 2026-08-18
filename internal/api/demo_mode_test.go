package api

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/auth"
	"github.com/acoseac/1-bit-bridge/internal/config"
)

// newDemoModeServer builds the demo posture exactly as cmd/bridge wires
// it: NO playlist / favorites / history stores, NO device registrar,
// WithDemoMode(true). The static demo token is seeded on the auth store
// the way `demo.tokenSHA256` does it at boot.
//
// Returns the server plus TWO credentials so tests can distinguish
// "auth layer" from "feature layer": rawStatic is the config-seeded
// demo token, rawMinted a normal `bridge pair`-shaped token.
func newDemoModeServer(t *testing.T) (srv *Server, rawStatic, rawMinted string) {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{LibraryRoots: []string{t.TempDir()}, ListenAddress: ":7788", LibraryName: "Demo"}
	authStore, err := auth.OpenStore(filepath.Join(dir, "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	rawStatic = "demo-raw-token-fixture"
	sum := sha256.Sum256([]byte(rawStatic))
	if err := authStore.SetStaticToken(hex.EncodeToString(sum[:]), "Demo access (config)"); err != nil {
		t.Fatal(err)
	}
	rawMinted, _, err = authStore.Mint("paired sibling")
	if err != nil {
		t.Fatal(err)
	}
	srv = New(cfg, authStore, nil, "fp").WithDemoMode(true)
	return srv, rawStatic, rawMinted
}

// Demo posture health: `demoMode` advertised, the five user-data flags
// honestly absent — the exact contract iOS keys its locked-toggle UI on.
func TestDemoModeHealthDropsUserDataFlags(t *testing.T) {
	srv, _, _ := newDemoModeServer(t)
	resp := doReq(t, srv, http.MethodGet, "/v1/health", "", "", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health: want 200, got %d", resp.StatusCode)
	}
	var got HealthResponse
	if err := jsonUnmarshalForTest(readAllOrFail(t, resp), &got); err != nil {
		t.Fatalf("decode health: %v", err)
	}
	feats := make(map[string]bool, len(got.Features))
	for _, f := range got.Features {
		feats[f] = true
	}
	if !feats["demoMode"] {
		t.Errorf("features should advertise demoMode, got %v", got.Features)
	}
	for _, absent := range []string{"playlistBackup", "playlistsCrossDevice", "favorites", "playbackHistory", "playbackHistoryRead"} {
		if feats[absent] {
			t.Errorf("features should NOT advertise %q in demo mode, got %v", absent, got.Features)
		}
	}
}

// Every user-data mutation returns its typed 404 in the demo posture —
// for the static token AND for a normally-minted one (the read-only
// property is the server's, not the credential's).
func TestDemoModeMutationsReturnNotFound(t *testing.T) {
	srv, rawStatic, rawMinted := newDemoModeServer(t)
	cases := []struct {
		method, path, body string
	}{
		{http.MethodPut, "/v1/playlists/5d9a2f4c-8e21-4c3a-9b77-0f1e2d3c4b5a", `{"name":"x"}`},
		{http.MethodDelete, "/v1/playlists/5d9a2f4c-8e21-4c3a-9b77-0f1e2d3c4b5a", ""},
		{http.MethodPut, "/v1/favorites", `{"lastModifiedAt":1,"tracks":[],"albums":[]}`},
		{http.MethodPost, "/v1/history/batch", `{"events":[]}`},
		{http.MethodGet, "/v1/history", ""},
	}
	for _, token := range []string{rawStatic, rawMinted} {
		for _, c := range cases {
			resp := doReq(t, srv, c.method, c.path, token, "deadbeef", c.body)
			if resp.StatusCode != http.StatusNotFound {
				t.Errorf("%s %s: want 404 (feature unwired), got %d", c.method, c.path, resp.StatusCode)
			}
			resp.Body.Close()
		}
	}
}

// The config-seeded static token clears the AUTH layer: with it, a
// feature-gated route reaches the feature check (404); without it, the
// request dies at 401. This is what makes a baked-in app credential
// usable at all.
func TestDemoModeStaticTokenClearsAuth(t *testing.T) {
	srv, rawStatic, _ := newDemoModeServer(t)

	resp := doReq(t, srv, http.MethodGet, "/v1/playlists", rawStatic, "", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("static token: want 404 (past auth, feature unwired), got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = doReq(t, srv, http.MethodGet, "/v1/playlists", "not-the-demo-token", "", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong token: want 401, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}
