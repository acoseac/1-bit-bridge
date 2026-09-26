package main

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"strconv"
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
	withExistingInstallDeps(&d, filepath.Join(t.TempDir(), "bridge.yaml"))
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
	withExistingInstallDeps(&unmanaged, plain)
	if unmanaged.Managed {
		t.Error("a config with no deployment block graded as managed")
	}

	var d doctor.Deps
	withExistingInstallDeps(&d, cfgPath)
	if !d.Managed {
		t.Error("the preflight does not carry the managed posture, so a hosted tenant " +
			"re-running init is told to run `bridge cert rotate` on a host it does not own")
	}
}

// TestInitPreflightGradesTheInstallsOwnPortsAndPidFile — the fourth and
// fifth fields on the same install, and the two that made the port
// checks answer about a bridge nobody runs.
//
// `bridge init`'s Deps literal hard-codes APIPort 7788 / AdminPort 7789,
// the DEFAULTS. On a public-mode install listening on :443 those two are
// free, so `port-api` graded ok — a check passing because the thing it
// guards is absent, which is the vacuous shape this repo keeps paying
// for. And with no OwnPIDFile, checkPort cannot run its "is that bound
// port US?" ladder at all, so a re-init while the operator's own bridge
// is running graded FAIL and aborted init. The call site's own comment
// named that as the reason --skip-doctor exists.
//
// Asserted on the Deps, like the managed-posture test above: what the
// ports change is which address checkPort dials, and driving that would
// be a test about the network rather than about the wiring.
func TestInitPreflightGradesTheInstallsOwnPortsAndPidFile(t *testing.T) {
	tmp := t.TempDir()
	lib := filepath.Join(tmp, "Music")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	dataDir := filepath.Join(tmp, "data")
	cfgPath := filepath.Join(tmp, "bridge.yaml")
	body := "libraryRoots:\n  - " + lib + "\n" +
		"dataDir: " + dataDir + "\n" +
		"listenAddress: \"0.0.0.0:443\"\n" +
		"adminAddress: \"127.0.0.1:7791\"\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	// Seeded with the defaults the call site passes, so the assertions
	// below fail if the helper simply leaves them alone.
	d := doctor.Deps{APIPort: 7788, AdminPort: 7789}
	withExistingInstallDeps(&d, cfgPath)

	if d.APIPort != 443 {
		t.Errorf("APIPort = %d, want 443 — the preflight graded the DEFAULT port, "+
			"which is free on this host, so the check passed about a listener nobody runs", d.APIPort)
	}
	if d.AdminPort != 7791 {
		t.Errorf("AdminPort = %d, want 7791", d.AdminPort)
	}
	wantPID := filepath.Join(dataDir, "server.pid")
	if d.OwnPIDFile != wantPID {
		t.Errorf("OwnPIDFile = %q, want %q — without it checkPort cannot tell the operator's "+
			"own running bridge from a stranger, and a re-init aborts", d.OwnPIDFile, wantPID)
	}
	// A config that loads names its bridge's ports, so the whole "is it
	// us?" ladder applies to them.
	if d.OwnPIDPortsUnknown {
		t.Error("OwnPIDPortsUnknown = true for a config that loaded")
	}
}

// TestInitPreflightLeavesAFirstInstallsPortsAlone is the negative
// control for the test above: with no config at the target path the
// helper must not invent ports or a pid file, or the "first install
// keeps the existing skip" judgement recorded in its docblock would be
// quietly untrue for the port checks.
func TestInitPreflightLeavesAFirstInstallsPortsAlone(t *testing.T) {
	dir := t.TempDir()
	// DataDir set as initCmd sets it, so a helper that pointed a missing
	// config at the data dir's pid file, as it does a broken one, fails
	// here rather than being stopped by an empty DataDir.
	d := doctor.Deps{DataDir: filepath.Join(dir, "data"), APIPort: 7788, AdminPort: 7789}
	withExistingInstallDeps(&d, filepath.Join(dir, "bridge.yaml"))
	if d.APIPort != 7788 || d.AdminPort != 7789 {
		t.Errorf("ports = %d/%d, want the caller's 7788/7789 untouched", d.APIPort, d.AdminPort)
	}
	if d.OwnPIDFile != "" || d.OwnPIDPortsUnknown {
		t.Errorf("OwnPIDFile = %q, OwnPIDPortsUnknown = %v; want empty and false — there is no install "+
			"to own a pid file", d.OwnPIDFile, d.OwnPIDPortsUnknown)
	}
}

