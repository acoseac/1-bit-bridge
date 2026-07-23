package adminauth

import "strings"

// IsSafeRelativePath returns true if s is a same-origin relative
// path safe to use as a post-login redirect target. Rejects every
// shape that could be coerced into an off-origin URL by a browser:
//
//   - "//attacker.com" — protocol-relative URL; browsers treat
//     `Location: //evil.example` as a navigation to evil.example
//     on the same scheme. Caught by the `!HasPrefix(s, "//")`
//     check.
//   - "/\\attacker.com" — Windows path coercion. Some legacy
//     browsers normalised backslashes to slashes before parsing
//     the URL; refuse defensively. Caught by `!HasPrefix(s, "/\\")`
//     and the `!ContainsAny(s, "\\")` general guard.
//   - "https://attacker.com" — explicit scheme. Caught by
//     `HasPrefix(s, "/")` (an absolute URL starts with the scheme,
//     not a slash) AND `!ContainsAny(s, ":")` (defense in depth
//     against any single-leading-slash bypass that happens to
//     include a colon).
//   - "/\t//attacker.com" — control-character smuggling. The WHATWG
//     URL Standard has browsers STRIP tab, CR and LF from a URL
//     before parsing the scheme/authority, so a target that is
//     merely `/`-prefixed here becomes protocol-relative in the
//     browser: `/<TAB>//evil.example` is fetched as
//     `//evil.example`. The prefix checks above can't see it (the
//     string starts `/<TAB>`, not `//`) and no colon or backslash is
//     present, so this needs its own rejection. Note Go's net/http
//     rewrites CR/LF in a header value to spaces, but passes TAB
//     through untouched — so the tab is the live vector and the
//     other two are defense in depth.
//
// Empty string returns false — the handler should fall through to
// the default landing page instead of redirecting to "" (which
// some routers interpret as "stay where you are" and some as
// "redirect to root"; ambiguous behaviour).
func IsSafeRelativePath(s string) bool {
	if s == "" {
		return false
	}
	if !strings.HasPrefix(s, "/") {
		return false
	}
	if strings.HasPrefix(s, "//") {
		return false
	}
	if strings.HasPrefix(s, "/\\") {
		return false
	}
	if strings.ContainsAny(s, ":\\") {
		return false
	}
	// Reject ANY ASCII control character (0x00-0x1F, 0x7F), not just the
	// three the WHATWG strip-list names. A legitimate post-login redirect
	// target is a path the admin console itself produced — none contain
	// control bytes — so a blanket refusal costs nothing and doesn't
	// depend on tracking which control characters a given browser
	// generation happens to strip.
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] == 0x7F {
			return false
		}
	}
	return true
}
