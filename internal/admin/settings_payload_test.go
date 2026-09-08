package admin

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// uiOnlySettingsInputs are form controls with no settings-PATCH field of
// their own. Each needs a reason, because "it is not in the payload" is
// exactly what a forgotten field looks like.
var uiOnlySettingsInputs = map[string]string{
	// Resolved into enrichMusicBrainzBaseURL + enrichCoverArtBaseURL by
	// mapEnrichSourceToBases before the payload is built. The picker
	// declares `data-managed-with` so it hides with the fields it feeds.
	"enrichSource":   "resolved into the two enrich base URLs",
	"enrichAtlasURL": "resolved into the two enrich base URLs",
	// Sent as customEndpointsText — the textarea string, so the server
	// stays the single source of truth on splitting and validation.
	"customEndpoints": "sent as customEndpointsText",
}

// TestEverySettingsInputReachesThePayload is the guard behind
// buildSettingsPayload's docblock.
//
// The payload is an explicit allowlist, not a FormData dump, so a control
// added to the template without a line in the builder saves nothing while
// the page still reports "Saved." That is worse than not offering the
// control at all, and it has been caught in a browser rather than by a
// test more than once — the retention controls shipped that way, and the
// backup cadence fields did too.
//
// Reads the template for what is OFFERED and the builder for what is
// SENT. Neither side can be checked from the other's file, which is the
// whole point: the defect is a disagreement between two files nothing
// else compares.
func TestEverySettingsInputReachesThePayload(t *testing.T) {
	tmpl, err := os.ReadFile("templates/settings.html")
	if err != nil {
		t.Fatal(err)
	}
	// CRLF-normalised: no .gitattributes pins eol, so a Windows checkout
	// would otherwise make every \n-anchored scan here find nothing.
	tmplSrc := strings.ReplaceAll(string(tmpl), "\r\n", "\n")

	// The package's own helper: it strips COMMENTS, which this scan needs.
	// The payload block's commentary names half the fields it discusses
	// (`dlnaEnabled`, `mdnsEnabled`, `updateAutoInstall`), so an unstripped
	// window would answer yes to almost anything and the test would pass
	// vacuously.
	builder := jsFunctionBody(t, "function buildSettingsPayload(")
	// A negative control on the extraction itself: the window must hold
	// the allowlist, not merely exist.
	if !strings.Contains(builder, "libraryName:") {
		t.Fatal("buildSettingsPayload does not contain the payload — did the extraction lose it?")
	}

	names := regexp.MustCompile(`name="([a-zA-Z][a-zA-Z0-9]*)"`).FindAllStringSubmatch(tmplSrc, -1)
	seen := map[string]bool{}
	var missing []string
	for _, m := range names {
		name := m[1]
		if seen[name] {
			continue
		}
		seen[name] = true
		if _, exempt := uiOnlySettingsInputs[name]; exempt {
			continue
		}
		if !strings.Contains(builder, name) {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	for _, name := range missing {
		t.Errorf("settings.html offers %q and buildSettingsPayload never sends it — "+
			"the control will save nothing while the page reports \"Saved.\" "+
			"Add it to the payload, or to uiOnlySettingsInputs with a reason.", name)
	}
	if len(seen) < 20 {
		t.Fatalf("only found %d named inputs in settings.html — the scan is not matching, so this test proves nothing", len(seen))
	}

	// And the other direction for the exemptions: a name listed as
	// UI-only must actually be in the template, or the list is stale and
	// silently exempting nothing.
	for name := range uiOnlySettingsInputs {
		if !seen[name] {
			t.Errorf("uiOnlySettingsInputs names %q, which settings.html no longer offers — stale exemption", name)
		}
	}
}
