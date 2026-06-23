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
