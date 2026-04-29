package tls

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestGenerateFirstRun(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := DefaultPaths(dir)

	cert, fp, err := LoadOrGenerate(certPath, keyPath, "myhost.local")
	if err != nil {
		t.Fatalf("LoadOrGenerate: %v", err)
	}
	if cert == nil || len(cert.Certificate) == 0 {
		t.Fatal("empty certificate returned")
	}
	if !fileExists(certPath) || !fileExists(keyPath) {
		t.Fatal("cert/key files not written")
	}
	if fp == "" {
		t.Fatal("empty fingerprint")
	}

	parsed, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("parse generated cert: %v", err)
	}
	if parsed.Subject.CommonName != "1-bit Bridge" {
		t.Errorf("CN = %q", parsed.Subject.CommonName)
	}
	if !slices.Contains(parsed.DNSNames, "localhost") {
		t.Errorf("DNSNames missing localhost: %v", parsed.DNSNames)
	}
	if !slices.Contains(parsed.DNSNames, "myhost.local") {
		t.Errorf("DNSNames missing myhost.local: %v", parsed.DNSNames)
	}
	if !containsIP(parsed.IPAddresses, "127.0.0.1") {
		t.Errorf("IPAddresses missing 127.0.0.1: %v", parsed.IPAddresses)
	}

	// Cap is 397 days to stay under Apple ATS's 398-day enforcement
	// (see package doc). Allow a one-day slop on each side for clock /
	// rounding (NotBefore is now-1h, NotAfter is now+397d).
	validity := parsed.NotAfter.Sub(parsed.NotBefore)
	const wantValidity = 397 * 24 * time.Hour
	if validity < wantValidity-2*24*time.Hour || validity > wantValidity+2*24*time.Hour {
		t.Errorf("validity = %v, want ~%v (±2d)", validity, wantValidity)
	}
	atsCap := 398 * 24 * time.Hour
	if remaining := time.Until(parsed.NotAfter); remaining > atsCap {
		t.Errorf("NotAfter exceeds ATS cap of 398 days: remaining=%v", remaining)
	}
	// Belt-and-braces — the duration check above could pass for a cert
	// that's already expired (right span, wrong absolute time). Confirm
	// the freshly-minted cert is actually usable right now.
	now := time.Now()
	if parsed.NotBefore.After(now) || parsed.NotAfter.Before(now) {
		t.Errorf("generated cert not valid at time of mint: notBefore=%s notAfter=%s now=%s",
			parsed.NotBefore, parsed.NotAfter, now)
	}

	// RFC 5480 §3: KeyEncipherment MUST NOT be set on an EC-keyed cert;
	// leaf server certs must not carry IsCA.
	if parsed.KeyUsage != x509.KeyUsageDigitalSignature {
		t.Errorf("KeyUsage = %b, want only DigitalSignature (%b)",
			parsed.KeyUsage, x509.KeyUsageDigitalSignature)
	}
	if parsed.IsCA {
		t.Error("IsCA = true on a leaf server cert")
	}
}

func TestGenerateOrphanCertCleanupOnKeyFailure(t *testing.T) {
	// If the key write fails after the cert write succeeded, the cert
	// must be removed so the next LoadOrGenerate mints a fresh pair
	// rather than tripping the "inconsistent TLS material" error.
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	// Point keyPath at a directory that doesn't exist AND can't be
	// created (a file at the parent). This makes writePEM(keyPath) fail
	// after cert write succeeds.
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("not a dir"), 0o600); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(blocker, "subdir", "key.pem")

	err := Generate(certPath, keyPath, "")
	if err == nil {
		t.Fatal("expected error when key path is unwritable")
	}
	if fileExists(certPath) {
		t.Error("cert left on disk after key-write failure — orphan state")
	}
}

func TestReloadsWithoutRegenerating(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := DefaultPaths(dir)

	_, fp1, err := LoadOrGenerate(certPath, keyPath, "host.local")
	if err != nil {
		t.Fatalf("first LoadOrGenerate: %v", err)
	}
	certMtime := mustMtime(t, certPath)

	// Small sleep so any mtime change would be visible on coarse-grained FSes.
	time.Sleep(10 * time.Millisecond)

	_, fp2, err := LoadOrGenerate(certPath, keyPath, "host.local")
	if err != nil {
		t.Fatalf("second LoadOrGenerate: %v", err)
	}
	if fp1 != fp2 {
		t.Errorf("fingerprint changed on reload: %q vs %q", fp1, fp2)
	}
	if mustMtime(t, certPath) != certMtime {
		t.Error("cert file was rewritten on reload — should have been loaded as-is")
	}
}

