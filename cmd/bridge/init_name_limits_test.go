package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/term"
)

// TestInitRefusesANameAPairingCodeCannotCarry: `bridge init --name` takes a
// name the app's pairing parser takes, or refuses it before it writes
// anything. On 2026-09-27 (main at 3214aa17) `--name $'Caf\xe9 Tunes'`,
// which is "Café Tunes" typed in a Latin-1 terminal, was saved as
// `!!binary`, served as "Caf\uFFFD Tunes", and put name=Caf%E9 in every
// pairing QR, which the app refuses as "missing the name field"; a name over
// 256 Characters it refuses as too long. The operator typed it, so the run
// says so and exits 2, as for any other bad flag: a first install makes no
// config dir, and a rewrite leaves bridge.yaml as it was. The cap counts
// runes, as the app counts Characters, so 256 two-byte runes are saved.
func TestInitRefusesANameAPairingCodeCannotCarry(t *testing.T) {
	for _, tc := range []struct{ label, name string }{
		{"257 runes", strings.Repeat("a", 257)},
		{"257 two-byte runes", strings.Repeat("é", 257)},
		{"not UTF-8", "Caf\xe9 Tunes"},
	} {
		t.Run("first install/"+tc.label, func(t *testing.T) {
			cfgDir := filepath.Join(t.TempDir(), "cfg")
			code, out := loopbackInit(t, cfgDir, testLibrary(t), tc.name)
			defer logRunOnFailure(t, out)
			if code != 2 {
				t.Fatalf("the init exited %d, want 2", code)
			}
			if !strings.Contains(out, "--name") {
				t.Errorf("the refusal does not name --name")
			}
			if _, err := os.Stat(cfgDir); !os.IsNotExist(err) {
				t.Errorf("the refused init made its config dir (stat: %v)", err)
			}
		})
		t.Run("rewrite/"+tc.label, func(t *testing.T) {
			cfgDir := filepath.Join(t.TempDir(), "cfg")
			lib := testLibrary(t)
			if code, out := loopbackInit(t, cfgDir, lib, "My Library"); code != 0 {
				t.Fatalf("the first init exited %d:\n%s", code, out)
			}
			before := readConfigFile(t, cfgDir)
			code, out := loopbackInit(t, cfgDir, lib, tc.name, "--force")
			defer logRunOnFailure(t, out)
			if code != 2 {
				t.Fatalf("the rewrite exited %d, want 2", code)
			}
			if after := readConfigFile(t, cfgDir); after != before {
				t.Errorf("the refused rewrite changed bridge.yaml:\n%s", after)
			}
		})
	}
	t.Run("256 two-byte runes", func(t *testing.T) {
		cfgDir := filepath.Join(t.TempDir(), "cfg")
		name := strings.Repeat("é", 256)
		code, out := loopbackInit(t, cfgDir, testLibrary(t), name)
		defer logRunOnFailure(t, out)
		if code != 0 {
			t.Fatalf("the init exited %d", code)
		}
		assertSavedLibraryName(t, cfgDir, name)
	})
}

// TestInitAsksAgainForANameAPairingCodeCannotCarry: at the prompt, where the
// library is already chosen and the preflight has run, a name the app would
// refuse is refused and asked for again rather than ending the run, and the
// next answer is the one saved.
func TestInitAsksAgainForANameAPairingCodeCannotCarry(t *testing.T) {
	if term.IsTerminal(int(os.Stdin.Fd())) {
		t.Skip("stdin is a terminal, where init would offer to start the bridge after the run")
	}
	cfgDir := filepath.Join(t.TempDir(), "cfg")
	answers := testLibrary(t) + "\n" + strings.Repeat("a", 257) + "\n" + "Jazz Archive\n"
	var stdout, stderr strings.Builder
	code := initCmd([]string{"--no-service", "--skip-doctor", "--dir", cfgDir},
		strings.NewReader(answers), &stdout, &stderr)
	out := stripANSI("--- stdout ---\n" + stdout.String() + "\n--- stderr ---\n" + stderr.String())
	defer logRunOnFailure(t, out)
	if code != 0 {
		t.Fatalf("the run exited %d", code)
	}
	if n := strings.Count(out, "Library display name ["); n != 2 {
		t.Errorf("the name was asked for %d times, want 2", n)
	}
	if !strings.Contains(stderr.String(), "256") {
		t.Errorf("the refusal does not say what the cap is")
	}
	assertSavedLibraryName(t, cfgDir, "Jazz Archive")
}

// TestInitKeepsTheNameAnInstallIsServedUnder: a rewrite that names no name
// keeps the install's, and the prompt offers it (#1041). A config written
// before the cap, or by hand, can hold a name over it, or one that is not
// UTF-8, and Load serves that repaired (config.RepairLibraryName): so that
// repaired name is the one kept, listed and offered, never the one in the
// file, which the prompt would then refuse when Enter took it.
func TestInitKeepsTheNameAnInstallIsServedUnder(t *testing.T) {
	if term.IsTerminal(int(os.Stdin.Fd())) {
		t.Skip("stdin is a terminal, where init would offer to start the bridge after the run")
	}
	for _, tc := range []struct{ label, line, served string }{
		{"over the cap", "libraryName: " + strings.Repeat("a", 300), strings.Repeat("a", 256)},
		{"not UTF-8", "libraryName: !!binary Q2Fm6SBUdW5lcw==", "Caf\uFFFD Tunes"},
	} {
		install := func(t *testing.T) (cfgDir, lib string) {
			t.Helper()
			cfgDir = filepath.Join(t.TempDir(), "cfg")
			lib = testLibrary(t)
			if code, out := loopbackInit(t, cfgDir, lib, "My Library"); code != 0 {
				t.Fatalf("the first init exited %d:\n%s", code, out)
			}
			setConfigKey(t, cfgDir, "libraryName", tc.line)
			if got := loadInstallConfig(t, cfgDir).LibraryName; got != tc.served {
				t.Fatalf("premise: Load serves the edited config as %q, want %q", got, tc.served)
			}
			return cfgDir, lib
		}
		t.Run(tc.label+"/--yes", func(t *testing.T) {
			cfgDir, lib := install(t)
			code, out := loopbackInit(t, cfgDir, lib, "", "--force")
			defer logRunOnFailure(t, out)
			if code != 0 {
				t.Fatalf("the rewrite exited %d", code)
			}
			assertSavedLibraryName(t, cfgDir, tc.served)
			if got, ok := keptValue(out, "libraryName"); !ok || got != tc.served {
				t.Errorf("the run lists libraryName as %q (listed %v), want %q", got, ok, tc.served)
			}
		})
		t.Run(tc.label+"/Enter", func(t *testing.T) {
			cfgDir, lib := install(t)
			var stdout, stderr strings.Builder
			code := initCmd([]string{"--no-service", "--skip-doctor", "--dir", cfgDir},
				strings.NewReader(lib+"\ny\n\n"), &stdout, &stderr)
			out := stripANSI("--- stdout ---\n" + stdout.String() + "\n--- stderr ---\n" + stderr.String())
			defer logRunOnFailure(t, out)
			if code != 0 {
				t.Fatalf("the run exited %d", code)
			}
			if want := "Library display name [" + tc.served + "]"; !strings.Contains(out, want) {
				t.Errorf("the prompt does not offer the name the install is served under, %q", tc.served)
			}
			assertSavedLibraryName(t, cfgDir, tc.served)
		})
	}
}
