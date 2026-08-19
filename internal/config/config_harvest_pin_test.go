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
		{"http is refused", "http://atlas.example", "", true},
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
