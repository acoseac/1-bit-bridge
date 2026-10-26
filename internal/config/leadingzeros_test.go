package config

import (
	"bytes"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// leadingZeroSentence pins the public diagnostic independently of its implementation.
const leadingZeroSentence = "a whole number is written with a leading zero, which YAML may read as octal; write it in decimal without the leading zero, or with 0o if you mean octal"

// wantYAMLError compares the entire diagnostic, including order and multiplicity.
func wantYAMLError(t *testing.T, err error, want string) {
	t.Helper()
	got := "<nil>"
	if err != nil {
		got = err.Error()
	}
	if got != want {
		t.Fatalf("got %q; want %q", got, want)
	}
}

// TestLeadingZeroSettingsRefused drives both the raw decode and Load for real integer kinds.
func TestLeadingZeroSettingsRefused(t *testing.T) {
	for _, path := range []string{"scanIntervalSec", "output.loopbackPort", "upload.maxFileBytes"} {
		for _, value := range []string{"010", "0777", "+010", "0_10", "00", "08", "07792", "010.0", "08e0", "!!int 010", "!!float 08", "!!float 036"} {
			t.Run(path+"/"+value, func(t *testing.T) {
				file, line := configSetting(t, path, value)
				raw, err := os.ReadFile(file)
				if err != nil {
					t.Fatal(err)
				}
				// Establish that yaml.v3 accepts the fixture before the shadow check.
				var plain Config
				dec := yaml.NewDecoder(bytes.NewReader(raw))
				dec.KnownFields(true)
				if err := dec.Decode(&plain); err != nil {
					t.Fatal(err)
				}
				want := fmt.Sprintf("yaml: unmarshal errors:\n  line %d: %s", line, leadingZeroSentence)
				t.Run("DecodeFile", func(t *testing.T) { wantYAMLError(t, DecodeFile(raw, &Config{}), want) })
				t.Run("Load", func(t *testing.T) {
					_, err := Load(file)
					wantYAMLError(t, err, fmt.Sprintf("parse config %q: %s", file, want))
				})
			})
		}
	}
	// The rate-limit pointer accepts negative numbers, unlike a port.
	t.Run("negative", func(t *testing.T) {
		file, line := configSetting(t, "limits.manifest.requestsPerMinute", "-010")
		_, err := Load(file)
		wantYAMLError(t, err, fmt.Sprintf("parse config %q: yaml: unmarshal errors:\n  line %d: %s", file, line, leadingZeroSentence))
	})
}

// TestLeadingZeroDecoderReach pins aliases, merges and the decoder's error ordering.
func TestLeadingZeroDecoderReach(t *testing.T) {
	zero := func(line int) string { return fmt.Sprintf("line %d: %s", line, leadingZeroSentence) }
	fraction := func(line int) string { return fmt.Sprintf("line %d: cannot unmarshal a !!float value into int", line) }
	for _, tc := range []struct {
		name, raw string
		lines     []string
	}{
		{"alias", "libraryName: &p 010\nscanIntervalSec: *p\n", []string{zero(1)}},
		{"merge", "<<: {scanIntervalSec: 010}\n", []string{zero(1)}},
		{"shared anchor", "scanIntervalSec: &p 010\nbackup: {keep: *p}\n", []string{zero(1), zero(1)}},
		{"mixed", "# settings\nscanIntervalSec: 010\nbackup: {keep: 30.5}\nupdate: {checkIntervalHours: 00.5}\noutput: {loopbackPort: 08}\n", []string{zero(2), fraction(3), fraction(4), zero(5)}},
		{"decoder order", "<<: {scanIntervalSec: 010}\nbackup: {keep: 08}\n", []string{zero(2), zero(1)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wantYAMLError(t, DecodeFile([]byte(tc.raw), &Config{}), "yaml: unmarshal errors:\n  "+strings.Join(tc.lines, "\n  "))
		})
	}
	t.Run("fraction anchor multiplicity", func(t *testing.T) {
		wantYAMLError(t, DecodeFile([]byte("scanIntervalSec: &p 30.5\nbackup: {keep: *p}\n"), &Config{}), "yaml: unmarshal errors:\n  "+fraction(1)+"\n  "+fraction(1))
	})
	t.Run("overridden merge", func(t *testing.T) {
		cfg, err := Load(configBody(t, "<<: {scanIntervalSec: 010}\nscanIntervalSec: 8080\n"))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.ScanIntervalSec != 8080 {
			t.Fatalf("got %d, want 8080", cfg.ScanIntervalSec)
		}
	})
}

// TestLeadingZeroBoundary leaves first-decode and existing fraction refusals unchanged.
func TestLeadingZeroBoundary(t *testing.T) {
	for _, raw := range []string{
		"unknown: true\nscanIntervalSec: 010\n", "scanIntervalSec: \"010\"\n", "scanIntervalSec: !!int 08\n",
		"scanIntervalSec: 099999999999999999999\n", "scanIntervalSec: !!bool 010\n",
	} {
		t.Run(raw, func(t *testing.T) {
			dec := yaml.NewDecoder(strings.NewReader(raw))
			dec.KnownFields(true)
			before := dec.Decode(&Config{})
			if before == nil {
				t.Fatal("fixture must fail its first decode")
			}
			wantYAMLError(t, DecodeFile([]byte(raw), &Config{}), before.Error())
		})
	}
	wantYAMLError(t, DecodeFile([]byte("scanIntervalSec: 00.5\n"), &Config{}), "yaml: unmarshal errors:\n  line 1: cannot unmarshal a !!float value into int")
}

// TestLeadingZeroAcceptedSpellings checks values through a signed, wide real setting.
func TestLeadingZeroAcceptedSpellings(t *testing.T) {
	for _, tc := range []struct {
		text  string
		value int
	}{
		{"0", 0}, {"-0", 0}, {"+0", 0}, {"0.0", 0}, {"0e5", 0}, {"30.0", 30}, {"3e1", 30}, {"8e03", 8000},
		{"1_000", 1000}, {"8008", 8008}, {"0o10", 8}, {"0x10", 16}, {"0b101", 5}, {"-0x10", -16},
	} {
		t.Run(tc.text, func(t *testing.T) {
			file, _ := configSetting(t, "limits.manifest.requestsPerMinute", tc.text)
			cfg, err := Load(file)
			if err != nil {
				t.Fatal(err)
			}
			if got := *cfg.Limits.Manifest.RequestsPerMinute; got != tc.value {
				t.Fatalf("got %d, want %d", got, tc.value)
			}
		})
	}
}

// TestLeadingZeroShadowKinds checks every synthetic width, inline fields and map values.
func TestLeadingZeroShadowKinds(t *testing.T) {
	for _, typ := range []reflect.Type{reflect.TypeFor[int](), reflect.TypeFor[int8](), reflect.TypeFor[int16](), reflect.TypeFor[int32](), reflect.TypeFor[int64](), reflect.TypeFor[uint](), reflect.TypeFor[uint8](), reflect.TypeFor[uint16](), reflect.TypeFor[uint32](), reflect.TypeFor[uint64](), reflect.TypeFor[uintptr]()} {
		t.Run(typ.String(), func(t *testing.T) {
			into := reflect.New(reflect.StructOf([]reflect.StructField{{Name: "N", Type: typ, Tag: `yaml:"n"`}})).Interface()
			var n yaml.Node
			if err := yaml.Unmarshal([]byte("n: 010\n"), &n); err != nil {
				t.Fatal(err)
			}
			if err := n.Decode(into); err != nil {
				t.Fatal(err)
			}
			wantYAMLError(t, CheckWholeNumbers(&n, into), "yaml: unmarshal errors:\n  line 1: "+leadingZeroSentence)
		})
	}
	t.Run("inline", func(t *testing.T) {
		var into struct {
			Inner struct {
				N int `yaml:"n"`
			} `yaml:",inline"`
		}
		var n yaml.Node
		if err := yaml.Unmarshal([]byte("n: 010\n"), &n); err != nil {
			t.Fatal(err)
		}
		if err := n.Decode(&into); err != nil {
			t.Fatal(err)
		}
		wantYAMLError(t, CheckWholeNumbers(&n, &into), "yaml: unmarshal errors:\n  line 1: "+leadingZeroSentence)
	})
	t.Run("map value", func(t *testing.T) {
		var into map[string]int
		var n yaml.Node
		if err := yaml.Unmarshal([]byte("count: 010\n"), &n); err != nil {
			t.Fatal(err)
		}
		if err := n.Decode(&into); err != nil {
			t.Fatal(err)
		}
		wantYAMLError(t, CheckWholeNumbers(&n, &into), "yaml: unmarshal errors:\n  line 1: "+leadingZeroSentence)
	})

	t.Run("map keys untouched", func(t *testing.T) {
		var into map[int]int
		var n yaml.Node
		if err := yaml.Unmarshal([]byte("010: 8\n"), &n); err != nil {
			t.Fatal(err)
		}
		if err := n.Decode(&into); err != nil {
			t.Fatal(err)
		}
		if err := CheckWholeNumbers(&n, &into); err != nil {
			t.Fatal(err)
		}
		if into[8] != 8 {
			t.Fatalf("got %v, want map[8:8]", into)
		}
	})
}

// TestLeadingZeroValueFreeDiagnostic keeps only the exact fixed sentence.
func TestLeadingZeroValueFreeDiagnostic(t *testing.T) {
	for i, lines := range [][]string{
		{"line 2: " + leadingZeroSentence},
		{"line 2: " + leadingZeroSentence, "line 3: cannot unmarshal a !!float value into int"},
	} {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			var err error = &yaml.TypeError{Errors: lines}
			want := err.Error()
			for range 3 {
				err = ValueFreeYAMLError(err)
				wantYAMLError(t, err, want)
			}
		})
	}
	wantYAMLError(t, ValueFreeYAMLError(&yaml.TypeError{Errors: []string{"line 2: " + leadingZeroSentence + " SECRET"}}), "yaml: unmarshal errors:\n  line 2: a value does not read")
}
