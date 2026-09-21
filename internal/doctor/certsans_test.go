package doctor

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	servertls "github.com/acoseac/1-bit-bridge/internal/tls"
)

// certFixture mints a cert+key pair in a fresh data dir with the given
// SANs and returns Deps pointing at it, advertising `want`.
func certFixture(t *testing.T, minted, want servertls.GenerateOptions) Deps {
	t.Helper()
	dataDir := t.TempDir()
	certPath, keyPath := servertls.DefaultPaths(dataDir)
	if err := servertls.GenerateWithOptions(certPath, keyPath, minted); err != nil {
		t.Fatalf("mint fixture cert: %v", err)
	}
	return Deps{
		DataDir:  dataDir,
		CertSANs: func(context.Context) servertls.GenerateOptions { return want },
	}
}

// oldHostCert is the 2026-09-20 field case: a data directory moved to a
// new host keeps the cert minted on the old one.
var oldHostCert = servertls.GenerateOptions{
	Hostname:      "1bitbridge",
	ExtraDNSNames: []string{"bridge.ars.md"},
	ExtraIPs:      []net.IP{net.ParseIP("10.0.0.4")},
}

// newHostEndpoints is what the new host advertises — the exact set the
// serve-time WARN reported after the move.
var newHostEndpoints = servertls.GenerateOptions{
	Hostname:      "nuc",
	ExtraDNSNames: []string{"nuc.sable-eagle.ts.net"},
	ExtraIPs: []net.IP{
		net.ParseIP("192.168.0.24"),
		net.ParseIP("100.102.105.89"),
		net.ParseIP("fd7a:115c:a1e0::1234"),
	},
}

// TestCheckTLSCertSANs_StaleCertWarnsWithTheExactMissingSet — the
// reason the check exists. Before it, `bridge doctor` said `[ok]
// tls-cert present` about this cert and the operator only found out
// after `bridge serve` was up and devices had pinned it.
func TestCheckTLSCertSANs_StaleCertWarnsWithTheExactMissingSet(t *testing.T) {
	c := checkTLSCertSANs(t.Context(), certFixture(t, oldHostCert, newHostEndpoints))

	if c.Name != checkNameTLSCertSANs {
		t.Errorf("Name = %q, want %q", c.Name, checkNameTLSCertSANs)
	}
	if c.Status != Warn {
		t.Fatalf("status = %q, want warn (summary %q)", c.Status, c.Summary)
	}
	// EXACTLY the missing names and addresses — nothing the cert does
	// carry. Compared as a parsed list rather than by substring: the
	// IPv6 address ends in `::1234`, so a `strings.Contains(hint,
	// "::1")` probe for the covered loopback matches it and the test
	// fails on its own fixture.
	wantMissing := []string{
		"nuc", "nuc.local", "nuc.sable-eagle.ts.net",
		"192.168.0.24", "100.102.105.89", "fd7a:115c:a1e0::1234",
	}
	if got := hintedMissing(t, c.Hint); !equalStrings(got, wantMissing) {
		t.Errorf("hint lists %v, want %v", got, wantMissing)
	}
	// The remediation, from the one const both surfaces share.
	if !strings.Contains(c.Hint, servertls.RotationRemediation) {
		t.Errorf("hint does not carry the shared remediation: %q", c.Hint)
	}
	// The summary counts rather than lists, and says which way it went.
	if !strings.Contains(c.Summary, "stale") {
		t.Errorf("summary = %q, want it to say the cert is stale", c.Summary)
	}
}

// TestCheckTLSCertSANs_MatchingCertIsOK is the negative control: the
// same check against a cert minted for the endpoints the host actually
// advertises must be clean, or the warn above proves nothing.
func TestCheckTLSCertSANs_MatchingCertIsOK(t *testing.T) {
	c := checkTLSCertSANs(t.Context(), certFixture(t, newHostEndpoints, newHostEndpoints))
	if c.Status != OK {
		t.Fatalf("status = %q, want ok (summary %q, hint %q)", c.Status, c.Summary, c.Hint)
	}
	if c.Hint != "" {
		t.Errorf("a covered cert still carries a hint: %q", c.Hint)
	}
	// The summary reports the size of the set it checked, so an operator
	// can tell a real all-clear from a vacuous one.
	if !strings.Contains(c.Summary, "covers all") {
		t.Errorf("summary = %q, want it to say what it covered", c.Summary)
	}
}

