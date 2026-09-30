package config

// autocert.domain is a host name alone: the publicly-routable name the phones
// dial. Public mode builds URLs from it as a string, `https://<domain>`, in
// /v1/health (which answers any caller) and in every pairing QR, and prints
// it in the startup banner, so a hand edit that left a user name and
// password in it (`user:password@host`) was published whole (backlog B66).
// Every other consumer wants the host too: the ACME host whitelist, the TLS
// SNI route and the console's Origin allowlist compare against it as one,
// and none of them could match such a value. A config that loaded before
// still loads: Normalize serves the value as the host it names, and a value
// that names none as InvalidAutocertDomain, with a warning either way.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// autocertHostCases is every shape a hand edit can leave in autocert.domain,
// beside the host AutocertHost reads from it ("" when it names none).
var autocertHostCases = []struct{ name, in, want string }{
	// A host name alone is kept as written.
	{"a host name", "bridge.example.test", "bridge.example.test"},
	{"case and the root dot kept", "Bridge.Example.Test.", "Bridge.Example.Test."},
	{"an IPv4 address", "192.0.2.10", "192.0.2.10"},
	// url.Parse reads 2001:db8::1 as the host 2001:db8: and the port 1, so
	// an address written without brackets must never reach it.
	{"an IPv6 address without brackets", "2001:db8::1", "2001:db8::1"},
	{"an IPv6 address in brackets", "[2001:db8::1]", "[2001:db8::1]"},
	{"an IPv6 address with a zone", "[fe80::1%25en0]", "[fe80::1%25en0]"},
	{"an internationalized name", "bücher.example", "bücher.example"},
	{"a Punycode name", "xn--bcher-kva.example", "xn--bcher-kva.example"},
	{"padding", "  bridge.example.test  ", "bridge.example.test"},

	// Every other part is removed.
	{"a password", "user:" + credentialSecret + "@bridge.example.test", "bridge.example.test"},
	{"a token as the user name", credentialSecret + "@bridge.example.test", "bridge.example.test"},
	{"an empty user name", "@bridge.example.test", "bridge.example.test"},
	{"a password with an @ in it", "user:" + credentialSecret + "@x@bridge.example.test", "bridge.example.test"},
	{"a scheme", "https://bridge.example.test", "bridge.example.test"},
	{"a scheme in capitals", "HTTPS://bridge.example.test", "bridge.example.test"},
	{"a scheme and a password", "https://user:" + credentialSecret + "@bridge.example.test", "bridge.example.test"},
	{"a port", "bridge.example.test:8443", "bridge.example.test"},
	{"an empty port", "bridge.example.test:", "bridge.example.test"},
	{"an IPv6 address with a port", "[2001:db8::1]:8443", "[2001:db8::1]"},
	{"a path", "bridge.example.test/" + credentialSecret, "bridge.example.test"},
	{"a root path", "bridge.example.test/", "bridge.example.test"},
	{"a query", "bridge.example.test?token=" + credentialSecret, "bridge.example.test"},
	{"a fragment", "bridge.example.test#" + credentialSecret, "bridge.example.test"},
	{"all of them", "https://user:" + credentialSecret + "@bridge.example.test:8443/" + credentialSecret +
		"?k=" + credentialSecret + "#" + credentialSecret, "bridge.example.test"},
	{"a token as the user name of an IPv6 address", credentialSecret + "@[2001:db8::1]:8443", "[2001:db8::1]"},
	// An address without brackets after user information, or before a
	// path, is kept as written too: url.Parse misreads it there as well.
	{"a password and an IPv6 address without brackets", "user:" + credentialSecret + "@2001:db8::1", "2001:db8::1"},
	{"a scheme, an IPv6 address without brackets and a path", "https://2001:db8::1/" + credentialSecret, "2001:db8::1"},
	{"a password and an IPv4 address", "user:" + credentialSecret + "@192.0.2.10", "192.0.2.10"},

	// A value that names no host that can be read.
	{"a password and no host", "user:" + credentialSecret + "@", ""},
	{"a port and no host", ":8443", ""},
	{"a user name and a password alone", "user:" + credentialSecret, ""},
	// url.Parse refuses a space in the user information.
	{"a password that does not parse", "user:" + credentialSecret + " x@bridge.example.test", ""},
	{"a host that does not parse", "bridge example.test", ""},
	// An "@" after the first "/", "?" or "#" could as well end user
	// information a hand edit left unescaped: url.Parse reads the first as
	// the host "user" with the port 12, and the last three as a host that
	// is followed by more "@". Nothing that precedes an "@" is served.
	{"an @ in the path", "user:12/34@bridge.example.test", ""},
	{"an @ in the query", "bridge.example.test/?next=a@b", ""},
	{"a password with a slash in it", "user:" + credentialSecret + "/x@bridge.example.test", ""},
	{"a password with a # in it", "user:" + credentialSecret + "#x@bridge.example.test", ""},
	{"a scheme and nothing else", "https://", ""},
	{"blank", "   ", ""},
}

