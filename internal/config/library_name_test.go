package config

import "testing"

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
