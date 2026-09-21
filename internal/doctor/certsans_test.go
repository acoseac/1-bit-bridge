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
	"runtime"
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
	// The COUNTS are pinned because the README's worked example got them
	// by hand and got the denominator wrong: the wanted name set is the
	// MERGED one, so `localhost` is in the total even though the cert
	// carries it. Three of four names, three of six addresses.
	for _, want := range []string{"stale", "3 of 4 name(s)", "3 of 6 address(es)"} {
		if !strings.Contains(c.Summary, want) {
			t.Errorf("summary = %q, want it to contain %q", c.Summary, want)
		}
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
// load this one, which is the fail side of the split.
func TestCheckTLSCert_UnreadableCertIsNotPresent(t *testing.T) {
	d := certFixture(t, newHostEndpoints, newHostEndpoints)
	certPath, _ := servertls.DefaultPaths(d.DataDir)
	if err := os.WriteFile(certPath, []byte("not a certificate"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := checkTLSCert(t.Context(), d)
	if c.Status != Fail || !strings.Contains(c.Summary, "unreadable") {
		t.Errorf("status = %q, summary = %q; want fail about an unreadable cert", c.Status, c.Summary)
	}
}

// TestCheckTLSCert_MismatchedPairFails — a certificate that parses says
// nothing about the key beside it. This is the residual state
// GenerateWithOptions' own docblock records: it commits the two files
// in two renames, and a crash between them leaves a new cert with the
// old key. Before this, doctor read the cert alone and answered
// `present, expires in 396 days` about a bridge that exits on startup.
func TestCheckTLSCert_MismatchedPairFails(t *testing.T) {
	d := certFixture(t, newHostEndpoints, newHostEndpoints)
	_, keyPath := servertls.DefaultPaths(d.DataDir)
	// Another install's key — same shape, wrong key.
	other := certFixture(t, newHostEndpoints, newHostEndpoints)
	_, otherKey := servertls.DefaultPaths(other.DataDir)
	raw, err := os.ReadFile(otherKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	c := checkTLSCert(t.Context(), d)
	if c.Status != Fail {
		t.Fatalf("status = %q, want fail (%q)", c.Status, c.Summary)
	}
	if !strings.Contains(c.Summary, "not a pair") {
		t.Errorf("summary = %q, want it to name the mismatch", c.Summary)
	}
	// The recovery has to be the command that re-mints BOTH files;
	// `bridge cert rotate` reads the old cert only best-effort, so it
	// still works on a pair that will not load.
	if !strings.Contains(c.Hint, "bridge cert rotate") {
		t.Errorf("hint does not name the recovery: %q", c.Hint)
	}

	// NEGATIVE CONTROL: the untouched fixture is a real pair and passes.
	if c := checkTLSCert(t.Context(), other); c.Status != OK {
		t.Errorf("a matched pair = %q, want ok (%q)", c.Status, c.Summary)
	}
}

// TestCheckTLSCert_AnUnreadableKeyIsNotAFinding — the key is 0600 and
// owned by the service user. On the public-mode layout the operator
// running `bridge doctor` is somebody else, and the bridge reads it
// perfectly well; failing there would be a preflight that fails on
// every such host, and `bridge init` bails on a fail. Expiry still
// grades, because it comes from the 0644 cert.
func TestCheckTLSCert_AnUnreadableKeyIsNotAFinding(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mode bits do not deny reads on Windows")
	}
	d := certFixture(t, newHostEndpoints, newHostEndpoints)
	_, keyPath := servertls.DefaultPaths(d.DataDir)
	if err := os.Chmod(keyPath, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(keyPath, 0o600) })
	// Root ignores the mode, so the fixture would not reproduce the
	// state and the assertion would pass for the wrong reason.
	if _, err := os.ReadFile(keyPath); err == nil {
		t.Skip("this user can read a 0000 file (root?) — the fixture cannot reproduce the state")
	}

	c := checkTLSCert(t.Context(), d)
	if c.Status != OK {
		t.Errorf("status = %q, want ok — an unreadable key is a fact about this run, not the bridge (%q / %q)",
			c.Status, c.Summary, c.Hint)
	}
	if !strings.Contains(c.Summary, "expires in") {
		t.Errorf("summary = %q, want expiry still graded from the cert", c.Summary)
	}
}

// TestCheckTLSCert_WarningBoundaryIsTheExactRemainingDuration —
// DaysUntilExpiry truncates toward zero, so a certificate with 30 days
// and 23 hours left reads as 30. Grading on `days*24h <=
// ExpiryWarningWindow` would warn here while `logIfExpiringSoon`, which
// compares time.Until(NotAfter), stays quiet: a 23-hour window in which
// the doctor and the next `bridge serve` disagree, which is the one
// thing this grading exists not to do.
func TestCheckTLSCert_WarningBoundaryIsTheExactRemainingDuration(t *testing.T) {
	d := certFixture(t, newHostEndpoints, newHostEndpoints)
	certPath, _ := servertls.DefaultPaths(d.DataDir)
	notAfter := time.Now().Add(servertls.ExpiryWarningWindow + 23*time.Hour)
	writeCertWithNotAfter(t, certPath, notAfter)

	// The fixture has to be a value the two forms disagree about, or the
	// test pins nothing.
	info, err := servertls.Inspect(certPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := time.Duration(info.DaysUntilExpiry) * 24 * time.Hour; got > servertls.ExpiryWarningWindow {
		t.Fatalf("fixture does not reproduce the disagreement: truncated to %v, window %v",
			got, servertls.ExpiryWarningWindow)
	}

	if c := checkTLSCert(t.Context(), d); c.Status != OK {
		t.Errorf("status = %q, want ok — %v remains, past the %v window (%q)",
			c.Status, time.Until(notAfter).Round(time.Hour), servertls.ExpiryWarningWindow, c.Summary)
	}
}

// writeCertWithNotAfter replaces the cert at path — AND the key beside
// it — with a self-signed pair expiring at `notAfter`.
//
// Minted here rather than through servertls.GenerateWithOptions because
// that path hard-codes 397 days (Apple ATS's ceiling), which is exactly
// the constant that makes the near-expiry and expired bands
// unreachable through the production minter.
//
// It writes the KEY too, and that is not incidental: an earlier version
// left the fixture's original key in place, which made every expiry
// fixture a MISMATCHED pair. The pair check caught it the moment it
// landed — which is the check working, but it also means a helper that
// only rewrites the cert silently tests a different state than the one
// its caller named.
//
// NotBefore is pinned to the PAST for the same reason, and that one was
// not caught by anything. It used to be `notAfter.Add(-24h)`, so the
// "expiring soon" fixture started 27 days from now and the boundary
// fixture 30 — both NOT YET VALID, and the boundary test asserted `ok`
// about one of them. A fixture has to be broken in exactly the way its
// caller names and no other, or a green test is about a state nobody
// chose. Callers that want a future NotBefore say so.
func writeCertWithNotAfter(t *testing.T, path string, notAfter time.Time) {
	t.Helper()
	writeCertWithWindow(t, path, time.Now().Add(-time.Hour), notAfter)
}

// writeCertWithWindow is the same, with both ends of the validity
// window given — for the not-yet-valid band, which no NotAfter can
// express.
func writeCertWithWindow(t *testing.T, path string, notBefore, notAfter time.Time) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "1-bit-bridge doctor fixture"},
		NotBefore:    notBefore,
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
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyPath := strings.TrimSuffix(path, filepath.Ext(path)) + ".key"
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(
		&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
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

// TestCheckTLSCert_NotYetValidWarns — the validity window has a near end
// too, and `LoadX509KeyPair` does not look at dates, so the pair check
// passes and the expiry arm reads a comfortable year of life left while
// no client will accept the certificate for another month.
//
// Reachable on this product's hardware rather than theoretical: the
// mint allows one hour of clock skew (`NotBefore: now-1h`), so a host
// whose clock was further ahead than that when the cert was minted — a
// NUC or Pi with no RTC, before NTP lands — leaves exactly this behind
// once the clock is corrected. Moving the data directory off such a
// host is this check's own subject.
func TestCheckTLSCert_NotYetValidWarns(t *testing.T) {
	d := certFixture(t, newHostEndpoints, newHostEndpoints)
	certPath, _ := servertls.DefaultPaths(d.DataDir)
	starts := time.Now().Add(30 * 24 * time.Hour)
	writeCertWithWindow(t, certPath, starts, starts.Add(397*24*time.Hour))

	c := checkTLSCert(t.Context(), d)
	if c.Status != Warn {
		t.Fatalf("status = %q, want warn (%q)", c.Status, c.Summary)
	}
	if !strings.Contains(c.Summary, "NOT YET VALID") {
		t.Errorf("summary = %q, want it to say the cert has not started", c.Summary)
	}
	// The remedy differs from every other band here: rotating against a
	// wrong clock mints another bad cert, so the clock comes first.
	if !strings.Contains(c.Hint, "CHECK THE CLOCK FIRST") {
		t.Errorf("hint sends the operator to rotate without checking the clock: %q", c.Hint)
	}
	// It must not be mistaken for the expiry bands — those grade a cert
	// that IS in its window.
	if strings.Contains(c.Summary, "EXPIRED") || strings.Contains(c.Summary, "expires in") {
		t.Errorf("summary = %q, want the not-yet-valid band, not an expiry one", c.Summary)
	}

	// NEGATIVE CONTROL: the same long-lived cert with a past NotBefore
	// is plainly ok, so the warn above is about the window and not about
	// the fixture.
	writeCertWithWindow(t, certPath, time.Now().Add(-time.Hour), starts.Add(397*24*time.Hour))
	if c := checkTLSCert(t.Context(), d); c.Status != OK {
		t.Errorf("a started cert = %q, want ok (%q)", c.Status, c.Summary)
	}
}

// TestExpiryFixturesAreInsideTheirValidityWindow — the fixtures this
// file hands the expiry bands must be broken in exactly the way their
// caller names. `writeCertWithNotAfter` used to derive NotBefore from
// NotAfter, so "expiring in 28 days" also meant "starts in 27", and the
// check now warns for the wrong reason. This pins the helper rather
// than each caller.
func TestExpiryFixturesAreInsideTheirValidityWindow(t *testing.T) {
	d := certFixture(t, newHostEndpoints, newHostEndpoints)
	certPath, _ := servertls.DefaultPaths(d.DataDir)
	for _, notAfter := range []time.Time{
		time.Now().Add(servertls.ExpiryWarningWindow - 48*time.Hour),
		time.Now().Add(servertls.ExpiryWarningWindow + 23*time.Hour),
		time.Now().Add(-48 * time.Hour),
	} {
		writeCertWithNotAfter(t, certPath, notAfter)
		info, err := servertls.Inspect(certPath)
		if err != nil {
			t.Fatal(err)
		}
		if info.NotBefore.After(time.Now()) {
			t.Errorf("fixture expiring %v has NotBefore %v — not yet valid, so the band under test is not the one being exercised",
				notAfter.UTC(), info.NotBefore.UTC())
		}
	}
}
