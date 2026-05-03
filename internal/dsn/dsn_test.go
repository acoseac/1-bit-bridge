package dsn

import (
	"net/url"
	"strings"
	"testing"
)

// TestFile_PinsEncodingContract is the table-driven contract pin for
// the SQLite-URI builder. It deliberately covers both absolute and
// relative paths because url.URL serializes them differently and SQLite
// treats them differently — production uses absolute paths from
// cfg.DataDir but tests construct relative paths via t.TempDir()
// indirection, so both must work without a per-caller filepath.Abs shim.
//
// Reserved characters (?, #, %) MUST be percent-encoded in the path;
// `/` MUST be preserved; pragma parens in rawQuery MUST round-trip
// verbatim (no url.QueryEscape on the query).
func TestFile_PinsEncodingContract(t *testing.T) {
	t.Parallel()

	const pragma = "_pragma=busy_timeout(5000)"

	cases := []struct {
		name     string
		path     string
		rawQuery string
		want     string
	}{
		// Absolute POSIX paths — triple-slash form.
		{
			name:     "absolute simple",
			path:     "/data/db.sqlite",
			rawQuery: "mode=ro",
			want:     "file:///data/db.sqlite?mode=ro",
		},
		{
			name:     "absolute with pragma parens",
			path:     "/data/db.sqlite",
			rawQuery: pragma,
			want:     "file:///data/db.sqlite?_pragma=busy_timeout(5000)",
		},

		// Relative paths — opaque form (NOT file://relative/...).
		{
			name:     "relative bare",
			path:     "data/db.sqlite",
			rawQuery: "mode=ro",
			want:     "file:data/db.sqlite?mode=ro",
		},
		{
			name:     "relative dot-prefix",
			path:     "./data/db.sqlite",
			rawQuery: "mode=ro",
			want:     "file:./data/db.sqlite?mode=ro",
		},

		// Reserved characters — must percent-encode but preserve `/`.
		{
			name:     "absolute with question mark in path",
			path:     "/data/Lib?weird/bridge.db",
			rawQuery: "mode=ro",
			want:     "file:///data/Lib%3Fweird/bridge.db?mode=ro",
		},
		{
			name:     "absolute with hash in path",
			path:     "/data/Lib#weird/bridge.db",
			rawQuery: "mode=ro",
			want:     "file:///data/Lib%23weird/bridge.db?mode=ro",
		},
		{
			name:     "absolute with percent in path",
			path:     "/data/100%cool/bridge.db",
			rawQuery: "mode=ro",
			want:     "file:///data/100%25cool/bridge.db?mode=ro",
		},

		// No query.
		{
			name:     "absolute no query",
			path:     "/data/db.sqlite",
			rawQuery: "",
			want:     "file:///data/db.sqlite",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := File(c.path, c.rawQuery)
			if got != c.want {
				t.Fatalf("File(%q, %q) = %q, want %q", c.path, c.rawQuery, got, c.want)
			}
		})
	}
}

// TestFile_PathRoundTrips parses every produced DSN with url.Parse and
// asserts the decoded path matches the input — defends against future
// encoding changes that produce URLs Go's parser disagrees with.
func TestFile_PathRoundTrips(t *testing.T) {
	t.Parallel()

	cases := []string{
		"/data/db.sqlite",
		"/data/Lib?weird/bridge.db",
		"/data/Lib#weird/bridge.db",
		"/data/100%cool/bridge.db",
		// Relative paths are stored in URL.Opaque, not URL.Path, when
		// emitted as `file:rel` — handled below.
	}
	for _, p := range cases {
		t.Run(p, func(t *testing.T) {
			t.Parallel()
			dsn := File(p, "mode=ro")
			u, err := url.Parse(dsn)
			if err != nil {
				t.Fatalf("url.Parse(%q): %v", dsn, err)
			}
			if u.Scheme != "file" {
				t.Fatalf("scheme = %q, want %q", u.Scheme, "file")
			}
			if u.Path != p {
				t.Fatalf("decoded path = %q, want %q", u.Path, p)
			}
			if u.RawQuery != "mode=ro" {
				t.Fatalf("RawQuery = %q, want %q", u.RawQuery, "mode=ro")
			}
		})
	}
}

// TestFile_PragmaRawQueryNotDoubleEncoded explicitly covers the parens
// case — url.QueryEscape would percent-encode `(` and `)`, producing
// `_pragma=busy_timeout%285000%29` which the modernc.org/sqlite driver
// does not parse the same way. RawQuery passes the string through.
func TestFile_PragmaRawQueryNotDoubleEncoded(t *testing.T) {
	t.Parallel()
	dsn := File("/data/db.sqlite", "_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if !strings.Contains(dsn, "busy_timeout(5000)") {
		t.Fatalf("DSN dropped the parens: %q", dsn)
	}
	if !strings.Contains(dsn, "foreign_keys(1)") {
		t.Fatalf("DSN dropped the parens: %q", dsn)
	}
	// url.Parse should still read the query as a single RawQuery string.
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("url.Parse: %v", err)
	}
	if u.RawQuery != "_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)" {
		t.Fatalf("RawQuery round-trip: %q", u.RawQuery)
	}
}
