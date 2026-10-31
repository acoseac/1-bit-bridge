package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/doctor"
)

// volumioRefusal is the medium check's refusal on a Volumio box whose data
// partition is not live, as the strict writer gives it (B603).
var volumioRefusal = fmt.Errorf("%w: /data/1-bit-bridge/config/data is on an overlay whose upper layer /mnt/ext/dyn, as the mount table names it, cannot be read here: statfs /mnt/ext/dyn: no such file or directory; and it is not Volumio's data partition, the one upper layer the check takes where no path reaches it: no ext4 superblock of partition 3 (mmcblk0p3) is live (/sys/fs/ext4/mmcblk0p3)", atomicwrite.ErrMediumUnknown)

// TestThePlayersStorageLineSaysWhatTheWritersCheckFound pins the doctor's
// player-storage line (B603, the orchestrator's amendment 8) on each
// outcome: a player the config leaves off has none, on Linux or off it; off
// Linux, a player the config asks for is not checked, ok, and nothing is
// inspected; on Linux, a player the config's own refusal turns off is not
// checked, a warn naming that refusal; a data directory that is not there
// yet is not checked, ok (serve makes it); one this user cannot inspect is
// a warn; storage the writer's check takes is ok, named where the check
// names it (Volumio's data partition), and never "the player will start";
// and storage it refuses says plainly that the player will not start and
// why, a fail with the fix, an overlay bind's its own. The data directory
// inspected is the config's.
func TestThePlayersStorageLineSaysWhatTheWritersCheckFound(t *testing.T) {
	const dataDir = "/data/1-bit-bridge/config/data"
	player := func(change func(*config.Config)) *config.Config {
		cfg := &config.Config{DataDir: dataDir}
		cfg.Output.Player.Enabled = true
		if change != nil {
			change(cfg)
		}
		return cfg
	}
	// notYet is a data directory not made yet, below a directory that is
	// there: the line walks the path the inspection found no entry at, so
	// the case reads the test's own tree, never the host's /data.
	notYet := filepath.Join(t.TempDir(), "data")
	zero := 0
	for _, tc := range []struct {
		name    string
		cfg     *config.Config
		goos    string
		medium  atomicwrite.Medium
		err     error
		none    bool
		status  doctor.Status
		says    []string
		hint    string
		inspect bool
		// dir is the data directory the case's config names, where it is
		// not dataDir.
		dir string
	}{
		{name: "a player left off, on Linux", cfg: &config.Config{DataDir: dataDir}, goos: "linux", none: true},
		{name: "a player left off, off Linux", cfg: &config.Config{DataDir: dataDir}, goos: "darwin", none: true},
		{name: "a player asked for on macOS", cfg: player(nil), goos: "darwin", status: doctor.OK,
			says: []string{"not checked: the bridge's own player runs on Linux alone, and this is darwin"}},
		{name: "a player asked for on Windows", cfg: player(nil), goos: "windows", status: doctor.OK,
			says: []string{"not checked", "Linux alone", "windows"}},
		{name: "a player the config turns off", cfg: player(func(c *config.Config) { c.Output.Player.PauseReleaseSeconds = &zero }), goos: "linux",
			status: doctor.Warn, says: []string{"not checked: the player is off: output.player.pauseReleaseSeconds: must be from 1 to 3600"}, hint: "fix output.player"},
		{name: "a public bridge's player", cfg: player(func(c *config.Config) { c.Deployment.Mode = "public" }), goos: "linux",
			status: doctor.Warn, says: []string{"not checked: the player is off", "a public bridge"}, hint: "fix output.player"},
		{name: "a data directory that is not there yet", cfg: player(func(c *config.Config) { c.DataDir = notYet }), goos: "linux", err: &fs.PathError{Op: "lstat", Path: notYet, Err: syscall.ENOENT},
			status: doctor.OK, says: []string{"not checked: the data directory " + notYet + " does not exist yet; bridge serve makes it"}, inspect: true, dir: notYet},
		{name: "a data directory this user cannot inspect", cfg: player(nil), goos: "linux", err: &fs.PathError{Op: "lstat", Path: dataDir, Err: syscall.EACCES},
			status: doctor.Warn, says: []string{"not checked: lstat " + dataDir + ": permission denied"}, hint: "the user the service runs as", inspect: true},
		{name: "Volumio's data partition", cfg: player(nil), goos: "linux", medium: atomicwrite.Medium{Storage: "Volumio's data partition (mmcblk0p3, ext4)"},
			status: doctor.OK, says: []string{dataDir + " is on Volumio's data partition (mmcblk0p3, ext4); serve checks the rest as it starts the player"}, inspect: true},
		{name: "storage the check takes and does not name", cfg: player(nil), goos: "linux",
			status: doctor.OK, says: []string{dataDir + " is on storage in which the check finds no volatile layer"}, inspect: true},
		{name: "storage the check cannot tell", cfg: player(nil), goos: "linux", medium: atomicwrite.Medium{Err: volumioRefusal},
			status: doctor.Fail, says: []string{"the player will not start: " + volumioRefusal.Error()}, hint: "keeps what is written to it through a reboot", inspect: true},
		{name: "storage that does not outlive a reboot", cfg: player(nil), goos: "linux", medium: atomicwrite.Medium{Err: fmt.Errorf("%w: %s is on tmpfs", atomicwrite.ErrVolatile, dataDir)},
			status: doctor.Fail, says: []string{"the player will not start", dataDir + " is on tmpfs"}, hint: "keeps what is written to it through a reboot", inspect: true},
		{name: "a bind of a directory inside an overlay", cfg: player(nil), goos: "linux", medium: atomicwrite.Medium{Err: fmt.Errorf("%w: %s is on a bind of /data", atomicwrite.ErrOverlayBind, dataDir)},
			status: doctor.Fail, says: []string{"the player will not start", "is on a bind of /data"}, hint: "on the overlay's own mount", inspect: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var asked []string
			inspect := func(root string) (atomicwrite.Medium, error) {
				asked = append(asked, root)
				return tc.medium, tc.err
			}
			c, ok := playerStorageCheck(tc.cfg, tc.goos, inspect)
			switch {
			case tc.none && ok:
				t.Fatalf("a player the config leaves off has the line %+v", c)
			case tc.none:
				return
			case !ok:
				t.Fatal("no player-storage line")
			}
			if c.Name != playerStorageCheckName || c.Status != tc.status {
				t.Errorf("the line is %s %s, want %s %s", c.Name, c.Status, playerStorageCheckName, tc.status)
			}
			for _, s := range tc.says {
				if !strings.Contains(c.Summary, s) {
					t.Errorf("the summary %q does not say %q", c.Summary, s)
				}
			}
			if strings.Contains(c.Summary, "will start") || strings.Contains(c.Summary, "ready") {
				t.Errorf("the summary %q says the player will start", c.Summary)
			}
			if tc.status == doctor.OK && c.Hint != "" || tc.status != doctor.OK && !strings.Contains(c.Hint, tc.hint) {
				t.Errorf("the hint is %q, want one containing %q for a line that is not ok", c.Hint, tc.hint)
			}
			dir := dataDir
			if tc.dir != "" {
				dir = tc.dir
			}
			if want := map[bool]int{true: 1, false: 0}[tc.inspect]; len(asked) != want || want == 1 && asked[0] != dir {
				t.Errorf("the check inspected %v, want the data directory %s %d time(s)", asked, dir, want)
			}
		})
	}
}

