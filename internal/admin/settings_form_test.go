package admin

import (
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
