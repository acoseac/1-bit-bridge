package enrich

import "testing"

// TestHostAllowed_AnchorsAtLabelBoundary pins the SSRF-hardening contract:
// a leading-dot allowlist entry matches the apex and any proper subdomain,
// but not look-alike hosts that share a suffix without a label boundary.
// The bare-HasSuffix path the function used before PR #13 review caught
// "attackerdeezer.com" against ".deezer.com" because ".deezer.com" was
// itself a suffix — the apex-only match is the fix.
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
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := hostAllowed(c.host, c.allowed); got != c.want {
				t.Errorf("hostAllowed(%q, %v) = %v, want %v", c.host, c.allowed, got, c.want)
			}
		})
	}
}
