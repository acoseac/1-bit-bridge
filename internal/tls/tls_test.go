package tls

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
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

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
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
	// With the two-phase commit, a key-write failure means the cert is
	// never renamed into place (both PEMs are staged first, then committed
	// together), so certPath must not exist afterward — no orphan cert for
	// the next LoadOrGenerate to trip over as "inconsistent TLS material".
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	// Point keyPath at a directory that doesn't exist AND can't be
	// created (a file at the parent). This makes staging the key PEM fail
	// while the cert PEM stages successfully.
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

func TestGeneratePreservesExistingOnWriteFailure(t *testing.T) {
	// writePEM commits each PEM via temp-file + rename, so a rename failure
	// during (re)generation must leave any pre-existing cert/key
	// byte-identical on disk — the bridge stays bootable instead of being
	// left with a truncated/half-written pair. Pins the atomicity contract
	// (and, with cmd/bridge/cert.go no longer pre-removing, the
	// crash-safety of `bridge cert rotate`).
	dir := t.TempDir()
	certPath, keyPath := DefaultPaths(dir)

	if _, _, err := LoadOrGenerate(certPath, keyPath, "myhost.local"); err != nil {
		t.Fatalf("initial LoadOrGenerate: %v", err)
	}
	origCert, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatal(err)
	}
	origKey, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}

	// Force every rename to fail — simulates disk-full / interruption at
	// the atomic commit step. The cert write attempts its rename first and
	// errors out, so the key write is never reached.
	restore := atomicwrite.SetRenameFuncForTest(func(_, _ string) error {
		return errors.New("simulated rename failure")
	})
	defer atomicwrite.SetRenameFuncForTest(restore)

	if err := Generate(certPath, keyPath, "myhost.local"); err == nil {
		t.Fatal("expected Generate to fail when the rename fails")
	}

	// The pre-existing pair must be intact and unchanged.
	gotCert, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatalf("cert missing after failed regeneration: %v", err)
	}
	if !bytes.Equal(gotCert, origCert) {
		t.Error("cert changed after a failed regeneration — atomicity violated")
	}
	gotKey, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("key missing after failed regeneration: %v", err)
	}
	if !bytes.Equal(gotKey, origKey) {
		t.Error("key changed after a failed regeneration — atomicity violated")
	}

	// No scratch .pem-*.tmp file must leak from the failed write.
	certDir := filepath.Dir(certPath)
	entries, err := os.ReadDir(certDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".pem-") && strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("leaked temp file after failed rename: %s", e.Name())
		}
	}
}

func TestGenerateKeyFailureDoesNotDeleteCert(t *testing.T) {
	// Gemini HIGH (PR #487): a key-write failure must never leave the
	// system with NO cert. The two-phase stage-both-then-rename-both commit
	// means the cert is either preserved (old) or committed (new) — never
	// deleted. Here the cert rename succeeds but the key rename fails; a
	// cert file must still exist afterward. The old orphan-cleanup
	// (os.Remove(certPath) on key failure) would have deleted it, leaving
	// the bridge with no cert at all.
	dir := t.TempDir()
	certPath, keyPath := DefaultPaths(dir)
	if _, _, err := LoadOrGenerate(certPath, keyPath, "myhost.local"); err != nil {
		t.Fatalf("initial LoadOrGenerate: %v", err)
	}

	// Fail only the KEY rename; the cert rename commits normally.
	restore := atomicwrite.SetRenameFuncForTest(func(src, dst string) error {
		if dst == keyPath {
			return errors.New("simulated key rename failure")
		}
		return os.Rename(src, dst)
	})
	defer atomicwrite.SetRenameFuncForTest(restore)

	if err := Generate(certPath, keyPath, "myhost.local"); err == nil {
		t.Fatal("expected Generate to fail when the key rename fails")
	}

	// The crux of the fix: a cert file MUST still exist (not deleted).
	if !fileExists(certPath) {
		t.Error("cert file was deleted after a key-write failure — the exact bug the two-phase commit prevents")
	}
	// No scratch .pem-*.tmp files must leak.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".pem-") && strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("leaked temp file after key-rename failure: %s", e.Name())
		}
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

func TestFingerprintFromDER_KnownVector(t *testing.T) {
	// SHA-256 of empty input is a well-known constant; pin the exact
	// colon-uppercase-hex output so a nibble-math regression in the
	// byte-iteration formatter is caught (the format-only test passes on
	// any well-formed hex, even wrong values).
	const want = "E3:B0:C4:42:98:FC:1C:14:9A:FB:F4:C8:99:6F:B9:24:27:AE:41:E4:64:9B:93:4C:A4:95:99:1B:78:52:B8:55"
	if got := FingerprintFromDER(nil); got != want {
		t.Errorf("FingerprintFromDER(nil) = %q, want %q", got, want)
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

	// DNSNames must include localhost (default), the hostname, the
	// auto-derived `<short>.local` (Qodo bot review on PR #93), and
	// the operator extras.
	wantDNS := []string{"localhost", "host.example.com", "host.local", "magic.tailfoo.ts.net", "my-bridge.example.com"}
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
	// Expect: localhost, Host.Example.Com, Host.local, new.example.com
	// (4 entries — `<short>.local` is auto-added by dnsNames since
	// PR #93 round 1).
	if len(got) != 4 {
		t.Errorf("merged DNSNames = %v, want 4 deduped entries", got)
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

func TestMergeIPs_DropsMalformedIP(t *testing.T) {
	// A non-nil but wrong-length net.IP has To16()==nil; it must be
	// dropped, not collapsed into the "" map key (which would alias every
	// malformed entry into one slot).
	got := mergeIPs([]net.IP{
		{1, 2, 3},               // 3 bytes — To16()==nil
		{4, 5, 6, 7, 8},         // 5 bytes — To16()==nil
		net.ParseIP("10.0.0.9"), // kept
	})
	// defaults: 127.0.0.1, ::1, 0.0.0.0 (3) + 10.0.0.9 (1) = 4
	if len(got) != 4 {
		t.Errorf("merged = %v (len %d), want 4 (malformed dropped, no '' collision)", got, len(got))
	}
}

// TestDNSNames_AppendsDotLocal pins the SAN-mismatch fix from Qodo
// PR #93 round 1: every hostname surfaces a `<shortLabel>.local`
// twin so the cert covers the mDNS URL `advertise.Endpoints` emits.
func TestDNSNames_AppendsDotLocal(t *testing.T) {
	cases := []struct {
		hostname string
		want     []string
	}{
		// Bare short hostname → adds `host.local`.
		{"box", []string{"localhost", "box", "box.local"}},
		// FQDN → strip suffix, then add `<short>.local`.
		{"box.example.com", []string{"localhost", "box.example.com", "box.local"}},
		// Already `.local`-suffixed → no duplicate.
		{"mac.local", []string{"localhost", "mac.local"}},
		// Empty hostname → only localhost.
		{"", []string{"localhost"}},
		// "localhost" → only localhost.
		{"localhost", []string{"localhost"}},
	}
	for _, c := range cases {
		t.Run(c.hostname, func(t *testing.T) {
			got := dnsNames(c.hostname)
			if len(got) != len(c.want) {
				t.Fatalf("len = %d, want %d: got %v", len(got), len(c.want), got)
			}
			for i, w := range c.want {
				if got[i] != w {
					t.Errorf("got[%d] = %q, want %q (full got=%v)", i, got[i], w, got)
				}
			}
		})
	}
}
