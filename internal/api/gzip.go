package api

import (
	"net/http"
	"strings"
)

// acceptsGzip parses the request's Accept-Encoding header and returns
// true when gzip is acceptable to the client. Honours `q=0` (explicit
// refusal) per RFC 9110 §12.5.3, treats `*` as accepting gzip, and is
// case-insensitive on the encoding tokens. Missing header → false
// (identity is the documented default per the spec).
//
// Two-pass: first scan for any explicit `gzip` token and record its
// outcome; only fall back to a wildcard if no explicit gzip token is
// present. RFC 9110 §12.5.3: "a more specific reference has precedence
// over a wildcard". Single-pass first-match-wins gets `*, gzip;q=0`
// wrong (returns true, but the explicit gzip refusal must win
// regardless of position).
//
// Deliberately simpler than a full content-negotiation parser: the
// bridge's primary client (iOS URLSession) sends a fixed
// `Accept-Encoding: gzip, deflate, br` shape and the secondary
// surfaces (curl `--compressed`, browser-driven admin XHR) all map
// onto the patterns this handles. A client that sends a quality-
// preference scheme more elaborate than `q=0` gets gzip if the gzip
// token itself isn't refused — same conservative default any HTTP
// proxy applies.
func acceptsGzip(r *http.Request) bool {
	ae := r.Header.Get("Accept-Encoding")
	if ae == "" {
		return false
	}
	var (
		sawGzip          bool
		gzipAccepted     bool
		sawWildcard      bool
		wildcardAccepted bool
	)
	for _, part := range strings.Split(ae, ",") {
		token, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		token = strings.ToLower(strings.TrimSpace(token))
		if token != "gzip" && token != "*" {
			continue
		}
		// Walk the q-params; only `q=0` (in any decimal form sox /
		// curl / URLSession might emit) refuses the encoding. Any
		// other value — present or absent — accepts.
		refused := false
		for _, p := range strings.Split(params, ";") {
			kv := strings.TrimSpace(p)
			if !strings.HasPrefix(strings.ToLower(kv), "q=") {
				continue
			}
			v := strings.TrimSpace(kv[2:])
			// "0", "0.", "0.0", "0.00", "0.000" all == 0 per RFC.
			if v == "0" || v == "0." || v == "0.0" || v == "0.00" || v == "0.000" {
				refused = true
				break
			}
		}
		if token == "gzip" {
			sawGzip = true
			// Multiple gzip tokens are malformed per spec; preserve
			// the prior "any non-refused gzip token rescues" shape so
			// `gzip;q=0, gzip` continues to accept.
			if !refused {
				gzipAccepted = true
			}
		} else {
			sawWildcard = true
			if !refused {
				wildcardAccepted = true
			}
		}
	}
	if sawGzip {
		return gzipAccepted
	}
	return sawWildcard && wildcardAccepted
}
