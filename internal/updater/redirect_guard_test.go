package updater

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// F31 (2026-07-20 review): neither updater HTTP client set CheckRedirect,
// while asset URLs come from the GitHub API JSON — so a fully-controlled
// API response could steer the download, AND the checksum operand it is
// compared against, to an arbitrary host including plaintext http://.
// internal/enrich/deezer.go already had a host-pinning guard; this is the
// one code path that downloads and then EXECUTES a binary, and on
// Linux/Windows nothing verifies the signature behind it.

func TestUpdaterHostAllowed(t *testing.T) {
	allowed := []string{
		"github.com",
		"api.github.com",
		"objects.githubusercontent.com",
		"release-assets.githubusercontent.com",
		"GitHub.com",  // case-insensitive
		"github.com.", // trailing root dot
	}
	for _, h := range allowed {
		if !updaterHostAllowed(h) {
			t.Errorf("updaterHostAllowed(%q) = false, want true", h)
		}
	}

	// The suffix match must be dot-anchored: a bare HasSuffix would let
	// every one of these through.
	denied := []string{
		"evil-github.com",
		"notgithub.com",
		"github.com.evil.tld",
		"githubusercontent.com.attacker.net",
		"attacker.io",
		"",
		"localhost",
		"127.0.0.1",
	}
	for _, h := range denied {
		if updaterHostAllowed(h) {
			t.Errorf("updaterHostAllowed(%q) = true, want false", h)
		}
	}
}

// TestRedirectGuardRefusesOffAllowlistHop pins that a redirect to a
// non-allowlisted host aborts with an error rather than silently following.
func TestRedirectGuardRefusesOffAllowlistHop(t *testing.T) {
	final := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("trojan"))
	}))
	defer final.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, final.URL, http.StatusFound)
	}))
	defer redirector.Close()

	hc := &http.Client{}
	installUpdaterRedirectGuard(hc)

	resp, err := hc.Get(redirector.URL)
	if err == nil {
		resp.Body.Close()
		t.Fatal("redirect to a non-allowlisted host was followed; want an error")
	}
	if !strings.Contains(err.Error(), "non-allowlisted host") &&
		!strings.Contains(err.Error(), "non-https") {
		t.Fatalf("unexpected error (want the guard's message): %v", err)
	}
}

// TestRedirectGuardRefusesPlaintextDowngrade pins the scheme half: an
// https->http hop must abort even if the host itself were allowlisted.
func TestRedirectGuardRefusesPlaintextDowngrade(t *testing.T) {
	hc := &http.Client{}
	installUpdaterRedirectGuard(hc)

	req, err := http.NewRequest("GET", "http://github.com/acoseac/1-bit-bridge", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate the redirect callback directly: an allowlisted HOST but a
	// plaintext scheme must still be refused.
	if err := hc.CheckRedirect(req, nil); err == nil {
		t.Fatal("http:// hop to an allowlisted host was permitted; want an error")
	}
}

// TestNewClientInstallsGuardOnBothClients pins that the poll client AND
// the timeout-free download client are both covered — the download client
// is the one that actually fetches the executable.
func TestNewClientInstallsGuardOnBothClients(t *testing.T) {
	c := NewClient("acoseac/1-bit-bridge", 0)
	if c.http == nil || c.http.CheckRedirect == nil {
		t.Error("poll client has no CheckRedirect guard")
	}
	if c.download == nil || c.download.CheckRedirect == nil {
		t.Error("download client has no CheckRedirect guard")
	}
	// And the download client must still be timeout-free (#374): a
	// multi-MiB archive over a slow link must not be killed mid-body.
	if c.download.Timeout != 0 {
		t.Errorf("download client Timeout = %v, want 0 (see #374)", c.download.Timeout)
	}
}
