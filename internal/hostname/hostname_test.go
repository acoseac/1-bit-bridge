package hostname

import (
	"strings"
	"testing"
)

// TestEachReportedNameBecomesOneName is backlog B739's table: what ASCII makes
// of each host name B739's entry asks about, and of their neighbours at each
// bound. A plain ASCII name is returned byte for byte (its case and its
// underscore kept, so a name that works today is untouched); a name with
// non-ASCII characters takes its IDNA ASCII form, every label of it 63 bytes
// at most; and anything else has no valid form and is no host name. Every
// name taken is taken again unchanged.
func TestEachReportedNameBecomesOneName(t *testing.T) {
	label63 := strings.Repeat("a", 63)
	name253 := strings.Join([]string{label63, label63, label63, strings.Repeat("a", 61)}, ".")
	for _, tc := range []struct {
		name, reported, want string
		ok                   bool
	}{
		// The names B739's entry asks about.
		{"Cyrillic", "Жук", "xn--f1ai0a", true},
		{"Latin with an umlaut", "müsik", "xn--msik-0ra", true},
		{"a Windows computer name with Japanese in it", "DESKTOP-日本", "xn--desktop--5y4qg6h", true},
		{"a test Mac's kern.hostname, a C1 control inside", "Ð\u0091Ä¯U", "", false},
		{"mixed case and an underscore", "MacBook-Pro_2", "MacBook-Pro_2", true},
		{"a space", "my host", "", false},
		{"an ASCII label of 64 bytes", strings.Repeat("a", 64), "", false},
		{"a Unicode label whose ASCII form is 64 bytes", strings.Repeat("ж", 58), "", false},

		// Their neighbours at each bound.
		{"an ASCII label of 63 bytes", label63, label63, true},
		{"a Unicode label whose ASCII form is 63 bytes", strings.Repeat("ж", 57), "xn--f1" + strings.Repeat("a", 57), true},
		{"a name of 253 bytes", name253, name253, true},
		{"a name of 255 bytes", name253 + "aa", "", false},
		{"macOS's .local name", "MacBook-Pro.local", "MacBook-Pro.local", true},
		{"a fully qualified name", "Box.Corp.example.com", "Box.Corp.example.com", true},
		{"a trailing dot", "box.", "box.", true},
		{"two trailing dots", "box..", "", false},
		{"an empty label", "a..b", "", false},
		{"a leading dot", ".box", "", false},
		{"a Unicode label beside an ASCII one", "box.Жук", "box.xn--f1ai0a", true},
		{"a Unicode name with an empty label", "a..б", "", false},
		{"an underscore beside Cyrillic", "Жук_2", "", false},
		{"a C0 control", "box\x01", "", false},
		{"a tab", "box\t", "", false},
		{"DEL", "box\x7f", "", false},
		{"punctuation a URL's host would take", "a+b", "", false},
		{"bytes that are not UTF-8", "\xff\xfe", "", false},
		{"the empty name", "", "", false},
		{"localhost", "localhost", "localhost", true},
		{"a generated container id", "0123456789ab", "0123456789ab", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ASCII(tc.reported)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("ASCII(%q) = (%q, %v), want (%q, %v)", tc.reported, got, ok, tc.want, tc.ok)
			}
			if !ok {
				return
			}
			if again, ok := ASCII(got); again != got || !ok {
				t.Errorf("ASCII(%q) = (%q, %v): a name it took is not taken again unchanged", got, again, ok)
			}
		})
	}
}
