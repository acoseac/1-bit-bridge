package manifest

import (
	"database/sql/driver"
	"testing"
)

// TestUnicodeLowerScalar_NFCComposes pins the M9 fix (2026-07-21
// review) at the function boundary: unicode_lower's output is the
// NFC composition of the case fold, so an NFD-stored path and iOS's
// NFC-normalised request shape land on the same byte sequence. The
// store-level contract is covered end-to-end in
// store_lookup_case_test.go; this table guards the edge inputs
// (already-NFC, ASCII passthrough, ill-formed UTF-8) that don't need
// a database.
func TestUnicodeLowerScalar_NFCComposes(t *testing.T) {
	cases := []struct {
		name string
		in   driver.Value
		want driver.Value
	}{
		{"NFD accented input composes",
			"Sigur Ro\u0301s/A\u0301gætis byrjun", "sigur rós/ágætis byrjun"},
		{"NFC input unchanged beyond the case fold",
			"Sigur Rós", "sigur rós"},
		{"ASCII lowercase passthrough",
			"Abdullah Ibrahim", "abdullah ibrahim"},
		{"nil passes through", nil, nil},
		{"non-text input folds to nil (LOWER compat)", 42, nil},
		{"ill-formed UTF-8 falls back to the uncomposed fold",
			"abc\xffCafé", "abc\xffcafé"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := unicodeLowerScalar(nil, []driver.Value{tc.in})
			if err != nil {
				t.Fatalf("unicodeLowerScalar(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("unicodeLowerScalar(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
