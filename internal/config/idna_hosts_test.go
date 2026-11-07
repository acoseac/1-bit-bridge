package config

// B744: a customEndpoints entry or an autocert.domain whose host is not ASCII
// loaded as written, and every certificate mint then refused it
// (x509.CreateCertificate takes an ASCII DNS name alone, an IA5String). What
// loads takes the host's IDNA ASCII form, idna.Lookup's (the lookup profile,
// which maps case: münchen.de and MÜNCHEN.DE are both xn--mnchen-3ya.de), the
// conversion the host's own name takes since B739; a host of ASCII alone is
// kept byte for byte; one with no IDNA form takes its field's existing repair,
// the prune or InvalidAutocertDomain, with a warning, and the config still
// loads. What is typed is converted, or refused when it has no form.

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// noIDNAHosts are hosts url.Parse reads that have no IDNA ASCII form: an
// underscore beside a character that is not ASCII (idna.Lookup refuses
// U+005F there), a label that begins with a hyphen, and a label whose IDNA
// form is 64 bytes, past a DNS label's 63, which Lookup does not check.
var noIDNAHosts = []string{"a_ü.example", "-ü.example", strings.Repeat("ж", 58) + ".example"}

// writeIDNAConfig writes a config holding body beside a library root, and
// returns its path.
func writeIDNAConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	lib := filepath.Join(dir, "lib")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "bridge.yaml")
	yaml := "libraryRoots:\n  - " + yamlStr(lib) + "\n" + body
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgPath
}

// TestALoadedCustomEndpointThatIsNotASCIITakesItsIDNAForm loads a config
// whose customEndpoints hold hosts that are not ASCII, from the file and from
// BRIDGE_CUSTOM_ENDPOINTS: each with an IDNA form is kept in it, its port and
// its place kept, and said once; an ASCII host is kept byte for byte, its case
// included, an xn-- one too; one with no form is dropped with a warning
// naming its place, as the prune drops any entry it cannot publish. The
// config loads.
func TestALoadedCustomEndpointThatIsNotASCIITakesItsIDNAForm(t *testing.T) {
	in := []string{
		"https://münchen.de:7788",
		"https://" + noIDNAHosts[0] + ":7788",
		"https://MÜNCHEN.DE:7789",
		"https://Bücher.Example/",
		"https://Example.COM:8443",
		"https://" + noIDNAHosts[2] + ":7788",
		"https://xn--mnchen-3ya.de:9000",
	}
	want := []string{
		"https://xn--mnchen-3ya.de:7788",
		"https://xn--mnchen-3ya.de:7789",
		"https://xn--bcher-kva.example/",
		"https://Example.COM:8443",
		"https://xn--mnchen-3ya.de:9000",
	}
	for _, from := range []string{"the file", "BRIDGE_CUSTOM_ENDPOINTS"} {
		t.Run(from, func(t *testing.T) {
			rec := loggingtest.Record(t)
			body := "customEndpoints:\n"
			for _, e := range in {
				body += "  - " + yamlStr(e) + "\n"
			}
			if from != "the file" {
				body = ""
				t.Setenv("BRIDGE_CUSTOM_ENDPOINTS", strings.Join(in, ","))
			}
			cfg, err := Load(writeIDNAConfig(t, body))
			if err != nil {
				t.Fatalf("Load: %v; a host with no IDNA form is dropped, never a refusal to start", err)
			}
			if !slices.Equal(cfg.CustomEndpoints, want) {
				t.Errorf("customEndpoints loaded as\n  %q\nwant\n  %q", cfg.CustomEndpoints, want)
			}
			dropped := rec.Failures("dropped invalid custom endpoint")
			if len(dropped) != 2 || !strings.Contains(dropped[0], "customEndpoints[1]") || !strings.Contains(dropped[1], "customEndpoints[5]") {
				t.Errorf("want one warning for each entry with no IDNA form, naming customEndpoints[1] and [5]: %q", dropped)
			}
			converted := 0
			for _, l := range rec.All() {
				if strings.Contains(l, "IDNA ASCII form") && !strings.HasPrefix(l, "WARN") && !strings.HasPrefix(l, "ERROR") {
					converted++
				}
			}
			if converted != 3 {
				t.Errorf("want one line, below Warn, for each of the three entries converted, got %d:\n%s", converted, strings.Join(rec.All(), "\n"))
			}

			// What a save stores loads unchanged and says nothing more.
			before := len(rec.All())
			c := Clone(cfg)
			if err := c.NormalizeAndValidate(); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(c.CustomEndpoints, want) || len(rec.All()) != before {
				t.Errorf("a second Normalize changed the list or logged again: %q, %q", c.CustomEndpoints, rec.All()[before:])
			}
		})
	}
}

