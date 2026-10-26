package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// configBody writes a loopback bridge.yaml of body after the two lines a
// config needs to load (its library root and data dir), and returns its
// path.
func configBody(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	file := filepath.Join(dir, "bridge.yaml")
	head := "libraryRoots: [" + yamlStr(dir) + "]\ndataDir: " + yamlStr(filepath.Join(dir, "data")) + "\n"
	if err := os.WriteFile(file, []byte(head+body), 0o600); err != nil {
		t.Fatal(err)
	}
	return file
}

// configSetting writes a config that sets path, a YAML path, to value as
// written, a key a line, and returns its path and the line value is on. An
// empty value leaves the setting out.
func configSetting(t *testing.T, path, value string) (file string, line int) {
	t.Helper()
	if value == "" {
		return configBody(t, ""), 0
	}
	var body strings.Builder
	keys := strings.Split(path, ".")
	for i, k := range keys {
		body.WriteString(strings.Repeat("  ", i) + k + ":")
		if i < len(keys)-1 {
			body.WriteString("\n")
		}
	}
	body.WriteString(" " + value + "\n")
	return configBody(t, body.String()), 2 + len(keys)
}

// integerSetting reports whether b is a whole-number setting, and the Go
// type yaml.v3 names for it.
func integerSetting(b envBinding) (goType string, ok bool) {
	kind := b.kind
	if kind == reflect.Pointer {
		kind = b.elemKind
	}
	switch kind {
	case reflect.Int:
		return "int", true
	case reflect.Int64:
		return "int64", true
	}
	return "", false
}

// settingIn is b's setting in c as Load left it, "nil" for a pointer it did
// not set.
func settingIn(c *Config, b envBinding) string {
	v := reflect.ValueOf(c).Elem()
	for _, i := range b.index {
		v = v.Field(i)
	}
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return "nil"
		}
		v = v.Elem()
	}
	return strconv.FormatInt(v.Int(), 10)
}

// carriesTheValue reports whether out holds value, or four bytes of it in a
// row.
func carriesTheValue(out, value string) bool {
	return strings.Contains(out, value) || carriesPartOf(out, value)
}

// TestAWholeNumberSettingTakesNoFraction: every whole-number setting of the
// config, envBindings' integer leaves outside output.player (whose reader
// refuses for itself: TestAWholeNumberOfThePlayerTakesNoFraction), refuses
// the load when written as a fraction, rather than load its whole part
// (backlog B528). yaml.v3 decodes a float into an int by dropping what
// follows the point, and measured before this change, 42 of the 44 such
// settings loaded 2.75 as 2, and the other two refused it only because 2 is
// outside what they take. The refusal is the one a word on that line gets,
// made value free (ValueFreeYAMLError): the line, the !!float tag and the Go
// type, and nothing of the value. Written as an integer the setting takes,
// or as a float whose value is that integer, it loads as written; null, it
// is what it is left out, its default.
func TestAWholeNumberSettingTakesNoFraction(t *testing.T) {
	var tested int
	for _, b := range envBindings() {
		goType, ok := integerSetting(b)
		if !ok || strings.HasPrefix(b.YAMLPath, "output.player.") {
			continue
		}
		tested++
		t.Run(b.YAMLPath, func(t *testing.T) {
			loads := func(t *testing.T, value string) *Config {
				t.Helper()
				file, _ := configSetting(t, b.YAMLPath, value)
				cfg, err := Load(file)
				if err != nil {
					t.Fatalf("Load = %v", err)
				}
				return cfg
			}
			absent := settingIn(loads(t, ""), b)
			if got := settingIn(loads(t, "~"), b); got != absent {
				t.Errorf("null loads as %s, and left out as %s", got, absent)
			}
			// An integer the setting takes: most take any, and the rest one
			// of these.
			var value string
			for _, try := range []string{"7", "250", "7800"} {
				file, _ := configSetting(t, b.YAMLPath, try)
				if cfg, err := Load(file); err == nil && settingIn(cfg, b) == try {
					value = try
					break
				}
			}
			if value == "" {
				t.Fatalf("no integer this test tries loads as written: give it one %s takes", b.YAMLPath)
			}
			if got := settingIn(loads(t, value+".0"), b); got != value {
				t.Errorf("%s.0 loads as %s, want %s", value, got, value)
			}

			fraction := value + ".4375"
			file, line := configSetting(t, b.YAMLPath, fraction)
			cfg, err := Load(file)
			want := fmt.Sprintf("line %d: cannot unmarshal a !!float value into %s", line, goType)
			if err == nil {
				t.Fatalf("a fraction loaded, as %s; want a refusal saying %q", settingIn(cfg, b), want)
			}
			if !strings.Contains(err.Error(), want) {
				t.Errorf("Load = %v, want a refusal saying %q", err, want)
			}
			if carriesTheValue(refusalText(err, file), fraction) {
				t.Errorf("the refusal carries the value: %v", err)
			}
		})
	}
	if tested == 0 {
		t.Fatal("envBindings names no whole-number setting")
	}
}

