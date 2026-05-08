package manifest

import (
	"database/sql/driver"
	"fmt"

	"golang.org/x/text/cases"
	"golang.org/x/text/language"
	sqlite "modernc.org/sqlite"
)

// unicodeLowerScalar is the SQL-side `unicode_lower(text)` function
// body. Mirrors SQLite's built-in `LOWER()` calling convention:
// nil → nil pass-through, non-text → nil (LOWER's documented
// behaviour for BLOB / non-text args), and text → Unicode-folded
// lowercase.
//
// **Determinism is required for use in functional indexes** — the
// caller registers via `MustRegisterDeterministicScalarFunction` so
// SQLite knows the expression `unicode_lower(path)` produces the
// same hash for the same input across `INSERT` and `SELECT`. Without
// the determinism flag, an index built on this expression would be
// silently bypassed at query time.
//
// `language.Und` keeps the fold language-agnostic — same semantics
// iOS gets from `String.lowercased()` with no locale set, which is
// what the iOS client uses to normalise paths before sending them
// to the bridge. Locale-specific transforms (Turkish dotted-I,
// Lithuanian decompositions) would diverge from iOS and re-introduce
// the same miss-class this function exists to fix.
//
// The Caser is constructed PER CALL, not cached at package init.
// `cases.Lower()` returns a stateful Caser that the docs explicitly
// warn must not be shared across goroutines (only `cases.Fold()`
// carries the documented concurrent-safety guarantee — which we
// can't use because Fold semantics differ from iOS's `lowercased()`).
// Under `database/sql`'s default connection pool the SQLite scalar
// runs on whichever goroutine is driving the connection, so a
// shared Caser would land concurrent calls on the same instance.
// Per-call construction is a cheap struct allocation — acceptable
// for the path-lookup hot path and unambiguously safe per the
// library's documented contract. (Greptile P1 on PR #182.)
func unicodeLowerScalar(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
	if len(args) != 1 {
		return nil, fmt.Errorf("unicode_lower: expected 1 argument, got %d", len(args))
	}
	caser := cases.Lower(language.Und)
	switch v := args[0].(type) {
	case nil:
		// SQLite NULL → NULL, matching LOWER()'s pass-through.
		return nil, nil
	case string:
		return caser.String(v), nil
	case []byte:
		// LOWER() accepts text that arrived as a BLOB and returns
		// it lowered; mirror that for compat. The folded form is
		// returned as a string (driver.Value supports both, and
		// string is the canonical representation for the indexed
		// expression `unicode_lower(path)` on a TEXT column).
		return caser.String(string(v)), nil
	default:
		// Non-text input → nil, matching SQLite LOWER()'s
		// behaviour on numeric / blob inputs that aren't text-
		// coercible. Conservative: a wrong type from a future
		// caller surfaces as a missed index match rather than a
		// query-time runtime fault.
		return nil, nil
	}
}

// init registers `unicode_lower(text)` against the modernc.org/sqlite
// driver at package-load time. The registration is global to the
// driver — every connection opened with `sql.Open("sqlite", …)` after
// this point sees the function.
//
// **Why init() is the right home**: modernc.org/sqlite (v1.50.0) does
// not expose a per-connection ConnectHook (mattn/go-sqlite3 does, but
// that driver isn't pure-Go). The driver-level registration is the
// idiomatic path; the registered scalar function is stateless (the
// per-call Caser is constructed inside the function body, not
// captured), so global registration carries no race or lifecycle
// risk.
//
// **Operator note**: a plain `sqlite3 bridge.db` CLI session does NOT
// see `unicode_lower(...)` — the function only exists inside processes
// that import this Go package. Operators reaching for raw sqlite3 for
// ad-hoc inspection should use the built-in `lower()` instead; the
// underlying TEXT data is preserved in original case in the column,
// so manual queries still work, just with the ASCII-only folding
// semantics the v3 indexes had.
func init() {
	sqlite.MustRegisterDeterministicScalarFunction("unicode_lower", 1, unicodeLowerScalar)
}
