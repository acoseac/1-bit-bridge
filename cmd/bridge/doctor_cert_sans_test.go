package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/doctor"
	servertls "github.com/acoseac/1-bit-bridge/internal/tls"
)

// The wiring half of the cert-SAN check. internal/doctor's own tests
// hand checkTLSCertSANs a Deps they built; only a test here can see
// whether buildDoctorDeps actually wires one — a probe nothing wires is
// one of the three shapes this repo records for "shipped a dead feature
// with a green suite", and the check's nil branch is a silent ok.

// TestDoctorReportsACertThatDoesNotCoverTheAdvertisedEndpoints drives
// the whole pipeline — buildDoctorDeps, the wired gather, doctor.Run —
// over the 2026-09-20 field shape: a data directory carried to a new
// host, still holding the old host's cert.
func TestDoctorReportsACertThatDoesNotCoverTheAdvertisedEndpoints(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeInstallAt(t, dir, "Artist/Album/01.flac")
	// A custom endpoint the on-disk cert cannot know about. It is the
	// one SAN input an operator sets by hand, so it is also the one a
	// test can pin without depending on what interfaces the host has.
	appendYAML(t, cfgPath, "customEndpoints:\n  - https://bridge.example.test:7788\n")

	certPath, keyPath := servertls.DefaultPaths(filepath.Join(dir, "data"))
	if err := servertls.GenerateWithOptions(certPath, keyPath, servertls.GenerateOptions{
		Hostname: "some-other-host",
	}); err != nil {
		t.Fatal(err)
	}

	d := buildDoctorDeps(cfgPath)
	if d.CertSANs == nil {
		t.Fatal("the cert-SAN gather is not wired — the check skips with an ok forever")
	}
	c := findCheck(t, doctor.Run(context.Background(), d), "tls-cert-sans")
	if c.Status != doctor.Warn {
		t.Fatalf("status = %v, want warn\nsummary: %s", c.Status, c.Summary)
	}
	if !strings.Contains(c.Hint, "bridge.example.test") {
		t.Errorf("hint does not name the uncovered custom endpoint: %q", c.Hint)
	}
	if !strings.Contains(c.Hint, servertls.RotationRemediation) {
		t.Errorf("hint does not carry the remediation: %q", c.Hint)
	}
}

// TestDoctorIsQuietAboutACertMintedForThisHost is the negative control:
// the same pipeline over a cert minted from the SAME gather the doctor
// grades against. Without it the warning above proves only that the
// check can fire, not that it can be satisfied — and a preflight line
// that is permanently yellow is one an operator learns to skip.
//
// Minting from certSANOptions rather than from a hand-written option
// set is the point: it is the call `bridge cert rotate` makes, so this
// asserts that rotating really does clear the warning.
func TestDoctorIsQuietAboutACertMintedForThisHost(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeInstallAt(t, dir, "Artist/Album/01.flac")
	appendYAML(t, cfgPath, "customEndpoints:\n  - https://bridge.example.test:7788\n")

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	certPath, keyPath := servertls.DefaultPaths(filepath.Join(dir, "data"))
	if err := servertls.GenerateWithOptions(certPath, keyPath, certSANOptions(cfg)); err != nil {
		t.Fatal(err)
	}

	c := findCheck(t, doctor.Run(context.Background(), buildDoctorDeps(cfgPath)), "tls-cert-sans")
	if c.Status != doctor.OK {
		t.Fatalf("a cert minted from certSANOptions still reads stale: %v %q\n%s", c.Status, c.Summary, c.Hint)
	}
	if !strings.Contains(c.Summary, "covers all") {
		t.Errorf("summary = %q, want it to report what it covered", c.Summary)
	}
}

