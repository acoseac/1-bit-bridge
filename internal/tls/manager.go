package tls

import (
	cryptotls "crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"
)

// Manager owns the cert(s) the bridge serves and routes incoming TLS
// handshakes to the right cert by SNI. The bridge can carry up to two
// active certs at a time:
//
//  1. The self-signed cert at <dataDir>/server.crt, served for LAN /
//     mDNS / IP-literal connections. iOS clients pin its fingerprint
//     at first contact.
//  2. (Optional) A Tailscale-issued Let's Encrypt cert at
//     <dataDir>/tls/tailscale.crt, served when the connecting client's
//     SNI hostname is under the local node's MagicDNS suffix
//     (e.g. *.<tailnet>.ts.net). iOS clients on the LE path do NOT
//     pin — the LE chain validates standard ATS, and the tailnet
//     tunnel already authenticates the peer.
//
// **Why a Manager and not just a `tls.Config{Certificates: ...}`**:
// `tls.Config.Certificates` matches by SAN, which would push the LE
// cert past the self-signed cert ONLY when the server name ends in
// the tailnet suffix — but Go's matching is implicit and depends on
// SAN-list correctness for both certs. An explicit `GetCertificate`
// callback makes the routing predicate visible and unit-testable, and
// future work (per-host pinning policy, cert pre-load timing, etc.)
// has one obvious place to land.
//
// Concurrency: `Manager.Get` is called from the TLS handshake
// goroutine for every accepted connection. The Tailscale cert is
// stored in an `atomic.Pointer` so the renewer (which runs in a
// separate goroutine) can swap a fresh cert in mid-flight without a
// mutex on the hot path. The MagicDNS suffix is stored in an
// `atomic.Value` for the same reason.
type Manager struct {
	selfSigned    *cryptotls.Certificate
	tailscaleCert atomic.Pointer[cryptotls.Certificate]

	// magicDNSSuffix holds a string. atomic.Value is fine here
	// because the type is fixed (always `string`), and the swap
	// happens at most once per process (right after Detect succeeds).
	magicDNSSuffix atomic.Value
}

// NewManager constructs a Manager with the self-signed cert as the
// only routing target. Tailscale auto-pilot calls SetTailscaleCert /
// SetMagicDNSSuffix after detection succeeds.
func NewManager(selfSigned *cryptotls.Certificate) *Manager {
	if selfSigned == nil {
		// Programmer error — the bridge's self-signed cert is loaded
		// before NewManager is called, and a nil here means the
		// boot path is broken. Fail loud rather than serve no cert.
		panic("tls: NewManager called with nil self-signed cert")
	}
	m := &Manager{selfSigned: selfSigned}
	m.magicDNSSuffix.Store("")
	return m
}

// SetTailscaleCert installs a freshly-loaded LE cert. Safe to call
// from any goroutine; the swap is atomic. Pass nil to clear (e.g.
// when the renewer detects the on-disk cert was deleted out from
// under the running bridge).
func (m *Manager) SetTailscaleCert(cert *cryptotls.Certificate) {
	m.tailscaleCert.Store(cert)
}

// TailscaleCert returns the current LE cert, or nil if none is loaded.
// Used by the admin tile + renewer for freshness checks.
func (m *Manager) TailscaleCert() *cryptotls.Certificate {
	return m.tailscaleCert.Load()
}

// SetMagicDNSSuffix records the local tailnet's MagicDNS suffix
// (e.g. "sable-eagle.ts.net"). Empty means "Tailscale not configured /
// not detected" — Get falls through to self-signed for every
// connection, same as if the LE cert weren't loaded.
func (m *Manager) SetMagicDNSSuffix(suffix string) {
	m.magicDNSSuffix.Store(strings.ToLower(strings.TrimSpace(suffix)))
}

// Get is the `tls.Config.GetCertificate` callback. Returns the LE cert
// when:
//
//  1. The client supplied an SNI hostname (no SNI → self-signed; rare
//     in 2026 but legal per RFC 6066, e.g. older curl with --resolve)
//  2. The SNI ends in the configured MagicDNS suffix
//  3. The Tailscale cert is loaded and non-expired
//
// All other branches return the self-signed cert. The renewer tries
// hard to keep the LE cert fresh, but if it slips past expiry the
// honest fallback is "serve self-signed and let ATS reject" — same
// behaviour as a host without Tailscale at all, no surprise.
func (m *Manager) Get(hello *cryptotls.ClientHelloInfo) (*cryptotls.Certificate, error) {
	if hello == nil || hello.ServerName == "" {
		return m.selfSigned, nil
	}
	suffix, _ := m.magicDNSSuffix.Load().(string)
	if suffix == "" {
		return m.selfSigned, nil
	}
	sni := strings.ToLower(strings.TrimSuffix(hello.ServerName, "."))
	if !strings.HasSuffix(sni, "."+suffix) && sni != suffix {
		return m.selfSigned, nil
	}

	leCert := m.tailscaleCert.Load()
	if leCert == nil {
		return m.selfSigned, nil
	}
	notAfter, err := CertNotAfter(leCert)
	if err != nil || time.Now().After(notAfter) {
		return m.selfSigned, nil
	}
	return leCert, nil
}

// CertNotAfter parses the leaf cert from a `tls.Certificate` and
// returns its NotAfter timestamp. **`tls.LoadX509KeyPair` does NOT
// populate `cert.Leaf`** — reading `cert.Leaf.NotAfter` directly is
// a nil-pointer panic waiting to happen (gotcha #1 from the plan
// review). This helper is the single approved way to read the
// expiry; every consumer (renewer, admin tile, SNI Get) MUST route
// through it.
//
// **Hot-path optimisation**: `Manager.Get` calls this on every TLS
// handshake to gate LE-cert use on freshness. When the caller
// loaded the cert via `LoadTailscaleCertFromDisk`, `Leaf` is
// already populated — short-circuit on that field to avoid a per-
// handshake `x509.ParseCertificate` (DER parse + allocation). The
// fall-through path keeps the helper safe for raw
// `tls.LoadX509KeyPair` callers (Gemini on PR #102).
func CertNotAfter(cert *cryptotls.Certificate) (time.Time, error) {
	if cert == nil {
		return time.Time{}, errors.New("tls: CertNotAfter on nil cert")
	}
	if cert.Leaf != nil {
		return cert.Leaf.NotAfter, nil
	}
	if len(cert.Certificate) == 0 {
		return time.Time{}, errors.New("tls: cert has no DER blocks")
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return time.Time{}, fmt.Errorf("parse leaf: %w", err)
	}
	return leaf.NotAfter, nil
}

// LoadTailscaleCertFromDisk is a convenience for the auto-mint /
// renewer paths: opens the PEM cert + key files, builds a
// `tls.Certificate`, and parses the leaf so callers can read the
// expiry without falling into the LoadX509KeyPair-sets-nil-Leaf
// trap. Returns the cert with `Leaf` populated.
func LoadTailscaleCertFromDisk(certPath, keyPath string) (*cryptotls.Certificate, error) {
	c, err := cryptotls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("parse leaf: %w", err)
	}
	c.Leaf = leaf
	return &c, nil
}
