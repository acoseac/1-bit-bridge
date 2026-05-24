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
	return true
}
