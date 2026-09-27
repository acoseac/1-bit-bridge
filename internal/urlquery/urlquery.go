// Package urlquery writes URI query components that every reader decodes to
// the same string.
//
// net/url's QueryEscape and Values.Encode write a space as "+", the
// application/x-www-form-urlencoded spelling. A form decoder reads it back
// as a space: url.ParseQuery, so r.URL.Query(), and a browser's
// URLSearchParams. An RFC 3986 reader keeps it as a plus: Foundation's
// URLComponents.queryItems, which the app reads the pairing QR with, and
// the admin console's safeQuery, which keeps "+" literal so that a file
// named "A+B.flac" resolves. So a space the bridge wrote for either of them
// arrived as a plus: the pairing QR named a library "My Library" as
// "My+Library", and `bridge enrichment misses --path "Meridian Glass"`
// asked for "Meridian+Glass", a scope nothing matched (both measured on
// 2026-09-27).
//
// Escape and Encode write %20 instead, which loses nothing: QueryEscape
// writes a literal "+" as %2B, so every "+" in its output stands for a
// space, and what is left holds only unreserved characters and %XX
// escapes, which both kinds of reader decode alike. Use them wherever the
// bridge writes a query for a reader it does not control, or for one that
// reads "+" as a plus.
package urlquery

import (
	"net/url"
	"strings"
)

// Escape escapes s for a URI query component, as url.QueryEscape does but
// with a space written %20.
func Escape(s string) string {
	return strings.ReplaceAll(url.QueryEscape(s), "+", "%20")
}

// Encode encodes v as url.Values.Encode does ("bar=baz&foo=quux", sorted by
// key), but with every space, in a key or a value, written %20.
func Encode(v url.Values) string {
	return strings.ReplaceAll(v.Encode(), "+", "%20")
}
