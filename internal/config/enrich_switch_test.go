package config

import (
	"os"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// The spellings the two opt-outs' variables take, as the file takes the keys
// (yaml.v3 reads y, yes, on, n, no and off into a bool, beside YAML 1.2's
// true and false), without regard to case, and strconv.ParseBool's.
var (
	switchEnvOn  = []string{"true", "TRUE", "True", "tRuE", "t", "T", "1", "y", "Y", "yes", "YES", "Yes", "yEs", "on", "On", "ON", "oN", " on "}
	switchEnvOff = []string{"false", "FALSE", "False", "fAlSe", "f", "F", "0", "n", "N", "no", "NO", "No", "nO", "off", "Off", "OFF", "oFf", "\toff\n"}
	// switchEnvUnread are values that read as neither: each is off.
	switchEnvUnread = []string{"maybe", "disabled-please", "nein", "enable", "-1", "2"}
)

// TestTheOffSwitchLoads: enrich.enabled and enrich.deezerEnabled are
// settings, so a config that writes either loads and reads as it says, and
// each is set from the environment by the name the struct derives for it,
// through the real Load (backlog B617). On main a config that wrote either
// did not load ("that key is not a field of type config.EnrichConfig"), and
// the environment's names set nothing: enrichment had no off switch.
//
// Each variable reads as the file reads its key, without regard to case:
// every spelling of on and off above, over a file that says the other. An
// empty value, or one of white space alone, says nothing, so the file's
// value stands. Any other value is off, with one Warn naming the variable
// and not the value: an opt-out read as on by mistake sends what the operator
// asked to stop, which `off` and `no` did when the variables were read as
// every other boolean is, by strconv.ParseBool, a refusal ignored.
func TestTheOffSwitchLoads(t *testing.T) {
	type switchCase struct {
		name           string
		yaml           string
		env            map[string]string
		master, deezer bool
		unread         string // the variable whose value reads as neither, if one
	}
	cases := []switchCase{
		{name: "enrich.enabled: false", yaml: "enrich:\n  enabled: false\n", deezer: true},
		{name: "enrich.deezerEnabled: false", yaml: "enrich:\n  deezerEnabled: false\n", master: true},
		{name: "both written true", yaml: "enrich:\n  enabled: true\n  deezerEnabled: true\n", master: true, deezer: true},
		{name: "the file's off and no", yaml: "enrich:\n  enabled: off\n  deezerEnabled: No\n"},
		{name: "the file's ON and yes", yaml: "enrich:\n  enabled: ON\n  deezerEnabled: yes\n", master: true, deezer: true},
		{name: "BRIDGE_ENRICH_ENABLED=false", env: map[string]string{"BRIDGE_ENRICH_ENABLED": "false"}, deezer: true},
		{name: "BRIDGE_ENRICH_DEEZER_ENABLED=false", env: map[string]string{"BRIDGE_ENRICH_DEEZER_ENABLED": "false"}, master: true},
		{name: "the environment over the file", yaml: "enrich:\n  enabled: false\n  deezerEnabled: false\n",
			env:    map[string]string{"BRIDGE_ENRICH_ENABLED": "true", "BRIDGE_ENRICH_DEEZER_ENABLED": "true"},
			master: true, deezer: true},
	}
	for _, v := range []struct{ name, key string }{
		{"BRIDGE_ENRICH_ENABLED", "enabled"},
		{"BRIDGE_ENRICH_DEEZER_ENABLED", "deezerEnabled"},
	} {
		// set gives the case the variable's switch at on and the other at
		// on, the default.
		set := func(c switchCase, on bool) switchCase {
			c.master, c.deezer = true, true
			if v.key == "enabled" {
				c.master = on
			} else {
				c.deezer = on
			}
			return c
		}
		for _, val := range switchEnvOn {
			cases = append(cases, set(switchCase{name: v.name + "=" + val + " over the file's false",
				yaml: "enrich:\n  " + v.key + ": false\n", env: map[string]string{v.name: val}}, true))
		}
		for _, val := range switchEnvOff {
			cases = append(cases, set(switchCase{name: v.name + "=" + val, env: map[string]string{v.name: val}}, false))
		}
		for _, val := range []string{"", "   "} {
			cases = append(cases,
				set(switchCase{name: v.name + "=" + val + " says nothing over the file's false",
					yaml: "enrich:\n  " + v.key + ": false\n", env: map[string]string{v.name: val}}, false),
				set(switchCase{name: v.name + "=" + val + " says nothing over no key",
					env: map[string]string{v.name: val}}, true))
		}
		for _, val := range switchEnvUnread {
			cases = append(cases, set(switchCase{name: v.name + "=" + val + " over the file's true",
				yaml: "enrich:\n  " + v.key + ": true\n", env: map[string]string{v.name: val}, unread: v.name}, false))
		}
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			clearSwitchEnv(t)
			for k, v := range c.env {
				t.Setenv(k, v)
			}
			rec := loggingtest.Record(t)
			cfg, err := Load(writeEnrichTestConfig(t, c.yaml))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got := cfg.Enrich.EffectiveEnabled(); got != c.master {
				t.Errorf("enrichment on = %v, want %v", got, c.master)
			}
			if got := cfg.Enrich.EffectiveDeezerEnabled(); got != c.deezer {
				t.Errorf("Deezer on = %v, want %v", got, c.deezer)
			}
			lines := rec.Lines(unreadableSwitchEnv)
			switch {
			case c.unread == "" && len(lines) != 0:
				t.Errorf("a value that reads warned:\n%s", strings.Join(lines, "\n"))
			case c.unread != "" && (len(lines) != 1 || !strings.Contains(lines[0], "var="+c.unread)):
				t.Errorf("an unreadable %s warned %d time(s), want once naming it:\n%s", c.unread, len(lines), strings.Join(lines, "\n"))
			}
			if c.unread != "" {
				if got := rec.Carrying(c.env[c.unread]); len(got) != 0 {
					t.Errorf("a line carries the unreadable value:\n%s", strings.Join(got, "\n"))
				}
			}
		})
	}
}

