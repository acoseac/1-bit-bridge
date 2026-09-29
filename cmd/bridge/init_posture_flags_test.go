package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/term"
)

// `bridge init`'s --domain, --email and --admin-tls-proxy describe a public
// install, and a run without --public ignored them without a word. Measured
// with the real binary on 2026-09-29 (backlog B61): a loopback first install
// given all three exited 0, printed nothing about them and saved none of
// them; and a --yes --force rewrite of a PUBLIC install given --domain and
// --admin-tls-proxy but not --public exited 0 with a loopback config, the
// public endpoint every paired device dials dropped, and printed nothing
// about the posture either.
//
// A rewrite of a public install is refused, before anything is written: the
// flags say the operator meant a public install, and the rewrite would make
// this one loopback. A first install, and a rewrite of a loopback install,
// warn and go on: each writes a working loopback install and loses nothing,
// so a flag that changes nothing it writes does not stop it. A run that keeps
// the config says nothing more: every flag it was given goes unused, which
// its "keeping it" line already says, and an idempotent `bridge init --yes`
// re-run must go on working.

// publicOnlyFlags are the three flags, each with a value where it takes one.
var publicOnlyFlags = []struct {
	flag string
	args []string
}{
	{"--domain", []string{"--domain", "bridge.example.test"}},
	{"--email", []string{"--email", "ops@example.test"}},
	{"--admin-tls-proxy", []string{"--admin-tls-proxy"}},
}

// allPublicOnlyFlags is the three flags' arguments together.
func allPublicOnlyFlags() []string {
	var args []string
	for _, f := range publicOnlyFlags {
		args = append(args, f.args...)
	}
	return args
}

// flagCase is a run's public-only flags, and the ones it must name.
type flagCase struct {
	name  string
	args  []string
	named []string
}

// flagCases are the three together and each alone.
func flagCases() []flagCase {
	cases := []flagCase{{"all three", allPublicOnlyFlags(), []string{"--domain", "--email", "--admin-tls-proxy"}}}
	for _, f := range publicOnlyFlags {
		cases = append(cases, flagCase{f.flag + " alone", f.args, []string{f.flag}})
	}
	return cases
}

// TestInitFirstInstallWarnsAboutAPublicOnlyFlagWithoutPublic: a first
// install given any of the three without --public is set up as the loopback
// install it writes, and says, in a warning naming the flag, that the flag
// applies only with --public.
func TestInitFirstInstallWarnsAboutAPublicOnlyFlagWithoutPublic(t *testing.T) {
	for _, tc := range flagCases() {
		t.Run(tc.name, func(t *testing.T) {
			cfgDir := filepath.Join(t.TempDir(), "cfg")
			code, out := loopbackInit(t, cfgDir, testLibrary(t), "First", tc.args...)
			defer logRunOnFailure(t, out)
			if code != 0 {
				t.Fatalf("a first install exited %d over a flag that changes nothing it writes", code)
			}
			assertWarnsLoopback(t, out, cfgDir, tc.named)
		})
	}
}

// TestInitLoopbackRewriteWarnsAboutAPublicOnlyFlagWithoutPublic: a --force
// rewrite of a loopback install given the three without --public writes the
// loopback install it would have written anyway, and warns as a first
// install does. It is not refused: a script that rewrites its config with
// --yes --force on every run, passing these flags, would otherwise fail on
// its second run, where its first only warned.
func TestInitLoopbackRewriteWarnsAboutAPublicOnlyFlagWithoutPublic(t *testing.T) {
	for _, tc := range flagCases() {
		t.Run(tc.name, func(t *testing.T) {
			cfgDir := filepath.Join(t.TempDir(), "cfg")
			lib := loopbackPosture.make(t, cfgDir)
			code, out := loopbackInit(t, cfgDir, lib, "Rewritten", append([]string{"--force"}, tc.args...)...)
			defer logRunOnFailure(t, out)
			if code != 0 {
				t.Fatalf("a --force rewrite of a loopback install exited %d over a flag that changes nothing it writes", code)
			}
			assertWarnsLoopback(t, out, cfgDir, tc.named)
			if got := loadInstallConfig(t, cfgDir).LibraryName; got != "Rewritten" {
				t.Errorf("libraryName = %q, want the rewrite's Rewritten", got)
			}
		})
	}
}

