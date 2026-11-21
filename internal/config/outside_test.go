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
// the bytes it last recorded (read at boot or found at a turn, or written) as
// Load reads a file for its settings, records each setting the two set
// differently until a restart, and every save then writes those settings as
// the file holds them beside its own change, publishing its own snapshot. A
// change of such a setting (a variable's included), a deployment setting
// changed, a file that writer left that the next start would refuse, and a
// merged file the next start would refuse where the one left would not are
// refused. Until B535 every save over a changed file was refused, whatever it
// changed, until a restart.

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
// block that does not read is one setting, compared by what its source holds
// (PlayerConfig.source), since its fields read nothing: another writer's
// change of a value in it is kept by a save of another setting.
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
// read, on both sides, is one setting compared by its source, what the block
// holds and what each anchor its aliases name holds, never by the text a save
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

// TestAnUnreadPlayerBlocksDigestLeavesOutHowItIsWritten: the digest of an
// unread player block (blockSource) is of what the block holds, never of how
// the file writes it: a comment on a line of the sequence its alias names, a
// comment inside the block and an anchor renamed with its alias change
// nothing, while a value, an alias moved to another anchor and a tree of
// another shape each change it. Every alias is followed to its node, a node
// met again is written as a reference to the order it was first met in, and
// every token and list of children is framed by its length: [&a {k: 1}, *a],
// whose second element is a reference to its first, and [&a {k: 1}, {k: 1}],
// whose second element is a node of its own, differ, and so do [[a], b] and
// [[a, b]]. Until B535's round 3 the digest took the comments, the style and
// the anchors' names.
func TestAnUnreadPlayerBlocksDigestLeavesOutHowItIsWritten(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bridge.yaml")
	read := func(text string) *Config {
		t.Helper()
		c, err := readSettings(path, []byte(text))
		if err != nil {
			t.Fatal(err)
		}
		if c.Output.Player.Unread() == nil {
			t.Fatalf("premise: the block reads:\n%s", text)
		}
		return c
	}
	const roots = "libraryRoots: &roots\n  - /music/a\n  - /music/b\n"
	block := func(lines string) string { return "output:\n  player:\n" + lines }
	aliased := roots + block("    enabled: true\n    noSuchSetting: *roots\n")
	for _, tc := range []struct {
		name, was, now string
		differs        bool
	}{
		{"a line comment inside the anchored sequence", aliased,
			"libraryRoots: &roots\n  - /music/a # the first\n  - /music/b\n" + block("    enabled: true\n    noSuchSetting: *roots\n"), false},
		{"a comment inside the block", aliased,
			roots + block("    # the player's own note\n    enabled: true\n    noSuchSetting: *roots\n"), false},
		{"an anchor renamed with its alias", aliased, strings.NewReplacer("&roots", "&songs", "*roots", "*songs").Replace(aliased), false},
		{"a value inside the block", aliased, roots + block("    enabled: false\n    noSuchSetting: *roots\n"), true},
		{"an alias moved to another anchor", block("    x: &x 1\n    y: &y 1\n    noSuchSetting: *x\n"),
			block("    x: &x 1\n    y: &y 1\n    noSuchSetting: *y\n"), true},
		{"a reference against a copy", block("    noSuchSetting: [&a {k: 1}, *a]\n"), block("    noSuchSetting: [&a {k: 1}, {k: 1}]\n"), true},
		{"a list inside a list against one beside it", block("    noSuchSetting: [[a], b]\n"), block("    noSuchSetting: [[a, b]]\n"), true},
	} {
		var want []string
		if tc.differs {
			want = []string{"output.player"}
		}
		if got := settingsChanged(read(tc.was), read(tc.now)); !slices.Equal(got, want) {
			t.Errorf("%s: found as %q, want %q", tc.name, got, want)
		}
	}
}

