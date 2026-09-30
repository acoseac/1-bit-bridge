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
		{"an empty port", "https://atlas.example:", "https://atlas.example", "https://atlas.example", true},
		{"an empty port on an IPv6 literal", "https://[::1]:", "https://[::1]", "https://[::1]", true},
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
		{"an empty port and no host", "https://:", "https://:", "", false},
		{"no host at all", "https://", "", "", false},
		// url.Parse checks that a port is digits, not that it is one a
		// connection can be made to. Such a pin keeps its canonical form, as a
		// port and no host does, so a config that loaded keeps loading; no
		// credential is held against it, since every dial refuses it.
		{"a port past 65535", "https://atlas.example:99999", "https://atlas.example:99999", "", true},
		{"port zero", "https://atlas.example:0", "https://atlas.example:0", "", true},
		{"a port no integer holds", "https://atlas.example:99999999999999999999", "https://atlas.example:99999999999999999999", "", true},
		{"a port with leading zeros", "https://atlas.example:08443", "https://atlas.example:08443", "https://atlas.example:08443", true},
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
			mustReduce(t, "CanonicalHTTPS", CanonicalHTTPS, tc.in, tc.canonical)
			mustReduce(t, "CredentialBase", CredentialBase, tc.in, tc.credential)
			if got := NamesHost(tc.in); got != tc.namesHost {
				t.Errorf("NamesHost(%q) = %v, want %v", tc.in, got, tc.namesHost)
			}
		})
	}
}

// mustReduce fails t unless reduce answers want for in, and, when want is not
// "", answers want again for want: the fixed point TestTheReductions pins.
func mustReduce(t *testing.T, name string, reduce func(string) string, in, want string) {
	t.Helper()
	if got := reduce(in); got != want {
		t.Errorf("%s(%q) = %q, want %q", name, in, got, want)
	}
	if want == "" {
		return
	}
	if again := reduce(want); again != want {
		t.Errorf("%s is not a fixed point on %q: %q", name, want, again)
	}
}
