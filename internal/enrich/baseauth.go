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
// never to another domain. That rule compares host names and nothing else, so
// it would also carry the header from an https request onto a plain-http hop
// on the same host; guardRedirects, which every client that sends one is
// built with, withholds it there. A redirected request carried the user
// information only when its Location was relative before.
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

// redirectLimit is net/http's own bound on a redirect chain: a Client with no
// CheckRedirect stops after this many consecutive requests. A Client that
// sets a policy takes that rule over too, so guardRedirects restates it.
const redirectLimit = 10

// guardRedirects returns a shallow copy of hc that never carries an
// Authorization header from an https request onto a redirect hop that is not
// https. Every client that sends a credential to an operator's mirror is
// built over it: the MusicBrainz and Cover Art clients (the Basic header
// newRequest sets) and the Atlas premium cover fetch (its bearer token).
//
// net/http copies an explicit Authorization header onto a redirect to the same
// host, and shouldCopyHeaderOnRedirect compares host names only: not the
// scheme and not the port. So an https mirror that answers with a redirect to
// http://<the same host>/… was sent the credential in cleartext once it moved
// out of the request URL (backlog B69). Before that an absolute Location
// carried no user information, so the plain hop got none, and a relative one
// keeps the scheme it came from. The header is copied from the FIRST request
// onto every hop, so this is judged per hop against that request's scheme: a
// cleartext hop that redirects again, relative to itself, would otherwise
// have it back.
//
// The header is stripped and the redirect followed, never refused. A request
// with no credential (the public MusicBrainz and Cover Art hosts, a mirror
// written without user information) has nothing to strip and follows a
// downgrade exactly as it always has, where a refusal would change that. For a
// request that does carry one, stripping is what the same redirect did before
// the credential left the URL, and the mirror's own answer at the plain hop is
// the report: a 401, which the enricher classifies as the persistent HTTP
// error it is, or the resource where the mirror serves it without one. A
// refusal would reach the enricher as an error IsTransient does not recognise,
// stamped persistent all the same, and say less. The credential still goes to
// the same host and its subdomains over https, and never to another domain
// (net/http's rule, untouched).
//
// A request that starts on plain http is left alone: its operator wrote a
// cleartext base, and the first request carried the credential already.
//
// The caller's client is copied, never written to: *http.Client values are
// shared, and NewDeezerClient's comment names what a guard installed on one
// does to every redirect in the process. A CheckRedirect the caller set is
// still asked, after the header is dropped, and is the one that decides. With
// none, net/http's own limit of ten applies, because a Client that sets a
// policy loses the default.
func guardRedirects(hc *http.Client) *http.Client {
	c := *hc
	prev := c.CheckRedirect
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		dropAuthorizationLeavingHTTPS(req, via)
		if prev != nil {
			return prev(req, via)
		}
		if len(via) >= redirectLimit {
			return errors.New("stopped after 10 redirects")
		}
		return nil
	}
	return &c
}

// dropAuthorizationLeavingHTTPS removes the Authorization header from the hop
// req when the request the redirect chain began with (via[0], whose headers
// net/http copies onto every hop) went to https and this hop does not.
func dropAuthorizationLeavingHTTPS(req *http.Request, via []*http.Request) {
	if len(via) == 0 || via[0].URL.Scheme != "https" || req.URL.Scheme == "https" {
		return
	}
	req.Header.Del("Authorization")
}