func TestInconsistentFilesError(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := DefaultPaths(dir)

	// Mint a pair, then delete the key to simulate a half-broken state.
	if _, _, err := LoadOrGenerate(certPath, keyPath, ""); err != nil {
		t.Fatalf("initial mint: %v", err)
	}
	if err := os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}

	_, _, err := LoadOrGenerate(certPath, keyPath, "")
	if err == nil {
		t.Fatal("expected error for cert-without-key")
	}
	if !strings.Contains(err.Error(), "inconsistent TLS material") {
		t.Errorf("error message: %v", err)
	}
}

func TestKeyFileIsMode0600(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix file permissions don't apply on Windows")
	}
	dir := t.TempDir()
	certPath, keyPath := DefaultPaths(dir)
	if _, _, err := LoadOrGenerate(certPath, keyPath, ""); err != nil {
		t.Fatalf("generate: %v", err)
	}
	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("key perms = %o, want 0600", mode)
	}
}

func TestFingerprintFormat(t *testing.T) {
	// 32 pairs (64 hex chars) separated by 31 colons = 95 chars, uppercase.
	pattern := regexp.MustCompile(`^([0-9A-F]{2}:){31}[0-9A-F]{2}$`)
	dir := t.TempDir()
	certPath, keyPath := DefaultPaths(dir)
	_, fp, err := LoadOrGenerate(certPath, keyPath, "")
	if err != nil {
		t.Fatal(err)
	}
	if !pattern.MatchString(fp) {
		t.Errorf("fingerprint %q doesn't match canonical form", fp)
	}
}

func TestFingerprintFromDERDeterministic(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := DefaultPaths(dir)
	_, fp, err := LoadOrGenerate(certPath, keyPath, "")
	if err != nil {
		t.Fatal(err)
	}
	// Recompute from the DER directly — must match.
	raw, _ := os.ReadFile(certPath)
	block, _ := pem.Decode(raw)
	fp2 := FingerprintFromDER(block.Bytes)
	if fp != fp2 {
		t.Errorf("fingerprint mismatch: PEM=%q, DER=%q", fp, fp2)
	}
}

// TestHandshakeWithPinnedFingerprint proves the generated cert actually works
// in a live TLS handshake — spin up an httptest server with it, then connect
// a client that pins to the reported fingerprint and verifies the body arrives.
// This is the closest we can get to an end-to-end test of the iOS pairing
// flow inside the Go test suite.
func TestHandshakeWithPinnedFingerprint(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := DefaultPaths(dir)
	cert, wantFP, err := LoadOrGenerate(certPath, keyPath, "localhost")
	if err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{*cert}}
	srv.StartTLS()
	defer srv.Close()

	var gotFP string
	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true, // we verify via VerifyConnection below
				VerifyConnection: func(s tls.ConnectionState) error {
					if len(s.PeerCertificates) == 0 {
						return errCertsEmpty
					}
					gotFP = FingerprintFromDER(s.PeerCertificates[0].Raw)
					return nil
				},
			},
		},
	}
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "ok" {
		t.Errorf("body = %q, want %q", body, "ok")
	}
	if gotFP != wantFP {
		t.Errorf("server fingerprint = %q, want %q", gotFP, wantFP)
	}
}

var errCertsEmpty = &stringError{"no peer certs in handshake"}

type stringError struct{ s string }

func (e *stringError) Error() string { return e.s }

func TestDefaultPaths(t *testing.T) {
	cert, key := DefaultPaths("/var/bridge")
	if cert != filepath.Join("/var/bridge", CertFileName) {
		t.Errorf("cert path = %q", cert)
	}
	if key != filepath.Join("/var/bridge", KeyFileName) {
		t.Errorf("key path = %q", key)
	}
}

func TestDNSNamesNoDuplicateLocalhost(t *testing.T) {
	// If hostname == "localhost", we shouldn't list it twice.
	got := dnsNames("localhost")
	if len(got) != 1 || got[0] != "localhost" {
		t.Errorf("dnsNames(localhost) = %v, want [localhost]", got)
	}
}

