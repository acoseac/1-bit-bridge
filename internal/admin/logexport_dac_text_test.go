package admin

import (
	"net/http"
	"strings"
	"testing"
)

// B882L: what the bug-report bundle's header and the Diagnostics page say a
// redaction drops, beside the paths and the IPs.

// TestTheBundleAndTheConsoleSayWhatTheRedactionDrops: the bundle's header,
// when it is redacted, and the Diagnostics page's text for the redacted export
// say that a DAC's product string is dropped from the player's lines that
// name a DAC, its USB IDs kept.
func TestTheBundleAndTheConsoleSayWhatTheRedactionDrops(t *testing.T) {
	s := logTestServer(t, "")
	fold := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	bundle := fold(doGet(t, s, "/api/logs/bundle").Body.String())
	const header = "a DAC's product string dropped from the player's lines that name a DAC, its USB IDs kept."
	if !strings.Contains(bundle, header) {
		t.Errorf("the redacted bundle's header does not say %q:\n%s", header, bundle)
	}
	if plain := fold(doGet(t, s, "/api/logs/bundle?redact=false").Body.String()); strings.Contains(plain, "product string") {
		t.Errorf("an unredacted bundle's header says what a redaction drops:\n%s", plain)
	}
	rec := doGet(t, s, "/diagnostics")
	if rec.Code != http.StatusOK {
		t.Fatalf("/diagnostics: %d", rec.Code)
	}
	const hint = "drops a DAC's product string from the player's lines that name a DAC, keeping its USB IDs."
	if page := fold(rec.Body.String()); !strings.Contains(page, hint) {
		t.Errorf("the Diagnostics page's text for the redacted export does not say %q", hint)
	}
}
