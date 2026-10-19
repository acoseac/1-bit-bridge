package doctor

import (
	"context"
	"fmt"
	"net"
	"strings"

	servertls "github.com/acoseac/1-bit-bridge/internal/tls"
)

const checkNameTLSCertSANs = "tls-cert-sans"

// checkTLSCertSANs reports names and addresses this bridge advertises
// that its certificate does not cover.
//
// The state it catches: a data directory carried to another host keeps
// the old host's `server.crt`, whose SANs name the machine it was
// minted on. Every Tailscale, custom-endpoint and host-name URL the new
// host advertises and the old cert does not carry fails TLS for a
// client that checks the certificate against the URL's host, and
// nothing says so until `bridge serve` is already up and has warned
// once into a log. The fix (`bridge cert rotate`, restart, re-pair) is
// cheap BEFORE devices have pinned the old cert and much less so after,
// which is the whole reason this belongs in a preflight rather than only
// in the startup path.
//
// An IPv6 address of the interface walk is the exception (backlog B457):
// serve does not advertise one a certificate it minted does not name,
// so such an address is not stale. The line stays ok and names it, with
// the rotation that would advertise it. An IPv4 address of the walk is
// advertised whatever the certificate names, so one the certificate
// lacks is stale like any other.
//
// The line also warns when a loopback bridge advertises no LAN IPv4
// address at all (B457's option B, the owner's choice on 2026-10-18): a
// phone on a VPN route cannot use the .local name, and the app has no
// IPv4 address to hand a DLNA renderer, many of which cannot resolve that
// name. That warning needs no certificate, and keeps all the comparison
// said. A public-mode bridge advertises its declared endpoints alone and
// is not asked.
//
// It reuses servertls.InspectSANCoverage — the comparison the startup
// warning makes — against the want-set cmd/bridge builds from the one
// helper every cert-minting path in the binary uses. Neither half is a
// second implementation of the other: a doctor that graded against a
// set `bridge cert rotate` would not mint could only mislead.
//
// Warn, never fail: a stale SAN set is not a reason for `bridge init`
// to refuse to proceed, and on a fresh install it cannot happen at all
// (the first mint covers whatever the host advertises today).
//
// Read-only, like the rest of doctor — it parses the cert and writes
// nothing. `--fix` declines cert rotation deliberately: rotation
// invalidates every paired device's pin, which is an operator decision
// with an iOS device in hand, not a mkdir-class remediation.
func checkTLSCertSANs(ctx context.Context, d Deps) Check {
	if d.Managed {
		// Unlike the expiry half — which is a deadline after which
		// every paired device stops working, and which a tenant needs
		// to escalate — this one is unactionable on a hosted bridge:
		// the control plane owns rotation, and the reader has no shell.
		// The on-disk pair may itself be the Let's Encrypt wildcard the
		// tenant serves. Naming `bridge cert rotate` would tell the
		// reader to replace it. An unmanaged install makes that same
		// decision from the certificate, below.
		return ok(checkNameTLSCertSANs, "certificate is the host's — check skipped")
	}
	if d.CertSANs == nil {
		// No readable config means no customEndpoints to gather, so the
		// want-set would be narrower than the one `bridge serve` builds
		// and "covered" would be a claim about a comparison that was
		// never made.
		return ok(checkNameTLSCertSANs, "no config to compare against (re-run with --config, or after `bridge init`)")
	}
	opts := d.CertSANs(ctx)
	c, cov := gradeCertSANs(d, opts)
	if d.PublicMode || len(advertisedLANIPv4(opts.ExtraIPs, cov.HeldBackIPs)) > 0 {
		return c
	}
	return withNoLANIPv4(c)
}

// gradeCertSANs compares the certificate this bridge serves with the SANs
// opts would mint. cov is the zero SANCoverage when there is no
// certificate to read.
func gradeCertSANs(d Deps, opts servertls.GenerateOptions) (Check, servertls.SANCoverage) {
	certPath, keyPath := certPaths(d)
	if certPath == "" {
		return ok(checkNameTLSCertSANs, "no data dir set — no certificate to compare"), servertls.SANCoverage{}
	}
	if !fileExists(certPath) || !fileExists(keyPath) {
		// Absent or half-present: checkTLSCert already reports both, and
		// a mint picks up whatever the host advertises now, so there is
		// nothing stale to find.
		return ok(checkNameTLSCertSANs, "no certificate yet — the first mint covers the current endpoints"),
			servertls.SANCoverage{}
	}
	cov, err := servertls.InspectSANCoverage(certPath, opts)
	if err != nil {
		// "Could not read" is not "fine" — checkTLSCert reports the
		// unreadable cert itself; this one says what it could not
		// answer rather than answering ok.
		return warn(checkNameTLSCertSANs, "could not read the certificate",
			fmt.Sprintf("%s: %v", certPath, err)), servertls.SANCoverage{}
	}
	if cov.Covered() {
		summary := fmt.Sprintf("covers all %d name(s) and %d address(es) this bridge advertises",
			len(cov.WantDNS), len(cov.WantIPs)-len(cov.HeldBackIPs))
		if len(cov.HeldBackIPs) > 0 {
			summary += ". Not advertised, since the certificate does not name it: " +
				strings.Join(cov.HeldBackIPStrings(), ", ") + ". To advertise it: " + servertls.RotationRemediation
		}
		return ok(checkNameTLSCertSANs, summary), cov
	}
	if !cov.MintedByBridge {
		// The startup warning uses this same field. A CA leaf can leave
		// the host name uncovered (a wildcard matches one leftmost
		// label, and the want-set always adds localhost and the host
		// name) without being a pair `bridge cert rotate` should replace.
		return ok(checkNameTLSCertSANs, "certificate was not minted by this bridge — rotation is not advised"), cov
	}
	return warn(checkNameTLSCertSANs, sansSummary(cov), sansHint(cov)), cov
}

