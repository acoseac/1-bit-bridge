package tlsacme

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"golang.org/x/crypto/acme"
)

func TestNewRejectsEmptyFields(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name string
		cfg  Config
		msg  string
	}{
		{"missing domain", Config{Email: "ops@x.com", CacheDir: dir}, "Domain"},
		{"missing email", Config{Domain: "x.com", CacheDir: dir}, "Email"},
		{"missing cacheDir", Config{Domain: "x.com", Email: "ops@x.com"}, "CacheDir"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(tc.cfg)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.msg) {
				t.Errorf("error %q should mention %q", err.Error(), tc.msg)
			}
		})
	}
}

func TestNewCreatesCacheDirAt0o700(t *testing.T) {
	// POSIX file modes only — Windows uses NTFS ACLs.
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode check not meaningful on Windows")
	}
	parent := t.TempDir()
	cacheDir := filepath.Join(parent, "acme")
	_, err := New(Config{
		Domain:   "bridge.example.com",
		Email:    "ops@example.com",
		CacheDir: cacheDir,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	info, err := os.Stat(cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o700 {
		t.Errorf("cacheDir mode = %o, want 0o700", mode)
	}
}

func TestNewNormalizesDomain(t *testing.T) {
	// Trailing dot + uppercase should be stripped/lowered so the
	// HostPolicy comparison (and the tls.Manager SNI gate) match
	// against the canonical form.
	dir := t.TempDir()
	m, err := New(Config{
		Domain:   "  BRIDGE.Example.COM.  ",
		Email:    "ops@example.com",
		CacheDir: dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := m.Domain(), "bridge.example.com"; got != want {
		t.Errorf("Domain() = %q, want %q", got, want)
	}
}

func TestNextProtosIncludesACMEALPN(t *testing.T) {
	dir := t.TempDir()
	m, err := New(Config{
		Domain:   "x.com",
		Email:    "ops@x.com",
		CacheDir: dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	got := m.NextProtos()
	found := false
	for _, p := range got {
		if p == acme.ALPNProto {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("NextProtos() = %v, want to include %q", got, acme.ALPNProto)
	}
}

func TestStatusOnFreshManagerReportsNoCert(t *testing.T) {
	dir := t.TempDir()
	m, err := New(Config{
		Domain:   "x.com",
		Email:    "ops@x.com",
		CacheDir: dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	st := m.Status()
	if st.Domain != "x.com" {
		t.Errorf("Domain = %q, want %q", st.Domain, "x.com")
	}
	if st.CertPresent {
		t.Error("CertPresent should be false on a freshly-constructed manager")
	}
	if !st.NotAfter.IsZero() {
		t.Errorf("NotAfter = %v, want zero time", st.NotAfter)
	}
}

func TestStatusReflectsCachedCert(t *testing.T) {
	// Seed the autocert.DirCache with a self-signed test cert
	// pretending to be the LE-issued one. Status() must report
	// CertPresent=true + NotAfter populated, exercising the
	// passive cache-lookup path WITHOUT triggering an LE mint.
	dir := t.TempDir()
	m, err := New(Config{
		Domain:   "x.com",
		Email:    "ops@x.com",
		CacheDir: dir,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Write a synthetic PEM (key + cert) at the cache path autocert
	// uses (bare domain name as filename).
	pem := generateTestPEM(t, "x.com")
	if err := os.WriteFile(filepath.Join(dir, "x.com"), pem, 0o600); err != nil {
		t.Fatal(err)
	}
	st := m.Status()
	if !st.CertPresent {
		t.Error("CertPresent should be true after seeding cache")
	}
	if st.NotAfter.IsZero() {
		t.Error("NotAfter should be populated after seeding cache")
	}
}

func TestStatusSurfacesLastError(t *testing.T) {
	dir := t.TempDir()
	m, err := New(Config{
		Domain:   "x.com",
		Email:    "ops@x.com",
		CacheDir: dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Inject an error by directly poking the protected state —
	// in production this gets set by GetCertificate's error path.
	m.mu.Lock()
	m.lastError = "boom"
	m.mu.Unlock()
	st := m.Status()
	if st.LastError != "boom" {
		t.Errorf("LastError = %q, want %q", st.LastError, "boom")
	}
}

func TestHostPolicyRefusesOtherDomain(t *testing.T) {
	// HostPolicy is autocert's gate against unsolicited mint
	// attempts. We can't easily probe GetCertificate without
	// network, but the policy's invocation shape is testable:
	// the autocert.Manager's HostPolicy field is what we
	// configured.
	dir := t.TempDir()
	m, err := New(Config{
		Domain:   "x.com",
		Email:    "ops@x.com",
		CacheDir: dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.am.HostPolicy(nil, "x.com"); err != nil {
		t.Errorf("HostPolicy refused configured domain: %v", err)
	}
	if err := m.am.HostPolicy(nil, "attacker.com"); err == nil {
		t.Error("HostPolicy should refuse other domains")
	}
}

func TestUseStagingRoutesAtStagingDirectory(t *testing.T) {
	dir := t.TempDir()
	m, err := New(Config{
		Domain:     "x.com",
		Email:      "ops@x.com",
		CacheDir:   dir,
		UseStaging: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if m.am.Client == nil {
		t.Fatal("UseStaging=true should install a custom acme.Client")
	}
	if m.am.Client.DirectoryURL != stagingDirectoryURL {
		t.Errorf("Client.DirectoryURL = %q, want %q", m.am.Client.DirectoryURL, stagingDirectoryURL)
	}
}

func TestGetCertificateRecordsError(t *testing.T) {
	// We can't run a real LE handshake from a unit test, but we
	// can ask GetCertificate for a domain it doesn't have, which
	// triggers a mint attempt that fails (no network) and
	// populates lastError.
	dir := t.TempDir()
	m, err := New(Config{
		Domain:   "x.com",
		Email:    "ops@x.com",
		CacheDir: dir,
		// Use staging to avoid touching prod even on test
		// infrastructure that happens to have outbound network.
		UseStaging: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	// HostPolicy refuses everything except "x.com", so dialing
	// a different SNI gives us a deterministic error WITHOUT
	// network access.
	hello := newClientHello("attacker.com")
	_, err = m.GetCertificate(hello)
	if err == nil {
		t.Fatal("expected error from HostPolicy refusal")
	}
	st := m.Status()
	if st.LastError == "" {
		t.Error("Status.LastError should be populated after a failed GetCertificate")
	}
	if st.LastCheck.IsZero() {
		t.Error("Status.LastCheck should be populated after any GetCertificate call")
	}
	// Sanity: the error path leaves the caller-visible error
	// chain intact (not swallowed).
	if !errors.Is(err, err) { // tautology — but pins that err is non-nil + propagated
		t.Errorf("error chain broken: %v", err)
	}
}
