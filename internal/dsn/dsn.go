// Package dsn builds SQLite URI DSN strings that safely encode
// filesystem paths containing URL-reserved characters (?, #, %).
//
// The bridge's data directory is operator-controlled, so a path like
// `/data/Lib?weird/bridge.db` is technically legal on POSIX but would
// be misparsed by SQLite's URI handler if pasted into the DSN with
// fmt.Sprintf — the `?` would terminate the path and the rest would
// be parsed as a query string.
//
// We use url.URL.EscapedPath() to percent-encode reserved characters
// while preserving `/`, then assemble the DSN manually so absolute
// paths get the SQLite-canonical triple-slash form (`file:///abs`)
// and relative paths get the opaque form (`file:rel`). url.URL.String()
// alone is unsafe here: an empty Host with a relative Path emits
// `file://relative/path`, which SQLite's URI parser interprets as
// host="relative" + path="/path" — the wrong file.
//
// RawQuery is preserved verbatim so the pragma syntax
// `_pragma=busy_timeout(5000)` (with parens that url.QueryEscape would
// otherwise percent-encode) round-trips untouched.
package dsn

import (
	"net/url"
	"strings"
)

// File builds a SQLite URI DSN of the form `file:<encoded-path>?<rawQuery>`.
//
// path may be absolute (`/data/db.sqlite` → `file:///data/db.sqlite`)
// or relative (`data/db.sqlite` → `file:data/db.sqlite`); both forms
// are accepted by SQLite's URI parser. Reserved characters in the
// path are percent-encoded; `/` is preserved.
//
// rawQuery is appended verbatim — the caller is responsible for any
// encoding inside the query string. Pass an empty string to omit the
// `?` separator.
func File(path, rawQuery string) string {
	encoded := (&url.URL{Path: path}).EscapedPath()
	var sb strings.Builder
	sb.WriteString("file:")
	// Absolute → triple-slash (`file:///abs/path`); relative → opaque
	// (`file:rel/path`). The bare `file://path` form would be
	// interpreted as host="path" by RFC 3986, not a relative path.
	if strings.HasPrefix(encoded, "/") {
		sb.WriteString("//")
	}
	sb.WriteString(encoded)
	if rawQuery != "" {
		sb.WriteByte('?')
		sb.WriteString(rawQuery)
	}
	return sb.String()
}