// advertisedLANIPv4 is the private IPv4 addresses among ips, the
// want-set's, that the bridge advertises: every one but an address it
// holds back, which since option B is never an IPv4 one. A private IPv4
// address in the want-set comes from the interface walk or a
// customEndpoints entry; Tailscale's are in 100.64.0.0/10, which is not
// private.
func advertisedLANIPv4(ips, heldBack []net.IP) []net.IP {
	held := make(map[string]bool, len(heldBack))
	for _, ip := range heldBack {
		held[string(ip.To16())] = true
	}
	var out []net.IP
	for _, ip := range ips {
		if ip.To4() != nil && ip.IsPrivate() && !held[string(ip.To16())] {
			out = append(out, ip)
		}
	}
	return out
}

// noLANIPv4Summary leads the line when a loopback bridge advertises no
// LAN IPv4 address.
const noLANIPv4Summary = "no LAN IPv4 address is advertised"

// noLANIPv4Hint says what a missing LAN IPv4 address costs, and how to
// advertise one.
const noLANIPv4Hint = "No private IPv4 address of this host, and none in customEndpoints, is advertised. " +
	"A phone on a VPN route cannot use the .local name, and the app has no IPv4 address to hand " +
	"a DLNA renderer, many of which cannot resolve that name. Connect this host to the LAN over IPv4, " +
	"or add its LAN address to customEndpoints (BRIDGE_CUSTOM_ENDPOINTS in a container)."

// withNoLANIPv4 is the warning a loopback bridge with no LAN IPv4 address
// gets, keeping everything c, the certificate comparison, said.
func withNoLANIPv4(c Check) Check {
	summary := noLANIPv4Summary
	if c.Summary != "" {
		summary += "; " + c.Summary
	}
	hint := noLANIPv4Hint
	if c.Hint != "" {
		hint += " " + c.Hint
	}
	return warn(checkNameTLSCertSANs, summary, hint)
}

// sansSummary is the one-line verdict: how much of the advertised set
// is uncovered, counted rather than listed (the hint carries the list).
func sansSummary(cov servertls.SANCoverage) string {
	parts := make([]string, 0, 2)
	if n := len(cov.MissingDNS); n > 0 {
		parts = append(parts, fmt.Sprintf("%d of %d name(s)", n, len(cov.WantDNS)))
	}
	if n := len(cov.MissingIPs); n > 0 {
		parts = append(parts, fmt.Sprintf("%d of %d address(es)", n, len(cov.WantIPs)-len(cov.HeldBackIPs)))
	}
	return "stale — " + strings.Join(parts, " and ") + " this bridge advertises are not in the certificate"
}

// sansHint names every missing entry. Listed in full rather than
// sampled: the set is bounded by the endpoints one host advertises
// (single digits in practice), and the operator's next question after
// "which" is exactly this list.
func sansHint(cov servertls.SANCoverage) string {
	var b strings.Builder
	b.WriteString("clients dialling ")
	missing := make([]string, 0, len(cov.MissingDNS)+len(cov.MissingIPs))
	missing = append(missing, cov.MissingDNS...)
	missing = append(missing, cov.MissingIPStrings()...)
	b.WriteString(strings.Join(missing, ", "))
	// "clients … fail", always — the subject is the clients, not the
	// entry list, so a singular/plural switch on len(missing) reads as
	// "clients dialling X fails" for the one-entry case, which is the
	// COMMON one (a single endpoint added since the mint).
	b.WriteString(" fail TLS hostname verification. ")
	// Both routes here, because naming only the first sends an operator
	// who just added an endpoint looking for a move that never happened.
	b.WriteString("Either the data directory was moved to a host this certificate was not minted on, ")
	b.WriteString("or an endpoint was added since. ")
	b.WriteString(servertls.RotationRemediation)
	return b.String()
}
