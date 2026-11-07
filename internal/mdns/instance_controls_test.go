package mdns

import (
	"testing"
	"unicode/utf8"
)

// TestSanitizeInstanceStripsEveryControlRune is B744's item 2 at the Bonjour
// instance: sanitizeInstance stripped the C0 controls and DEL and kept the C1
// ones (U+0080 to U+009F), which a library name could hold. It strips every
// control rune now, through the one function the library name's entries
// strip by, read as runes after decoding: the second bytes of Ā (C4 80) and
// š (C5 A1) are C1's numbers and stay, and a byte that is not UTF-8 is still
// U+FFFD.
func TestSanitizeInstanceStripsEveryControlRune(t *testing.T) {
	const nel, csi, pu1 = "\xc2\x85", "\xc2\x9b", "\xc2\x91"
	for _, c := range []struct{ in, want string }{
		{"Living" + nel + "Room", "LivingRoom"},
		{pu1 + "Kitchen" + csi, "Kitchen"},
		{"Control\x01Chars\x7F", "ControlChars"},
		{"Ā Šoštarić", "Ā Šoštarić"},
		{"Caf\xe9" + nel, "Caf" + string(utf8.RuneError)},
		{"With.Dots" + nel, "WithDots"},
	} {
		if got := sanitizeInstance(c.in); got != c.want {
			t.Errorf("sanitizeInstance(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
