package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/term"
)

// B744's item 2: a library name could hold a control character. What the
// operator typed (init's --name, its prompt, the console's settings save) was
// refused only for bytes that are not UTF-8 and for a name past 256 runes, and
// what loads (bridge.yaml, BRIDGE_LIBRARY_NAME, a prior installation's name,
// which the prompt offers) was repaired for those alone. Every entry now
// removes the runes U+0000 to U+001F, U+007F and U+0080 to U+009F, read as
// runes after decoding and never as bytes, after a typed name's refusal of
// bytes that are not UTF-8, then trims again, before the entry's own blank and
// length policy.

// The controls, written as their UTF-8 bytes: an escape that names the rune
// would not survive every editor that touches this file.
const (
	ctlNEL = "\xc2\x85" // U+0085 NEXT LINE, a C1 control
	ctlCSI = "\xc2\x9b" // U+009B, a C1 control
	ctlPU1 = "\xc2\x91" // U+0091, the C1 control a test Mac's host name holds
	ctlBEL = "\x07"     // U+0007, a C0 control
	ctlDEL = "\x7f"     // U+007F
)

// controlNames is each shape of a name holding controls, beside the name every
// entry stores for it ("" for one blank once they are gone, which each entry
// answers by its own policy).
var controlNames = []struct{ label, name, stored string }{
	{"a C1 control inside", "Living" + ctlNEL + "Room", "LivingRoom"},
	{"C0, DEL and C1 controls around it", ctlBEL + ctlPU1 + "Kitchen" + ctlCSI + ctlDEL, "Kitchen"},
	{"controls between spaces at its ends", " " + ctlNEL + " Den " + ctlBEL + " ", "Den"},
	// The second bytes of Ā (C4 80) and š (C5 A1) are C1's numbers, and no
	// controls: a name is read as runes, never as bytes.
	{"runes whose second byte is a C1 number", "Ā Šoštarić", "Ā Šoštarić"},
	{"nothing but controls", ctlBEL + ctlNEL + ctlDEL, ""},
	// The cap counts the name stored: 260 runes typed, 250 once the controls
	// are gone. The save's Normalize strips controls too, so this row and
	// the one above are what the typed strip alone decides.
	{"controls past the cap", strings.Repeat("a", 250) + strings.Repeat(ctlBEL, 10), strings.Repeat("a", 250)},
}

// TestInitTypedNameLosesItsControlCharacters: `bridge init --name` stores a
// name without its control characters, the file and what Load serves alike; a
// name that is nothing but controls is no name, as a blank one is (the host's
// name is taken); and a name with bytes that are not UTF-8 is still refused
// before anything is written, whatever controls it holds besides, since the
// strip maps runes and would turn those bytes into U+FFFD.
func TestInitTypedNameLosesItsControlCharacters(t *testing.T) {
	for _, tc := range controlNames {
		t.Run(tc.label, func(t *testing.T) {
			reportInstallHostName(t, "musicbox")
			cfgDir := filepath.Join(t.TempDir(), "cfg")
			code, out := loopbackInit(t, cfgDir, testLibrary(t), tc.name)
			defer logRunOnFailure(t, out)
			if code != 0 {
				t.Fatalf("the init exited %d", code)
			}
			want := tc.stored
			if want == "" {
				want = "musicbox"
			}
			assertSavedLibraryName(t, cfgDir, want)
		})
	}
	t.Run("not UTF-8, with a control", func(t *testing.T) {
		cfgDir := filepath.Join(t.TempDir(), "cfg")
		code, out := loopbackInit(t, cfgDir, testLibrary(t), "Caf\xe9"+ctlNEL+" Tunes")
		defer logRunOnFailure(t, out)
		if code != 2 || !strings.Contains(out, "--name") || !strings.Contains(out, "UTF-8") {
			t.Fatalf("the init exited %d, want 2 and the --name refusal of bytes that are not UTF-8", code)
		}
		if _, err := os.Stat(cfgDir); !os.IsNotExist(err) {
			t.Errorf("the refused init made its config dir (stat: %v)", err)
		}
	})
}

