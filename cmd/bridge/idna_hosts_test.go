package main

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/config"
	servertls "github.com/acoseac/1-bit-bridge/internal/tls"
)

// noIDNADomains are hosts with no IDNA ASCII form: an underscore beside a
// character that is not ASCII, a label that begins with a hyphen, and a
// label whose IDNA form is 64 bytes, past a DNS label's 63.
var noIDNADomains = []string{"a_ü.example", "-ü.example", strings.Repeat("ж", 58) + ".example"}

// TestInitConvertsADomainThatIsNotASCII is B744 at `bridge init --public
// --domain`: a domain that is not ASCII and has an IDNA form is written in it,
// the form the certificate init mints carries, as autocert.domain and as the
// endpoint init writes from it, whatever its case or padding, and init says
// so; one with no form is refused before anything is written, exit 2, naming
// the flag, as any other domain init cannot write is.
func TestInitConvertsADomainThatIsNotASCII(t *testing.T) {
	for _, domain := range []string{"münchen.de", "MÜNCHEN.DE", " münchen.de "} {
		t.Run(domain, func(t *testing.T) {
			cfgDir := filepath.Join(t.TempDir(), "cfg")
			var out, errOut bytes.Buffer
			code := initCmd([]string{
				"--yes", "--no-service", "--skip-doctor",
				"--dir", cfgDir, "--library", testLibrary(t),
				"--public", "--domain", domain, "--admin-tls-proxy",
			}, strings.NewReader(""), &out, &errOut)
			all := stripANSI(out.String() + errOut.String())
			defer logRunOnFailure(t, all)
			if code != 0 {
				t.Fatalf("init exited %d for --domain %q, want 0", code, domain)
			}
			cfgPath := filepath.Join(cfgDir, "bridge.yaml")
			cfg, err := config.Load(cfgPath)
			if err != nil {
				t.Fatalf("init wrote no config that loads: %v", err)
			}
			if cfg.Autocert.Domain != "xn--mnchen-3ya.de" || !slices.Equal(cfg.CustomEndpoints, []string{"https://xn--mnchen-3ya.de"}) {
				t.Errorf("--domain %q saved autocert.domain %q and customEndpoints %q, want xn--mnchen-3ya.de and [https://xn--mnchen-3ya.de]",
					domain, cfg.Autocert.Domain, cfg.CustomEndpoints)
			}
			raw, err := os.ReadFile(cfgPath)
			if err != nil {
				t.Fatal(err)
			}
			// The file's own lines, not only what Load serves (the test's
			// directory names carry the domain as typed, so only the lines
			// that write it are asked).
			for _, line := range strings.Split(string(raw), "\n") {
				if (strings.Contains(line, "domain:") || strings.Contains(line, "https://")) && !isASCIIText(line) {
					t.Errorf("the file writes the domain as typed, not in its IDNA form: %q", line)
				}
			}
			if !strings.Contains(all, "xn--mnchen-3ya.de") {
				t.Errorf("init did not say the domain is written in its IDNA form")
			}
			leaf := readCertificate(t, filepath.Join(cfgDir, "data", servertls.CertFileName))
			if err := leaf.VerifyHostname("xn--mnchen-3ya.de"); err != nil {
				t.Errorf("the certificate init minted does not cover the domain's IDNA form: %v", err)
			}
		})
	}
	for _, domain := range noIDNADomains {
		t.Run("refuses "+domain, func(t *testing.T) {
			cfgDir := filepath.Join(t.TempDir(), "cfg")
			var out, errOut bytes.Buffer
			code := initCmd([]string{
				"--yes", "--no-service", "--skip-doctor",
				"--dir", cfgDir, "--library", testLibrary(t),
				"--public", "--domain=" + domain, "--admin-tls-proxy",
			}, strings.NewReader(""), &out, &errOut)
			all := stripANSI(out.String() + errOut.String())
			defer logRunOnFailure(t, all)
			if code != 2 {
				t.Errorf("init exited %d for --domain %q, which has no IDNA form, want 2", code, domain)
			}
			if !strings.Contains(errOut.String(), "--domain") {
				t.Errorf("the refusal does not name --domain")
			}
			if _, err := os.Stat(cfgDir); !os.IsNotExist(err) {
				t.Errorf("the refused init made its config dir (stat: %v)", err)
			}
		})
	}
}

// isASCIIText reports whether s holds no byte outside ASCII.
func isASCIIText(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// TestAConfiguredNameThatIsNotASCIIIsCertifiedInItsIDNAForm is B744's other
// end: a customEndpoints entry whose host is not ASCII (café.example) loads in
// its IDNA ASCII form, the certificate's want-set takes that form, and `bridge
// cert rotate` mints a certificate that covers it, where every mint refused
// the config (B739's finding). An entry with no IDNA form is dropped at the
// load, said, and refuses nothing. On a host whose own name is converted too.
func TestAConfiguredNameThatIsNotASCIIIsCertifiedInItsIDNAForm(t *testing.T) {
	for _, host := range []string{"musicbox", hostCyrillic} {
		t.Run(host, func(t *testing.T) {
			reportHostName(t, host)
			dir := t.TempDir()
			cfgPath := writeInstallAt(t, dir, "Artist/Album/01.flac")
			appendYAML(t, cfgPath, "customEndpoints:\n  - https://café.example:7788\n  - https://a_ü.example:7788\n")
			cfg, err := config.Load(cfgPath)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(cfg.CustomEndpoints, []string{"https://xn--caf-dma.example:7788"}) {
				t.Fatalf("the configured endpoints load as %q, want [https://xn--caf-dma.example:7788]", cfg.CustomEndpoints)
			}
			if !slices.Contains(certSANOptions(cfg).ExtraDNSNames, "xn--caf-dma.example") {
				t.Fatalf("the certificate's want-set does not take the IDNA form: %q", certSANOptions(cfg).ExtraDNSNames)
			}

			var stdout, stderr strings.Builder
			if code := certRotateCmd([]string{"--config", cfgPath, "--yes"}, nil, &stdout, &stderr); code != 0 {
				t.Fatalf("bridge cert rotate exited %d, want 0; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
			}
			leaf := readCertificate(t, filepath.Join(dir, "data", servertls.CertFileName))
			if err := leaf.VerifyHostname("xn--caf-dma.example"); err != nil {
				t.Errorf("the minted certificate does not cover the configured endpoint: %v", err)
			}
		})
	}
}
