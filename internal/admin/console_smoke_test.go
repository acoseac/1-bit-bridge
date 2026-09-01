package admin

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

// Console smoke pass.
//
// `go test` was green through five defects in the upload stack, four
// console defects in #798, and two separate incidents where a misplaced
// `boot();` put the whole player module in the temporal dead zone and the
// page rendered nothing. Every one was found by a human opening a
// browser.
//
// This file closes the cheap half of that gap: every page renders, every
// page's HTML parses, and every read-only API route answers something
// other than a 5xx. It does NOT execute JavaScript — the module-load
// layer that catches the TDZ class lives in player_module_load_test.go,
// and full runtime coverage needs a real browser, which is a deliberate
// follow-on rather than part of this.

// smokeRequest issues a loopback GET against the admin handler.
func smokeRequest(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.RemoteAddr = "127.0.0.1:54321"
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// TestEveryAdminPageRenders walks every page the server knows about —
// enumerated from `pages` and `playerRoutes` rather than a hand-written
// list, because a hand-written list is the forgot-the-list failure this
// exists to catch — and asserts each returns 200 and parses as HTML.
//
// A template that references a missing field, a nil map, or a helper that
// panics surfaces here as a 500 or a parse failure. Before this, it
// surfaced when someone opened the page.
func TestEveryAdminPageRenders(t *testing.T) {
	s, _, _ := newTestServer(t)
	h := s.Handler()

	paths := []string{"/"}
	// The `pages` map is keyed by tab name, and the URL is not derivable
	// from it (the player is "/", stats is "/stats", duplicates is
	// "/library/duplicates"), so the mapping is explicit — but the TEST
	// asserts it covers every entry, so a new page cannot be added
	// without either appearing here or failing.
	tabToPath := map[string]string{
		"player":      "/",
		"stats":       "/stats",
		"library":     "/library",
		"upload":      "/upload",
		"duplicates":  "/library/duplicates",
		"jobs":        "/jobs",
		"devices":     "/devices",
		"upnp":        "/upnp",
		"history":     "/history",
		"settings":    "/settings",
		"diagnostics": "/diagnostics",
		// Rendered by the catch-all, reached below via an unknown path.
		"notfound": "",
	}
	var missing []string
	for tab := range pages {
		p, ok := tabToPath[tab]
		if !ok {
			missing = append(missing, tab)
			continue
		}
		if p != "" && p != "/" {
			paths = append(paths, p)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Fatalf("pages %v have no path in this test's table — add them, so the smoke pass "+
			"actually covers every page rather than the ones someone remembered", missing)
	}

	// Player sub-routes are server-rendered too (the shell plus a seed),
	// so each must return a real document rather than a 404.
	for _, p := range playerRoutes {
		// Wildcard routes need a concrete id; any value renders the shell.
		paths = append(paths, strings.NewReplacer("{id}", "abc123", "{slug}", "daily-mix").Replace(p))
	}

	for _, p := range paths {
		t.Run(p, func(t *testing.T) {
			rr := smokeRequest(t, h, p)
			if rr.Code != http.StatusOK {
				t.Fatalf("GET %s: status %d, want 200\nbody: %s", p, rr.Code, truncate(rr.Body.String(), 400))
			}
			body := rr.Body.String()
			if strings.TrimSpace(body) == "" {
				t.Fatalf("GET %s: empty body", p)
			}
			if _, err := html.Parse(strings.NewReader(body)); err != nil {
				t.Fatalf("GET %s: body does not parse as HTML: %v", p, err)
			}
			// A Go template that fails mid-render writes what it produced
			// before the error, so a page can be 200 AND truncated. The
			// closing tag is the cheapest proof it got to the end.
			if !strings.Contains(strings.ToLower(body), "</html>") {
				t.Errorf("GET %s: no closing </html> — the template probably failed mid-render", p)
			}
			// html/template writes the error text into the output on
			// some failures rather than failing the write.
			for _, marker := range []string{"executing \"", "template: ", "<no value>"} {
				if strings.Contains(body, marker) {
					t.Errorf("GET %s: body carries a template error marker %q", p, marker)
				}
			}
		})
	}
}

// TestUnknownPathRendersTheNotFoundPage covers the catch-all, which is a
// real page with the real shell on purpose — Go's bare "404 page not
// found" has no nav, leaving the operator with the Back button.
func TestUnknownPathRendersTheNotFoundPage(t *testing.T) {
	s, _, _ := newTestServer(t)
	rr := smokeRequest(t, s.Handler(), "/this-page-does-not-exist")
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(strings.ToLower(body), "</html>") {
		t.Errorf("the 404 is not a rendered page:\n%s", truncate(body, 300))
	}
	if _, err := html.Parse(strings.NewReader(body)); err != nil {
		t.Errorf("the 404 body does not parse as HTML: %v", err)
	}
}

// TestEveryReadOnlyAPIRouteAnswers hits every GET /api/* route the mux
// serves and asserts it does not 5xx on a fresh, empty bridge.
//
// A 4xx is fine — several routes legitimately require a parameter — but a
// 500 on an empty install means a nil dereference or an unhandled
// empty-set case, which is exactly the shape that reaches production
// because nobody clicks that page on a fresh bridge.
func TestEveryReadOnlyAPIRouteAnswers(t *testing.T) {
	s, _, _ := newTestServer(t)
	h := s.Handler()

	// Routes needing a concrete parameter; the value need not exist.
	substitute := strings.NewReplacer(
		"{sid}", "nope",
		"{id}", "nope",
		"{slug}", "nope",
		"{mbid}", "00000000-0000-4000-8000-000000000000",
	)
	// Streaming / long-lived routes are excluded: SSE never returns, and
	// a smoke test that hangs is worse than no smoke test.
	skip := map[string]string{
		"/api/events": "SSE — a long-lived stream that never returns",
	}

	for _, route := range readOnlyAPIRoutes(t) {
		if why, ok := skip[route]; ok {
			t.Logf("skipping %s: %s", route, why)
			continue
		}
		path := substitute.Replace(route)
		t.Run(path, func(t *testing.T) {
			rr := smokeRequest(t, h, path)
			body := rr.Body.String()

			// 503 is a legitimate answer: several routes report "this
			// subsystem is not wired on this bridge", and the bare test
			// harness wires almost nothing. But a 503 must be a DELIBERATE
			// refusal, so it has to carry the JSON error envelope — that
			// is what distinguishes it from a handler that fell over.
			if rr.Code == http.StatusServiceUnavailable {
				if !strings.Contains(body, `"error"`) {
					t.Errorf("GET %s: 503 without an error envelope; a refusal must say why\nbody: %s",
						path, truncate(body, 300))
				}
				return
			}
			if rr.Code >= 500 {
				t.Errorf("GET %s: status %d on an empty bridge\nbody: %s",
					path, rr.Code, truncate(body, 300))
			}
		})
	}
}

// readOnlyAPIRoutes extracts every `GET /api/...` pattern registered in
// admin.go's mux, by reading the source. Reflection can't reach a
// ServeMux's patterns, and a hand-written list is the thing this is
// meant to protect against.
func readOnlyAPIRoutes(t *testing.T) []string {
	t.Helper()
	src := readFile(t, "admin.go")
	var out []string
	for _, line := range strings.Split(src, "\n") {
		const marker = `mux.HandleFunc("GET /api/`
		i := strings.Index(line, marker)
		if i < 0 {
			continue
		}
		rest := line[i+len(`mux.HandleFunc("`):]
		end := strings.IndexByte(rest, '"')
		if end < 0 {
			continue
		}
		pattern := rest[:end] // e.g. `GET /api/stats`
		_, path, ok := strings.Cut(pattern, " ")
		if !ok {
			continue
		}
		out = append(out, path)
	}
	if len(out) < 10 {
		t.Fatalf("only found %d GET /api routes by scanning admin.go; the scan is broken and "+
			"this test would pass vacuously", len(out))
	}
	sort.Strings(out)
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// smokeBodyReaderCloses keeps io imported for the helper above without a
// blank import; it is used by the module-load test's sibling helpers.
var _ = io.Discard
