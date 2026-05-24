package tlsacme

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"
)

// generateTestPEM mints a fresh self-signed cert + key as a
// concatenated PEM bundle in the shape autocert.DirCache writes:
// PEM-encoded EC PRIVATE KEY followed by PEM-encoded CERTIFICATE.
// Used to seed the cache for Status() tests without running a real
// LE handshake.
func generateTestPEM(t *testing.T, host string) []byte {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    time.Now().Add(-1 * time.Hour),
		NotAfter:     time.Now().Add(90 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{host},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	// autocert.DirCache layout: key first, then cert chain.
	return append(keyPEM, certPEM...)
}

func newClientHello(serverName string) *tls.ClientHelloInfo {
	return &tls.ClientHelloInfo{
		ServerName: serverName,
		// autocert.Manager peeks at SupportedProtos to decide
		// whether the connection is the TLS-ALPN-01 challenge
		// path. Leave nil here — the failing-on-network path
		// we're testing doesn't depend on it.
	}
}
