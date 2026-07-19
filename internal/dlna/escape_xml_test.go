package dlna

import "testing"

// TestEscapeXMLText_StripsControlCharsAndEscapes is the B18 regression guard:
// XML-1.0-illegal C0 control bytes (all < 0x20 except tab/LF/CR) must be dropped
// so one stray control char in a tag can't break the whole Browse response;
// the five metacharacters are still escaped, and tab/LF/CR survive.
func TestEscapeXMLText_StripsControlCharsAndEscapes(t *testing.T) {
	cases := map[string]string{
		"":              "",
		"clean":         "clean",
		"a&b<c>":        "a&amp;b&lt;c&gt;",
		"tab\ttab":      "tab\ttab", // 0x09 allowed
		"nl\nnl":        "nl\nnl",   // 0x0A allowed
		"cr\rcr":        "cr\rcr",   // 0x0D allowed
		"form\x0cfeed":  "formfeed", // 0x0C dropped
		"back\x08space": "backspace",
		"nul\x00byte":   "nulbyte",
		"x\x1fy&z":      "xy&amp;z", // control dropped + metachar escaped
	}
	for in, want := range cases {
		if got := escapeXMLText(in); got != want {
			t.Errorf("escapeXMLText(%q) = %q, want %q", in, got, want)
		}
	}
}
