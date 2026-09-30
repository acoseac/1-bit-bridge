package atlasharvest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// authRecorder is an httptest server that records the path and the
// Authorization header of every request it serves, and answers answer.
type authRecorder struct {
	*httptest.Server
	mu   sync.Mutex
	seen []string
}

func newAuthRecorder(t *testing.T, tls bool, answer http.HandlerFunc) *authRecorder {
	t.Helper()
	r := &authRecorder{}
	h := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.seen = append(r.seen, req.URL.Path+" "+req.Header.Get("Authorization"))
		r.mu.Unlock()
		answer(w, req)
	})
	if tls {
		r.Server = httptest.NewTLSServer(h)
	} else {
		r.Server = httptest.NewServer(h)
	}
	t.Cleanup(r.Close)
	return r
}

// requests returns "path authorization" for every request served so far.
func (r *authRecorder) requests() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.seen...)
}

// TestNoHarvestRequestCarriesItsTokenOntoACleartextHop pins backlog B133:
// every request of the harvest client (the submit and booklet-check POSTs,
// the results and lyrics GETs, the booklet PDF) sets its bearer token as an
// explicit Authorization header, and net/http copies that header onto a
// redirect to the same host whatever the scheme. So an https Atlas answering
// with a redirect to http on its own host had the token sent in the clear on
// the second hop. The client is now built over authredirect.Guard, the
// enrich clients' guard since #1091: the plain hop is followed without the
// header, while the https hop still carries it.
func TestNoHarvestRequestCarriesItsTokenOntoACleartextHop(t *testing.T) {
	const token = "bh-secret-token"
	legs := []struct {
		name string
		run  func(c *Client, st State) error
	}{
		{"the submit POST", func(c *Client, st State) error {
			var out submitResponse
			return c.postJSON(context.Background(), st, "/v1/atlas/harvest/submit", submitRequest{MBIDs: []string{"a1"}}, &out)
		}},
		{"the results GET", func(c *Client, st State) error {
			var out resultsResponse
			return c.getJSONCapped(context.Background(), st, "/v1/atlas/harvest/results?since=0&limit=1", &out, resultsDecodeMaxBytes, c.bulkTimeout())
		}},
		{"the booklet PDF", func(c *Client, st State) error {
			return c.fetchBookletPDF(context.Background(), st, "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
		}},
	}
	for _, leg := range legs {
		t.Run(leg.name, func(t *testing.T) {
			plain := newAuthRecorder(t, false, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNotFound)
			})
			atlas := newAuthRecorder(t, true, func(w http.ResponseWriter, r *http.Request) {
				// 307 keeps the method and the body, so a POST is followed as a POST.
				http.Redirect(w, r, plain.URL+"/moved", http.StatusTemporaryRedirect)
			})
			c := &Client{HTTP: atlas.Client(), BookletFiles: newFakeBookletFiles(),
				RequestTimeout: 5 * time.Second, BulkTimeout: 5 * time.Second}
			st := State{Token: token, AtlasBaseURL: atlas.URL, ExpiresAt: time.Now().Add(time.Hour)}

			_ = leg.run(c, st) // the plain hop answers 404; what matters is what it was sent

			first := atlas.requests()
			if len(first) != 1 || !strings.HasSuffix(first[0], "Bearer "+token) {
				t.Fatalf("the https Atlas saw %q, want one request carrying the token", first)
			}
			hops := plain.requests()
			if len(hops) != 1 {
				t.Fatalf("the plain hop saw %q, want the redirect followed once", hops)
			}
			if strings.Contains(hops[0], token) {
				t.Errorf("the token went to the plain http hop in the clear: %q", hops[0])
			}
		})
	}

	// The https hop keeps it: the guard strips only where the scheme leaves
	// https, so an Atlas that moves a path on its own origin still
	// authenticates the request it redirected.
	t.Run("a redirect that stays on https keeps the token", func(t *testing.T) {
		var atlas *authRecorder
		atlas = newAuthRecorder(t, true, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/moved" {
				http.Redirect(w, r, atlas.URL+"/moved", http.StatusTemporaryRedirect)
				return
			}
			w.WriteHeader(http.StatusNotFound)
		})
		c := &Client{HTTP: atlas.Client(), RequestTimeout: 5 * time.Second, BulkTimeout: 5 * time.Second}
		st := State{Token: token, AtlasBaseURL: atlas.URL, ExpiresAt: time.Now().Add(time.Hour)}
		var out resultsResponse
		_ = c.getJSONCapped(context.Background(), st, "/v1/atlas/harvest/results", &out, resultsDecodeMaxBytes, c.bulkTimeout())
		got := atlas.requests()
		if len(got) != 2 || got[1] != "/moved Bearer "+token {
			t.Errorf("the https hops saw %q, want the second to carry the token", got)
		}
	})
}

// TestTheDefaultHarvestClientDropsTheTokenLeavingHTTPS pins the production
// client, which the test above cannot reach (it would not trust the test
// server's certificate): its redirect policy strips the header from a hop
// that leaves https and keeps it on one that does not.
func TestTheDefaultHarvestClientDropsTheTokenLeavingHTTPS(t *testing.T) {
	check := defaultHarvestHTTPClient.CheckRedirect
	if check == nil {
		t.Fatal("the default harvest client has no redirect policy: net/http copies the token onto a cleartext hop")
	}
	if defaultHarvestHTTPClient.Timeout != 0 {
		t.Fatalf("the guard gave the default client a Timeout (%v); it must stay 0", defaultHarvestHTTPClient.Timeout)
	}
	first := httptest.NewRequest(http.MethodGet, "https://atlas.example/v1/atlas/harvest/results", nil)
	first.Header.Set("Authorization", "Bearer bh-secret-token")
	for _, tc := range []struct {
		hop  string
		keep bool
	}{
		{"http://atlas.example/moved", false},
		{"https://atlas.example/moved", true},
	} {
		hop := httptest.NewRequest(http.MethodGet, tc.hop, nil)
		hop.Header.Set("Authorization", "Bearer bh-secret-token")
		if err := check(hop, []*http.Request{first}); err != nil {
			t.Fatalf("%s: the policy refused the hop: %v", tc.hop, err)
		}
		if got := hop.Header.Get("Authorization") != ""; got != tc.keep {
			t.Errorf("%s: Authorization kept = %v, want %v", tc.hop, got, tc.keep)
		}
	}
}
