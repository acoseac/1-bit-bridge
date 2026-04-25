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

	validity := parsed.NotAfter.Sub(parsed.NotBefore)
	if validity < 9*365*24*time.Hour {
		t.Errorf("validity too short: %v", validity)
	}
	if parsed.NotAfter.Before(time.Now().Add(9 * 365 * 24 * time.Hour)) {
		t.Errorf("NotAfter %v is sooner than expected", parsed.NotAfter)
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