// TestAutocertHostReadsTheHostAValueNames is the reduction Normalize serves
// the field as and `bridge init --domain` refuses a value by: the host a
// value names, without a scheme, user information, a port, a path, a query
// or a fragment, or "" when it names none that can be read. What it returns
// is a fixed point, so Normalize stays idempotent and init takes back what
// Normalize stores, and it never carries text that preceded an "@".
func TestAutocertHostReadsTheHostAValueNames(t *testing.T) {
	for _, tc := range autocertHostCases {
		t.Run(tc.name, func(t *testing.T) {
			got := AutocertHost(tc.in)
			if got != tc.want {
				t.Errorf("AutocertHost(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if carriesSecret(got) {
				t.Errorf("AutocertHost(%q) = %q carries the secret", tc.in, got)
			}
			if again := AutocertHost(got); got != "" && again != got {
				t.Errorf("AutocertHost is not a fixed point: %q, then %q", got, again)
			}
		})
	}
	// The placeholder for a value that names none is itself a host it keeps.
	if got := AutocertHost(InvalidAutocertDomain); got != InvalidAutocertDomain {
		t.Errorf("AutocertHost(%q) = %q; the placeholder must be a host it keeps", InvalidAutocertDomain, got)
	}
}

// mkPublicConfig is a minimal valid public-mode Config serving domain.
func mkPublicConfig(t *testing.T, domain string) *Config {
	t.Helper()
	return &Config{
		LibraryRoots:    []string{t.TempDir()},
		ListenAddress:   ":7788",
		AdminAddress:    "0.0.0.0:7789",
		ScanIntervalSec: 3600,
		Deployment:      DeploymentConfig{Mode: "public", AdminTLSTerminatedByProxy: true},
		Autocert:        AutocertConfig{Domain: domain},
	}
}

// TestNormalizeServesAutocertDomainAsItsHost drives the load sequence
// (NormalizeAndValidate, which Load and every writer run) over a public
// config holding each shape. The config still loads (a refusal would take a
// bridge down on update, and its phones may reach it through
// customEndpoints), the field is the host it names, or InvalidAutocertDomain
// when it names none, and one warning says which, naming the field, and its
// scheme and host where it has them, never the value. A second pass changes
// nothing and says nothing more.
func TestNormalizeServesAutocertDomainAsItsHost(t *testing.T) {
	for _, tc := range autocertHostCases {
		if strings.TrimSpace(tc.in) == "" {
			continue // an empty domain is refused in public mode, as it always was
		}
		t.Run(tc.name, func(t *testing.T) {
			rec := loggingtest.Record(t)
			c := mkPublicConfig(t, tc.in)
			if err := c.NormalizeAndValidate(); err != nil {
				t.Fatalf("NormalizeAndValidate: %v; a config that loaded before must still load", err)
			}
			want := tc.want
			if want == "" {
				want = InvalidAutocertDomain
			}
			if c.Autocert.Domain != want {
				t.Errorf("autocert.domain = %q, want %q", c.Autocert.Domain, want)
			}
			reduced := rec.Failures(autocertDomainServedAsItsHost)
			noHost := rec.Failures(autocertDomainNamesNoHost)
			switch {
			case tc.want == "":
				if len(noHost) != 1 || len(reduced) != 0 {
					t.Errorf("want one %q line and no other: %q", autocertDomainNamesNoHost, rec.Failures())
				}
			case tc.want != strings.TrimSpace(tc.in):
				origin := urlOriginForLog("https://" + hostForURL(want))
				if origin == "" {
					t.Fatalf("no scheme and host can be read from https://%s; the warning could name only the field", hostForURL(want))
				}
				if len(reduced) != 1 || len(noHost) != 0 {
					t.Errorf("want one %q line and no other: %q", autocertDomainServedAsItsHost, rec.Failures())
				} else if field := "autocert.domain (" + origin + ")"; !strings.Contains(reduced[0], field) {
					t.Errorf("the warning does not name the field by scheme and host as %q: %s", field, reduced[0])
				}
			default:
				if len(reduced)+len(noHost) != 0 {
					t.Errorf("a host name alone was warned about: %q", rec.Failures())
				}
			}
			for _, l := range rec.All() {
				if carriesSecret(l) {
					t.Errorf("a log line carries the secret: %s", l)
				}
			}
			before := len(rec.All())
			if err := c.NormalizeAndValidate(); err != nil {
				t.Fatal(err)
			}
			if c.Autocert.Domain != want || len(rec.All()) != before {
				t.Errorf("a second Normalize changed the domain or logged again: %q, %q", c.Autocert.Domain, rec.All())
			}
		})
	}
}

// TestAutocertDomainIsReducedWhereItIsUsed: the field is read in public mode
// and by the ACME manager wherever autocert is enabled, which Validate
// checks only in public mode, and serve starts it in either posture and
// prints the domain as it does. A loopback config with autocert off never
// reads it, so a value there is left as written and not warned about.
func TestAutocertDomainIsReducedWhereItIsUsed(t *testing.T) {
	const handEdited = "user:" + credentialSecret + "@bridge.example.test"

	rec := loggingtest.Record(t)
	c := mkLoopbackConfig(t)
	c.Autocert.Domain = handEdited
	if err := c.NormalizeAndValidate(); err != nil {
		t.Fatal(err)
	}
	if c.Autocert.Domain != handEdited || len(rec.All()) != 0 {
		t.Errorf("a loopback config with autocert off: domain %q, lines %q; want it untouched and quiet",
			c.Autocert.Domain, rec.All())
	}

	c = mkLoopbackConfig(t)
	c.Autocert = AutocertConfig{Enabled: true, Domain: handEdited, Email: "ops@example.test"}
	if err := c.NormalizeAndValidate(); err != nil {
		t.Fatal(err)
	}
	if c.Autocert.Domain != "bridge.example.test" {
		t.Errorf("a loopback config with autocert on: domain %q, want the host it names", c.Autocert.Domain)
	}
}

// TestALoadedPublicConfigServesItsAutocertDomainAsItsHost is the same rule
// through Load, from a file an operator edited by hand: it loads, and every
// consumer of the loaded config (/v1/health, the pairing QR, the banner, the
// ACME manager, the SNI route, the Origin allowlist) reads the host.
func TestALoadedPublicConfigServesItsAutocertDomainAsItsHost(t *testing.T) {
	dir := t.TempDir()
	lib := filepath.Join(dir, "lib")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ in, want string }{
		{"user:" + credentialSecret + "@bridge.example.test", "bridge.example.test"},
		{"user:" + credentialSecret + "@", InvalidAutocertDomain},
	} {
		cfgPath := filepath.Join(dir, "bridge.yaml")
		yaml := "libraryRoots:\n  - " + yamlStr(lib) + "\n" +
			"listenAddress: \"0.0.0.0:7788\"\nadminAddress: \"0.0.0.0:7789\"\n" +
			"deployment:\n  mode: public\n  adminTLSTerminatedByProxy: true\n" +
			"autocert:\n  domain: " + yamlStr(tc.in) + "\n"
		if err := os.WriteFile(cfgPath, []byte(yaml), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(cfgPath)
		if err != nil {
			t.Fatalf("Load over autocert.domain %q: %v; a config that loaded before must still load", tc.in, err)
		}
		if cfg.Autocert.Domain != tc.want {
			t.Errorf("loaded autocert.domain = %q, want %q", cfg.Autocert.Domain, tc.want)
		}
	}
}
