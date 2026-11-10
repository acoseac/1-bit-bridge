package api

import (
	"net/http"
	"slices"
	"strings"
	"testing"
)

// TestNoHealthFlagBeforeItsRoute (B728 B8, updated by B9; the design's
// section 7.1): no health flag names a DSD route that is not mounted or that
// cannot write, whichever gate holds and whatever the element carries:
// bridgeDSDChoice arrives with the choice's PUT (B9) and a player that writes
// it, and bridgeDSDCheck with the check's routes (B12), which are not
// registered here. A player that writes no choice advertises neither, and a
// client that asks a route anyway is answered as for one the bridge does not
// serve.
func TestNoHealthFlagBeforeItsRoute(t *testing.T) {
	flags := []string{"bridgeDSDChoice", "bridgeDSDCheck"}
	for _, player := range []bool{true, false} {
		for _, mpd := range []string{"on", "off"} {
			b := &fakeBridgeOutput{enabled: player}
			b.set(populatedDSD())
			hs, token, srv := newBridgeOutputsHTTP(t, quietOutputs("linux", mpd), b)
			feats := healthFeatures(t, hs)
			for _, flag := range flags {
				if slices.Contains(feats, flag) {
					t.Errorf("player %v, MPD %s: /v1/health advertises %s, and nothing writes it: %v", player, mpd, flag, feats)
				}
			}
			for _, rt := range srv.routeRegistry() {
				if strings.Contains(rt.pattern, "/dsd") && rt.pattern != "PUT /v1/outputs/{id}/dsd" {
					t.Errorf("a DSD route is registered before its PR: %s", rt.pattern)
				}
			}
			for _, r := range []struct{ method, path string }{
				{http.MethodPut, "/v1/outputs/bridge:" + testPlayerUUID + "/dsd"},
				{http.MethodPost, "/v1/outputs/bridge:" + testPlayerUUID + "/dsd/check"},
				{http.MethodDelete, "/v1/outputs/bridge:" + testPlayerUUID + "/dsd/check"},
				{http.MethodPost, "/v1/outputs/bridge:" + testPlayerUUID + "/dsd/check/answer"},
			} {
				req, err := http.NewRequest(r.method, hs.URL+r.path, strings.NewReader(`{}`))
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Authorization", "Bearer "+token)
				req.Header.Set("Content-Type", "application/json")
				resp, err := hs.Client().Do(req)
				if err != nil {
					t.Fatal(err)
				}
				resp.Body.Close()
				if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusMethodNotAllowed {
					t.Errorf("player %v, MPD %s: %s %s answered %d, want the bridge's answer to a route it does not serve", player, mpd, r.method, r.path, resp.StatusCode)
				}
			}
		}
	}
}
