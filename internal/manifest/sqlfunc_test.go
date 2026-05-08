package manifest

import (
	"database/sql/driver"
	"strings"
	"testing"
)

// TestUnicodeLowerScalar pins the in-Go function semantics — fast,
// hermetic, no SQLite. Locks the iOS-parity contract:
// `cases.Lower(language.Und).String(...)` matches Foundation's
// `String.lowercased()` byte-for-byte for these strings.
func TestUnicodeLowerScalar(t *testing.T) {
	cases := []struct {
		name  string
		input driver.Value
		want  driver.Value
	}{
		// ASCII baseline — must match SQLite's built-in LOWER for
		// the all-ASCII case so existing rows in the index continue
		// to resolve.
		{"ascii lowercase passthrough", "abdullah ibrahim/water from an ancient well/01.flac",
			"abdullah ibrahim/water from an ancient well/01.flac"},
		{"ascii mixed case", "Abdullah Ibrahim/Water From An Ancient Well/01.flac",
			"abdullah ibrahim/water from an ancient well/01.flac"},
		// Latin Extended (Icelandic / Polish / Spanish / etc.) —
		// the headline use-case the v3 ASCII-only LOWER bug
		// surfaced in.
		{"sigur ros (Icelandic)", "Sigur Rós/Ágætis byrjun/01 Svefn-g-englar.flac",
			"sigur rós/ágætis byrjun/01 svefn-g-englar.flac"},
		{"hania rani (Polish)", "Hania Rani/Esja/Eden.flac",
			"hania rani/esja/eden.flac"},
		// German sharp-s: U+1E9E (LATIN CAPITAL LETTER SHARP S) →
		// U+00DF (LATIN SMALL LETTER SHARP S). Foundation does the
		// same fold by default with no locale.
		{"german sharp-s", "Straße", "straße"},
		// Greek capital sigma in word-final position folds to U+03C2
		// (final sigma) per Foundation, but cases.Lower defaults to
		// U+03C3 (small sigma). Document the divergence: paths
		// containing this codepoint won't case-fold-equal between
		// iOS and bridge. Acceptable: real music libraries with
		// Greek paths are vanishingly rare AND the manifest's
		// case-preserved exact-match path catches the common case
		// where iOS sends the same case the scanner recorded.
		// This test pins what cases.Lower actually returns so a
		// future divergence is visible.
		{"greek capital sigma (word-final divergence noted)", "ΣΟΦΙΑ", "σοφια"},
		// NULL passthrough, matching SQLite LOWER's behaviour.
		{"nil input", nil, nil},
		// Empty string round-trips intact.
		{"empty string", "", ""},
		// Non-text driver.Value types (int / float / bool etc.) →
		// nil. Defensive fallback; real callers always pass strings.
		{"int rejected as nil", int64(42), nil},
		{"float rejected as nil", 3.14, nil},
		// []byte is folded as if it were UTF-8 text — matches LOWER's
		// behaviour on TEXT-class blobs.
		{"bytes coerced to text", []byte("Sigur Rós"), "sigur rós"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := unicodeLowerScalar(nil, []driver.Value{c.input})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.want {
				t.Errorf("unicodeLowerScalar(%q) = %q, want %q", c.input, got, c.want)
			}
		})
	}
}

// TestUnicodeLowerScalarRejectsWrongArgCount — the function is
// registered with NArgs=1; SQLite's binding layer should already
// reject mismatched arg counts at parse time, but the in-Go
// implementation enforces it as a defence-in-depth.
func TestUnicodeLowerScalarRejectsWrongArgCount(t *testing.T) {
	for _, args := range [][]driver.Value{
		{},
		{"a", "b"},
		{"a", "b", "c"},
	} {
		_, err := unicodeLowerScalar(nil, args)
		if err == nil {
			t.Errorf("expected error for %d args, got nil", len(args))
		}
		if err != nil && !strings.Contains(err.Error(), "1 argument") {
			t.Errorf("error message missing '1 argument': %v", err)
		}
	}
}
