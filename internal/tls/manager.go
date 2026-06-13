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
// handshakes to the right cert by SNI. The bridge can carry up to
// THREE active certs at a time:
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
//  3. (Optional, public-mode only) A native autocert-minted LE cert
//     for the operator's configured public domain (set via
//     `SetAutocertProvider` after `internal/tlsacme.New`). Served
//     when the SNI matches the configured domain exactly.
//     iOS clients on this path do NOT pin either — same LE-chain
//     reasoning as the Tailscale cert.
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
// `atomic.Value` for the same reason. The autocert provider is also
// `atomic.Pointer`-held + atomic.Value-name'd; production wiring
// sets these once at boot and never touches them again, but the
// atomic shape future-proofs against a runtime SetAutocertProvider
// (e.g. for a TLS renewal that swaps the underlying autocert.Manager).
type Manager struct {
	selfSigned    *cryptotls.Certificate
	tailscaleCert atomic.Pointer[cryptotls.Certificate]

	// magicDNSSuffix holds a string. atomic.Value is fine here
	// because the type is fixed (always `string`), and the swap
	// happens at most once per process (right after Detect succeeds).
	magicDNSSuffix atomic.Value

	// autocertProvider is the GetCertificate callback for the
	// operator's configured public domain (PR 3). Nil = not
	// configured (loopback installs, public installs running
	// behind a reverse proxy that terminates TLS, or any
	// deployment with `autocert.enabled: false`).
	autocertProvider atomic.Pointer[autocertHook]

	// autocertDomain holds the lowercase + trailing-dot-stripped
	// hostname the autocert provider is pinned to. Empty = no
	// autocert configured. atomic.Value typed string for the same
	// rationale as magicDNSSuffix.
	autocertDomain atomic.Value

	// nextProtos is the union of NextProtos requirements from
	// every active cert provider. PR 3 adds `acme.ALPNProto`
	// when autocert is wired so the same listener handles the
	// TLS-ALPN-01 challenge handshake. Read by AdminTLSConfig +
	// the public-API tlsConfig.
	nextProtos atomic.Value

	// autocertCachedCert returns the LE leaf the listener actually
	// serves for the autocert domain, read passively from the autocert
	// cache. Set via SetAutocertCachedCertFn (wired alongside
	// SetAutocertProvider). Read ONLY by FingerprintForServerName so the
	// pairing QR advertises the cert iOS will capture — NOT via a
	// synthetic-hello GetCertificate, which returns a different leaf.
	// nil pointer = not wired (FingerprintForServerName falls back to
	// self-signed for the autocert domain).
	autocertCachedCert atomic.Pointer[servedCertProvider]
}

// autocertHook is the interface tlsacme.Manager satisfies; the tls
// package keeps the indirection so it doesn't need to import
// internal/tlsacme directly (and tlsacme already imports
// internal/tls for CertNotAfter — avoiding the cycle).
type autocertHook struct {
	GetCert    func(*cryptotls.ClientHelloInfo) (*cryptotls.Certificate, error)
	NextProtos []string
}

// servedCertProvider wraps the func that returns the LE leaf the
// listener actually serves for the autocert domain (read passively from
// the autocert cache). Held in an atomic.Pointer so the value type is
// fixed for atomic storage. Used ONLY by FingerprintForServerName.
type servedCertProvider struct {
	fn func() *cryptotls.Certificate
}

// NewManager constructs a Manager with the self-signed cert as the
// only routing target. Tailscale auto-pilot calls SetTailscaleCert /
// SetMagicDNSSuffix after detection succeeds; autocert wiring calls
// SetAutocertProvider when `cfg.Autocert.Enabled` is true.
func NewManager(selfSigned *cryptotls.Certificate) *Manager {
	if selfSigned == nil {
		// Programmer error — the bridge's self-signed cert is loaded
		// before NewManager is called, and a nil here means the
		// boot path is broken. Fail loud rather than serve no cert.
		panic("tls: NewManager called with nil self-signed cert")
	}
	m := &Manager{selfSigned: selfSigned}
	m.magicDNSSuffix.Store("")
	m.autocertDomain.Store("")
	m.nextProtos.Store([]string(nil))
	return m
}