// TestCheckTLSCertSANs_SkipsWhenThereIsNothingToCompare — each of these
// is a state where a verdict would be an invention, and the check has
// to stay quiet rather than warn a first-run operator about a cert that
// does not exist yet.
func TestCheckTLSCertSANs_SkipsWhenThereIsNothingToCompare(t *testing.T) {
	withCert := certFixture(t, newHostEndpoints, newHostEndpoints)

	noSANs := withCert
	noSANs.CertSANs = nil

	noDataDir := Deps{CertSANs: withCert.CertSANs}

	noCert := Deps{DataDir: t.TempDir(), CertSANs: withCert.CertSANs}

	// Cert present, key gone: checkTLSCert reports the partial state; a
	// second complaint about the same pair is noise.
	halfPair := certFixture(t, newHostEndpoints, newHostEndpoints)
	_, keyPath := servertls.DefaultPaths(halfPair.DataDir)
	if err := os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}

	managed := certFixture(t, oldHostCert, newHostEndpoints)
	managed.Managed = true

	for name, d := range map[string]Deps{
		"no config wired": noSANs,
		"no data dir":     noDataDir,
		"no cert yet":     noCert,
		"half a pair":     halfPair,
		"managed":         managed,
	} {
		c := checkTLSCertSANs(t.Context(), d)
		if c.Status != OK {
			t.Errorf("%s: status = %q, want ok (%q)", name, c.Status, c.Summary)
		}
		if c.Hint != "" {
			t.Errorf("%s: carries a hint it cannot act on: %q", name, c.Hint)
		}
	}
}

