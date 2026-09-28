package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestInitRefusesADomainCarryingACredential: `bridge init --public --domain`
// writes the endpoint every phone dials, `https://<domain>`, into
// customEndpoints, and /v1/health (answering any caller) and every pairing
// QR publish that list (backlog B54). A domain carrying a user name, a
// password, a query or a fragment is refused before anything is written,
// exit 2, as any other bad flag is, and the refusal does not echo it. A
// config that already holds such an endpoint is published without the part
// instead (config.ValidateCustomEndpoints): the operator typed this one.
func TestInitRefusesADomainCarryingACredential(t *testing.T) {
	const secret = "s3cret-Pw"
	for _, tc := range []struct{ name, domain string }{
		{"a password", "user:" + secret + "@bridge.example.test"},
		{"a token as the user name", secret + "@bridge.example.test"},
		{"a query", "bridge.example.test?token=" + secret},
		{"a fragment", "bridge.example.test#" + secret},
		// url.Parse refuses a space in the userinfo, so HasCredentialParts
		// answers false for it: the value is refused for not parsing.
		{"a password that does not parse", "user:" + secret + " x@bridge.example.test"},
		{"a password behind a space", " user:" + secret + "@bridge.example.test"},
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
}
