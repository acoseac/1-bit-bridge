package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// healthFeaturesOf reads /v1/health through the real handler stack and
// returns the advertised feature set as a lookup.
func healthFeaturesOf(t *testing.T, hs *httptest.Server, token string) map[string]bool {
	t.Helper()
	req, _ := http.NewRequest("GET", hs.URL+"/v1/health", nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /v1/health: %v", err)
	}
	defer resp.Body.Close()
	var body struct {
		Features []string `json:"features"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode health: %v", err)
	}
	out := map[string]bool{}
	for _, f := range body.Features {
		out[f] = true
	}
	return out
}