// TestInitRefusesToSaveAPortItNeverGraded drives the real initCmd,
// because the gap is in the ORDER and no assertion on doctor.Deps can
// see it.
//
// The preflight runs before the keep-or-overwrite decision, against the
// config already on disk. For the certificate that reading is right and
// deliberate: init does not rewrite the cert, so the pair on disk IS
// the pair. The ports are the opposite — baseConfig always seeds the
// loopback defaults and --public replaces them — so an install on
// :9090/:9091 was graded on 9090/9091, passed, and was then handed
// :7788/127.0.0.1:7789. If something else holds one of those, the
// operator learns it from a `bridge serve` that cannot bind, having
// just been told the host was fine.
//
// #963's own test asserts the Deps and never runs initCmd, which is why
// it stayed green.
func TestInitRefusesToSaveAPortItNeverGraded(t *testing.T) {
	tmp := t.TempDir()
	lib := filepath.Join(tmp, "Music")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgDir := filepath.Join(tmp, "cfg")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(cfgDir, "bridge.yaml")

	// Hold the port this init is ABOUT to write, and nothing else. An
	// ephemeral listener gives a real number to put in the existing
	// config, so the ports genuinely differ.
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	_, heldPortStr, err := net.SplitHostPort(held.Addr().String())
	if err != nil {
		t.Fatal(err)
	}

	// The install that is THERE listens somewhere else entirely, so the
	// preflight's own pass grades two ports that are free.
	free, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, otherPortStr, err := net.SplitHostPort(free.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	free.Close()

	body := "libraryRoots:\n  - " + lib + "\n" +
		"dataDir: " + filepath.Join(cfgDir, "data") + "\n" +
		"listenAddress: \"127.0.0.1:" + otherPortStr + "\"\n" +
		"adminAddress: \"127.0.0.1:" + otherPortStr + "\"\n" +
		"libraryName: Existing\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	code := initCmd([]string{
		"--yes", "--force", "--no-service",
		"--dir", cfgDir,
		"--library", lib,
		"--name", "Rewritten",
		// The admin address this run will actually save, which is the
		// one nothing graded.
		"--public", "--domain", "example.test", "--admin-tls-proxy",
		"--admin-address", "127.0.0.1:" + heldPortStr,
		"--listen-address", "127.0.0.1:" + otherPortStr,
	}, strings.NewReader(""), &out, &errOut)

	if code == 0 {
		t.Fatalf("init exited 0 while saving a port another process holds\n--- stdout ---\n%s\n--- stderr ---\n%s",
			out.String(), errOut.String())
	}
	if !strings.Contains(out.String(), "port-admin") {
		t.Errorf("the refusal does not name port-admin:\n%s", out.String())
	}
	// And the existing config survives a refusal — the whole reason the
	// second pass runs before Save.
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "Existing") {
		t.Errorf("the config was rewritten despite the refusal:\n%s", raw)
	}
}

// TestInitWritesAConfigWhosePortsAreFree is the positive control: the
// second pass must not refuse an ordinary overwrite, or "grade the
// ports you will save" would be indistinguishable from "refuse to
// save".
func TestInitWritesAConfigWhosePortsAreFree(t *testing.T) {
	tmp := t.TempDir()
	lib := filepath.Join(tmp, "Music")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgDir := filepath.Join(tmp, "cfg")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(cfgDir, "bridge.yaml")

	free := func(t *testing.T) string {
		t.Helper()
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		_, p, err := net.SplitHostPort(l.Addr().String())
		l.Close()
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	oldPort, newAPI, newAdmin := free(t), free(t), free(t)

	body := "libraryRoots:\n  - " + lib + "\n" +
		"dataDir: " + filepath.Join(cfgDir, "data") + "\n" +
		"listenAddress: \"127.0.0.1:" + oldPort + "\"\n" +
		"adminAddress: \"127.0.0.1:" + oldPort + "\"\n" +
		"libraryName: Existing\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	if code := initCmd([]string{
		"--yes", "--force", "--no-service",
		"--dir", cfgDir, "--library", lib, "--name", "Rewritten",
		"--public", "--domain", "example.test", "--admin-tls-proxy",
		"--admin-address", "127.0.0.1:" + newAdmin,
		"--listen-address", "127.0.0.1:" + newAPI,
	}, strings.NewReader(""), &out, &errOut); code != 0 {
		t.Fatalf("init exited %d on free ports\n--- stdout ---\n%s\n--- stderr ---\n%s",
			code, out.String(), errOut.String())
	}
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "Rewritten") {
		t.Errorf("the config was not rewritten:\n%s", raw)
	}
}

