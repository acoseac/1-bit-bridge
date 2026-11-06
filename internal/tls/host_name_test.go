package tls

import (
	"crypto/x509"
	"os"
	"slices"
	"testing"
)

// TestAMintCertifiesTheHostNameInItsASCIIForm is backlog B739's second layer.
// The host's name reaches the certificate through GenerateOptions.Hostname,
// and x509.CreateCertificate refuses a DNS name that is not ASCII, so a host
// named in Cyrillic, or a Mac whose kern.hostname holds other bytes, could not
// mint at all ("create cert: x509: ... cannot be encoded as an IA5String").
// The mint takes the name in the form the bridge advertises it in: an ASCII
// name as it is, byte for byte, its case and its underscore kept; a name with
// non-ASCII characters in its IDNA ASCII form, with that form's `.local`; and
// a name with no valid form not at all, so the certificate carries localhost.
// The coverage check grades the same want-set, so a certificate minted now
// covers it.
func TestAMintCertifiesTheHostNameInItsASCIIForm(t *testing.T) {
	for _, tc := range []struct {
		name, host string
		want       []string
	}{
		{"a Cyrillic name", "Жук", []string{"localhost", "xn--f1ai0a", "xn--f1ai0a.local"}},
		{"the test Mac's bytes", "Ð\u0091Ä¯U", []string{"localhost"}},
		{"mixed case and an underscore", "MacBook-Pro_2", []string{"localhost", "MacBook-Pro_2", "MacBook-Pro_2.local"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := GenerateOptions{Hostname: tc.host}
			certPath := mintCert(t, opts)
			if got := certDNSNames(t, certPath); !slices.Equal(got, tc.want) {
				t.Errorf("the certificate names %q, want %q", got, tc.want)
			}
			cov, err := InspectSANCoverage(certPath, opts)
			if err != nil {
				t.Fatal(err)
			}
			if !cov.Covered() || !slices.Equal(cov.WantDNS, tc.want) {
				t.Errorf("the coverage check wants %q and misses %q of a certificate minted now", cov.WantDNS, cov.MissingDNS)
			}
		})
	}
}

// certDNSNames returns the DNS names of the certificate in the PEM file at
// path.
func certDNSNames(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	block, err := decodeCertificatePEM(raw)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("the certificate does not parse: %v", err)
	}
	return leaf.DNSNames
}
