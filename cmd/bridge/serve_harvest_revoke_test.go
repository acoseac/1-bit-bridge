package main

import (
	"crypto/tls"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/atlasharvest"
	"github.com/acoseac/1-bit-bridge/internal/auth"
)

// TestServeRevokesAHarvestCredentialWithTheHarvestOff boots the real serve
// with atlas.harvestEnabled off over a state file that still holds the
// credential from when it was on. Re-enabling the harvest reads that file
// again, so the app's switch-off DELETE must clear it there: 204, the token
// gone from the file, the sync position kept. The route's own tests use a
// fixture server; only a boot shows serve wires the clearer when no store is
// open.
func TestServeRevokesAHarvestCredentialWithTheHarvestOff(t *testing.T) {
	cfgPath := writeValidConfig(t)
	dataDir := filepath.Join(filepath.Dir(cfgPath), "data")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	statePath := harvestStatePath(dataDir)
	seeded, err := atlasharvest.OpenStateStore(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := seeded.SetCredential("bh-secret-token", "https://atlas.example", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := seeded.SetCursor(42); err != nil {
		t.Fatal(err)
	}
	tokens, err := auth.OpenStore(filepath.Join(dataDir, tokensFileName))
	if err != nil {
		t.Fatal(err)
	}
	bearer, _, err := tokens.Mint("harvest-revoke-test")
	if err != nil {
		t.Fatal(err)
	}

	served := bootServe(t, "--config", cfgPath, "--addr", "127.0.0.1:0")

	req, err := http.NewRequest(http.MethodDelete, "https://"+served.addr+"/v1/atlas-harvest/credential", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	client := &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v; stderr=%s", err, served.stderr.String())
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE status = %d body %q, want 204", resp.StatusCode, body)
	}

	reopened, err := atlasharvest.OpenStateStore(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := reopened.AtlasCredential(); ok {
		t.Fatal("re-enabling the harvest would find the revoked credential")
	}
	if got := reopened.Snapshot().ResultCursor; got != 42 {
		t.Fatalf("cursor = %d, want 42", got)
	}
}
