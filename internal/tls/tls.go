// Package tls mints a self-signed certificate on first run and loads existing
// cert/key material on subsequent runs.
//
// The generated certificate is ECDSA P-256, valid for 397 days, with SANs
// covering localhost, 127.0.0.1, ::1, 0.0.0.0, and the provided hostname (if
// any). iOS clients pin by the SHA-256 fingerprint captured during pairing,
// so the SANs are mostly a convenience for browser-based debugging — the pin
// is what actually secures the session.
//
// **Why 397 days, not 10 years.** Apple ATS (and the underlying iOS
// SecureTransport stack) enforces a 398-day maximum validity for server
// certificates issued after 2020-09-01. A longer cert is rejected at the
// TLS-handshake layer *before* `URLSessionDelegate` is ever consulted —
// fingerprint pinning can't override the platform's baseline check. The
// previous 10-year duration shipped fine because ATS was relaxed via
// `NSAllowsLocalNetworking`, but iOS 26.4's lower-layer (Network.framework)
// path applies the 398-day rule independently. Capping at 397 keeps a small
// safety margin under that ceiling.
//
// **Operator UX.** Yearly rotation is now expected. Surfaced via:
//   - Startup log at `.notice` when the cert is within 30 days of expiry
//     (operator gets a heads-up before iOS clients start failing to handshake).
//   - `bridge cert info` CLI + admin-console cert tile (DaysUntilExpiry).
//   - `bridge cert rotate` CLI + admin "Rotate" button — minting a new cert
//     forces every paired iOS client to re-pair via `bridge://pair?...`
//     deep-link or admin-console QR.
package tls

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	cryptotls "crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/logging"
)

var logger = logging.Component("tls")

const (
	CertFileName = "server.crt"
	KeyFileName  = "server.key"
	// certDuration is capped under Apple ATS's 398-day enforcement
	// (see package doc). 397 days leaves a small safety margin; raising
	// this past 398 will break iOS clients at the TLS handshake layer
	// before pinning is consulted.
	certDuration = 397 * 24 * time.Hour
	// expiryWarningWindow controls when LoadOrGenerate logs an
	// approaching-expiry warning. 30 days covers a typical
	// notice-to-operator → re-pair-every-device cycle.
	expiryWarningWindow = 30 * 24 * time.Hour
)

// DefaultPaths returns the cert and key paths used when the user hasn't
// configured explicit TLS paths in bridge.yaml. Both live inside dataDir.
func DefaultPaths(dataDir string) (certPath, keyPath string) {
	return filepath.Join(dataDir, CertFileName), filepath.Join(dataDir, KeyFileName)
}

// GenerateOptions bundles the SAN inputs to `Generate`. Empty / nil
// fields fall back to the legacy hardcoded set (loopback + Hostname).
//
// ExtraDNSNames + ExtraIPs are unioned with the defaults; duplicates
// across "default + extra" are silently deduped at cert-build time.
type GenerateOptions struct {
	// Hostname is the host's short or fully-qualified hostname, added
	// to the SAN DNSNames as a convenience for browser-based access
	// to the admin console. Empty / "localhost" is silently dropped
	// (localhost is already in the defaults).
	Hostname string

	// ExtraDNSNames is the operator-relevant DNS list — Tailscale
	// MagicDNS (`*.ts.net`), parsed hosts of `cfg.CustomEndpoints`,
	// reverse-proxy front-doors. Each entry must be a bare hostname
	// (no scheme, no port). Use `parseHostFromURL` to get the
	// hostname out of a URL string.
	ExtraDNSNames []string

	// ExtraIPs is the operator-relevant IP list — every up,
	// non-loopback, non-virtual-switch interface IP plus the
	// Tailscale CGNAT addresses surfaced by `tailscale status`.
	// loopback is always included regardless of this slice.
	ExtraIPs []net.IP
}

// LoadOrGenerate loads the cert+key at the given paths, or mints a new
// self-signed ECDSA P-256 pair if both files are absent. hostname (if non-
// empty) is added to the cert's SANs alongside the default loopback entries.
//
// Returns the loaded certificate and its SHA-256 fingerprint in the standard
// colon-separated uppercase-hex form ("AB:CD:..."), ready to display in the
// iOS pairing UI and to compare on the client side for pinning.
//
// Backwards-compat shim: callers that need the broader SAN options
// (LAN / Tailscale / CustomEndpoints) call LoadOrGenerateWithOptions.
func LoadOrGenerate(certPath, keyPath, hostname string) (*cryptotls.Certificate, string, error) {
	return LoadOrGenerateWithOptions(certPath, keyPath, GenerateOptions{Hostname: hostname})
}

