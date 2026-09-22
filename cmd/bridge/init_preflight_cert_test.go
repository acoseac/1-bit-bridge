package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/doctor"
	servertls "github.com/acoseac/1-bit-bridge/internal/tls"
)

// `bridge init`'s preflight, on the run that is not the first one.
//
// init is re-run far more often than it is run — reinstalling the
// service, rewriting a hand-edited config, moving a data directory to a
// new host — and it built doctor.Deps from its prompts alone. So the
// two cert checks graded `<cfgDir>/data/server.{crt,key}`, which is not
// where an install with an explicit `tlsCertPath` keeps its pair, and
// tls-cert-sans skipped itself entirely, a nil CertSANs being a silent
// ok.
//
// Driven through initCmd rather than through the Deps builder on
// purpose: only the real entry point can show that the wiring is
// reached, and a probe nothing wires is one of this repo's three
// recorded shapes for shipping a dead feature with a green suite.

// TestInitPreflightGradesTheInstalledCertificate puts BOTH cert
// findings in front of the operator in one run: a certificate at a
// configured path whose window has not opened, and whose SANs do not
// cover an endpoint the config advertises.
func TestInitPreflightGradesTheInstalledCertificate(t *testing.T) {
	tmp := t.TempDir()
	cfgDir := filepath.Join(tmp, "cfg")
	lib := filepath.Join(tmp, "Music")
	for _, d := range []string{cfgDir, lib} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Deliberately NOT under <cfgDir>/data: the DataDir default is what
	// the preflight used to grade, so a pair that lived there would
	// pass against the pre-fix resolution too.
	certPath := filepath.Join(tmp, "pki", "custom.crt")
	keyPath := filepath.Join(tmp, "pki", "custom.key")
	starts := time.Now().Add(30 * 24 * time.Hour)
	certFixtureWindow(t, certPath, keyPath, starts, starts.Add(397*24*time.Hour))

	// customEndpoints is the one SAN input an operator sets by hand, so
	// it is the one a test can pin without depending on what interfaces
	// the host has — and it is the field that makes grading the
	// PRE-init config the right call: init never prompts for it, so the
	// old value survives the rewrite verbatim.
	body := "libraryRoots:\n  - " + lib + "\n" +
		"dataDir: " + filepath.Join(cfgDir, "data") + "\n" +
		"tlsCertPath: " + certPath + "\ntlsKeyPath: " + keyPath + "\n" +
		"customEndpoints:\n  - https://bridge.example.test:7788\n"
	if err := os.WriteFile(filepath.Join(cfgDir, "bridge.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	// No --skip-doctor: the preflight is the subject. --yes without
	// --force leaves the config alone, so the run stops at "config file
	// already exists" once the preflight is past.
	initCmd([]string{
		"--yes", "--no-service",
		"--dir", cfgDir,
		"--library", lib,
		"--name", "Preflight Fixture",
	}, strings.NewReader(""), &stdout, &stderr)
	out := stdout.String()

	// The exit code is the host's to decide, not this test's: init's
	// preflight probes the default 7788 / 7789, so a dev box already
	// running a bridge fails the port checks. Both outcomes print the
	// cert lines — a fail prints the whole report, a clean run prints
	// the warnings — so assert on WHICH path ran and then on the
	// content, rather than tolerating either silently.
	failed := strings.Contains(out, "1-bit-bridge preflight:")
	if !failed && !strings.Contains(out, "preflight warnings") {
		t.Fatalf("preflight produced neither a report nor a warning block:\n%s", out)
	}

	if !strings.Contains(out, "NOT YET VALID") {
		t.Errorf("preflight says nothing about the configured cert's window:\n%s", out)
	}
	if !strings.Contains(out, servertls.NotYetValidRemediation) {
		t.Errorf("preflight drops the not-yet-valid remediation:\n%s", out)
	}
	// The SAN half. Pre-fix this check answered ok without comparing
	// anything, so the endpoint never appeared.
	if !strings.Contains(out, "bridge.example.test") {
		t.Errorf("preflight does not name the endpoint the cert fails to cover:\n%s", out)
	}
	// And the DataDir default — which holds nothing — must not be what
	// got graded. "absent (init will mint)" was the pre-fix answer.
	if strings.Contains(out, "absent (init will mint)") {
		t.Errorf("preflight graded the DataDir default rather than the configured pair:\n%s", out)
	}
}

// TestInitPreflightIsQuietAboutACertMintedForThisHost is the negative
// control. A preflight line that is permanently yellow is one an
// operator learns to skip, so the warnings have to be clearable — and
// clearable by the command this repo tells them to run, which is why
// the fixture is minted from certSANOptions rather than from a
// hand-written option set.
func TestInitPreflightIsQuietAboutACertMintedForThisHost(t *testing.T) {
	tmp := t.TempDir()
	cfgDir := filepath.Join(tmp, "cfg")
	lib := filepath.Join(tmp, "Music")
	for _, d := range []string{cfgDir, lib} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	certPath := filepath.Join(tmp, "pki", "custom.crt")
	keyPath := filepath.Join(tmp, "pki", "custom.key")
	cfgPath := filepath.Join(cfgDir, "bridge.yaml")
	body := "libraryRoots:\n  - " + lib + "\n" +
		"dataDir: " + filepath.Join(cfgDir, "data") + "\n" +
		"tlsCertPath: " + certPath + "\ntlsKeyPath: " + keyPath + "\n" +
		"customEndpoints:\n  - https://bridge.example.test:7788\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(certPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := servertls.GenerateWithOptions(certPath, keyPath, certSANOptions(cfg)); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	initCmd([]string{
		"--yes", "--no-service",
		"--dir", cfgDir,
		"--library", lib,
		"--name", "Preflight Fixture",
	}, strings.NewReader(""), &stdout, &stderr)
	out := stdout.String()

	for _, unwanted := range []string{"NOT YET VALID", "bridge.example.test"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("a cert `bridge cert rotate` would mint today still warns about %q:\n%s", unwanted, out)
		}
	}
}

