package api

import (
	"net/http/httptest"
	"testing"
)

func TestSafeQueryPreservesLiteralPlusInPath(t *testing.T) {
	// iOS URLComponents.queryItems leaves '+' literal in the query
	// string (because '+' is in Apple's `urlQueryAllowed` set per RFC
	// 3986) and percent-encodes spaces as %20. The on-the-wire shape
	// for "Kwabs/Love + War/01.flac" is therefore
	// "Kwabs/Love%20+%20War/01.flac".
	//
	// Pre-fix, r.URL.Query().Get("path") form-decoded the '+' to a
	// space, producing "Kwabs/Love   War/01.flac" (three spaces) —
	// the bridge would then 404 on a path that doesn't exist on disk.
	// safeQuery preserves the '+' literal.
	req := httptest.NewRequest("GET", "/v1/download?path=Kwabs/Love%20+%20War/01.flac", nil)
	got := safeQuery(req).Get("path")
	want := "Kwabs/Love + War/01.flac"
	if got != want {
		t.Fatalf("safeQuery got %q, want %q", got, want)
	}
}

func TestSafeQueryStdlibGetWouldHaveLostSpaces(t *testing.T) {
	// Sanity-check the contract: confirm that the stdlib path the helper
	// replaces would have produced the wrong value for the same input.
	// If this assertion ever fails, the Go stdlib changed semantics and
	// the helper may no longer be needed.
	req := httptest.NewRequest("GET", "/v1/download?path=Kwabs/Love%20+%20War/01.flac", nil)
	stdlib := req.URL.Query().Get("path")
	if stdlib == "Kwabs/Love + War/01.flac" {
		t.Fatalf("Go stdlib unexpectedly preserved '+' in query — helper may be redundant")
	}
	// Confirm the specific wrong shape: '+' decoded to space.
	want := "Kwabs/Love   War/01.flac"
	if stdlib != want {
		t.Fatalf("Go stdlib produced %q, expected the historical wrong shape %q", stdlib, want)
	}
}

func TestSafeQueryPreservesTimezoneOffsetPlusInRFC3339(t *testing.T) {
	// Pre-fix bug Gemini flagged: ?since=2026-05-23T15:40:00+02:00 with
	// an un-percent-encoded '+' would parse the timezone offset's '+' as
	// a space, and time.Parse would then reject the resulting string.
	req := httptest.NewRequest("GET", "/v1/manifest?since=2026-05-23T15:40:00+02:00", nil)
	got := safeQuery(req).Get("since")
	want := "2026-05-23T15:40:00+02:00"
	if got != want {
		t.Fatalf("safeQuery got %q, want %q", got, want)
	}
}

func TestSafeQueryRoundTripsPercentEncodedPlus(t *testing.T) {
	// A client that already correctly sends %2B should see no change.
	req := httptest.NewRequest("GET", "/v1/download?path=a%2Bb", nil)
	got := safeQuery(req).Get("path")
	want := "a+b"
	if got != want {
		t.Fatalf("safeQuery got %q, want %q", got, want)
	}
}

func TestSafeQueryHandlesEmptyRawQuery(t *testing.T) {
	req := httptest.NewRequest("GET", "/v1/health", nil)
	values := safeQuery(req)
	if len(values) != 0 {
		t.Fatalf("safeQuery on empty RawQuery: got %d values, want 0", len(values))
	}
	if got := values.Get("path"); got != "" {
		t.Fatalf("safeQuery.Get on empty RawQuery: got %q, want \"\"", got)
	}
}

func TestSafeQueryHandlesMultipleParams(t *testing.T) {
	// Mixed: one value with literal '+' (path), one ASCII-only (limit).
	// Both must round-trip cleanly.
	req := httptest.NewRequest("GET", "/v1/download?path=A+B&limit=500", nil)
	values := safeQuery(req)
	if got := values.Get("path"); got != "A+B" {
		t.Fatalf("safeQuery path: got %q, want %q", got, "A+B")
	}
	if got := values.Get("limit"); got != "500" {
		t.Fatalf("safeQuery limit: got %q, want %q", got, "500")
	}
}

func TestSafeQueryMalformedQueryFallsBackGracefully(t *testing.T) {
	// A pathological query that fails url.ParseQuery still produces a
	// usable result (the stdlib fallback). The exact behaviour matches
	// pre-fix stdlib behaviour, which is the right shape for a defensive
	// fallback.
	req := httptest.NewRequest("GET", "/v1/x?%ZZ", nil)
	values := safeQuery(req)
	// Don't assert exact contents — just confirm it returned without panic.
	_ = values
}