// LoadOrGenerateWithOptions is the SAN-aware path. New installs mint a
// cert with the full SAN set (hostname + ExtraDNSNames + loopback IPs +
// ExtraIPs); existing installs LOAD the on-disk cert without
// regenerating, even if the cert's SANs no longer match `opts` — that
// would silently break iOS pinning. A SAN mismatch is logged at
// `.notice` so operators see the staleness signal in the startup log
// and can drive a deliberate `bridge cert rotate` (every paired iOS
// device must re-pair afterward; we don't pay that cost without
// operator consent).
func LoadOrGenerateWithOptions(certPath, keyPath string, opts GenerateOptions) (*cryptotls.Certificate, string, error) {
	certExists := fileExists(certPath)
	keyExists := fileExists(keyPath)
	switch {
	case certExists && keyExists:
		// fall through to load
	case !certExists && !keyExists:
		if err := GenerateWithOptions(certPath, keyPath, opts); err != nil {
			return nil, "", fmt.Errorf("generate TLS material: %w", err)
		}
	default:
		return nil, "", fmt.Errorf(
			"inconsistent TLS material: one of %q / %q exists but not the other — delete the orphan and retry",
			certPath, keyPath,
		)
	}
	cert, err := cryptotls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, "", fmt.Errorf("load TLS material: %w", err)
	}
	fp, err := fingerprintFromPEM(certPath)
	if err != nil {
		return nil, "", fmt.Errorf("fingerprint: %w", err)
	}
	logIfExpiringSoon(certPath)
	logIfSANsStale(certPath, opts)
	return &cert, fp, nil
}

// logIfExpiringSoon parses the on-disk cert and logs a warning when its
// remaining validity is below expiryWarningWindow. Runs once per process
// start (called from LoadOrGenerate) — operators see the warning in the
// startup log alongside the usual listen-address line. Best-effort: a
// parse failure here is silent (Inspect already covers the operator-facing
// surface and would surface a parse error there).
func logIfExpiringSoon(certPath string) {
	info, err := Inspect(certPath)
	if err != nil {
		return
	}
	remaining := time.Until(info.NotAfter)
	switch {
	case remaining <= 0:
		logger.Error("cert expired — every paired iOS client will fail at TLS handshake until you rotate (`bridge cert rotate` or admin console) and re-pair",
			"path", certPath, "expired_days_ago", -info.DaysUntilExpiry)
	case remaining <= expiryWarningWindow:
		logger.Warn("cert expires soon — schedule a `bridge cert rotate` and re-pair every paired iOS client before then (Apple ATS rejects expired certs at the handshake layer)",
			"path", certPath, "days_remaining", info.DaysUntilExpiry)
	}
}

// Generate (re-)mints the cert + key at the given paths. Used by
// `bridge cert rotate` for an operator-driven rotation, and
// internally by `LoadOrGenerate` for first-run minting.
//
// **Overwrites atomically** — `writePEM` commits each PEM via a
// temp-file + rename, so any pre-existing files at `certPath` /
// `keyPath` are replaced atomically; a failed or interrupted write
// leaves the prior files intact (the bridge stays bootable). The
// first-run path in `LoadOrGenerate` gates on file-existence before
// calling Generate; the CLI rotate path relies on this atomic
// overwrite and does NOT pre-remove the files (see cmd/bridge/cert.go).
// Callers that don't want to replace an existing cert must check
// themselves before calling.
//
// **A rotated cert always has a new SHA-256 fingerprint** —
// even if the public key is unchanged, the cert binary differs
// (NotBefore / NotAfter / serial number) and iOS pins the cert,
// not the key. Operators must re-pair every device after a
// rotation; the admin console's per-token "Rotate" button or a
// fresh `bridge://pair?...` deep link is the supported path.
// Generate is the legacy 3-arg form. Equivalent to GenerateWithOptions
// with `GenerateOptions{Hostname: hostname}`. Kept so non-PR-5 call
// sites (CLI tests, future callers that don't need the broader SAN
// list) don't have to reach for the options struct.
func Generate(certPath, keyPath, hostname string) error {
	return GenerateWithOptions(certPath, keyPath, GenerateOptions{Hostname: hostname})
}