// TestInitPreflightLeavesAFirstInstallUnwired — the other side of the
// judgement call. With no config at the target path there are no
// customEndpoints to gather, so the want-set would be narrower than the
// one `bridge serve` builds and "covered" would be a claim about a
// comparison nobody made. Nothing is stale on a host whose first mint
// has not happened yet.
func TestInitPreflightLeavesAFirstInstallUnwired(t *testing.T) {
	var d doctor.Deps
	withExistingInstallCertDeps(&d, filepath.Join(t.TempDir(), "bridge.yaml"))
	if d.CertSANs != nil {
		t.Error("cert-SAN gather wired from a config that does not exist")
	}
	if d.TLSCertPath != "" || d.TLSKeyPath != "" {
		t.Errorf("cert paths resolved from a config that does not exist: %q / %q", d.TLSCertPath, d.TLSKeyPath)
	}
}

// TestInitPreflightCarriesTheManagedPosture — the third field on the
// same install, and the one the helper copied its neighbours without.
//
// `bridge doctor` sets Deps.Managed from the loaded config, and
// checkTLSCertSANs skips on it: a hosted tenant reaches its bridge over
// the autocert domain, whose Let's Encrypt certificate the SNI switcher
// serves instead of the self-signed pair, and the only remedy that
// check names is a shell command the tenant cannot run. The preflight
// read the cert PATHS and the SAN want-set from the config on disk and
// then graded them as if the bridge were self-hosted, so a re-init of a
// managed install printed advice for somebody else's console.
//
// Asserted on the Deps rather than through initCmd because Managed is
// only observable through a check's verdict, and the verdict that
// changes is the one the test above already drives from the unmanaged
// side.
func TestInitPreflightCarriesTheManagedPosture(t *testing.T) {
	tmp := t.TempDir()
	lib := filepath.Join(tmp, "Music")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(tmp, "bridge.yaml")
	body := "libraryRoots:\n  - " + lib + "\n" +
		"dataDir: " + filepath.Join(tmp, "data") + "\n" +
		"deployment:\n  managedControls:\n    - restart\n    - updates\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	// Control first: the same helper against a config with no
	// deployment block must NOT claim the posture, or an unconditional
	// `true` would pass the assertion below.
	plain := filepath.Join(tmp, "plain.yaml")
	if err := os.WriteFile(plain, []byte("libraryRoots:\n  - "+lib+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var unmanaged doctor.Deps
	withExistingInstallCertDeps(&unmanaged, plain)
	if unmanaged.Managed {
		t.Error("a config with no deployment block graded as managed")
	}

	var d doctor.Deps
	withExistingInstallCertDeps(&d, cfgPath)
	if !d.Managed {
		t.Error("the preflight does not carry the managed posture, so a hosted tenant " +
			"re-running init is told to run `bridge cert rotate` on a host it does not own")
	}
}
