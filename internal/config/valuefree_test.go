package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// The values pasted onto a line in the value-free tests: letters and digits,
// as a password or a DAC's serial is, and a serial of digits, which a
// whole-number field reads as a number.
const (
	pastedWord   = "Zq8xVk2pRw4T"
	pastedDigits = "202311050044"
)

// carriesPartOf reports whether out holds any four bytes of secret in a row.
func carriesPartOf(out, secret string) bool {
	for i := 0; i+4 <= len(secret); i++ {
		if strings.Contains(out, secret[i:i+4]) {
			return true
		}
	}
	return false
}

// refusalText is Load's refusal of the config at path without that path,
// which Load names and which is the test's, not the config's: a temporary
// directory's name holds random digits, which a serial of digits shares by
// chance.
func refusalText(err error, path string) string {
	return strings.ReplaceAll(err.Error(), path, "")
}

// configWith writes a loopback bridge.yaml with the given settings, each a
// YAML path and its value, beside what a config needs to load, and returns
// its path.
func configWith(t *testing.T, settings map[string]any) string {
	t.Helper()
	dir := t.TempDir()
	doc := map[string]any{
		"libraryRoots": []string{dir},
		"dataDir":      filepath.Join(dir, "data"),
	}
	for path, value := range settings {
		m := doc
		keys := strings.Split(path, ".")
		for _, k := range keys[:len(keys)-1] {
			next, ok := m[k].(map[string]any)
			if !ok {
				next = map[string]any{}
				m[k] = next
			}
			m = next
		}
		m[keys[len(keys)-1]] = value
	}
	raw, err := yaml.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "bridge.yaml")
	if err := os.WriteFile(file, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return file
}

// The settings Validate checks only beside another: what each needs set for
// its check to run.
var valueFreeConditions = map[string]map[string]any{
	"dlna.listenAddress":                    {"dlna.enabled": true},
	"dlna.discovery.msearchIntervalSeconds": {"dlna.discovery.enabled": true},
	"dlna.discovery.rendererTTLSeconds":     {"dlna.discovery.enabled": true},
	"upnpUpstream.msearchIntervalSeconds":   {"upnpUpstream.enabled": true},
	"upnpUpstream.serverTTLSeconds":         {"upnpUpstream.enabled": true},
}

// A public install's settings, in which Validate checks adminAddress as an
// address of any interface, and with publicAutocert the port listenAddress
// names.
var (
	publicInstall = map[string]any{
		"deployment.mode": "public", "autocert.domain": "bridge.example",
		"deployment.adminTLSTerminatedByProxy": true, "adminAddress": "127.0.0.1:7789",
	}
	publicAutocert = map[string]any{
		"deployment.mode": "public", "autocert.domain": "bridge.example",
		"autocert.enabled": true, "autocert.email": "ops@bridge.example", "adminAddress": "127.0.0.1:7789",
	}
)

