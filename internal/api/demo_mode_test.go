package api

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/auth"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
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

// Upscale MUTATIONS are 403 demo_read_only in the demo posture — for
// every bearer, wired or not (the guard runs FIRST, before the
// enablement nil-checks). Unlike the user-data 404s this is a refusal
// on a feature that may be genuinely ON: a demo bridge can serve
// pre-generated + auto-optimized variants, but its effectively-public
// token must not be able to submit server work. The non-demo control
// asserts the same routes do NOT 403 (they fall through to their normal
// feature-off shapes), so the guard can't leak outside demo mode.
func TestDemoModeUpscaleMutationsForbidden(t *testing.T) {
	demoSrv, rawStatic, rawMinted := newDemoModeServer(t)
	cases := []struct {
		method, path string
	}{
		{http.MethodPost, "/v1/upscale"},
		{http.MethodPost, "/v1/upscale/batch"},
		{http.MethodDelete, "/v1/upscale/batches/5d9a2f4c-8e21-4c3a-9b77-0f1e2d3c4b5a"},
		{http.MethodDelete, "/v1/upscale/variants"},
	}
	for _, token := range []string{rawStatic, rawMinted} {
		for _, c := range cases {
			resp := doReq(t, demoSrv, c.method, c.path, token, "", `{}`)
			if resp.StatusCode != http.StatusForbidden {
				t.Errorf("demo %s %s: want 403 demo_read_only, got %d", c.method, c.path, resp.StatusCode)
			}
			resp.Body.Close()
		}
	}

	// Control: a NON-demo server never returns the demo 403 on these.
	dir := t.TempDir()
	cfg := &config.Config{LibraryRoots: []string{t.TempDir()}, ListenAddress: ":7788", LibraryName: "T"}
	authStore, err := auth.OpenStore(filepath.Join(dir, "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	raw, _, err := authStore.Mint("test")
	if err != nil {
		t.Fatal(err)
	}
	ctrl := New(cfg, authStore, nil, "fp")
	for _, c := range cases {
		resp := doReq(t, ctrl, c.method, c.path, raw, "", `{}`)
		if resp.StatusCode == http.StatusForbidden {
			t.Errorf("non-demo %s %s: must not 403, got %d", c.method, c.path, resp.StatusCode)
		}
		resp.Body.Close()
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

// Demo + Atlas: the content-push ingest is refused (403 demo_read_only)
// for BOTH credentials — a demo bearer is effectively public, and an open
// ingest would poison the bios served to every demo user — while the
// read-only meta GETs and the harvest-credential endpoint are NOT
// demo-refused (the credential carries a capability token, not content;
// asserting != 403 pins that contract without caring whether harvest is
// wired in this harness). A non-demo server with the same Atlas wiring is
// the control: its ingest reaches validation instead of the guard.
func TestDemoModeAtlasIngestForbidden(t *testing.T) {
	srv, rawStatic, rawMinted := newDemoModeServer(t)
	mstore, err := manifest.OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mstore.Close() })
	srv.WithAtlasMeta(true, 30*24*time.Hour, mstore)

	ingestBody := `{"release":{"mbid":"` + atlasTestRelMBID + `","found":true,"description":"D"}}`
	for name, tok := range map[string]string{"static": rawStatic, "minted": rawMinted} {
		resp := doReq(t, srv, http.MethodPost, "/v1/atlas-ingest", tok, "", ingestBody)
		body := readAllOrFail(t, resp)
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s token: ingest want 403, got %d (%s)", name, resp.StatusCode, body)
		}
		if !strings.Contains(string(body), "demo_read_only") {
			t.Fatalf("%s token: want demo_read_only, got %s", name, body)
		}
	}

	// Read path + capability handoff stay reachable (never the demo 403).
	for _, probe := range []struct{ method, path, body string }{
		{http.MethodGet, "/v1/atlas-meta/release/" + atlasTestRelMBID, ""},
		{http.MethodPost, "/v1/atlas-harvest/credential", `{"token":"x","atlasBaseURL":"https://a.example","expiresInSeconds":60}`},
	} {
		resp := doReq(t, srv, probe.method, probe.path, rawStatic, "", probe.body)
		body := readAllOrFail(t, resp)
		resp.Body.Close()
		if resp.StatusCode == http.StatusForbidden && strings.Contains(string(body), "demo_read_only") {
			t.Fatalf("%s %s: must not be demo-refused, got 403 %s", probe.method, probe.path, body)
		}
	}

	// Non-demo control: same wiring, ingest reaches validation (200 here —
	// the body is valid), never the demo guard.
	ctrlToken, ctrl := newAtlasMetaTestServer(t, true)
	resp := doReq(t, ctrl, http.MethodPost, "/v1/atlas-ingest", ctrlToken, "", ingestBody)
	body := readAllOrFail(t, resp)
	resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden {
		t.Fatalf("non-demo control: ingest must not 403, got %d (%s)", resp.StatusCode, body)
	}
}
