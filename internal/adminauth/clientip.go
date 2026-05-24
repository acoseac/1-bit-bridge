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
// When trustForwardedHeaders is true:
//   - X-Forwarded-For: split on ",", trim whitespace, take the
//     left-most non-empty + net.ParseIP-valid element. The
//     left-most is the original client (Caddy/nginx default).
//     Reject invalid IP strings — `X-Forwarded-For: garbage, 1.2.3.4`
//     would otherwise key the bucket on "garbage".
//   - X-Real-IP: single value, validated via net.ParseIP.
//   - Fall through to r.RemoteAddr only when both headers are
//     absent / invalid.
//
// Documented proxy-config expectation: operator's Caddy/nginx MUST
// strip client-supplied X-Forwarded-For / X-Real-IP on ingress
// before injecting its own (Caddy default; nginx requires explicit
// `proxy_set_header X-Forwarded-For $remote_addr` — the
// append-default form would let the client poison the chain).
func ExtractClientIP(r *http.Request, trustForwardedHeaders bool) string {
	if trustForwardedHeaders {
		if ip := firstValidForwardedFor(r.Header.Get("X-Forwarded-For")); ip != "" {
			return ip
		}
		if ip := strings.TrimSpace(r.Header.Get("X-Real-IP")); ip != "" {
			if net.ParseIP(ip) != nil {
				return ip
			}
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

// firstValidForwardedFor returns the left-most non-empty,
// net.ParseIP-valid element of an X-Forwarded-For header value.
// Empty string if none found.
func firstValidForwardedFor(raw string) string {
	if raw == "" {
		return ""
	}
	for _, part := range strings.Split(raw, ",") {
		candidate := strings.TrimSpace(part)
		if candidate == "" {
			continue
		}
		if net.ParseIP(candidate) != nil {
			return candidate
		}
	}
	return ""
}
