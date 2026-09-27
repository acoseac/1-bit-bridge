package config

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// TestLoadServesATrimmedLibraryNameAndNeverABlankOne: the name a bridge is
// served under reaches /v1/health (to a caller with no token too), its
// Bonjour record and the name= of every pairing QR, and the app's pairing
// parser refuses a code whose name is empty ("missing the name field") or
// carries leading or trailing whitespace ("extra spaces in the name field").
//
// Load served DefaultLibraryName only for a name that was exactly "", from
// applyDefaults. A config written by `bridge init --name "  "` was served as
// "  ", and one written by `--name " Padded "` with its spaces, both measured
// on 2026-09-27 with the real binary (main at a2a72f1b), and so was a
// BRIDGE_LIBRARY_NAME of spaces, which the overrides apply AFTER
// applyDefaults. Normalize trims the name and gives a blank one the default,
// after the overrides, and on every other path that saves a config.
//
// The parser also refuses a name over 256 Characters ("Pairing code's name
// field is too long.") and one whose bytes are not UTF-8, whose field
// Foundation then reads as absent ("missing the name field"). Both were
// served as written, measured with the real binary on 2026-09-27 (main at
// 3214aa17): a 257-character name through the console, and `bridge init
// --name $'Caf\xe9 Tunes'`, which is "Café Tunes" typed in a Latin-1
// terminal and which init saved as the `!!binary` row below. Load must not
// refuse either, which would stop a bridge from starting over a display
// name, so Normalize repairs them: U+FFFD for the bytes that are not UTF-8,
// and a name cut to MaxLibraryNameLength runes.
func TestLoadServesATrimmedLibraryNameAndNeverABlankOne(t *testing.T) {
	for _, tc := range []struct {
		name     string
		nameLine string // the config's libraryName line; empty writes none
		env      string // BRIDGE_LIBRARY_NAME; empty leaves it unset
		want     string
	}{
		{name: "no name", want: DefaultLibraryName},
		{name: "an empty one", nameLine: `libraryName: ""`, want: DefaultLibraryName},
		{name: "spaces alone", nameLine: `libraryName: "  "`, want: DefaultLibraryName},
		{name: "a tab and a newline", nameLine: `libraryName: "\t\n"`, want: DefaultLibraryName},
		{name: "a padded one", nameLine: `libraryName: " Padded Name "`, want: "Padded Name"},
		// U+200B, which the app's trim removes and strings.TrimSpace does
		// not (TrimLibraryName).
		{name: "a zero-width space alone", nameLine: `libraryName: "\u200b"`, want: DefaultLibraryName},
		{name: "one padded with zero-width spaces", nameLine: `libraryName: "\u200bZW\u200b"`, want: "ZW"},
		{name: "a plain one", nameLine: `libraryName: Plain`, want: "Plain"},
		{name: "an override of spaces alone", env: "   ", want: DefaultLibraryName},
		{name: "a padded override", nameLine: `libraryName: Plain`, env: " Env Name ", want: "Env Name"},
		{name: "one at the cap", nameLine: "libraryName: " + strings.Repeat("a", 256), want: strings.Repeat("a", 256)},
		{name: "one over the cap", nameLine: "libraryName: " + strings.Repeat("a", 300), want: strings.Repeat("a", 256)},
		// The cut leaves a trailing space, which the app would refuse.
		{name: "one whose cut ends on a space", nameLine: `libraryName: "` + strings.Repeat("a", 255) + ` bbb"`, want: strings.Repeat("a", 255)},
		// Two bytes a rune: the cap counts runes, as the app counts
		// Characters, never bytes.
		{name: "an override over the cap", env: strings.Repeat("é", 300), want: strings.Repeat("é", 256)},
		// As main's `bridge init --name $'Caf\xe9 Tunes'` saved it.
		{name: "one that is not UTF-8", nameLine: "libraryName: !!binary Q2Fm6SBUdW5lcw==", want: "Caf\uFFFD Tunes"},
		{name: "an override that is not UTF-8", env: "Caf\xe9", want: "Caf\uFFFD"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.env != "" {
				t.Setenv("BRIDGE_LIBRARY_NAME", tc.env)
			}
			path, _ := writeConfig(t, "libraryRoots:\n  - {{LIBRARY_ROOT}}\n"+tc.nameLine+"\n")
			cfg, err := Load(path)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.LibraryName != tc.want {
				t.Errorf("LibraryName = %q, want %q", cfg.LibraryName, tc.want)
			}
		})
	}
}

// TestNormalizeGivesABlankLibraryNameTheDefault: the same rule on a config
// built in memory, which is what every writer that is not Load hands to
// NormalizeAndValidate before it saves (the console's settings PATCH, init,
// the serve auto-init), so none of them can store a name Load would serve
// differently. Idempotent, as Normalize's doc requires.
func TestNormalizeGivesABlankLibraryNameTheDefault(t *testing.T) {
	for sent, want := range map[string]string{
		"":              DefaultLibraryName,
		" \t ":          DefaultLibraryName,
		"  Home Hi-Fi ": "Home Hi-Fi",
		"Home Hi-Fi":    "Home Hi-Fi",
		"\u200b":        DefaultLibraryName,
		"Hi\u200bFi":    "Hi\u200bFi", // inside a name, the app keeps it too
	} {
		cfg := &Config{
			LibraryRoots:    []string{t.TempDir()},
			ListenAddress:   ":7788",
			AdminAddress:    "127.0.0.1:7789",
			ScanIntervalSec: 3600,
			LibraryName:     sent,
		}
		for pass := 1; pass <= 2; pass++ {
			if err := cfg.NormalizeAndValidate(); err != nil {
				t.Fatalf("%q, pass %d: %v", sent, pass, err)
			}
			if cfg.LibraryName != want {
				t.Errorf("%q, pass %d: LibraryName = %q, want %q", sent, pass, cfg.LibraryName, want)
			}
		}
		if TrimLibraryName(cfg.LibraryName) != cfg.LibraryName || cfg.LibraryName == "" {
			t.Errorf("%q normalizes to %q, which the app's pairing parser refuses", sent, cfg.LibraryName)
		}
	}
}

