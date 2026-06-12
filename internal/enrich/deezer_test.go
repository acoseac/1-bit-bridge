package enrich

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestHostAllowed_AnchorsAtLabelBoundary pins the SSRF-hardening contract:
// a leading-dot allowlist entry behaves as a label-boundary-anchored
// suffix match — it matches the apex (".deezer.com" → "deezer.com") and
// any proper subdomain ("cdn.deezer.com", "foo.bar.deezer.com"), but
// never a look-alike that shares a suffix without a DNS label boundary
// ("attackerdeezer.com"). Dot-less entries require an exact match; this
// protects allowlists that name an IP literal like "127.0.0.1" from
// admitting look-alikes ("evil.127.0.0.1", "127.0.0.100").
func TestHostAllowed_AnchorsAtLabelBoundary(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		host    string
		allowed []string
		want    bool
	}{
		// .deezer.com matches the apex and subdomains.
		{"apex under dot rule", "deezer.com", []string{".deezer.com"}, true},
		{"subdomain under dot rule", "cdn.deezer.com", []string{".deezer.com"}, true},
		{"nested subdomain", "foo.bar.deezer.com", []string{".deezer.com"}, true},

		// Anchor at the dot — look-alikes must not match.
		{"look-alike apex", "attackerdeezer.com", []string{".deezer.com"}, false},
		{"look-alike subdomain", "evil.attackerdeezer.com", []string{".deezer.com"}, false},

		// Dot-less entries require exact match.
		{"exact dot-less allowlist", "127.0.0.1", []string{"127.0.0.1"}, true},
		{"look-alike under dot-less entry", "evil.127.0.0.1", []string{"127.0.0.1"}, false},
		{"prefix look-alike under dot-less entry", "127.0.0.100", []string{"127.0.0.1"}, false},

		// Case-folding.
		{"uppercased host matches lowercase rule", "DEEZER.COM", []string{".deezer.com"}, true},
		{"uppercased rule matches lowercase host", "deezer.com", []string{".DEEZER.COM"}, true},

		// Multi-entry allowlist.
		{"matches second entry", "cdn.dzcdn.net", []string{".deezer.com", ".dzcdn.net"}, true},
		{"matches neither", "example.org", []string{".deezer.com", ".dzcdn.net"}, false},

		// Empty allowlist rejects everything.
		{"empty allowlist", "deezer.com", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := hostAllowed(c.host, c.allowed); got != c.want {
				t.Errorf("hostAllowed(%q, %v) = %v, want %v", c.host, c.allowed, got, c.want)
			}
		})
	}
}

// TestFetchImage_RefusesRedirectToDisallowedHost pins the redirect-guard
// half of the SSRF contract. The initial hostAllowed check only covers the
// first URL; without a CheckRedirect gate a compromised/misconfigured CDN
// could 30x-redirect to 169.254.169.254 or an RFC1918 address and we'd
// follow. The guard has to reject every hop whose host is outside the
// allowlist — not the first one only.
func TestFetchImage_RefusesRedirectToDisallowedHost(t *testing.T) {
	t.Parallel()
	// Attacker-controlled server: responds 302 → 169.254.169.254, a canonical
	// cloud-metadata target. We never want the client to actually dial that.
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
	}))
	defer attacker.Close()

	c := NewDeezerClient("", "test-agent", &http.Client{Timeout: 2 * time.Second})
	// httptest servers live on 127.0.0.1:<ephemeral>; the allowlist is
	// matched against Hostname() which strips the port, so the bare IP
	// literal is what we whitelist for first-hop to reach httptest.
	c.SetAllowedImageHostsForTest([]string{"127.0.0.1"})

	_, err := c.FetchImage(context.Background(), attacker.URL+"/picture.jpg")
	if err == nil {
		t.Fatalf("FetchImage followed redirect to disallowed host — expected error")
	}
	// The redirect target is 169.254.169.254 — not in the allowlist —
	// so the CheckRedirect gate must fire. The http.Client wraps
	// CheckRedirect errors in url.Error, so the substring is what we
	// can portably assert on.
	if !strings.Contains(err.Error(), "refusing redirect") {
		t.Fatalf("unexpected error %q; want 'refusing redirect' substring", err.Error())
	}
}

