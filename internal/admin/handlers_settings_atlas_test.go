package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestSettingsPatchAtlasEnabled round-trips the rich-tier Atlas opt-in
// through PATCH + GET and pins the restart-required contract (the
// /v1/atlas-* routes + the atlasEnrichment health flag are wired at startup).
func TestSettingsPatchAtlasEnabled(t *testing.T) {
	srv, _, _ := newTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp := patchSettings(t, ts.URL, `{"atlasEnabled":true}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH status %d, want 200", resp.StatusCode)
	}
	var pr settingsPatchResponse
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		t.Fatal(err)
	}
	if !pr.RestartRequired {
		t.Error("enabling atlas must mark RestartRequired (routes wired at startup)")
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
	if got["atlasEnabled"] != true {
		t.Errorf("atlasEnabled = %v, want true", got["atlasEnabled"])
	}
}
