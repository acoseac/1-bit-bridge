package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/hotplug"
	"github.com/acoseac/1-bit-bridge/internal/player/qualification"
)

// The migration inside serve's start (B728 B6): the legacy keys' warnings,
// once per start; the player never turned off by them; the data directory's
// lock; a stop during the migration; a migration that stalls; and bridge
// doctor, which reads nothing of the state.

// legacyKeyLines are the legacy-key warnings a log holds for key.
func legacyKeyLines(log, key string) []string {
	var out []string
	for _, l := range strings.Split(log, "\n") {
		if strings.Contains(l, "output.player: a legacy DSD key is set") && strings.Contains(l, "key=output.player."+key) {
			out = append(out, l)
		}
	}
	return out
}

// TestLegacyDSDKeysStillLoadAndAreWarnedOnce (B728 B6): a block of the three
// legacy keys loads on every path that reads the config (serve, the console's
// settings and its Player page, bridge output dac inspect, bridge doctor),
// refused by none, and within one start each key is warned of once, in words
// that say it stays in effect until the cutover and claim no migration made,
// however many of those readers validate the config: the scope is the
// start's, and a reader outside it warns of nothing.
func TestLegacyDSDKeysStillLoadAndAreWarnedOnce(t *testing.T) {
	needsDirectorySync(t)
	persistentMedium(t)
	box := newDSDBox(t)
	log := watchLog(t)
	cs := bootConsoleServe(t, playerServeCase{player: goBarPlayer + "    dsdMode: dop\n    allowDoP: true\n    dsdQualification: approved.json\n",
		standIn: newPlayerStandIn(), with: box.with})
	availableOutput(t, cs.runningEngine(t))
	// The console's settings: a read, and a save, which validates the whole
	// config, the player's block with it.
	if code, body := cs.call(t, http.MethodGet, "/api/settings", "", nil); code != http.StatusOK {
		t.Fatalf("GET /api/settings = %d: %s", code, body)
	}
	if code, body := cs.call(t, http.MethodPatch, "/api/settings", `{"libraryName":"Kitchen 2"}`, nil); code != http.StatusOK {
		t.Fatalf("PATCH /api/settings = %d: %s", code, body)
	}
	// The console's Player page: its status, read as the page polls it, and
	// two writes that each validate their clone.
	for range 3 {
		if st := cs.status(t); !st.Configured.Enabled || !st.Running.Started {
			t.Fatalf("the Player page says the player is off: %+v", st)
		}
	}
	cs.change(t, "/api/output/player/openhome", `{"openhome":true}`, "restart")
	cs.change(t, "/api/output/player/openhome", `{"openhome":false}`, "restart")
	// bridge output dac inspect and bridge doctor read the file the console
	// saved, each with no start of its own.
	out, errOut, code := runDAC(t, dacEnv(), "inspect", "--json", "--config", cs.configPath, "--proc-root", box.trees.proc, "--sysfs-root", box.trees.sysfs)
	var rep dacInspectReport
	if code != 0 || json.Unmarshal([]byte(out), &rep) != nil || rep.ConfigProblem != "" || !rep.Player.Enabled || rep.Player.Reason != "" {
		t.Errorf("bridge output dac inspect: exit %d, player %+v, config problem %q: %s", code, rep.Player, rep.ConfigProblem, errOut)
	}
	var so, se bytes.Buffer
	doctorCmd([]string{"--json", "--config", cs.configPath}, &so, &se)
	var report jsonDoctorReport
	if err := json.Unmarshal(so.Bytes(), &report); err != nil {
		t.Fatalf("bridge doctor's report: %v\n%s", err, so.String())
	}
	found := false
	for _, c := range report.Checks {
		if c.Name == "config-file" {
			found = true
			if c.Status != "ok" {
				t.Errorf("bridge doctor's config-file line is %s: %s", c.Status, c.Summary)
			}
		}
	}
	if !found {
		t.Errorf("bridge doctor printed no config-file line: %s", so.String())
	}
	stopped(t, cs.playerServe)
	all := log.all()
	for _, key := range []string{"dsdMode", "allowDoP", "dsdQualification"} {
		lines := legacyKeyLines(all, key)
		if len(lines) != 1 {
			t.Errorf("output.player.%s was warned of %d times in one start, want once:\n%s", key, len(lines), strings.Join(lines, "\n"))
			continue
		}
		if l := lines[0]; !strings.Contains(l, "stays in effect until the player's DSD cutover") || strings.Contains(l, "approved.json") || strings.Contains(l, "was migrated") {
			t.Errorf("the warning of %s: %s; want the transitional words, no value", key, l)
		}
	}
}

