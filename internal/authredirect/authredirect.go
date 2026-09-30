// Package authredirect keeps a request's Authorization header off a
// redirect hop that leaves https: the one guard every client that sends a
// credential is built over, the enrich clients (a mirror's Basic credential,
// the Atlas premium cover fetch's bearer token) and the Atlas harvest client
// (its bulk_harvest bearer token).
//
// It was internal/enrich's guardRedirects (backlog B69, #1091) and moved here
// so internal/atlasharvest, which must not import enrich, uses the same guard
// rather than a copy (backlog B133).
package authredirect

import (
	"errors"
	"net/http"
)

// redirectLimit is net/http's own bound on a redirect chain: a Client with no
// CheckRedirect stops after this many consecutive requests. A Client that
// sets a policy takes that rule over too, so Guard restates it.
const redirectLimit = 10

// Guard returns a shallow copy of hc that never carries an Authorization
// header from an https request onto a redirect hop that is not https.
//
// net/http copies an explicit Authorization header onto a redirect to the same
// host, and shouldCopyHeaderOnRedirect compares host names only: not the
// scheme and not the port. So an https server that answers with a redirect to
// http://<the same host>/… is sent the credential in cleartext. The header is
// copied from the FIRST request onto every hop, so this is judged per hop
// against that request's scheme: a cleartext hop that redirects again,
// relative to itself, would otherwise have it back.
//
// The header is stripped and the redirect followed, never refused. A request
// with no credential has nothing to strip and follows a downgrade exactly as
// it always has, where a refusal would change that. For a request that does
// carry one, stripping is what the same redirect did when the credential
// travelled in the URL, and the server's own answer at the plain hop is the
// report (a 401, or the resource where it serves it without one). The
// credential still goes to the same host and its subdomains over https, and
// never to another domain (net/http's rule, untouched).
//
// A request that starts on plain http is left alone: its operator wrote a
// cleartext base, and the first request carried the credential already.
//
// The caller's client is copied, never written to: *http.Client values are
// shared, and a guard installed on one (http.DefaultClient, say) would apply
// to every redirect in the process. A CheckRedirect the caller set is still
// asked, after the header is dropped, and is the one that decides. With none,
// net/http's own limit of ten applies, because a Client that sets a policy
// loses the default.
func Guard(hc *http.Client) *http.Client {
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