// TestInitWarnsWithoutEchoingTheDomain: the warning names the flag and never
// its value. A --domain may carry a user name and password, which --public
// refuses without echoing (backlog B54), and the warning must not print what
// that refusal keeps out of the output.
func TestInitWarnsWithoutEchoingTheDomain(t *testing.T) {
	cfgDir := filepath.Join(t.TempDir(), "cfg")
	code, out := loopbackInit(t, cfgDir, testLibrary(t), "First", "--domain", "user:S3cret-pw@bridge.example.test")
	defer logRunOnFailure(t, out)
	if code != 0 {
		t.Fatalf("a first install exited %d", code)
	}
	if publicOnlyWarning(out) == "" {
		t.Fatalf("the run said nothing about --domain")
	}
	if strings.Contains(strings.ToLower(out), "s3cret-pw") {
		t.Errorf("the run printed the password the --domain value carries")
	}
}

// TestInitPublicRewriteRefusesAPublicOnlyFlagWithoutPublic: a --yes --force
// rewrite of a PUBLIC install given one of the three without --public is
// refused with exit 2, the config as it was, and says the rewrite would make
// the public install loopback.
func TestInitPublicRewriteRefusesAPublicOnlyFlagWithoutPublic(t *testing.T) {
	for _, tc := range flagCases() {
		t.Run(tc.name, func(t *testing.T) {
			cfgDir := filepath.Join(t.TempDir(), "cfg")
			lib := publicPosture.make(t, cfgDir)
			before := readConfigFile(t, cfgDir)

			code, out := loopbackInit(t, cfgDir, lib, "Rewritten", append([]string{"--force"}, tc.args...)...)
			defer logRunOnFailure(t, out)
			if code != 2 {
				t.Fatalf("a --force rewrite of a public install given %v without --public exited %d, want 2", tc.args, code)
			}
			if after := readConfigFile(t, cfgDir); after != before {
				t.Errorf("the refused rewrite changed the config:\n%s", after)
			}
			for _, want := range append(slices.Clone(tc.named), "--public", "the public install", "NOT changed") {
				if !strings.Contains(out, want) {
					t.Errorf("the refusal does not say %q", want)
				}
			}
		})
	}
}

// TestInitRewriteWithPublicIsNotRefused is the control: the same rewrite with
// --public writes a public install, over either posture.
func TestInitRewriteWithPublicIsNotRefused(t *testing.T) {
	for _, install := range []installPosture{loopbackPosture, publicPosture} {
		t.Run(install.name, func(t *testing.T) {
			cfgDir := filepath.Join(t.TempDir(), "cfg")
			lib := install.make(t, cfgDir)
			code, out := loopbackInit(t, cfgDir, lib, "Rewritten",
				"--force", "--public", "--domain", "bridge.example.test", "--admin-tls-proxy",
				"--listen-address", loopbackAddr(freeLoopbackPort(t)), "--admin-address", loopbackAddr(freeLoopbackPort(t)))
			defer logRunOnFailure(t, out)
			if code != 0 {
				t.Fatalf("a public --force rewrite exited %d", code)
			}
			if !loadInstallConfig(t, cfgDir).IsPublic() {
				t.Errorf("the public rewrite did not save a public install")
			}
			if publicOnlyWarning(out) != "" {
				t.Errorf("a public run warned that its public flags need --public")
			}
		})
	}
}

