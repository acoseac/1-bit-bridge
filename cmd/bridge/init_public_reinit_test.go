package main

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/adminauth"
)

// `bridge init --public` run again over the install it made: how an operator
// rewrites a public bridge's config, with the same flags and --force.
//
// Measured on 2026-09-26 on dido (row C of #1027's log entry) and again on
// 2026-09-27 on a Mac: the re-init saved the new bridge.yaml, kept the TLS
// cert, and then exited 1, "adminauth: store already has credentials; use
// reset-password to rotate". initCmd wrote the config and loaded the cert
// before it asked the credential store anything, and MintInitial refuses a
// store that holds an account. So the command reported a failure about an
// install it had already changed, and never reached the service install or
// the footer.
//
// Each test drives the real initCmd twice over one --dir, and reads every
// file the runs write or keep, byte for byte, after the second.

// TestInitPublicReinitKeepsTheAdminCredentials is the reported case.
//
// Re-running init is not a request to rotate the admin password, any more
// than it is one to rotate the TLS cert, and `bridge admin reset-password`
// exists for that. So the re-init keeps the account and says so. Both halves
// are held: rotating would lock out whoever holds the password the first run
// printed, and keeping it silently would leave an operator who expects the
// "shown ONCE" box looking for a password that was never made. The file is
// compared byte for byte and the first run's password must still verify.
func TestInitPublicReinitKeepsTheAdminCredentials(t *testing.T) {
	cfgDir := filepath.Join(t.TempDir(), "cfg")
	ports := pickPublicInitPorts(t)

	code, first := publicInit(t, cfgDir, ports, "Existing", "--skip-doctor")
	if code != 0 {
		t.Fatalf("the first public init exited %d:\n%s", code, first)
	}
	password := mintedPassword(t, first)
	before := readInstall(t, cfgDir)

	// The reported command: the same flags again, with --force, and the
	// preflight on.
	code, out := publicInit(t, cfgDir, ports, "Rewritten", "--force")
	// Each assertion names what went wrong, and the run's output is
	// printed once at the end rather than beside each.
	defer logRunOnFailure(t, out)
	after := readInstall(t, cfgDir)

	if code != 0 {
		t.Errorf("a public re-init over a public install exited %d", code)
	}
	if !strings.Contains(after[installConfig], "libraryName: Rewritten") {
		t.Errorf("the re-init did not write the config it was asked for:\n%s", after[installConfig])
	}
	for _, name := range []string{installCredentials, installCert, installKey} {
		if after[name] != before[name] {
			t.Errorf("the re-init changed %s, which it keeps", name)
		}
	}
	store, err := adminauth.OpenStore(filepath.Join(cfgDir, filepath.FromSlash(installCredentials)))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Verify("admin", password); err != nil {
		t.Errorf("the password the first init printed no longer verifies after the re-init: %v", err)
	}
	for _, want := range []string{"Admin credentials — kept", "Username:  admin"} {
		if !strings.Contains(out, want) {
			t.Errorf("the re-init does not say it kept the credentials: no %q", want)
		}
	}
	if strings.Contains(out, "Password:") {
		t.Errorf("the re-init printed a password, and the install's account is the first run's")
	}
	if !strings.Contains(out, "Admin console:") {
		t.Errorf("the re-init stopped before its footer")
	}
}

// TestInitPublicReinitRefusesADamagedCredentialStoreBeforeWriting: a store
// that is there and does not load is a refusal, and a refusal is decided
// before anything is written.
//
// Minting over it would destroy whatever the file still holds, so init
// refuses, as `bridge serve` does on the same file. It did refuse, but only
// after Save: the config was rewritten and the run exited 1 about it.
func TestInitPublicReinitRefusesADamagedCredentialStoreBeforeWriting(t *testing.T) {
	cfgDir := filepath.Join(t.TempDir(), "cfg")
	ports := pickPublicInitPorts(t)
	if code, out := publicInit(t, cfgDir, ports, "Existing", "--skip-doctor"); code != 0 {
		t.Fatalf("the first public init exited %d:\n%s", code, out)
	}
	writeInstallFile(t, cfgDir, installCredentials, `{"user": {"username": "adm`)
	before := readInstall(t, cfgDir)

	code, out := publicInit(t, cfgDir, ports, "Rewritten", "--force")
	defer logRunOnFailure(t, out)
	after := readInstall(t, cfgDir)

	if code == 0 {
		t.Errorf("a public re-init exited 0 over a credential store that does not load")
	}
	assertInstallUnchanged(t, before, after)
	for _, want := range []string{"the config was NOT changed", "move it aside"} {
		if !strings.Contains(out, want) {
			t.Errorf("the refusal does not say %q", want)
		}
	}
}

