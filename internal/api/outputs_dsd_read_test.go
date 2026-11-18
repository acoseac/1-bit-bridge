package api

import (
	"net/http"
	"slices"
	"strings"
	"testing"
)

// TestTheChoiceFlagArrivesWithItsRoute (B728 B9 PR 3): bridgeDSDChoice is
// advertised exactly while the player's gate holds and serve wired the
// write, whichever MPD's gate says; PUT /v1/outputs/{id}/dsd is the one DSD
// route registered, and the check's routes (B12) are not, nor is its flag, a
// client that asks one anyway being answered as for any path the bridge does
// not serve.
func TestTheChoiceFlagArrivesWithItsRoute(t *testing.T) {
	for _, player := range []bool{true, false} {
		for _, wired := range []bool{true, false} {
			for _, mpd := range []string{"on", "off"} {
				var c BridgeDSDChoice
				if wired {
					c = &fakeDSDChoice{answer: BridgeDSDChoiceResult{Outcome: DSDChoiceWritten}}
				}
				h := newChoiceHTTP(t, quietOutputs("linux", mpd), player, c, 0)
				feats := healthFeatures(t, h.hs)
				if got, want := slices.Contains(feats, "bridgeDSDChoice"), player && wired; got != want {
					t.Errorf("player %v, wired %v, MPD %s: bridgeDSDChoice advertised %v, want %v: %v", player, wired, mpd, got, want, feats)
				}
				if slices.Contains(feats, "bridgeDSDCheck") {
					t.Errorf("bridgeDSDCheck advertised before its route: %v", feats)
				}
				var dsd []string
				for _, rt := range h.srv.routeRegistry() {
					if strings.Contains(rt.pattern, "/dsd") {
						dsd = append(dsd, rt.pattern)
					}
				}
				if !slices.Equal(dsd, []string{"PUT /v1/outputs/{id}/dsd"}) {
					t.Errorf("the DSD routes registered: %q, want the choice's alone", dsd)
				}
				a := h.putOK(t, bridgeID, aChoice)
				if want := player && wired; (a.status == http.StatusOK) != want {
					t.Errorf("player %v, wired %v, MPD %s: the PUT answered %d %s", player, wired, mpd, a.status, a.body)
				}
				for _, r := range []struct{ method, path string }{
					{http.MethodPost, "/v1/outputs/" + bridgeID + "/dsd/check"},
					{http.MethodDelete, "/v1/outputs/" + bridgeID + "/dsd/check"},
					{http.MethodPost, "/v1/outputs/" + bridgeID + "/dsd/check/answer"},
				} {
					req, err := http.NewRequest(r.method, h.hs.URL+r.path, strings.NewReader(`{}`))
					if err != nil {
						t.Fatal(err)
					}
					req.Header.Set("Authorization", "Bearer "+h.token)
					resp, err := h.hs.Client().Do(req)
					if err != nil {
						t.Fatal(err)
					}
					resp.Body.Close()
					if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusMethodNotAllowed {
						t.Errorf("%s %s answered %d, want the bridge's answer to a route it does not serve", r.method, r.path, resp.StatusCode)
					}
				}
			}
		}
	}
}
