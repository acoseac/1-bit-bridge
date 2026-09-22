package tls

import (
	cryptotls "crypto/tls"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mintCert writes a cert+key pair under a fresh temp dir with the given
// SAN options and returns the cert path.
func mintCert(t *testing.T, opts GenerateOptions) string {
	t.Helper()
	dir := t.TempDir()
	certPath, keyPath := DefaultPaths(dir)
	if err := GenerateWithOptions(certPath, keyPath, opts); err != nil {
		t.Fatalf("mint: %v", err)
	}
	return certPath
}

// TestInspectSANCoverage_StaleCertNamesTheMissingSet is the field case:
// a data directory carried to a new host keeps the old host's cert, so
// every name and address the NEW host advertises is uncovered.
func TestInspectSANCoverage_StaleCertNamesTheMissingSet(t *testing.T) {
	// The old host, as observed 2026-09-20.
	certPath := mintCert(t, GenerateOptions{
		Hostname:      "1bitbridge",
		ExtraDNSNames: []string{"bridge.ars.md"},
		ExtraIPs:      []net.IP{net.ParseIP("10.0.0.4")},
	})

	// The new host, advertising a different name and three new addresses.
	cov, err := InspectSANCoverage(certPath, GenerateOptions{
		Hostname:      "nuc",
		ExtraDNSNames: []string{"nuc.sable-eagle.ts.net"},
		ExtraIPs: []net.IP{
			net.ParseIP("192.168.0.24"),
			net.ParseIP("100.102.105.89"),
			net.ParseIP("fd7a:115c:a1e0::1234"),
		},
	})
	if err != nil {
		t.Fatalf("InspectSANCoverage: %v", err)
	}
	if cov.Covered() {
		t.Fatal("stale cert reported as covering the new host's endpoints")
	}

	// `nuc` and `nuc.local` come from the hostname, the ts.net name from
	// the extras. `localhost` is in both certs and must NOT be listed.
	wantDNS := []string{"nuc", "nuc.local", "nuc.sable-eagle.ts.net"}
	if got := cov.MissingDNS; !equalStrings(got, wantDNS) {
		t.Errorf("MissingDNS = %v, want %v", got, wantDNS)
	}
	wantIPs := []string{"192.168.0.24", "100.102.105.89", "fd7a:115c:a1e0::1234"}
	if got := cov.MissingIPStrings(); !equalStrings(got, wantIPs) {
		t.Errorf("MissingIPs = %v, want %v", got, wantIPs)
	}

	// Want* are the FULL merged sets, defaults included — the counts the
	// doctor summary renders as "N name(s) and M address(es)".
	if !containsString(cov.WantDNS, "localhost") {
		t.Errorf("WantDNS = %v, want it to carry the unconditional localhost", cov.WantDNS)
	}
	if len(cov.WantIPs) != len(cov.MissingIPs)+3 {
		t.Errorf("WantIPs = %v, want the 3 loopback defaults on top of the 3 missing", ipsToStrings(cov.WantIPs))
	}
}

// TestInspectSANCoverage_MatchingCertIsCovered is the negative control
// for the test above: the SAME options the cert was minted with must
// come back with nothing missing, or the comparison is reporting a
// difference that is really an artefact of the merge.
func TestInspectSANCoverage_MatchingCertIsCovered(t *testing.T) {
	opts := GenerateOptions{
		Hostname:      "nuc",
		ExtraDNSNames: []string{"nuc.sable-eagle.ts.net"},
		ExtraIPs:      []net.IP{net.ParseIP("192.168.0.24"), net.ParseIP("100.102.105.89")},
	}
	cov, err := InspectSANCoverage(mintCert(t, opts), opts)
	if err != nil {
		t.Fatalf("InspectSANCoverage: %v", err)
	}
	if !cov.Covered() {
		t.Fatalf("fresh cert reported stale: missing dns %v, ips %v", cov.MissingDNS, cov.MissingIPStrings())
	}
}

// TestInspectSANCoverage_CoverageIsCaseInsensitiveForNames — DNS names
// are case-insensitive, and a cert minted from `os.Hostname()` on one
// host against a config that spells the same name differently is an
// operator typo, not a stale cert. A false "stale" here sends someone
// to re-pair every device for nothing.
func TestInspectSANCoverage_CoverageIsCaseInsensitiveForNames(t *testing.T) {
	certPath := mintCert(t, GenerateOptions{Hostname: "NUC", ExtraDNSNames: []string{"Bridge.Example.COM"}})
	cov, err := InspectSANCoverage(certPath, GenerateOptions{Hostname: "nuc", ExtraDNSNames: []string{"bridge.example.com"}})
	if err != nil {
		t.Fatalf("InspectSANCoverage: %v", err)
	}
	if !cov.Covered() {
		t.Errorf("case difference reported as missing: %v", cov.MissingDNS)
	}
}

// TestInspectSANCoverage_UnreadableCertIsAnError — "don't know" must
// never surface as coverage. Every caller reports the error rather than
// answering ok about a cert it could not parse.
func TestInspectSANCoverage_UnreadableCertIsAnError(t *testing.T) {
	dir := t.TempDir()
	garbage := filepath.Join(dir, "server.crt")
	if err := os.WriteFile(garbage, []byte("not a certificate"), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{
		"absent":     filepath.Join(dir, "nope.crt"),
		"not-a-pem":  garbage,
		"empty-path": "",
	} {
		if _, err := InspectSANCoverage(path, GenerateOptions{Hostname: "h"}); err == nil {
			t.Errorf("%s: want an error, got coverage", name)
		}
	}
}

// TestRotationRemediationNamesBothSteps — the const is the single copy
// of the fix the startup warning and both doctor cert checks end with,
// and a rotation that is not followed by a re-pair leaves every paired
// device unable to connect: the cert it pinned no longer exists.
// Losing either half of that sentence is the failure worth pinning.
//
// The reject half is the other lesson. The sentence offered "or click
// Rotate in the admin console's Cert tile" for as long as it existed,
// and no such control has ever been rendered — the tile's own panel
// note says rotation is CLI-only. A remediation is read by an operator
// who is already stuck, so a step that cannot be taken costs them the
// search. The rejected substrings are lowercased on both sides so a
// reworded reintroduction ("the Cert tile's Rotate button") is caught
// too.
func TestRotationRemediationNamesBothSteps(t *testing.T) {
	for _, want := range []string{"bridge cert rotate", "re-pair", "fingerprint"} {
		if !strings.Contains(RotationRemediation, want) {
			t.Errorf("RotationRemediation does not mention %q: %q", want, RotationRemediation)
		}
	}
	lower := strings.ToLower(RotationRemediation)
	for _, reject := range []string{"cert tile", "click rotate", "rotate button"} {
		if strings.Contains(lower, reject) {
			t.Errorf("RotationRemediation offers %q — the admin console has no Rotate control, "+
				"and its own panel note says rotation is CLI-only: %q", reject, RotationRemediation)
		}
	}
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

func containsString(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

// TestEveryCertReaderAgreesWithWhatServeLoads — `crypto/tls.
// LoadX509KeyPair`, the load `bridge serve` performs, walks EVERY PEM
// block and collects the CERTIFICATE ones, so a file whose first block
// is a key or an openssl `Bag Attributes` preamble loads fine. The
// read-side surfaces here decoded only the first block and answered "no
// CERTIFICATE block in PEM" for the same file — so `bridge doctor`
// called a certificate the bridge is happily serving unreadable, on
// both of its cert lines.
//
// The property is agreement, not any one function's behaviour: doctor
// prints an expiry line and a SAN line about ONE certificate, and one
// of them grading it while the other calls it unreadable would be worse
// than either answer alone.
func TestEveryCertReaderAgreesWithWhatServeLoads(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := DefaultPaths(dir)
	opts := GenerateOptions{Hostname: "nuc", ExtraDNSNames: []string{"nuc.example.test"}}
	if err := GenerateWithOptions(certPath, keyPath, opts); err != nil {
		t.Fatal(err)
	}
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}

	// A cert file carrying the key first, then a metadata preamble —
	// both shapes real PKI tooling emits.
	mixed := filepath.Join(dir, "keyfirst.crt")
	body := append([]byte("Bag Attributes\n    friendlyName: bridge\n"), keyPEM...)
	if err := os.WriteFile(mixed, append(body, certPEM...), 0o644); err != nil {
		t.Fatal(err)
	}

	// The reference: what serve does with it.
	if _, err := cryptotls.LoadX509KeyPair(mixed, keyPath); err != nil {
		t.Skipf("crypto/tls itself rejects this shape (%v) — the premise is gone, not the code", err)
	}

	if _, err := Inspect(mixed); err != nil {
		t.Errorf("Inspect: %v — serve loads this file", err)
	}
	cov, err := InspectSANCoverage(mixed, opts)
	if err != nil {
		t.Fatalf("InspectSANCoverage: %v — serve loads this file", err)
	}
	if !cov.Covered() {
		t.Errorf("read the wrong block: missing %v / %v", cov.MissingDNS, cov.MissingIPStrings())
	}
	if _, err := fingerprintFromPEM(mixed); err != nil {
		t.Errorf("fingerprintFromPEM: %v — serve loads this file", err)
	}

	// NEGATIVE CONTROL: a file with no CERTIFICATE block at all is still
	// an error. Skipping past non-cert blocks must not become skipping
	// past the absence of one.
	keyOnly := filepath.Join(dir, "keyonly.crt")
	if err := os.WriteFile(keyOnly, keyPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Inspect(keyOnly); err == nil {
		t.Error("Inspect accepted a file holding no certificate")
	}
	if _, err := InspectSANCoverage(keyOnly, opts); err == nil {
		t.Error("InspectSANCoverage accepted a file holding no certificate")
	}
}

// TestVerifyKeyPairIsServesOwnAnswer — the doctor's pair check has to be
// the load `bridge serve` performs, not a second opinion about it. The
// mismatched arm is the residual GenerateWithOptions documents: it
// commits cert and key in two renames, and a crash between them leaves
// a new cert with the old key.
func TestVerifyKeyPairIsServesOwnAnswer(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	aCert, aKey := DefaultPaths(a)
	bCert, bKey := DefaultPaths(b)
	for _, d := range []struct{ c, k string }{{aCert, aKey}, {bCert, bKey}} {
		if err := GenerateWithOptions(d.c, d.k, GenerateOptions{Hostname: "h"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := VerifyKeyPair(aCert, aKey); err != nil {
		t.Errorf("a real pair: %v", err)
	}
	if err := VerifyKeyPair(aCert, bKey); err == nil {
		t.Error("a cert and another install's key verified as a pair")
	}
	// And the cert half alone still parses clean, which is exactly why
	// reading it was not enough.
	if _, err := Inspect(aCert); err != nil {
		t.Errorf("Inspect on the mismatched pair's cert: %v", err)
	}
}
