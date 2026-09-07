package transcode

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestFirstLine_TruncatesByRuneNotByte: the probe's error excerpt is cut
// at 80 CHARACTERS, never 80 bytes — a byte cut can land inside a
// multi-byte UTF-8 sequence and hand the operator an invalid string.
func TestFirstLine_TruncatesByRuneNotByte(t *testing.T) {
	// A CJK character: always ONE code point, three UTF-8 bytes, so the
	// fixture cannot be silently NFD-decomposed into two runes by an
	// editor or a heredoc the way an accented Latin letter can (that is
	// how the first draft of this test failed against a correct helper).
	long := strings.Repeat("日", 100) + "\nsecond line"
	got := firstLine(long)
	if !utf8.ValidString(got) {
		t.Fatalf("firstLine produced invalid UTF-8: %q", got)
	}
	if want := strings.Repeat("日", 80) + "…"; got != want {
		t.Errorf("firstLine = %q, want the first 80 runes + an ellipsis", got)
	}
	// Outer whitespace is trimmed and the cut is at the first newline;
	// a short first line comes back whole.
	if got := firstLine("  short\nrest"); got != "short" {
		t.Errorf("firstLine(short) = %q, want the first line untouched", got)
	}
	if got := firstLine(""); got != "" {
		t.Errorf("firstLine(\"\") = %q, want empty", got)
	}
}
