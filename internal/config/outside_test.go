package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// A console save over a config file another writer changed since serve read
// it (backlog B535). The live holder reads the file its save's turn finds and
// the bytes it last read or wrote as Load reads a file for its settings,
// records each setting the two set differently until a restart, and every save
// then writes those settings as the file holds them beside its own change,
// publishing its own snapshot. A change of such a setting, a deployment
// setting changed, and a merged file the next start would refuse are refused.
// Until B535 every save over a changed file was refused, whatever it changed,
// until a restart.

// appendToConfig appends text to the config file at path, as an edit or a
// writer that takes no lock leaves it.
func appendToConfig(t *testing.T, path, text string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(text); err != nil {
		t.Fatal(err)
	}
}

// replaceInConfig replaces old, which the config file at path holds once, with
// replacement, as an edit does.
func replaceInConfig(t *testing.T, path, old, replacement string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if n := bytes.Count(raw, []byte(old)); n != 1 {
		t.Fatalf("the config holds %q %d times, want once:\n%s", old, n, raw)
	}
	if err := os.WriteFile(path, bytes.Replace(raw, []byte(old), []byte(replacement), 1), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestASaveKeepsASettingAnotherWriterChanged: a console save over a config
// file another writer changed (scanIntervalSec: 7200 appended) lands and
// writes that setting as the file holds it beside its own change, save after
// save: the holder keeps the setting until a restart. The live interval stays
// the one serve loaded, since nothing reloads the file before a restart; and
// the log says once which setting another writer changed, never its value.
func TestASaveKeepsASettingAnotherWriterChanged(t *testing.T) {
	logs := loggingtest.Record(t)
	path := envSaveConfig(t, "libraryName: Home\n")
	rc := bootHolder(t, path)
	booted := rc.Load().ScanIntervalSec
	appendToConfig(t, path, "scanIntervalSec: 7200\n")
	for _, step := range []struct {
		name   string
		change func(*Config)
	}{
		{"a rename", func(c *Config) { c.LibraryName = "Renamed" }},
		{"a save of another setting", func(c *Config) { c.Library.AllowDelete = true }},
	} {
		if err := rc.Update(path, func(c *Config) error { step.change(c); return nil }); err != nil {
			t.Fatalf("%s over a file another writer changed: %v", step.name, err)
		}
		if after := loadAt(t, path); after.LibraryName != "Renamed" || after.ScanIntervalSec != 7200 {
			t.Errorf("after %s the file loads libraryName %q and scanIntervalSec %d, want the rename and the other writer's 7200",
				step.name, after.LibraryName, after.ScanIntervalSec)
		}
		if got := rc.Load().ScanIntervalSec; got != booted {
			t.Errorf("after %s the live scanIntervalSec is %d, want %d, the one serve loaded", step.name, got, booted)
		}
	}
	lines := logs.Lines(outsideSettingMessage)
	if len(lines) != 1 || !strings.Contains(lines[0], "setting=scanIntervalSec") || strings.Contains(lines[0], "7200") {
		t.Errorf("logged %q, want one line naming scanIntervalSec and not its value", lines)
	}
}

// TestASettingAnotherWriterChangedIsKeptPastASaveThatDidNotLand: the turn
// that finds another writer's change records it, with the bytes it read it
// from, whatever its own save then does: a save whose change refuses itself
// after that turn is followed by one that lands and still writes the other
// writer's value.
func TestASettingAnotherWriterChangedIsKeptPastASaveThatDidNotLand(t *testing.T) {
	path := envSaveConfig(t, "libraryName: Home\n")
	rc := bootHolder(t, path)
	appendToConfig(t, path, "scanIntervalSec: 7200\n")
	refused := errors.New("the change refused itself")
	if err := rc.Update(path, func(*Config) error { return refused }); err != refused {
		t.Fatalf("a change that refuses itself = %v, want its own error", err)
	}
	if err := rc.Update(path, func(c *Config) error { c.LibraryName = "Renamed"; return nil }); err != nil {
		t.Fatalf("the next save: %v", err)
	}
	if after := loadAt(t, path); after.LibraryName != "Renamed" || after.ScanIntervalSec != 7200 {
		t.Errorf("the file loads libraryName %q and scanIntervalSec %d, want the rename and the other writer's 7200",
			after.LibraryName, after.ScanIntervalSec)
	}
}

// TestAWriterThatWritesOutDefaultsChangesOnlyWhatItChanged: config.Edit
// (library add and remove) saves the whole config it loaded, every default
// written out, every relative path resolved and the library name Normalize
// gives. Read as Load reads them, the two files then differ in the one
// setting it changed, so a console save of another setting lands: of the
// listen address over a file that had none and a relative data dir, and a
// rename over a file that had no name.
func TestAWriterThatWritesOutDefaultsChangesOnlyWhatItChanged(t *testing.T) {
	for _, tc := range []struct {
		name   string
		file   string
		change func(*Config)
		want   func(*Config) bool
	}{
		{"the listen address", "libraryRoots:\n  - Music\ndataDir: data\nlibraryName: Home\n",
			func(c *Config) { c.ListenAddress = "127.0.0.1:9911" },
			func(c *Config) bool { return c.ListenAddress == "127.0.0.1:9911" }},
		{"the library name", "libraryRoots:\n  - Music\ndataDir: data\n",
			func(c *Config) { c.LibraryName = "Renamed" },
			func(c *Config) bool { return c.LibraryName == "Renamed" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Mkdir(filepath.Join(dir, "Music"), 0o755); err != nil {
				t.Fatal(err)
			}
			path := writeConfigFile(t, dir, tc.file)
			rc := bootHolder(t, path)
			if err := Edit(path, func(c *Config) (bool, error) { c.ScanIntervalSec = 7200; return true, nil }); err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			listen, _ := textLeaf(t, raw, "listenAddress")
			data, _ := textLeaf(t, raw, "dataDir")
			if listen != DefaultListenAddress || data != filepath.Join(dir, "data") {
				t.Fatalf("premise: Edit wrote listenAddress %#v and dataDir %#v, want the default and the resolved path", listen, data)
			}
			if err := rc.Update(path, func(c *Config) error { tc.change(c); return nil }); err != nil {
				t.Fatalf("a save of %s over a file a writer of defaults changed: %v", tc.name, err)
			}
			if after := loadAt(t, path); !tc.want(after) || after.ScanIntervalSec != 7200 {
				t.Errorf("the file loads %+v, want the console's %s and the other writer's 7200", after, tc.name)
			}
		})
	}
}

// TestASaveKeepsAPlayerBlockThatDoesNotReadAsAnotherWriterLeftIt: a player
// block that does not read is one setting, compared by the text it keeps
// (PlayerConfig.written), since its fields read nothing: another writer's
// change of its text is kept by a save of another setting.
func TestASaveKeepsAPlayerBlockThatDoesNotReadAsAnotherWriterLeftIt(t *testing.T) {
	path := writePlayerConfig(t, "  player:\n    enabled: true\n    noSuchSetting: 1\n")
	rc := bootHolder(t, path)
	if rc.Load().Output.Player.Unread() == nil {
		t.Fatal("premise: the player block read")
	}
	replaceInConfig(t, path, "noSuchSetting: 1", "noSuchSetting: 2")
	if err := rc.Update(path, func(c *Config) error { c.LibraryName = "Renamed"; return nil }); err != nil {
		t.Fatalf("a rename beside another writer's player block: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, there := textLeaf(t, raw, "output.player.noSuchSetting"); !there || got != 2 {
		t.Errorf("the file's player block holds noSuchSetting %#v (there %v), want the other writer's 2:\n%s", got, there, raw)
	}
	if got, _ := textLeaf(t, raw, "libraryName"); got != "Renamed" {
		t.Errorf("the file's libraryName is %#v, want the rename", got)
	}
}

// TestAnUnreadPlayerBlockIsComparedByItsSource: a player block that does not
// read, on both sides, is one setting compared by its source, the block's own
// text and that of each anchor its aliases name, never by the text a save
// would write back, which a block whose alias copies more than 1024 nodes
// keeps none of (notKept): such a block's change is found, and so is a change
// of the anchor its alias names, while a comment added elsewhere in the file
// is no change of it. Until B535's round 2 two such blocks compared alike
// whatever their text.
func TestAnUnreadPlayerBlockIsComparedByItsSource(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bridge.yaml")
	roots := func(first string) string {
		var b strings.Builder
		b.WriteString("libraryRoots: &roots\n  - /music/" + first + "\n")
		for i := 1; i < 1100; i++ {
			fmt.Fprintf(&b, "  - /music/%d\n", i)
		}
		return b.String()
	}
	block := func(enabled string) string {
		return "output:\n  player:\n    enabled: " + enabled + "\n    noSuchSetting: *roots\n"
	}
	read := func(text string) *Config {
		t.Helper()
		c, err := readSettings(path, []byte(text))
		if err != nil {
			t.Fatal(err)
		}
		if c.Output.Player.Unread() == nil || c.Output.Player.written != nil {
			t.Fatalf("premise: the block reads, or keeps text a save would write: %v", c.Output.Player.Unread())
		}
		return c
	}
	was := read(roots("0") + block("true"))
	for _, tc := range []struct {
		name, text string
		want       []string
	}{
		{"the block's own text changed", roots("0") + block("false"), []string{"output.player"}},
		{"the anchor its alias names changed", roots("zero") + block("true"), []string{"libraryRoots", "output.player"}},
		{"a comment added elsewhere", "# a note\n" + roots("0") + block("true"), nil},
	} {
		if got := settingsChanged(was, read(tc.text)); !slices.Equal(got, tc.want) {
			t.Errorf("%s: found as %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestASaveKeepsASerialAnotherWriterChanged: a serial (usbid.Serial, whose
// one field is unexported and which writes itself as one value) is one
// setting, so another writer's change of it is kept by a save of another
// setting, and no line carries either serial.
func TestASaveKeepsASerialAnotherWriterChanged(t *testing.T) {
	logs := loggingtest.Record(t)
	const theirs = "GOBAR-8R3Y-02LA-5582"
	path := writePlayerConfig(t, "  player:\n    device:\n      binding: serial\n      vendorId: \"0x20B1\"\n      productId: \"3009\"\n"+
		"      serial: \""+playerSerial+"\"\n")
	rc := bootHolder(t, path)
	if p := rc.Load().Output.Player; p.Unread() != nil || !p.Device.Serial.Present() {
		t.Fatalf("premise: the block does not read, or holds no serial: %v", p.Unread())
	}
	replaceInConfig(t, path, playerSerial, theirs)
	if err := rc.Update(path, func(c *Config) error { c.LibraryName = "Renamed"; return nil }); err != nil {
		t.Fatalf("a rename beside another writer's serial: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := textLeaf(t, raw, "output.player.device.serial"); got != theirs {
		t.Errorf("the file holds the serial %#v, want the other writer's", got)
	}
	for _, line := range logs.All() {
		if leaksSerial(line, playerSerial) || leaksSerial(line, theirs) {
			t.Errorf("a line carries a serial: %q", line)
		}
	}
}

// TestAMergedSaveTheNextStartWouldRefuseIsRefused: the file a merged save
// would write is loaded as the next start loads it, the variables applied,
// before its rename, and one that would not load, or whose OpenHome surface
// would sit on a port another listener binds where the file another writer
// left had no such clash, is refused with nothing written or stored. Each
// change is one its own writer's checks take, on the live config: a variants
// folder below a root another writer added, which the console's check against
// the live roots cannot see, and the OpenHome surface turned on beside a
// listen address another writer moved onto its port.
func TestAMergedSaveTheNextStartWouldRefuseIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name      string
		elsewhere func(t *testing.T, path string) string
		change    func(c *Config, extra string)
		stored    func(c *Config) bool
	}{
		{"a variants folder below a root another writer added",
			func(t *testing.T, path string) string {
				extra := filepath.Join(filepath.Dir(path), "Extra")
				if err := os.Mkdir(extra, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := Edit(path, func(c *Config) (bool, error) {
					c.LibraryRoots = append(c.LibraryRoots, extra)
					return true, nil
				}); err != nil {
					t.Fatal(err)
				}
				return extra
			},
			func(c *Config, extra string) { c.Upscale.VariantsDir = filepath.Join(extra, "variants") },
			func(c *Config) bool { return c.Upscale.VariantsDir != "" }},
		{"the OpenHome surface on a port another writer gave the listener",
			func(t *testing.T, path string) string {
				appendToConfig(t, path, "listenAddress: ':7792'\n")
				return ""
			},
			func(c *Config, _ string) { c.Output.Player.OpenHome = true },
			func(c *Config) bool { return c.Output.Player.OpenHome }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := envSaveConfig(t, "libraryName: Home\n")
			rc := bootHolder(t, path)
			extra := tc.elsewhere(t, path)
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			err = rc.Update(path, func(c *Config) error { tc.change(c, extra); return nil })
			if !errors.Is(err, ErrChangedOutside) {
				t.Errorf("a save the next start would refuse = %v, want ErrChangedOutside", err)
			}
			if after, _ := os.ReadFile(path); !bytes.Equal(after, before) {
				t.Errorf("the refused save wrote the file:\n%s", after)
			}
			if tc.stored(rc.Load()) {
				t.Error("the refused save stored its change")
			}
			if _, err := Load(path); err != nil {
				t.Errorf("the other writer's file no longer loads: %v", err)
			}
		})
	}
}

// TestTheMergedFileIsLoadedAsTheNextStartLoadsItWithoutAWord:
// loadsAtNextStart takes the file Load takes and refuses the one Load
// refuses, the BRIDGE_* variables applied, and says nothing either way: not
// the default a public config's Tailscale mode is given, not a variable that
// does not read, not why the player is off, and the player-off reason it
// leaves is the one it found. Load takes an OpenHome surface on a port
// another listener binds, turning the player off over it, and so does
// loadsAtNextStart: whether a save adds such a clash is refuseMerge's to say.
func TestTheMergedFileIsLoadedAsTheNextStartLoadsItWithoutAWord(t *testing.T) {
	t.Setenv("BRIDGE_SMART_PLAYLISTS_ENABLED", "now and then")
	for _, tc := range []struct {
		name, body string
		loads      bool
	}{
		{"a config that loads", "libraryName: Home\n", true},
		{"a public config whose Tailscale mode is the default", "deployment:\n  mode: public\n  adminTLSTerminatedByProxy: true\n" +
			"adminAddress: 127.0.0.1:7789\nautocert:\n  domain: bridge.example.com\n", true},
		{"a player block that does not read", "output:\n  player:\n    enabled: true\n    noSuchSetting: 1\n", true},
		{"an OpenHome surface on a port a listener binds", "listenAddress: ':7792'\noutput:\n  player:\n    enabled: true\n" +
			"    openhome: true\n", true},
		{"a whole number written as a fraction", "scanIntervalSec: 2.5\n", false},
		{"a variants folder below a library root", "upscale:\n  variantsDir: {lib}\n", false},
		{"a setting this release does not know", "noSuchSetting: 1\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path, _ := envSaveFile(t, "libraryRoots:\n  - {lib}\ndataDir: {dir}\n"+tc.body)
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			forgetPlayerOffReason(t)
			logs := loggingtest.Record(t)
			_, err = loadsAtNextStart(path, raw)
			if lines := logs.All(); len(lines) != 0 {
				t.Errorf("the merged load said %q", lines)
			}
			playerOffLogged.mu.Lock()
			reason := playerOffLogged.reason
			playerOffLogged.mu.Unlock()
			if reason != "" {
				t.Errorf("the merged load moved the player-off reason to %q", reason)
			}
			if _, loadErr := Load(path); (loadErr == nil) != tc.loads || (err == nil) != tc.loads {
				t.Errorf("Load answers %v and the merged load %v, want both to load: %v", loadErr, err, tc.loads)
			}
		})
	}
}

// TestASaveOverAFileAnotherWriterLeftUnloadableIsRefusedSayingToCorrectIt: a
// file another writer left that the next start would refuse on its own (a
// variants folder below a library root) refuses every console save, a rename
// included, as the file left so (OutsideLeftUnloadable), not as a save that
// would leave such a file: its words say to correct the file, never to
// restart, which would fail on that file. Nothing is written or stored, and
// once that writer has corrected the file the next save lands.
func TestASaveOverAFileAnotherWriterLeftUnloadableIsRefusedSayingToCorrectIt(t *testing.T) {
	path := envSaveConfig(t, "libraryName: Home\n")
	rc := bootHolder(t, path)
	booted, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	appendToConfig(t, path, "upscale:\n  variantsDir: "+yamlStr(filepath.Join(rc.Load().LibraryRoots[0], "variants"))+"\n")
	if _, err := Load(path); err == nil {
		t.Fatal("premise: the file the other writer left loads")
	}
	edited, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	rename := func(c *Config) error { c.LibraryName = "Renamed"; return nil }
	err = rc.Update(path, rename)
	var outside *ChangedOutsideError
	if !errors.As(err, &outside) || outside.Kind != OutsideLeftUnloadable {
		t.Fatalf("a rename over a file another writer left unloadable = %v, want an OutsideLeftUnloadable refusal", err)
	}
	if msg := err.Error(); !strings.Contains(msg, "correct the file") || strings.Contains(msg, "restart") {
		t.Errorf("refusal %q: want it to say to correct the file, and nothing of a restart", msg)
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(after, edited) {
		t.Errorf("the refused rename wrote the file:\n%s", after)
	}
	if got := rc.Load().LibraryName; got != "Home" {
		t.Errorf("the refused rename stored libraryName %q", got)
	}
	if err := os.WriteFile(path, booted, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := rc.Update(path, rename); err != nil {
		t.Fatalf("a rename once the other writer corrected the file: %v", err)
	}
	if got := loadAt(t, path).LibraryName; got != "Renamed" {
		t.Errorf("the file loads libraryName %q, want the rename", got)
	}
}

// TestAnOpenHomeClashAnotherWriterLeftRefusesOnlyASaveThatAddsOne: Load takes
// a file whose OpenHome surface sits on a port another listener binds, and
// turns the player off over it, so a clash another writer left in the file
// refuses nothing on its own: a save of another setting lands, and so does
// one that moves another listener to a port of its own, each keeping that
// writer's listen address. A save that brings a second listener onto the
// OpenHome port adds a clash the file did not have, and is refused
// (OutsideUnloadable, its reason naming that listener's setting), with
// nothing written or stored.
func TestAnOpenHomeClashAnotherWriterLeftRefusesOnlyASaveThatAddsOne(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Config)
		// refused is the setting the refusal's clash names, "" for a save
		// that lands.
		refused string
	}{
		{"a save of another setting", func(c *Config) { c.LibraryName = "Renamed" }, ""},
		{"another listener moved to a port of its own", func(c *Config) { c.AdminAddress = "127.0.0.1:7799" }, ""},
		{"a second listener moved onto the OpenHome port", func(c *Config) { c.AdminAddress = "127.0.0.1:7792" }, "adminAddress"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := envSaveConfig(t, "libraryName: Home\noutput:\n  player:\n    openhome: true\n")
			rc := bootHolder(t, path)
			appendToConfig(t, path, "listenAddress: ':7792'\n")
			if _, err := Load(path); err != nil {
				t.Fatalf("premise: Load refuses the clash the other writer left: %v", err)
			}
			edited, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			err = rc.Update(path, func(c *Config) error { tc.change(c); return nil })
			if tc.refused == "" {
				if err != nil {
					t.Fatalf("%s beside the clash another writer left: %v", tc.name, err)
				}
				if got := loadAt(t, path).ListenAddress; got != ":7792" {
					t.Errorf("the file loads listenAddress %q, want the other writer's", got)
				}
				return
			}
			var outside *ChangedOutsideError
			var clash *OpenHomePortClashError
			if !errors.As(err, &outside) || outside.Kind != OutsideUnloadable || !errors.As(outside.Err, &clash) || clash.Setting != tc.refused {
				t.Errorf("%s = %v, want an OutsideUnloadable refusal over the clash of %s", tc.name, err, tc.refused)
			}
			if after, _ := os.ReadFile(path); !bytes.Equal(after, edited) {
				t.Errorf("the refused save wrote the file:\n%s", after)
			}
			if got := rc.Load().AdminAddress; got == "127.0.0.1:7792" {
				t.Error("the refused save stored its change")
			}
		})
	}
}

// TestADeploymentSettingAnotherWriterChangedRefusesEverySave: the console
// decides what it may change from its live copy of the posture and the
// managed lists, so once another writer changes a deployment setting every
// save is refused, whatever it changes, until a restart, and the file keeps
// that writer's change.
func TestADeploymentSettingAnotherWriterChangedRefusesEverySave(t *testing.T) {
	path := envSaveConfig(t, "libraryName: Home\n")
	rc := bootHolder(t, path)
	appendToConfig(t, path, "deployment:\n  managedSettings:\n    - libraryName\n")
	edited, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*Config){
		func(c *Config) { c.LibraryName = "Renamed" },
		func(c *Config) { c.Library.AllowDelete = true },
	} {
		if err := rc.Update(path, func(c *Config) error { change(c); return nil }); !errors.Is(err, ErrChangedOutside) ||
			!strings.Contains(err.Error(), "restart the bridge") {
			t.Errorf("a save after another writer changed a deployment setting = %v, want it refused, saying to restart", err)
		}
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(after, edited) {
		t.Errorf("a refused save wrote the file:\n%s", after)
	}
	if live := rc.Load(); live.LibraryName != "Home" || live.Library.AllowDelete {
		t.Errorf("a refused save stored its change: %q, %v", live.LibraryName, live.Library.AllowDelete)
	}
}

// TestAChangeOfAVariablesSettingAnotherWriterChangedIsRefused: a setting a
// BRIDGE_* variable set at the start that another writer changed in the file
// is recorded for the conflict check alone (outsideEnv), so a console change
// of it is refused, naming it, with the file as that writer left it and the
// live value as it was: until round 2 of B535 such a change landed, and B761's
// rule then wrote the console's value over the other writer's. A change is
// refused whatever value it writes, as for every setting another writer
// changed: the second case's flip writes off where the other writer wrote off
// too.
func TestAChangeOfAVariablesSettingAnotherWriterChangedIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name      string
		flipFirst bool
	}{
		{"another writer's edit, then the console's flip", false},
		{"the console's flip, another writer's edit, then a flip to the value it wrote", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("BRIDGE_ENRICH_ENABLED", "false")
			path := envSaveConfig(t, "libraryName: Home\n")
			rc := bootHolder(t, path)
			flip := func(on bool) error {
				return rc.Update(path, func(c *Config) error { c.Enrich.Enabled = &on; return nil })
			}
			if tc.flipFirst {
				if err := flip(true); err != nil {
					t.Fatalf("the console's first flip of enrichment, with nothing changed outside: %v", err)
				}
			}
			appendToConfig(t, path, "enrich:\n  enabled: false\n")
			edited, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			live := rc.Load().Enrich.EffectiveEnabled()
			err = flip(!live)
			var outside *ChangedOutsideError
			if !errors.As(err, &outside) || outside.Kind != OutsideConflict || !slices.Equal(outside.Settings, []string{"enrich.enabled"}) {
				t.Errorf("the console's flip of enrichment to %v after another writer wrote it = %v, want a conflict naming enrich.enabled", !live, err)
			}
			if after, _ := os.ReadFile(path); !bytes.Equal(after, edited) {
				t.Errorf("the refused flip wrote the file:\n%s", after)
			}
			if got := rc.Load().Enrich.EffectiveEnabled(); got != live {
				t.Errorf("the refused flip stored enrichment %v, want %v", got, live)
			}
		})
	}
}

// TestASaveOfAnotherSettingKeepsAVariablesSettingAnotherWriterChanged: a save
// of another setting beside another writer's change of a setting a variable
// set lands, and writes that setting as the file holds it (B761's rule), the
// live value staying the variable's; and it logs no line saying the change
// takes effect at the next start, which the variable makes false while it is
// set.
func TestASaveOfAnotherSettingKeepsAVariablesSettingAnotherWriterChanged(t *testing.T) {
	t.Setenv("BRIDGE_SCAN_INTERVAL_SEC", "120")
	logs := loggingtest.Record(t)
	path := envSaveConfig(t, "libraryName: Home\n")
	rc := bootHolder(t, path)
	appendToConfig(t, path, "scanIntervalSec: 7200\n")
	if err := rc.Update(path, func(c *Config) error { c.LibraryName = "Renamed"; return nil }); err != nil {
		t.Fatalf("a rename beside another writer's change of a variable's setting: %v", err)
	}
	if got, there := fileLeaf(t, path, "scanIntervalSec"); !there || got != 7200 {
		t.Errorf("the file holds scanIntervalSec %#v (there %v), want the other writer's 7200", got, there)
	}
	if got := rc.Load().ScanIntervalSec; got != 120 {
		t.Errorf("the live scanIntervalSec is %d, want the variable's 120", got)
	}
	if lines := logs.Lines(outsideSettingMessage); len(lines) != 0 {
		t.Errorf("logged %q for a setting a variable sets", lines)
	}
}

// TestAnUnrelatedSaveLandsBesideAVariablesDeploymentSettingAnotherWriterChanged:
// a deployment setting a variable set is recorded for the conflict check
// alone, never as a deployment setting another writer changed, which would
// refuse every save: a save of another setting lands beside another writer's
// change of deployment.managedControls under BRIDGE_DEPLOYMENT_MANAGED_CONTROLS,
// and the file keeps that writer's list (B761's rule).
func TestAnUnrelatedSaveLandsBesideAVariablesDeploymentSettingAnotherWriterChanged(t *testing.T) {
	t.Setenv("BRIDGE_DEPLOYMENT_MANAGED_CONTROLS", "restart")
	path := envSaveConfig(t, "libraryName: Home\n")
	rc := bootHolder(t, path)
	appendToConfig(t, path, "deployment:\n  managedControls:\n    - updates\n")
	if err := rc.Update(path, func(c *Config) error { c.LibraryName = "Renamed"; return nil }); err != nil {
		t.Fatalf("a rename beside another writer's change of a variable's deployment setting: %v", err)
	}
	if got, there := fileLeaf(t, path, "deployment.managedControls"); !there || !reflect.DeepEqual(got, []any{"updates"}) {
		t.Errorf("the file holds deployment.managedControls %#v (there %v), want the other writer's [updates]", got, there)
	}
	if got := rc.Load().Deployment.ManagedControls; !slices.Equal(got, []string{"restart"}) {
		t.Errorf("the live deployment.managedControls is %q, want the variable's [restart]", got)
	}
}

// TestASwitchIsComparedByWhatItTurnsOn: the three opt-outs (enrich.enabled,
// enrich.deezerEnabled, update.checkEnabled) write on as no key or as true,
// so they are compared by what they turn on, never by their pointers: a
// switch written true is no change of one left out, and a save that sets a
// switch to what it already was changes nothing, so it lands beside another
// writer's change of that switch, which the file keeps.
func TestASwitchIsComparedByWhatItTurnsOn(t *testing.T) {
	on, off := true, false
	for _, sw := range []struct {
		path string
		set  func(c *Config, v *bool)
	}{
		{"enrich.enabled", func(c *Config, v *bool) { c.Enrich.Enabled = v }},
		{"enrich.deezerEnabled", func(c *Config, v *bool) { c.Enrich.DeezerEnabled = v }},
		{"update.checkEnabled", func(c *Config, v *bool) { c.Update.CheckEnabled = v }},
	} {
		none, written, turnedOff := &Config{}, &Config{}, &Config{}
		sw.set(written, &on)
		sw.set(turnedOff, &off)
		if got := settingsChanged(none, written); len(got) != 0 {
			t.Errorf("%s written true is found to differ from none in %q", sw.path, got)
		}
		if got := settingsChanged(none, turnedOff); !slices.Equal(got, []string{sw.path}) {
			t.Errorf("%s written false is found as %q, want it alone", sw.path, got)
		}
	}
	path := envSaveConfig(t, "libraryName: Home\n")
	rc := bootHolder(t, path)
	appendToConfig(t, path, "enrich:\n  enabled: false\n")
	if err := rc.Update(path, func(c *Config) error { c.Enrich.Enabled = &on; return nil }); err != nil {
		t.Fatalf("a save that sets enrichment on where it was on, beside another writer's off: %v", err)
	}
	if got, there := fileLeaf(t, path, "enrich.enabled"); !there || got != false {
		t.Errorf("the file holds enrich.enabled %#v (there %v), want the other writer's false", got, there)
	}
	if !rc.Load().Enrich.EffectiveEnabled() {
		t.Error("the live switch is off, want it on as it was")
	}
}

// settingAt is one setting of Config as TestEachSettingIsFoundToDifferAlone
// reads the type, apart from the holder's own walk: its yaml path and its
// field's index from Config.
type settingAt struct {
	path  string
	index []int
}

// settingCensus is every setting of t under prefix: an exported field yaml
// writes under a key (its tag's, else its name lower-cased), reached through
// the structs that hold it; a struct that writes itself as one value (it
// implements yaml.Marshaler: a serial) is one setting, and the player block,
// which writes itself too, its fields.
func settingCensus(t reflect.Type, prefix string, index []int) []settingAt {
	var out []settingAt
	for i := range t.NumField() {
		f := t.Field(i)
		tag := f.Tag.Get("yaml")
		key, _, _ := strings.Cut(tag, ",")
		if !f.IsExported() || tag == "-" {
			continue
		}
		if key == "" {
			key = strings.ToLower(f.Name)
		}
		path := key
		if prefix != "" {
			path = prefix + "." + key
		}
		at := append(slices.Clone(index), i)
		if f.Type.Kind() == reflect.Struct && (f.Type == reflect.TypeFor[PlayerConfig]() || !f.Type.Implements(reflect.TypeFor[yaml.Marshaler]())) {
			out = append(out, settingCensus(f.Type, path, at)...)
			continue
		}
		out = append(out, settingAt{path, at})
	}
	return out
}

// anotherValue is a value of v's type other than v, a field fillNonZero
// filled: another word, number or switch; another pointee; a list whose first
// entry is another; a struct that writes itself as one value decoded from
// another word; and any other struct with its first field another.
func anotherValue(t *testing.T, v reflect.Value) reflect.Value {
	t.Helper()
	out := reflect.New(v.Type()).Elem()
	switch v.Kind() {
	case reflect.String:
		out.SetString(v.String() + "-another")
	case reflect.Bool:
		out.SetBool(!v.Bool())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		out.SetInt(v.Int() + 1)
	case reflect.Pointer:
		p := reflect.New(v.Type().Elem())
		p.Elem().Set(anotherValue(t, v.Elem()))
		out.Set(p)
	case reflect.Slice:
		out.Set(reflect.MakeSlice(v.Type(), v.Len(), v.Len()))
		reflect.Copy(out, v)
		out.Index(0).Set(anotherValue(t, v.Index(0)))
	case reflect.Struct:
		if v.Type().Implements(reflect.TypeFor[yaml.Marshaler]()) {
			if err := yaml.Unmarshal([]byte("another-value"), out.Addr().Interface()); err != nil {
				t.Fatalf("%s decodes no word: %v", v.Type(), err)
			}
			break
		}
		out.Set(v)
		out.Field(0).Set(anotherValue(t, v.Field(0)))
	default:
		t.Fatalf("no other value of a %s", v.Type())
	}
	return out
}

// TestEachSettingIsFoundToDifferAlone: the holder's comparison of two configs
// (settingsChanged), over a config with every setting set (fillNonZero, a
// serial included), finds a change of any one setting as that setting alone,
// by its yaml path: a list (upnpUpstream.servers among them) is one setting,
// never walked by its entries, and a serial one. Where Load reads them alike,
// an empty list and none are one value, an entry's own list too.
func TestEachSettingIsFoundToDifferAlone(t *testing.T) {
	base := &Config{}
	fillNonZero(reflect.ValueOf(base).Elem())
	if !base.Output.Player.Device.Serial.Present() {
		t.Fatal("premise: the fill left the serial unset")
	}
	census := settingCensus(reflect.TypeFor[Config](), "", nil)
	if len(census) < 100 {
		t.Fatalf("the census found %d settings; it reads nothing of Config", len(census))
	}
	if got := settingsChanged(base, Clone(base)); len(got) != 0 {
		t.Errorf("a clone is found to differ in %q", got)
	}
	for _, s := range census {
		other := Clone(base)
		field := reflect.ValueOf(other).Elem().FieldByIndex(s.index)
		field.Set(anotherValue(t, field))
		if got := settingsChanged(base, other); !slices.Equal(got, []string{s.path}) {
			t.Errorf("a change of %s alone is found as %q", s.path, got)
		}
		if field.Kind() != reflect.Slice {
			continue
		}
		none, empty := Clone(base), Clone(base)
		reflect.ValueOf(none).Elem().FieldByIndex(s.index).Set(reflect.Zero(field.Type()))
		reflect.ValueOf(empty).Elem().FieldByIndex(s.index).Set(reflect.MakeSlice(field.Type(), 0, 0))
		if got := settingsChanged(none, empty); len(got) != 0 {
			t.Errorf("no %s and an empty one are found to differ in %q", s.path, got)
		}
	}
	none, empty := Clone(base), Clone(base)
	none.UPnPUpstream.Servers[0].SkipTopLevelContainers = nil
	empty.UPnPUpstream.Servers[0].SkipTopLevelContainers = []string{}
	if got := settingsChanged(none, empty); len(got) != 0 {
		t.Errorf("an entry with no list and one with an empty list are found to differ in %q", got)
	}
}
