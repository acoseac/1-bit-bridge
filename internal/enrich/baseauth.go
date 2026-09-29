package enrich

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
)

// errBaseNotUsable is what a request built from an unusable base answers. It
// names no part of the base: the value may carry a credential, and net/url's
// own error for it (`parse "…": …`) quotes it whole.
var errBaseNotUsable = errors.New("enrich: the configured base URL is not an absolute http(s) URL with a host")

// baseEndpoint is a configured base URL (`enrich.musicbrainzBaseURL`,
// `enrich.coverArtBaseURL`) cut in two: the ROOT every request URL is built
// from, and the USER INFORMATION the operator wrote into the base, which may
// only travel as an Authorization header (backlog B69).
//
// A mirror behind an auth proxy is configured as `https://user:password@mirror`
// or with a token written as the user name (`https://TOKEN@mirror`), and
// net/http sends a URL's user information as Basic auth. It also names the
// request URL in every error it returns, through a `stripPassword` that masks
// a password and nothing else, so a token written as the user name reached
// the journal whole in every transport failure: the enricher logs those
// errors (`MB search`, `artwork`, `MB artist search`, `release-group
// lookup`) and puts them in the `enrichment skipped` line's detail. No
// RoundTripper can prevent that, because the client builds the error from
// the request's own URL. So the user information never enters a request URL:
// root has none, and newRequest sends it with SetBasicAuth, the same header
// net/http built from the URL (the user alone as `user:`).
//
// An unusable base (not absolute, not http(s), no host) is an err, and every
// request built from it answers errBaseNotUsable, so no error out of these
// clients quotes a base. The configuration refuses such a value
// (config.normalizeBaseURL), which makes this a backstop.
type baseEndpoint struct {
	root string
	user *url.Userinfo
	err  error
}

// parseBaseEndpoint cuts raw into its root and its user information. The root
// ends in no slash, because newRequest joins a path that begins with one: a
// base written `https://mirror/ws/2/` would otherwise request
// `…/ws/2//release/…`, which a strict mirror answers 404. Config trims it
// already, and a live value is trimmed by the client that reads it; the
// constructed base and the premium fetch's stored one reach this without.
// A base with no user information keeps its own bytes as the root, less
// that. One with some is rebuilt without it (url.URL.String), which changes
// nothing else in it but the scheme's case.
func parseBaseEndpoint(raw string) baseEndpoint {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return baseEndpoint{err: errBaseNotUsable}
	}
	if u.User == nil {
		return baseEndpoint{root: raw}
	}
	user := u.User
	u.User = nil
	return baseEndpoint{root: u.String(), user: user}
}

// newRequest builds the GET for path, which follows the root, and gives it
// the Basic credential the base carried. The one place a request is built
// from a configured base: what a caller adds after this (a Bearer token
// replaces the Basic header) is its own.
//
// The credential follows a redirect as net/http's own rule has it for an
// explicit Authorization header: to the same host and its subdomains, and
// never to another domain. A redirected request carried the user information
// only when its Location was relative before.
func (b baseEndpoint) newRequest(ctx context.Context, path string) (*http.Request, error) {
	if b.err != nil {
		return nil, b.err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.root+path, nil)
	if err != nil {
		return nil, err
	}
	if b.user != nil {
		password, _ := b.user.Password()
		req.SetBasicAuth(b.user.Username(), password)
	}
	return req, nil
}
