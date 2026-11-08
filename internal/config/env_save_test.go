package config

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// A console save and the BRIDGE_* variables (backlog B761). Load applies a
// variable over the file, the live holder publishes what Load gave it, and a
// console save (Update, UpdateChecked) writes the holder's snapshot. For each
// leaf a variable set that the save left at its live value, the save writes
// the file's own value, read from the bytes its turn found, so a variable
// never lands in the file, and a start without it finds what the file said;
// a save that changes such a leaf writes and publishes the change, which the
// variable sets again at the next start. These tests drive the holder
// through serve's own boot read (LoadFileAt, RecordFile).

// envSaveConfig writes a config holding body beside a library of its own,
// and returns its path.
func envSaveConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	lib := filepath.Join(dir, "Music")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "bridge.yaml")
	text := "libraryRoots:\n  - " + yamlStr(lib) + "\ndataDir: " + yamlStr(filepath.Join(dir, "data")) + "\n" + body
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// bootHolder loads the config at path as serve's boot read does and hands it
// to a live holder told the bytes and the file that read read (RecordFile).
func bootHolder(t *testing.T, path string) *RuntimeConfig {
	t.Helper()
	cfg, raw, file, err := LoadFileAt(path)
	if err != nil {
		t.Fatalf("the config does not load: %v", err)
	}
	rc := NewRuntimeConfig(cfg)
	rc.RecordFile(file, raw)
	return rc
}

// fileLeaf is the value the config at path writes at the yaml path leaf, as
// yaml.v3 decodes it into an any, and whether the file writes it at all.
func fileLeaf(t *testing.T, path, leaf string) (any, bool) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return textLeaf(t, raw, leaf)
}

// textLeaf is fileLeaf for raw, a config's text.
func textLeaf(t *testing.T, raw []byte, leaf string) (any, bool) {
	t.Helper()
	var v any
	if err := yaml.Unmarshal(raw, &v); err != nil {
		t.Fatalf("the saved config does not parse: %v", err)
	}
	for _, key := range strings.Split(leaf, ".") {
		m, ok := v.(map[string]any)
		if !ok {
			return nil, false
		}
		if v, ok = m[key]; !ok {
			return nil, false
		}
	}
	return v, true
}

// loadAt is the config at path as a start loads it.
func loadAt(t *testing.T, path string) *Config {
	t.Helper()
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("the saved config does not load: %v", err)
	}
	return cfg
}

