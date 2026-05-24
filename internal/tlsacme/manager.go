// Package tlsacme wraps `golang.org/x/crypto/acme/autocert` with the
// glue the bridge needs to participate in `internal/tls.Manager`'s
// SNI switcher:
//
//   - Constructor pinned to the operator's single public domain
//     (autocert's HostPolicy refuses any other SNI — defense in
//     depth against an attacker who manages to resolve a stranger
//     domain to the bridge's IP and tricks LE into provisioning).
//   - Cert cache directory created at 0o700 + cert files at 0o600
//     (autocert.DirCache uses 0o644 by default; we explicitly
//     tighten the dir mode after construction to keep the cache
//     consistent with the rest of the bridge's on-disk state).
//   - A `Status()` snapshot that surfaces "is the cert present,
//     what's its expiry, what was the most-recent error" for the
//     admin tile — autocert.Manager doesn't expose this directly.
//   - Configurable ACME directory URL (production vs LE staging)
//     so the operator can test the full flow without burning the
//     production rate-limit budget.
//
// Threading: the underlying `autocert.Manager` is safe for
// concurrent use; the per-error / per-success status writes go
// through a `sync.RWMutex` on the wrapper so the admin tile read
// can't tear against a renewal happening on a separate goroutine.
package tlsacme

import (
	"context"
	cryptotls "crypto/tls"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"

	"github.com/acoseac/1-bit-bridge/internal/logging"
	servertls "github.com/acoseac/1-bit-bridge/internal/tls"
)

// contextWithShortTimeout returns a context that auto-cancels after
// 5 s — used by the admin-status passive cache lookup so a hung
// filesystem can't wedge the dashboard.
func contextWithShortTimeout() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 5*time.Second)
}

var logger = logging.Component("tlsacme")

// stagingDirectoryURL is LE's staging endpoint — untrusted cert
// (browsers warn), no rate limit. Operators opt in via
// `autocert.useStaging: true` for dev/test.
const stagingDirectoryURL = "https://acme-staging-v02.api.letsencrypt.org/directory"

// Config bundles the operator-supplied autocert parameters in a
// shape the bridge cmd-level wiring can pass to New without
// importing internal/config (avoids circular-dep risk if config
// ever needs to consult tlsacme during validation).
type Config struct {
	// Domain is the single hostname this manager is allowed to
	// mint for. Lowercased + trailing-dot stripped at New time so
	// HostPolicy comparisons are direct.
	Domain string

	// Email registers with the ACME directory; LE uses it for
	// expiry warnings + service-disruption notices.
	Email string

	// CacheDir is where autocert stores the account key, issued
	// certs, and pending challenge state. Must persist across
	// restarts — wiping it burns the LE duplicate-cert quota
	// (5/week per domain).
	CacheDir string

	// UseStaging routes the ACME client at LE's staging
	// directory instead of production.
	UseStaging bool
}

// Manager wraps autocert.Manager with the bridge-side glue. The
// zero value is unusable; construct via New.
type Manager struct {
	cfg Config
	am  *autocert.Manager

	mu        sync.RWMutex
	lastError string
	lastCheck time.Time
}

// New constructs a wired-up Manager. Creates the cache directory
// (0o700) if absent. Returns an error only when CacheDir creation
// fails — Config sanity is the caller's responsibility (the
// internal/config Validate has already gated Domain + Email +
// port-443 by the time this is called).
func New(cfg Config) (*Manager, error) {
	if cfg.Domain == "" {
		return nil, errors.New("tlsacme: Domain must not be empty")
	}
	if cfg.Email == "" {
		return nil, errors.New("tlsacme: Email must not be empty")
	}
	if cfg.CacheDir == "" {
		return nil, errors.New("tlsacme: CacheDir must not be empty")
	}
	// Lowercase + strip trailing dot so HostPolicy compares
	// against the same normalized form that `tls.Manager.Get`
	// produces from the ClientHello.
	cfg.Domain = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(cfg.Domain)), ".")

	if err := os.MkdirAll(cfg.CacheDir, 0o700); err != nil {
		return nil, fmt.Errorf("tlsacme: mkdir cache dir: %w", err)
	}
	// Belt-and-braces: tighten the mode in case the dir already
	// existed at a wider mode (e.g. inherited from a less-locked-
	// down deploy). Cost: one chmod syscall at boot.
	if err := os.Chmod(cfg.CacheDir, 0o700); err != nil {
		// Non-fatal — some filesystems ignore POSIX modes.
		logger.Warn("could not tighten cache dir mode", "path", cfg.CacheDir, "err", err)
	}

	m := &Manager{cfg: cfg}

	// Pin HostPolicy to the configured Domain only. autocert
	// invokes this on every cert-mint request; refusing every
	// non-Domain SNI defeats an attacker who resolves a stranger
	// domain to the bridge's IP and tries to trick LE into
	// minting a cert for it (the attacker still couldn't use the
	// cert, but the bridge would needlessly burn the LE quota).
	hostPolicy := autocert.HostWhitelist(cfg.Domain)

	am := &autocert.Manager{
		Cache:      autocert.DirCache(cfg.CacheDir),
		Prompt:     autocert.AcceptTOS,
		HostPolicy: hostPolicy,
		Email:      cfg.Email,
	}
	if cfg.UseStaging {
		am.Client = &acme.Client{DirectoryURL: stagingDirectoryURL}
		logger.Warn("autocert using LE STAGING directory — certs will be untrusted by browsers",
			"domain", cfg.Domain)
	}

	m.am = am
	return m, nil
}

