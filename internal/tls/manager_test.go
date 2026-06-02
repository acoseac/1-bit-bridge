package tls

import (
	cryptotls "crypto/tls"
	"path/filepath"
	"testing"
	"time"
)

// mintTestCert produces a `tls.Certificate` with `Leaf` populated
// for the manager tests. Uses the production `GenerateWithOptions`
// path (397-day validity) so test certs are byte-shape-compatible
// with what the bridge serves.
//
// **No lifetime override**: every consumer in this file uses the
// default 397-day window. Pre-fix this helper accepted a
// `lifetime` arg with a no-op `mintCustomLifetime` fallback that
// returned the original disk-loaded cert (CodeRabbit on PR #102).
// The expiry-based SNI-switcher branches (the freshness check)
// are covered by the on-disk cert-mutation tests in
// internal/tailscale/, not by an in-memory mutate that crypto/tls
// wouldn't actually honour at handshake time. Keeping this
// signature lifetime-free prevents future callers from adding a
// silent regression by passing a non-zero value.
func mintTestCert(t *testing.T, dnsNames []string) *cryptotls.Certificate {
	t.Helper()
	dir := t.TempDir()
	certPath := filepath.Join(dir, "leaf.crt")
	keyPath := filepath.Join(dir, "leaf.key")
	opts := GenerateOptions{Hostname: "host.local"}
	for _, n := range dnsNames {
		opts.ExtraDNSNames = append(opts.ExtraDNSNames, n)
	}
	if err := GenerateWithOptions(certPath, keyPath, opts); err != nil {
		t.Fatal(err)
	}
	cert, err := LoadTailscaleCertFromDisk(certPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

// silence unused-import warning; time is referenced in tests below
// even after the lifetime helper was removed.
var _ = time.Hour

// --- CertNotAfter ---

func TestCertNotAfter_ParsedFromLeafSafely(t *testing.T) {
	// Gotcha #1 from the plan review: tls.LoadX509KeyPair returns a
	// `tls.Certificate` with `Leaf == nil`. CertNotAfter MUST parse
	// the DER itself rather than read `cert.Leaf.NotAfter` directly.
	// This test forces the nil-Leaf shape and asserts the helper
	// still returns a valid time.
	cert := mintTestCert(t, []string{"host.local"})
	cert.Leaf = nil // simulate fresh LoadX509KeyPair output

	when, err := CertNotAfter(cert)
	if err != nil {
		t.Fatalf("CertNotAfter on nil-Leaf cert returned err = %v, want nil (parsing must work without populated Leaf)", err)
	}
	if when.IsZero() {
		t.Error("CertNotAfter returned zero time")
	}
	if when.Before(time.Now()) {
		t.Errorf("CertNotAfter = %v, want a future time", when)
	}
}

func TestCertNotAfter_NilCertReturnsError(t *testing.T) {
	if _, err := CertNotAfter(nil); err == nil {
		t.Error("CertNotAfter(nil) returned nil err, want non-nil")
	}
}

func TestCertNotAfter_EmptyDerReturnsError(t *testing.T) {
	cert := &cryptotls.Certificate{}
	if _, err := CertNotAfter(cert); err == nil {
		t.Error("CertNotAfter on empty Certificate returned nil err, want non-nil")
	}
}

// --- LoadTailscaleCertFromDisk ---

func TestLoadTailscaleCertFromDisk_PopulatesLeaf(t *testing.T) {
	// LoadTailscaleCertFromDisk's whole reason to exist is to populate
	// `cert.Leaf` so callers can read NotAfter without falling into
	// the LoadX509KeyPair-sets-nil-Leaf trap. Lock that contract.
	dir := t.TempDir()
	certPath := filepath.Join(dir, "x.crt")
	keyPath := filepath.Join(dir, "x.key")
	if err := GenerateWithOptions(certPath, keyPath, GenerateOptions{Hostname: "host.local"}); err != nil {
		t.Fatal(err)
	}
	cert, err := LoadTailscaleCertFromDisk(certPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if cert.Leaf == nil {
		t.Fatal("Leaf is nil after LoadTailscaleCertFromDisk; the helper's whole purpose is to populate it")
	}
	if cert.Leaf.NotAfter.IsZero() {
		t.Error("Leaf.NotAfter is zero after parse")
	}
}

// --- Manager ---

func TestManager_GetReturnsSelfSignedWhenNoSNI(t *testing.T) {
	// No SNI is rare in 2026 (TLS 1.2+ clients almost always send it)
	// but legal per RFC 6066. The manager falls through to self-signed
	// rather than guessing — the LE cert is for explicit magic-DNS
	// hostnames only.
	self := mintTestCert(t, []string{"host.local"})
	mgr := NewManager(self)
	mgr.SetMagicDNSSuffix("sable-eagle.ts.net")
	mgr.SetTailscaleCert(mintTestCert(t, []string{"home-pc.sable-eagle.ts.net"}))

	got, err := mgr.Get(&cryptotls.ClientHelloInfo{ServerName: ""})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != self {
		t.Errorf("no-SNI Get returned a non-self-signed cert; want self-signed fallback")
	}
}

func TestManager_GetReturnsLECertOnMagicDNSSNI(t *testing.T) {
	self := mintTestCert(t, []string{"host.local"})
	le := mintTestCert(t, []string{"home-pc.sable-eagle.ts.net"})
	mgr := NewManager(self)
	mgr.SetMagicDNSSuffix("sable-eagle.ts.net")
	mgr.SetTailscaleCert(le)

	got, err := mgr.Get(&cryptotls.ClientHelloInfo{ServerName: "home-pc.sable-eagle.ts.net"})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != le {
		t.Errorf("magic-DNS SNI returned wrong cert; want LE cert")
	}
}

func TestManager_GetFallsThroughToSelfSignedWhenLECertMissing(t *testing.T) {
	// LE cert not loaded yet (auto-pilot still detecting / mint
	// failed). Manager must fall through to self-signed for the
	// magic-DNS SNI — ATS will reject on the iOS side, exactly the
	// same as today's no-LE-cert state. Honest fallback, no surprise.
	self := mintTestCert(t, []string{"host.local"})
	mgr := NewManager(self)
	mgr.SetMagicDNSSuffix("sable-eagle.ts.net")
	// Deliberately no SetTailscaleCert.

	got, err := mgr.Get(&cryptotls.ClientHelloInfo{ServerName: "home-pc.sable-eagle.ts.net"})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != self {
		t.Errorf("missing LE cert + magic-DNS SNI returned non-self-signed cert; want self-signed fallback")
	}
}

func TestManager_GetReturnsSelfSignedForLANSNI(t *testing.T) {
	// LAN / mDNS / IP-literal SNI must always route to self-signed
	// — these are the connections iOS pins by fingerprint, and
	// serving the LE cert on those would break every existing pin.
	self := mintTestCert(t, []string{"host.local"})
	le := mintTestCert(t, []string{"home-pc.sable-eagle.ts.net"})
	mgr := NewManager(self)
	mgr.SetMagicDNSSuffix("sable-eagle.ts.net")
	mgr.SetTailscaleCert(le)

	for _, sni := range []string{
		"home-pc.local",
		"192.168.1.5",
		"127.0.0.1",
		"my-hostname",
		"sable-eagle.ts.net.malicious.example", // not actually under the suffix
	} {
		got, err := mgr.Get(&cryptotls.ClientHelloInfo{ServerName: sni})
		if err != nil {
			t.Fatalf("Get(%q): %v", sni, err)
		}
		if got != self {
			t.Errorf("SNI %q returned wrong cert; want self-signed", sni)
		}
	}
}

func TestManager_GetCaseInsensitiveSNI(t *testing.T) {
	// SNI hostnames are case-insensitive per RFC 6066.
	self := mintTestCert(t, []string{"host.local"})
	le := mintTestCert(t, []string{"home-pc.sable-eagle.ts.net"})
	mgr := NewManager(self)
	mgr.SetMagicDNSSuffix("sable-eagle.ts.net")
	mgr.SetTailscaleCert(le)

	got, err := mgr.Get(&cryptotls.ClientHelloInfo{ServerName: "HOME-PC.SABLE-EAGLE.TS.NET"})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != le {
		t.Error("uppercase magic-DNS SNI didn't match suffix")
	}
}

func TestManager_GetEmptySuffixDisablesLERouting(t *testing.T) {
	// Tailscale not detected (or MagicDNS not enabled) → empty
	// suffix → every SNI routes to self-signed regardless of LE
	// cert presence.
	self := mintTestCert(t, []string{"host.local"})
	le := mintTestCert(t, []string{"home-pc.sable-eagle.ts.net"})
	mgr := NewManager(self)
	// SetMagicDNSSuffix("") — explicitly empty.
	mgr.SetMagicDNSSuffix("")
	mgr.SetTailscaleCert(le)

	got, err := mgr.Get(&cryptotls.ClientHelloInfo{ServerName: "home-pc.sable-eagle.ts.net"})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != self {
		t.Errorf("empty MagicDNS suffix didn't disable LE routing")
	}
}

func TestManager_NewManagerPanicsOnNilSelfSigned(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("NewManager(nil) didn't panic; want loud failure on programmer error")
		}
	}()
	_ = NewManager(nil)
}

func TestManager_GetServesAutocertCertOnExactDomainSNI(t *testing.T) {
	// PR 3: when an autocert provider is wired and the SNI
	// matches the configured public domain exactly, the
	// autocert cert wins — not the self-signed.
	self := mintTestCert(t, []string{"host.local"})
	acmeCert := mintTestCert(t, []string{"bridge.example.com"})
	mgr := NewManager(self)
	mgr.SetAutocertProvider(
		"bridge.example.com",
		func(hi *cryptotls.ClientHelloInfo) (*cryptotls.Certificate, error) {
			return acmeCert, nil
		},
		[]string{"acme-tls/1"},
	)

	got, err := mgr.Get(&cryptotls.ClientHelloInfo{ServerName: "bridge.example.com"})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != acmeCert {
		t.Error("autocert path didn't win against exact-domain SNI")
	}
}

func TestManager_GetNormalizesUppercaseAndTrailingDotSNI(t *testing.T) {
	// SNI ClientHello can carry trailing dot + mixed case. The
	// normalization at the top of Get must lowercase + strip the
	// dot so the autocert comparison matches.
	self := mintTestCert(t, []string{"host.local"})
	acmeCert := mintTestCert(t, []string{"bridge.example.com"})
	mgr := NewManager(self)
	mgr.SetAutocertProvider(
		"bridge.example.com",
		func(hi *cryptotls.ClientHelloInfo) (*cryptotls.Certificate, error) {
			return acmeCert, nil
		},
		nil,
	)

	for _, sni := range []string{"BRIDGE.EXAMPLE.COM", "bridge.example.com.", "Bridge.Example.Com.", "BRIDGE.EXAMPLE.COM."} {
		got, err := mgr.Get(&cryptotls.ClientHelloInfo{ServerName: sni})
		if err != nil {
			t.Fatalf("Get(%q): %v", sni, err)
		}
		if got != acmeCert {
			t.Errorf("SNI normalization missed %q (got self-signed instead of autocert)", sni)
		}
	}
}

func TestManager_GetFallsThroughToSelfSignedOnAutocertError(t *testing.T) {
	// When autocert.GetCertificate errors (DNS not propagated,
	// LE rate-limited, etc.), serve self-signed rather than
	// 500'ing the TLS handshake. iOS clients on the LE path will
	// see ATS rejection on the self-signed — observable, but no
	// worse than a config-time refuse-to-start (which would lock
	// the operator out completely while LE propagated).
	self := mintTestCert(t, []string{"host.local"})
	mgr := NewManager(self)
	mgr.SetAutocertProvider(
		"bridge.example.com",
		func(hi *cryptotls.ClientHelloInfo) (*cryptotls.Certificate, error) {
			return nil, errSentinel
		},
		nil,
	)

	got, err := mgr.Get(&cryptotls.ClientHelloInfo{ServerName: "bridge.example.com"})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != self {
		t.Errorf("autocert error: got %p, want self-signed %p", got, self)
	}
}

func TestManager_GetIgnoresAutocertForMismatchedSNI(t *testing.T) {
	// SNI doesn't match the configured autocert domain → fall
	// through to Tailscale / self-signed routing (the autocert
	// hook is NOT invoked).
	self := mintTestCert(t, []string{"host.local"})
	autocertCalled := false
	mgr := NewManager(self)
	mgr.SetAutocertProvider(
		"bridge.example.com",
		func(hi *cryptotls.ClientHelloInfo) (*cryptotls.Certificate, error) {
			autocertCalled = true
			return nil, nil
		},
		nil,
	)
	got, err := mgr.Get(&cryptotls.ClientHelloInfo{ServerName: "attacker.example"})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != self {
		t.Errorf("mismatched SNI: got %p, want self-signed %p", got, self)
	}
	if autocertCalled {
		t.Error("autocert hook called for non-matching SNI — defense in depth violated")
	}
}

func TestManager_SetAutocertProviderClearWithNilGetCert(t *testing.T) {
	// SetAutocertProvider(..., nil, ...) clears the route — the
	// hot path should behave as if autocert was never configured.
	self := mintTestCert(t, []string{"host.local"})
	mgr := NewManager(self)
	mgr.SetAutocertProvider(
		"bridge.example.com",
		func(hi *cryptotls.ClientHelloInfo) (*cryptotls.Certificate, error) {
			return mintTestCert(t, []string{"bridge.example.com"}), nil
		},
		nil,
	)
	mgr.SetAutocertProvider("", nil, nil)

	got, err := mgr.Get(&cryptotls.ClientHelloInfo{ServerName: "bridge.example.com"})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != self {
		t.Error("after Clear, autocert SNI still routes to autocert cert")
	}
	if np := mgr.NextProtos(); len(np) != 0 {
		t.Errorf("after Clear, NextProtos() = %v, want empty", np)
	}
}

// --- FingerprintForServerName ---

func TestManager_FingerprintForServerName_SelfSignedForLANHost(t *testing.T) {
	// A LAN / IP / .local host gets the self-signed cert, so its
	// fingerprint MUST be the self-signed cert's — the pairing-QR baker
	// relies on this for the loopback-mode default dial URL.
	self := mintTestCert(t, []string{"host.local"})
	mgr := NewManager(self)

	got := mgr.FingerprintForServerName("home-pc.local")
	want := FingerprintFromDER(self.Certificate[0])
	if got != want {
		t.Errorf("LAN host fingerprint = %q, want self-signed %q", got, want)
	}
}

func TestManager_FingerprintForServerName_AutocertForPublicDomain(t *testing.T) {
	// THE fix: a device dialing the public domain receives the autocert
	// LE cert, so the QR must advertise THAT fingerprint — not the
	// self-signed LAN pin (which the device would never see, making the
	// iOS first-contact pin check fail).
	self := mintTestCert(t, []string{"host.local"})
	acmeCert := mintTestCert(t, []string{"bridge.example.com"})
	mgr := NewManager(self)
	mgr.SetAutocertProvider(
		"bridge.example.com",
		func(*cryptotls.ClientHelloInfo) (*cryptotls.Certificate, error) { return acmeCert, nil },
		nil,
	)
	// Served-cert accessor: the fingerprint MUST come from the cert the
	// listener actually serves (read passively), NOT from GetCertificate
	// with a synthetic hello (which returns a different leaf).
	mgr.SetAutocertCachedCertFn(func() *cryptotls.Certificate { return acmeCert })

	got := mgr.FingerprintForServerName("bridge.example.com")
	want := FingerprintFromDER(acmeCert.Certificate[0])
	if got != want {
		t.Errorf("public-domain fingerprint = %q, want served autocert %q", got, want)
	}
	if got == FingerprintFromDER(self.Certificate[0]) {
		t.Error("public-domain host resolved to the self-signed fingerprint — the bug this fix closes")
	}
}

func TestManager_FingerprintForServerName_NormalizesSNI(t *testing.T) {
	// Mirrors Get's normalization: mixed case + trailing dot still
	// resolve to the autocert cert (operators may type the URL host in
	// any case).
	self := mintTestCert(t, []string{"host.local"})
	acmeCert := mintTestCert(t, []string{"bridge.example.com"})
	mgr := NewManager(self)
	mgr.SetAutocertProvider(
		"bridge.example.com",
		func(*cryptotls.ClientHelloInfo) (*cryptotls.Certificate, error) { return acmeCert, nil },
		nil,
	)
	mgr.SetAutocertCachedCertFn(func() *cryptotls.Certificate { return acmeCert })

	want := FingerprintFromDER(acmeCert.Certificate[0])
	for _, sni := range []string{"BRIDGE.EXAMPLE.COM", "bridge.example.com.", "Bridge.Example.Com."} {
		if got := mgr.FingerprintForServerName(sni); got != want {
			t.Errorf("FingerprintForServerName(%q) = %q, want autocert %q", sni, got, want)
		}
	}
}

func TestManager_FingerprintForServerName_FallsBackToSelfSignedWhenNoServedCert(t *testing.T) {
	// Autocert domain configured but the served-cert accessor is unset
	// (cert not minted yet / older wiring) OR returns nil → fall back to
	// the self-signed fingerprint rather than a synthetic-hello mint.
	self := mintTestCert(t, []string{"host.local"})
	mgr := NewManager(self)
	mgr.SetAutocertProvider(
		"bridge.example.com",
		func(*cryptotls.ClientHelloInfo) (*cryptotls.Certificate, error) { return nil, errSentinel },
		nil,
	)
	want := FingerprintFromDER(self.Certificate[0])

	// (a) accessor never wired
	if got := mgr.FingerprintForServerName("bridge.example.com"); got != want {
		t.Errorf("no-accessor fingerprint = %q, want self-signed fallback %q", got, want)
	}
	// (b) accessor wired but returns nil (cert not minted yet)
	mgr.SetAutocertCachedCertFn(func() *cryptotls.Certificate { return nil })
	if got := mgr.FingerprintForServerName("bridge.example.com"); got != want {
		t.Errorf("nil-served-cert fingerprint = %q, want self-signed fallback %q", got, want)
	}
}

func TestManager_FingerprintForServerName_EmptyHostIsSelfSigned(t *testing.T) {
	self := mintTestCert(t, []string{"host.local"})
	mgr := NewManager(self)
	got := mgr.FingerprintForServerName("")
	if want := FingerprintFromDER(self.Certificate[0]); got != want {
		t.Errorf("empty-SNI fingerprint = %q, want self-signed %q", got, want)
	}
}

func TestManager_AdminTLSConfigMergesAutocertALPN(t *testing.T) {
	// AdminTLSConfig must advertise both http/1.1 + h2 AND any
	// autocert-required ALPN (acme-tls/1) so the same listener
	// handles real admin traffic AND the TLS-ALPN-01 challenge.
	self := mintTestCert(t, []string{"host.local"})
	mgr := NewManager(self)
	mgr.SetAutocertProvider(
		"bridge.example.com",
		func(*cryptotls.ClientHelloInfo) (*cryptotls.Certificate, error) {
			return self, nil
		},
		[]string{"acme-tls/1"},
	)
	cfg := mgr.AdminTLSConfig()
	if cfg == nil {
		t.Fatal("AdminTLSConfig returned nil")
	}
	if cfg.GetCertificate == nil {
		t.Error("AdminTLSConfig.GetCertificate is nil")
	}
	wantAny := map[string]bool{"h2": false, "http/1.1": false, "acme-tls/1": false}
	for _, p := range cfg.NextProtos {
		if _, ok := wantAny[p]; ok {
			wantAny[p] = true
		}
	}
	for p, found := range wantAny {
		if !found {
			t.Errorf("AdminTLSConfig.NextProtos missing %q (got %v)", p, cfg.NextProtos)
		}
	}
}

// errSentinel is a sentinel error for the autocert-error fall-through test.
var errSentinel = errSentinelT{}

type errSentinelT struct{}

func (errSentinelT) Error() string { return "sentinel autocert error" }
