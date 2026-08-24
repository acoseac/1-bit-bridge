package admin

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestRetiredInspectorURLsStillResolve pins the redirect that keeps the
// Library Inspector's URLs working after the page itself was retired.
//
// Two of its three shapes were reachable from outside the page — a
// bookmark, and the Smart Mixes harmonic wheel's deep link — so a 404
// would read as a broken console rather than as a moved feature. Each
// shape has an exact successor, and `?camelot=` in particular must NOT
// land on /folders: a harmonic key is not a place, and a folder tree
// cannot filter by one.
func TestRetiredInspectorURLsStillResolve(t *testing.T) {
	srv, _, _ := newTestServer(t)

	for _, tc := range []struct{ from, want string }{
		{"/library/inspector", "/folders"},
		{"/library/inspector?camelot=8A", "/tracks?camelot=8A"},
		{"/library/inspector?path=Music%2FAlbum", "/folders?path=Music%2FAlbum"},
		// A path that normalises to the whole library is the root
		// folder view, which is what the Inspector showed for it too.
		{"/library/inspector?path=%2F%2F", "/folders"},
		// Traversal never becomes a redirect target.
		{"/library/inspector?path=..%2F..%2Fetc", "/folders"},
	} {
		t.Run(tc.from, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.from, nil)
			req.RemoteAddr = "127.0.0.1:54321"
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, req)
			if w.Code != http.StatusMovedPermanently {
				t.Fatalf("status %d, want 301", w.Code)
			}
			if got := w.Header().Get("Location"); got != tc.want {
				t.Errorf("Location = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestRetiredInspectorRedirectTargetsAreRegistered: a redirect to a
// route that does not exist trades a 404 for a slower 404. Both targets
// are player routes, which the shell serves for a cold load.
func TestRetiredInspectorRedirectTargetsAreRegistered(t *testing.T) {
	srv, _, _ := newTestServer(t)
	for _, target := range []string{"/folders", "/tracks?camelot=8A"} {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		req.RemoteAddr = "127.0.0.1:54321"
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		if w.Code == http.StatusNotFound {
			t.Errorf("redirect target %s is not a registered route", target)
		}
	}
}

// TestInspectorPageIsGone is the deletion's own pin: the page must not
// come back by accident, and the redirect must not quietly become a
// rendered page again.
func TestInspectorPageIsGone(t *testing.T) {
	if _, ok := pages["library_inspector"]; ok {
		t.Error("library_inspector is still a registered page template")
	}
	if _, err := templateFS.ReadFile("templates/library_inspector.html"); err == nil {
		t.Error("templates/library_inspector.html is still embedded")
	}
}
