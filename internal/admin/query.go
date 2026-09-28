package admin

import (
	"net/http"
	"net/url"
	"strings"
)

// safeQuery parses the query string WITHOUT collapsing a literal "+"
// into a space.
//
// The admin twin of internal/api's safeQuery, and it exists for the
// same reason that one does: url.Values decodes "+" as a space in
// application/x-www-form-urlencoded, which is correct for form fields
// and wrong for a filesystem path. A track at "Plus Test/A+B Song.flac"
// otherwise resolves to "A B Song.flac", which does not exist, and the
// request 404s with nothing obviously wrong.
//
// This is a documented trap in this codebase — the /v1 variant-delete
// endpoint shipped with it and silently no-op'd for every path
// containing a "+" — and the admin browse, projection, enrichment,
// player-audio and variant-delete handlers all read a path from the
// query. A curl, a bookmark or any other client can send a literal "+",
// so the fix belongs at the parse, not at one caller.
//
// It binds the console's own JS too, and in the other direction.
// URLSearchParams escapes a "+" as %2B, which survives, but writes a
// SPACE as "+", which this parser keeps as a plus: every path with a
// space in it would arrive wrong. So a client of a path parameter
// escapes with encodeURIComponent (%20 and %2B), as audioURL and
// deleteVariants in static/player/api.js do, and a handler moving onto
// safeQuery moves its client in the same commit.
//
// Percent-escaping before parsing preserves an ALREADY-encoded "%2B"
// too: that decodes to "+" in one pass, and this only rewrites the
// literal character.
func safeQuery(r *http.Request) url.Values {
	if r == nil || r.URL == nil || r.URL.RawQuery == "" {
		return url.Values{}
	}
	values, _ := url.ParseQuery(strings.ReplaceAll(r.URL.RawQuery, "+", "%2B"))
	return values
}
