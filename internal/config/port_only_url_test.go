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

// TestNoConfigWarningCarriesAURLsCredentials drives the load sequence with a
// credential in every URL a warning names (review round 1 on #1074). An
// enrich base URL may carry userinfo (normalizeBaseURL accepts it), so the
// warning about a base that names no host logged the password whole; and a
// dropped custom endpoint was quoted whole in its warning, a parse failure
// twice (the parse error quotes it too). Each warning still fires, still
// says which entry, and none carries the secret: not a password, not a token
// written as the user name (which url.URL.Redacted would have kept), not a
// query.
func TestNoConfigWarningCarriesAURLsCredentials(t *testing.T) {
	const secret = "s3cret-Pw"
	rec := loggingtest.Record(t)
	c := mkLoopbackConfig(t)
	c.Enrich.MusicBrainzBaseURL = "http://user:" + secret + "@:5000"
	c.Enrich.CoverArtBaseURL = "http://" + secret + "@:5001/?apikey=" + secret
	c.CustomEndpoints = []string{
		"https://user:" + secret + "@:8443",              // names no host
		"http://user:" + secret + "@bridge.example:7788", // not https
		"https://user:" + secret + " x@bridge.example",   // does not parse
		"https://bridge.example:8443",                    // kept
	}
	if err := c.NormalizeAndValidate(); err != nil {
		t.Fatalf("NormalizeAndValidate: %v; the bridge would not start", err)
	}
	if len(c.CustomEndpoints) != 1 {
		t.Errorf("kept endpoints = %q, want only the one with a host", c.CustomEndpoints)
	}
	lines := rec.Failures()
	for _, want := range []string{
		"field=enrich.musicbrainzBaseURL value=http://:5000",
		"field=enrich.coverArtBaseURL value=http://:5001",
		"customEndpoints[0] (https://:8443): missing host",
		"customEndpoints[1] (http://bridge.example:7788): scheme must be https",
		"customEndpoints[2]: does not parse as a URL",
	} {
		found := false
		for _, l := range lines {
			found = found || strings.Contains(l, want)
		}
		if !found {
			t.Errorf("no warning says %q; warnings: %q", want, lines)
		}
	}
	for _, l := range lines {
		if strings.Contains(l, secret) {
			t.Errorf("a warning carries the URL's secret: %s", l)
		}
	}
}
