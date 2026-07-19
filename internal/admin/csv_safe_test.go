package admin

import "testing"

// csvSafe must neutralize a leading formula/control character (CSV formula
// injection, CWE-1236) while leaving ordinary values — including dates and
// interior operators — untouched.
func TestCSVSafe(t *testing.T) {
	cases := map[string]string{
		"":                     "",
		"normal":               "normal",
		"FLAC":                 "FLAC",
		"2024-01-02T03:04:05Z": "2024-01-02T03:04:05Z", // starts with a digit
		"a=b":                  "a=b",                  // '=' not leading
		"=HYPERLINK(\"x\")":    "'=HYPERLINK(\"x\")",
		"+cmd":                 "'+cmd",
		"-1+2":                 "'-1+2",
		"@SUM(A1)":             "'@SUM(A1)",
		"\tlead-tab":           "'\tlead-tab",
		"\rlead-cr":            "'\rlead-cr",
		"\nlead-lf":            "'\nlead-lf", // leading LF (Unix line-injection)
	}
	for in, want := range cases {
		if got := csvSafe(in); got != want {
			t.Errorf("csvSafe(%q) = %q, want %q", in, got, want)
		}
	}
}