// TestInitRunThatKeepsTheConfigIsNotRefused: `bridge init --yes` without
// --force keeps the config, and every flag it was given goes unused, which
// its "keeping it" line says. An idempotent re-run that passes the three
// without --public is neither refused nor warned at, over either posture: it
// did not fail on the run before, and "sets up a loopback install" would be
// false about a run that sets up nothing.
func TestInitRunThatKeepsTheConfigIsNotRefused(t *testing.T) {
	for _, install := range []installPosture{loopbackPosture, publicPosture} {
		t.Run(install.name, func(t *testing.T) {
			cfgDir := filepath.Join(t.TempDir(), "cfg")
			lib := install.make(t, cfgDir)
			before := readConfigFile(t, cfgDir)
			code, out := loopbackInit(t, cfgDir, lib, "", allPublicOnlyFlags()...)
			defer logRunOnFailure(t, out)
			if code != 0 {
				t.Fatalf("a --yes re-run that keeps the config exited %d", code)
			}
			if !strings.Contains(out, "keeping it") {
				t.Errorf("the run does not say it keeps the config")
			}
			if w := publicOnlyWarning(out); w != "" {
				t.Errorf("the run that keeps the config warned %q", w)
			}
			if after := readConfigFile(t, cfgDir); after != before {
				t.Errorf("the run changed the config it keeps")
			}
		})
	}
}

// TestInitInteractiveRewriteRefusesAPublicOnlyFlagWithoutPublic: an
// interactive run asks "Overwrite?" before it grades or writes anything, and
// over a public install a yes makes it the rewrite a --force run is, refused
// alike. A no keeps the config, as --yes without --force does.
func TestInitInteractiveRewriteRefusesAPublicOnlyFlagWithoutPublic(t *testing.T) {
	if term.IsTerminal(int(os.Stdin.Fd())) {
		t.Skip("stdin is a terminal, where init would offer to start the bridge after a run that keeps the config")
	}
	for _, tc := range []struct {
		answer   string
		wantCode int
	}{{"y", 2}, {"n", 0}} {
		t.Run("Overwrite? "+tc.answer, func(t *testing.T) {
			cfgDir := filepath.Join(t.TempDir(), "cfg")
			lib := publicPosture.make(t, cfgDir)
			before := readConfigFile(t, cfgDir)
			var stdout, stderr strings.Builder
			code := initCmd(append([]string{"--no-service", "--skip-doctor", "--dir", cfgDir}, allPublicOnlyFlags()...),
				strings.NewReader(lib+"\n"+tc.answer+"\n"), &stdout, &stderr)
			out := stripANSI("--- stdout ---\n" + stdout.String() + "\n--- stderr ---\n" + stderr.String())
			defer logRunOnFailure(t, out)
			if code != tc.wantCode {
				t.Fatalf("the run answered %q exited %d, want %d", tc.answer, code, tc.wantCode)
			}
			if after := readConfigFile(t, cfgDir); after != before {
				t.Errorf("the run changed the config:\n%s", after)
			}
		})
	}
}

// TestInitWarnsAboutAnEmailTheProxyDoesNotUse: a public run with
// --admin-tls-proxy writes no ACME client, so --email, the ACME contact, is
// ignored there too. The run warns, and a public run without the proxy, which
// writes the email, does not.
func TestInitWarnsAboutAnEmailTheProxyDoesNotUse(t *testing.T) {
	for _, tc := range []struct {
		name  string
		args  []string
		warns bool
	}{
		{"with --admin-tls-proxy", []string{"--admin-tls-proxy", "--email", "ops@example.test",
			"--listen-address", loopbackAddr(freeLoopbackPort(t)), "--admin-address", loopbackAddr(freeLoopbackPort(t))}, true},
		// The bridge's own ACME: the API on :443, which --skip-doctor does not bind.
		{"with the bridge's own ACME", []string{"--email", "ops@example.test"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfgDir := filepath.Join(t.TempDir(), "cfg")
			code, out := loopbackInit(t, cfgDir, testLibrary(t), "First",
				append([]string{"--public", "--domain", "bridge.example.test"}, tc.args...)...)
			defer logRunOnFailure(t, out)
			if code != 0 {
				t.Fatalf("a public first install exited %d", code)
			}
			w := emailIgnoredWarning(out)
			if tc.warns && (w == "" || !strings.Contains(w, "--admin-tls-proxy")) {
				t.Errorf("the run said nothing about the --email it does not write, or not why: %q", w)
			}
			if !tc.warns && w != "" {
				t.Errorf("the run warned about an --email it writes: %q", w)
			}
			want := "ops@example.test"
			if tc.warns {
				want = ""
			}
			if got := loadInstallConfig(t, cfgDir).Autocert.Email; got != want {
				t.Errorf("autocert.email = %q, want %q", got, want)
			}
		})
	}
}