// SetAutocertProvider installs (or clears, when both args are
// zero-valued) the autocert SNI route. Safe to call from any
// goroutine; the swap is atomic.
//
// domain is the operator's configured public hostname (normalized
// to lowercase, trailing-dot stripped — pass the value
// `tlsacme.Manager.Domain()` returns directly).
//
// getCert is the autocert-side GetCertificate function — typically
// `tlsacmeManager.GetCertificate`. Pass nil to clear the route.
//
// nextProtos is the ALPN proto-id list autocert needs the listener
// to advertise (`acme.ALPNProto`). Empty when clearing.
func (m *Manager) SetAutocertProvider(domain string, getCert func(*cryptotls.ClientHelloInfo) (*cryptotls.Certificate, error), nextProtos []string) {
	domain = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(domain)), ".")
	if getCert == nil || domain == "" {
		m.autocertProvider.Store(nil)
		m.autocertDomain.Store("")
		m.nextProtos.Store([]string(nil))
		m.autocertCachedCert.Store(nil)
		return
	}
	hook := &autocertHook{
		GetCert:    getCert,
		NextProtos: append([]string(nil), nextProtos...),
	}
	m.autocertProvider.Store(hook)
	m.autocertDomain.Store(domain)
	m.nextProtos.Store(append([]string(nil), nextProtos...))
}

// SetAutocertCachedCertFn wires the passive accessor for the LE leaf the
// autocert provider serves on the configured domain (typically
// tlsacme.Manager.CachedCert). FingerprintForServerName uses it so the
// pairing QR advertises the exact cert iOS captures. Pass nil to clear.
// Wired alongside SetAutocertProvider in cmd/bridge/main.go.
func (m *Manager) SetAutocertCachedCertFn(fn func() *cryptotls.Certificate) {
	if fn == nil {
		m.autocertCachedCert.Store(nil)
		return
	}
	m.autocertCachedCert.Store(&servedCertProvider{fn: fn})
}

// NextProtos returns the ALPN proto-ids the wired providers need
// the underlying listener to advertise. Public-API + admin
// listener tls.Config merge this with their own NextProtos
// (typically "h2", "http/1.1"). Returns nil when no provider has
// custom requirements — callers default to the standard
// proto list.
func (m *Manager) NextProtos() []string {
	v, _ := m.nextProtos.Load().([]string)
	if len(v) == 0 {
		return nil
	}
	return append([]string(nil), v...)
}

// AdminTLSConfig returns a tls.Config suitable for wrapping the
// admin listener via tls.NewListener. The config's GetCertificate
// is bound to this Manager's `Get` so the admin listener serves
// the same SNI-routed cert pool as the public API (autocert cert
// for the operator's domain, self-signed for direct-IP / unknown
// SNI). NextProtos merges the autocert ALPN proto with the
// standard HTTP/1.1 + HTTP/2 entries so the admin listener
// handles both real admin traffic AND the TLS-ALPN-01 challenge
// (the challenge runs against TCP/443 by spec; deployments where
// the admin listener IS the :443 listener get both for free).
func (m *Manager) AdminTLSConfig() *cryptotls.Config {
	cfg := &cryptotls.Config{
		GetCertificate: m.Get,
		MinVersion:     cryptotls.VersionTLS12,
	}
	cfg.NextProtos = append(cfg.NextProtos, "h2", "http/1.1")
	if extra := m.NextProtos(); len(extra) > 0 {
		cfg.NextProtos = append(cfg.NextProtos, extra...)
	}
	return cfg
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
	// Strip a trailing dot to mirror Get's SNI normalization. A suffix
	// stored as "foo.ts.net." would otherwise never match a normalized
	// (trailing-dot-stripped, lowercased) SNI, falling every Tailscale
	// handshake through to the self-signed cert.
	m.magicDNSSuffix.Store(strings.TrimSuffix(strings.ToLower(strings.TrimSpace(suffix)), "."))
}

