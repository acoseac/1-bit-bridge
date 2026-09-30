package config

import "testing"

// atlas.harvestBaseUrl pins which Atlas host the credential endpoint accepts.
// A malformed value must fail at LOAD, not reduce silently to "" — an empty
// canonical form means "unpinned", which is exactly the state the field exists
// to prevent, and a typo would otherwise leave a demo bridge open while its
// config looks configured.
func TestAtlasHarvestBaseURLValidation(t *testing.T) {
	for _, tc := range []struct {
		name, in, wantCanonical string
		wantErr                 bool
	}{
		{"unset is unpinned", "", "", false},
		{"plain https", "https://atlas.example", "https://atlas.example", false},
		{"trailing slash canonicalises", "https://atlas.example/", "https://atlas.example", false},
		{"host and port", "https://atlas.example:8443", "https://atlas.example:8443", false},
		{"default port is stripped", "https://atlas.example:443", "https://atlas.example", false},
		{"userinfo is refused", "https://u:p@atlas.example", "", true},
		{"path is refused", "https://atlas.example/v1", "", true},
		{"query is refused", "https://atlas.example?a=b", "", true},
		{"http is refused", "http://atlas.example", "", true},
		// A port and no host still pins, to a value the credential endpoint
		// refuses on the wire, so it matches nothing: never "unpinned", and
		// never a load error that stops a bridge that loaded it before
		// (backlog B36; Normalize warns about it).
		{"a port with no host pins to nothing", "https://:8443", "https://:8443", false},
		// The default port is not stripped from it: stripped, it was
		// `https://`, which the handler's own reduction turns into ""
		// (unpinned), so this pin left a non-demo bridge open (backlog B97).
		{"the default port with no host pins to nothing", "https://:443", "https://:443", false},
		// A port no connection can be made to pins to nothing the same way:
		// the credential endpoint refuses it (baseurl.CredentialBase), and a
		// config that loaded with it keeps loading (#1110).
		{"a port past 65535 pins to nothing", "https://atlas.example:99999", "https://atlas.example:99999", false},
		{"no scheme is refused", "atlas.example", "", true},
		{"garbage is refused", "://nope", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &Config{Atlas: AtlasConfig{HarvestBaseURL: tc.in}}
			if got := c.Atlas.CanonicalHarvestBaseURL(); got != tc.wantCanonical {
				t.Errorf("CanonicalHarvestBaseURL() = %q, want %q", got, tc.wantCanonical)
			}
			c2 := mkLoopbackConfig(t)
			c2.Atlas.HarvestBaseURL = tc.in
			err := c2.Validate()
			if tc.wantErr && err == nil {
				t.Errorf("Validate() = nil, want an error — a malformed pin must not degrade to unpinned")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("Validate() = %v, want nil", err)
			}
		})
	}
}
