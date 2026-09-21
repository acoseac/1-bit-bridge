package tls

import (
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
func TestRotationRemediationNamesBothSteps(t *testing.T) {
	for _, want := range []string{"bridge cert rotate", "re-pair", "fingerprint"} {
		if !strings.Contains(RotationRemediation, want) {
			t.Errorf("RotationRemediation does not mention %q: %q", want, RotationRemediation)
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