// TestALoadedAutocertDomainThatIsNotASCIITakesItsIDNAForm loads public
// configs whose autocert.domain is not ASCII: one with an IDNA form is served
// in it, after the host is read out of a URL as before (AutocertHost); an
// ASCII one is kept byte for byte; one with no form is served as
// InvalidAutocertDomain with a warning, the field's existing repair. Every
// config loads.
func TestALoadedAutocertDomainThatIsNotASCIITakesItsIDNAForm(t *testing.T) {
	type row struct{ in, want string }
	rows := []row{
		{"münchen.de", "xn--mnchen-3ya.de"},
		{"MÜNCHEN.DE", "xn--mnchen-3ya.de"},
		{"https://münchen.de:443", "xn--mnchen-3ya.de"},
		{"Bridge.Example.Test", "Bridge.Example.Test"},
		{"xn--mnchen-3ya.de", "xn--mnchen-3ya.de"},
	}
	for _, h := range noIDNAHosts {
		rows = append(rows, row{h, InvalidAutocertDomain})
	}
	for _, tc := range rows {
		t.Run(tc.in, func(t *testing.T) {
			rec := loggingtest.Record(t)
			cfg, err := Load(writeIDNAConfig(t, "listenAddress: \"0.0.0.0:7788\"\nadminAddress: \"0.0.0.0:7789\"\n"+
				"deployment:\n  mode: public\n  adminTLSTerminatedByProxy: true\n"+
				"autocert:\n  domain: "+yamlStr(tc.in)+"\n"))
			if err != nil {
				t.Fatalf("Load over autocert.domain %q: %v; a config that loaded before must still load", tc.in, err)
			}
			if cfg.Autocert.Domain != tc.want {
				t.Errorf("autocert.domain loaded as %q, want %q", cfg.Autocert.Domain, tc.want)
			}
			// Public mode warns of its own defaults (the Tailscale mode); the
			// lines asked about are the domain's.
			var warned []string
			for _, l := range rec.Failures() {
				if strings.Contains(l, "autocert.domain") {
					warned = append(warned, l)
				}
			}
			switch {
			case tc.want == InvalidAutocertDomain && len(warned) != 1:
				t.Errorf("want one warning for a domain with no IDNA form: %q", warned)
			case tc.want != InvalidAutocertDomain && !strings.Contains(tc.in, "://") && len(warned) != 0:
				t.Errorf("a domain with an IDNA form was warned about: %q", warned)
			}
		})
	}
}

// TestATypedCustomEndpointIsConvertedOrRefused: the console's settings save
// asks CheckCustomEndpoints of the list the operator typed. An entry whose
// host has an IDNA form passes (Normalize then stores that form), and one
// whose host has none is refused, naming its place: the prune would drop it,
// storing a list the operator did not type.
func TestATypedCustomEndpointIsConvertedOrRefused(t *testing.T) {
	if err := CheckCustomEndpoints([]string{"https://ok.example:7788", "https://münchen.de:7788", "https://MÜNCHEN.DE"}); err != nil {
		t.Errorf("a typed host with an IDNA form was refused: %v", err)
	}
	for _, h := range noIDNAHosts {
		err := CheckCustomEndpoints([]string{"https://ok.example:7788", "https://" + h + ":7788"})
		if err == nil {
			t.Errorf("a typed host with no IDNA form, %q, was not refused", h)
			continue
		}
		if !strings.Contains(err.Error(), "customEndpoints[1]") || !strings.Contains(err.Error(), "IDNA") {
			t.Errorf("the refusal of %q does not name the entry's place and why: %v", h, err)
		}
	}
	// An entry the prune drops for another reason is left to it, as before.
	if err := CheckCustomEndpoints([]string{"http://münchen.de:7788"}); err != nil {
		t.Errorf("an entry the prune drops for its scheme was refused at the save: %v", err)
	}
}