func TestDNSNamesIncludesCustomHost(t *testing.T) {
	got := dnsNames("music-server.local")
	if !slices.Contains(got, "music-server.local") {
		t.Errorf("dnsNames missing custom host: %v", got)
	}
	if !slices.Contains(got, "localhost") {
		t.Errorf("dnsNames always includes localhost: %v", got)
	}
}

// ---- helpers ----

func mustMtime(t *testing.T, path string) time.Time {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.ModTime()
}

func containsIP(ips []net.IP, want string) bool {
	for _, ip := range ips {
		if ip.String() == want {
			return true
		}
	}
	return false
}

func TestParseHostFromURL_StripsSchemeAndPort(t *testing.T) {
	cases := []struct {
		in       string
		wantHost string
		wantIP   bool
	}{
		{"https://foo.example.com:7788", "foo.example.com", false},
		{"https://192.168.1.10:7788", "192.168.1.10", true},
		{"https://[fe80::1]:7788", "fe80::1", true},
		{"https://bare.example.com", "bare.example.com", false},
		{"not-a-url", "", false},
		{"https://", "", false},
	}
	for _, c := range cases {
		gotHost, gotIP := ParseHostFromURL(c.in)
		if gotHost != c.wantHost || gotIP != c.wantIP {
			t.Errorf("ParseHostFromURL(%q) = (%q, %v), want (%q, %v)",
				c.in, gotHost, gotIP, c.wantHost, c.wantIP)
		}
	}
}

func TestGenerateWithOptions_IncludesExtraSANs(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "server.crt")
	keyPath := filepath.Join(dir, "server.key")
	opts := GenerateOptions{
		Hostname:      "host.example.com",
		ExtraDNSNames: []string{"magic.tailfoo.ts.net", "my-bridge.example.com"},
		ExtraIPs: []net.IP{
			net.ParseIP("100.91.73.88"),
			net.ParseIP("192.168.1.10"),
		},
	}
	if err := GenerateWithOptions(certPath, keyPath, opts); err != nil {
		t.Fatalf("GenerateWithOptions: %v", err)
	}
	// Parse the cert and inspect SAN slots.
	raw, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatalf("read cert: %v", err)
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		t.Fatalf("no PEM block")
	}
	parsed, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse cert: %v", err)
	}

	// DNSNames must include localhost (default), the hostname, and
	// the operator extras.
	wantDNS := []string{"localhost", "host.example.com", "magic.tailfoo.ts.net", "my-bridge.example.com"}
	for _, want := range wantDNS {
		found := false
		for _, n := range parsed.DNSNames {
			if strings.EqualFold(n, want) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("DNSNames missing %q; got %v", want, parsed.DNSNames)
		}
	}

	// IPAddresses must include the loopback defaults plus the operator extras.
	wantIPs := []net.IP{
		net.IPv4(127, 0, 0, 1),
		net.IPv6loopback,
		net.IPv4zero,
		net.ParseIP("100.91.73.88"),
		net.ParseIP("192.168.1.10"),
	}
	for _, want := range wantIPs {
		found := false
		for _, ip := range parsed.IPAddresses {
			if ip.Equal(want) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("IPAddresses missing %v; got %v", want, parsed.IPAddresses)
		}
	}
}

func TestMergeDNSNames_DedupesCaseInsensitively(t *testing.T) {
	got := mergeDNSNames("Host.Example.Com", []string{
		"host.example.com",    // dup of hostname (case-fold)
		"localhost",           // dup of default
		"new.example.com",     // kept
		"  new.example.com  ", // dup with whitespace
		"",                    // dropped
	})
	// Expect: localhost, Host.Example.Com, new.example.com (3 entries).
	if len(got) != 3 {
		t.Errorf("merged DNSNames = %v, want 3 deduped entries", got)
	}
}

func TestMergeIPs_DedupesByCanonicalForm(t *testing.T) {
	got := mergeIPs([]net.IP{
		net.ParseIP("127.0.0.1"), // dup of default
		net.ParseIP("::1"),       // dup of default
		net.ParseIP("10.0.0.5"),  // kept
		net.ParseIP("10.0.0.5"),  // dup
		nil,                      // dropped
	})
	if len(got) != 4 { // 127.0.0.1, ::1, 0.0.0.0, 10.0.0.5
		t.Errorf("merged IPs = %v, want 4 deduped entries", got)
	}
}