// standInForALinuxBox makes the player-storage line read a Linux box whose
// player's strict writer answers medium, err for the data directory, and
// returns what it was asked; production's readers are put back on cleanup.
func standInForALinuxBox(t *testing.T, medium atomicwrite.Medium, err error) *[]string {
	t.Helper()
	var asked []string
	prevGOOS, prevInspect := playerStorageGOOS, inspectPlayerStorage
	playerStorageGOOS = "linux"
	inspectPlayerStorage = func(root string) (atomicwrite.Medium, error) {
		asked = append(asked, root)
		return medium, err
	}
	t.Cleanup(func() { playerStorageGOOS, inspectPlayerStorage = prevGOOS, prevInspect })
	return &asked
}

// writeStorageConfig writes a config asking for the bridge's own player, with
// its data directory, and returns its path and the data directory.
func writeStorageConfig(t *testing.T) (path, data string) {
	t.Helper()
	dir := t.TempDir()
	data = filepath.Join(dir, "data")
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatal(err)
	}
	path = filepath.Join(dir, "bridge.yaml")
	body := "libraryRoots:\n  - " + dir + "\nlistenAddress: \"127.0.0.1:0\"\nadminAddress: \"127.0.0.1:0\"\ndataDir: " + data +
		"\noutput:\n  mpd: off\n  player:\n    enabled: true\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path, data
}

