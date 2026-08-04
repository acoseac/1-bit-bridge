package tls

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	cryptotls "crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"testing"
	"time"
)

// mintExpiredTailscaleCert builds a leaf whose NotAfter is already in
// the past.
//
// The sibling mintTestCert deliberately has no lifetime override —
// its docblock explains that in-memory expiry mutation isn't
// meaningful for HANDSHAKE-time branches, since crypto/tls wouldn't
// honour it. That reasoning doesn't apply here: both Get and
// FingerprintForServerName merely READ the stored certificate and
// branch on CertNotAfter, so a genuinely-expired leaf exercises the
// real code path. Built directly rather than through
// GenerateWithOptions, which pins a 397-day forward window by design.
func mintExpiredTailscaleCert(t *testing.T, dnsName string) *cryptotls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: dnsName},
		DNSNames:     []string{dnsName},
		NotBefore:    time.Now().Add(-48 * time.Hour),
		NotAfter:     time.Now().Add(-1 * time.Hour), // expired
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &cryptotls.Certificate{
		Certificate: [][]byte{der},
		PrivateKey:  key,
		Leaf:        leaf,
	}
}

// FingerprintForServerName must not advertise an EXPIRED Tailscale
// leaf.
//
// Get gates the Tailscale branch on CertNotAfter and falls back to the
// self-signed cert once the LE leaf is past its window.
// FingerprintForServerName mirrors Get's SNI routing but reads the
// stored cert per branch rather than delegating (the autocert branch
// must not delegate — see its docblock), and the Tailscale branch
// inherited the routing WITHOUT the freshness check.
//
// The consequence is a pairing failure that looks like nothing else:
// the QR advertises the expired leaf's fingerprint, the listener
// presents the self-signed cert, and the device rejects the join on a
// pin mismatch it has no way to explain.
func TestFingerprintForServerName_ExpiredTailscaleCertFallsBackToSelfSigned(t *testing.T) {
	self := mintTestCert(t, []string{"host.local"})
	expired := mintExpiredTailscaleCert(t, "home-pc.sable-eagle.ts.net")

	mgr := NewManager(self)
	mgr.SetMagicDNSSuffix("sable-eagle.ts.net")
	mgr.SetTailscaleCert(expired)

	selfFP := fingerprintLeaf(self)
	expiredFP := fingerprintLeaf(expired)
	if selfFP == "" || expiredFP == "" || selfFP == expiredFP {
		t.Fatalf("fixture: fingerprints must both be non-empty and distinct (self=%q expired=%q)",
			selfFP, expiredFP)
	}

	got := mgr.FingerprintForServerName("home-pc.sable-eagle.ts.net")
	if got == expiredFP {
		t.Fatal("advertised the EXPIRED Tailscale cert's fingerprint; the listener " +
			"serves self-signed for this SNI, so every pairing attempt fails the pin check")
	}
	if got != selfFP {
		t.Fatalf("fingerprint = %q, want the self-signed %q", got, selfFP)
	}
}

// The real invariant: whatever Get SERVES for an SNI is what
// FingerprintForServerName ADVERTISES for it. Asserted across the
// fresh and expired cases together, since the two functions duplicate
// the routing rather than sharing it.
func TestFingerprintForServerName_AgreesWithGet(t *testing.T) {
	const sni = "home-pc.sable-eagle.ts.net"
	cases := []struct {
		name string
		cert *cryptotls.Certificate
	}{
		{"fresh tailscale cert", nil}, // filled below (needs t)
		{"expired tailscale cert", nil},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			self := mintTestCert(t, []string{"host.local"})
			var le *cryptotls.Certificate
			if i == 0 {
				le = mintTestCert(t, []string{sni})
			} else {
				le = mintExpiredTailscaleCert(t, sni)
			}
			mgr := NewManager(self)
			mgr.SetMagicDNSSuffix("sable-eagle.ts.net")
			mgr.SetTailscaleCert(le)

			served, err := mgr.Get(&cryptotls.ClientHelloInfo{ServerName: sni})
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			wantFP := fingerprintLeaf(served)
			if got := mgr.FingerprintForServerName(sni); got != wantFP {
				t.Fatalf("advertised %q but the listener serves a cert with fingerprint %q",
					got, wantFP)
			}
		})
	}
}