// TestDoctorGradesTheConfiguredCertPair — an install with an explicit
// `tlsCertPath` serves that pair, and both cert checks have to read it.
// Resolved from DataDir alone they would find nothing there and report
// an all-clear about a bridge whose real certificate is stale.
func TestDoctorGradesTheConfiguredCertPair(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeInstallAt(t, dir, "Artist/Album/01.flac")
	certPath := filepath.Join(dir, "pki", "custom.crt")
	keyPath := filepath.Join(dir, "pki", "custom.key")
	if err := os.MkdirAll(filepath.Dir(certPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := servertls.GenerateWithOptions(certPath, keyPath, servertls.GenerateOptions{
		Hostname: "some-other-host",
	}); err != nil {
		t.Fatal(err)
	}
	appendYAML(t, cfgPath, "tlsCertPath: "+certPath+"\ntlsKeyPath: "+keyPath+
		"\ncustomEndpoints:\n  - https://bridge.example.test:7788\n")

	d := buildDoctorDeps(cfgPath)
	if d.TLSCertPath != certPath || d.TLSKeyPath != keyPath {
		t.Fatalf("doctor resolved %q/%q, want the configured pair %q/%q",
			d.TLSCertPath, d.TLSKeyPath, certPath, keyPath)
	}
	rep := doctor.Run(context.Background(), d)
	// The DataDir holds no cert at all, so the pre-fix resolution
	// answered "absent (init will mint)" here.
	if c := findCheck(t, rep, "tls-cert"); !strings.HasPrefix(c.Summary, "present") {
		t.Errorf("tls-cert summary = %q, want it to describe the configured cert", c.Summary)
	}
	if c := findCheck(t, rep, "tls-cert-sans"); c.Status != doctor.Warn {
		t.Errorf("tls-cert-sans = %v %q, want warn about the configured cert", c.Status, c.Summary)
	}
}

// TestDoctorCertSANsAreUnwiredWithoutAConfig — with no readable config
// there are no customEndpoints to gather, so grading against a narrower
// want-set than `bridge serve` builds would be a comparison the report
// presents as authoritative and is not. The probe is left nil and the
// check says so.
func TestDoctorCertSANsAreUnwiredWithoutAConfig(t *testing.T) {
	if d := buildDoctorDeps(filepath.Join(t.TempDir(), "absent.yaml")); d.CertSANs != nil {
		t.Error("cert-SAN gather wired from a config that does not exist")
	}
}

// TestCertSANOptionsIsWhatEveryCertPathMints — the doctor's verdict is
// a claim about what a rotation WOULD produce, which holds only while
// the four cert paths in this binary gather the same set. They had the
// same three lines copied out four times before this helper existed;
// the copy that drifts is the one that tells an operator their cert is
// fine when `bridge cert rotate` would mint something different.
//
// Asserted structurally: every call site names certSANOptions, and none
// of them reaches for the advertise gatherers directly.
func TestCertSANOptionsIsWhatEveryCertPathMints(t *testing.T) {
	// The gather's own output, so the assertion below is about a helper
	// that really produces a SAN set rather than a name.
	opts := certSANOptions(&config.Config{CustomEndpoints: []string{"https://bridge.example.test:7788"}})
	if !containsName(opts.ExtraDNSNames, "bridge.example.test") {
		t.Fatalf("certSANOptions dropped the custom endpoint host: %v", opts.ExtraDNSNames)
	}
	if host, _ := os.Hostname(); host != "" && opts.Hostname != host {
		t.Errorf("certSANOptions Hostname = %q, want this host's %q", opts.Hostname, host)
	}

	for _, file := range []string{"main.go", "init.go", "cert.go", "doctor.go"} {
		// readPackageFile blanks comments AND string literals, for the
		// reason this package's other scan guards record: the commentary
		// beside a rule names the identifiers the rule forbids, so an
		// unstripped scan finds its own explanation. Both anchors below
		// are identifiers, which survive the strip.
		src := readPackageFile(t, file)
		if !strings.Contains(src, "certSANOptions(") {
			t.Errorf("%s does not go through certSANOptions", file)
		}
		// cert.go declares the helper, so it is the one file that legitimately
		// names the gatherers.
		if file == "cert.go" {
			continue
		}
		for _, direct := range []string{"GatherCertSANDNS", "GatherCertSANIPs"} {
			if strings.Contains(src, direct) {
				t.Errorf("%s calls advertise.%s directly — put it behind certSANOptions "+
					"so the doctor grades the set a rotation would mint", file, direct)
			}
		}
	}
}

func containsName(names []string, want string) bool {
	for _, n := range names {
		if strings.EqualFold(n, want) {
			return true
		}
	}
	return false
}
