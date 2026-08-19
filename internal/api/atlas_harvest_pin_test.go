package api

import (
	"encoding/json"
	"net/http"
	"testing"
)

// POST /v1/atlas-harvest/credential lets its caller choose `atlasBaseUrl`, and
// the harvest client then dials that host for both submit and fetch. Whatever
// it returns lands in `artist_atlas` and is served to every client by
// GET /v1/atlas-meta — including a `SourceURL` the app renders as a
// "Read more on …" link. So the endpoint decides where the bridge's editorial
// metadata comes from.
//
// On a demo bridge the bearer is public by construction: the static
// `demo.tokenSHA256` ships inside every installed copy of the app. Unpinned,
// that is the same content injection refuseAtlasIngestInDemoMode blocks on
// /v1/atlas-ingest — whose comment reasoned about the TOKEN ("a bogus one just
// fails the harvest") and not about the base URL in the same request.
//
// Found live on bridge.1-bit.app in the 2026-08-19 bug review: it advertises
// `booklets`, which is wired in the same `harvestState != nil` block as
// WithAtlasHarvest, so the endpoint was reachable there.

func postCredential(t *testing.T, srv *Server, token, baseURL string) *http.Response {
	t.Helper()
	return doReq(t, srv, http.MethodPost, "/v1/atlas-harvest/credential", token, "",
		`{"token":"bh-token","atlasBaseUrl":"`+baseURL+`","expiresInSeconds":3600}`)
}

func TestAtlasHarvestCredentialRefusesUnpinnedBaseURLInDemoMode(t *testing.T) {
	sink := &fakeHarvestCred{}
	token, srv := newHarvestCredTestServerPinned(t, sink, "", true /* demo */)

	resp := postCredential(t, srv, token, "https://attacker.example")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 — a public demo bearer must not be able to "+
			"repoint the harvest pull at a host of its choosing", resp.StatusCode)
	}
	if sink.called != 0 {
		t.Errorf("sink.called = %d, want 0 — the refusal must happen BEFORE persistence; "+
			"SetCredential resets the sync cursor and clobbers the operator's token when the base URL differs",
			sink.called)
	}
	var body ErrorResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode error envelope: %v", err)
	}
	if body.Error != "demo_read_only" {
		t.Errorf("error code = %q, want demo_read_only", body.Error)
	}
}

func TestAtlasHarvestCredentialRefusesNonPinnedHost(t *testing.T) {
	sink := &fakeHarvestCred{}
	// Pinned, and NOT demo — the pin binds in every mode, because an operator
	// who configured a host meant it.
	token, srv := newHarvestCredTestServerPinned(t, sink, "https://atlas.example", false)

	resp := postCredential(t, srv, token, "https://attacker.example")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for a host other than the pin", resp.StatusCode)
	}
	if sink.called != 0 {
		t.Errorf("sink.called = %d, want 0 — refused before persistence", sink.called)
	}
	var body ErrorResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode error envelope: %v", err)
	}
	if body.Error != "harvest_base_url_not_allowed" {
		t.Errorf("error code = %q, want harvest_base_url_not_allowed", body.Error)
	}
}

// The operator's own bootstrap must still work — a demo bridge is DESIGNED to
// be credentialed through this endpoint (that is why refuseAtlasIngestInDemoMode
// deliberately left it open). The pin is what separates that from the attack,
// so a pinned host has to be accepted, demo or not.
func TestAtlasHarvestCredentialAcceptsThePinnedHost(t *testing.T) {
	for _, demo := range []bool{false, true} {
		sink := &fakeHarvestCred{}
		token, srv := newHarvestCredTestServerPinned(t, sink, "https://atlas.example", demo)

		resp := postCredential(t, srv, token, "https://atlas.example")
		if resp.StatusCode != http.StatusOK {
			t.Errorf("demo=%v: status = %d, want 200 — the operator bootstrap must survive the pin",
				demo, resp.StatusCode)
		}
		resp.Body.Close()
		if sink.called != 1 || sink.baseURL != "https://atlas.example" {
			t.Errorf("demo=%v: sink = %+v, want one call for the pinned host", demo, sink)
		}
	}
}

// A trailing slash is the same host. The handler canonicalises to
// scheme://host before comparing, so `https://atlas.example/` must match a pin
// written without the slash — otherwise the pin rejects the operator's own
// perfectly valid spelling.
func TestAtlasHarvestCredentialPinIgnoresTrailingSlash(t *testing.T) {
	sink := &fakeHarvestCred{}
	token, srv := newHarvestCredTestServerPinned(t, sink, "https://atlas.example", true)

	resp := postCredential(t, srv, token, "https://atlas.example/")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 — `https://host/` and `https://host` are the same pin", resp.StatusCode)
	}
	if sink.called != 1 {
		t.Errorf("sink.called = %d, want 1", sink.called)
	}
}

// Unpinned on a NON-demo bridge stays allowed: that is the back-compatible
// case, and its bearers are the operator's own paired devices — the trust
// model the rest of the device→bridge write surface already assumes. Breaking
// it would 403 every existing harvest deployment on upgrade.
func TestAtlasHarvestCredentialUnpinnedStillWorksOffDemo(t *testing.T) {
	sink := &fakeHarvestCred{}
	token, srv := newHarvestCredTestServerPinned(t, sink, "", false)

	resp := postCredential(t, srv, token, "https://atlas.example")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 — an unpinned non-demo bridge must keep working", resp.StatusCode)
	}
	if sink.called != 1 {
		t.Errorf("sink.called = %d, want 1", sink.called)
	}
}
