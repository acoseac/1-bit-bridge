package config

// A URL an operator configures can carry a credential: a password, a token
// written as the user name, a token in the query or the fragment. Three
// surfaces published or printed such a URL whole (backlog B54). Two errors
// that stop the bridge from starting quoted the value (normalizeBaseURL's
// and the harvest pin's), and a customEndpoints entry was advertised as
// written by /v1/health, which answers without a token, and baked into every
// pairing QR. The rule: an error or a warning names a configured URL by its
// field, and its scheme and host alone; an endpoint the bridge publishes
// carries no user name, password, query or fragment. A config that loaded
// before still loads: an endpoint it holds is published without them, with a
// warning, and one the operator types is refused.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// credentialSecret is the value every URL in these tests hides somewhere.
// It is a valid URL scheme too ("s3cret-Pw:x@host" parses with it as the
// scheme), which is the shape that reached a warning through the scheme.
const credentialSecret = "s3cret-Pw"

// carriesSecret reports whether s holds credentialSecret in any case:
// url.Parse lowercases a scheme, so a secret that parses as one leaks as
// "s3cret-pw", which a case-sensitive search does not find.
func carriesSecret(s string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(credentialSecret))
}

// TestNoStartupErrorCarriesAURLsCredentials loads a config whose URL field
// carries the secret in every shape an operator can write it, each in a
// value the load refuses. The refusal stops the bridge from starting and is
// printed to its log, by `bridge doctor`'s config-file line, and in the
// console's settings response. It must still refuse, still name the field,
// name the scheme and host where it has them, and never carry the secret:
// normalizeBaseURL's error and the harvest pin's quoted the value whole.
func TestNoStartupErrorCarriesAURLsCredentials(t *testing.T) {
	const s = credentialSecret
	for _, tc := range []struct {
		name, field, yamlKey, value string
		// origin, when set, is the scheme and host the error must name.
		origin string
	}{
		{"enrich password, wrong scheme", "enrich.musicbrainzBaseURL", "enrich:\n  musicbrainzBaseURL",
			"ftp://user:" + s + "@mirror.example/ws/2", "ftp://mirror.example"},
		{"enrich token as the user name", "enrich.coverArtBaseURL", "enrich:\n  coverArtBaseURL",
			"ftp://" + s + "@mirror.example", "ftp://mirror.example"},
		{"enrich query", "enrich.musicbrainzBaseURL", "enrich:\n  musicbrainzBaseURL",
			"ftp://mirror.example/ws/2?apikey=" + s, "ftp://mirror.example"},
		{"enrich fragment", "enrich.musicbrainzBaseURL", "enrich:\n  musicbrainzBaseURL",
			"ftp://mirror.example/ws/2#" + s, "ftp://mirror.example"},
		{"enrich password, no host", "enrich.musicbrainzBaseURL", "enrich:\n  musicbrainzBaseURL",
			"https://user:" + s + "@", ""},
		{"enrich password, no scheme", "enrich.coverArtBaseURL", "enrich:\n  coverArtBaseURL",
			"user:" + s + "@mirror.example", ""},
		{"enrich user name, no scheme", "enrich.coverArtBaseURL", "enrich:\n  coverArtBaseURL",
			s + ":x@mirror.example", ""},
		{"enrich query, no scheme", "enrich.musicbrainzBaseURL", "enrich:\n  musicbrainzBaseURL",
			"mirror.example/ws/2?apikey=" + s, ""},
		{"enrich password that does not parse", "enrich.musicbrainzBaseURL", "enrich:\n  musicbrainzBaseURL",
			"https://user:" + s + " x@mirror.example", ""},
		{"pin password", "atlas.harvestBaseUrl", "atlas:\n  harvestBaseUrl",
			"https://user:" + s + "@atlas.example", "https://atlas.example"},
		{"pin token as the user name", "atlas.harvestBaseUrl", "atlas:\n  harvestBaseUrl",
			"https://" + s + "@atlas.example", "https://atlas.example"},
		{"pin query", "atlas.harvestBaseUrl", "atlas:\n  harvestBaseUrl",
			"https://atlas.example/?token=" + s, "https://atlas.example"},
		{"pin fragment", "atlas.harvestBaseUrl", "atlas:\n  harvestBaseUrl",
			"https://atlas.example/#" + s, "https://atlas.example"},
		{"pin path", "atlas.harvestBaseUrl", "atlas:\n  harvestBaseUrl",
			"https://atlas.example/" + s, "https://atlas.example"},
		{"pin password over http", "atlas.harvestBaseUrl", "atlas:\n  harvestBaseUrl",
			"http://user:" + s + "@atlas.example", "http://atlas.example"},
		{"pin user name, no scheme", "atlas.harvestBaseUrl", "atlas:\n  harvestBaseUrl",
			s + ":x@atlas.example", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			check := func(how string, err error) {
				t.Helper()
				if err == nil {
					t.Fatalf("%s accepted %s = %q; it must still refuse it", how, tc.field, tc.value)
				}
				msg := err.Error()
				if carriesSecret(msg) {
					t.Errorf("%s: the error carries the URL's secret: %s", how, msg)
				}
				if !strings.Contains(msg, tc.field) {
					t.Errorf("%s: the error does not name the field %s: %s", how, tc.field, msg)
				}
				if tc.origin != "" && !strings.Contains(msg, tc.field+" ("+tc.origin+")") {
					t.Errorf("%s: the error does not name the URL's scheme and host as %s (%s): %s",
						how, tc.field, tc.origin, msg)
				}
			}
			_, err := Load(writeEnrichTestConfig(t, tc.yamlKey+": "+yamlStr(tc.value)+"\n"))
			check("Load", err)

			c := mkLoopbackConfig(t)
			switch tc.field {
			case "enrich.musicbrainzBaseURL":
				c.Enrich.MusicBrainzBaseURL = tc.value
			case "enrich.coverArtBaseURL":
				c.Enrich.CoverArtBaseURL = tc.value
			case "atlas.harvestBaseUrl":
				c.Atlas.HarvestBaseURL = tc.value
			}
			check("Validate", c.Validate())
			check("NormalizeAndValidate", c.NormalizeAndValidate())
		})
	}

	// The environment reaches the same check, and an operator sets a mirror's
	// password there as readily as in the file.
	t.Run("an environment override", func(t *testing.T) {
		t.Setenv("BRIDGE_MUSICBRAINZ_BASE_URL", "ftp://user:"+s+"@mirror.example/ws/2")
		_, err := Load(writeEnrichTestConfig(t, ""))
		if err == nil {
			t.Fatal("Load accepted an ftp base URL from the environment")
		}
		if carriesSecret(err.Error()) {
			t.Errorf("the error carries the URL's secret: %v", err)
		}
	})
}

