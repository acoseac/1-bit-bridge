package main

import (
	"net"
	"strings"

	"github.com/acoseac/1-bit-bridge/internal/config"
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

// operatorAdminURL is the single source of truth for the
// "where do I point my browser?" string surfaced to operators
// after `bridge init` and at the top of `bridge serve` output.
//
// Three-branch dispatch keeps `bridge init`'s footer in lockstep
// with `bridge serve`'s banner — pre-fix `init.go` hardcoded
// `http://<adminAddress>/` (e.g. `http://0.0.0.0:7789/`) which
// is dial-broken from any browser on a fresh VPS install and
// gave the operator the wrong impression of where admin lives.
// Post-fix both surfaces converge on the same URL for the same
// posture. CodeRabbit/Gemini followup post-PR-#296.
//
//   - loopback (no public mode): plain http on the configured
//     bind address — same as the historical pre-public banner.
//   - public + reverse-proxy: https://<domain>/ — the proxy
//     fronts admin at the canonical URL on the operator's
//     domain. The bridge can't know the proxy's external port
//     mapping, so we surface the conventional 443 form and trust
//     the operator to know if their proxy uses a non-default
//     port.
//   - public + autocert-direct-TLS: https://<domain>[:port]/ —
//     port suffix omitted for :443 (the https default) so a
//     ":443" bind reads cleanly.
//
// adminScheme parameter is the **loopback-only** override slot —
// the public-mode branches force https unconditionally (autocert
// direct-TLS terminates TLS in-process; reverse-proxy fronts TLS
// externally — both are https-on-the-wire from the operator's
// browser). For loopback installs pass "http" (or "" to default
// to http); the historical-shape `http://<adminAddress>/` is what
// existing operators expect. Falls back to "http" on empty for
// loopback callers who don't carry a scheme context (e.g.
// init.go's footer, where TLS state isn't yet wired).
func operatorAdminURL(cfg *config.Config, adminScheme string) string {
	if cfg == nil {
		if adminScheme == "" {
			adminScheme = "https"
		}
		return adminScheme + "://"
	}
	if cfg.IsPublic() {
		if cfg.Deployment.AdminTLSTerminatedByProxy {
			return "https://" + strings.TrimSuffix(cfg.Autocert.Domain, ".") + "/"
		}
		// Public direct-TLS: scheme is always https, regardless of
		// the caller's adminScheme hint. The hint is a loopback
		// concern only — callers from init.go pass "http" as a
		// historical default, which would otherwise downgrade the
		// public-mode URL silently.
		return banneradminURL(cfg.Autocert.Domain, cfg.AdminAddress, "https")
	}
	if adminScheme == "" {
		adminScheme = "http"
	}
	return adminScheme + "://" + cfg.AdminAddress + "/"
}