// GetCertificate is the SNI-callback shape suitable for plugging
// into a tls.Config (directly) OR for routing from a higher-level
// dispatcher (`internal/tls.Manager.Get`). Delegates straight to
// autocert.Manager.GetCertificate.
//
// Records the most recent error + success-check timestamp under
// `m.mu` so the admin tile can surface a meaningful "last error"
// without polling autocert's internals.
func (m *Manager) GetCertificate(hello *cryptotls.ClientHelloInfo) (*cryptotls.Certificate, error) {
	cert, err := m.am.GetCertificate(hello)
	m.mu.Lock()
	m.lastCheck = time.Now().UTC()
	if err != nil {
		m.lastError = err.Error()
	} else {
		m.lastError = ""
	}
	m.mu.Unlock()
	return cert, err
}

// NextProtos returns the ALPN proto-id the autocert TLS-ALPN-01
// challenge requires the listener to advertise. The bridge merges
// this into its tls.Config.NextProtos so LE's challenge handshake
// finds the right path.
//
// Wraps `acme.ALPNProto` so callers don't have to import the acme
// package directly (the dep is otherwise self-contained inside
// this package).
func (m *Manager) NextProtos() []string {
	return []string{acme.ALPNProto}
}

// Domain returns the operator-configured public hostname (normalized
// to lowercase, trailing-dot stripped). Useful for the `tls.Manager`
// SNI gate that needs to compare incoming SNI against this name
// without re-normalizing on every handshake.
func (m *Manager) Domain() string {
	return m.cfg.Domain
}

// Status snapshots the live cert + error state for the admin tile.
// Returns ("", time.Time{}, "") when no cert has been minted yet —
// the tile renders "minting…" in that case.
type Status struct {
	Domain      string
	CertPresent bool
	NotAfter    time.Time
	LastError   string
	LastCheck   time.Time
}

// Status returns a copy-safe snapshot. Reads under RLock so a
// renewal-in-progress can't tear the response.
func (m *Manager) Status() Status {
	m.mu.RLock()
	st := Status{
		Domain:    m.cfg.Domain,
		LastError: m.lastError,
		LastCheck: m.lastCheck,
	}
	m.mu.RUnlock()

	// Cache lookup is via the autocert.Cache interface;
	// autocert.DirCache stores the issued cert under the domain
	// name. A nil/missing return means autocert hasn't minted
	// yet (or never has). NotAfter parsing reuses servertls'
	// helper so the gotcha-handling stays in one place.
	cert := m.cachedCert()
	if cert != nil {
		st.CertPresent = true
		if notAfter, err := servertls.CertNotAfter(cert); err == nil {
			st.NotAfter = notAfter
		}
	}
	return st
}

// cachedCert returns the issued cert from the on-disk cache, or
// nil when nothing has been minted yet. **Passive lookup** —
// reads the autocert.Cache directly, never triggers a mint
// (calling autocert.Manager.GetCertificate from a status path
// would burn the LE quota every time the operator opened the
// dashboard).
//
// Errors are silently mapped to nil — the admin tile renders
// "minting…" / "no cert yet" in that case, no point in surfacing
// a meaningless "cache miss" error to the operator.
func (m *Manager) cachedCert() *cryptotls.Certificate {
	// autocert.DirCache stores the issued cert under the bare
	// domain name. The PEM bundle contains the private key
	// followed by the cert chain — same shape
	// tls.X509KeyPair expects.
	ctx, cancel := contextWithShortTimeout()
	defer cancel()
	pem, err := m.am.Cache.Get(ctx, m.cfg.Domain)
	if err != nil {
		return nil
	}
	cert, err := cryptotls.X509KeyPair(pem, pem)
	if err != nil {
		return nil
	}
	return &cert
}
