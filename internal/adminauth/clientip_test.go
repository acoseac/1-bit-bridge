package adminauth

import (
	"net/http"
	"testing"
)

func newReq(remote string, headers map[string]string) *http.Request {
	r := &http.Request{
		RemoteAddr: remote,
		Header:     http.Header{},
	}
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

func TestExtractClientIPFallsBackToRemoteAddrWhenNotTrusted(t *testing.T) {
	r := newReq("203.0.113.7:54321", map[string]string{
		"X-Forwarded-For": "1.2.3.4",
		"X-Real-IP":       "5.6.7.8",
	})
	// trustForwardedHeaders=false: the proxy headers MUST be ignored,
	// otherwise a direct attacker forges them to evade rate-limits.
	got := ExtractClientIP(r, false)
	if got != "203.0.113.7" {
		t.Errorf("trust=false: got %q, want %q", got, "203.0.113.7")
	}
}

func TestExtractClientIPPrefersXRealIPOverXForwardedFor(t *testing.T) {
	// Gemini-high security review on PR #290: X-Real-IP is the
	// preferred signal because Caddy/nginx overwrite it on every
	// ingress, so it can't be poisoned by a client. We take it
	// before consulting XFF (which is more spoofable even with
	// the right-most strategy when the proxy is misconfigured).
	r := newReq("10.0.0.1:54321", map[string]string{
		"X-Real-IP":       "203.0.113.7",
		"X-Forwarded-For": "should-not-be-consulted, 9.9.9.9",
	})
	got := ExtractClientIP(r, true)
	if got != "203.0.113.7" {
		t.Errorf("got %q, want %q (X-Real-IP must win over XFF)", got, "203.0.113.7")
	}
}

func TestExtractClientIPUsesRightmostXForwardedForWhenNoRealIP(t *testing.T) {
	// XFF chain semantics: each proxy appends the IP it saw on
	// connect. Right-most = trusted-proxy view of the immediate
	// client = the spoof-resistant choice (left-most could be
	// arbitrary client-supplied junk).
	r := newReq("10.0.0.1:54321", map[string]string{
		"X-Forwarded-For": "198.51.100.2, 203.0.113.7",
	})
	got := ExtractClientIP(r, true)
	if got != "203.0.113.7" {
		t.Errorf("got %q, want %q (right-most XFF entry)", got, "203.0.113.7")
	}
}

func TestExtractClientIPRightmostHandlesWhitespace(t *testing.T) {
	r := newReq("10.0.0.1:54321", map[string]string{
		"X-Forwarded-For": "  198.51.100.2  ,  203.0.113.7  ",
	})
	got := ExtractClientIP(r, true)
	if got != "203.0.113.7" {
		t.Errorf("got %q, want %q", got, "203.0.113.7")
	}
}

func TestExtractClientIPRightmostRejectsInvalidLastElement(t *testing.T) {
	// `X-Forwarded-For: 1.2.3.4, garbage` should NOT key the
	// bucket on "garbage" — we walk back from the right to the
	// first valid IP. Defeats trailing-junk injection.
	r := newReq("10.0.0.1:54321", map[string]string{
		"X-Forwarded-For": "1.2.3.4, garbage",
	})
	got := ExtractClientIP(r, true)
	if got != "1.2.3.4" {
		t.Errorf("invalid-right injection: got %q, want %q", got, "1.2.3.4")
	}
}

func TestExtractClientIPRejectsClientSpoofedLeftmost(t *testing.T) {
	// Threat model: attacker controls their own connection and
	// puts a forged value in XFF. Proxy appends its true-client
	// view. With right-most strategy, the proxy's view wins; the
	// attacker's forge is ignored. Use realistic IP values so the
	// walk's net.ParseIP check accepts both entries — the
	// assertion below pins that we still take the right-most.
	r := newReq("10.0.0.1:54321", map[string]string{
		"X-Forwarded-For": "1.1.1.1, 2.2.2.2",
	})
	got := ExtractClientIP(r, true)
	if got != "2.2.2.2" {
		t.Errorf("spoofed-left attack: got %q, want right-most %q", got, "2.2.2.2")
	}
}

func TestExtractClientIPRejectsInvalidXRealIP(t *testing.T) {
	r := newReq("10.0.0.1:54321", map[string]string{
		"X-Real-IP": "not-an-ip",
	})
	got := ExtractClientIP(r, true)
	if got != "10.0.0.1" {
		t.Errorf("invalid X-Real-IP should fall through to RemoteAddr (no XFF set); got %q", got)
	}
}

func TestExtractClientIPHandlesIPv6(t *testing.T) {
	r := newReq("[2001:db8::1]:54321", nil)
	got := ExtractClientIP(r, false)
	if got != "2001:db8::1" {
		t.Errorf("ipv6: got %q, want %q", got, "2001:db8::1")
	}
}

func TestExtractClientIPEmptyXRealIPFallsThroughToXFF(t *testing.T) {
	r := newReq("10.0.0.1:54321", map[string]string{
		"X-Real-IP":       "",
		"X-Forwarded-For": "203.0.113.7",
	})
	got := ExtractClientIP(r, true)
	if got != "203.0.113.7" {
		t.Errorf("empty X-Real-IP should fall through to XFF; got %q", got)
	}
}

// The returned IP is CANONICAL, not the caller's spelling.
//
// `192.0.2.1` and `::ffff:192.0.2.1` are the same host but were
// distinct strings, so they keyed distinct rate-limit buckets — varying
// the spelling handed a caller a fresh bucket and diluted the limiter.
// Canonicalising collapses them, and makes "this only ever returns a
// bare IP literal" structural rather than a property you re-derive from
// the validation at each return.
func TestExtractClientIP_CanonicalisesEquivalentSpellings(t *testing.T) {
	cases := []struct {
		name   string
		header string
		want   string
	}{
		{"v4-mapped v6 collapses to v4", "::ffff:192.0.2.1", "192.0.2.1"},
		{"plain v4 unchanged", "192.0.2.1", "192.0.2.1"},
		{"v6 lowercased and compressed", "2001:0DB8:0000:0000:0000:0000:0000:0001", "2001:db8::1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newReq("10.0.0.1:1234", map[string]string{"X-Real-IP": tc.header})
			if got := ExtractClientIP(r, true); got != tc.want {
				t.Fatalf("ExtractClientIP = %q, want %q — equivalent spellings must "+
					"collapse to one rate-limit bucket key", got, tc.want)
			}
		})
	}
}

// Repeated X-Forwarded-For header LINES must all be considered, not
// just the first. Go stores them as separate slice entries and
// Header.Get returns only index 0, so a proxy that appends a line
// (HAProxy's `option forwardfor`) rather than comma-appending would
// leave the rightmost-valid walk seeing nothing but the
// client-supplied line.
func TestExtractClientIP_ConsidersRepeatedForwardedForLines(t *testing.T) {
	r := newReq("10.0.0.1:1234", nil)
	r.Header.Add("X-Forwarded-For", "203.0.113.9")  // attacker-supplied
	r.Header.Add("X-Forwarded-For", "198.51.100.7") // appended by the trusted proxy

	if got := ExtractClientIP(r, true); got != "198.51.100.7" {
		t.Fatalf("ExtractClientIP = %q, want the proxy-appended 198.51.100.7 — "+
			"only the first header line was examined", got)
	}
}
