// Package baseurl holds the reductions a base URL goes through before the
// bridge compares it with another or holds a credential against it: the one
// definition the configuration, the harvest credential endpoint and the
// harvest state store share.
//
// It exists so that those three agree by construction. The Atlas harvest
// base reaches the bridge three ways: the operator's pin
// (`atlas.harvestBaseUrl`, reduced by internal/config), the base a paired
// app sends with a credential (reduced by internal/api), and the base the
// state file holds (read back by internal/atlasharvest, which imports
// neither of the other two). A reduction each of them spelled out itself
// would drift: the handler once built `u.Scheme + "://" + u.Host` by hand,
// which is how the `:443` case in CanonicalHTTPS went missing, and the state
// store trusted whatever the file held until backlog B97, so a base edited
// by hand to carry user information reached every request error of the
// harvest client.
package baseurl

import (
	"net/url"
	"strings"
)

// CanonicalHTTPS reduces a plain https base URL to `scheme://host`, or
// returns "" when it is empty, unparseable, not https, host-less, or carries
// userinfo / path / query / fragment.
//
// **Both sides of the harvest pin MUST go through this one function.** The
// config value and the wire value are compared for equality, so any reduction
// applied to one and not the other silently turns a correct pin into a
// mismatch — which fails CLOSED (the operator's own bootstrap is refused) and
// therefore looks like a broken feature rather than a broken comparison. The
// handler used to build `u.Scheme + "://" + u.Host` itself; that duplication
// is exactly how the `:443` case below got missed.
//
// The default port is stripped so `https://host:443` and `https://host` are
// the same pin — they address the same endpoint, and an operator may write
// either (gemini-code-assist on PR #724). So is an empty one: `https://host:`
// names no port, which net/http dials on the default, and it was a third
// spelling of the same endpoint until backlog B97.
//
// "Host-less" here means an empty url.URL.Host. A base naming a port and no
// host (`https://:8443`) reduces to itself, and deliberately so: reducing it
// to "" would turn a pin written that way into "unpinned" (or, through
// config's Validate, stop the bridge from starting). What names no host is
// refused where a credential would be held against it instead
// (CredentialBase), so a pin written that way matches no credential, and
// config's Normalize warns about it.
//
// **The result is a fixed point**: reducing it again answers it again. The
// pin is reduced twice on its way to the comparison (config's
// CanonicalHarvestBaseURL, then the handler's WithAtlasHarvest, which does
// not trust its caller), and the harvest state store reduces what it loads
// at every open. So a port is stripped only from a host it leaves something
// of: `https://:443` stays itself, a port and no host like `https://:8443`.
// Stripped, it was `https://`, which reduces to "", so a pin
// written `https://:443` reached the handler as "" and left a non-demo
// bridge unpinned, taking a credential for any host (found by the
// fixed-point check in TestTheReductions, backlog B97).
func CanonicalHTTPS(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" ||
		u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		(u.Path != "" && u.Path != "/") {
		return ""
	}
	host := strings.TrimSuffix(strings.TrimSuffix(u.Host, ":"), ":443")
	if host == "" {
		host = u.Host
	}
	return u.Scheme + "://" + host
}

// NamesHost reports whether raw parses as a URL whose host names a machine:
// url.URL.Hostname is not empty. A URL naming a port and no host
// (`https://:8443`, `http://:5000`) has a Host and no hostname, and Go's
// client dials such a URL on THIS machine (measured in #1069), so a check of
// url.URL.Host passes a value that leads to the bridge's own ports. The
// harvest credential endpoint and the harvest state store refuse a base that
// names no host (CredentialBase), and config's Normalize warns about a
// configured one, which it cannot refuse: the value loaded before, and a
// refusal would stop the bridge from starting after an update (backlog B36).
func NamesHost(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	return err == nil && u.Hostname() != ""
}

// CredentialBase is the base a harvest credential may be held against: raw
// reduced by CanonicalHTTPS, when that reduction names a host, and "" for
// anything else. It is the one form POST /v1/atlas-harvest/credential stores
// and the one form the harvest state store holds (backlog B97), so the
// harvest client, the booklet fetch, the lyrics tier and the premium cover
// fetch, which build every request URL from it, see a base that carries no
// user information, path, query or fragment, never plain http, and never a
// port with no host.
//
// Why each refusal matters to a request built from it: user information
// reaches every error net/http returns (its `*url.Error` names the request
// URL, masking a password and nothing else, so a token written as the user
// name is quoted whole); a path moves every request under it, and a query or
// a fragment swallows the path the client appends; plain http sends the
// bearer token in cleartext; and a port with no host is dialled on this
// machine.
func CredentialBase(raw string) string {
	base := CanonicalHTTPS(raw)
	if base == "" || !NamesHost(base) {
		return ""
	}
	return base
}