// TestBridgeDoctorNamesThePlayersStorage pins the line through bridge
// doctor's own run (doctorCmd), on a config that asks for the player, on a
// stand-in Linux box: Volumio's data partition is named, ok, the data
// directory the config names being the one inspected; and storage the
// writer's check refuses fails the report, saying the player will not start
// and why, with its fix, and exits 1.
func TestBridgeDoctorNamesThePlayersStorage(t *testing.T) {
	path, data := writeStorageConfig(t)
	asked := standInForALinuxBox(t, atomicwrite.Medium{Storage: "Volumio's data partition (mmcblk0p3, ext4)"}, nil)
	var out, errOut bytes.Buffer
	doctorCmd([]string{"--config", path}, &out, &errOut)
	want := "player-storage      " + data + " is on Volumio's data partition (mmcblk0p3, ext4); serve checks the rest as it starts the player"
	if !strings.Contains(out.String(), want) {
		t.Errorf("bridge doctor printed\n%s\nwant a line ending %q", out.String(), want)
	}
	if len(*asked) != 1 || (*asked)[0] != data {
		t.Errorf("bridge doctor inspected %v, want the config's data directory %s", *asked, data)
	}
	standInForALinuxBox(t, atomicwrite.Medium{Err: volumioRefusal}, nil)
	out.Reset()
	if code := doctorCmd([]string{"--config", path}, &out, &errOut); code != 1 {
		t.Errorf("bridge doctor over storage the writer refuses exited %d, want 1", code)
	}
	for _, s := range []string{"[FAIL] player-storage      the player will not start: " + volumioRefusal.Error(), "↳ put the data directory (dataDir in bridge.yaml) on storage that keeps what is written to it through a reboot"} {
		if !strings.Contains(out.String(), s) {
			t.Errorf("bridge doctor printed\n%s\nwant %q", out.String(), s)
		}
	}
}

// TestTheConsolesDoctorNamesThePlayersStorageAsBridgeDoctorDoes pins the
// console's doctor (adminDoctorRunner, which assembles its Deps as bridge
// doctor does): the same player-storage line, word for word, with the same
// data directory inspected, and none for a config that leaves the player off.
func TestTheConsolesDoctorNamesThePlayersStorageAsBridgeDoctorDoes(t *testing.T) {
	path, data := writeStorageConfig(t)
	for _, medium := range []atomicwrite.Medium{{Storage: "Volumio's data partition (mmcblk0p3, ext4)"}, {Err: volumioRefusal}} {
		asked := standInForALinuxBox(t, medium, nil)
		cli := findCheck(t, doctor.Run(context.Background(), buildDoctorDeps(path)), playerStorageCheckName)
		var console *doctor.Check
		for _, c := range adminDoctorRunner(path, nil, nil)(context.Background()).Checks {
			if c.Name == playerStorageCheckName {
				console = &doctor.Check{Name: c.Name, Status: doctor.Status(c.Status), Summary: c.Summary, Hint: c.Hint}
			}
		}
		switch {
		case console == nil:
			t.Fatal("the console's doctor has no player-storage line")
		case *console != cli:
			t.Errorf("the console's doctor says %+v, bridge doctor %+v", *console, cli)
		}
		if len(*asked) != 2 || (*asked)[0] != data || (*asked)[1] != data {
			t.Errorf("the two doctors inspected %v, want the data directory %s, once each", *asked, data)
		}
	}
	off := strings.Replace(mustRead(t, path), "    enabled: true\n", "    enabled: false\n", 1)
	if err := os.WriteFile(path, []byte(off), 0o600); err != nil {
		t.Fatal(err)
	}
	asked := standInForALinuxBox(t, atomicwrite.Medium{Err: errors.New("asked")}, nil)
	for _, c := range adminDoctorRunner(path, nil, nil)(context.Background()).Checks {
		if c.Name == playerStorageCheckName {
			t.Errorf("the console's doctor carries %+v for a player the config leaves off", c)
		}
	}
	if len(*asked) != 0 {
		t.Errorf("the console's doctor inspected %v for a player the config leaves off", *asked)
	}
}

// mustRead is the file at path.
func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
