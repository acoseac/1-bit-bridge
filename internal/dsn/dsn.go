// Package dsn builds SQLite URI DSN strings that safely encode
// filesystem paths containing URL-reserved characters (?, #, %)
// across POSIX and Windows.
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
// Windows: filepath.ToSlash normalizes `C:\path` to `C:/path` BEFORE
// EscapedPath runs. Without it, the backslash gets percent-encoded
// to %5C, producing a DSN SQLite cannot parse on Windows. Drive-letter
// detection (`<letter>:/...`) gives those paths the SQLite-canonical
// triple-slash form `file:///C:/path`. The bridge ships windows/amd64
// + windows/arm64; goreleaser's cross-build doesn't run `go test` on
// Windows, so this code path is exercised by the test suite only —
// don't strip the drive-letter handling on the assumption "POSIX-
// only" or Windows operators silently lose every DB open.
//
// RawQuery is preserved verbatim so the pragma syntax
// `_pragma=busy_timeout(5000)` (with parens that url.QueryEscape would
// otherwise percent-encode) round-trips untouched.
package dsn

import (
	"net/url"
	"path/filepath"
	"strings"
)

// File builds a SQLite URI DSN of the form `file:<encoded-path>?<rawQuery>`.
//
// path may be absolute (`/data/db.sqlite` → `file:///data/db.sqlite`,
// or `C:\data\db.sqlite` → `file:///C:/data/db.sqlite` on Windows)
// or relative (`data/db.sqlite` → `file:data/db.sqlite`); all three
// shapes are accepted by SQLite's URI parser. Reserved characters
// in the path are percent-encoded; `/` is preserved; `\` is
// converted to `/` via filepath.ToSlash.
//
// rawQuery is appended verbatim — the caller is responsible for any
// encoding inside the query string. Pass an empty string to omit the
// `?` separator.
func File(path, rawQuery string) string {
	// filepath.ToSlash swaps OS-native separators to `/` (no-op on
	// POSIX; on Windows `C:\foo\bar` → `C:/foo/bar`). The follow-on
	// strings.ReplaceAll covers the cross-OS case where an explicit
	// Windows-shape path arrives on a non-Windows host (the helper's
	// test suite exercises both shapes regardless of GOOS — see
	// dsn_test.go). Without these conversions, EscapedPath would
	// percent-encode every backslash to %5C and SQLite would refuse
	// the URI.
	//
	// Limitation: a POSIX path containing literal `\` characters
	// will be corrupted (those bytes are valid in POSIX filenames
	// but vanishingly rare in practice). Operator data dirs that
	// hit this edge case are unsupported.
	slashed := strings.ReplaceAll(filepath.ToSlash(path), `\`, "/")
	encoded := (&url.URL{Path: slashed}).EscapedPath()
	var sb strings.Builder
	sb.WriteString("file:")
	// Triple-slash form for absolute paths; opaque for relative.
	// The pre-slash check works for both POSIX absolute (`/foo` →
	// "//" prefix → `file:///foo`) and Windows-absolute-after-slash
	// (`C:/foo` → "///" prefix → `file:///C:/foo`). Bare `file://`
	// + relative would be interpreted as host=<first segment>,
	// the wrong file.
	switch {
	case strings.HasPrefix(encoded, "/"):
		// POSIX absolute (or UNC after ToSlash → //server/share/...).
		sb.WriteString("//")
	case hasDriveLetter(encoded):
		// Windows absolute drive-letter path. SQLite expects three
		// slashes followed by the drive letter: file:///C:/path.
		sb.WriteString("///")
	}
	sb.WriteString(encoded)
	if rawQuery != "" {
		sb.WriteByte('?')
		sb.WriteString(rawQuery)
	}
	return sb.String()
}

// hasDriveLetter reports whether s is a slashed Windows absolute
// path of the form `<letter>:/...`. Detection runs on string
// content (post-ToSlash) so callers and tests get the same answer
// on every OS — `filepath.IsAbs` would only return true for these
// when running on Windows itself.
func hasDriveLetter(s string) bool {
	if len(s) < 3 || s[1] != ':' || s[2] != '/' {
		return false
	}
	c := s[0]
	return ('A' <= c && c <= 'Z') || ('a' <= c && c <= 'z')
}