// clearSwitchEnv removes both opt-outs' variables for the test, so a value
// a developer's shell carries cannot decide a case; Setenv first restores
// whatever was there when the test ends.
func clearSwitchEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"BRIDGE_ENRICH_ENABLED", "BRIDGE_ENRICH_DEEZER_ENABLED"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
}

// TestEnrichEnabledFromEnvReadsAsLoadDoes: a command that reads the switch
// with no config file to load (bridge fingerprint) takes the variable alone,
// read as Load reads it: set where it says on or off, off where it says
// neither, and unset where it is empty or absent.
func TestEnrichEnabledFromEnvReadsAsLoadDoes(t *testing.T) {
	for _, c := range []struct {
		val      string
		absent   bool
		on, set  bool
		unreadOK bool
	}{
		{absent: true},
		{val: ""},
		{val: "  "},
		{val: "On", on: true, set: true},
		{val: "OFF", set: true},
		{val: "no", set: true},
		{val: "maybe", set: true, unreadOK: true},
	} {
		t.Run(c.val, func(t *testing.T) {
			clearSwitchEnv(t)
			if !c.absent {
				t.Setenv("BRIDGE_ENRICH_ENABLED", c.val)
			}
			rec := loggingtest.Record(t)
			on, set := EnrichEnabledFromEnv()
			if on != c.on || set != c.set {
				t.Errorf("EnrichEnabledFromEnv() = %v, %v; want %v, %v", on, set, c.on, c.set)
			}
			if got := len(rec.Lines(unreadableSwitchEnv)); (got == 1) != c.unreadOK || got > 1 {
				t.Errorf("warned %d time(s)", got)
			}
		})
	}
}
