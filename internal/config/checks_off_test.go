package config

import (
	"os"
	"reflect"
	"testing"

	"gopkg.in/yaml.v3"
)

// What an off of the update check's switch, decided while an install is on
// probation, hands a backup's settings check (backlog B761's round 3), and
// what a writer outside the live holder writes of the three opt-outs.

// settingsAsDecoded is raw decoded into a map, an empty one for a file that
// says nothing, the update block's checkEnabled taken out and an update
// block left with nothing taken out with it: what two files must share for
// their difference to be the key alone.
func settingsAsDecoded(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	m := map[string]any{}
	if err := yaml.Unmarshal(raw, &m); err != nil {
		t.Fatalf("the settings do not parse: %v", err)
	}
	if m == nil {
		m = map[string]any{}
	}
	if u, ok := m["update"].(map[string]any); ok {
		delete(u, "checkEnabled")
		if len(u) == 0 {
			delete(m, "update")
		}
	} else if v, there := m["update"]; there && v == nil {
		delete(m, "update")
	}
	return m
}

// TestWithUpdateChecksOffWritesTheKeyAndKeepsTheRest: the settings an off's
// decision hands a backup's settings check are the file's, every other key as
// it was, with update.checkEnabled written false, in an update block that
// holds other keys or none, over the key written either way, with no block,
// in an empty file and beside a comment and a flow block; and this version's
// own decoder takes what it takes from them, the key false. A file whose
// document or update block is no mapping is refused.
func TestWithUpdateChecksOffWritesTheKeyAndKeepsTheRest(t *testing.T) {
	for _, c := range []struct{ name, file string }{
		{"no update block", "libraryName: Hi-Fi\nscanIntervalSec: 60\n"},
		{"an update block with other keys", "libraryName: Hi-Fi\nupdate:\n  autoInstall: true\n  quietHours: \"02:00-05:00\"\n"},
		{"the key written true", "update:\n  checkEnabled: true\n  autoInstall: true\n"},
		{"the key written false", "update:\n  checkEnabled: false\n"},
		{"an update block that says nothing", "libraryName: Hi-Fi\nupdate:\n"},
		{"an empty file", ""},
		{"a comment and a flow block", "# the operator's note\nupdate: {autoInstall: true}\nlibraryName: Hi-Fi\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := WithUpdateChecksOff([]byte(c.file))
			if err != nil {
				t.Fatalf("WithUpdateChecksOff = %v, want the settings with the key", err)
			}
			if v, there := textLeaf(t, got, "update.checkEnabled"); !there || v != false {
				t.Errorf("update.checkEnabled in what the check is handed = %#v (there %v), want false", v, there)
			}
			if before, after := settingsAsDecoded(t, []byte(c.file)), settingsAsDecoded(t, got); !reflect.DeepEqual(before, after) {
				t.Errorf("the other settings changed:\nwas  %#v\nnow  %#v", before, after)
			}
			var cfg Config
			if DecodeFile([]byte(c.file), &cfg) == nil {
				var off Config
				if err := DecodeFile(got, &off); err != nil {
					t.Fatalf("this version's decoder refuses the settings with the key: %v", err)
				}
				if off.Update.CheckEnabled == nil || *off.Update.CheckEnabled {
					t.Errorf("this version decodes update.checkEnabled as %v, want false", off.Update.CheckEnabled)
				}
			}
		})
	}
	for _, bad := range []string{"- a list\n", "update: [1, 2]\n", "update: a word\n", "just a word\n", "update: *nowhere\n",
		"base: &b\n  autoInstall: true\nupdate: *b\n"} {
		if got, err := WithUpdateChecksOff([]byte(bad)); err == nil {
			t.Errorf("WithUpdateChecksOff(%q) = %q, nil; want a refusal", bad, got)
		}
	}
}

// TestACLIWritersSaveKeepsTheFilesOwnSwitches: a writer outside the live
// holder, config.Edit's (`bridge library add` and `remove`; `bridge output
// dac bind` takes the same step), loads the file with this process's BRIDGE_*
// variables over it and saves the three opt-outs as the file holds them,
// never the variables' values (KeepFiledSwitches): until B761's round 3 a
// variable set in the shell that runs the command wrote update.checkEnabled
// into a file that said nothing, true or false, either of which a release
// from before the switch refuses, and B617's two switches likewise.
func TestACLIWritersSaveKeepsTheFilesOwnSwitches(t *testing.T) {
	const enrichEnv, deezerEnv = "BRIDGE_ENRICH_ENABLED", "BRIDGE_ENRICH_DEEZER_ENABLED"
	for _, c := range []struct {
		name string
		file string
		env  map[string]string
		// want is each leaf's value the file is to hold after the save,
		// nil where it is to write none.
		want map[string]any
	}{
		{name: "the update check's variable on over a file that says nothing", env: map[string]string{updateCheckEnv: "true"},
			want: map[string]any{"update.checkEnabled": nil}},
		{name: "the update check's variable off over a file that says nothing", env: map[string]string{updateCheckEnv: "false"},
			want: map[string]any{"update.checkEnabled": nil}},
		{name: "the variable on over the file's false", file: "update:\n  checkEnabled: false\n",
			env: map[string]string{updateCheckEnv: "on"}, want: map[string]any{"update.checkEnabled": false}},
		{name: "the enrichment variables off over a file that says nothing",
			env:  map[string]string{enrichEnv: "false", deezerEnv: "off"},
			want: map[string]any{"enrich.enabled": nil, "enrich.deezerEnabled": nil}},
	} {
		t.Run(c.name, func(t *testing.T) {
			for _, name := range []string{updateCheckEnv, enrichEnv, deezerEnv} {
				t.Setenv(name, "")
				os.Unsetenv(name)
			}
			for name, v := range c.env {
				t.Setenv(name, v)
			}
			path := envSaveConfig(t, c.file)
			if err := Edit(path, func(cfg *Config) (bool, error) {
				cfg.LibraryName = "Edited"
				return true, nil
			}); err != nil {
				t.Fatalf("Edit = %v", err)
			}
			if got, there := fileLeaf(t, path, "libraryName"); !there || got != "Edited" {
				t.Fatalf("the edit was not saved: libraryName = %#v (there %v)", got, there)
			}
			for leaf, want := range c.want {
				got, there := fileLeaf(t, path, leaf)
				switch {
				case want == nil && there:
					t.Errorf("the save wrote %s = %#v from this process's environment into a file that wrote none", leaf, got)
				case want != nil && (!there || got != want):
					t.Errorf("the save wrote %s = %#v (there %v), want the file's own %#v", leaf, got, there, want)
				}
			}
		})
	}
}
