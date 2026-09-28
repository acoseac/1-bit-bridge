package config

// A URL naming a port and no host (`https://:8443`) has a url.URL.Host and no
// hostname, and Go's client dials it on THIS machine (measured in #1069).
// Three config values accepted one (backlog B36, the validators #1069 left):
// customEndpoints, advertised to every phone, and the enrich and harvest base
// URLs, which the bridge dials. The endpoint list prunes it, as it prunes
// every entry a phone cannot use. The base URLs are warned about and still
// load: they loaded before, and refusing one would stop the bridge from
// starting after an update.

import (
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

func TestValidateCustomEndpoints_DropsAPortWithNoHost(t *testing.T) {
	kept, warns := ValidateCustomEndpoints([]string{
		"https://:8443",
		"https://:443/",
		"https://bridge.example.com:8443",
	})
	if len(kept) != 1 || kept[0] != "https://bridge.example.com:8443" {
		t.Errorf("kept = %q, want only the entry that names a host", kept)
	}
	if len(warns) != 2 {
		t.Errorf("warnings = %v, want one per dropped entry", warns)
	}
}

// TestNormalizeWarnsOfABaseURLThatNamesNoHostAndStillLoads drives the load
// sequence for each base URL the bridge dials. Each loads, with one warning
// naming the field; a base that names its host is not warned about.
func TestNormalizeWarnsOfABaseURLThatNamesNoHostAndStillLoads(t *testing.T) {
	for _, tc := range []struct {
		field string
		set   func(*Config, string)
		bad   string
		good  string
	}{
		{"enrich.musicbrainzBaseURL", func(c *Config, v string) { c.Enrich.MusicBrainzBaseURL = v }, "http://:5000/", "http://localhost:5000"},
		{"enrich.coverArtBaseURL", func(c *Config, v string) { c.Enrich.CoverArtBaseURL = v }, "http://:5001", "http://127.0.0.1:5001"},
		{"atlas.harvestBaseUrl", func(c *Config, v string) { c.Atlas.HarvestBaseURL = v }, "https://:8443", "https://atlas.example"},
	} {
		for _, v := range []struct {
			name, value string
			warn        bool
		}{{"names no host", tc.bad, true}, {"names a host", tc.good, false}} {
			t.Run(tc.field+" "+v.name, func(t *testing.T) {
				rec := loggingtest.Record(t)
				c := mkLoopbackConfig(t)
				tc.set(c, v.value)
				if err := c.NormalizeAndValidate(); err != nil {
					t.Fatalf("%s = %q: %v; the bridge would not start", tc.field, v.value, err)
				}
				lines := rec.Failures(baseURLNamesNoHostWarning)
				warned := len(lines) == 1 && strings.Contains(lines[0], "field="+tc.field)
				if warned != v.warn || len(lines) > 1 {
					t.Errorf("%s = %q: warnings %q, want a warning: %v", tc.field, v.value, lines, v.warn)
				}
			})
		}
	}
}
