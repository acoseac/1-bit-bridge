package admin

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// settingsFieldRe finds every named form control in settings.html.
var settingsFieldRe = regexp.MustCompile(`<(?:input|select|textarea)[^>]*\bname="([A-Za-z0-9_]+)"`)

// fieldsNotSentToTheServer are rendered controls that deliberately do
// NOT appear in the PATCH payload. Each needs a reason, because the
// default assumption — a field that renders is a field that saves — is
// what this test enforces.
var fieldsNotSentToTheServer = map[string]string{
	"enrichSource":             "UI-only picker; app.js maps it into the two base URLs via mapEnrichSourceToBases",
	"enrichAtlasURL":           "UI-only input feeding the same mapping",
	"enrichMusicBrainzBaseURL": "sent, but under a key derived by mapEnrichSourceToBases",
	"enrichCoverArtBaseURL":    "sent, but under a key derived by mapEnrichSourceToBases",
	"customEndpoints":          "sent as customEndpointsText — the server owns splitting/validation",
	"dataDir":                  "read-only display; set at bridge init",
}

// TestEverySettingsFieldIsMappedIntoThePatchPayload pins a cross-file
// contract that has already been broken once, in the commit that made
// the backup fields editable.
//
// The failure mode is specific and nasty: app.js builds the PATCH body
// from an explicit ALLOWLIST, not a FormData dump. Add a field to the
// template and forget the allowlist and the control renders, the
// operator changes it, the page says "Saved." — and nothing happened.
// That is strictly worse than not offering the field, because the
// operator now believes a setting is applied that isn't.
//
// Caught in a browser, not in review. Hence a test.
func TestEverySettingsFieldIsMappedIntoThePatchPayload(t *testing.T) {
	tmpl, err := templateFS.ReadFile("templates/settings.html")
	if err != nil {
		t.Fatalf("read settings.html: %v", err)
	}
	js, err := staticFS.ReadFile("static/app.js")
	if err != nil {
		t.Fatalf("read app.js: %v", err)
	}
	source := string(js)

	seen := map[string]bool{}
	for _, m := range settingsFieldRe.FindAllStringSubmatch(string(tmpl), -1) {
		name := m[1]
		if seen[name] {
			continue
		}
		seen[name] = true
		if reason, ok := fieldsNotSentToTheServer[name]; ok {
			// Still assert app.js knows the name at all — an exempt
			// field that vanished from the JS entirely is a different
			// bug wearing the same clothes.
			if !strings.Contains(source, `"`+name+`"`) {
				t.Errorf("field %q is exempt (%s) but app.js does not mention it at all",
					name, reason)
			}
			continue
		}
		// The payload reads every field as fd.get("<name>").
		if !strings.Contains(source, `fd.get("`+name+`")`) {
			t.Errorf("settings.html renders a control named %q but app.js never reads it "+
				"with fd.get(%q) — the PATCH payload is an explicit allowlist, so this "+
				"field will render, accept an edit, report \"Saved.\", and change nothing",
				name, name)
		}
	}
	if len(seen) < 10 {
		t.Fatalf("only found %d named fields — the regex probably stopped matching", len(seen))
	}
}

// boolPtrT is a test-local helper for the nil-means-on config fields.
func boolPtrT(b bool) *bool { return &b }

// TestPrimaryNavHighlightsEveryEntry replaces what the old
// hand-enumerated CSS selector list did implicitly, and does it better.
//
// The paint is now keyed on aria-current, which layout.html emits from
// ActiveSection. That means a nav entry whose section key never matches
// renders permanently unhighlighted — which is invisible in review and
// exactly what the enumerated selectors used to get wrong when someone
// added an entry and forgot to extend the list. Assert every entry
// lights up on its own landing page.
func TestPrimaryNavHighlightsEveryEntry(t *testing.T) {
	srv, _, _ := newTestServer(t)
	for _, tc := range []struct{ path, tab string }{
		{"/", "player"},
		{"/stats", "stats"},
		{"/settings", "settings"},
		{"/devices", "server"},
		// Every operator page folds into the Server entry.
		{"/library", "server"},
		{"/library/inspector", "server"},
		{"/jobs", "server"},
		{"/diagnostics", "server"},
		{"/data", "server"},
	} {
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		req.RemoteAddr = "127.0.0.1:54321"
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Errorf("GET %s: status %d", tc.path, w.Code)
			continue
		}
		body := w.Body.String()
		want := `data-tab="` + tc.tab + `" aria-current="page"`
		if !strings.Contains(body, want) {
			t.Errorf("GET %s: nav entry %q is not marked current — the CSS keys on "+
				"aria-current, so it renders unhighlighted and is announced to a screen "+
				"reader as an ordinary link", tc.path, tc.tab)
		}
		// Exactly one PRIMARY nav entry may be current. The count has
		// to be scoped to #primary-nav: the Server section also renders
		// a .subnav whose active page carries its own aria-current, so
		// every operator page legitimately yields two document-wide and
		// a bare count of the whole body can only ever assert ">= 1".
		// That weaker form would pass while two primary entries were
		// lit at once, which is the actual defect worth catching —
		// aria-current is what the CSS keys on AND what a screen reader
		// announces, so two of them is both a visual and an a11y bug.
		nav := primaryNavMarkup(t, tc.path, body)
		if n := strings.Count(nav, `aria-current="page"`); n != 1 {
			t.Errorf("GET %s: %d primary nav entries marked current, want exactly 1\n%s",
				tc.path, n, nav)
		}
	}
}

// primaryNavMarkup returns just the <nav id="primary-nav"> element, so a
// count of aria-current inside it is not confounded by the Server section's
// .subnav, which marks its own active page the same way.
func primaryNavMarkup(t *testing.T, path, body string) string {
	t.Helper()
	const open = `<nav id="primary-nav">`
	i := strings.Index(body, open)
	if i < 0 {
		t.Fatalf("GET %s: no %s in the rendered page — layout.html moved and this "+
			"assertion would otherwise silently scope to nothing", path, open)
	}
	rest := body[i:]
	j := strings.Index(rest, "</nav>")
	if j < 0 {
		t.Fatalf("GET %s: primary nav is unclosed", path)
	}
	return rest[:j]
}
