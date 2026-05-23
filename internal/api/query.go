package api

import (
	"net/http"
	"net/url"
	"strings"
)

// safeQuery returns r.URL.Query() with literal '+' preserved as '+' rather
// than form-decoded to space.
//
// Why this exists: Go's stdlib `r.URL.Query()` (and its underlying
// `url.ParseQuery`) treats `+` as form-encoded space per the
// `application/x-www-form-urlencoded` semantic. iOS clients
// (URLComponents.queryItems) leave `+` literal because RFC 3986 permits it
// in the query component. The mismatch breaks any iOS request whose path
// or value carries `+` — the bridge sees a space where a plus was sent,
// `os.Stat` fails, the handler returns 404. Same hazard quietly applies
// to RFC3339 `since=` timestamps with positive timezone offsets
// (`2026-05-23T15:40:00+02:00`).
//
// Fix: pre-escape literal `+` in `RawQuery` to `%2B` before parsing, so
// `url.ParseQuery` decodes it back to a literal `+`. Existing `%2B`
// occurrences from already-correct clients are left intact by
// `strings.ReplaceAll`, so the change is fully backward-compatible.
//
// The malformed-query fallback returns the stdlib's `r.URL.Query()` so
// callers never observe a behaviour regression even if the request is
// pathological.
func safeQuery(r *http.Request) url.Values {
	if r.URL.RawQuery == "" {
		return url.Values{}
	}
	safe := strings.ReplaceAll(r.URL.RawQuery, "+", "%2B")
	values, err := url.ParseQuery(safe)
	if err != nil {
		return r.URL.Query()
	}
	return values
}
