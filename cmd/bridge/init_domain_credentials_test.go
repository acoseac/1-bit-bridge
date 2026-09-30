package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestInitRefusesADomainCarryingACredential: `bridge init --public --domain`
// writes the endpoint every phone dials, `https://<domain>`, into
// customEndpoints, and /v1/health (answering any caller) and every pairing
// QR publish that list (backlog B54). A domain carrying a user name, a
// password, a query or a fragment is refused before anything is written,
// exit 2, as any other bad flag is, and the refusal does not echo it. So is
// one that does not parse (a password with a space in it). And since backlog
// B66 so is any domain that is not a host name alone (a path, a scheme, a
// port, no host at all): Normalize serves a stored one as the host it names
// (config.AutocertHost), and init refuses what the operator TYPED rather
// than write a value they did not.
func TestInitRefusesADomainCarryingACredential(t *testing.T) {
	const secret = "s3cret-Pw"
	for _, tc := range []struct{ name, domain string }{
		{"a password", "user:" + secret + "@bridge.example.test"},
		{"a token as the user name", secret + "@bridge.example.test"},
		{"a query", "bridge.example.test?token=" + secret},
		{"a fragment", "bridge.example.test#" + secret},
		// url.Parse refuses a space in the userinfo, so no host can be read
		// from it (config.AutocertHost answers ""), and it is refused.
		{"a password that does not parse", "user:" + secret + " x@bridge.example.test"},
		{"a password behind a space", " user:" + secret + "@bridge.example.test"},
		{"a path", "bridge.example.test/" + secret},
		{"a scheme", "https://bridge.example.test"},
		{"a port", "bridge.example.test:8443"},
		{"no host", "user:" + secret + "@"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfgDir := filepath.Join(t.TempDir(), "cfg")
			var out, errOut bytes.Buffer
			code := initCmd([]string{
				"--yes", "--no-service", "--skip-doctor",
				"--dir", cfgDir, "--library", testLibrary(t),
				"--public", "--domain", tc.domain, "--admin-tls-proxy",
			}, strings.NewReader(""), &out, &errOut)
			all := stripANSI(out.String() + errOut.String())
			defer logRunOnFailure(t, all)
			if code != 2 {
				t.Errorf("init exited %d for --domain %q, want 2", code, tc.domain)
			}
			if !strings.Contains(all, "--domain") {
				t.Errorf("the refusal does not name --domain")
			}
			if strings.Contains(strings.ToLower(all), strings.ToLower(secret)) {
				t.Errorf("the refusal echoes the secret")
			}
			if _, err := os.Stat(cfgDir); !os.IsNotExist(err) {
				t.Errorf("the refused init made its config dir (stat: %v)", err)
			}
		})
	}

	// The controls: a plain domain is taken, and so is one padded with
	// whitespace, as it was before the check, which reads the domain
	// trimmed (Normalize trims the autocert host the same way). Untrimmed,
	// the padded one does not parse and would be refused.
	for _, domain := range []string{"bridge.example.test", " bridge.example.test "} {
		t.Run("accepts "+strings.TrimSpace(domain)+" as given "+strconv.Quote(domain), func(t *testing.T) {
			cfgDir := filepath.Join(t.TempDir(), "cfg")
			var out, errOut bytes.Buffer
			code := initCmd([]string{
				"--yes", "--no-service", "--skip-doctor",
				"--dir", cfgDir, "--library", testLibrary(t),
				"--public", "--domain", domain, "--admin-tls-proxy",
			}, strings.NewReader(""), &out, &errOut)
			if code != 0 {
				t.Errorf("init exited %d for --domain %q, want 0:\n%s", code, domain, stripANSI(out.String()+errOut.String()))
			}
			if _, err := os.Stat(filepath.Join(cfgDir, "bridge.yaml")); err != nil {
				t.Errorf("init wrote no config for --domain %q: %v", domain, err)
			}
		})
	}
}
