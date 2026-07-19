package dsn

import (
	"net/url"
	"runtime"
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
		// wantPosix, when non-empty, is the expected output on
		// non-Windows hosts. Used by the drive-letter cases whose
		// absolute-vs-relative meaning is OS-dependent — File() emits the
		// triple-slash absolute form only on Windows (runtime.GOOS).
		wantPosix string
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

		// Windows drive-letter paths (Gemini on PR #146). The bridge
		// ships windows/amd64 + windows/arm64; without ToSlash +
		// drive-letter handling, EscapedPath would percent-encode every
		// `\` to `%5C` and SQLite would refuse the URI on Windows.
		//
		// The absolute-vs-relative meaning of `<letter>:/…` is
		// OS-dependent: File() emits the triple-slash `file:///C:/…`
		// (absolute) only on Windows; on POSIX the identical string is a
		// relative path (dir literally named "C:") → opaque `file:C:/…`.
		// So each case carries wantPosix for the non-Windows expectation.
		{
			name:      "windows absolute backslash",
			path:      `C:\data\db.sqlite`,
			rawQuery:  "mode=ro",
			want:      "file:///C:/data/db.sqlite?mode=ro",
			wantPosix: "file:C:/data/db.sqlite?mode=ro",
		},
		{
			name:      "windows absolute forward-slash already",
			path:      "C:/data/db.sqlite",
			rawQuery:  "mode=ro",
			want:      "file:///C:/data/db.sqlite?mode=ro",
			wantPosix: "file:C:/data/db.sqlite?mode=ro",
		},
		{
			name:      "windows lowercase drive letter",
			path:      `d:\data\db.sqlite`,
			rawQuery:  "mode=ro",
			want:      "file:///d:/data/db.sqlite?mode=ro",
			wantPosix: "file:d:/data/db.sqlite?mode=ro",
		},
		{
			name:      "windows absolute with reserved char",
			path:      `C:\data\Lib?weird\bridge.db`,
			rawQuery:  "mode=ro",
			want:      "file:///C:/data/Lib%3Fweird/bridge.db?mode=ro",
			wantPosix: "file:C:/data/Lib%3Fweird/bridge.db?mode=ro",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			want := c.want
			if c.wantPosix != "" && runtime.GOOS != "windows" {
				want = c.wantPosix
			}
			got := File(c.path, c.rawQuery)
			if got != want {
				t.Fatalf("File(%q, %q) = %q, want %q", c.path, c.rawQuery, got, want)
			}
		})
	}
}

// TestFile_PosixRelativeDriveLetterNotAbsolute pins B53: off Windows, a
// path like "A:/db.sqlite" is a RELATIVE path (a dir literally named
// "A:"), so File() MUST emit the opaque `file:A:/db.sqlite` form, NOT the
// Windows-absolute `file:///A:/db.sqlite` (which SQLite would open as the
// wrong file, `/A:/…`). The drive-letter → triple-slash mapping is gated
// on runtime.GOOS == "windows".
func TestFile_PosixRelativeDriveLetterNotAbsolute(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("on Windows a leading <letter>:/ IS an absolute drive-letter path")
	}
	const (
		in   = "A:/db.sqlite"
		want = "file:A:/db.sqlite?mode=ro"
	)
	if got := File(in, "mode=ro"); got != want {
		t.Fatalf("File(%q) = %q, want %q (a POSIX dir named %q must not be read as a Windows drive)", in, got, want, "A:")
	}
}

// TestFile_AbsolutePathRoundTrips parses every produced absolute-path
// DSN with url.Parse and asserts the decoded path matches the input —
// defends against future encoding changes that produce URLs Go's
// parser disagrees with.
//
// Absolute paths land in `URL.Path` (triple-slash form
// `file:///abs/path`); relative paths land in `URL.Opaque` (opaque
// form `file:rel/path`) and are pinned by
// TestFile_RelativePathRoundTrips below.
func TestFile_AbsolutePathRoundTrips(t *testing.T) {
	t.Parallel()

	cases := []string{
		"/data/db.sqlite",
		"/data/Lib?weird/bridge.db",
		"/data/Lib#weird/bridge.db",
		"/data/100%cool/bridge.db",
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

// TestFile_RelativePathRoundTrips pins the relative-path side of the
// contract (Qodo on PR #146): `File("data/db.sqlite", ...)` MUST
// emit the opaque form `file:data/db.sqlite?...` — NOT
// `file://data/db.sqlite`, which RFC 3986 reads as host="data" +
// path="/db.sqlite" (the wrong file).
//
// Relative paths come back from url.Parse as `URL.Opaque`, so the
// assertion target is different from the absolute case. Without a
// test that explicitly exercises this, a future refactor could swap
// the helper to a `url.URL{Path: ...}.String()`-only implementation
// (which produces `file://data/...`) and pass the absolute-path
// suite while silently regressing every t.TempDir()-anchored test
// in the rest of the codebase.
func TestFile_RelativePathRoundTrips(t *testing.T) {
	t.Parallel()

	cases := []string{
		"data/db.sqlite",
		"./data/db.sqlite",
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
			// Relative path → opaque, NOT host or path. A non-empty
			// u.Host means we accidentally produced `file://rel/path`
			// — the wrong file.
			if u.Host != "" {
				t.Fatalf("relative path produced non-empty host %q (would parse as host, not path) — DSN was %q", u.Host, dsn)
			}
			if u.Opaque != p {
				t.Fatalf("decoded opaque = %q, want %q (DSN: %q)", u.Opaque, p, dsn)
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
