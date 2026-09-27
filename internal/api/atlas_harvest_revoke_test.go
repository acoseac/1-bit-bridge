package api

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/atlasharvest"
)

// TestAtlasHarvestCredentialDeleteForgetsIt pins the revoke half of the
// harvest credential (the 2026-09-23 audit's H3): DELETE drops the held
// credential at once, answers 204, and answers 204 again when there is
// nothing to drop, so the app can call it whenever its switch goes off.
func TestAtlasHarvestCredentialDeleteForgetsIt(t *testing.T) {
	sink := &fakeHarvestCred{}
	token, srv := newHarvestCredTestServer(t, sink)

	post := doReq(t, srv, http.MethodPost, "/v1/atlas-harvest/credential", token, "",
		`{"token":"bh-token","atlasBaseUrl":"https://atlas.example","expiresInSeconds":3600}`)
	post.Body.Close()
	if post.StatusCode != http.StatusOK {
		t.Fatalf("POST status = %d, want 200", post.StatusCode)
	}

	for i, want := range []int{1, 2} {
		resp := doReq(t, srv, http.MethodDelete, "/v1/atlas-harvest/credential", token, "", "")
		resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("DELETE #%d status = %d, want 204", i+1, resp.StatusCode)
		}
		if sink.cleared != want || sink.token != "" {
			t.Fatalf("after DELETE #%d: cleared %d times, token %q; want %d and empty", i+1, sink.cleared, sink.token, want)
		}
	}
}

// TestAtlasHarvestCredentialDeleteRefusals pins the three refusals: a bridge
// with harvest off has no credential route (404, the POST's shape), a demo
// bridge refuses (403 demo_read_only: its credential is shared by every
// demo user, so one user switching harvest off must not stop it for all),
// and an unauthenticated caller gets 401. None of them clears anything.
func TestAtlasHarvestCredentialDeleteRefusals(t *testing.T) {
	t.Run("harvest off", func(t *testing.T) {
		token, srv := newHarvestCredTestServer(t, nil)
		resp := doReq(t, srv, http.MethodDelete, "/v1/atlas-harvest/credential", token, "", "")
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", resp.StatusCode)
		}
	})
	t.Run("demo bridge", func(t *testing.T) {
		sink := &fakeHarvestCred{token: "shared"}
		token, srv := newHarvestCredTestServerPinned(t, sink, "https://atlas.example", true)
		resp := doReq(t, srv, http.MethodDelete, "/v1/atlas-harvest/credential", token, "", "")
		body := readBody(t, resp)
		if resp.StatusCode != http.StatusForbidden || !strings.Contains(body, "demo_read_only") {
			t.Fatalf("status = %d body %q, want 403 demo_read_only", resp.StatusCode, body)
		}
		if sink.cleared != 0 || sink.token != "shared" {
			t.Fatalf("a refused DELETE cleared the credential (cleared=%d token=%q)", sink.cleared, sink.token)
		}
	})
	t.Run("no bearer", func(t *testing.T) {
		sink := &fakeHarvestCred{token: "held"}
		_, srv := newHarvestCredTestServer(t, sink)
		resp := doReq(t, srv, http.MethodDelete, "/v1/atlas-harvest/credential", "", "", "")
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", resp.StatusCode)
		}
		if sink.cleared != 0 {
			t.Fatal("an unauthenticated DELETE cleared the credential")
		}
	})
}

// TestAtlasHarvestCredentialDeleteClearsTheStoredToken drives the route
// against the real atlasharvest.StateStore that serve wires: after the
// DELETE the harvest client finds no credential to use, and the token is
// gone from the file on disk, while the sync position it kept is not.
func TestAtlasHarvestCredentialDeleteClearsTheStoredToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "atlas-harvest.json")
	store, err := atlasharvest.OpenStateStore(path)
	if err != nil {
		t.Fatal(err)
	}
	token, srv := newHarvestCredTestServer(t, store)

	post := doReq(t, srv, http.MethodPost, "/v1/atlas-harvest/credential", token, "",
		`{"token":"bh-secret-token","atlasBaseUrl":"https://atlas.example","expiresInSeconds":3600}`)
	post.Body.Close()
	if err := store.SetCursor(42); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := store.AtlasCredential(); !ok {
		t.Fatal("the POST did not provision a credential")
	}

	resp := doReq(t, srv, http.MethodDelete, "/v1/atlas-harvest/credential", token, "", "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE status = %d, want 204", resp.StatusCode)
	}
	if _, _, ok := store.AtlasCredential(); ok {
		t.Fatal("the harvest client still finds a credential after the DELETE")
	}
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(onDisk), "bh-secret-token") {
		t.Fatalf("the revoked token is still in %s:\n%s", path, onDisk)
	}
	if got := store.Snapshot().ResultCursor; got != 42 {
		t.Fatalf("the DELETE reset the sync cursor to %d; a re-provision should resume at 42", got)
	}
}

// readBody returns a response's body and closes it.
func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
