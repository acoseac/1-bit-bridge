package manifest

import (
	"database/sql/driver"
	"fmt"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/language"
	"golang.org/x/text/unicode/norm"
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
// The folded string is NFC-composed before returning (2026-07-21
// review, M9). `cases.Lower` folds case only — it never composes —
// while iOS's `share.normalize(path:)` emits NFC + lowercase and the
// scanner stores the on-disk form, which is NFD for files migrated
// from HFS+ or synced from Linux/NAS onto a Mac. The two forms
// compared byte-wise UNEQUAL, so every accented NFD path missed the
// LookupTrack / LookupVariant / LookupAnalysis fallback. Composing
// here covers both sides of the comparison — the indexed column
// expression AND the query parameter both flow through
// `unicode_lower(...)`. Migration v26 rebuilds the three functional
// indexes embedding this function because its output changes for
// NFD input.
//
// The Caser is constructed PER CALL, not cached at package init.
// Under `database/sql`'s default connection pool the SQLite scalar
// runs on whichever goroutine is driving the connection, so anything
// shared here is shared across goroutines.
//
// The rationale this comment used to give — "cases.Lower() returns a
// stateful Caser that must not be shared" — is TOO STRONG, and
// contradicted `internal/fs`, which caches exactly this expression at
// package level. For the `und` tag with no options x/text's makeLower
// returns the `undLower` SINGLETON, an empty struct whose Transform
// builds its context as a local; caching it is safe, and fs.go's
// comment now states the precise condition. Corrected here rather than
// on that side, because that side was right.
//
// Per-call construction is KEPT anyway, and not merely from inertia:
// this function is embedded in three functional indexes, so a
// concurrency fault would not surface as a crash but as silently
// mismatched lookup keys, and unpicking that needs a migration (v26
// already rebuilt those indexes once). The allocation is a cheap empty
// struct on a path already doing SQLite work — the wrong thing to
// optimise against a failure that expensive. If you do hoist it, hoist
// ONLY the `language.Und` form: naming a locale x/text special-cases
// (az, lt, tr) switches makeLower to a caser that embeds mutable state,
// and then sharing really is a race. (Original note: Greptile P1 on
// PR #182.)
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
		return nfcCompose(caser.String(v)), nil
	case []byte:
		// LOWER() accepts text that arrived as a BLOB and returns
		// it lowered; mirror that for compat. The folded form is
		// returned as a string (driver.Value supports both, and
		// string is the canonical representation for the indexed
		// expression `unicode_lower(path)` on a TEXT column).
		return nfcCompose(caser.String(string(v))), nil
	default:
		// Non-text input → nil, matching SQLite LOWER()'s
		// behaviour on numeric / blob inputs that aren't text-
		// coercible. Conservative: a wrong type from a future
		// caller surfaces as a missed index match rather than a
		// query-time runtime fault.
		return nil, nil
	}
}

// nfcCompose NFC-composes a case-folded lookup key. Ill-formed UTF-8
// falls back to the uncomposed fold: `norm.NFC.String` never fails
// (it passes ill-formed input through unchanged), but the explicit
// guard keeps that edge deterministic instead of depending on norm's
// pass-through internals. Manifest paths are always valid UTF-8 —
// they originate from the FS scan — so the guard is defensive.
func nfcCompose(s string) string {
	if !utf8.ValidString(s) {
		return s
	}
	return norm.NFC.String(s)
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