// TestAWholeNumberSettingTakesWhatDecodesUnchanged: what decides is the
// value written, with leading-zero spellings refused separately (B533).
// A float yaml.v3 decodes unchanged into the setting is taken (6.0, 6e1), and one it would change is refused with the refusal yaml.v3 gives
// a float outside its range, made value free: on
// limits.manifest.requestsPerMinute, where 0 and a negative number turn the
// manifest limit off, 0.5 loaded as 0 and -.inf as the smallest int64, each
// a limit nobody wrote turned off. What is weighed is the value the text
// writes, exactly, never the float64 strconv.ParseFloat rounds it to: that
// made 6.0000000000000001 a 6, 1e-400 a 0, which turned the limit off too,
// and 9007199254740993.0 a 9007199254740992. A fraction is refused wherever
// the decoder takes it from, through a merge key or an alias, and every one
// in the file is named, at its line.
func TestAWholeNumberSettingTakesWhatDecodesUnchanged(t *testing.T) {
	const path = "limits.manifest.requestsPerMinute"
	for _, tc := range []struct {
		name, value string
		// rpm is the limit as Load reads it, for a value it takes.
		rpm     int
		refused bool
	}{
		{"an integer", "6", 6, false},
		{"a whole float", "6.0", 6, false},
		{"an exponent", "6e1", 60, false},
		{"a negative zero", "-0.0", 0, false},
		{"a float tag on a whole number", "!!float 6", 6, false},
		{"a float tag on a binary spelling with a sign after its prefix, which yaml.v3 still reads", "!!float 0b+11110", 30, false},
		{"a whole number with a million zeros after its point", "30." + strings.Repeat("0", 1_000_001), 30, false},
		{"zero with an exponent past an int64", "0e99999999999999999999", 0, false},
		{"hexadecimal", "0x1F", 31, false},
		{"null", "~", DefaultManifestRequestsPerMinute, false},
		{"left out", "", DefaultManifestRequestsPerMinute, false},
		{"a fraction that loaded as 0", "0.5", 0, true},
		{"a negative fraction", "-0.5", 0, true},
		{"a fraction past a whole number", "6.000001", 0, true},
		{"a fraction ParseFloat rounds to a whole number", "6.0000000000000001", 0, true},
		{"a fraction ParseFloat rounds to zero", "1e-400", 0, true},
		{"a fraction with a ten-digit exponent", "1e-1000000000", 0, true},
		{"a float tag on an integer past what a float64 holds", "!!float 0x20000000000001", 0, true},
		{"a number whose exponent strconv stops reading, which yaml.v3 decodes as 30", "0." + strings.Repeat("0", 9998) + "3e10000000", 0, true},
		{"a whole number ParseFloat rounds to another", "9007199254740993.0", 0, true},
		{"a whole number a float64 holds exactly", "9007199254740992.0", 9007199254740992, false},
		{"minus infinity", "-.inf", 0, true},
		{"infinity", ".inf", 0, true},
		{"not a number", ".nan", 0, true},
		{"two to the 63rd", "9223372036854775808.0", 0, true},
		{"past an int64", "1e19", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file, line := configSetting(t, path, tc.value)
			cfg, err := Load(file)
			if !tc.refused {
				if err != nil {
					t.Fatalf("Load = %v", err)
				}
				if got := cfg.Limits.Manifest.EffectiveRPM(); got != tc.rpm {
					t.Errorf("the limit loads as %d, want %d", got, tc.rpm)
				}
				return
			}
			want := fmt.Sprintf("line %d: cannot unmarshal a !!float value into int", line)
			if err == nil {
				t.Fatalf("Load took it, as a limit of %d; want a refusal saying %q", cfg.Limits.Manifest.EffectiveRPM(), want)
			}
			if !strings.Contains(err.Error(), want) {
				t.Errorf("Load = %v, want a refusal saying %q", err, want)
			}
			// The tag is the refusal's own word; the scalar's text is the value.
			if carriesTheValue(refusalText(err, file), strings.TrimPrefix(tc.value, "!!float ")) {
				t.Errorf("the refusal carries the value: %v", err)
			}
		})
	}

	for _, tc := range []struct {
		name, body string
		// said is each line the refusal must hold, and times how often.
		said  []string
		times int
	}{
		{"through a merge key", "<<: {scanIntervalSec: 900.5}\n",
			[]string{"line 3: cannot unmarshal a !!float value into int"}, 1},
		{"through an alias, at the anchor's line, for each setting it reaches", "update: {checkIntervalHours: &f 12.5}\nscanIntervalSec: *f\n",
			[]string{"line 3: cannot unmarshal a !!float value into int"}, 2},
		{"every fraction in the file", "scanIntervalSec: 900.5\nbackup:\n  keep: 2.5\nupload:\n  maxFileBytes: 1024.5\n",
			[]string{"line 3: cannot unmarshal a !!float value into int", "line 5: cannot unmarshal a !!float value into int",
				"line 7: cannot unmarshal a !!float value into int64"}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := configBody(t, tc.body)
			cfg, err := Load(file)
			if err == nil {
				t.Fatalf("Load took it (scanIntervalSec %d); want a refusal", cfg.ScanIntervalSec)
			}
			for _, s := range tc.said {
				if n := strings.Count(err.Error(), s); n != tc.times {
					t.Errorf("the refusal says %q %d times, want %d: %v", s, n, tc.times, err)
				}
			}
			for _, value := range []string{"900.5", "12.5", "2.5", "1024.5"} {
				if strings.Contains(refusalText(err, file), value) {
					t.Errorf("the refusal carries %s: %v", value, err)
				}
			}
		})
	}
}

