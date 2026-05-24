package main

import (
	"net"
	"strings"
)

// banneradminURL composes the operator-facing admin console URL
// printed in the public-mode startup banner. Derived from the
// publicly-routable hostname (cfg.Autocert.Domain) rather than
// the admin listener's bind target (0.0.0.0:7789 / 127.0.0.1:7789),
// which is meaningless to anyone browsing from elsewhere.
//
// Port-suffix rules:
//   - admin bind on the magic ":443" (rare for the admin
//     listener; typical for a reverse-proxy install that exposes
//     admin on a path of the same domain): omit the port.
//   - admin bind on any other port: append ":<port>" so the URL
//     dials correctly in the autocert-direct-TLS shape.
//
// Falls back to `https://<domain>/` when the bind address can't
// be split (defensive — public-mode validate already rejects
// malformed adminAddress, but a graceful banner-side fallback
// keeps the operator from seeing an empty URL).
//
// Scheme comes from the caller (https when direct-TLS via
// autocert; http behind a reverse proxy — the proxy upgrades).
//
// CodeRabbit Major review post-PR-#295.
func banneradminURL(domain, adminAddress, scheme string) string {
	// Normalize inputs up front so every return path sees the
	// canonical forms (Gemini medium review post-PR-#296):
	//   - scheme default applies even on the empty-domain
	//     fallback (otherwise `://0.0.0.0:7789/` lands on stdout)
	//   - trailing-dot trim applies uniformly so a FQDN-form
	//     "bridge.example.com." doesn't leak the dot into the
	//     bare-domain path (e.g. when port == 443).
	if scheme == "" {
		scheme = "https"
	}
	domain = strings.TrimSuffix(domain, ".")
	if domain == "" {
		return scheme + "://" + adminAddress + "/"
	}
	_, port, err := net.SplitHostPort(adminAddress)
	if err != nil || port == "" || port == "443" {
		return scheme + "://" + domain + "/"
	}
	return scheme + "://" + domain + ":" + port + "/"
}
