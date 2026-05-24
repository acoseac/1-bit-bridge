package main

import "testing"

// TestBanneradminURLShapes pins the operator-facing URL the
// public-mode startup banner prints. CodeRabbit Major review
// post-PR-#295: pre-fix the banner printed cfg.AdminAddress
// (a bind target like 0.0.0.0:7789 or 127.0.0.1:7789), which
// is meaningless to anyone browsing from elsewhere. Now the
// URL is derived from cfg.Autocert.Domain + the admin port.
func TestBanneradminURLShapes(t *testing.T) {
	cases := []struct {
		name         string
		domain       string
		adminAddress string
		scheme       string
		want         string
	}{
		{
			name:         "https + non-:443 admin port",
			domain:       "bridge.example.com",
			adminAddress: "0.0.0.0:7789",
			scheme:       "https",
			want:         "https://bridge.example.com:7789/",
		},
		{
			name:         "https + :443 admin port omits port",
			domain:       "bridge.example.com",
			adminAddress: "0.0.0.0:443",
			scheme:       "https",
			want:         "https://bridge.example.com/",
		},
		{
			name:         "loopback bind doesn't leak into URL — domain wins",
			domain:       "bridge.example.com",
			adminAddress: "127.0.0.1:7789",
			scheme:       "https",
			want:         "https://bridge.example.com:7789/",
		},
		{
			name:         "specific-iface bind doesn't leak",
			domain:       "bridge.example.com",
			adminAddress: "192.168.1.5:7789",
			scheme:       "https",
			want:         "https://bridge.example.com:7789/",
		},
		{
			name:         "trailing dot stripped from domain",
			domain:       "bridge.example.com.",
			adminAddress: "0.0.0.0:7789",
			scheme:       "https",
			want:         "https://bridge.example.com:7789/",
		},
		{
			name:         "empty domain falls back to bind address (test wiring)",
			domain:       "",
			adminAddress: "127.0.0.1:7789",
			scheme:       "https",
			want:         "https://127.0.0.1:7789/",
		},
		{
			name:         "empty scheme defaults to https",
			domain:       "bridge.example.com",
			adminAddress: "0.0.0.0:7789",
			scheme:       "",
			want:         "https://bridge.example.com:7789/",
		},
		{
			name:         "malformed adminAddress falls back to bare domain",
			domain:       "bridge.example.com",
			adminAddress: "not-an-addr",
			scheme:       "https",
			want:         "https://bridge.example.com/",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := banneradminURL(tc.domain, tc.adminAddress, tc.scheme)
			if got != tc.want {
				t.Errorf("banneradminURL(%q, %q, %q) = %q, want %q",
					tc.domain, tc.adminAddress, tc.scheme, got, tc.want)
			}
		})
	}
}