// GenerateWithOptions (re-)mints the cert + key at the given paths.
// Used by `bridge cert rotate` for an operator-driven rotation (where
// the broader SAN list matters), and internally by
// `LoadOrGenerateWithOptions` for first-run minting.
//
// See also `Generate` (legacy 3-arg shim).
func GenerateWithOptions(certPath, keyPath string, opts GenerateOptions) error {
	// 0o700: the dir holds the matching private key (written 0o600
	// at line ~146). The cert.pem itself is conventionally world-
	// readable (0o644) — public material — but the directory is the
	// outer barrier, so keep it tight on POSIX. Windows ignores the
	// mode and relies on per-user-profile ACLs at %LOCALAPPDATA%.
	//
	// MkdirAll leaves existing-dir mode untouched; the follow-up
	// Chmod is what hardens upgrades from a previous 0o755 install.
	// Chmod failures are non-fatal — caller still gets a working
	// cert/key pair, just on a less-restricted parent.
	certDir := filepath.Dir(certPath)
	if err := os.MkdirAll(certDir, 0o700); err != nil {
		return fmt.Errorf("mkdir (cert): %w", err)
	}
	_ = os.Chmod(certDir, 0o700)
	keyDir := filepath.Dir(keyPath)
	if err := os.MkdirAll(keyDir, 0o700); err != nil {
		return fmt.Errorf("mkdir (key): %w", err)
	}
	_ = os.Chmod(keyDir, 0o700)
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generate key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return fmt.Errorf("serial: %w", err)
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   "1-bit Bridge",
			Organization: []string{"acoseac"},
		},
		NotBefore: time.Now().Add(-time.Hour), // allow tiny clock skew
		NotAfter:  time.Now().Add(certDuration),
		// ECDSA leaf server cert: only DigitalSignature is meaningful.
		// KeyEncipherment is RSA-specific (RFC 5480 §3) and IsCA must be
		// false for a non-signing end-entity cert.
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  false,
		DNSNames:              mergeDNSNames(opts.Hostname, opts.ExtraDNSNames),
		IPAddresses:           mergeIPs(opts.ExtraIPs),
	}
	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &priv.PublicKey, priv)
	if err != nil {
		return fmt.Errorf("create cert: %w", err)
	}
	if err := writePEM(certPath, "CERTIFICATE", certDER, 0o644); err != nil {
		return fmt.Errorf("write cert: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		_ = os.Remove(certPath) // best-effort: avoid orphan cert without matching key
		return fmt.Errorf("marshal key: %w", err)
	}
	if err := writePEM(keyPath, "EC PRIVATE KEY", keyDER, 0o600); err != nil {
		_ = os.Remove(certPath) // best-effort: avoid orphan cert without matching key
		return fmt.Errorf("write key: %w", err)
	}
	return nil
}

// CertInfo describes the on-disk certificate. Returned by Inspect
// and surfaced by the admin `GET /api/cert` endpoint + the
// `bridge cert info` CLI. Fingerprint is in the canonical
// colon-separated uppercase-hex form.
type CertInfo struct {
	NotBefore       time.Time `json:"notBefore"`
	NotAfter        time.Time `json:"notAfter"`
	Fingerprint     string    `json:"fingerprint"`
	DaysUntilExpiry int       `json:"daysUntilExpiry"`
	Subject         string    `json:"subject"`
}

