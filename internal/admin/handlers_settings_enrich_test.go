package admin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// patchSettings PATCHes a raw JSON body to /api/settings and returns the
// response (caller closes the body). Factored so the per-test boilerplate
// isn't repeated.
func patchSettings(t *testing.T, baseURL, jsonBody string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("PATCH", baseURL+"/api/settings", bytes.NewBufferString(jsonBody))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// TestSettingsPatchEnrichBaseURLs round-trips the enrich upstream base-URL
// overrides (#406's config, surfaced in the admin console by this change)
// through PATCH + GET and pins the restart-required contract.
func TestSettingsPatchEnrichBaseURLs(t *testing.T) {
	srv, _, _ := newTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp := patchSettings(t, ts.URL, `{"enrichMusicBrainzBaseURL":"https://atlas.test/ws/2","enrichCoverArtBaseURL":"https://atlas.test"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH status %d, want 200", resp.StatusCode)
	}
	var patchResp settingsPatchResponse
	if err := json.NewDecoder(resp.Body).Decode(&patchResp); err != nil {
		t.Fatal(err)
	}
	if !patchResp.RestartRequired {
		t.Error("changing enrich base URLs must mark RestartRequired (clients wired at startup)")
	}

	gresp, err := http.Get(ts.URL + "/api/settings")
	if err != nil {
		t.Fatal(err)
	}
	defer gresp.Body.Close()
	var got map[string]any
	if err := json.NewDecoder(gresp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got["enrichMusicBrainzBaseURL"] != "https://atlas.test/ws/2" || got["enrichCoverArtBaseURL"] != "https://atlas.test" {
		t.Errorf("round-trip mismatch: mb=%v caa=%v", got["enrichMusicBrainzBaseURL"], got["enrichCoverArtBaseURL"])
	}
}

// TestSettingsPatchEnrichRejectsInvalidURL pins that Config.Validate rejects a
// non-absolute / non-http(s) enrich base URL at the PATCH boundary.
func TestSettingsPatchEnrichRejectsInvalidURL(t *testing.T) {
	srv, _, _ := newTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp := patchSettings(t, ts.URL, `{"enrichMusicBrainzBaseURL":"not-a-url"}`)
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Error("PATCH with an invalid enrich URL should be rejected, got 200")
	}
}
