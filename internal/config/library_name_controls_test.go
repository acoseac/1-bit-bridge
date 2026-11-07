package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// B744's item 2 at the load: RepairLibraryName repaired a name that is not
// UTF-8 or is past 256 runes and kept its control characters, so a hand edit,
// BRIDGE_LIBRARY_NAME or a prior installation's name served them in
// /v1/health, the Bonjour record and every pairing QR. What loads now loses
// the runes U+0000 to U+001F, U+007F and U+0080 to U+009F, as a typed name
// does, read as runes after the bytes that are not UTF-8 become U+FFFD, and
// is trimmed again; one that is nothing but controls is blank, served as
// DefaultLibraryName. It is said, and never a refusal to start.

// The controls, written as their UTF-8 bytes.
const (
	ctlNEL = "\xc2\x85" // U+0085 NEXT LINE, a C1 control
	ctlCSI = "\xc2\x9b" // U+009B, a C1 control
	ctlBEL = "\x07"     // U+0007, a C0 control
	ctlDEL = "\x7f"     // U+007F
)

var loadedControlNames = []struct{ label, name, served string }{
	{"a C1 control inside", "Living" + ctlNEL + "Room", "LivingRoom"},
	{"C0, DEL and C1 controls around it", ctlBEL + "Kitchen" + ctlCSI + ctlDEL, "Kitchen"},
	{"controls between spaces at its ends", " " + ctlNEL + " Den " + ctlBEL + " ", "Den"},
	{"runes whose second byte is a C1 number", "Ā Šoštarić", "Ā Šoštarić"},
	{"not UTF-8, with a control", "Caf\xe9" + ctlNEL + " Tunes", "Caf" + string(utf8.RuneError) + " Tunes"},
	{"nothing but controls", ctlBEL + ctlNEL + ctlDEL, DefaultLibraryName},
}

// TestALoadedLibraryNameLosesItsControlCharacters loads each name from
// BRIDGE_LIBRARY_NAME, and the ones YAML can carry from the file, and
// requires the name served without its controls, a warning saying it was
// repaired, and the save of what loaded loading the same name again.
func TestALoadedLibraryNameLosesItsControlCharacters(t *testing.T) {
	for _, tc := range loadedControlNames {
		t.Run(tc.label, func(t *testing.T) {
			if got := RepairLibraryName(tc.name); got != tc.served && !(tc.served == DefaultLibraryName && got == "") {
				t.Errorf("RepairLibraryName(%q) = %q, want %q", tc.name, got, tc.served)
			}

			rec := loggingtest.Record(t)
			dir := t.TempDir()
			lib := filepath.Join(dir, "lib")
			if err := os.MkdirAll(lib, 0o755); err != nil {
				t.Fatal(err)
			}
			cfgPath := filepath.Join(dir, "bridge.yaml")
			if err := os.WriteFile(cfgPath, []byte("libraryRoots:\n  - "+yamlStr(lib)+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("BRIDGE_LIBRARY_NAME", tc.name)
			cfg, err := Load(cfgPath)
			if err != nil {
				t.Fatalf("Load: %v; a library name never stops a bridge from starting", err)
			}
			if cfg.LibraryName != tc.served {
				t.Errorf("BRIDGE_LIBRARY_NAME %q is served as %q, want %q", tc.name, cfg.LibraryName, tc.served)
			}
			if tc.name != tc.served && len(rec.Failures()) == 0 {
				t.Errorf("a name served repaired was not said")
			}

			// What a save stores loads unchanged: the name served live is the
			// name served after a restart.
			os.Unsetenv("BRIDGE_LIBRARY_NAME")
			if err := saveAt(cfg, cfgPath); err != nil {
				t.Fatal(err)
			}
			again, err := Load(cfgPath)
			if err != nil {
				t.Fatal(err)
			}
			if again.LibraryName != cfg.LibraryName {
				t.Errorf("the saved name loads as %q, served live as %q", again.LibraryName, cfg.LibraryName)
			}
		})
	}
}

// TestALibraryNameInTheFileLosesItsControlCharacters: the same through the
// file, whose double-quoted YAML escapes write the controls.
func TestALibraryNameInTheFileLosesItsControlCharacters(t *testing.T) {
	for _, tc := range []struct{ yaml, served string }{
		{`"Living\x85Room"`, "LivingRoom"},
		{`"\aKitchen\x9b\x7f"`, "Kitchen"},
		{`" \N Den \a "`, "Den"},
		{`"\a\x85"`, DefaultLibraryName},
	} {
		t.Run(tc.yaml, func(t *testing.T) {
			dir := t.TempDir()
			lib := filepath.Join(dir, "lib")
			if err := os.MkdirAll(lib, 0o755); err != nil {
				t.Fatal(err)
			}
			cfgPath := filepath.Join(dir, "bridge.yaml")
			body := "libraryRoots:\n  - " + yamlStr(lib) + "\nlibraryName: " + tc.yaml + "\n"
			if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(cfgPath)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.LibraryName != tc.served {
				t.Errorf("libraryName %s is served as %q, want %q", tc.yaml, cfg.LibraryName, tc.served)
			}
			if strings.ContainsFunc(cfg.LibraryName, func(r rune) bool { return r < 0x20 || (r >= 0x7f && r <= 0x9f) }) {
				t.Errorf("the served name holds a control: %q", cfg.LibraryName)
			}
		})
	}
}