// Inspect parses the PEM cert at `certPath` and returns its
// metadata. Used by the admin's cert-tile endpoint and the CLI
// `bridge cert info` command. No live validation is performed
// (the operator's view of the on-disk cert is what we report);
// expired certs are surfaced via DaysUntilExpiry being zero or
// negative, not by erroring out.
func Inspect(certPath string) (CertInfo, error) {
	raw, err := os.ReadFile(certPath)
	if err != nil {
		return CertInfo{}, err
	}
	block, _ := pem.Decode(raw)
	if block == nil || block.Type != "CERTIFICATE" {
		return CertInfo{}, errors.New("no CERTIFICATE block in PEM")
	}
	parsed, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return CertInfo{}, fmt.Errorf("parse: %w", err)
	}
	now := time.Now()
	days := int(parsed.NotAfter.Sub(now).Hours() / 24)
	// Integer truncation rounds toward zero, so a cert that
	// expired 23 hours ago and one that expires in 23 hours both
	// land at `days == 0`. Force a negative sentinel when the
	// cert is actually past its NotAfter so the CLI / dashboard
	// can distinguish "expiring within 24h" from "already
	// expired" without re-checking the timestamp themselves
	// (Gemini flagged on PR #46).
	if now.After(parsed.NotAfter) && days >= 0 {
		days = -1
	}
	return CertInfo{
		NotBefore:       parsed.NotBefore,
		NotAfter:        parsed.NotAfter,
		Fingerprint:     FingerprintFromDER(block.Bytes),
		DaysUntilExpiry: days,
		Subject:         parsed.Subject.String(),
	}, nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func dnsNames(hostname string) []string {
	names := []string{"localhost"}
	hostname = strings.TrimSpace(hostname)
	if hostname != "" && hostname != "localhost" {
		names = append(names, hostname)
		// `<shortHostname>.local` is the canonical mDNS form the
		// advertise package emits as `ClassMDNSHost`. Without it in
		// the SAN list, iOS clients dialing the advertised mDNS URL
		// fail TLS hostname verification even though pinning would
		// otherwise accept (Qodo bot review on PR #93). The "short"
		// form strips any FQDN suffix, matching the same trim
		// `advertise.Endpoints()` does.
		short := hostname
		if i := strings.IndexByte(short, '.'); i > 0 {
			short = short[:i]
		}
		dotLocal := short + ".local"
		// Skip when the hostname is already `.local`-suffixed (macOS
		// hostnames typically are, e.g. `mac.local`) so we don't
		// duplicate.
		if dotLocal != hostname && dotLocal != "localhost" {
			names = append(names, dotLocal)
		}
	}
	return names
}

// mergeDNSNames builds the SAN DNSNames list: defaults (localhost +
// hostname) plus operator-supplied extras, deduped case-insensitively.
// Empty / "localhost" entries in extras are dropped silently — already
// in the defaults.
func mergeDNSNames(hostname string, extras []string) []string {
	out := dnsNames(hostname)
	seen := make(map[string]bool, len(out)+len(extras))
	for _, n := range out {
		seen[strings.ToLower(n)] = true
	}
	for _, e := range extras {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		key := strings.ToLower(e)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, e)
	}
	return out
}

func defaultIPs() []net.IP {
	return []net.IP{
		net.IPv4(127, 0, 0, 1),
		net.IPv6loopback,
		net.IPv4zero,
	}
}

// mergeIPs builds the SAN IPAddresses list: defaults (loopback v4/v6 +
// IPv4zero for `0.0.0.0` admin binds) plus operator-supplied extras,
// deduped by canonical 16-byte form.
func mergeIPs(extras []net.IP) []net.IP {
	out := defaultIPs()
	seen := make(map[string]bool, len(out)+len(extras))
	for _, ip := range out {
		seen[string(ip.To16())] = true
	}
	for _, ip := range extras {
		b16 := ip.To16()
		if b16 == nil {
			// nil IP, or malformed (wrong byte length) — To16 returns
			// nil, whose string key would alias every such entry to ""
			// and collapse them into one map slot. Skip outright.
			continue
		}
		key := string(b16)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, ip)
	}
	return out
}

// ParseHostFromURL extracts the bare hostname from a URL string and
// reports whether the host is an IP literal. Used by the SAN-gather
// pipeline to route `cfg.CustomEndpoints` entries into either
// `ExtraDNSNames` (DNS hostnames) or `ExtraIPs` (IP literals) — the
// raw URL string can't go directly into a SAN slot, since x509 rejects
// `https://host:port` shapes with an opaque error.
//
// Returns ("", false) for unparseable input or empty host.
func ParseHostFromURL(raw string) (host string, isIP bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u == nil {
		return "", false
	}
	h := u.Hostname() // strips :port; strips IPv6 brackets
	if h == "" {
		return "", false
	}
	if ip := net.ParseIP(h); ip != nil {
		return h, true
	}
	return h, false
}