// Get is the `tls.Config.GetCertificate` callback. SNI precedence:
//
//  1. **Normalize SNI once at the top** — lowercase + trailing-dot
//     strip. The TLS ClientHello can carry a trailing dot (FQDN
//     form) AND mixed-case host (RFC 4343: DNS comparison is
//     case-insensitive). Without normalization,
//     "BRIDGE.EXAMPLE.COM." mis-matches a stored lowercase
//     "bridge.example.com" and falls through to self-signed,
//     causing iOS ATS rejection on a perfectly-routed handshake.
//  2. **autocert path** (PR 3): SNI matches the configured public
//     domain exactly. Delegates to the autocert hook's
//     GetCertificate, which mints / serves the LE cert. On
//     autocert error, falls through to self-signed (logged
//     elsewhere — the renewer surfaces the failure to the admin
//     tile).
//  3. **Tailscale path**: SNI ends in the configured MagicDNS
//     suffix AND the Tailscale cert is loaded + non-expired.
//  4. **Fallback**: self-signed cert.
//
// Empty SNI (rare in 2026 but legal per RFC 6066, e.g. older curl
// with --resolve) goes straight to self-signed.
//
// Order: autocert before Tailscale because an operator running
// public mode with Tailscale also enabled (legitimate — tailnet-
// routed public bridge) wants the LE cert to win against the
// public domain SNI even if it happens to share a suffix with the
// MagicDNS zone. In practice the two never overlap (autocert
// domain is the operator's public FQDN; MagicDNS is *.ts.net),
// but the precedence is documented.
func (m *Manager) Get(hello *cryptotls.ClientHelloInfo) (*cryptotls.Certificate, error) {
	if hello == nil || hello.ServerName == "" {
		return m.selfSigned, nil
	}
	sni := strings.TrimSuffix(strings.ToLower(hello.ServerName), ".")

	// (2) Autocert — operator's public domain.
	if domain, _ := m.autocertDomain.Load().(string); domain != "" && sni == domain {
		if hook := m.autocertProvider.Load(); hook != nil && hook.GetCert != nil {
			cert, err := hook.GetCert(hello)
			if err == nil && cert != nil {
				return cert, nil
			}
			// Fall through to self-signed on autocert error.
			// The tlsacme.Manager records lastError for the
			// admin tile; we don't double-log here because
			// every failed handshake would spam the log.
		}
	}

	// (3) Tailscale magic-DNS path.
	suffix, _ := m.magicDNSSuffix.Load().(string)
	if suffix == "" {
		return m.selfSigned, nil
	}
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

// FingerprintForServerName returns the SHA-256 fingerprint (canonical
// colon-hex, matching FingerprintFromDER) of the certificate the
// listener actually serves for a TLS handshake carrying serverName as
// SNI. This lets the pairing-QR baker advertise the fingerprint the
// device will ACTUALLY capture on connect — the public-domain (autocert)
// or Tailscale magic-DNS LE fingerprint for those SNIs, the self-signed
// LAN fingerprint otherwise.
//
// Routing MIRRORS Get's SNI precedence but reads the STORED served cert
// for each branch rather than calling Get(syntheticHello): autocert's
// GetCertificate returns a different leaf for a hello that lacks real
// cipher-suite / sig-alg negotiation, so a synthetic-hello fingerprint
// would advertise a cert the iOS client never captures. Autocert →
// CachedCert() (the on-disk issued leaf); Tailscale → the loaded LE
// cert; else → self-signed. Returns "" only when the matched branch has
// no cert yet AND the self-signed leaf is somehow empty (never in
// practice — self-signed is loaded before NewManager).
func (m *Manager) FingerprintForServerName(serverName string) string {
	sni := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(serverName)), ".")

	// (1) Autocert domain → the LE leaf the listener actually serves,
	// read from the autocert cache. Deliberately NOT via
	// Get(syntheticHello): autocert.Manager.GetCertificate returns a
	// different leaf for a hello that lacks real cipher-suite / sig-alg
	// negotiation, so a synthetic-hello fingerprint would advertise a
	// cert the iOS client never captures (the cause of the first
	// public-pairing fix's residual mismatch). No cached cert yet → fall
	// through to self-signed rather than to a synthetic-hello mint.
	if domain, _ := m.autocertDomain.Load().(string); domain != "" && sni == domain {
		if p := m.autocertCachedCert.Load(); p != nil && p.fn != nil {
			if fp := fingerprintLeaf(p.fn()); fp != "" {
				return fp
			}
		}
	}

	// (2) Tailscale magic-DNS → the loaded LE cert (the leaf Get serves
	// for that SNI).
	if m.sniMatchesTailscale(sni) {
		if fp := fingerprintLeaf(m.tailscaleCert.Load()); fp != "" {
			return fp
		}
	}

	// (3) Fallback: self-signed.
	return fingerprintLeaf(m.selfSigned)
}

// sniMatchesTailscale reports whether sni (already normalized) falls
// under the configured MagicDNS suffix — mirrors Get's Tailscale branch.
func (m *Manager) sniMatchesTailscale(sni string) bool {
	suffix, _ := m.magicDNSSuffix.Load().(string)
	if suffix == "" {
		return false
	}
	return sni == suffix || strings.HasSuffix(sni, "."+suffix)
}

// fingerprintLeaf returns the canonical SHA-256 fingerprint of a cert's
// leaf DER, or "" when the cert is nil / carries no DER blocks.
func fingerprintLeaf(cert *cryptotls.Certificate) string {
	if cert == nil || len(cert.Certificate) == 0 {
		return ""
	}
	return FingerprintFromDER(cert.Certificate[0])
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
