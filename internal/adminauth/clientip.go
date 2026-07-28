package adminauth

import (
	"net"
	"net/http"
	"strings"
)

// ExtractClientIP returns the client IP for rate-limit and audit
// purposes. The trustForwardedHeaders flag MUST come from the
// operator's explicit `deployment.adminTLSTerminatedByProxy: true`
// — never default it to true, never sniff it from header presence:
// a direct attacker could otherwise forge X-Forwarded-For to
// arbitrary values and key the rate-limit bucket onto an
// uninvolved third party (defeating the limiter for everyone).
//
// Returns "" only when r.RemoteAddr itself is unparseable, which
// shouldn't happen for an HTTP request that reached a handler.
// Callers should treat an empty return as "use a fallback key" or
// simply log + skip (don't crash the request).
//
// When trustForwardedHeaders is true, header preference order:
//
//  1. **X-Real-IP** — preferred. Caddy and nginx both set this to
//     a single value: the immediate connecting client's IP, fresh
//     on every request, overwriting any client-supplied value.
//     If the proxy is correctly configured this is the most
//     spoof-resistant signal.
//  2. **X-Forwarded-For, right-most valid element** — fallback.
//     XFF is a comma-separated chain where each proxy APPENDS the
//     IP it saw on connect. The RIGHT-most entry is therefore the
//     IP your trusted proxy saw (== the true client in a one-hop
//     setup). The left-most is whatever the client claimed,
//     including arbitrary attacker-forged values (Gemini High
//     security review on PR #290). Taking the right-most defeats
//     header injection regardless of whether the proxy strips
//     client-supplied XFF on ingress.
//  3. **r.RemoteAddr** — when neither header is set / valid.
//
// Documented proxy-config expectation: operator's Caddy/nginx MUST
// set X-Real-IP to the connecting client's IP on ingress (Caddy
// default; nginx requires `proxy_set_header X-Real-IP $remote_addr`).
// XFF append-or-overwrite either way works under the right-most
// strategy.
//
// Single-proxy assumption: this code is correct for the common
// `Caddy → bridge` and `nginx → bridge` shapes. Operators chaining
// multiple proxies (CDN → load balancer → bridge) get the
// load-balancer's IP, not the original client; a trusted-proxy-
// count hop wouldn't be hard to add later, but the PR 2 scope
// only documents single-proxy reverse-proxy deployments.
func ExtractClientIP(r *http.Request, trustForwardedHeaders bool) string {
	if trustForwardedHeaders {
		if ip := strings.TrimSpace(r.Header.Get("X-Real-IP")); ip != "" {
			if net.ParseIP(ip) != nil {
				return ip
			}
		}
		// Values, not Get: repeated field lines are stored as separate
		// slice entries and Get returns only index 0, so a proxy that
		// APPENDS a header line rather than comma-appending to the
		// existing one would leave the rightmost-valid walk looking at
		// nothing but the client-supplied first line.
		//
		// Caddy and nginx — the two proxies this file documents — both
		// collapse into a single line, so this is hardening rather than
		// a live hole. HAProxy's `option forwardfor` adds a line, which
		// is the shape that would bite. rightmostValidForwardedFor
		// TrimSpaces each element before net.ParseIP, so joining with a
		// bare comma is safe.
		if ip := rightmostValidForwardedFor(
			strings.Join(r.Header.Values("X-Forwarded-For"), ",")); ip != "" {
			return ip
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		// RemoteAddr could already be host-only on some test
		// shims — return it verbatim if it parses as an IP.
		if net.ParseIP(r.RemoteAddr) != nil {
			return r.RemoteAddr
		}
		return ""
	}
	return host
}

// rightmostValidForwardedFor returns the right-most non-empty,
// net.ParseIP-valid element of an X-Forwarded-For header value.
// Empty string if none found.
//
// Right-most is the spoof-resistant choice — see ExtractClientIP's
// docstring for the threat model. An attacker who can append to
// XFF (the realistic case where they control their own connection
// to the reverse proxy) can spoof the LEFT entries but the proxy
// will append its own (true-client) view as the rightmost.
func rightmostValidForwardedFor(raw string) string {
	if raw == "" {
		return ""
	}
	parts := strings.Split(raw, ",")
	for i := len(parts) - 1; i >= 0; i-- {
		candidate := strings.TrimSpace(parts[i])
		if candidate == "" {
			continue
		}
		if net.ParseIP(candidate) != nil {
			return candidate
		}
	}
	return ""
}