// logIfSANsStale parses the on-disk cert and warns if the operator-
// supplied SAN options aren't fully covered. Best-effort: a parse
// failure is silent; the operator surface (`Inspect` / admin Cert
// tile) carries the user-facing diagnostic. Runs once at startup
// from LoadOrGenerateWithOptions.
//
// Why this exists: cert auto-rotation on upgrade would silently
// invalidate every paired iOS device's pinned fingerprint. Warning-
// only preserves the pinning contract — the operator drives rotation
// when they have an iOS device in hand to re-pair.
func logIfSANsStale(certPath string, opts GenerateOptions) {
	raw, err := os.ReadFile(certPath)
	if err != nil {
		return
	}
	block, _ := pem.Decode(raw)
	if block == nil || block.Type != "CERTIFICATE" {
		return
	}
	parsed, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return
	}
	wantDNS := mergeDNSNames(opts.Hostname, opts.ExtraDNSNames)
	wantIPs := mergeIPs(opts.ExtraIPs)
	missingDNS := stringDiff(wantDNS, parsed.DNSNames)
	missingIPs := ipDiff(wantIPs, parsed.IPAddresses)
	if len(missingDNS) == 0 && len(missingIPs) == 0 {
		return
	}
	logger.Warn(
		"cert SANs are stale relative to advertised endpoints — Tailscale and custom-endpoint URLs will fail TLS until you rotate. Use `bridge cert rotate` or click Rotate in the admin Cert tile, then re-pair every iOS device.",
		"missing_dns", missingDNS,
		"missing_ips", ipsToStrings(missingIPs),
	)
}

// stringDiff returns elements in `want` that aren't in `got`, case-
// insensitively. Order preserves `want`. Used by logIfSANsStale to
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

// writePEM encodes der as a PEM block and commits it to path ATOMICALLY:
// the block is written to a temp file in the same directory, fsync'd, and
// renamed over path. A failed or interrupted write (disk full, process
// kill) therefore leaves any pre-existing file at path intact instead of
// truncating it — the bridge stays bootable across a failed cert rotate.
// (The rotate CLI must NOT pre-remove the old files, or this guarantee is
// lost; see cmd/bridge/cert.go.)
//
// RenameWithRetry absorbs the Windows AV-scan-on-close rename window — the
// cert/key live under %LOCALAPPDATA%. Chmod runs on the temp file BEFORE
// the rename so the final mode is correct with no post-rename perms window
// (os.CreateTemp yields 0o600; the 0o644 cert needs widening, the key
// stays at 0o600).
func writePEM(path, blockType string, der []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".pem-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	// Remove defer registered FIRST → runs LAST (LIFO). Close defer
	// registered SECOND → runs FIRST, releasing the FD before Remove tries
	// to unlink (Windows holds an open file from being removed). Mirrors
	// the canonical pattern in internal/auth/auth.go and internal/config.
	defer func() {
		if tmpName != "" {
			_ = os.Remove(tmpName)
		}
	}()
	defer func() { _ = tmp.Close() }()
	if err := tmp.Chmod(mode); err != nil {
		return err
	}
	if err := pem.Encode(tmp, &pem.Block{Type: blockType, Bytes: der}); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := atomicwrite.RenameWithRetry(tmpName, path); err != nil {
		return err
	}
	tmpName = "" // rename succeeded — suppress the deferred Remove
	return nil
}

// fingerprintFromPEM reads the PEM-encoded cert file and returns its SHA-256
// fingerprint. Helper for the LoadOrGenerate path that already has a file
// handy; callers with a parsed cert should use FingerprintFromDER.
func fingerprintFromPEM(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	block, _ := pem.Decode(raw)
	if block == nil || block.Type != "CERTIFICATE" {
		return "", errors.New("no CERTIFICATE block in PEM")
	}
	return FingerprintFromDER(block.Bytes), nil
}

// FingerprintFromDER returns the SHA-256 fingerprint of a DER-encoded cert in
// the canonical colon-separated uppercase-hex form. This is exactly what
// openssl x509 -fingerprint -sha256 and Safari's certificate inspector show,
// so users can verify by eye during pairing.
func FingerprintFromDER(der []byte) string {
	h := sha256.Sum256(der)
	const hexChars = "0123456789ABCDEF"
	var b strings.Builder
	b.Grow(95) // 32 hex pairs + 31 colons; no mid-loop reallocation
	for i, c := range h {
		if i > 0 {
			b.WriteByte(':')
		}
		b.WriteByte(hexChars[c>>4])
		b.WriteByte(hexChars[c&0x0F])
	}
	return b.String()
}
