package manifest

import (
	"database/sql/driver"
	"strings"
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

// TestRenameFoldMatchesPathFold holds the deletion pass's compose-first
// key to the date map's fold, and holds U+0130 apart from a plain "i".
// strings.ToLower applied before NFC splits the dotted pair; applied
// after NFC it joins that pair and also joins it to a plain "i".
func TestRenameFoldMatchesPathFold(t *testing.T) {
	pairs := [][2]string{
		{"Caf\u00e9/song.flac", "Cafe\u0301/song.flac"},
		{"İstanbul/song.flac", "I\u0307stanbul/song.flac"},
	}
	for _, p := range pairs {
		if renameFold(p[0]) != renameFold(p[1]) {
			t.Errorf("renameFold split %q and %q", p[0], p[1])
		}
		if renameFold(p[0]) != pathFold(p[0]) || renameFold(p[1]) != pathFold(p[1]) {
			t.Errorf("renameFold diverged from pathFold for %q / %q", p[0], p[1])
		}
	}
	if renameFold("İstanbul/song.flac") == renameFold("istanbul/song.flac") {
		t.Error("renameFold equated U+0130 with a plain i")
	}
	if strings.ToLower("İ") == strings.ToLower("I\u0307") {
		t.Fatal("premise: strings.ToLower already equates the dotted pair")
	}
}
