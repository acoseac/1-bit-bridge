package api

import (
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/auth"
	"github.com/acoseac/1-bit-bridge/internal/config"
)

type fakeHarvestCred struct {
	token, baseURL string
	expiresAt      time.Time
	called         int
}

func (f *fakeHarvestCred) SetCredential(token, baseURL string, expiresAt time.Time) error {
	f.called++
	f.token, f.baseURL, f.expiresAt = token, baseURL, expiresAt
	return nil
}

func newHarvestCredTestServer(t *testing.T, sink AtlasHarvestCredentialSink) (string, *Server) {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{LibraryRoots: []string{t.TempDir()}, ListenAddress: ":7788", LibraryName: "T"}
	authStore, err := auth.OpenStore(filepath.Join(dir, "tokens.json"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	raw, _, err := authStore.Mint("test")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	srv := New(cfg, authStore, nil, "fp")
	if sink != nil {
		srv.WithAtlasHarvest(sink)
	}
	return raw, srv
}

func TestAtlasHarvestCredentialStoresAndValidates(t *testing.T) {
	sink := &fakeHarvestCred{}
	token, srv := newHarvestCredTestServer(t, sink)

	resp := doReq(t, srv, http.MethodPost, "/v1/atlas-harvest/credential", token, "",
		`{"token":"bh-token","atlasBaseUrl":"https://atlas.example","expiresInSeconds":3600}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("valid credential status = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()
	if sink.called != 1 || sink.token != "bh-token" || sink.baseURL != "https://atlas.example" {
		t.Fatalf("sink = %+v", sink)
	}
	if sink.expiresAt.IsZero() {
		t.Error("expiresAt should be set when expiresInSeconds > 0")
	}

	// Missing token → 400.
	r2 := doReq(t, srv, http.MethodPost, "/v1/atlas-harvest/credential", token, "",
		`{"atlasBaseUrl":"https://atlas.example"}`)
	if r2.StatusCode != http.StatusBadRequest {
		t.Errorf("missing token status = %d, want 400", r2.StatusCode)
	}
	r2.Body.Close()

	// Non-https base URL → 400 (don't ship the token in cleartext).
	r3 := doReq(t, srv, http.MethodPost, "/v1/atlas-harvest/credential", token, "",
		`{"token":"t","atlasBaseUrl":"http://insecure.example"}`)
	if r3.StatusCode != http.StatusBadRequest {
		t.Errorf("http url status = %d, want 400", r3.StatusCode)
	}
	r3.Body.Close()

	// A base URL with a path is rejected (the client appends /v1/... paths).
	r4 := doReq(t, srv, http.MethodPost, "/v1/atlas-harvest/credential", token, "",
		`{"token":"t","atlasBaseUrl":"https://atlas.example/some/path"}`)
	if r4.StatusCode != http.StatusBadRequest {
		t.Errorf("path-bearing url status = %d, want 400", r4.StatusCode)
	}
	r4.Body.Close()

	// None of the rejected payloads should have reached the sink.
	if sink.called != 1 {
		t.Errorf("sink called %d times, want 1 (rejected payloads must not persist)", sink.called)
	}
}

// With the feature unwired the route still exists but the handler 404s, matching
// the atlas-meta feature-off shape.
func TestAtlasHarvestCredentialOffReturns404(t *testing.T) {
	token, srv := newHarvestCredTestServer(t, nil)
	resp := doReq(t, srv, http.MethodPost, "/v1/atlas-harvest/credential", token, "",
		`{"token":"t","atlasBaseUrl":"https://atlas.example"}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("feature-off status = %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()
}