// TestNormalizeGivesEveryLibraryNameOneAPairingCodeCarries: whatever a config
// or the environment holds, Normalize leaves a name the app's pairing parser
// takes (CheckLibraryName), trimmed as its trim leaves it, and does it in one
// pass (Normalize's doc requires it to be idempotent). A cut can land after a
// space, or inside a run of them, which a second trim removes; the rune
// count, never the byte count, is what is capped.
func TestNormalizeGivesEveryLibraryNameOneAPairingCodeCarries(t *testing.T) {
	a255 := strings.Repeat("a", 255)
	for sent, want := range map[string]string{
		strings.Repeat("a", 256):             strings.Repeat("a", 256),
		strings.Repeat("a", 257):             strings.Repeat("a", 256),
		" " + strings.Repeat("a", 256) + " ": strings.Repeat("a", 256),
		a255 + " b":                          a255,
		a255 + "\u200b\u200bb":               a255,
		strings.Repeat("🎧", 300):             strings.Repeat("🎧", 256),
		"Caf\xe9 Tunes":                      "Caf\uFFFD Tunes",
		"\xff":                               "\uFFFD",
		" \xff\xfe ":                         "\uFFFD",
		"Café Tunes":                         "Café Tunes",
	} {
		cfg := &Config{
			LibraryRoots:    []string{t.TempDir()},
			ListenAddress:   ":7788",
			AdminAddress:    "127.0.0.1:7789",
			ScanIntervalSec: 3600,
			LibraryName:     sent,
		}
		for pass := 1; pass <= 2; pass++ {
			if err := cfg.NormalizeAndValidate(); err != nil {
				t.Fatalf("%q, pass %d: %v", sent, pass, err)
			}
			if cfg.LibraryName != want {
				t.Errorf("%q, pass %d: LibraryName = %q, want %q", sent, pass, cfg.LibraryName, want)
			}
		}
		got := cfg.LibraryName
		if got == "" || TrimLibraryName(got) != got || CheckLibraryName(got) != nil {
			t.Errorf("%q normalizes to %q, which the app's pairing parser refuses", sent, got)
		}
	}
}

// TestCheckLibraryNameRefusesWhatAPairingCodeCannotCarry: the settings PATCH
// and `bridge init` refuse a name the operator typed rather than repair it,
// and this is the rule they refuse by. It counts runes, as the app counts
// Characters: a name of 256 two-byte or four-byte runes pairs, one of 257
// does not, and the refusal names the cap.
func TestCheckLibraryNameRefusesWhatAPairingCodeCannotCarry(t *testing.T) {
	for _, ok := range []string{
		"x", "My Library", strings.Repeat("a", 256), strings.Repeat("é", 256), strings.Repeat("🎧", 256),
	} {
		if err := CheckLibraryName(ok); err != nil {
			t.Errorf("CheckLibraryName(%d runes, %d bytes) = %v, want nil", utf8.RuneCountInString(ok), len(ok), err)
		}
	}
	for _, bad := range []string{strings.Repeat("a", 257), strings.Repeat("é", 257), "Caf\xe9"} {
		err := CheckLibraryName(bad)
		if err == nil {
			t.Errorf("CheckLibraryName(%q) = nil, want a refusal", bad)
			continue
		}
		if utf8.ValidString(bad) && !strings.Contains(err.Error(), "256") {
			t.Errorf("the refusal %q does not name the cap", err)
		}
	}
}

// TestNormalizeSaysWhenItRepairsALibraryName: a repair Normalize makes where
// Load finds the name is said, once for the Load, and names the name it
// serves in its place. Trimming is not a repair and stays silent, as it
// always was.
func TestNormalizeSaysWhenItRepairsALibraryName(t *testing.T) {
	for _, tc := range []struct {
		sent  string
		warns bool
	}{
		{strings.Repeat("a", 300), true},
		{"Caf\xe9 Tunes", true},
		{"  Padded  ", false},
		{"Plain", false},
	} {
		rec := loggingtest.Record(t)
		cfg := &Config{
			LibraryRoots:    []string{t.TempDir()},
			ListenAddress:   ":7788",
			AdminAddress:    "127.0.0.1:7789",
			ScanIntervalSec: 3600,
			LibraryName:     tc.sent,
		}
		if err := cfg.NormalizeAndValidate(); err != nil {
			t.Fatalf("%q: %v", tc.sent, err)
		}
		lines := rec.Failures()
		if !tc.warns {
			if len(lines) != 0 {
				t.Errorf("%q: Normalize warned %q, want nothing", tc.sent, lines)
			}
			continue
		}
		if len(lines) != 1 || !strings.Contains(lines[0], "libraryName") || !strings.Contains(lines[0], cfg.LibraryName) {
			t.Errorf("%q: Normalize warned %q, want one line naming libraryName and the %q it serves", tc.sent, lines, cfg.LibraryName)
		}
	}
}
