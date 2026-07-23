package adminauth

import "testing"

// TestIsSafeRelativePath_RejectsControlCharacters pins the WHATWG
// strip-list bypass.
//
// Browsers remove ASCII tab, CR and LF from a URL BEFORE parsing its
// scheme/authority, so a target that looks same-origin here can become
// protocol-relative in the browser: "/\t//evil.example" is fetched as
// "//evil.example". None of the pre-existing checks catch it — the
// string starts "/<TAB>" (not "//"), and carries no colon or backslash.
//
// The tab case is the live one: Go's net/http rewrites CR and LF in a
// header value to spaces, but passes tab through untouched.
func TestIsSafeRelativePath_RejectsControlCharacters(t *testing.T) {
	bypasses := []struct{ name, in string }{
		{"tab_then_protocol_relative", "/\t//evil.example"},
		{"cr_then_protocol_relative", "/\r//evil.example"},
		{"lf_then_protocol_relative", "/\n//evil.example"},
		{"tab_before_scheme", "/\thttps://evil.example"},
		{"nul_byte", "/admin\x00/../../evil"},
		{"vertical_tab", "/\v//evil.example"},
		{"form_feed", "/\f//evil.example"},
		{"del", "/\x7f//evil.example"},
		{"trailing_tab", "/devices\t"},
	}
	for _, tc := range bypasses {
		t.Run(tc.name, func(t *testing.T) {
			if IsSafeRelativePath(tc.in) {
				t.Errorf("IsSafeRelativePath(%q) = true, want false — "+
					"a browser may strip the control character and follow it off-origin", tc.in)
			}
		})
	}
}

// TestIsSafeRelativePath_AcceptsRealTargets is the companion guard: the
// control-character rejection must not start refusing the paths the
// admin console actually redirects to.
func TestIsSafeRelativePath_AcceptsRealTargets(t *testing.T) {
	ok := []string{
		"/",
		"/devices",
		"/library",
		"/settings",
		"/library?tab=inspector",
		"/library/Some%20Album",
		"/devices#pairing",
		"/library/Björk",           // non-ASCII is fine — not a control char
		"/library/Album%20(2019)/", // trailing slash, parens
	}
	for _, s := range ok {
		if !IsSafeRelativePath(s) {
			t.Errorf("IsSafeRelativePath(%q) = false, want true — legitimate target rejected", s)
		}
	}
}
