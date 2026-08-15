package admin

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/config"
)

// TestSettingsResponseFromConfigReflectsLiveConfig pins the shared builder
// that backs BOTH apiSettingsGet and pageSettings. The enrich base URLs,
// atlas-enabled flag, and Tailscale mode must mirror the config — pageSettings
// previously omitted these from its own struct literal, so the Settings →
// General tab rendered them blank regardless of the live config (and a Save
// would clobber them back to defaults).
func TestSettingsResponseFromConfigReflectsLiveConfig(t *testing.T) {
	cfg := &config.Config{
		LibraryName: "Test",
		DataDir:     t.TempDir(),
		Enrich: config.EnrichConfig{
			MusicBrainzBaseURL: "https://atlas.test/ws/2",
			CoverArtBaseURL:    "https://atlas.test",
		},
		Atlas: config.AtlasConfig{Enabled: true},
	}
	resp := settingsResponseFromConfig(cfg, false)
	if resp.EnrichMusicBrainzBaseURL != "https://atlas.test/ws/2" {
		t.Errorf("EnrichMusicBrainzBaseURL = %q, want the Atlas URL", resp.EnrichMusicBrainzBaseURL)
	}
	if resp.EnrichCoverArtBaseURL != "https://atlas.test" {
		t.Errorf("EnrichCoverArtBaseURL = %q, want the Atlas URL", resp.EnrichCoverArtBaseURL)
	}
	if !resp.AtlasEnabled {
		t.Error("AtlasEnabled = false, want true (rich metadata enabled in config)")
	}
	if resp.TailscaleMode == "" {
		t.Error("TailscaleMode empty, want a concrete effective mode so the dropdown renders a selection")
	}
}

// TestSettingsPageRendersEnrichURLFromConfig is the end-to-end regression for
// the reported bug: the server-rendered /settings General tab must show the
// configured enrich base URL as the input VALUE — not leave the input empty
// (which displays only the public-default placeholder). Pre-fix pageSettings
// omitted the field, so the URL rendered blank and a Save sent "" → clobbering
// the Atlas config.
func TestSettingsPageRendersEnrichURLFromConfig(t *testing.T) {
	srv, _, _ := newTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp := patchSettings(t, ts.URL, `{"enrichMusicBrainzBaseURL":"https://atlas.test/ws/2"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH enrich URL: status %d, want 200", resp.StatusCode)
	}

	gresp, err := http.Get(ts.URL + "/settings")
	if err != nil {
		t.Fatal(err)
	}
	defer gresp.Body.Close()
	if gresp.StatusCode != http.StatusOK {
		t.Fatalf("GET /settings: status %d, want 200", gresp.StatusCode)
	}
	body, _ := io.ReadAll(gresp.Body)
	if !strings.Contains(string(body), `value="https://atlas.test/ws/2"`) {
		t.Error("/settings did not render the configured enrich URL as an input value — pageSettings omitting the field again?")
	}
}

// TestSettingsPageRendersEnrichmentTab pins the dedicated Enrichment tab: the
// tab button + pane exist, the source picker derives "atlas" from an
// Atlas-shaped config (with the URL prefilled), and the enrichment fields no
// longer live inside the General pane.
func TestSettingsPageRendersEnrichmentTab(t *testing.T) {
	srv, _, _ := newTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp := patchSettings(t, ts.URL,
		`{"enrichMusicBrainzBaseURL":"https://atlas.test/ws/2","enrichCoverArtBaseURL":"https://atlas.test"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH enrich URLs: status %d, want 200", resp.StatusCode)
	}

	gresp, err := http.Get(ts.URL + "/settings")
	if err != nil {
		t.Fatal(err)
	}
	defer gresp.Body.Close()
	body, _ := io.ReadAll(gresp.Body)
	page := string(body)

	for _, want := range []string{
		`data-tab="enrichment"`,
		`id="settings-panel-enrichment"`,
		`name="enrichSource"`,
		`name="enrichAtlasURL"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("/settings missing %s", want)
		}
	}
	// Atlas-shaped config → the atlas option is selected + URL prefilled.
	if !strings.Contains(page, `value="atlas" selected`) {
		t.Error("/settings source picker did not derive 'atlas' from the Atlas-shaped config")
	}
	if !strings.Contains(page, `name="enrichAtlasURL" type="url" value="https://atlas.test"`) {
		t.Error("/settings did not prefill the Atlas URL field from the derived config")
	}
	// The General pane must no longer host the enrichment fields — they
	// moved to the Enrichment pane (raw fields live in its Advanced block).
	generalPane := page[strings.Index(page, `id="settings-panel-general"`):strings.Index(page, `id="settings-panel-enrichment"`)]
	if strings.Contains(generalPane, "enrichMusicBrainzBaseURL") {
		t.Error("General pane still contains the enrichment fields — the move to the Enrichment tab regressed")
	}
}

// TestSettingsPageRendersNewControls pins the console controls added by
// the 2026-08-14 feature review: the upscale-target picker (P2-34 —
// PATCH /api/upscale/target had no console control; initUpscaleTarget
// in app.js looks these ids up) and the two formerly YAML-only toggles
// (P2-37). The name-less target selects are load-bearing: a `name`
// attribute would serialise them into the settings form's Save payload,
// but the target is DB-backed and applies through its own PATCH.
func TestSettingsPageRendersNewControls(t *testing.T) {
	srv, _, _ := newTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	gresp, err := http.Get(ts.URL + "/settings")
	if err != nil {
		t.Fatal(err)
	}
	defer gresp.Body.Close()
	body, _ := io.ReadAll(gresp.Body)
	page := string(body)

	for _, want := range []string{
		`id="upscale-target-rate"`,
		`id="upscale-target-bits"`,
		`id="upscale-target-apply"`,
		`name="libraryWatchEnabled"`,
		`name="optimizeEnabled"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("/settings missing %s", want)
		}
	}
	if strings.Contains(page, `name="upscale-target-rate"`) ||
		strings.Contains(page, `name="upscale-target-bits"`) {
		t.Error("target selects must stay name-less — a named control would ride the settings form's Save payload")
	}
	// optimizeEnabled resolves true on an unset YAML pointer, so the
	// fresh-install page must render the checkbox checked.
	optIdx := strings.Index(page, `name="optimizeEnabled"`)
	if optIdx < 0 {
		t.Fatal("optimizeEnabled checkbox missing")
	}
	optTag := page[optIdx : strings.Index(page[optIdx:], ">")+optIdx]
	if !strings.Contains(optTag, "checked") {
		t.Errorf("optimizeEnabled renders unchecked on a fresh install (%q); the nil YAML pointer resolves true", optTag)
	}
}