// withoutVariables runs fn with each variable vars names unset, and sets
// them back as they were for the rest of the test.
func withoutVariables(t *testing.T, vars map[string]string, fn func()) {
	t.Helper()
	for name := range vars {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
	fn()
	for name, v := range vars {
		t.Setenv(name, v)
	}
}

// envSaveCase is one kind of leaf a variable sets: the variables, set at the
// boot; what the file writes; the leaf, as the file holds it after a save
// (filedThere false for no key) and as a config gives it (value); and what
// the variable makes it and what the file alone does.
type envSaveCase struct {
	name       string
	vars       map[string]string
	file       string
	leaf       string
	filed      any
	filedThere bool
	value      func(c *Config) any
	varValue   any
	ownValue   any
}

// envSaveCases cover the kinds a binding takes: an integer, a pointer to an
// integer the file does not write, a pointer to a boolean, a list, a string,
// and a legacy alias.
var envSaveCases = []envSaveCase{
	{
		name: "an integer", vars: map[string]string{"BRIDGE_SCAN_INTERVAL_SEC": "120"},
		file: "scanIntervalSec: 600\n", leaf: "scanIntervalSec", filed: 600, filedThere: true,
		value: func(c *Config) any { return c.ScanIntervalSec }, varValue: 120, ownValue: 600,
	},
	{
		name: "a pointer to an integer the file does not write", vars: map[string]string{"BRIDGE_BACKUP_INTERVAL_HOURS": "12"},
		file: "", leaf: "backup.intervalHours", filed: nil, filedThere: false,
		value: func(c *Config) any {
			if c.Backup.IntervalHours == nil {
				return nil
			}
			return *c.Backup.IntervalHours
		},
		varValue: 12, ownValue: nil,
	},
	{
		name: "a pointer to a boolean", vars: map[string]string{"BRIDGE_SMART_PLAYLISTS_ENABLED": "true"},
		file: "smartPlaylists:\n  enabled: false\n", leaf: "smartPlaylists.enabled", filed: false, filedThere: true,
		value: func(c *Config) any {
			if c.SmartPlaylists.Enabled == nil {
				return nil
			}
			return *c.SmartPlaylists.Enabled
		},
		varValue: true, ownValue: false,
	},
	{
		name: "a list", vars: map[string]string{"BRIDGE_METRICS_ALLOW_CIDRS": "10.0.0.0/8,172.16.0.0/12"},
		file: "metrics:\n  allowCidrs:\n    - 192.168.0.0/16\n", leaf: "metrics.allowCidrs",
		filed: []any{"192.168.0.0/16"}, filedThere: true,
		value:    func(c *Config) any { return c.Metrics.AllowCIDRs },
		varValue: []string{"10.0.0.0/8", "172.16.0.0/12"}, ownValue: []string{"192.168.0.0/16"},
	},
	{
		name: "a string", vars: map[string]string{"BRIDGE_LIBRARY_NAME": "Env Name"},
		file: "libraryName: File Name\n", leaf: "libraryName", filed: "File Name", filedThere: true,
		value: func(c *Config) any { return c.LibraryName }, varValue: "Env Name", ownValue: "File Name",
	},
	{
		name: "a legacy alias", vars: map[string]string{"BRIDGE_MUSICBRAINZ_BASE_URL": "https://env.example/ws/2"},
		file: "enrich:\n  musicbrainzBaseURL: https://file.example/ws/2\n", leaf: "enrich.musicbrainzBaseURL",
		filed: "https://file.example/ws/2", filedThere: true,
		value:    func(c *Config) any { return c.Enrich.MusicBrainzBaseURL },
		varValue: "https://env.example/ws/2", ownValue: "https://file.example/ws/2",
	},
}

// requireFiled fails unless the config at path holds leaf as c's file
// holds it after a save that did not change it.
func (c envSaveCase) requireFiled(t *testing.T, path, when string) {
	t.Helper()
	got, there := fileLeaf(t, path, c.leaf)
	if there != c.filedThere || !reflect.DeepEqual(got, c.filed) {
		t.Errorf("%s: the file holds %s = %#v (there %v), want the file's own %#v (there %v)",
			when, c.leaf, got, there, c.filed, c.filedThere)
	}
}

// TestASaveKeepsTheFilesOwnValueForEachVariable: a save of another setting,
// twice, writes the file's own value for the leaf a variable set, and the
// holder goes on publishing the variable's; a start with the variable reads
// the variable's, and one without it the file's own.
func TestASaveKeepsTheFilesOwnValueForEachVariable(t *testing.T) {
	for _, c := range envSaveCases {
		t.Run(c.name, func(t *testing.T) {
			for name, v := range c.vars {
				t.Setenv(name, v)
			}
			path := envSaveConfig(t, c.file)
			rc := bootHolder(t, path)
			if got := c.value(rc.Load()); !reflect.DeepEqual(got, c.varValue) {
				t.Fatalf("booted with %v, the live %s is %#v, want the variable's %#v", c.vars, c.leaf, got, c.varValue)
			}
			for i := 1; i <= 2; i++ {
				if err := rc.Update(path, func(next *Config) error {
					next.Library.AllowDelete = !next.Library.AllowDelete
					return nil
				}); err != nil {
					t.Fatalf("save %d: %v", i, err)
				}
				c.requireFiled(t, path, "a save of another setting")
				if got := c.value(rc.Load()); !reflect.DeepEqual(got, c.varValue) {
					t.Errorf("save %d: the holder publishes %s = %#v, want the variable's %#v", i, c.leaf, got, c.varValue)
				}
			}
			if got := c.value(loadAt(t, path)); !reflect.DeepEqual(got, c.varValue) {
				t.Errorf("a start with %v reads %s = %#v, want the variable's %#v", c.vars, c.leaf, got, c.varValue)
			}
			withoutVariables(t, c.vars, func() {
				if got := c.value(loadAt(t, path)); !reflect.DeepEqual(got, c.ownValue) {
					t.Errorf("a start without %v reads %s = %#v, want the file's own %#v", c.vars, c.leaf, got, c.ownValue)
				}
			})
		})
	}
}

// TestASaveThatChangesAVariablesSettingWritesAndPublishesTheChange: a save
// that changes a leaf a variable set writes the change and the holder
// publishes it; the variable sets the leaf again at the next start, and a
// start without it reads the change.
func TestASaveThatChangesAVariablesSettingWritesAndPublishesTheChange(t *testing.T) {
	for _, c := range []struct {
		envSaveCase
		change func(next *Config)
		// changed is the change as a config gives it, and changeFiled as
		// the file holds it.
		changed, changeFiled any
	}{
		{envSaveCases[0], func(next *Config) { next.ScanIntervalSec = 300 }, 300, 300},
		{envSaveCases[1], func(next *Config) { six := 6; next.Backup.IntervalHours = &six }, 6, 6},
		{envSaveCases[3], func(next *Config) { next.Metrics.AllowCIDRs = []string{"10.1.0.0/16"} },
			[]string{"10.1.0.0/16"}, []any{"10.1.0.0/16"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			for name, v := range c.vars {
				t.Setenv(name, v)
			}
			path := envSaveConfig(t, c.file)
			rc := bootHolder(t, path)
			if err := rc.Update(path, func(next *Config) error {
				c.change(next)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if got, there := fileLeaf(t, path, c.leaf); !there || !reflect.DeepEqual(got, c.changeFiled) {
				t.Errorf("the file holds %s = %#v (there %v), want the change %#v", c.leaf, got, there, c.changeFiled)
			}
			if got := c.value(rc.Load()); !reflect.DeepEqual(got, c.changed) {
				t.Errorf("the holder publishes %s = %#v, want the change %#v", c.leaf, got, c.changed)
			}
			if got := c.value(loadAt(t, path)); !reflect.DeepEqual(got, c.varValue) {
				t.Errorf("a start with %v reads %s = %#v, want the variable's %#v", c.vars, c.leaf, got, c.varValue)
			}
			withoutVariables(t, c.vars, func() {
				if got := c.value(loadAt(t, path)); !reflect.DeepEqual(got, c.changed) {
					t.Errorf("a start without %v reads %s = %#v, want the change %#v", c.vars, c.leaf, got, c.changed)
				}
			})
		})
	}
}

// TestASaveOverAFileThatDoesNotDecodeWritesTheLiveValue: a holder told
// nothing of the file saves over whatever it holds, and where that does not
// decode as Load decodes it (a key no config has), the leaf a variable set
// is written as the update holds it, its live value. Green before B761 and
// after: what changed is the file that decodes.
func TestASaveOverAFileThatDoesNotDecodeWritesTheLiveValue(t *testing.T) {
	t.Setenv("BRIDGE_SCAN_INTERVAL_SEC", "120")
	path := envSaveConfig(t, "scanIntervalSec: 600\n")
	rc := NewRuntimeConfig(loadAt(t, path))
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("scanIntervalSecs: 5\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err := rc.Update(path, func(next *Config) error {
		next.Library.AllowDelete = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got, there := fileLeaf(t, path, "scanIntervalSec"); !there || got != 120 {
		t.Errorf("the file holds scanIntervalSec = %#v (there %v), want the live 120 written as it is", got, there)
	}
}

// TestTheOptOutsVariablesAreStillWrittenAsTheirSwitchesAre is a regression
// guard, green before B761 and after: BRIDGE_ENRICH_ENABLED's switch is
// written as B617 writes it, whatever the other variables' leaves now do: a
// save of another setting writes the file's own (no key), a change to on
// writes no key, and a change to off writes false.
func TestTheOptOutsVariablesAreStillWrittenAsTheirSwitchesAre(t *testing.T) {
	t.Setenv("BRIDGE_ENRICH_ENABLED", "false")
	path := envSaveConfig(t, "")
	rc := bootHolder(t, path)
	if rc.Load().Enrich.EffectiveEnabled() {
		t.Fatal("booted with BRIDGE_ENRICH_ENABLED=false, enrichment is on")
	}
	for _, step := range []struct {
		name   string
		change func(next *Config)
		filed  any
		there  bool
		live   bool
	}{
		{"a save of another setting", func(next *Config) { next.Library.AllowDelete = true }, nil, false, false},
		{"the switch changed on", func(next *Config) { on := true; next.Enrich.Enabled = &on }, nil, false, true},
		{"the switch changed off", func(next *Config) { off := false; next.Enrich.Enabled = &off }, false, true, false},
	} {
		if err := rc.Update(path, func(next *Config) error {
			step.change(next)
			return nil
		}); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		if got, there := fileLeaf(t, path, "enrich.enabled"); there != step.there || got != step.filed {
			t.Errorf("%s: the file holds enrich.enabled = %#v (there %v), want %#v (there %v)",
				step.name, got, there, step.filed, step.there)
		}
		if got := rc.Load().Enrich.EffectiveEnabled(); got != step.live {
			t.Errorf("%s: the holder publishes enrichment %v, want %v", step.name, got, step.live)
		}
	}
}

// envSaveFile writes text as a config in a directory of its own, beside a
// library, {lib} and {dir} in it standing for the library's path and the
// config's directory, and returns the config's path and its directory.
func envSaveFile(t *testing.T, text string) (path, dir string) {
	t.Helper()
	dir = t.TempDir()
	lib := filepath.Join(dir, "Music")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	text = strings.NewReplacer("{lib}", yamlStr(lib), "{dir}", yamlStr(dir)).Replace(text)
	path = filepath.Join(dir, "bridge.yaml")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path, dir
}

// TestASaveWritesNoKeyTheFileDoesNotWrite: a leaf a variable set that the
// file does not write is written as no key by a save of another setting,
// whatever its field's tag (backlog B761's second round). The five leaves
// whose field writes its zero value as a key (no omitempty) are the ones an
// empty or zero value would put there, a key the file never had: each is
// left out of the file, through serve's boot read and two saves, and the
// next start without the variable reads the file's own value, the default.
func TestASaveWritesNoKeyTheFileDoesNotWrite(t *testing.T) {
	const roots = "libraryRoots:\n  - {lib}\n"
	for _, c := range []struct {
		name, leaf, file string
		vars             func(dir string) map[string]string
		// value is the leaf as a config gives it, and own what the next start
		// without the variable reads, the file's own value, its default.
		value func(c *Config) any
		own   func(dir string) any
	}{
		{
			name: "scanIntervalSec", leaf: "scanIntervalSec", file: roots,
			vars:  func(string) map[string]string { return map[string]string{"BRIDGE_SCAN_INTERVAL_SEC": "120"} },
			value: func(c *Config) any { return c.ScanIntervalSec },
			own:   func(string) any { return DefaultScanIntervalSec },
		},
		{
			name: "listenAddress", leaf: "listenAddress", file: roots,
			vars:  func(string) map[string]string { return map[string]string{"BRIDGE_LISTEN_ADDRESS": "127.0.0.1:9911"} },
			value: func(c *Config) any { return c.ListenAddress },
			own:   func(string) any { return DefaultListenAddress },
		},
		{
			name: "dataDir", leaf: "dataDir", file: roots,
			vars: func(dir string) map[string]string {
				return map[string]string{"BRIDGE_DATA_DIR": filepath.Join(dir, "elsewhere")}
			},
			value: func(c *Config) any { return c.DataDir },
			own:   func(dir string) any { return filepath.Join(dir, "data") },
		},
		{
			name: "libraryName", leaf: "libraryName", file: roots,
			vars:  func(string) map[string]string { return map[string]string{"BRIDGE_LIBRARY_NAME": "Env Name"} },
			value: func(c *Config) any { return c.LibraryName },
			own:   func(string) any { return DefaultLibraryName },
		},
		{
			// An output-only bridge is a loopback install with no library
			// root, so a file without roots loads without the variable too.
			name: "libraryRoots", leaf: "libraryRoots", file: "output:\n  mpd: \"on\"\n",
			vars: func(dir string) map[string]string {
				return map[string]string{"BRIDGE_LIBRARY_ROOTS": filepath.Join(dir, "Music")}
			},
			value: func(c *Config) any { return len(c.LibraryRoots) },
			own:   func(string) any { return 0 },
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			path, dir := envSaveFile(t, c.file)
			vars := c.vars(dir)
			for name, v := range vars {
				t.Setenv(name, v)
			}
			if _, there := fileLeaf(t, path, c.leaf); there {
				t.Fatalf("the fixture writes %s; it must not", c.leaf)
			}
			rc := bootHolder(t, path)
			live := c.value(rc.Load())
			if reflect.DeepEqual(live, c.own(dir)) {
				t.Fatalf("booted with %v, the live %s is %#v, the default: the variable set nothing", vars, c.leaf, live)
			}
			for i := 1; i <= 2; i++ {
				if err := rc.Update(path, func(next *Config) error {
					next.Library.AllowDelete = !next.Library.AllowDelete
					return nil
				}); err != nil {
					t.Fatalf("save %d: %v", i, err)
				}
				if got, there := fileLeaf(t, path, c.leaf); there {
					t.Errorf("save %d of another setting wrote %s = %#v, a key the file did not have", i, c.leaf, got)
				}
				if got := c.value(rc.Load()); !reflect.DeepEqual(got, live) {
					t.Errorf("save %d: the holder publishes %s = %#v, want the variable's %#v", i, c.leaf, got, live)
				}
			}
			withoutVariables(t, vars, func() {
				if got := c.value(loadAt(t, path)); !reflect.DeepEqual(got, c.own(dir)) {
					t.Errorf("a start without %v reads %s = %#v, want the file's own %#v", vars, c.leaf, got, c.own(dir))
				}
			})
		})
	}
}

// TestAnEmptyListOverrideASaveLeftKeepsTheFilesList: a variable that sets a
// list to none (BRIDGE_METRICS_ALLOW_CIDRS="," loads an empty list) and that
// a save of another setting leaves as it was keeps the file's own list in
// the file, through two saves, and a start without the variable reads it
// (backlog B761's second round): an empty list and no list are one value,
// whatever a clone makes of the one the load gave.
func TestAnEmptyListOverrideASaveLeftKeepsTheFilesList(t *testing.T) {
	vars := map[string]string{"BRIDGE_METRICS_ALLOW_CIDRS": ","}
	for name, v := range vars {
		t.Setenv(name, v)
	}
	path := envSaveConfig(t, "metrics:\n  allowCidrs:\n    - 192.168.0.0/16\n")
	rc := bootHolder(t, path)
	if got := rc.Load().Metrics.AllowCIDRs; len(got) != 0 {
		t.Fatalf("booted with BRIDGE_METRICS_ALLOW_CIDRS=\",\", the live list is %q, want none", got)
	}
	for i := 1; i <= 2; i++ {
		if err := rc.Update(path, func(next *Config) error {
			next.Library.AllowDelete = !next.Library.AllowDelete
			return nil
		}); err != nil {
			t.Fatalf("save %d: %v", i, err)
		}
		if got, there := fileLeaf(t, path, "metrics.allowCidrs"); !there || !reflect.DeepEqual(got, []any{"192.168.0.0/16"}) {
			t.Errorf("save %d of another setting left metrics.allowCidrs = %#v (there %v), want the file's own list", i, got, there)
		}
		if got := rc.Load().Metrics.AllowCIDRs; len(got) != 0 {
			t.Errorf("save %d: the holder publishes the list %q, want the variable's none", i, got)
		}
	}
	withoutVariables(t, vars, func() {
		if got := loadAt(t, path).Metrics.AllowCIDRs; !reflect.DeepEqual(got, []string{"192.168.0.0/16"}) {
			t.Errorf("a start without the variable reads metrics.allowCidrs %q, want the file's own list", got)
		}
	})
}

// TestASaveWritesNoKeyForAnyLeafTheFileDoesNotWrite: for every leaf a
// variable sets, whatever its field's tag, a save that leaves the leaf at the
// variable's value over a file that does not write it writes no key for it:
// the save's own steps (filedEnvValues, then marshal) on a config with every
// other field set, so that every section is written, over an empty file, an
// empty mapping and a file that writes another setting alone. The opt-outs
// are filedSwitches' (TestTheOptOutsVariablesAreStillWrittenAsTheirSwitchesAre).
func TestASaveWritesNoKeyForAnyLeafTheFileDoesNotWrite(t *testing.T) {
	values := map[reflect.Kind]string{reflect.String: "v", reflect.Bool: "true", reflect.Int: "7", reflect.Int64: "7", reflect.Slice: "v"}
	for _, b := range envBindings() {
		if switchEnvPaths[b.YAMLPath] {
			continue
		}
		t.Run(b.YAMLPath, func(t *testing.T) {
			var marked Config
			if markLeaf(envLeaf(&marked, b)); envLeaf(&marked, b).IsZero() {
				t.Fatalf("markLeaf leaves %s at its zero value, so a save cannot tell whether a file writes it", b.YAMLPath)
			}
			kind := b.kind
			if kind == reflect.Pointer {
				kind = b.elemKind
			}
			before := &Config{}
			fillNonZero(reflect.ValueOf(before).Elem())
			before.envSet = []string{b.YAMLPath}
			if !before.applyEnvValue(b, b.EnvVar, values[kind]) {
				t.Fatalf("%s=%q sets nothing", b.EnvVar, values[kind])
			}
			other := "libraryName: Kept\n"
			if b.YAMLPath == "libraryName" {
				other = "scanIntervalSec: 60\n"
			}
			for _, file := range []string{"", "{}\n", other} {
				saved := *Clone(before)
				filedEnvValues(&saved, before, []byte(file))
				text, err := saved.marshal()
				if err != nil {
					t.Fatal(err)
				}
				if got, there := textLeaf(t, text, b.YAMLPath); there {
					t.Errorf("over a file of %q, a save that left %s wrote it as %#v, a key the file did not have", file, b.YAMLPath, got)
				}
			}
		})
	}
}

// TestASavesTextIsYAMLMarshalsWithoutTheKeysItLeavesOut: the text a save
// writes where it leaves keys out (marshal) is yaml.Marshal's with those keys
// taken out and nothing else changed, over a config with every field set and
// strings YAML must quote, escape, fold or encode (a plain << among them,
// which a yaml.Node re-encodes as !!merge <<): byte for byte where no field
// of Config's own is named, a leaf below the top level naming none (its tag
// omits its zero value); without that key's line for each field of Config's
// own at its zero value; and byte for byte for one at another value, which is
// written as it is.
func TestASavesTextIsYAMLMarshalsWithoutTheKeysItLeavesOut(t *testing.T) {
	cfg := &Config{}
	fillNonZero(reflect.ValueOf(cfg).Elem())
	awkward := []string{
		"", " ", "  leading", "trailing  ", "two\nlines", "a newline at its end\n", "\ttab", "a: b", "a #b", "#hash",
		"- dash", "? q", "'single'", `"double"`, `back\slash`, "true", "no", "On", "y", "123", "0x1F", "0o17", "1e3",
		"1_000", ".5", "-.inf", ".NaN", "~", "null", "2026-11-08", "12:30", "@at", "`tick`", "%pct", "!bang",
		"&anchor", "*alias", "|pipe", ">fold", "{}", "[]", "[a, b]", "{a: b}", ",", "<<", "Japanese 日本語",
		"nul\x00byte", "bell\a", "del\x7f", "line" + string(rune(0x2028)) + "separator", string(rune(0xfeff)) + "bom",
		"emoji " + string(rune(0x1f3b5)), "caf\xe9, Latin-1", strings.Repeat("long ", 60),
	}
	cfg.LibraryRoots = awkward
	cfg.CustomEndpoints = awkward
	cfg.Metrics.AllowCIDRs = awkward
	cfg.LibraryName = "two\nlines"
	cfg.ListenAddress = "a: b"
	cfg.DataDir = "  leading"
	cfg.Enrich.MusicBrainzBaseURL = strings.Repeat("long ", 60)
	plain, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfg.unwritten = []string{"nothing", "no.such.key", "metrics.allowCidrs"}
	if got, err := cfg.marshal(); err != nil || !bytes.Equal(got, plain) {
		t.Errorf("naming no field of Config's own, a save writes (%v)\n%s\nwant yaml.Marshal's\n%s", err, got, plain)
	}
	for _, path := range []string{"scanIntervalSec", "libraryRoots", "listenAddress"} {
		cfg.unwritten = []string{path}
		if got, err := cfg.marshal(); err != nil || !bytes.Equal(got, plain) {
			t.Errorf("naming %s, set, a save writes (%v)\n%s\nwant it written as it is\n%s", path, err, got, plain)
		}
	}

	for _, b := range envBindings() {
		c := *cfg
		c.unwritten = nil
		envLeaf(&c, b).Set(reflect.Zero(envLeaf(&c, b).Type()))
		zero, err := yaml.Marshal(&c)
		if err != nil {
			t.Fatal(err)
		}
		want := zero
		if !strings.Contains(b.YAMLPath, ".") {
			want = withoutTopLevelKey(t, zero, b.YAMLPath)
		}
		c.unwritten = []string{b.YAMLPath}
		if got, err := c.marshal(); err != nil || !bytes.Equal(got, want) {
			t.Errorf("leaving %s out at its zero value, a save writes (%v)\n%s\nwant\n%s", b.YAMLPath, err, got, want)
		}
	}
}

// withoutTopLevelKey is text, a config's YAML, without the line that writes
// key at the top level, and text as it is where no line does.
func withoutTopLevelKey(t *testing.T, text []byte, key string) []byte {
	t.Helper()
	var out strings.Builder
	found := 0
	for _, line := range strings.SplitAfter(string(text), "\n") {
		if strings.HasPrefix(line, key+":") {
			found++
			continue
		}
		out.WriteString(line)
	}
	if found > 1 {
		t.Fatalf("%d lines write %s", found, key)
	}
	return []byte(out.String())
}

// TestEveryLeafWhoseZeroIsAKeyIsAtTheTopLevel: a leaf a variable sets whose
// field writes its zero value as a key (its yaml tag names no omitempty) is a
// field of Config itself, which a save that leaves it out gives omitempty
// (marshal), and which lies below no type that reads its own YAML, so the
// save's second decode says whether the file writes it (filedEnvValues). One
// below the top level would be written as its zero value, a key the file did
// not have: give it omitempty (every leaf below the top level has it).
func TestEveryLeafWhoseZeroIsAKeyIsAtTheTopLevel(t *testing.T) {
	for _, b := range envBindings() {
		typ := reflect.TypeFor[Config]()
		var tag string
		for _, i := range b.index {
			f := typ.Field(i)
			typ, tag = f.Type, f.Tag.Get("yaml")
		}
		if len(b.index) > 1 && !slices.Contains(strings.Split(tag, ",")[1:], "omitempty") {
			t.Errorf("%s writes its zero value as a key below the top level: give it omitempty", b.YAMLPath)
		}
	}
}
