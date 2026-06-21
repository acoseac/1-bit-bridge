package api

import (
	"net/http"
	"testing"
)

// TestClientIP_PortlessFallsBackToRaw pins F8: a RemoteAddr without a
// port (SplitHostPort fails) falls back to the raw host with IPv6
// brackets stripped, so the pairing rate limiter keys on a stable
// per-host value instead of the empty-key fall-open bucket. A genuinely
// empty RemoteAddr still yields "" (limiter falls open — truly unknown
// source).
func TestClientIP_PortlessFallsBackToRaw(t *testing.T) {
	cases := []struct {
		remoteAddr string
		want       string
	}{
		{"1.2.3.4:5678", "1.2.3.4"}, // normal: port stripped
		{"[::1]:7788", "::1"},       // normal IPv6: brackets + port stripped
		{"9.9.9.9", "9.9.9.9"},      // portless IPv4: raw host preserved
		{"[fe80::1]", "fe80::1"},    // portless IPv6: brackets stripped
		{"", ""},                    // genuinely unknown → "" (fall-open)
	}
	for _, c := range cases {
		r := &http.Request{RemoteAddr: c.remoteAddr}
		if got := clientIP(r); got != c.want {
			t.Errorf("clientIP(%q) = %q, want %q", c.remoteAddr, got, c.want)
		}
	}
}