// TestCheckTLSCertSANs_UnreadableCertWarnsRatherThanPassing — a cert
// that will not parse is not a covered one. The pair exists, so the
// "no certificate yet" skip does not apply, and answering ok would be
// the confident-wrong-answer shape.
func TestCheckTLSCertSANs_UnreadableCertWarnsRatherThanPassing(t *testing.T) {
	d := certFixture(t, newHostEndpoints, newHostEndpoints)
	certPath, _ := servertls.DefaultPaths(d.DataDir)
	if err := os.WriteFile(certPath, []byte("not a certificate"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := checkTLSCertSANs(t.Context(), d)
	if c.Status != Warn {
		t.Fatalf("status = %q, want warn for an unparseable cert (%q)", c.Status, c.Summary)
	}
	if !strings.Contains(c.Summary, "could not read") {
		t.Errorf("summary = %q, want it to say the cert could not be read", c.Summary)
	}
}

// TestCheckTLSCertSANs_GradesTheConfiguredCertNotTheDefault — an
// install with an explicit `tlsCertPath` serves THAT cert, and the
// check has to compare the one `bridge serve` loads. Graded against the
// DataDir default it would find no cert at all and skip, reporting an
// all-clear for a bridge whose real cert is stale.
func TestCheckTLSCertSANs_GradesTheConfiguredCertNotTheDefault(t *testing.T) {
	elsewhere := t.TempDir()
	certPath := filepath.Join(elsewhere, "custom.crt")
	keyPath := filepath.Join(elsewhere, "custom.key")
	if err := servertls.GenerateWithOptions(certPath, keyPath, oldHostCert); err != nil {
		t.Fatal(err)
	}
	// A DataDir with NO cert in it, so the default resolution would skip.
	d := Deps{
		DataDir:     t.TempDir(),
		TLSCertPath: certPath,
		TLSKeyPath:  keyPath,
		CertSANs:    func(context.Context) servertls.GenerateOptions { return newHostEndpoints },
	}
	if c := checkTLSCertSANs(t.Context(), d); c.Status != Warn {
		t.Errorf("tls-cert-sans status = %q, want warn about the configured cert (%q)", c.Status, c.Summary)
	}
	// Its sibling reads the same pair, so it must see a present cert
	// rather than the "absent (init will mint)" the default resolution
	// would report.
	if c := checkTLSCert(t.Context(), d); !strings.HasPrefix(c.Summary, "present") {
		t.Errorf("tls-cert summary = %q, want it to describe the configured cert", c.Summary)
	}
}

// TestCheckTLSCert_ExpiryGrading pins the three bands against the SAME
// threshold `bridge serve`'s startup warning uses. A doctor that said
// ok about a cert the next serve warns on describes a different bridge
// than the one being started.
func TestCheckTLSCert_ExpiryGrading(t *testing.T) {
	// A freshly minted cert is 397 days out — comfortably past the
	// window, so this is the ok band.
	fresh := certFixture(t, newHostEndpoints, newHostEndpoints)
	c := checkTLSCert(t.Context(), fresh)
	if c.Status != OK {
		t.Errorf("fresh cert: status = %q, want ok (%q)", c.Status, c.Summary)
	}
	if !strings.Contains(c.Summary, "expires in") {
		t.Errorf("fresh cert summary = %q, want the days-to-expiry", c.Summary)
	}

	// Inside the window, and past NotAfter. Both are warn, never fail:
	// `bridge init` bails on a fail, and refusing to initialise because
	// the old cert lapsed would block the run that mints the new one.
	for name, tc := range map[string]struct {
		notAfter   time.Time
		wantInSumm string
	}{
		"expiring soon": {time.Now().Add(servertls.ExpiryWarningWindow - 48*time.Hour), "expires in"},
		"expired":       {time.Now().Add(-48 * time.Hour), "EXPIRED"},
	} {
		d := certFixture(t, newHostEndpoints, newHostEndpoints)
		certPath, _ := servertls.DefaultPaths(d.DataDir)
		writeCertWithNotAfter(t, certPath, tc.notAfter)

		c := checkTLSCert(t.Context(), d)
		if c.Status != Warn {
			t.Errorf("%s: status = %q, want warn (%q)", name, c.Status, c.Summary)
		}
		if !strings.Contains(c.Summary, tc.wantInSumm) {
			t.Errorf("%s: summary = %q, want it to contain %q", name, c.Summary, tc.wantInSumm)
		}
		if c.Hint == "" {
			t.Errorf("%s: no hint — the operator is told nothing about what to do", name)
		}
	}
}

// TestCheckTLSCert_UnreadableCertIsNotPresent — "present" was the old
// answer for any pair of files that existed. `bridge serve` fails to
// load this one.
func TestCheckTLSCert_UnreadableCertIsNotPresent(t *testing.T) {
	d := certFixture(t, newHostEndpoints, newHostEndpoints)
	certPath, _ := servertls.DefaultPaths(d.DataDir)
	if err := os.WriteFile(certPath, []byte("not a certificate"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := checkTLSCert(t.Context(), d)
	if c.Status != Warn || !strings.Contains(c.Summary, "unreadable") {
		t.Errorf("status = %q, summary = %q; want warn about an unreadable cert", c.Status, c.Summary)
	}
}

// writeCertWithNotAfter replaces the cert at path with a self-signed
// one expiring at `notAfter`.
//
// Minted here rather than through servertls.GenerateWithOptions because
// that path hard-codes 397 days (Apple ATS's ceiling), which is exactly
// the constant that makes the near-expiry and expired bands
// unreachable through the production minter. The key on disk is left
// alone: nothing in these checks loads the pair, only parses the cert.
func writeCertWithNotAfter(t *testing.T, path string, notAfter time.Time) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "1-bit-bridge doctor fixture"},
		NotBefore:    notAfter.Add(-24 * time.Hour),
		NotAfter:     notAfter,
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(path, pemBytes, 0o644); err != nil {
		t.Fatal(err)
	}
}

// hintedMissing pulls the comma-separated entry list back out of the
// hint, so the assertion is over the SET the operator is shown rather
// than over substrings of a sentence.
func hintedMissing(t *testing.T, hint string) []string {
	t.Helper()
	const lead = "clients dialling "
	_, rest, ok := strings.Cut(hint, lead)
	if !ok {
		t.Fatalf("hint does not open with %q: %q", lead, hint)
	}
	list, _, ok := strings.Cut(rest, " fail TLS")
	if !ok {
		t.Fatalf("hint does not name the consequence: %q", hint)
	}
	return strings.Split(list, ", ")
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