// TestConfiguredPortReadsAnEphemeralPortAsItself.
//
// splitHostPort folds port 0 in with a parse failure, and both Deps
// assemblies seed the DEFAULTS and overwrite them only when it says ok.
// So an install on `listenAddress: ":0"` — the documented
// OS-picks-an-ephemeral-port mode config.validatePort accepts, and what
// every `:0` fixture uses — was graded on 7788 and 7789: ports it does
// not use, usually free, so the checks passed about listeners this
// bridge does not have, and a re-init aborted if something else held
// 7789. checkPort has had the honest answer for 0 all along ("no port
// set", warn, non-blocking); it simply never received it.
func TestConfiguredPortReadsAnEphemeralPortAsItself(t *testing.T) {
	for _, tc := range []struct {
		addr string
		port int
		ok   bool
	}{
		{":0", 0, true},
		{"127.0.0.1:0", 0, true},
		// Atoi, not a text compare: validatePort runs Atoi, so every
		// spelling of zero is legal and a compare against "0" would
		// admit the rest.
		{"127.0.0.1:00", 0, true},
		{":7788", 7788, true},
		{"", 0, false},
		{"no-port-here", 0, false},
		{"127.0.0.1:http", 0, false},
	} {
		t.Run(tc.addr, func(t *testing.T) {
			port, ok := configuredPort(tc.addr)
			if port != tc.port || ok != tc.ok {
				t.Errorf("configuredPort(%q) = %d, %v; want %d, %v", tc.addr, port, ok, tc.port, tc.ok)
			}
		})
	}

	// And through the Deps assembly, which is where the defaults were
	// substituted.
	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "bridge.yaml")
	body := "libraryRoots:\n  - " + tmp + "\n" +
		"dataDir: " + filepath.Join(tmp, "data") + "\n" +
		"listenAddress: \":0\"\n" +
		"adminAddress: \"127.0.0.1:0\"\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	d := doctor.Deps{APIPort: 7788, AdminPort: 7789}
	withExistingInstallDeps(&d, cfgPath)
	if d.APIPort != 0 || d.AdminPort != 0 {
		t.Errorf("ports = %d/%d, want 0/0 — the defaults were substituted for an install "+
			"that names no port, so the checks answered about listeners it does not have",
			d.APIPort, d.AdminPort)
	}
}