// TestInitNamesTheRemedyForCredentialsThisUserCannotRead is the refusal's
// other branch. A store this user cannot read is not damaged, and "move it
// aside" is the wrong advice for it: the bridge's own user reads it fine.
// The branch rests on OpenStore wrapping the read error with %w, which a
// change to %v would break without a word.
func TestInitNamesTheRemedyForCredentialsThisUserCannotRead(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a file mode does not deny its owner a read on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root reads a file whatever its mode")
	}
	path := filepath.Join(t.TempDir(), "adminauth.json")
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}

	var stderr bytes.Buffer
	if _, ok := openInitAdminAuth(path, &stderr); ok {
		t.Fatal("opened a credential store this user cannot read")
	}
	out := stderr.String()
	for _, want := range []string{"run init as the user the bridge runs as", "the config was NOT changed"} {
		if !strings.Contains(out, want) {
			t.Errorf("the refusal does not say %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "move it aside") {
		t.Errorf("the refusal tells the operator to move aside a file that is not damaged:\n%s", out)
	}
}

// TestInitReinitRefusesAnIncompleteCertPairBeforeWriting is the same order
// for the TLS pair.
//
// The preflight's tls-cert check FAILs a pair that cannot load, before
// anything is written, but only when the preflight runs, and only for the
// pair it grades (the existing config's, which need not be the one init
// loads). With --skip-doctor the cert step refused after Save, with the
// config already rewritten.
func TestInitReinitRefusesAnIncompleteCertPairBeforeWriting(t *testing.T) {
	cfgDir := filepath.Join(t.TempDir(), "cfg")
	ports := pickPublicInitPorts(t)
	if code, out := publicInit(t, cfgDir, ports, "Existing", "--skip-doctor"); code != 0 {
		t.Fatalf("the first public init exited %d:\n%s", code, out)
	}
	if err := os.Remove(filepath.Join(cfgDir, filepath.FromSlash(installKey))); err != nil {
		t.Fatal(err)
	}
	before := readInstall(t, cfgDir)

	code, out := publicInit(t, cfgDir, ports, "Rewritten", "--force", "--skip-doctor")
	defer logRunOnFailure(t, out)
	after := readInstall(t, cfgDir)

	if code == 0 {
		t.Errorf("a re-init exited 0 over a cert with no key beside it")
	}
	assertInstallUnchanged(t, before, after)
	if !strings.Contains(out, "the config was NOT changed") {
		t.Errorf("the refusal does not say the config was left alone")
	}
}

// TestInitPublicReinitMintsIntoAStoreWithNoAccount is the positive control:
// what init keeps is an ACCOUNT, not a file or an install. A re-init over a
// store with no account in it mints one and shows it, as the first run did.
// That is also what the damaged-store refusal tells the operator to do, move
// the file aside and re-run, so this is the remedy working.
func TestInitPublicReinitMintsIntoAStoreWithNoAccount(t *testing.T) {
	for _, tc := range []struct {
		name    string
		prepare func(t *testing.T, cfgDir string)
	}{
		{"no file", func(t *testing.T, cfgDir string) {
			if err := os.Remove(filepath.Join(cfgDir, filepath.FromSlash(installCredentials))); err != nil {
				t.Fatal(err)
			}
		}},
		{"an empty file", func(t *testing.T, cfgDir string) {
			writeInstallFile(t, cfgDir, installCredentials, "")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfgDir := filepath.Join(t.TempDir(), "cfg")
			ports := pickPublicInitPorts(t)
			if code, out := publicInit(t, cfgDir, ports, "Existing", "--skip-doctor"); code != 0 {
				t.Fatalf("the first public init exited %d:\n%s", code, out)
			}
			tc.prepare(t, cfgDir)

			code, out := publicInit(t, cfgDir, ports, "Rewritten", "--force")
			defer logRunOnFailure(t, out)
			if code != 0 {
				t.Fatalf("a public re-init over a store with no account exited %d", code)
			}
			password := mintedPassword(t, out)
			store, err := adminauth.OpenStore(filepath.Join(cfgDir, filepath.FromSlash(installCredentials)))
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Verify("admin", password); err != nil {
				t.Errorf("the password the re-init printed does not verify: %v", err)
			}
			if strings.Contains(out, "Admin credentials — kept") {
				t.Errorf("the re-init says it kept credentials there were none of")
			}
		})
	}
}

// TestAdminCredentialBoxesAreNotTruncated renders both boxes as init prints
// them. box() cuts a line longer than its body in the middle, with "...",
// and the "shown ONCE" box opened with a 53-character line, so every public
// install until 2026-09-27 printed "Save these now. The plai... is not
// stored anywhere." The password is 16 characters, as MintInitial makes it.
func TestAdminCredentialBoxesAreNotTruncated(t *testing.T) {
	for name, rendered := range map[string]string{
		"minted": mintedAdminCredentialsBox("admin", "ABCDEFGHJKLMNPQR"),
		"kept":   keptAdminCredentialsBox("admin"),
	} {
		if plain := stripANSI(rendered); strings.Contains(plain, "...") {
			t.Errorf("the %s box cuts a line:\n%s", name, plain)
		}
	}
}

// The files a public init writes or keeps, by their slash path under the
// config dir.
const (
	installConfig      = "bridge.yaml"
	installCredentials = "data/adminauth.json"
	installCert        = "data/server.crt"
	installKey         = "data/server.key"
)

// absentFile is how readInstall records a file that is not there. It cannot
// be any file's content, since none of the four holds a NUL.
const absentFile = "\x00absent"

// publicInitPorts are the loopback ports a test's public install binds.
type publicInitPorts struct{ api, admin int }

// pickPublicInitPorts picks two ports that are free when picked, so the
// preflight of the second run, which grades the config's ports, has nothing
// to refuse on them.
func pickPublicInitPorts(t *testing.T) publicInitPorts {
	t.Helper()
	return publicInitPorts{api: freeLoopbackPort(t), admin: freeLoopbackPort(t)}
}

// publicInit runs `bridge init --yes --no-service --public` over cfgDir, on
// the given ports, as the report ran it, named name (no --name when it is
// empty), with any extra flags. It returns the exit code and both streams.
func publicInit(t *testing.T, cfgDir string, ports publicInitPorts, name string, extra ...string) (int, string) {
	t.Helper()
	args := []string{
		"--yes", "--no-service", "--dir", cfgDir,
		"--public", "--domain", "localhost", "--admin-tls-proxy",
		"--listen-address", "127.0.0.1:" + strconv.Itoa(ports.api),
		"--admin-address", "127.0.0.1:" + strconv.Itoa(ports.admin),
	}
	if name != "" {
		args = append(args, "--name", name)
	}
	args = append(args, extra...)
	var out, errOut bytes.Buffer
	code := initCmd(args, strings.NewReader(""), &out, &errOut)
	return code, stripANSI("--- stdout ---\n" + out.String() + "\n--- stderr ---\n" + errOut.String())
}

// mintedPasswordLine is the password line of init's "shown ONCE" box.
var mintedPasswordLine = regexp.MustCompile(`Password:\s+([A-Za-z0-9]+)`)

// mintedPassword returns the password an init printed, failing the test if
// it printed none.
func mintedPassword(t *testing.T, out string) string {
	t.Helper()
	m := mintedPasswordLine.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("the init printed no password:\n%s", out)
	}
	return m[1]
}