// TestInitPromptedNameLosesItsControlCharacters: the name typed at init's
// prompt is stored without its control characters; one that is nothing but
// controls takes the offered name, as Enter does; and one with bytes that are
// not UTF-8 is refused and asked for again.
func TestInitPromptedNameLosesItsControlCharacters(t *testing.T) {
	if term.IsTerminal(int(os.Stdin.Fd())) {
		t.Skip("stdin is a terminal, where init would offer to start the bridge after the run")
	}
	for _, tc := range []struct {
		label, answers, saved string
		asked                 int
	}{
		{"a C1 control inside", "Living" + ctlNEL + "Room\n", "LivingRoom", 1},
		{"nothing but controls", ctlBEL + ctlNEL + "\n", "musicbox", 1},
		{"not UTF-8, then a name with controls", "Caf\xe9" + ctlNEL + "\n" + ctlPU1 + "Jazz Archive\n", "Jazz Archive", 2},
	} {
		t.Run(tc.label, func(t *testing.T) {
			reportInstallHostName(t, "musicbox")
			cfgDir := filepath.Join(t.TempDir(), "cfg")
			var stdout, stderr strings.Builder
			code := initCmd([]string{"--no-service", "--skip-doctor", "--dir", cfgDir},
				strings.NewReader(testLibrary(t)+"\n"+tc.answers), &stdout, &stderr)
			out := stripANSI("--- stdout ---\n" + stdout.String() + "\n--- stderr ---\n" + stderr.String())
			defer logRunOnFailure(t, out)
			if code != 0 {
				t.Fatalf("the run exited %d", code)
			}
			if n := strings.Count(out, "Library display name ["); n != tc.asked {
				t.Errorf("the name was asked for %d times, want %d", n, tc.asked)
			}
			assertSavedLibraryName(t, cfgDir, tc.saved)
		})
	}
}

// TestInitOffersAnInstallsNameWithoutItsControlCharacters: a prior install's
// name holding controls (a hand edit; YAML's double-quoted escapes write
// them) is served by Load without them, and that name is what a rewrite
// keeps and the prompt offers, so Enter stores it without them too.
func TestInitOffersAnInstallsNameWithoutItsControlCharacters(t *testing.T) {
	if term.IsTerminal(int(os.Stdin.Fd())) {
		t.Skip("stdin is a terminal, where init would offer to start the bridge after the run")
	}
	install := func(t *testing.T) (cfgDir, lib string) {
		t.Helper()
		cfgDir = filepath.Join(t.TempDir(), "cfg")
		lib = testLibrary(t)
		if code, out := loopbackInit(t, cfgDir, lib, "My Library"); code != 0 {
			t.Fatalf("the first init exited %d:\n%s", code, out)
		}
		setConfigKey(t, cfgDir, "libraryName", `libraryName: "\x07Living\x85Room\x9b"`)
		if got := loadInstallConfig(t, cfgDir).LibraryName; got != "LivingRoom" {
			t.Errorf("Load serves the edited config's name as %q, want %q", got, "LivingRoom")
		}
		return cfgDir, lib
	}
	t.Run("--yes", func(t *testing.T) {
		cfgDir, lib := install(t)
		code, out := loopbackInit(t, cfgDir, lib, "", "--force")
		defer logRunOnFailure(t, out)
		if code != 0 {
			t.Fatalf("the rewrite exited %d", code)
		}
		assertSavedLibraryName(t, cfgDir, "LivingRoom")
	})
	t.Run("Enter", func(t *testing.T) {
		cfgDir, lib := install(t)
		var stdout, stderr strings.Builder
		code := initCmd([]string{"--no-service", "--skip-doctor", "--dir", cfgDir},
			strings.NewReader(lib+"\ny\n\n"), &stdout, &stderr)
		out := stripANSI("--- stdout ---\n" + stdout.String() + "\n--- stderr ---\n" + stderr.String())
		defer logRunOnFailure(t, out)
		if code != 0 {
			t.Fatalf("the run exited %d", code)
		}
		if !strings.Contains(out, "Library display name [LivingRoom]") {
			t.Errorf("the prompt does not offer the install's name without its controls")
		}
		assertSavedLibraryName(t, cfgDir, "LivingRoom")
	})
}