// TestNoLoadRefusalOrWarningCarriesAValue: whatever is pasted onto whichever
// line of bridge.yaml, MPD's password or a DAC's serial on the wrong line,
// Load's refusal and every line Load logs carry no part of it (B505's round
// 3: yaml.v3's own type error quoted a value of ten bytes or fewer whole and
// the first seven of a longer one, and Validate's refusals printed numbers,
// addresses and words as they were written). Every setting the config has,
// through envBindings: a word on each line, and a serial of digits on each
// line that takes text or a whole number; and in public mode the settings
// public mode checks, and each setting Validate checks only beside another
// with that one set (valueFreeConditions).
func TestNoLoadRefusalOrWarningCarriesAValue(t *testing.T) {
	digits, err := strconv.ParseInt(pastedDigits, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	type pasted struct {
		path   string
		value  any
		secret string
		with   map[string]any
		// refusedBy, when set, is what the refusal must name: the case is
		// there for a check that quoted the value, and must reach it.
		refusedBy string
	}
	var cases []pasted
	for _, b := range envBindings() {
		var values []any
		switch {
		case b.kind == reflect.Slice:
			values = []any{[]string{pastedWord}}
		case b.kind == reflect.String:
			values = []any{pastedWord, digits}
		case b.kind == reflect.Int || b.kind == reflect.Int64 ||
			(b.kind == reflect.Pointer && (b.elemKind == reflect.Int || b.elemKind == reflect.Int64)):
			values = []any{pastedWord, digits}
		default:
			values = []any{pastedWord}
		}
		for _, v := range values {
			secret := pastedWord
			if _, ok := v.(int64); ok {
				secret = pastedDigits
			}
			cases = append(cases, pasted{b.YAMLPath, v, secret, nil, ""})
			// With the setting its check needs, for each value that reaches
			// the check: a word on a whole-number line is yaml.v3's to
			// refuse, with or without it.
			_, isDigits := v.(int64)
			if with, ok := valueFreeConditions[b.YAMLPath]; ok && (b.kind == reflect.String || isDigits) {
				cases = append(cases, pasted{b.YAMLPath, v, secret, with, b.YAMLPath})
			}
			if b.YAMLPath == "adminAddress" || b.YAMLPath == "listenAddress" {
				cases = append(cases, pasted{b.YAMLPath, v, secret, publicInstall, b.YAMLPath})
			}
		}
	}
	upstream := map[string]any{"upnpUpstream.enabled": true}
	cases = append(cases,
		// A host the secret, before a port: an address that parses, refused
		// for its host (a loopback console's) or its port (autocert's 443).
		pasted{"adminAddress", pastedWord + ":7789", pastedWord, nil, "adminAddress: its host must be a loopback address"},
		pasted{"listenAddress", pastedWord + ":7788", pastedWord, publicAutocert, "autocert.enabled requires listenAddress on port :443"},
		// An upstream server's name is a label, and its refusals quoted it.
		pasted{"upnpUpstream.servers", []map[string]any{{"name": pastedWord}}, pastedWord, upstream,
			"upnpUpstream.servers[0]: either udn or manualDescriptionURL is required"},
		pasted{"upnpUpstream.servers", []map[string]any{
			{"name": pastedWord, "udn": "uuid:1"}, {"name": pastedWord, "udn": "uuid:2"},
		}, pastedWord, upstream, "upnpUpstream.servers[1]: its path prefix"},
	)

	for _, c := range cases {
		kind := "word"
		if c.secret == pastedDigits {
			kind = "digits"
		}
		if c.with != nil {
			kind += "+conditions"
		}
		// The subtest is named for the kind of value, never the value:
		// t.TempDir's path carries the name, and Load names the path.
		t.Run(c.path+"="+kind, func(t *testing.T) {
			forgetPlayerOffReason(t)
			rec := loggingtest.Record(t)
			settings := map[string]any{c.path: c.value}
			for k, v := range c.with {
				if k != c.path {
					settings[k] = v
				}
			}
			file := configWith(t, settings)
			_, err := Load(file)
			if c.refusedBy != "" && (err == nil || !strings.Contains(err.Error(), c.refusedBy)) {
				t.Errorf("Load = %v, want a refusal naming %q", err, c.refusedBy)
			}
			if err != nil && carriesPartOf(refusalText(err, file), c.secret) {
				t.Errorf("Load's refusal carries the value: %v", err)
			}
			for _, l := range rec.All() {
				if carriesPartOf(l, c.secret) {
					t.Errorf("a line Load logged carries the value: %s", l)
				}
			}
		})
	}
}

// TestNoEnvironmentOverrideWarningCarriesAValue: a BRIDGE_* variable whose
// value does not read as its setting is ignored, and the warning names the
// variable alone: the value, and strconv's error, which repeats it, stay out.
func TestNoEnvironmentOverrideWarningCarriesAValue(t *testing.T) {
	for _, b := range envBindings() {
		if b.kind == reflect.String || b.kind == reflect.Slice {
			continue
		}
		t.Run(b.EnvVar, func(t *testing.T) {
			t.Setenv(b.EnvVar, pastedWord)
			forgetPlayerOffReason(t)
			rec := loggingtest.Record(t)
			_, _ = Load(configWith(t, map[string]any{"libraryName": "Kitchen"}))
			for _, l := range rec.All() {
				if carriesPartOf(l, pastedWord) {
					t.Errorf("a line Load logged carries the value: %s", l)
				}
			}
			want := "ignoring unparseable integer env var"
			if b.kind == reflect.Bool || b.elemKind == reflect.Bool {
				want = "ignoring unparseable boolean env var"
			}
			var named bool
			for _, l := range rec.Lines(want) {
				named = named || strings.Contains(l, b.EnvVar)
			}
			if !named {
				t.Errorf("no warning names %s", b.EnvVar)
			}
		})
	}
}
