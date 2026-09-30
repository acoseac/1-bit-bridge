package baseurl

import "testing"

// TestTheReductions pins the three answers for each shape a base URL
// reaches the bridge in: the configured pin, the base a paired app sends
// with a credential, and the base the harvest state file holds. canonical is
// CanonicalHTTPS's answer (the pin's reduction), credential is
// CredentialBase's (what the endpoint stores and the store holds), and every
// non-empty answer must be a fixed point of both: the store reduces what it
// loads, and a base that changed on every load would read as a new Atlas and
// reset the sync position at each re-provision.
func TestTheReductions(t *testing.T) {
	for _, tc := range []struct {
		name, in, canonical, credential string
		namesHost                       bool
	}{
		{"empty", "", "", "", false},
		{"blank", "   ", "", "", false},
		{"plain https", "https://atlas.example", "https://atlas.example", "https://atlas.example", true},
		{"trailing slash", "https://atlas.example/", "https://atlas.example", "https://atlas.example", true},
		{"the default port", "https://atlas.example:443", "https://atlas.example", "https://atlas.example", true},
		{"the default port and a slash", "https://atlas.example:443/", "https://atlas.example", "https://atlas.example", true},
		{"another port", "https://atlas.example:8443", "https://atlas.example:8443", "https://atlas.example:8443", true},
		{"an uppercase scheme", "HTTPS://atlas.example", "https://atlas.example", "https://atlas.example", true},
		{"surrounding space", " https://atlas.example ", "https://atlas.example", "https://atlas.example", true},
		{"an IPv6 literal", "https://[::1]:8443", "https://[::1]:8443", "https://[::1]:8443", true},
		{"an IPv6 literal on the default port", "https://[::1]:443", "https://[::1]", "https://[::1]", true},
		{"an empty query", "https://atlas.example?", "https://atlas.example", "https://atlas.example", true},
		{"an empty fragment", "https://atlas.example#", "https://atlas.example", "https://atlas.example", true},
		// A port and no host keeps its canonical form, so a pin written that
		// way pins to a value no credential can carry (backlog B36), and
		// CredentialBase refuses it: Go dials it on this machine.
		{"a port and no host", "https://:8443", "https://:8443", "", false},
		// And the default port is not stripped from it: stripped, it was
		// `https://`, which reduces to "", unpinned.
		{"the default port and no host", "https://:443", "https://:443", "", false},
		{"no host at all", "https://", "", "", false},
		{"a token as the user name", "https://tok@atlas.example", "", "", true},
		{"a user name and a password", "https://user:pw@atlas.example", "", "", true},
		{"a password alone", "https://:pw@atlas.example", "", "", true},
		{"an empty user", "https://@atlas.example", "", "", true},
		{"a path", "https://atlas.example/v1", "", "", true},
		{"a query", "https://atlas.example?key=x", "", "", true},
		{"a fragment", "https://atlas.example#x", "", "", true},
		{"plain http", "http://atlas.example", "", "", true},
		{"no scheme", "atlas.example", "", "", false},
		{"a user name read as the scheme", "user:pw@atlas.example", "", "", false},
		{"unparseable", "://nope", "", "", false},
		{"a port that is not a number", "https://atlas.example:443:443", "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := CanonicalHTTPS(tc.in); got != tc.canonical {
				t.Errorf("CanonicalHTTPS(%q) = %q, want %q", tc.in, got, tc.canonical)
			}
			if got := CredentialBase(tc.in); got != tc.credential {
				t.Errorf("CredentialBase(%q) = %q, want %q", tc.in, got, tc.credential)
			}
			if got := NamesHost(tc.in); got != tc.namesHost {
				t.Errorf("NamesHost(%q) = %v, want %v", tc.in, got, tc.namesHost)
			}
			if tc.canonical != "" {
				if again := CanonicalHTTPS(tc.canonical); again != tc.canonical {
					t.Errorf("CanonicalHTTPS is not a fixed point on %q: %q", tc.canonical, again)
				}
			}
			if tc.credential != "" {
				if again := CredentialBase(tc.credential); again != tc.credential {
					t.Errorf("CredentialBase is not a fixed point on %q: %q", tc.credential, again)
				}
			}
		})
	}
}
