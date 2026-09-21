package doctor

import (
	"context"
	"fmt"
	"strings"

	servertls "github.com/acoseac/1-bit-bridge/internal/tls"
)

const checkNameTLSCertSANs = "tls-cert-sans"

// checkTLSCertSANs reports names and addresses this bridge advertises
// that its certificate does not cover.
//
// The state it catches: a data directory carried to another host keeps
// the old host's `server.crt`, whose SANs name the machine it was
// minted on. Every endpoint the new host advertises and the old cert
// does not carry fails TLS — `/v1/health` withholds them, the
// Tailscale and custom-endpoint URLs in the pairing QR do not
// handshake — and nothing says so until `bridge serve` is already up
// and has warned once into a log. The fix (`bridge cert rotate`,
// restart, re-pair) is cheap BEFORE devices have pinned the old cert
// and much less so after, which is the whole reason this belongs in a
// preflight rather than only in the startup path.
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
		// to escalate — this one is both unactionable and mostly moot
		// on a hosted bridge: the control plane owns rotation, and
		// clients reach the tenant over its autocert domain, whose
		// Let's Encrypt cert the SNI switcher serves instead of this
		// one. A warning naming `bridge cert rotate` to a reader with
		// no shell is the unactionable-preflight shape Deps.Managed
		// exists to avoid.
		return ok(checkNameTLSCertSANs, "certificate is the host's — check skipped")
	}
	if d.CertSANs == nil {
		// No readable config means no customEndpoints to gather, so the
		// want-set would be narrower than the one `bridge serve` builds
		// and "covered" would be a claim about a comparison that was
		// never made.
		return ok(checkNameTLSCertSANs, "no config to compare against (re-run with --config, or after `bridge init`)")
	}
	certPath, keyPath := certPaths(d)
	if certPath == "" {
		return ok(checkNameTLSCertSANs, "no data dir set — no certificate to compare")
	}
	if !fileExists(certPath) || !fileExists(keyPath) {
		// Absent or half-present: checkTLSCert already reports both, and
		// a mint picks up whatever the host advertises now, so there is
		// nothing stale to find.
		return ok(checkNameTLSCertSANs, "no certificate yet — the first mint covers the current endpoints")
	}
	cov, err := servertls.InspectSANCoverage(certPath, d.CertSANs(ctx))
	if err != nil {
		// "Could not read" is not "fine" — checkTLSCert reports the
		// unreadable cert itself; this one says what it could not
		// answer rather than answering ok.
		return warn(checkNameTLSCertSANs, "could not read the certificate",
			fmt.Sprintf("%s: %v", certPath, err))
	}
	if cov.Covered() {
		return ok(checkNameTLSCertSANs, fmt.Sprintf("covers all %d name(s) and %d address(es) this bridge advertises",
			len(cov.WantDNS), len(cov.WantIPs)))
	}
	return warn(checkNameTLSCertSANs, sansSummary(cov), sansHint(cov))
}

// sansSummary is the one-line verdict: how much of the advertised set
// is uncovered, counted rather than listed (the hint carries the list).
func sansSummary(cov servertls.SANCoverage) string {
	parts := make([]string, 0, 2)
	if n := len(cov.MissingDNS); n > 0 {
		parts = append(parts, fmt.Sprintf("%d of %d name(s)", n, len(cov.WantDNS)))
	}
	if n := len(cov.MissingIPs); n > 0 {
		parts = append(parts, fmt.Sprintf("%d of %d address(es)", n, len(cov.WantIPs)))
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
