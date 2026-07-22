package updater

import (
	"net/url"
	"strings"
	"testing"
)

// allowTestAssetHost points the validateAssetURL seam at a validator that
// accepts ONLY the given test server's host, restoring the production
// validator on cleanup.
//
// The install-flow fixtures drive a plain-http httptest.Server. Before the
// pre-request check those URLs were reachable for free, because
// CheckRedirect only fires on a 3xx hop. Rather than stub the seam to a
// blanket no-op, each fixture narrows it to its own origin — so a test
// that accidentally reaches for any other host still fails, and the only
// property given up is the scheme/allowlist pair for one known-local
// server. The real policy is covered directly by TestValidateUpdaterAssetURL.
func allowTestAssetHost(t *testing.T, serverURL string) {
	t.Helper()
	u, err := url.Parse(serverURL)
	if err != nil {
		t.Fatalf("parse test server URL %q: %v", serverURL, err)
	}
	host := u.Host // host:port — the fixture's exact origin
	prev := validateAssetURL
	validateAssetURL = func(raw string) error {
		p, err := url.Parse(raw)
		if err != nil {
			return err
		}
		if p.Host == host {
			return nil
		}
		return prev(raw)
	}
	t.Cleanup(func() { validateAssetURL = prev })
}

// TestValidateUpdaterAssetURL pins the pre-request policy for updater
// asset URLs.
//
// installUpdaterRedirectGuard covers only redirect hops — net/http calls
// CheckRedirect when it is about to FOLLOW a 3xx, never for the initial
// request. Every asset URL comes out of the GitHub API JSON, so without
// this check a fully-controlled API response could name
// http://attacker/payload directly and have it fetched unchecked. That
// matters more here than anywhere else in the tree: this is the one path
// that downloads and then executes a binary, and verify_other.go is a
// no-op on Linux and Windows.
//
// The host cases mirror updaterHostAllowed's dot-boundary contract —
// "evil-github.com" and "notgithub.com" must not pass as "github.com".
func TestValidateUpdaterAssetURL(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantErr bool
	}{
		{"github release asset", "https://github.com/acoseac/1-bit-bridge/releases/download/v1/a.tar.gz", false},
		{"codeload subdomain", "https://codeload.githubusercontent.com/x/y", false},
		{"objects cdn subdomain", "https://objects.githubusercontent.com/asset", false},
		{"exact allowlisted host", "https://githubusercontent.com/a", false},

		{"plaintext http", "http://github.com/asset.tar.gz", true},
		{"non-http scheme", "file:///etc/passwd", true},
		{"scheme-less", "github.com/asset.tar.gz", true},
		{"foreign host", "https://attacker.example/payload.tar.gz", true},
		{"prefix-collision host", "https://evil-github.com/payload", true},
		{"suffix-collision host", "https://notgithub.com/payload", true},
		{"embedded-allowlist host", "https://github.com.attacker.example/payload", true},
		{"userinfo spoof", "https://github.com@attacker.example/payload", true},
		{"empty", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateUpdaterAssetURL(tc.raw)
			if tc.wantErr && err == nil {
				t.Errorf("ValidateUpdaterAssetURL(%q) = nil, want an error", tc.raw)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("ValidateUpdaterAssetURL(%q) = %v, want nil", tc.raw, err)
			}
		})
	}
}

// TestDownloadVerifiedRejectsHostileURLsBeforeRequesting pins that the
// check happens BEFORE any network call — a validator that ran after the
// fetch would already have leaked the request (and, for a checksum URL,
// accepted an attacker-chosen expected hash).
func TestDownloadVerifiedRejectsHostileURLsBeforeRequesting(t *testing.T) {
	const good = "https://github.com/acoseac/1-bit-bridge/releases/download/v1/a.tar.gz"
	cases := []struct {
		name      string
		archive   string
		checksums string
	}{
		{"hostile archive URL", "http://attacker.example/payload.tar.gz", good},
		{"hostile checksums URL", good, "http://attacker.example/checksums.txt"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A nil *http.Client would panic on any actual Do — so
			// reaching the network at all fails loudly rather than
			// silently passing.
			_, err := downloadVerified(t.Context(), nil,
				tc.archive, "a.tar.gz", tc.checksums,
				t.TempDir()+"/out.tar.gz")
			if err == nil {
				t.Fatal("downloadVerified accepted a hostile URL")
			}
			if !strings.Contains(err.Error(), "refusing") {
				t.Errorf("error %v does not look like the policy refusal", err)
			}
		})
	}
}
