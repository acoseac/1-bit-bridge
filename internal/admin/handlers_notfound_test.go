package admin

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestNotFoundRendersTheShell pins the catch-all.
//
// Guessing "/roots" and "/duplicates" from the sidebar labels — the real
// paths are /library and /library/duplicates — used to land on Go's
// default `404 page not found`: unstyled black-on-white, no nav, and the
// browser Back button as the only route onward. On a hosted bridge that
// page is what a stale bookmark reaches.
func TestNotFoundRendersTheShell(t *testing.T) {
	srv, _, _ := newTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// The two paths that actually caught me, plus a plainly-absent one.
	for _, p := range []string{"/roots", "/duplicates", "/no/such/page"} {
		t.Run(p, func(t *testing.T) {
			res, err := http.Get(ts.URL + p)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			body, _ := io.ReadAll(res.Body)
			if res.StatusCode != http.StatusNotFound {
				t.Errorf("status = %d, want 404", res.StatusCode)
			}
			if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
				t.Errorf("Content-Type = %q, want text/html — the status is only half "+
					"the fix; the point is that a person gets a page", ct)
			}
			s := string(body)
			// The shell, not a bare string: the nav is what makes this a
			// way out rather than a dead end.
			if !strings.Contains(s, `id="primary-nav"`) {
				t.Error("404 body has no primary nav — it is rendering without the shell")
			}
			if !strings.Contains(s, "Page not found") {
				t.Error("404 body does not say what happened")
			}
			if strings.Contains(s, "404 page not found") {
				t.Error("still Go's default 404 body")
			}
		})
	}
}

// TestNotFoundOnAPIPathsIsJSON: an API client must never get an HTML body
// it cannot parse. This also covers the one deliberate consequence of a
// "/" catch-all — it absorbs Go's 405 for a method mismatch — by making
// sure the absorbed case is still machine-readable.
func TestNotFoundOnAPIPathsIsJSON(t *testing.T) {
	srv, _, _ := newTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	res, err := http.Get(ts.URL + "/api/no-such-endpoint")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", res.StatusCode)
	}
	var env struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(res.Body).Decode(&env); err != nil {
		t.Fatalf("body is not the JSON error envelope: %v", err)
	}
	if env.Error != "not_found" {
		t.Errorf("error = %q, want %q", env.Error, "not_found")
	}
}

// TestRegisteredRoutesStillResolve is the control for the two tests above:
// a catch-all that swallowed real routes would make them pass while
// breaking the console outright.
func TestRegisteredRoutesStillResolve(t *testing.T) {
	srv, _, _ := newTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	for _, p := range []string{"/", "/stats", "/library", "/library/duplicates", "/jobs", "/settings", "/diagnostics"} {
		res, err := http.Get(ts.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Errorf("GET %s = %d, want 200 — the catch-all is shadowing a real route", p, res.StatusCode)
		}
	}
}