// TestAPlayerBlockWithLegacyKeysNeverTurnsThePlayerOff (B728 B6): a player
// block with legacy keys starts the player as it would without them, in each
// value the keys take and over a migration that refuses a record, finds none
// or cannot read the state it would write into, and the config says the
// player is on.
func TestAPlayerBlockWithLegacyKeysNeverTurnsThePlayerOff(t *testing.T) {
	needsDirectorySync(t)
	persistentMedium(t)
	for _, tc := range []struct {
		name, keys string
		before     func(t *testing.T, dataDir string)
	}{
		{"every key", "    dsdMode: native\n    allowDoP: true\n    dsdQualification: approved.json\n", nil},
		{"allowDoP alone", "    allowDoP: false\n", nil},
		{"a record named and no mode", "    dsdQualification: elsewhere.json\n", nil},
		{"auto", "    dsdMode: auto\n", nil},
		{"dop with DoP held back", "    dsdMode: dop\n    allowDoP: false\n", nil},
		{"a record refused", "    dsdMode: native\n    dsdQualification: approved.json\n", func(t *testing.T, dataDir string) {
			legacyRecordIn(t, dataDir, qualification.Identity{Vendor: "20b1", ProductID: "3009"}, qualification.LegacyVersion-1, nativeAt(dsdstate.DSD64))
		}},
		{"a state that does not read", "    dsdMode: native\n", func(t *testing.T, dataDir string) {
			if err := os.WriteFile(filepath.Join(dataDir, dsdstate.FileName), []byte("not a state"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			box := newDSDBox(t)
			log := watchLog(t)
			ps := bootDSD(t, box, tc.keys, func(dataDir string) {
				if tc.before != nil {
					tc.before(t, dataDir)
				}
			})
			if !ps.player.plan.start {
				t.Fatalf("the plan does not start the player: %+v", ps.player.plan)
			}
			availableOutput(t, ps.runningEngine(t))
			stopped(t, ps)
			if strings.Contains(log.all(), "player: not started") || strings.Contains(log.all(), "the player is off") {
				t.Errorf("legacy keys turned the player off:\n%s", log.all())
			}
			cfg, err := config.Load(ps.configPath)
			if err != nil {
				t.Fatal(err)
			}
			if on, why := cfg.PlayerEnabled("linux"); !on || why != nil {
				t.Errorf("the config with %q: PlayerEnabled = %v, %v; want on", tc.keys, on, why)
			}
		})
	}
}

// TestASecondServeRefusedByTheLockMigratesNothing (B728 B6): the state lives
// in the data directory, under its lock, and the migration runs only in the
// serve that holds it: a serve the lock refuses migrates nothing and warns of
// no key, the state left byte for byte as it was.
func TestASecondServeRefusedByTheLockMigratesNothing(t *testing.T) {
	needsDirectorySync(t)
	persistentMedium(t)
	root := t.TempDir()
	music, dataDir := filepath.Join(root, "Music"), filepath.Join(root, "data")
	for _, d := range []string{music, dataDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// A state a first start left, its part for a DAC waiting.
	store, err := dsdstate.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Migrate(dataDir, dsdstate.Legacy{DSDMode: "native"}, nil); err != nil {
		t.Fatal(err)
	}
	before := readBytes(t, filepath.Join(dataDir, dsdstate.FileName))
	holdDataDirLock(t, dataDir)
	configPath := filepath.Join(root, "bridge.yaml")
	text := "libraryRoots:\n  - " + music + "\ndataDir: " + dataDir + "\nadminAddress: 127.0.0.1:0\noutput:\n  mpd: off\n  player:\n" + goBarPlayer + "    dsdMode: off\n"
	if err := os.WriteFile(configPath, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	log := watchLog(t)
	var stderr bytes.Buffer
	code := runServe(context.Background(), serveOpts{configPath: configPath, addrOverride: "127.0.0.1:0", playerGOOS: "linux", playerTrees: goBarTrees(t),
		hotplugSource: func(context.Context) hotplug.Source { return hotplug.PollingSource(hearsNothing) }, playerBackend: newPlayerStandIn().build}, io.Discard, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "stop it first") {
		t.Fatalf("a serve beside one that holds the data dir exited %d: %s", code, stderr.String())
	}
	if after := readBytes(t, filepath.Join(dataDir, dsdstate.FileName)); !bytes.Equal(before, after) {
		t.Errorf("a serve the lock refused changed the state:\n%s\n%s", before, after)
	}
	if lines := migrationLines(log.all()); len(lines) != 0 || len(legacyKeyLines(log.all(), "dsdMode")) != 0 {
		t.Errorf("a serve the lock refused migrated or warned: %s", log.all())
	}
}

// stateFiles are the names the data directory holds that are the state's: the
// file and any file the state's writer stages beside it.
func stateFiles(t *testing.T, dataDir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if n := e.Name(); strings.HasPrefix(n, dsdstate.FileName) || strings.HasPrefix(n, ".player-dsd-") {
			out = append(out, n)
		}
	}
	return out
}

// TestACancelDuringTheMigrationIsARequestedStop (B728 B6; serve_cancel_test's
// rule): serve stopped while a step of the migration is under way, at the
// start's step or at a DAC's part, exits as a requested stop, 0, with nothing
// said failed; the step sees the stop before its write, so the state file is
// absent, or holds its prior committed content, and once the player's share of
// the data directory's lock is let go no file the writer stages is left.
func TestACancelDuringTheMigrationIsARequestedStop(t *testing.T) {
	needsDirectorySync(t)
	persistentMedium(t)
	for _, dacPart := range []bool{false, true} {
		name := "the start's step"
		if dacPart {
			name = "a DAC's part"
		}
		t.Run(name, func(t *testing.T) {
			box := newDSDBox(t)
			log := watchLog(t)
			parked := make(chan struct{})
			var once sync.Once
			seams := &servePlayerSeams{migrates: func(ctx context.Context, b *dsdstate.Bound) {
				if (b != nil) != dacPart {
					return
				}
				once.Do(func() { close(parked) })
				// Held until the player's stop has begun, which ends ctx.
				select {
				case <-ctx.Done():
				case <-serveGiveUp(t):
				}
			}}
			keys := "    dsdMode: off\n"
			var before []byte
			c := playerServeCase{player: goBarPlayer + keys, standIn: newPlayerStandIn(), seams: seams, with: box.with}
			if dacPart {
				keys = "    dsdMode: native\n    dsdQualification: approved.json\n"
				c.player = goBarPlayer + keys
				c.before = func(dataDir string) {
					legacyRecordIn(t, dataDir, goBarLiveIdentity(t, box.trees), qualification.Version, nativeAt(dsdstate.DSD64))
					store, err := dsdstate.Open(dataDir)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := store.Migrate(dataDir, dsdstate.Legacy{DSDMode: "native", DSDQualification: "approved.json"}, nil); err != nil {
						t.Fatal(err)
					}
					before = readBytes(t, filepath.Join(dataDir, dsdstate.FileName))
				}
			}
			ps := bootPlayerServe(t, c)
			select {
			case <-parked:
			case <-serveGiveUp(t):
				t.Fatalf("the migration's step was never under way; stderr=%s", ps.stderr.String())
			}
			if code := ps.stopServe(t); code != 0 {
				t.Fatalf("serve stopped during the migration exited %d, want 0, a requested stop; stderr=%s", code, ps.stderr.String())
			}
			select {
			case <-ps.player.settled:
			case <-serveGiveUp(t):
				t.Fatal("the player never let go of its share of the data directory's lock")
			}
			if lines := migrationLines(log.all()); len(lines) != 0 {
				t.Errorf("a step the stop overtook said %q", lines)
			}
			if strings.Contains(log.all(), "player: not started") || strings.Contains(log.all(), "did not stop within its bound") {
				t.Errorf("a requested stop during the migration was said to fail:\n%s", log.all())
			}
			files := stateFiles(t, ps.dataDir)
			switch {
			case before == nil && len(files) != 0:
				t.Errorf("a stop before the start's write left %v", files)
			case before != nil && !slices.Equal(files, []string{dsdstate.FileName}):
				t.Errorf("a stop before the part's write left %v, want the state file alone", files)
			case before != nil && !bytes.Equal(readBytes(t, filepath.Join(ps.dataDir, dsdstate.FileName)), before):
				t.Errorf("a stop before the part's write changed the state")
			}
			// The lock is free once the share is let go: a serve could take it.
			release, err := lockServeDataDir(ps.dataDir)
			if err != nil {
				t.Fatalf("the data directory's lock is still held: %v", err)
			}
			release()
		})
	}
}

// TestAStalledMigrationDoesNotHoldUpTheLibrary (B728 B6): the migration runs
// on the player's supervisor, so a step that does not return holds the
// player's start and nothing else: the library is served meanwhile, and the
// player runs once the step returns.
func TestAStalledMigrationDoesNotHoldUpTheLibrary(t *testing.T) {
	needsDirectorySync(t)
	persistentMedium(t)
	box := newDSDBox(t)
	parked, release := make(chan struct{}), make(chan struct{})
	seams := &servePlayerSeams{migrates: func(ctx context.Context, b *dsdstate.Bound) {
		if b != nil {
			return
		}
		close(parked)
		select {
		case <-release:
		case <-ctx.Done():
		}
	}}
	ps := bootPlayerServe(t, playerServeCase{player: goBarPlayer + "    dsdMode: off\n", standIn: newPlayerStandIn(), seams: seams, with: box.with})
	select {
	case <-parked:
	case <-serveGiveUp(t):
		t.Fatal("the migration's step was never under way")
	}
	if code := ps.health(t); code != http.StatusOK {
		t.Errorf("GET /v1/health = %d while the migration stalls, want the library served", code)
	}
	if closedNow(ps.player.ready) {
		t.Error("the player ran before its migration's step returned")
	}
	close(release)
	availableOutput(t, ps.runningEngine(t))
	stopped(t, ps)
	if st, there := dsdStateIn(t, ps.dataDir); !there || st.DefaultMode != dsdstate.ModeOff {
		t.Errorf("once the step returned the state is %+v (a file: %v), want the keys migrated", st, there)
	}
}

// TestBridgeDoctorReadsNothingOfThePlayersDSDState (B728 B6): bridge doctor
// stays read-only beside the player's DSD state: with the state absent, and
// with one mid-write (the writer's staged file beside it), it says the same,
// for none of its lines reads the state, and it writes nothing in the data
// directory and takes no lock there.
func TestBridgeDoctorReadsNothingOfThePlayersDSDState(t *testing.T) {
	persistentMedium(t)
	root := t.TempDir()
	music, dataDir := filepath.Join(root, "Music"), filepath.Join(root, "data")
	for _, d := range []string{music, dataDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	configPath := filepath.Join(root, "bridge.yaml")
	text := "libraryRoots:\n  - " + music + "\ndataDir: " + dataDir + "\nadminAddress: 127.0.0.1:0\nlistenAddress: 127.0.0.1:0\noutput:\n  mpd: off\n  player:\n" +
		goBarPlayer + "    dsdMode: native\n    dsdQualification: approved.json\n"
	if err := os.WriteFile(configPath, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	listing := func() []string {
		entries, err := os.ReadDir(dataDir)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, e := range entries {
			out = append(out, e.Name())
		}
		return out
	}
	lines := func() map[string]string {
		var so, se bytes.Buffer
		doctorCmd([]string{"--json", "--config", configPath}, &so, &se)
		var report jsonDoctorReport
		if err := json.Unmarshal(so.Bytes(), &report); err != nil {
			t.Fatalf("bridge doctor's report: %v\n%s", err, so.String())
		}
		out := map[string]string{}
		for _, c := range report.Checks {
			out[c.Name] = c.Status
		}
		return out
	}
	absentBefore := listing()
	absent := lines()
	if absent["config-file"] != "ok" || absent["data-dir"] == "" || len(absent) < 5 {
		t.Fatalf("bridge doctor did not grade the config: %v", absent)
	}
	if after := listing(); !slices.Equal(absentBefore, after) {
		t.Errorf("bridge doctor changed the data directory: %v, then %v", absentBefore, after)
	}
	// A state mid-write: the file a migration left, and a staged file beside
	// it, as the writer makes one before its rename.
	if err := os.WriteFile(filepath.Join(dataDir, dsdstate.FileName), []byte(`{"schemaVersion":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, ".player-dsd-123.tmp"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	midBefore := listing()
	mid := lines()
	if after := listing(); !slices.Equal(midBefore, after) {
		t.Errorf("bridge doctor changed the data directory mid-write: %v, then %v", midBefore, after)
	}
	for name, status := range absent {
		if mid[name] != status {
			t.Errorf("bridge doctor's %s line is %s with the state absent and %s mid-write: it reads the state", name, status, mid[name])
		}
	}
	if len(mid) != len(absent) {
		t.Errorf("bridge doctor printed %d lines with the state absent and %d mid-write", len(absent), len(mid))
	}
	for _, name := range listing() {
		if name == serveLockFileName {
			t.Errorf("bridge doctor made the data directory's lock file")
		}
	}
}
