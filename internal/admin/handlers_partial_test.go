package admin

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestPartialBoostRendersContentOnly pins the server half of the partial
// boost (PR 11): with X-Bridge-Partial: 1, renderPage emits the "content"
// block alone — the same inner HTML <main> holds — and reports the page's
// authoritative active tab + section in headers, so the client router can
// swap <main> in place while the persistent chrome (header, nav, and the
// player's <audio> element on <body>) survives.
//
// The same route WITHOUT the header must still render the full document, or
// a direct visit / a client that can't boost gets a headless fragment.
func TestPartialBoostRendersContentOnly(t *testing.T) {
	srv, _, _ := newTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Every page route, with the tab + section the server should report and
	// a content marker that appears in that page's "content" block.
	cases := []struct {
		path        string
		wantTab     string
		wantSection string
		marker      string // substring unique to the page's content
	}{
		{"/", "player", "player", `id="player-root"`},
		{"/stats", "stats", "stats", `id="tracks-indexed"`},
		{"/settings", "settings", "settings", `id="settings-form"`},
		{"/devices", "devices", "server", `class="subnav"`},
		{"/library", "library", "server", `class="subnav"`},
		{"/library/inspector", "library_inspector", "server", `class="subnav"`},
		{"/library/duplicates", "duplicates", "server", `class="subnav"`},
		{"/jobs", "jobs", "server", `class="subnav"`},
		{"/data", "data", "server", `class="subnav"`},
		{"/diagnostics", "diagnostics", "server", `class="subnav"`},
		{"/smartmixes", "smartmixes", "server", `class="subnav"`},
	}

	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			// --- partial ---
			req, _ := http.NewRequest(http.MethodGet, ts.URL+tc.path, nil)
			req.Header.Set("X-Bridge-Partial", "1")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			s := string(body)

			if resp.StatusCode != http.StatusOK {
				t.Fatalf("partial GET %s: status %d, want 200", tc.path, resp.StatusCode)
			}
			if got := resp.Header.Get("X-Bridge-Active"); got != tc.wantTab {
				t.Errorf("partial %s: X-Bridge-Active=%q, want %q", tc.path, got, tc.wantTab)
			}
			if got := resp.Header.Get("X-Bridge-Section"); got != tc.wantSection {
				t.Errorf("partial %s: X-Bridge-Section=%q, want %q", tc.path, got, tc.wantSection)
			}
			// The fragment is content ONLY: the layout wrappers must be absent,
			// or the client would inject a second <html>/<head>/nav into the
			// live document.
			for _, forbidden := range []string{"<!doctype", "<html", "<head>", `id="primary-nav"`, "/static/app.js"} {
				if strings.Contains(strings.ToLower(s), strings.ToLower(forbidden)) {
					t.Errorf("partial %s: fragment leaked layout chrome %q", tc.path, forbidden)
				}
			}
			if !strings.Contains(s, tc.marker) {
				t.Errorf("partial %s: fragment missing content marker %q", tc.path, tc.marker)
			}

			// --- full (no header) ---
			fresp, err := http.Get(ts.URL + tc.path)
			if err != nil {
				t.Fatal(err)
			}
			fbody, _ := io.ReadAll(fresp.Body)
			fresp.Body.Close()
			full := strings.ToLower(string(fbody))
			if !strings.Contains(full, "<html") || !strings.Contains(full, `id="primary-nav"`) {
				t.Errorf("full GET %s: expected a complete document with layout chrome", tc.path)
			}
			// Vary must be advertised on BOTH shapes so a cache can't cross them.
			if !strings.Contains(fresp.Header.Get("Vary"), "X-Bridge-Partial") {
				t.Errorf("full GET %s: Vary header missing X-Bridge-Partial", tc.path)
			}
		})
	}
}