// credentialEndpointShapes is a custom endpoint carrying the secret in each
// part of a URL that can carry one, beside the endpoint each must be
// published as.
var credentialEndpointShapes = []struct{ name, in, published string }{
	{"a password", "https://user:" + credentialSecret + "@a.example:7788", "https://a.example:7788"},
	{"a token as the user name", "https://" + credentialSecret + "@b.example:7788", "https://b.example:7788"},
	{"a query", "https://c.example:7788/?token=" + credentialSecret, "https://c.example:7788/"},
	{"a fragment", "https://d.example:7788/#" + credentialSecret, "https://d.example:7788/"},
	{"all of them", "https://user:" + credentialSecret + "@e.example:7788/bridge?k=" + credentialSecret + "#" + credentialSecret,
		"https://e.example:7788/bridge"},
	{"an IPv6 host", "https://" + credentialSecret + "@[2001:db8::7]:7788", "https://[2001:db8::7]:7788"},
}

// TestACustomEndpointIsPublishedWithoutItsCredentials drives the prune every
// writer and every load runs (ValidateCustomEndpoints, through Normalize) over
// an endpoint carrying the secret in each part of a URL that can carry one.
// Each is KEPT, and kept without the part: /v1/health publishes the list to
// any caller and every pairing QR carries it, and the phone uses none of it
// (it sends its own Authorization header and cancels every challenge but the
// server's certificate). Dropping the endpoint would cost the phone a route
// for no reason, and refusing the config would stop a bridge that started
// before. One warning per entry names it by position, scheme and host, and
// says it was published without them, never that it was dropped.
func TestACustomEndpointIsPublishedWithoutItsCredentials(t *testing.T) {
	in := make([]string, 0, len(credentialEndpointShapes)+2)
	want := make([]string, 0, len(credentialEndpointShapes)+1)
	for _, sh := range credentialEndpointShapes {
		in = append(in, sh.in)
		want = append(want, sh.published)
	}
	// A clean entry beside them, and the same endpoint again with a
	// password: the second is a duplicate once published without it.
	in = append(in, "https://f.example:7788", "https://user:"+credentialSecret+"@f.example:7788")
	want = append(want, "https://f.example:7788")

	kept, warns := ValidateCustomEndpoints(in)
	if strings.Join(kept, "\n") != strings.Join(want, "\n") {
		t.Errorf("kept:\n  got  %q\n  want %q", kept, want)
	}
	for _, k := range kept {
		if carriesSecret(k) {
			t.Errorf("a kept endpoint carries the secret: %s", k)
		}
	}
	if len(warns) != len(credentialEndpointShapes) {
		t.Errorf("%d warnings, want one per endpoint published without its credentials (%d): %v",
			len(warns), len(credentialEndpointShapes), warns)
	}
	for i, w := range warns {
		if !errors.Is(w, errCustomEndpointCredentials) {
			t.Errorf("warning %d is not the credentials one: %v", i, w)
		}
		if carriesSecret(w.Error()) {
			t.Errorf("warning %d carries the secret: %v", i, w)
		}
	}
	if len(warns) > 0 && !strings.Contains(warns[0].Error(), "customEndpoints[0] (https://a.example:7788)") {
		t.Errorf("the first warning does not name its entry by position, scheme and host: %v", warns[0])
	}

	// The load sequence: the config loads, the list is the published form,
	// and the journal says what happened under its own message.
	rec := loggingtest.Record(t)
	c := mkLoopbackConfig(t)
	c.CustomEndpoints = in
	if err := c.NormalizeAndValidate(); err != nil {
		t.Fatalf("NormalizeAndValidate: %v; a config that loaded before must still load", err)
	}
	if strings.Join(c.CustomEndpoints, "\n") != strings.Join(want, "\n") {
		t.Errorf("customEndpoints after Normalize:\n  got  %q\n  want %q", c.CustomEndpoints, want)
	}
	published := rec.Failures(customEndpointPublishedWithoutCredentials)
	if len(published) != len(credentialEndpointShapes) {
		t.Errorf("%d %q lines, want %d: %q", len(published), customEndpointPublishedWithoutCredentials,
			len(credentialEndpointShapes), rec.Failures())
	}
	if dropped := rec.Failures("dropped invalid custom endpoint"); len(dropped) != 0 {
		t.Errorf("an endpoint that was kept is reported dropped: %q", dropped)
	}
	for _, l := range rec.Failures() {
		if carriesSecret(l) {
			t.Errorf("a warning carries the secret: %s", l)
		}
	}
	// Idempotent, as Normalize promises: a second pass changes nothing and
	// says nothing more.
	before := len(rec.Failures())
	if err := c.NormalizeAndValidate(); err != nil {
		t.Fatal(err)
	}
	if strings.Join(c.CustomEndpoints, "\n") != strings.Join(want, "\n") || len(rec.Failures()) != before {
		t.Errorf("a second Normalize changed the list or warned again: %q, %q", c.CustomEndpoints, rec.Failures())
	}
}

