package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	servertls "github.com/acoseac/1-bit-bridge/internal/tls"
)

// The near end of the certificate validity window, on the CLI.
//
// `Inspect` reports DaysUntilExpiry and nothing else about the window's
// start, so every surface that grades that number alone answers "396
// days" about a certificate no client will accept. PR #950 closed that
// on `bridge doctor` and the serve-time warning; these drive the two
// `bridge cert` subcommands, which were left grading NotAfter only.
//
// Reachable on this product's hardware because the mint allows one hour
// of clock skew (`NotBefore: now-1h`): a NUC or Pi that minted before
// NTP landed leaves a future NotBefore behind once the clock is
// corrected.

// certFixtureWindow mints a self-signed pair at certPath with both ends
// of the validity window given — the state no NotAfter can express and
// servertls.GenerateWithOptions cannot produce, since it always anchors
// NotBefore to now-1h.
func certFixtureWindow(t *testing.T, certPath, keyPath string, notBefore, notAfter time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(certPath), 0o755); err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "1-bit-bridge cert-window fixture"},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(certPath, pem.EncodeToMemory(
		&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(
		&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
}

// certFixtureConfig writes the smallest config `bridge cert` needs — a
// dataDir naming where the pair lives — and returns its path.
func certFixtureConfig(t *testing.T, dir string) string {
	t.Helper()
	lib := filepath.Join(dir, "Music")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "bridge.yaml")
	body := "libraryRoots:\n  - " + lib + "\ndataDir: " + filepath.Join(dir, "data") + "\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgPath
}

// TestCertInfoReportsACertThatHasNotStarted drives `bridge cert info`
// over a certificate whose window opens in 30 days. It has ~396 days of
// NotAfter ahead of it, which is exactly why the pre-fix output was a
// clean "Days until expiry: 396" with no warning at all.
func TestCertInfoReportsACertThatHasNotStarted(t *testing.T) {
	dir := t.TempDir()
	cfgPath := certFixtureConfig(t, dir)
	certPath, keyPath := servertls.DefaultPaths(filepath.Join(dir, "data"))
	starts := time.Now().Add(30 * 24 * time.Hour)
	certFixtureWindow(t, certPath, keyPath, starts, starts.Add(397*24*time.Hour))

	var stdout, stderr bytes.Buffer
	if code := certInfoCmd([]string{"--config", cfgPath}, &stdout, &stderr); code != 0 {
		t.Fatalf("cert info: code=%d stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "NOT YET VALID") {
		t.Errorf("output does not say the cert has not started:\n%s", out)
	}
	// The remedy inverts here and the clock clause is what makes it
	// work, so assert on the shared const rather than on a paraphrase:
	// a surface that grew its own wording is the defect this closes.
	if !strings.Contains(out, servertls.NotYetValidRemediation) {
		t.Errorf("output does not carry servertls.NotYetValidRemediation:\n%s", out)
	}
	// And it must not ALSO claim the cert is fine for another year in
	// the same breath — the expiry bands are mutually exclusive with
	// this one.
	if strings.Contains(out, "expiring soon") {
		t.Errorf("output mixes the expiry warning into the not-yet-valid band:\n%s", out)
	}
}

// TestCertInfoJSONCarriesTheNotYetValidVerdict — the `--json` envelope
// is what automation grades on, and it carried `expired` and
// `expiringSoon` with no third answer, so a not-yet-valid certificate
// serialised as all-false: indistinguishable from a healthy one.
func TestCertInfoJSONCarriesTheNotYetValidVerdict(t *testing.T) {
	dir := t.TempDir()
	cfgPath := certFixtureConfig(t, dir)
	certPath, keyPath := servertls.DefaultPaths(filepath.Join(dir, "data"))
	starts := time.Now().Add(30 * 24 * time.Hour)
	certFixtureWindow(t, certPath, keyPath, starts, starts.Add(397*24*time.Hour))

	var stdout, stderr bytes.Buffer
	if code := certInfoCmd([]string{"--config", cfgPath, "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("cert info --json: code=%d stderr=%s", code, stderr.String())
	}
	var env struct {
		NotYetValid     bool `json:"notYetValid"`
		Expired         bool `json:"expired"`
		ExpiringSoon    bool `json:"expiringSoon"`
		DaysUntilExpiry int  `json:"daysUntilExpiry"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v\n%s", err, stdout.String())
	}
	if !env.NotYetValid {
		t.Error("notYetValid is false for a cert whose window opens in 30 days")
	}
	if env.Expired || env.ExpiringSoon {
		t.Errorf("the three verdicts overlap: %+v", env)
	}
	// The field that made this invisible, pinned so the reason stays
	// legible: the day count is comfortably positive throughout.
	if env.DaysUntilExpiry < 300 {
		t.Errorf("daysUntilExpiry = %d, want the ~396 that hid this state", env.DaysUntilExpiry)
	}
}

// TestCertInfoIsQuietAboutAHealthyCert is the negative control for both
// of the above: the same command over a pair minted by the real mint.
// Without it they prove only that the branch can fire.
func TestCertInfoIsQuietAboutAHealthyCert(t *testing.T) {
	dir := t.TempDir()
	cfgPath := certFixtureConfig(t, dir)
	certPath, keyPath := servertls.DefaultPaths(filepath.Join(dir, "data"))
	if err := servertls.GenerateWithOptions(certPath, keyPath, servertls.GenerateOptions{
		Hostname: "fixture-host",
	}); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := certInfoCmd([]string{"--config", cfgPath}, &stdout, &stderr); code != 0 {
		t.Fatalf("cert info: code=%d stderr=%s", code, stderr.String())
	}
	if out := stdout.String(); strings.Contains(out, "WARNING") {
		t.Errorf("a freshly minted cert warns:\n%s", out)
	}
}

// TestCertInfoGradesTheThirtyDayBandOnTheDuration — the day count
// truncates toward zero, so a cert with 30 days 23 hours left reports
// 30 and the pre-fix `DaysUntilExpiry <= 30` called it expiring, while
// `bridge doctor` and the next `bridge serve` — both comparing
// time.Until(NotAfter) against servertls.ExpiryWarningWindow — stayed
// quiet about the same file. Same cert, same host, two answers.
func TestCertInfoGradesTheThirtyDayBandOnTheDuration(t *testing.T) {
	dir := t.TempDir()
	cfgPath := certFixtureConfig(t, dir)
	certPath, keyPath := servertls.DefaultPaths(filepath.Join(dir, "data"))
	// Inside the truncation gap: over the window by 23 hours, so the
	// day count reads exactly 30.
	ends := time.Now().Add(servertls.ExpiryWarningWindow + 23*time.Hour)
	certFixtureWindow(t, certPath, keyPath, time.Now().Add(-time.Hour), ends)

	var stdout, stderr bytes.Buffer
	if code := certInfoCmd([]string{"--config", cfgPath}, &stdout, &stderr); code != 0 {
		t.Fatalf("cert info: code=%d stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	// The fixture only reproduces the gap while the day count really
	// does truncate to the threshold — assert that, or this passes
	// vacuously against any implementation.
	if !strings.Contains(out, "Days until expiry: 30") {
		t.Fatalf("fixture no longer sits in the truncation gap:\n%s", out)
	}
	if strings.Contains(out, "WARNING") {
		t.Errorf("warned at 30d23h, where serve and doctor stay quiet:\n%s", out)
	}
}

// TestCertRotateWarnsAboutTheClockBeforeItAsks — the surface where the
// remedy inverts. Everywhere else the answer is "rotate"; here the mint
// about to run reads the same clock that produced the bad dates, so
// rotating now mints a second wrong certificate and burns every paired
// device's pin to do it. The warning has to land BEFORE the prompt, or
// it is a post-mortem.
func TestCertRotateWarnsAboutTheClockBeforeItAsks(t *testing.T) {
	dir := t.TempDir()
	cfgPath := certFixtureConfig(t, dir)
	certPath, keyPath := servertls.DefaultPaths(filepath.Join(dir, "data"))
	starts := time.Now().Add(30 * 24 * time.Hour)
	certFixtureWindow(t, certPath, keyPath, starts, starts.Add(397*24*time.Hour))

	// Decline at the prompt: the assertion is about what the operator
	// is told while they can still act on it, and a rotation here would
	// also replace the fixture.
	var stdout, stderr bytes.Buffer
	code := certRotateCmd([]string{"--config", cfgPath}, strings.NewReader("no\n"), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("declined rotate: code=%d, want 1\nstdout=%s", code, stdout.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "NOT YET VALID") {
		t.Fatalf("preamble does not name the state:\n%s", out)
	}
	if !strings.Contains(out, servertls.NotYetValidRemediation) {
		t.Errorf("preamble does not carry the remediation:\n%s", out)
	}
	// Ordering is the point, and it is not visible from presence alone.
	// The bullet list is what precedes the prompt on stdout.
	warnAt := strings.Index(out, "NOT YET VALID")
	bulletsAt := strings.Index(out, "Rotating the TLS cert will:")
	if bulletsAt < 0 {
		t.Fatalf("no confirmation preamble on stdout:\n%s", out)
	}
	if warnAt > bulletsAt {
		t.Errorf("the clock warning lands after the confirmation bullets — an operator reading top-down "+
			"has already decided:\n%s", out)
	}
}

// TestCertRotateSaysNothingAboutTheClockOnAHealthyCert — the negative
// control, and also the rule for what the preamble carries: only the
// band where this command is NOT the fix. An expired or expiring cert
// is why the operator is here, so naming it would be noise.
func TestCertRotateSaysNothingAboutTheClockOnAHealthyCert(t *testing.T) {
	dir := t.TempDir()
	cfgPath := certFixtureConfig(t, dir)
	certPath, keyPath := servertls.DefaultPaths(filepath.Join(dir, "data"))
	// Expiring in a week — a real reason to be running this command.
	certFixtureWindow(t, certPath, keyPath, time.Now().Add(-390*24*time.Hour), time.Now().Add(7*24*time.Hour))

	var stdout, stderr bytes.Buffer
	if code := certRotateCmd([]string{"--config", cfgPath}, strings.NewReader("no\n"), &stdout, &stderr); code != 1 {
		t.Fatalf("declined rotate: code=%d, want 1", code)
	}
	if out := stdout.String(); strings.Contains(out, "NOT YET VALID") {
		t.Errorf("an expiring cert is reported as not yet valid:\n%s", out)
	}
}

// TestCertInfoVerdictsAreMutuallyExclusiveOnAnInvertedWindow pins the
// property that makes `--json`'s three booleans readable: at most one
// is true, and it is the one the human switch would print.
//
// The case that can break it is a certificate whose NotAfter precedes
// its NotBefore. Gemini read the `!notYetValid` guard on `expired` as
// dead code on PR #951, on the premise that NotAfter is always after
// NotBefore for a valid certificate. Nothing in this path enforces
// that: `x509.CreateCertificate` and `x509.ParseCertificate` both
// accept an inverted window, `LoadX509KeyPair` ignores dates, and
// `tlsCertPath` takes any operator-supplied pair — so a hand-assembled
// or restored data directory reaches `Inspect` with whatever it holds.
//
// The fixture asserts the inversion it depends on, so it cannot quietly
// stop reproducing the case it exists for.
func TestCertInfoVerdictsAreMutuallyExclusiveOnAnInvertedWindow(t *testing.T) {
	dir := t.TempDir()
	cfgPath := certFixtureConfig(t, dir)
	certPath, keyPath := servertls.DefaultPaths(filepath.Join(dir, "data"))
	notBefore := time.Now().Add(30 * 24 * time.Hour)
	notAfter := time.Now().Add(-10 * 24 * time.Hour)
	if !notAfter.Before(notBefore) {
		t.Fatalf("fixture is not inverted: NotBefore=%s NotAfter=%s", notBefore, notAfter)
	}
	certFixtureWindow(t, certPath, keyPath, notBefore, notAfter)

	var stdout, stderr bytes.Buffer
	if code := certInfoCmd([]string{"--config", cfgPath, "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("cert info --json: code=%d stderr=%s", code, stderr.String())
	}
	var env struct {
		NotYetValid  bool `json:"notYetValid"`
		Expired      bool `json:"expired"`
		ExpiringSoon bool `json:"expiringSoon"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v\n%s", err, stdout.String())
	}
	set := 0
	for _, v := range []bool{env.NotYetValid, env.Expired, env.ExpiringSoon} {
		if v {
			set++
		}
	}
	if set != 1 {
		t.Errorf("%d verdicts set, want exactly 1: %+v", set, env)
	}
	// And it must be the one the human switch prints, or the two halves
	// of this command describe the same file differently.
	if !env.NotYetValid {
		t.Errorf("notYetValid is not the winning verdict: %+v", env)
	}
	var human, humanErr bytes.Buffer
	if code := certInfoCmd([]string{"--config", cfgPath}, &human, &humanErr); code != 0 {
		t.Fatalf("cert info: code=%d stderr=%s", code, humanErr.String())
	}
	if !strings.Contains(human.String(), "NOT YET VALID") {
		t.Errorf("the human output takes a different band than the envelope:\n%s", human.String())
	}
}
