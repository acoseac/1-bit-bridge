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

func TestExtractClientIPUsesXForwardedForWhenTrusted(t *testing.T) {
	r := newReq("10.0.0.1:54321", map[string]string{
		"X-Forwarded-For": "203.0.113.7",
	})
	got := ExtractClientIP(r, true)
	if got != "203.0.113.7" {
		t.Errorf("got %q, want %q", got, "203.0.113.7")
	}
}

func TestExtractClientIPLeftmostNonEmptyXForwardedFor(t *testing.T) {
	r := newReq("10.0.0.1:54321", map[string]string{
		"X-Forwarded-For": "  203.0.113.7  ,  198.51.100.2  ",
	})
	got := ExtractClientIP(r, true)
	if got != "203.0.113.7" {
		t.Errorf("got %q, want %q (left-most after trim)", got, "203.0.113.7")
	}
}

func TestExtractClientIPRejectsInvalidLeftmost(t *testing.T) {
	// `X-Forwarded-For: garbage, 1.2.3.4` should NOT key the bucket
	// on "garbage" — we walk to the first valid IP rather than
	// taking the literal left-most. Defeats injection.
	r := newReq("10.0.0.1:54321", map[string]string{
		"X-Forwarded-For": "garbage, 1.2.3.4",
	})
	got := ExtractClientIP(r, true)
	if got != "1.2.3.4" {
		t.Errorf("invalid-left injection: got %q, want %q", got, "1.2.3.4")
	}
}

func TestExtractClientIPFallsBackToXRealIP(t *testing.T) {
	r := newReq("10.0.0.1:54321", map[string]string{
		"X-Real-IP": "203.0.113.7",
	})
	got := ExtractClientIP(r, true)
	if got != "203.0.113.7" {
		t.Errorf("got %q, want %q", got, "203.0.113.7")
	}
}

func TestExtractClientIPRejectsInvalidXRealIP(t *testing.T) {
	r := newReq("10.0.0.1:54321", map[string]string{
		"X-Real-IP": "not-an-ip",
	})
	got := ExtractClientIP(r, true)
	if got != "10.0.0.1" {
		t.Errorf("invalid X-Real-IP should fall through to RemoteAddr; got %q", got)
	}
}

func TestExtractClientIPHandlesIPv6(t *testing.T) {
	r := newReq("[2001:db8::1]:54321", nil)
	got := ExtractClientIP(r, false)
	if got != "2001:db8::1" {
		t.Errorf("ipv6: got %q, want %q", got, "2001:db8::1")
	}
}

func TestExtractClientIPEmptyXForwardedForWalksOn(t *testing.T) {
	r := newReq("10.0.0.1:54321", map[string]string{
		"X-Forwarded-For": "",
		"X-Real-IP":       "203.0.113.7",
	})
	got := ExtractClientIP(r, true)
	if got != "203.0.113.7" {
		t.Errorf("empty XFF should fall through to X-Real-IP; got %q", got)
	}
}