// TestALoadedConfigPublishesItsEndpointsWithoutCredentials is the same rule
// through Load, from the file and from the environment: every consumer of
// the loaded config (/v1/health, the pairing QR, the console's endpoints
// panel, the certificate's names) reads the list Normalize leaves.
func TestALoadedConfigPublishesItsEndpointsWithoutCredentials(t *testing.T) {
	dir := t.TempDir()
	lib := filepath.Join(dir, "lib")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	var yaml strings.Builder
	yaml.WriteString("libraryRoots:\n  - " + yamlStr(lib) + "\ncustomEndpoints:\n")
	for _, sh := range credentialEndpointShapes {
		yaml.WriteString("  - " + yamlStr(sh.in) + "\n")
	}
	cfgPath := filepath.Join(dir, "bridge.yaml")
	if err := os.WriteFile(cfgPath, []byte(yaml.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v; a config that loaded before must still load", err)
	}
	for i, sh := range credentialEndpointShapes {
		if i >= len(cfg.CustomEndpoints) || cfg.CustomEndpoints[i] != sh.published {
			t.Errorf("%s: loaded endpoints %q, want %q at %d", sh.name, cfg.CustomEndpoints, sh.published, i)
		}
	}

	t.Setenv("BRIDGE_CUSTOM_ENDPOINTS", credentialEndpointShapes[0].in+","+credentialEndpointShapes[2].in)
	cfg, err = Load(cfgPath)
	if err != nil {
		t.Fatalf("Load with BRIDGE_CUSTOM_ENDPOINTS: %v", err)
	}
	want := credentialEndpointShapes[0].published + "\n" + credentialEndpointShapes[2].published
	if got := strings.Join(cfg.CustomEndpoints, "\n"); got != want {
		t.Errorf("endpoints from the environment = %q, want %q", got, want)
	}
}

// TestCheckCustomEndpointsRefusesATypedCredential pins the other half of the
// rule: a list the operator TYPES (the console's settings PATCH) is refused
// when an entry carries a user name, password, query or fragment, rather
// than stored as a URL they did not type. The refusal names the entry by
// position, scheme and host and never carries the secret. Entries that are
// invalid for another reason are left to the prune, as they always were.
func TestCheckCustomEndpointsRefusesATypedCredential(t *testing.T) {
	for i, sh := range credentialEndpointShapes {
		list := []string{"https://ok.example:7788", sh.in}
		err := CheckCustomEndpoints(list)
		if err == nil {
			t.Errorf("%s: %q accepted", sh.name, sh.in)
			continue
		}
		if !errors.Is(err, errCustomEndpointCredentials) {
			t.Errorf("%s: refusal %v is not the credentials one", sh.name, err)
		}
		if carriesSecret(err.Error()) {
			t.Errorf("%s: the refusal carries the secret: %v", sh.name, err)
		}
		if i == 0 && !strings.Contains(err.Error(), "customEndpoints[1] (https://a.example:7788)") {
			t.Errorf("%s: the refusal does not name its entry by position, scheme and host: %v", sh.name, err)
		}
	}
	for _, ok := range [][]string{
		nil,
		{"https://bridge.example:7788", "https://bridge.example:7788/", "  "},
		// Invalid for another reason: the prune drops them, with a warning.
		{"http://bridge.example:7788", "https://:8443", "not a url"},
	} {
		if err := CheckCustomEndpoints(ok); err != nil {
			t.Errorf("CheckCustomEndpoints(%q) = %v, want nil", ok, err)
		}
	}
}

// TestHasCredentialPartsReadsEveryPartThatCanCarryOne is the predicate the
// three sites share (the prune, the PATCH, `bridge init --domain`): true for
// a user name, a password, a query and a fragment, empty ones included,
// false for a URL with none and for a value that does not parse.
func TestHasCredentialPartsReadsEveryPartThatCanCarryOne(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"https://host:7788", false},
		{"https://host:7788/path/", false},
		{"https://[fe80::1%25en0]:7788", false},
		{"https://user@host", true},
		{"https://user:pw@host", true},
		{"https://:pw@host", true},
		{"https://@host", true},
		{"https://host?", true},
		{"https://host/?a=b", true},
		{"https://host/#frag", true},
		{"https://user:pw x@host", false}, // does not parse; the prune drops it
		{"", false},
	} {
		if got := HasCredentialParts(tc.in); got != tc.want {
			t.Errorf("HasCredentialParts(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
