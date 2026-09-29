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
// A first install warns and goes on: a run that has nothing to lose must not
// be stopped over a flag that changes nothing it writes. A rewrite is
// refused, before anything is written: the flags say the operator meant a
// public install, and the rewrite would write a loopback one over the
// install that is there. A run that keeps the config refuses nothing: every
// flag it was given goes unused, which its "keeping it" line already says,
// and an idempotent `bridge init --yes` re-run must go on working.

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

// TestInitFirstInstallWarnsAboutAPublicOnlyFlagWithoutPublic: a first
// install given any of the three without --public is set up as the loopback
// install it writes, and says, in a warning naming the flag, that the flag
// applies only with --public.
func TestInitFirstInstallWarnsAboutAPublicOnlyFlagWithoutPublic(t *testing.T) {
	cases := []struct {
		name  string
		args  []string
		named []string
	}{{"all three", allPublicOnlyFlags(), []string{"--domain", "--email", "--admin-tls-proxy"}}}
	for _, f := range publicOnlyFlags {
		cases = append(cases, struct {
			name  string
			args  []string
			named []string
		}{f.flag + " alone", f.args, []string{f.flag}})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfgDir := filepath.Join(t.TempDir(), "cfg")
			code, out := loopbackInit(t, cfgDir, testLibrary(t), "First", tc.args...)
			defer logRunOnFailure(t, out)
			if code != 0 {
				t.Fatalf("a first install exited %d over a flag that changes nothing it writes", code)
			}
			w := publicOnlyWarning(out)
			if w == "" {
				t.Fatalf("the run said nothing about %v, which it ignores without --public", tc.named)
			}
			for _, f := range tc.named {
				if !strings.Contains(w, f) {
					t.Errorf("the warning does not name %s: %q", f, w)
				}
			}
			for _, f := range publicOnlyFlags {
				if !slices.Contains(tc.named, f.flag) && strings.Contains(w, f.flag) {
					t.Errorf("the warning names %s, which the run was not given: %q", f.flag, w)
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

// TestInitRewriteRefusesAPublicOnlyFlagWithoutPublic: a --yes --force rewrite
// given one of the three without --public is refused with exit 2, the config
// as it was, over a loopback install and a public one alike. Over the public
// one the refusal says that the rewrite would make the install loopback.
func TestInitRewriteRefusesAPublicOnlyFlagWithoutPublic(t *testing.T) {
	for _, install := range postureInstalls {
		for _, tc := range []struct {
			name string
			args []string
		}{{"all three", allPublicOnlyFlags()}, {"--domain alone", publicOnlyFlags[0].args}} {
			t.Run(install.name+"/"+tc.name, func(t *testing.T) {
				cfgDir := filepath.Join(t.TempDir(), "cfg")
				lib := install.make(t, cfgDir)
				before := readConfigFile(t, cfgDir)

				code, out := loopbackInit(t, cfgDir, lib, "Rewritten", append([]string{"--force"}, tc.args...)...)
				defer logRunOnFailure(t, out)
				if code != 2 {
					t.Fatalf("a --force rewrite given %v without --public exited %d, want 2", tc.args, code)
				}
				if after := readConfigFile(t, cfgDir); after != before {
					t.Errorf("the refused rewrite changed the config:\n%s", after)
				}
				for _, want := range []string{tc.args[0], "--public", "NOT changed"} {
					if !strings.Contains(out, want) {
						t.Errorf("the refusal does not say %q", want)
					}
				}
				const public = "is a public one"
				if install.public != strings.Contains(out, public) {
					t.Errorf("over a public install: %v; the refusal says %q: %v", install.public, public, !install.public)
				}
			})
		}
	}
}

// TestInitRewriteWithPublicIsNotRefused is the control: the same rewrite with
// --public writes a public install.
func TestInitRewriteWithPublicIsNotRefused(t *testing.T) {
	for _, install := range postureInstalls {
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
// without --public is not refused: it did not fail on the run before.
func TestInitRunThatKeepsTheConfigIsNotRefused(t *testing.T) {
	cfgDir := filepath.Join(t.TempDir(), "cfg")
	lib := postureInstalls[0].make(t, cfgDir)
	before := readConfigFile(t, cfgDir)
	code, out := loopbackInit(t, cfgDir, lib, "", allPublicOnlyFlags()...)
	defer logRunOnFailure(t, out)
	if code != 0 {
		t.Fatalf("a --yes re-run that keeps the config exited %d", code)
	}
	if !strings.Contains(out, "keeping it") {
		t.Errorf("the run does not say it keeps the config")
	}
	// A warning that the run "sets up a loopback install" would be false:
	// it sets up nothing, and the config it keeps may be public.
	if w := publicOnlyWarning(out); w != "" {
		t.Errorf("the run that keeps the config warned %q", w)
	}
	if after := readConfigFile(t, cfgDir); after != before {
		t.Errorf("the run changed the config it keeps")
	}
}

// TestInitInteractiveRewriteRefusesAPublicOnlyFlagWithoutPublic: an
// interactive run asks "Overwrite?" before it grades or writes anything, and
// a yes makes it the rewrite a --force run is, refused alike. A no keeps the
// config, as --yes without --force does.
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
			lib := postureInstalls[0].make(t, cfgDir)
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
			if got, want := loadInstallConfig(t, cfgDir).Autocert.Email, map[bool]string{true: "", false: "ops@example.test"}[tc.warns]; got != want {
				t.Errorf("autocert.email = %q, want %q", got, want)
			}
		})
	}
}

// postureInstalls are the two installs a rewrite can find: a loopback one and
// a public one, each written by `bridge init`. make returns the library the
// install was made with.
var postureInstalls = []struct {
	name   string
	public bool
	make   func(t *testing.T, cfgDir string) string
}{
	{"over a loopback install", false, func(t *testing.T, cfgDir string) string {
		t.Helper()
		lib := testLibrary(t)
		if code, out := loopbackInit(t, cfgDir, lib, "First"); code != 0 {
			t.Fatalf("the first init exited %d:\n%s", code, out)
		}
		return lib
	}},
	{"over a public install", true, func(t *testing.T, cfgDir string) string {
		t.Helper()
		lib := testLibrary(t)
		if code, out := loopbackInit(t, cfgDir, lib, "First",
			"--public", "--domain", "bridge.example.test", "--admin-tls-proxy",
			"--listen-address", loopbackAddr(freeLoopbackPort(t)), "--admin-address", loopbackAddr(freeLoopbackPort(t))); code != 0 {
			t.Fatalf("the first init exited %d:\n%s", code, out)
		}
		return lib
	}},
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