// TestNormalizeRefusesNothingButABaseURL: normalize, which readSettings runs
// on a file's settings, refuses nothing but one of the two enrich base URLs,
// which normalizeSettings blanks before it normalizes again: with both blank,
// a config whose every setting is set (fillNonZero), and one whose every word
// is a word no setting takes (empty, white space, control characters, a scheme
// with no host, 4 KiB), normalizes. normalizeSettings returns a refusal it
// meets with no URL to blame, which the turn's read refuses the file over;
// none exists today, so this holds the invariant that hardening rests on
// (B535's round 2).
func TestNormalizeRefusesNothingButABaseURL(t *testing.T) {
	census := settingCensus(reflect.TypeFor[Config](), "", nil)
	for _, word := range []string{"", "  ", "\x00\x07\x1b", "http://", "a b\tc\nd", strings.Repeat("x", 4096)} {
		c := &Config{}
		fillNonZero(reflect.ValueOf(c).Elem())
		for _, s := range census {
			if f := reflect.ValueOf(c).Elem().FieldByIndex(s.index); f.Kind() == reflect.String {
				f.SetString(word)
			}
		}
		c.Enrich.MusicBrainzBaseURL, c.Enrich.CoverArtBaseURL = "", ""
		if err := c.normalize(false); err != nil {
			t.Errorf("normalize, every setting's word %q but the base URLs blank, refused: %v", word, err)
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

// TestAFileThatDoesNotLoadIsSaidOnceAndItsSettingsAreKept: a turn that finds
// another writer's file the next start would refuse records the settings that
// writer changed all the same, one beside the setting that fails included, and
// logs once, naming the file and no value, that the file does not load and is
// to be corrected, writing none of the lines that say a change takes effect at
// the next start, which would not start. Saves are refused, their changes
// never run, until that writer corrects the setting that fails; then a save
// lands and writes every setting that writer changed as the file holds it,
// none of them published, and nothing more is said of the file.
func TestAFileThatDoesNotLoadIsSaidOnceAndItsSettingsAreKept(t *testing.T) {
	path := envSaveConfig(t, "libraryName: Home\n")
	rc := bootHolder(t, path)
	booted := rc.Load().ScanIntervalSec
	logs := loggingtest.Record(t)
	inRoot := filepath.Join(rc.Load().LibraryRoots[0], "another-writers-variants")
	appendToConfig(t, path, "scanIntervalSec: 7200\nupscale:\n  variantsDir: "+yamlStr(inRoot)+"\n")
	if _, err := Load(path); err == nil {
		t.Fatal("premise: the file the other writer left loads")
	}
	ran := 0
	rename := func(c *Config) error { ran++; c.LibraryName = "Renamed"; return nil }
	for i := 1; i <= 2; i++ {
		var outside *ChangedOutsideError
		if err := rc.Update(path, rename); !errors.As(err, &outside) || outside.Kind != OutsideLeftUnloadable {
			t.Fatalf("save %d over a file another writer left that does not load = %v, want an OutsideLeftUnloadable refusal", i, err)
		}
	}
	if ran != 0 {
		t.Errorf("the refused saves ran their change %d times", ran)
	}
	if said := logs.Lines(leftUnloadableMessage); len(said) != 1 || !strings.Contains(said[0], "path="+path) ||
		!strings.Contains(said[0], "err=upscale.variantsDir: must not be under a library root") {
		t.Errorf("said %q, want one line naming the file and, as its err, why the next start would refuse it", said)
	}
	if said := logs.Lines(outsideSettingMessage); len(said) != 0 {
		t.Errorf("said %q of a file the next start would refuse, want no line that its changes take effect then", said)
	}
	if carrying := logs.Carrying("another-writers-variants"); len(carrying) != 0 {
		t.Errorf("a line carries the variants folder another writer set: %q", carrying)
	}
	corrected := filepath.Join(filepath.Dir(path), "Variants")
	replaceInConfig(t, path, yamlStr(inRoot), yamlStr(corrected))
	if err := rc.Update(path, rename); err != nil {
		t.Fatalf("a save once the other writer corrected the variants folder: %v", err)
	}
	if ran != 1 {
		t.Errorf("the save ran its change %d times, want once", ran)
	}
	if after := loadAt(t, path); after.LibraryName != "Renamed" || after.ScanIntervalSec != 7200 || after.Upscale.VariantsDir != corrected {
		t.Errorf("the file loads libraryName %q, scanIntervalSec %d and upscale.variantsDir %q, want the rename and "+
			"the other writer's 7200 and corrected folder", after.LibraryName, after.ScanIntervalSec, after.Upscale.VariantsDir)
	}
	if live := rc.Load(); live.ScanIntervalSec != booted || live.Upscale.VariantsDir != "" {
		t.Errorf("the save published scanIntervalSec %d and upscale.variantsDir %q, want %d and none, the ones serve loaded",
			live.ScanIntervalSec, live.Upscale.VariantsDir, booted)
	}
	if said := logs.Lines(leftUnloadableMessage); len(said) != 1 {
		t.Errorf("said %q once the file loads, want the one line of before", said)
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

// TestAFileWhoseRootsAVariableGivesTakesAnotherWritersComment: the file
// another writer left is loaded as the next start would load it, with this
// process's environment (loadsAtNextStart), whose variables that start reads
// too: a bridge whose library roots a variable gives, its file naming none,
// takes another writer's comment and lands an unrelated save, though the file
// alone, with no root, would not load. B535's round 3 kept the variables in
// that load: read without them, such a file would refuse every save after any
// other write, a comment's included.
func TestAFileWhoseRootsAVariableGivesTakesAnotherWritersComment(t *testing.T) {
	dir := t.TempDir()
	lib := filepath.Join(dir, "Music")
	if err := os.Mkdir(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BRIDGE_LIBRARY_ROOTS", lib)
	path := writeConfigFile(t, dir, "dataDir: "+yamlStr(filepath.Join(dir, "data"))+"\nlibraryName: Home\n")
	rc := bootHolder(t, path)
	if got := rc.Load().LibraryRoots; len(got) != 1 {
		t.Fatalf("premise: the live roots are %q, want the variable's one", got)
	}
	withoutVariables(t, map[string]string{"BRIDGE_LIBRARY_ROOTS": lib}, func() {
		if _, err := Load(path); err == nil {
			t.Fatal("premise: the file loads without the variable that gives its roots")
		}
	})
	appendToConfig(t, path, "# a note\n")
	if err := rc.Update(path, func(c *Config) error { c.LibraryName = "Renamed"; return nil }); err != nil {
		t.Fatalf("a rename after another writer's comment, the roots a variable's: %v", err)
	}
	if got := loadAt(t, path).LibraryName; got != "Renamed" {
		t.Errorf("the file loads libraryName %q, want the rename", got)
	}
}

// TestTheMergedFileIsLoadedWithTheVariablesThisBridgeStartedWith: the file a
// merged save would write is loaded as the next start would load it, with this
// process's environment (refuseMerge's loadsAtNextStart), as the file another
// writer left is (TestAFileWhoseRootsAVariableGivesTakesAnotherWritersComment,
// whose comment records no setting and so merges nothing): a bridge whose
// library roots a variable gives, its file naming none, lands a rename beside
// another writer's change of a setting, which the merged file writes as that
// writer left it, though that file alone, with no root, would not load
// (backlog B974).
func TestTheMergedFileIsLoadedWithTheVariablesThisBridgeStartedWith(t *testing.T) {
	dir := t.TempDir()
	lib := filepath.Join(dir, "Music")
	if err := os.Mkdir(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BRIDGE_LIBRARY_ROOTS", lib)
	path := writeConfigFile(t, dir, "dataDir: "+yamlStr(filepath.Join(dir, "data"))+"\nlibraryName: Home\n")
	rc := bootHolder(t, path)
	appendToConfig(t, path, "scanIntervalSec: 7200\n")
	withoutVariables(t, map[string]string{"BRIDGE_LIBRARY_ROOTS": lib}, func() {
		if _, err := Load(path); err == nil {
			t.Fatal("premise: the other writer's file loads without the variable that gives its roots")
		}
	})
	if err := rc.Update(path, func(c *Config) error { c.LibraryName = "Renamed"; return nil }); err != nil {
		t.Fatalf("a rename beside another writer's change, the roots a variable's: %v", err)
	}
	if after := loadAt(t, path); after.LibraryName != "Renamed" || after.ScanIntervalSec != 7200 {
		t.Errorf("the file loads libraryName %q and scanIntervalSec %d, want the rename and the other writer's 7200",
			after.LibraryName, after.ScanIntervalSec)
	}
}

// TestTheSettingsRecordedWhileTheFileDidNotLoadAreSaidOnceItLoads: the
// settings a turn records while the file another writer left would not load
// at the next start are said by no line while it would not (B535's round 3),
// and each is said once, by the ordinary line, by the turn that finds the file
// loading again, the line being true by then (backlog B974): an edit that
// changes a setting that loads and one that does not (scanIntervalSec, and a
// variants folder inside a library root), two saves it refuses, saying
// nothing of either; that writer's correction of the failing one alone, and
// the save after it, which says each once; then a save over a comment that
// writer added, whose turn finds bytes that differ and load, which says
// neither again. No line carries the value either setting was given.
func TestTheSettingsRecordedWhileTheFileDidNotLoadAreSaidOnceItLoads(t *testing.T) {
	path := envSaveConfig(t, "libraryName: Home\n")
	rc := bootHolder(t, path)
	logs := loggingtest.Record(t)
	inRoot := filepath.Join(rc.Load().LibraryRoots[0], "another-writers-variants")
	appendToConfig(t, path, "scanIntervalSec: 7200\nupscale:\n  variantsDir: "+yamlStr(inRoot)+"\n")
	rename := func(name string) func(*Config) error {
		return func(c *Config) error { c.LibraryName = name; return nil }
	}
	said := func(setting string) int {
		n := 0
		for _, line := range logs.Lines(outsideSettingMessage) {
			if strings.Contains(line, "setting="+setting) {
				n++
			}
		}
		return n
	}
	for i := 1; i <= 2; i++ {
		if err := rc.Update(path, rename("Refused")); !errors.Is(err, ErrChangedOutside) {
			t.Fatalf("save %d over the file another writer left that would not load = %v, want it refused", i, err)
		}
		if lines := logs.Lines(outsideSettingMessage); len(lines) != 0 {
			t.Fatalf("after refused save %d said %q, want nothing of a setting the next start would not load", i, lines)
		}
	}
	corrected := filepath.Join(filepath.Dir(path), "Another Writers Variants")
	replaceInConfig(t, path, yamlStr(inRoot), yamlStr(corrected))
	if err := rc.Update(path, rename("Renamed")); err != nil {
		t.Fatalf("a save once the other writer corrected the failing setting: %v", err)
	}
	for _, setting := range []string{"scanIntervalSec", "upscale.variantsDir"} {
		if n := said(setting); n != 1 {
			t.Errorf("once the file loads, %s was said %d times, want once: %q", setting, n, logs.Lines(outsideSettingMessage))
		}
	}
	appendToConfig(t, path, "# another writer's note\n")
	if err := rc.Update(path, rename("Renamed again")); err != nil {
		t.Fatalf("the save after another writer's comment: %v", err)
	}
	if lines := logs.Lines(outsideSettingMessage); len(lines) != 2 {
		t.Errorf("after the save over another writer's comment, said %q, want the two lines of before and no more", lines)
	}
	if after := loadAt(t, path); after.LibraryName != "Renamed again" || after.ScanIntervalSec != 7200 || after.Upscale.VariantsDir != corrected {
		t.Errorf("the file loads libraryName %q, scanIntervalSec %d and upscale.variantsDir %q, want the last rename and the other writer's values",
			after.LibraryName, after.ScanIntervalSec, after.Upscale.VariantsDir)
	}
	if carrying := logs.Carrying("another-writers-variants", "Another Writers Variants", "7200"); len(carrying) != 0 {
		t.Errorf("a line carries a value another writer set: %q", carrying)
	}
}

// TestASwitchPathOfAnotherTypeIsComparedByValue: an entry of switchEnvPaths
// is compared by what it turns on only where it is what switchOn reads, a
// *bool (backlog B974): a future entry of another type, a bool or a whole
// number here, is compared by value as any setting is, never read as a
// pointer, which panicked (reflect's IsNil of a bool).
func TestASwitchPathOfAnotherTypeIsComparedByValue(t *testing.T) {
	for _, tc := range []struct {
		path string
		set  func(c *Config)
	}{
		{"library.allowDelete", func(c *Config) { c.Library.AllowDelete = true }},
		{"scanIntervalSec", func(c *Config) { c.ScanIntervalSec = 7200 }},
	} {
		switchEnvPaths[tc.path] = true
		t.Cleanup(func() { delete(switchEnvPaths, tc.path) })
		a, b := &Config{}, &Config{}
		tc.set(b)
		got, same, panicked := func() (got, same []string, panicked any) {
			defer func() { panicked = recover() }()
			return settingsChanged(a, b), settingsChanged(a, Clone(a)), nil
		}()
		if panicked != nil || !slices.Equal(got, []string{tc.path}) || len(same) != 0 {
			t.Errorf("with %s listed as an opt-out, a change of it is found as %q and a clone as %q (panicked %v), want it alone and none",
				tc.path, got, same, panicked)
		}
	}
}

// TestEverySettingPathNamesAFieldOfEveryConfigAlike: settingValue answers by
// Config's type alone, never by what a config holds, so a setting
// fileOutside finds in the config a save writes it finds in the one decoded
// from the file too, and its copy has a source wherever it has a destination
// (backlog B974): every setting's yaml path names a field of a zero config and
// of one with every setting set, alike, and a path that names none (a key no
// setting has, one below a leaf) names none in either.
func TestEverySettingPathNamesAFieldOfEveryConfigAlike(t *testing.T) {
	filled := &Config{}
	fillNonZero(reflect.ValueOf(filled).Elem())
	census := settingCensus(reflect.TypeFor[Config](), "", nil)
	paths := []string{"noSuchSetting", "scanIntervalSec.below", "output.player.device.serial.below", "output.noSuchSetting"}
	for _, s := range census {
		paths = append(paths, s.path)
	}
	for _, p := range paths {
		dst, okDst := settingValue(filled, p)
		src, okSrc := settingValue(&Config{}, p)
		if okDst != okSrc || okDst && dst.Type() != src.Type() {
			t.Errorf("%s names a field of a config with every setting set (%v) and of a zero one (%v) unalike", p, okDst, okSrc)
		}
		if want := !strings.Contains(p, "noSuchSetting") && !strings.HasSuffix(p, ".below"); okDst != want {
			t.Errorf("%s names a field: %v, want %v", p, okDst, want)
		}
	}
}

// TestAClashAVariableMakesIsNoNewClashOfASave: the file another writer left
// and the file a save would write are loaded alike, with this process's
// environment, so an OpenHome port clash only a variable makes
// (BRIDGE_LISTEN_ADDRESS on the OpenHome port) is the file left's: unrelated
// saves land beside it, one after another, the holder taking the clashes
// again from each landed save's merged load, and a save that brings another
// listener onto that port is refused for the clash it adds.
func TestAClashAVariableMakesIsNoNewClashOfASave(t *testing.T) {
	t.Setenv("BRIDGE_LISTEN_ADDRESS", ":7792")
	path := envSaveConfig(t, "libraryName: Home\noutput:\n  player:\n    openhome: true\n")
	rc := bootHolder(t, path)
	var clash *OpenHomePortClashError
	if err := rc.Load().OpenHomePortClash(); !errors.As(err, &clash) || clash.Setting != "listenAddress" {
		t.Fatalf("premise: the live config's clash is %v, want the listen address the variable sets", err)
	}
	appendToConfig(t, path, "scanIntervalSec: 7200\n")
	for i, name := range []string{"Renamed", "Renamed again"} {
		if err := rc.Update(path, func(c *Config) error { c.LibraryName = name; return nil }); err != nil {
			t.Fatalf("save %d beside the clash the variable makes: %v", i+1, err)
		}
	}
	err := rc.Update(path, func(c *Config) error { c.AdminAddress = "127.0.0.1:7792"; return nil })
	var outside *ChangedOutsideError
	if !errors.As(err, &outside) || outside.Kind != OutsideUnloadable || !errors.As(outside.Err, &clash) || clash.Setting != "adminAddress" {
		t.Errorf("a save that brings the admin listener onto the OpenHome port = %v, want an OutsideUnloadable refusal over the clash of adminAddress", err)
	}
	if after := loadAt(t, path); after.LibraryName != "Renamed again" || after.ScanIntervalSec != 7200 {
		t.Errorf("the file loads libraryName %q and scanIntervalSec %d, want the second rename and the other writer's 7200",
			after.LibraryName, after.ScanIntervalSec)
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

// TestAFileAnotherWriterLeftUnloadableIsRefusedBeforeItsDeploymentChange: the
// turn refuses a file another writer left that the next start would refuse as
// that file's (OutsideLeftUnloadable) before it refuses a deployment setting
// that writer changed (OutsideDeployment): a deployment mode the next start
// refuses is both, and the refusal says to correct the file, never to restart
// into a start that fails on it. A deployment change the next start takes
// keeps its refusal. Until B535's round 3 the deployment refusal came first.
func TestAFileAnotherWriterLeftUnloadableIsRefusedBeforeItsDeploymentChange(t *testing.T) {
	for _, tc := range []struct {
		name, edit string
		kind       OutsideKind
		// says is what the refusal tells the operator to do.
		says string
	}{
		{"a deployment mode the next start refuses", "deployment:\n  mode: sideways\n", OutsideLeftUnloadable, "correct the file"},
		{"a deployment change the next start takes", "deployment:\n  managedSettings:\n    - libraryName\n", OutsideDeployment,
			"restart the bridge"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := envSaveConfig(t, "libraryName: Home\n")
			rc := bootHolder(t, path)
			appendToConfig(t, path, tc.edit)
			edited, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			ran := false
			err = rc.Update(path, func(c *Config) error { ran = true; c.LibraryName = "Renamed"; return nil })
			var outside *ChangedOutsideError
			if !errors.As(err, &outside) || outside.Kind != tc.kind {
				t.Fatalf("a rename after %s = %v, want a refusal of kind %d", tc.name, err, tc.kind)
			}
			if msg := err.Error(); !strings.Contains(msg, tc.says) || tc.kind == OutsideLeftUnloadable && strings.Contains(msg, "restart") {
				t.Errorf("refusal %q: want it to say %q, and nothing of a restart for a file the next start refuses", msg, tc.says)
			}
			if ran {
				t.Error("the refused rename ran its change")
			}
			if after, _ := os.ReadFile(path); !bytes.Equal(after, edited) {
				t.Errorf("the refused rename wrote the file:\n%s", after)
			}
			if got := rc.Load().LibraryName; got != "Home" {
				t.Errorf("the refused rename stored libraryName %q", got)
			}
		})
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