// TestInitDoesNotExcuseAChangedPortWithItsOwnLivePID.
//
// checkPort's "is it us?" ladder answers ok or warn — never fail —
// whenever the pid in OwnPIDFile is alive and the owner probe could not
// rule it out: a probe that could not attribute the port warns, and a
// listener merely owned by this uid is reported ok. That is right for a
// port the running bridge is supposed
// to hold, and wrong for one it is not. A live bridge binds what ITS
// config says, so it cannot legitimately own a port absent from it —
// and with the fallback left on, an occupied NEW port read as "our
// bridge is still running", HasFail stayed false, and the config was
// saved anyway.
//
// Which is the check passing because the thing it guards is absent, one
// level in from the defect this whole pass exists for. (CodeRabbit on
// #970.)
//
// The fixture is this test process: its own pid in the pid file is
// alive by construction, and it holds the port itself. So a pid file
// left in place would excuse the port through every arm of the ladder:
// lsof finds the recorded pid listening, and on a host without lsof the
// recorded pid is alive and, on Linux, the listener carries this uid.
//
// On a host without lsof this test also failed with the pid file
// CLEARED, because checkPort then warned about any port with no live pid
// of ours behind it. That fallback is gone (see
// internal/doctor/nolsof_notwindows_test.go).
func TestInitDoesNotExcuseAChangedPortWithItsOwnLivePID(t *testing.T) {
	tmp := t.TempDir()
	lib := filepath.Join(tmp, "Music")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgDir := filepath.Join(tmp, "cfg")
	dataDir := filepath.Join(cfgDir, "data")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// The pid file the preflight wires from the config's dataDir. Our
	// own pid, so pidAliveFunc says yes.
	if err := os.WriteFile(filepath.Join(dataDir, "server.pid"),
		[]byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		t.Fatal(err)
	}

	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	_, heldPortStr, err := net.SplitHostPort(held.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	freeL, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, otherPortStr, err := net.SplitHostPort(freeL.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	freeL.Close()

	cfgPath := filepath.Join(cfgDir, "bridge.yaml")
	body := "libraryRoots:\n  - " + lib + "\n" +
		"dataDir: " + dataDir + "\n" +
		"listenAddress: \"127.0.0.1:" + otherPortStr + "\"\n" +
		"adminAddress: \"127.0.0.1:" + otherPortStr + "\"\n" +
		"libraryName: Existing\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	code := initCmd([]string{
		"--yes", "--force", "--no-service",
		"--dir", cfgDir, "--library", lib, "--name", "Rewritten",
		"--public", "--domain", "example.test", "--admin-tls-proxy",
		"--admin-address", "127.0.0.1:" + heldPortStr,
		"--listen-address", "127.0.0.1:" + otherPortStr,
	}, strings.NewReader(""), &out, &errOut)

	if code == 0 {
		t.Fatalf("init exited 0 while saving a changed port another process holds (our recorded pid "+
			"is alive, so a pid file left in place would excuse it)\n--- stdout ---\n%s\n--- stderr ---\n%s",
			out.String(), errOut.String())
	}
	// WHICH check refused, not merely that something did. The api port
	// is released and unchanged, so nothing grades it as held here, and
	// a bare "nonzero exit plus an unchanged config" would be satisfied
	// by an unrelated failure and prove nothing about the held admin port
	// (CodeRabbit on #970).
	if !strings.Contains(out.String(), "port-admin") {
		t.Errorf("the refusal does not name port-admin, so it is not the held port that stopped "+
			"this init:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "these are the ports this init would write") {
		t.Errorf("the refusal did not come from the second port pass:\n%s", out.String())
	}
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "Existing") {
		t.Errorf("the config was rewritten despite the refusal:\n%s", raw)
	}
}

// TestAutoStartProbeTargetSkipsAnEphemeralPort.
//
// spawnNowOrWarn skips its auto-start when something already holds the
// admin port. With `adminAddress: ":0"` there is no such port — the OS
// picks one at bind time — and the old code folded that in with a parse
// failure and substituted 127.0.0.1:7789, so a listener the operator
// never asked for could suppress an auto-start that would have worked.
//
// The decision is extracted because spawnNowOrWarn's other branch
// starts a real detached process, so the behaviour had no test at all.
func TestAutoStartProbeTargetSkipsAnEphemeralPort(t *testing.T) {
	for _, tc := range []struct {
		addr, host string
		port       int
		probe      bool
	}{
		// The ephemeral modes: parsed, but nothing to dial.
		{":0", "", 0, false},
		{"127.0.0.1:0", "127.0.0.1", 0, false},
		{"127.0.0.1:00", "127.0.0.1", 0, false},
		// An ordinary address is probed as before.
		{"127.0.0.1:7789", "127.0.0.1", 7789, true},
		{":7789", "", 7789, true},
		// And an unparseable one takes the documented fallback.
		{"", "127.0.0.1", 7789, true},
		{"no-port-here", "127.0.0.1", 7789, true},
		{"127.0.0.1:http", "127.0.0.1", 7789, true},
	} {
		t.Run(tc.addr, func(t *testing.T) {
			host, port, probe := autoStartProbeTarget(tc.addr)
			if host != tc.host || port != tc.port || probe != tc.probe {
				t.Errorf("autoStartProbeTarget(%q) = %q, %d, %v; want %q, %d, %v",
					tc.addr, host, port, probe, tc.host, tc.port, tc.probe)
			}
		})
	}
}