// TestFetchImage_FollowsRedirectWithinAllowlist confirms the guard still
// lets legitimate redirects through — a common Deezer pattern is 302 from
// the search-returned URL to a geo-routed CDN host under the same apex.
func TestFetchImage_FollowsRedirectWithinAllowlist(t *testing.T) {
	t.Parallel()
	// Final server returns a tiny payload.
	final := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer final.Close()
	// Hop server 302s to `final`.
	hop := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, final.URL+"/img.jpg", http.StatusFound)
	}))
	defer hop.Close()

	c := NewDeezerClient("", "test-agent", &http.Client{Timeout: 2 * time.Second})
	// Both httptest servers bind 127.0.0.1 with different ephemeral
	// ports; Hostname() strips the port, so a single IP allowlist entry
	// covers the full hop chain.
	c.SetAllowedImageHostsForTest([]string{"127.0.0.1"})

	buf, err := c.FetchImage(context.Background(), hop.URL+"/picture.jpg")
	if err != nil {
		t.Fatalf("FetchImage: %v", err)
	}
	if string(buf) != "ok" {
		t.Fatalf("unexpected body %q", string(buf))
	}
}

// TestFetchImage_RejectsEmptyBody pins the zero-byte cache-poisoning
// guard: a 200 OK with an empty body must surface as errNotFound rather
// than an empty buffer. Without the guard the caller would write a
// 0-byte file to the artwork cache, and because existence is checked via
// os.Stat (size-blind) it would become a permanent broken cache hit the
// enricher never retries.
func TestFetchImage_RejectsEmptyBody(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK) // 200 with no body
	}))
	defer srv.Close()

	c := NewDeezerClient("", "test-agent", &http.Client{Timeout: 2 * time.Second})
	c.SetAllowedImageHostsForTest([]string{"127.0.0.1"})

	buf, err := c.FetchImage(context.Background(), srv.URL+"/picture.jpg")
	if buf != nil {
		t.Fatalf("expected nil buffer on empty body, got %d bytes", len(buf))
	}
	if !errors.Is(err, errNotFound) {
		t.Fatalf("expected errNotFound on empty body, got %v", err)
	}
}

// TestNewDeezerClient_DoesNotMutateSharedHTTPClient pins the shallow-copy
// contract. *http.Client instances are designed to be shared across
// services and goroutines; without the copy, installRedirectGuard would
// overwrite CheckRedirect on the caller's pointer, retroactively
// constraining redirects in unrelated code paths (a future caller passing
// http.DefaultClient or a MusicBrainzClient-shared client).
func TestNewDeezerClient_DoesNotMutateSharedHTTPClient(t *testing.T) {
	t.Parallel()

	// Shared client with no CheckRedirect installed. After
	// NewDeezerClient returns, the original pointer's CheckRedirect must
	// still be nil, and a second consumer of the same shared pointer
	// must see Go's default redirect behaviour (not Deezer's allowlist).
	shared := &http.Client{Timeout: 2 * time.Second}
	if shared.CheckRedirect != nil {
		t.Fatalf("precondition: shared.CheckRedirect should be nil before NewDeezerClient")
	}

	_ = NewDeezerClient("", "test-agent", shared)

	if shared.CheckRedirect != nil {
		t.Fatalf("NewDeezerClient mutated the caller's *http.Client.CheckRedirect; the deezer guard leaked into the shared client")
	}

	// Drive a real redirect through the shared client to a host the
	// Deezer allowlist would reject. Default policy follows up to 10
	// redirects; if Deezer's guard had leaked onto this client, this
	// would error with "refusing redirect" instead.
	final := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer final.Close()
	hop := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, final.URL, http.StatusFound)
	}))
	defer hop.Close()

	resp, err := shared.Get(hop.URL)
	if err != nil {
		t.Fatalf("shared client follow-redirect: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected status %d on shared client redirect chain", resp.StatusCode)
	}
}