// installPosture is an install a rewrite can find, written by `bridge init`.
// make writes it at cfgDir and returns the library it was made with.
type installPosture struct {
	name string
	make func(t *testing.T, cfgDir string) string
}

// loopbackPosture and publicPosture are the two installs `bridge init` writes.
var (
	loopbackPosture = installPosture{"over a loopback install", func(t *testing.T, cfgDir string) string {
		t.Helper()
		lib := testLibrary(t)
		if code, out := loopbackInit(t, cfgDir, lib, "First"); code != 0 {
			t.Fatalf("the first init exited %d:\n%s", code, out)
		}
		return lib
	}}
	publicPosture = installPosture{"over a public install", func(t *testing.T, cfgDir string) string {
		t.Helper()
		lib := testLibrary(t)
		if code, out := loopbackInit(t, cfgDir, lib, "First",
			"--public", "--domain", "bridge.example.test", "--admin-tls-proxy",
			"--listen-address", loopbackAddr(freeLoopbackPort(t)), "--admin-address", loopbackAddr(freeLoopbackPort(t))); code != 0 {
			t.Fatalf("the first init exited %d:\n%s", code, out)
		}
		return lib
	}}
)

// assertWarnsLoopback requires out to carry the warning naming exactly the
// named flags, and the config at cfgDir to be the loopback install the run
// wrote, with none of the three's settings.
func assertWarnsLoopback(t *testing.T, out, cfgDir string, named []string) {
	t.Helper()
	w := publicOnlyWarning(out)
	if w == "" {
		t.Fatalf("the run said nothing about %v, which it ignores without --public", named)
	}
	for _, f := range publicOnlyFlags {
		if slices.Contains(named, f.flag) != strings.Contains(w, f.flag) {
			t.Errorf("the warning's naming of %s is %v, want %v: %q",
				f.flag, strings.Contains(w, f.flag), slices.Contains(named, f.flag), w)
		}
	}
	if !strings.Contains(w, "--public") || !strings.Contains(w, "loopback") {
		t.Errorf("the warning does not say the flags need --public and that the install is loopback: %q", w)
	}
	cfg := loadInstallConfig(t, cfgDir)
	if cfg.IsPublic() || cfg.Autocert.Domain != "" || cfg.Autocert.Email != "" ||
		cfg.Deployment.AdminTLSTerminatedByProxy {
		t.Errorf("the loopback install saved a public setting: public %v, domain %q, email %q, proxy %v",
			cfg.IsPublic(), cfg.Autocert.Domain, cfg.Autocert.Email, cfg.Deployment.AdminTLSTerminatedByProxy)
	}
}

// publicOnlyWarning returns the line of out that warns about a flag that
// applies only with --public, or "".
func publicOnlyWarning(out string) string {
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "warning:") && strings.Contains(l, "only with --public") {
			return l
		}
	}
	return ""
}

// emailIgnoredWarning returns the line of out that warns about an --email
// the run does not write, or "".
func emailIgnoredWarning(out string) string {
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "warning: --email") {
			return l
		}
	}
	return ""
}
