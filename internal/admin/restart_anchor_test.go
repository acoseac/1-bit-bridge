package admin

import (
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
)

// settingsAnchorRe finds every `/settings#<anchor>` link app.js emits.
var settingsAnchorRe = regexp.MustCompile(`/settings#([A-Za-z0-9_-]+)`)

// TestAppJSSettingsAnchorsExistInTheRenderedPage is the F5 pin, and it is a
// CROSS-FILE one on purpose: the bug was a link in app.js pointing at an
// anchor (`#danger`) that exists in no template, so following the UPnP
// "Restart required" banner landed the operator on a page with nothing
// highlighted — and, because `#restart-btn` is `hidden` until a
// restart-requiring save reveals it, no Restart button either.
//
// Neither file is wrong on its own; only the pair is. So assert the pair:
// every anchor app.js links to must be an id the settings page actually
// renders.
func TestAppJSSettingsAnchorsExistInTheRenderedPage(t *testing.T) {
	js, err := os.ReadFile("static/app.js")
	if err != nil {
		t.Fatalf("read app.js: %v", err)
	}
	matches := settingsAnchorRe.FindAllStringSubmatch(string(js), -1)
	if len(matches) == 0 {
		t.Skip("app.js links to no /settings anchors")
	}

	srv, _, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/settings", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /settings: status %d", w.Code)
	}
	page := w.Body.String()

	seen := map[string]bool{}
	for _, m := range matches {
		anchor := m[1]
		if seen[anchor] {
			continue
		}
		seen[anchor] = true
		if !strings.Contains(page, `id="`+anchor+`"`) {
			t.Errorf("app.js links to /settings#%s but the settings page renders no "+
				"element with that id — the operator lands on the page with nothing "+
				"to act on", anchor)
		}
	}
}

// TestSettingsPageRendersRestartActionsAnchor pins the target itself, so a
// future template edit that drops the id fails here rather than only through
// the cross-file test above.
func TestSettingsPageRendersRestartActionsAnchor(t *testing.T) {
	srv, _, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/settings", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /settings: status %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `id="restart-actions"`) {
		t.Error(`settings page renders no id="restart-actions" — the UPnP ` +
			`"Restart required" banner links there`)
	}
	// The button must still exist inside it; the anchor alone is useless.
	if !strings.Contains(body, `id="restart-btn"`) {
		t.Error(`settings page renders no id="restart-btn"`)
	}
}
