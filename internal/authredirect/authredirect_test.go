package authredirect

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// hop builds a request to target carrying the Authorization header net/http
// copies from the first request of a redirect chain.
func hop(target string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, target, nil)
	r.Header.Set("Authorization", "Bearer tok")
	return r
}

// TestGuardDropsTheHeaderOnlyWhereTheChainLeavesHTTPS pins the policy Guard
// installs, judged per hop against the request the chain began with: a hop
// that leaves https loses the header, one that stays keeps it, a chain that
// began on plain http is left alone, and a later cleartext hop of an https
// chain loses it too, whatever the hop before it was.
func TestGuardDropsTheHeaderOnlyWhereTheChainLeavesHTTPS(t *testing.T) {
	check := Guard(&http.Client{}).CheckRedirect
	httpsFirst := hop("https://atlas.example/v1/x")
	for _, tc := range []struct {
		name string
		via  []*http.Request
		next string
		keep bool
	}{
		{"https to http on the same host", []*http.Request{httpsFirst}, "http://atlas.example/moved", false},
		{"https to https", []*http.Request{httpsFirst}, "https://atlas.example/moved", true},
		{"a chain that began on plain http", []*http.Request{hop("http://atlas.example/v1/x")}, "http://atlas.example/moved", true},
		{"a later cleartext hop of an https chain", []*http.Request{httpsFirst, hop("http://atlas.example/a")}, "http://atlas.example/b", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			next := hop(tc.next)
			if err := check(next, tc.via); err != nil {
				t.Fatalf("the policy refused the hop: %v", err)
			}
			if got := next.Header.Get("Authorization") != ""; got != tc.keep {
				t.Errorf("Authorization kept = %v, want %v", got, tc.keep)
			}
		})
	}
}

// TestGuardKeepsTheCallersPolicyAndClient pins the rest of the contract: a
// caller's CheckRedirect is asked, with the header already dropped, and
// decides; without one, net/http's limit of ten holds; and the caller's
// client is copied, never written to.
func TestGuardKeepsTheCallersPolicyAndClient(t *testing.T) {
	refuse := errors.New("the caller's policy refused")
	var sawHeader bool
	caller := &http.Client{CheckRedirect: func(req *http.Request, _ []*http.Request) error {
		sawHeader = req.Header.Get("Authorization") != ""
		return refuse
	}}
	guarded := Guard(caller)
	if guarded == caller {
		t.Fatal("Guard returned the caller's client instead of a copy")
	}
	err := guarded.CheckRedirect(hop("http://atlas.example/moved"), []*http.Request{hop("https://atlas.example/v1/x")})
	if !errors.Is(err, refuse) {
		t.Errorf("the caller's policy did not decide: %v", err)
	}
	if sawHeader {
		t.Error("the caller's policy was asked with the header still on the cleartext hop")
	}
	if err := caller.CheckRedirect(hop("http://x/"), nil); !errors.Is(err, refuse) {
		t.Error("the caller's client was written to")
	}

	limited := Guard(&http.Client{}).CheckRedirect
	via := make([]*http.Request, redirectLimit)
	for i := range via {
		via[i] = hop("https://atlas.example/loop")
	}
	if err := limited(hop("https://atlas.example/loop"), via); err == nil {
		t.Error("a chain of ten redirects was followed an eleventh time")
	}
	if err := limited(hop("https://atlas.example/loop"), via[:redirectLimit-1]); err != nil {
		t.Errorf("the tenth request was refused: %v", err)
	}
}
