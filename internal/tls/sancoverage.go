// SAN coverage: does the on-disk cert carry every name and address this
// bridge advertises?
//
// One computation, two surfaces. `logIfSANsStale` runs it at every
// `bridge serve` start and warns; `bridge doctor`'s `tls-cert-sans`
// check runs it BEFORE the first start and reports. Both ask the same
// question of the same cert with the same want-set, so they cannot
// answer differently — which is the whole point: the serve-time warning
// arrives after paired devices have already pinned a cert that will
// fail TLS for the endpoints it does not cover, and the operator wants
// to know before that.
package tls

import (
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
)

// SANStaleRemediation is the one sentence every surface that reports
// stale SANs ends with. A const rather than a string in each caller:
// the startup log, the doctor hint and anything that follows them are
// describing the same two-step fix, and a second copy is a second
// thing to forget to update.
const SANStaleRemediation = "Run `bridge cert rotate` (or click Rotate in the admin console's Cert tile) and restart the bridge, " +
	"then re-pair every paired device — a rotation changes the SHA-256 fingerprint iOS pinned at pairing."

// SANCoverage compares the SAN set a cert minted right now would carry
// against the set the on-disk cert actually carries.
//
// Want* are the FULL merged sets — the loopback/localhost defaults the
// cert template adds unconditionally included — so `len(WantDNS)` reads
// as "names this cert has to cover" in an operator-facing summary.
// Missing* are the subset the cert lacks, in Want* order.
type SANCoverage struct {
	WantDNS    []string
	WantIPs    []net.IP
	MissingDNS []string
	MissingIPs []net.IP
}

// Covered reports whether the cert carries every wanted SAN.
func (c SANCoverage) Covered() bool {
	return len(c.MissingDNS) == 0 && len(c.MissingIPs) == 0
}

// MissingIPStrings renders MissingIPs for a log attribute or an
// operator-facing hint.
func (c SANCoverage) MissingIPStrings() []string { return ipsToStrings(c.MissingIPs) }

// InspectSANCoverage parses the PEM cert at certPath and reports which
// of the SANs `opts` would mint are absent from it.
//
// Read-only and non-mutating — it never rotates, never rewrites, never
// creates. A cert whose SANs have gone stale is deliberately LOADED as
// it is: auto-rotating would silently invalidate every paired iOS
// device's pinned fingerprint, so staleness is reported and the
// operator drives the rotation when they have the devices in hand.
//
// Errors: the file is unreadable, holds no CERTIFICATE block, or does
// not parse. "Don't know" is never reported as coverage — the callers
// say the cert could not be read rather than answering ok about it.
func InspectSANCoverage(certPath string, opts GenerateOptions) (SANCoverage, error) {
	raw, err := os.ReadFile(certPath)
	if err != nil {
		return SANCoverage{}, err
	}
	block, _ := pem.Decode(raw)
	if block == nil || block.Type != "CERTIFICATE" {
		return SANCoverage{}, errors.New("no CERTIFICATE block in PEM")
	}
	parsed, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return SANCoverage{}, fmt.Errorf("parse: %w", err)
	}
	cov := SANCoverage{
		WantDNS: mergeDNSNames(opts.Hostname, opts.ExtraDNSNames),
		WantIPs: mergeIPs(opts.ExtraIPs),
	}
	cov.MissingDNS = stringDiff(cov.WantDNS, parsed.DNSNames)
	cov.MissingIPs = ipDiff(cov.WantIPs, parsed.IPAddresses)
	return cov, nil
}

// logIfSANsStale warns at startup when the on-disk cert doesn't cover
// the SAN set `opts` describes. Best-effort: a read or parse failure is
// silent here, because the operator surfaces (`Inspect`, `bridge
// doctor`'s tls-cert-sans check, the admin Cert tile) carry the
// user-facing diagnostic and a startup log is the wrong place to
// report one twice. Runs once per process from
// LoadOrGenerateWithOptions.
//
// Why warn rather than rotate: see InspectSANCoverage.
func logIfSANsStale(certPath string, opts GenerateOptions) {
	cov, err := InspectSANCoverage(certPath, opts)
	if err != nil || cov.Covered() {
		return
	}
	logger.Warn(
		"cert SANs are stale relative to advertised endpoints — Tailscale and custom-endpoint URLs will fail TLS until you rotate. "+SANStaleRemediation,
		"missing_dns", cov.MissingDNS,
		"missing_ips", cov.MissingIPStrings(),
	)
}

// stringDiff returns elements in `want` that aren't in `got`, case-
// insensitively. Order preserves `want`. Used by InspectSANCoverage to
// list missing DNS SAN names.
func stringDiff(want, got []string) []string {
	have := make(map[string]bool, len(got))
	for _, g := range got {
		have[strings.ToLower(g)] = true
	}
	var miss []string
	for _, w := range want {
		if !have[strings.ToLower(w)] {
			miss = append(miss, w)
		}
	}
	return miss
}

func ipDiff(want, got []net.IP) []net.IP {
	have := make(map[string]bool, len(got))
	for _, g := range got {
		have[string(g.To16())] = true
	}
	var miss []net.IP
	for _, w := range want {
		if !have[string(w.To16())] {
			miss = append(miss, w)
		}
	}
	return miss
}

func ipsToStrings(ips []net.IP) []string {
	out := make([]string, 0, len(ips))
	for _, ip := range ips {
		out = append(out, ip.String())
	}
	return out
}
