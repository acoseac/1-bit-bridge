package smartplaylist

import (
	"fmt"
	"testing"
)

// TestFromCamelotRoundTrip pins FromCamelot as the exact inverse of
// ToCamelot for every (keyRoot, mode) pair. Both back the admin coverage
// wheel AND its key-filter deep-link, so a clicked segment must resolve to
// the very rows the wheel counted.
func TestFromCamelotRoundTrip(t *testing.T) {
	for root := 0; root < 12; root++ {
		for _, mode := range []string{"minor", "major"} {
			cam, ok := ToCamelot(root, mode)
			if !ok {
				t.Fatalf("ToCamelot(%d, %q) ok=false", root, mode)
			}
			letter := "B"
			if cam.Minor {
				letter = "A"
			}
			code := fmt.Sprintf("%d%s", cam.Num, letter)
			gotRoot, gotMode, ok := FromCamelot(code)
			if !ok || gotRoot != root || gotMode != mode {
				t.Errorf("FromCamelot(%q) = (%d, %q, %v); want (%d, %q, true)",
					code, gotRoot, gotMode, ok, root, mode)
			}
		}
	}
}

// TestFromCamelotNormalisesInput confirms case + surrounding whitespace are
// tolerated (the deep-link value may arrive lower-cased or padded).
func TestFromCamelotNormalisesInput(t *testing.T) {
	root, mode, ok := FromCamelot("  8a ")
	if !ok || mode != "minor" {
		t.Fatalf(`FromCamelot("  8a ") = (%d, %q, %v); want minor + ok`, root, mode, ok)
	}
	if cam, _ := ToCamelot(root, "minor"); cam.Num != 8 {
		t.Errorf("8A resolved to root %d (minor #%d), want minor #8", root, cam.Num)
	}
}

// TestFromCamelotRejectsBadInput pins the ok=false contract so a malformed
// or out-of-range query param can't be turned into a bogus key filter.
func TestFromCamelotRejectsBadInput(t *testing.T) {
	for _, code := range []string{"", "A", "B", "8", "0A", "13A", "8C", "AB", "8.5A", "-1A", "12"} {
		if root, mode, ok := FromCamelot(code); ok {
			t.Errorf("FromCamelot(%q) = (%d, %q, true); want ok=false", code, root, mode)
		}
	}
}
