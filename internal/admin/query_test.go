package admin

import (
	"net/http/httptest"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/urlquery"
)

// TestSafeQueryPreservesLiteralPlus pins the trap this helper exists
// for. url.Values decodes "+" as a space — correct for a form field,
// wrong for a filesystem path — so a track at "Plus Test/A+B Song.flac"
// resolves to "A B Song.flac", which does not exist, and the request
// 404s with nothing obviously wrong.
//
// Verified against a real file on a running bridge before the fix: the
// %2B form returned 200 and the raw "+" form returned 404.
func TestSafeQueryPreservesLiteralPlus(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{"path=Plus+Test/A+B.flac", "Plus+Test/A+B.flac"},
		{"path=Plus%20Test/A%2BB.flac", "Plus Test/A+B.flac"}, // already-encoded still decodes
		{"path=a%20b", "a b"},
		{"path=plain", "plain"},
		{"", ""},
	} {
		r := httptest.NewRequest("GET", "/x?"+tc.raw, nil)
		if got := safeQuery(r).Get("path"); got != tc.want {
			t.Errorf("raw %q: got %q, want %q", tc.raw, got, tc.want)
		}
	}
}

// TestSafeQueryLeavesOtherParamsAlone — the rewrite must not disturb
// anything else in the query string.
func TestSafeQueryLeavesOtherParamsAlone(t *testing.T) {
	r := httptest.NewRequest("GET", "/x?path=a+b&sort=recent&limit=20", nil)
	q := safeQuery(r)
	if q.Get("sort") != "recent" || q.Get("limit") != "20" {
		t.Errorf("sibling params disturbed: %v", q)
	}
}

// TestSafeQueryRoundTripsEncodeURIComponent pins the CROSS-FILE
// contract between the player's JS and this parser, which is a real
// pair rather than two independent choices.
//
// The trap, hit for real during development: the client originally
// built its URLs with URLSearchParams, which serialises to
// application/x-www-form-urlencoded and writes a SPACE as "+". Once
// safeQuery started reading "+" as a literal plus — so that a file
// genuinely named "A+B.flac" resolves — every path containing a space
// broke. The two encodings are incompatible, and the resolution is
// that the client must use encodeURIComponent (%20 for space, %2B for
// plus), which is what this asserts.
//
// The Go client is `bridge enrichment misses --path`, which escapes with
// urlquery.Escape, and the cases are encoded with that same function. It
// writes what encodeURIComponent writes for them, and it is the escape the
// CLI used url.QueryEscape in place of until 2026-09-27, the same "+" for
// a space.
func TestSafeQueryRoundTripsEncodeURIComponent(t *testing.T) {
	for _, want := range []string{
		"Plus Test/A+B Song.flac",
		"Artist/Album Name/01 - Title.flac",
		"Ünïcödé Álbum/01 — Trëck.flac",
		"a&b/c=d/e?f.flac",
		"plain.flac",
	} {
		encoded := urlquery.Escape(want)
		r := httptest.NewRequest("GET", "/x?path="+encoded, nil)
		if got := safeQuery(r).Get("path"); got != want {
			t.Errorf("path %q encoded as %q round-tripped to %q", want, encoded, got)
		}
	}
}
