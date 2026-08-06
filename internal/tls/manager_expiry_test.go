package tls

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	cryptotls "crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"testing"
	"time"
)

// mintExpiredCert builds a leaf whose NotAfter is already in the past.
// Used for BOTH LE branches (Tailscale magic-DNS and autocert) — an
// expired leaf is an expired leaf, and the routing under test differs
// only in which store it is read from.
//
// The sibling mintTestCert deliberately has no lifetime override —
// its docblock explains that in-memory expiry mutation isn't
// meaningful for HANDSHAKE-time branches, since crypto/tls wouldn't
// honour it. That reasoning doesn't apply here: both Get and
// FingerprintForServerName merely READ the stored certificate and
// branch on CertNotAfter, so a genuinely-expired leaf exercises the
// real code path. Built directly rather than through
// GenerateWithOptions, which pins a 397-day forward window by design.
func mintExpiredCert(t *testing.T, dnsName string) *cryptotls.Certificate {
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
	expired := mintExpiredCert(t, "home-pc.sable-eagle.ts.net")

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
// FingerprintForServerName ADVERTISES for it.
//
// The two functions duplicate the routing rather than sharing it, so
// this has to be asserted per BRANCH and in both the fresh and the
// expired state — a freshness gate added to Get alone leaves the other
// side advertising a pin the listener never presents. The autocert
// branch is the sharper case of the two: it doesn't merely duplicate
// Get's routing, it reads a DIFFERENT store (the on-disk autocert
// cache, vs. autocert.Manager.GetCertificate), so the two can disagree
// about the same cert.
func TestFingerprintForServerName_AgreesWithGet(t *testing.T) {
	const (
		tsSNI    = "home-pc.sable-eagle.ts.net"
		tsSuffix = "sable-eagle.ts.net"
		acmeSNI  = "bridge.example.com"
	)
	cases := []struct {
		name string
		sni  string
		// setup stages one branch and returns the LE leaf it staged.
		setup func(t *testing.T, mgr *Manager) *cryptotls.Certificate
		// wantSelfSigned asserts the fixture really did fall back, so a
		// passing agreement can't be vacuous.
		wantSelfSigned bool
	}{
		{
			name: "fresh tailscale cert",
			sni:  tsSNI,
			setup: func(t *testing.T, mgr *Manager) *cryptotls.Certificate {
				le := mintTestCert(t, []string{tsSNI})
				mgr.SetMagicDNSSuffix(tsSuffix)
				mgr.SetTailscaleCert(le)
				return le
			},
		},
		{
			name: "expired tailscale cert",
			sni:  tsSNI,
			setup: func(t *testing.T, mgr *Manager) *cryptotls.Certificate {
				le := mintExpiredCert(t, tsSNI)
				mgr.SetMagicDNSSuffix(tsSuffix)
				mgr.SetTailscaleCert(le)
				return le
			},
			wantSelfSigned: true,
		},
		{
			name: "fresh autocert cert",
			sni:  acmeSNI,
			setup: func(t *testing.T, mgr *Manager) *cryptotls.Certificate {
				le := mintTestCert(t, []string{acmeSNI})
				mgr.SetAutocertProvider(acmeSNI,
					func(*cryptotls.ClientHelloInfo) (*cryptotls.Certificate, error) { return le, nil },
					nil)
				mgr.SetAutocertCachedCertFn(func() *cryptotls.Certificate { return le })
				return le
			},
		},
		{
			// autocert.Manager.GetCertificate refuses to serve a cached
			// leaf past NotAfter and attempts a renewal instead; once
			// that renewal keeps failing (expired ACME account, DNS
			// moved, :443 unreachable) it returns an error and Get falls
			// through to self-signed — while the on-disk cache this
			// branch reads still holds the stale leaf. That divergence
			// is the whole reason the gate has to be restated.
			name: "expired autocert cert with failing renewal",
			sni:  acmeSNI,
			setup: func(t *testing.T, mgr *Manager) *cryptotls.Certificate {
				stale := mintExpiredCert(t, acmeSNI)
				mgr.SetAutocertProvider(acmeSNI,
					func(*cryptotls.ClientHelloInfo) (*cryptotls.Certificate, error) {
						return nil, errors.New("acme: renewal failed")
					}, nil)
				mgr.SetAutocertCachedCertFn(func() *cryptotls.Certificate { return stale })
				return stale
			},
			wantSelfSigned: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			self := mintTestCert(t, []string{"host.local"})
			mgr := NewManager(self)
			staged := tc.setup(t, mgr)

			selfFP := fingerprintLeaf(self)
			stagedFP := fingerprintLeaf(staged)
			if selfFP == "" || stagedFP == "" || selfFP == stagedFP {
				t.Fatalf("fixture: fingerprints must be non-empty and distinct (self=%q staged=%q)",
					selfFP, stagedFP)
			}

			served, err := mgr.Get(&cryptotls.ClientHelloInfo{ServerName: tc.sni})
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			wantFP := fingerprintLeaf(served)
			if tc.wantSelfSigned && wantFP != selfFP {
				t.Fatalf("fixture: Get should have fallen back to self-signed, got %q", wantFP)
			}
			if !tc.wantSelfSigned && wantFP != stagedFP {
				t.Fatalf("fixture: Get should serve the staged LE cert, got %q", wantFP)
			}

			if got := mgr.FingerprintForServerName(tc.sni); got != wantFP {
				t.Fatalf("advertised %q but the listener serves a cert with fingerprint %q — "+
					"the pairing QR bakes a pin no device can ever match", got, wantFP)
			}
		})
	}
}