// TestWrittenIntegerReadsTheTextAsYAMLDoes: the value a !!float's text writes
// is read as yaml.v3's resolve reads it, an integer spelling through
// strconv.ParseInt with base 0 and anything else as a decimal float, exactly
// and in time linear in the text; a fraction, a whole number past 20 digits,
// a nonzero number with an exponent of a million or more, and a text that
// reads as no number write no whole number.
func TestWrittenIntegerReadsTheTextAsYAMLDoes(t *testing.T) {
	for _, tc := range []struct {
		text  string
		value string // "" when no whole number is written
	}{
		{"30", "30"}, {"+30", "30"}, {"-30", "-30"}, {"30.", "30"}, {"30.000", "30"}, {"3e1", "30"},
		{"3E+1", "30"}, {"300e-1", "30"}, {".3e2", "30"}, {"0030.0", "30"}, {"3_0.0", "30"},
		{"036", "30"}, {"0o36", "30"}, {"0x1E", "30"}, {"0b11110", "30"}, {"-0x1E", "-30"}, {"08", "8"},
		{"0b+11110", "30"}, {"0b-11110", "-30"}, {"0o+36", "30"}, {"0o-36", "-30"}, {"-0b11110", "-30"}, {"-0o36", "-30"},
		{"0b+", ""}, {"0x+1E", ""}, {"-0b+11110", ""}, {"0b+2", ""},
		{"-0.0", "0"}, {"0e99999999999999999999", "0"}, {"0.0e-99999999999999999999", "0"},
		{"18446744073709551615", "18446744073709551615"}, {"1.8446744073709551615e19", "18446744073709551615"},
		{"9007199254740993.0", "9007199254740993"}, {"1" + strings.Repeat("0", 19), "1" + strings.Repeat("0", 19)},
		{"30." + strings.Repeat("0", 1_000_001), "30"}, {"0." + strings.Repeat("0", 1_000_000) + "3e1000002", ""},
		{"30.5", ""}, {"0.5", ""}, {"1e-400", ""}, {"1e-1000000000", ""}, {"3e999999", ""},
		{"1" + strings.Repeat("0", 20), ""}, {"123456789012345678901", ""},
		{"", ""}, {".", ""}, {"e5", ""}, {"1e", ""}, {"1e+", ""}, {"--1", ""}, {"+-1", ""}, {"1.2.3", ""},
		{"1e5e5", ""}, {"0x1p4", ""}, {"1e5.0", ""}, {" 30", ""}, {".inf", ""}, {".nan", ""},
	} {
		v, ok := writtenInteger(tc.text)
		switch {
		case tc.value == "" && ok:
			t.Errorf("writtenInteger(%.40q) = %v, want no whole number", tc.text, v)
		case tc.value != "" && !ok:
			t.Errorf("writtenInteger(%.40q) wrote no whole number, want %s", tc.text, tc.value)
		case ok && v.String() != tc.value:
			t.Errorf("writtenInteger(%.40q) = %v, want %s", tc.text, v, tc.value)
		}
	}
}

// TestTheShadowNeedsNoEmbeddedField: CheckWholeNumbers' shadow (buildShadow)
// rebuilds each struct of Config field by field with reflect.StructOf and
// does not carry a field's Anonymous flag, which StructOf does not support for
// every embedded type. yaml.v3 inlines a struct by its ",inline" tag, which
// the shadow keeps, not by Anonymous; Anonymous only changes how yaml.v3
// treats an unexported embedded type, which the shadow would skip. So the
// shadow decodes as Config does only while Config embeds nothing: this fails
// the day a struct Config reaches gains an embedded field, before the shadow
// can silently map a value to another setting (Gemini on #1339).
func TestTheShadowNeedsNoEmbeddedField(t *testing.T) {
	seen := map[reflect.Type]bool{}
	var walk func(reflect.Type, string)
	walk = func(rt reflect.Type, path string) {
		switch rt.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Array, reflect.Map:
			walk(rt.Elem(), path)
			return
		case reflect.Struct:
		default:
			return
		}
		if seen[rt] {
			return
		}
		seen[rt] = true
		for i := range rt.NumField() {
			f := rt.Field(i)
			if f.Anonymous {
				t.Errorf("%s embeds %s: give it a name, or make buildShadow handle embedded fields before CheckWholeNumbers relies on them", path, f.Type)
			}
			walk(f.Type, path+"."+f.Name)
		}
	}
	walk(reflect.TypeOf(Config{}), "Config")
	if len(seen) < 2 {
		t.Fatalf("the walk reached %d struct types; it is not reading Config", len(seen))
	}
}