// logRunOnFailure prints a run's output once, if the test has failed by the
// time it returns.
func logRunOnFailure(t *testing.T, out string) {
	t.Helper()
	if t.Failed() {
		t.Logf("the run's output:\n%s", out)
	}
}

// readInstall reads the files a public init writes or keeps.
func readInstall(t *testing.T, cfgDir string) map[string]string {
	t.Helper()
	files := map[string]string{}
	for _, name := range []string{installConfig, installCredentials, installCert, installKey} {
		raw, err := os.ReadFile(filepath.Join(cfgDir, filepath.FromSlash(name)))
		switch {
		case err == nil:
			files[name] = string(raw)
		case errors.Is(err, fs.ErrNotExist):
			files[name] = absentFile
		default:
			t.Fatal(err)
		}
	}
	return files
}

// writeInstallFile replaces one of the install's files.
func writeInstallFile(t *testing.T, cfgDir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(cfgDir, filepath.FromSlash(name)), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// assertInstallUnchanged fails for every file a refused run changed. Only the
// config is printed: the others hold a password hash and a private key.
func assertInstallUnchanged(t *testing.T, before, after map[string]string) {
	t.Helper()
	for _, name := range []string{installConfig, installCredentials, installCert, installKey} {
		switch {
		case after[name] == before[name]:
		case name == installConfig:
			t.Errorf("the refused run rewrote the config:\n--- before ---\n%s\n--- after ---\n%s",
				before[name], after[name])
		default:
			t.Errorf("the refused run changed %s", name)
		}
	}
}
