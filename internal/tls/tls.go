// Package tls mints a self-signed certificate on first run and loads existing
// cert/key material on subsequent runs.
//
// The generated certificate is ECDSA P-256, valid for 10 years, with SANs
// covering localhost, 127.0.0.1, ::1, 0.0.0.0, and the provided hostname (if
// any). iOS clients pin by the SHA-256 fingerprint captured during pairing,
// so the SANs are mostly a convenience for browser-based debugging — the pin
// is what actually secures the session.
//
// The long validity window is deliberate: a pinned cert can't silently rotate
// (every iOS client would have to re-pair), so renewal is a user event, not a
// background one. 10 years puts the rotation cost off for as long as plausible
// without hitting CA/Browser-Forum limits (which don't apply to self-signed
// anyway).
package tls

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	cryptotls "crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	CertFileName = "server.crt"
	KeyFileName  = "server.key"
	certDuration = 10 * 365 * 24 * time.Hour
)

// DefaultPaths returns the cert and key paths used when the user hasn't
// configured explicit TLS paths in bridge.yaml. Both live inside dataDir.
func DefaultPaths(dataDir string) (certPath, keyPath string) {
	return filepath.Join(dataDir, CertFileName), filepath.Join(dataDir, KeyFileName)
}

// LoadOrGenerate loads the cert+key at the given paths, or mints a new
// self-signed ECDSA P-256 pair if both files are absent. hostname (if non-
// empty) is added to the cert's SANs alongside the default loopback entries.
//
// Returns the loaded certificate and its SHA-256 fingerprint in the standard
// colon-separated uppercase-hex form ("AB:CD:..."), ready to display in the
// iOS pairing UI and to compare on the client side for pinning.
func LoadOrGenerate(certPath, keyPath, hostname string) (*cryptotls.Certificate, string, error) {
	certExists := fileExists(certPath)
	keyExists := fileExists(keyPath)
	switch {
	case certExists && keyExists:
		// fall through to load
	case !certExists && !keyExists:
		if err := generate(certPath, keyPath, hostname); err != nil {
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
	return &cert, fp, nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func generate(certPath, keyPath, hostname string) error {
	if err := os.MkdirAll(filepath.Dir(certPath), 0o755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
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
		NotBefore:             time.Now().Add(-time.Hour), // allow tiny clock skew
		NotAfter:              time.Now().Add(certDuration),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              dnsNames(hostname),
		IPAddresses:           defaultIPs(),
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
		return fmt.Errorf("marshal key: %w", err)
	}
	if err := writePEM(keyPath, "EC PRIVATE KEY", keyDER, 0o600); err != nil {
		return fmt.Errorf("write key: %w", err)
	}
	return nil
}

func dnsNames(hostname string) []string {
	names := []string{"localhost"}
	hostname = strings.TrimSpace(hostname)
	if hostname != "" && hostname != "localhost" {
		names = append(names, hostname)
	}
	return names
}

func defaultIPs() []net.IP {
	return []net.IP{
		net.IPv4(127, 0, 0, 1),
		net.IPv6loopback,
		net.IPv4zero,
	}
}

func writePEM(path, blockType string, der []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := pem.Encode(f, &pem.Block{Type: blockType, Bytes: der}); err != nil {
		return err
	}
	return f.Sync()
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
	hexStr := strings.ToUpper(hex.EncodeToString(h[:]))
	var b strings.Builder
	b.Grow(len(hexStr) + len(hexStr)/2)
	for i := 0; i < len(hexStr); i += 2 {
		if i > 0 {
			b.WriteByte(':')
		}
		b.WriteString(hexStr[i : i+2])
	}
	return b.String()
}
