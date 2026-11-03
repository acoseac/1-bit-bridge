package main

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/doctor"
	"github.com/acoseac/1-bit-bridge/internal/player/binding"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
	"github.com/acoseac/1-bit-bridge/internal/usbid"
)

// The GO bar's node, card 5 device 0, the one goBarPlayer's binding names on
// goBarTrees, and the gids of the groups the fake box's database names.
const (
	goBarNode = "/dev/snd/pcmC5D0p"
	gidAudio  = 29
	gidUser   = 1000
)

// dacBox is a Linux box for the player-dac line: what each of its seams
// answers, and what the line asked of it.
type dacBox struct {
	serving    bool
	boot       *config.PlayerDevice
	locateErr  error
	node       dacNode
	statErr    error
	accessErr  error
	creds      dacCreds
	account    []uint32
	accountErr error
	service    *dacService
	noSudo     bool
	groups     map[uint32]string

	mu     sync.Mutex
	asked  []string
	locate int
}

// newDACBox is a box whose GO bar is connected at card 5, its node owned by
// audio, and whose user tester (uid 1000) is in user's group alone, the
// account and the process alike: the node is refused unless a case says
// otherwise.
func newDACBox() *dacBox {
	return &dacBox{
		node:      dacNode{gid: gidAudio, char: true},
		accessErr: syscall.EACCES,
		creds:     dacCreds{euid: 1000, name: "tester", egid: gidUser},
		account:   []uint32{gidUser},
		groups:    map[uint32]string{gidAudio: "audio", gidUser: "tester"},
	}
}

// host is the line's host over b.
func (b *dacBox) host() dacAccessHost {
	h := dacAccessHost{
		goos:    "linux",
		serving: b.serving,
		boot:    b.boot,
		locate: func(_ context.Context, d config.PlayerDevice) (playback.Unit, error) {
			b.mu.Lock()
			b.locate++
			b.mu.Unlock()
			if b.locateErr != nil {
				return playback.Unit{}, b.locateErr
			}
			return playback.Unit{Endpoint: alsapcm.Endpoint{Card: 5, Device: 0}, Label: "card 5, the GO bar"}, nil
		},
		nodes: dacNodeProbe{
			stat: func(path string) (dacNode, error) {
				b.ask("stat " + path)
				return b.node, b.statErr
			},
			access: func(path string) error {
				b.ask("access " + path)
				return b.accessErr
			},
		},
		groupName:     func(gid uint32) string { return b.groups[gid] },
		creds:         func() (dacCreds, error) { return b.creds, nil },
		accountGroups: func(string) ([]uint32, error) { return b.account, b.accountErr },
		sudo:          func() bool { return !b.noSudo },
		slot:          make(chan struct{}, 1),
	}
	if !b.serving {
		h.service = func(string) (dacService, bool) {
			if b.service == nil {
				return dacService{}, false
			}
			return *b.service, true
		}
	}
	return h
}

func (b *dacBox) ask(what string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.asked = append(b.asked, what)
}

// playerDACConfig is a config that asks for the player, bound to the GO bar
// by serial.
func playerDACConfig() *config.Config {
	cfg := &config.Config{DataDir: "/srv/bridge/data"}
	cfg.Output.Player = config.PlayerConfig{Enabled: true, Device: config.PlayerDevice{Transport: "usb", Binding: config.BindingSerial,
		VendorID: "20b1", ProductID: "3009", Serial: usbid.New(goBarSerial)}}
	return cfg
}

// dacLine is the line b's host gives for cfg, failing the test where there is
// none.
func dacLine(t *testing.T, cfg *config.Config, b *dacBox) doctor.Check {
	t.Helper()
	c, ok := playerDACCheck(context.Background(), cfg, b.host())
	if !ok {
		t.Fatal("the player-dac line is missing")
	}
	if c.Name != playerDACCheckName {
		t.Fatalf("the line is named %q", c.Name)
	}
	return c
}

// wantDACLine fails the test unless c has status and says each of says, in
// its summary or its hint, and none of not.
func wantDACLine(t *testing.T, c doctor.Check, status doctor.Status, says, not []string) {
	t.Helper()
	all := c.Summary + "\n" + c.Hint
	if c.Status != status {
		t.Errorf("the line is %s, want %s: %s", c.Status, status, all)
	}
	for _, s := range says {
		if !strings.Contains(all, s) {
			t.Errorf("the line does not say %q:\n%s", s, all)
		}
	}
	for _, s := range not {
		if strings.Contains(all, s) {
			t.Errorf("the line says %q:\n%s", s, all)
		}
	}
}

// The fix as the installer prints it, for tester (uid 1000) and audio.
const (
	dacJoinCommand    = "sudo usermod -aG audio tester"
	dacRestartCommand = "sudo systemctl restart user@1000.service"
)

// TestThePlayersDACLineSaysWhatTheNodeAllows pins the console's line, made
// in the serving process of itself, over each answer the node's probe can
// give (B676): access through the node's group, or an ACL, is ok, saying
// how it was asked and that it is no word on the DAC being free; a node
// refused is a fail by its cause, naming the user, the group and the fix
// (the user not in the group: the usermod and the restart of the user
// manager, the installer's two lines, with what the restart costs; in the
// group but in a process started before: the restart alone; refused despite
// the group: the mode, the ACL or a security module); ENOSYS and EPERM are
// not checked, never ok and never a fail; and the probe is asked of the
// endpoint's own node, the one the player opens.
func TestThePlayersDACLineSaysWhatTheNodeAllows(t *testing.T) {
	for _, tc := range []struct {
		name      string
		box       func(b *dacBox)
		status    doctor.Status
		says, not []string
	}{
		{"open through the group", func(b *dacBox) {
			b.accessErr, b.creds.groups, b.account = nil, []uint32{gidAudio}, []uint32{gidUser, gidAudio}
		}, doctor.OK, []string{"the bridge, as tester may open " + goBarNode + ", the DAC's node, for playback, through the audio group",
			"asked with faccessat2, which opens nothing", "whether the DAC is free, and plays"}, []string{"usermod"}},
		{"open through an ACL", func(b *dacBox) { b.accessErr = nil },
			doctor.OK, []string{"through an ACL or the node's mode, not the audio group"}, []string{"usermod"}},
		{"refused, the user not in the group", nil,
			doctor.Fail, []string{"the player cannot open its DAC: " + goBarNode + ", which the audio group owns, is refused to the bridge, as tester: tester is not in audio, and no ACL grants it access",
				dacJoinCommand + ", then " + dacRestartCommand + ", or reboot", "restart tester's user manager, whose services keep the groups it started with",
				"stops every service of tester's, a desktop or terminal session under it included, so run it from another administrative session"}, nil},
		{"refused, the process older than the user's membership", func(b *dacBox) { b.account = []uint32{gidUser, gidAudio} },
			doctor.Fail, []string{"tester is in audio, which owns " + goBarNode + ", but the bridge started before tester joined it, and keeps the groups it started with",
				dacRestartCommand + ", or reboot"}, []string{"usermod"}},
		{"refused despite the group", func(b *dacBox) { b.creds.groups, b.account = []uint32{gidAudio}, []uint32{gidUser, gidAudio} },
			doctor.Fail, []string{"is refused to the bridge, as tester, though the bridge is in audio, which owns it", "getfacl " + goBarNode}, []string{"usermod", "systemctl"}},
		{"refused, the group with no name", func(b *dacBox) { delete(b.groups, gidAudio) },
			doctor.Fail, []string{"which the audio group owns", dacJoinCommand}, nil},
		{"refused, a group of another name", func(b *dacBox) { b.groups[gidAudio] = "snd" },
			doctor.Fail, []string{"which the snd group owns", "tester is not in snd", "sudo usermod -aG snd tester"}, []string{"audio"}},
		{"refused, on a box with no sudo", func(b *dacBox) { b.noSudo = true },
			doctor.Fail, []string{"run as root: usermod -aG audio tester, then systemctl restart user@1000.service, or reboot"}, []string{"sudo"}},
		{"a kernel without faccessat2", func(b *dacBox) { b.accessErr = syscall.ENOSYS },
			doctor.Warn, []string{"not checked: this kernel has no faccessat2"}, []string{"usermod"}},
		{"a policy that refuses the check", func(b *dacBox) { b.accessErr = syscall.EPERM },
			doctor.Warn, []string{"not checked: a security policy refused the access check on " + goBarNode + " (EPERM)"}, []string{"usermod"}},
		{"a node that goes as it is asked", func(b *dacBox) { b.accessErr = syscall.ENOENT },
			doctor.Warn, []string{"not checked: " + goBarNode + " is not there: the DAC is not connected now"}, nil},
		{"another answer of the check", func(b *dacBox) { b.accessErr = syscall.EIO },
			doctor.Warn, []string{"not checked: the access check on " + goBarNode + " failed: " + syscall.EIO.Error()}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newDACBox()
			b.serving = true
			if tc.box != nil {
				tc.box(b)
			}
			wantDACLine(t, dacLine(t, playerDACConfig(), b), tc.status, tc.says, tc.not)
			if got := strings.Join(b.asked, ", "); got != "stat "+goBarNode+", access "+goBarNode {
				t.Errorf("the line asked %s, want the GO bar's node stat'ed, then its access", got)
			}
		})
	}
}

// TestThePlayersDACLineIsInconclusiveWithoutTheDAC pins what the line says
// when there is no node to ask, never a permission failure (B676): a DAC
// unplugged or powered off, a binding that names more than one, a box whose
// cards cannot be read, a node that is not there or is not a character
// device; nothing of the node is asked where the binding found no unit.
func TestThePlayersDACLineIsInconclusiveWithoutTheDAC(t *testing.T) {
	for _, tc := range []struct {
		name  string
		box   func(b *dacBox)
		says  string
		asked string
	}{
		{"the DAC unplugged", func(b *dacBox) {
			b.locateErr = &playback.UnitError{State: binding.StateDeviceMissing, Reason: "no unit with the bound serial is connected"}
		}, "not checked: the bound DAC is not connected now (device_missing: no unit with the bound serial is connected)", ""},
		{"two DACs the binding names", func(b *dacBox) {
			b.locateErr = &playback.UnitError{State: binding.StateDeviceAmbiguous, Reason: "two units of the bound model are connected"}
		}, "not checked: the bound DAC is not connected now (device_ambiguous: two units of the bound model are connected)", ""},
		{"a box whose cards cannot be read", func(b *dacBox) {
			b.locateErr = &playback.UnitError{Reason: "the box's sound cards could not be read", Err: syscall.EIO}
		}, "not checked: the box's sound cards could not be read: " + syscall.EIO.Error(), ""},
		{"a box not read whole", func(b *dacBox) {
			b.locateErr = &playback.UnitError{Reason: "the box could not be read whole (card 5's devices could not be listed): resolve again"}
		}, "not checked: the box could not be read whole (card 5's devices could not be listed): resolve again", ""},
		{"the node not there", func(b *dacBox) {
			b.statErr = &fs.PathError{Op: "lstat", Path: goBarNode, Err: syscall.ENOENT}
		}, "not checked: " + goBarNode + " is not there: the DAC is not connected now", "stat " + goBarNode},
		{"the node no character device", func(b *dacBox) { b.node.char = false },
			"not checked: " + goBarNode + " is not a character device, which the player opens", "stat " + goBarNode},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, serving := range []bool{true, false} {
				b := newDACBox()
				b.serving = serving
				tc.box(b)
				wantDACLine(t, dacLine(t, playerDACConfig(), b), doctor.Warn, []string{tc.says}, []string{"cannot open its DAC", "usermod"})
				if got := strings.Join(b.asked, ", "); got != tc.asked {
					t.Errorf("serving %v: the line asked %q, want %q", serving, got, tc.asked)
				}
			}
		})
	}
}

// TestBridgeDoctorComparesItsAccessWithTheRunningBridges pins bridge
// doctor's line (B676), which checks its own credentials and reads the
// running bridge's groups beside them: a login that has the node's group
// while the lingering service does not is a warn, with the restart (a fresh
// SSH login passes while the service still fails); the service in the group
// too is ok, and none running is ok, saying it was not compared; a login
// that started before its user joined the group, beside a service that has
// it, is a warn with no restart; run as root, or as another user than the
// service's, it is not checked.
func TestBridgeDoctorComparesItsAccessWithTheRunningBridges(t *testing.T) {
	inAudio := func(b *dacBox) { b.creds.groups, b.account = []uint32{gidAudio}, []uint32{gidUser, gidAudio} }
	for _, tc := range []struct {
		name      string
		box       func(b *dacBox)
		status    doctor.Status
		says, not []string
	}{
		{"open to this login, the running bridge not in the group", func(b *dacBox) {
			inAudio(b)
			b.accessErr = nil
			b.service = &dacService{pid: 4242, euid: 1000, egid: gidUser, groups: []uint32{gidUser}}
		}, doctor.Warn, []string{"tester (this account) may open " + goBarNode + " through the audio group, but the running bridge (pid 4242) is not in audio: it keeps the groups its user manager started with",
			dacRestartCommand}, []string{"usermod"}},
		{"open to this login and the running bridge", func(b *dacBox) {
			inAudio(b)
			b.accessErr = nil
			b.service = &dacService{pid: 4242, euid: 1000, egid: gidUser, groups: []uint32{gidUser, gidAudio}}
		}, doctor.OK, []string{"tester (this account) may open " + goBarNode, "the running bridge (pid 4242) is in audio too"}, nil},
		{"open, no running bridge", func(b *dacBox) { inAudio(b); b.accessErr = nil },
			doctor.OK, []string{"no running bridge was found to compare with"}, nil},
		{"refused to a login older than the membership, the running bridge in the group", func(b *dacBox) {
			b.account = []uint32{gidUser, gidAudio}
			b.service = &dacService{pid: 4242, euid: 1000, egid: gidUser, groups: []uint32{gidAudio}}
		}, doctor.Warn, []string{"this login started before tester joined it; the running bridge (pid 4242) is in audio", "log in again"}, []string{"usermod", "systemctl"}},
		{"refused to a login older than the membership, no running bridge", func(b *dacBox) { b.account = []uint32{gidUser, gidAudio} },
			doctor.Fail, []string{"but this login started before tester joined it", "log in again for this shell; for the bridge's service, restart", dacRestartCommand}, []string{"usermod"}},
		{"refused, the user not in the group", nil,
			doctor.Fail, []string{"is refused to tester (this account): tester is not in audio", dacJoinCommand, dacRestartCommand}, nil},
		{"run as root", func(b *dacBox) { b.creds = dacCreds{euid: 0, name: "root"}; b.accessErr = nil },
			doctor.Warn, []string{"not checked: bridge doctor runs as root, which may open any node", "run bridge doctor as the user the bridge runs as"}, nil},
		{"run as another user than the running bridge", func(b *dacBox) {
			b.accessErr = nil
			b.service = &dacService{pid: 4242, euid: 1001, egid: 1001}
		}, doctor.Warn, []string{"not checked: bridge doctor runs as tester, and the running bridge (pid 4242) as another user (uid 1001)"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newDACBox()
			if tc.box != nil {
				tc.box(b)
			}
			wantDACLine(t, dacLine(t, playerDACConfig(), b), tc.status, tc.says, tc.not)
		})
	}
}

// TestThePlayersDACLineIsLinuxsAndThePlayers pins where the line is: on
// Linux, for a config that asks for the player; none off Linux, none for a
// player off or refused (the player-storage line says why), and, for one
// with no DAC bound, not checked, saying how to bind one; nothing is read for
// any of them.
func TestThePlayersDACLineIsLinuxsAndThePlayers(t *testing.T) {
	b := newDACBox()
	h := b.host()
	for _, goos := range []string{"darwin", "windows"} {
		h.goos = goos
		if c, ok := playerDACCheck(context.Background(), playerDACConfig(), h); ok {
			t.Errorf("%s has a line: %+v", goos, c)
		}
	}
	off := playerDACConfig()
	off.Output.Player.Enabled = false
	refused := playerDACConfig()
	refused.Output.Player.DSDMode = "a mode no release knows"
	for name, cfg := range map[string]*config.Config{"the player off": off, "the player refused": refused} {
		if c, ok := playerDACCheck(context.Background(), cfg, b.host()); ok {
			t.Errorf("%s has a line: %+v", name, c)
		}
	}
	unbound := playerDACConfig()
	unbound.Output.Player.Device = config.PlayerDevice{}
	wantDACLine(t, dacLine(t, unbound, b), doctor.Warn, []string{"not checked: no DAC is bound to the player", "bridge output dac bind"}, nil)
	if b.locate != 0 || len(b.asked) != 0 {
		t.Errorf("the line read the box (%d readings) or a node (%q) where it has nothing to check", b.locate, b.asked)
	}
}

// TestTheConsolesDACLineSaysWhenTheConfigNamesAnotherDAC: the console's
// doctor grades the config's binding, which the next start opens, and says
// so where the running player started with another (B676); the same binding,
// in any spelling hotplug.SameBinding takes, says nothing of it.
func TestTheConsolesDACLineSaysWhenTheConfigNamesAnotherDAC(t *testing.T) {
	const note = "the running player opens the one it started with until the bridge restarts"
	same := playerDACConfig().Output.Player.Device
	same.Transport = ""
	for _, tc := range []struct {
		name string
		boot *config.PlayerDevice
		want bool
	}{
		{"no player started", nil, false},
		{"the binding it started with", &same, false},
		{"another binding", &goBarByModel, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newDACBox()
			b.serving, b.boot = true, tc.boot
			c := dacLine(t, playerDACConfig(), b)
			if got := strings.Contains(c.Summary, note); got != tc.want {
				t.Errorf("the line says the running player's DAC differs: %v, want %v: %s", got, tc.want, c.Summary)
			}
		})
	}
}

// TestThePlayersDACLineReadsTheBoxOnceAtATime: the line's reading of the box
// is bounded, and one at a time: a reading still under way holds the next
// back, which says so, and a reading past its bound is abandoned to return
// on its own, the line saying it could not be read in time; once it returns,
// the next line reads the box again.
func TestThePlayersDACLineReadsTheBoxOnceAtATime(t *testing.T) {
	b := newDACBox()
	b.serving = true
	h := b.host()
	held, letGo := make(chan struct{}), make(chan struct{})
	h.locate = func(ctx context.Context, d config.PlayerDevice) (playback.Unit, error) {
		close(held)
		<-letGo
		return playback.Unit{}, ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan doctor.Check, 1)
	go func() {
		c, _ := playerDACCheck(ctx, playerDACConfig(), h)
		first <- c
	}()
	select {
	case <-held:
	case c := <-first:
		t.Fatalf("the line answered %+v without reading the box", c)
	case <-serveGiveUp(t):
		t.Fatal("the line never read the box")
	}
	wantDACLine(t, dacLineOf(t, h), doctor.Warn, []string{"not checked: an earlier reading of the box's sound cards has not returned yet"}, nil)
	// The first line's caller goes: the line answers without its reading,
	// which goes on holding the slot.
	cancel()
	wantDACLine(t, <-first, doctor.Warn, []string{"not checked: the bound DAC could not be found: " + context.Canceled.Error()}, nil)
	wantDACLine(t, dacLineOf(t, h), doctor.Warn, []string{"an earlier reading of the box's sound cards has not returned yet"}, nil)
	// The reading returns and lets the slot go: a send waits for that.
	close(letGo)
	h.slot <- struct{}{}
	<-h.slot
	h.locate = b.host().locate
	wantDACLine(t, dacLineOf(t, h), doctor.Fail, []string{"tester is not in audio"}, nil)
}

// dacLineOf is the line h gives for playerDACConfig.
func dacLineOf(t *testing.T, h dacAccessHost) doctor.Check {
	t.Helper()
	c, ok := playerDACCheck(context.Background(), playerDACConfig(), h)
	if !ok {
		t.Fatal("the player-dac line is missing")
	}
	return c
}

// TestReadServingBridgeReadsTheRunningBridgesGroups pins bridge doctor's
// reading of the running bridge (B676): its pid file's pid, taken only for a
// process whose command line is a bridge serve, and its effective uid and
// gid and its groups from the status file; a pid file that is not there or
// holds no pid, a process gone or that is no serve, and a status file that
// does not parse, are no running bridge.
func TestReadServingBridgeReadsTheRunningBridgesGroups(t *testing.T) {
	status := "Name:\tbridge\nUmask:\t0022\nState:\tS (sleeping)\nTgid:\t4242\nPid:\t4242\nPPid:\t1734\n" +
		"Uid:\t1000\t1000\t1000\t1000\nGid:\t1000\t1000\t1000\t1000\nFDSize:\t128\nGroups:\t4 24 27 1000 \nNStgid:\t4242\n"
	for _, tc := range []struct {
		name            string
		pid             string
		cmdline, status string
		want            dacService
		running         bool
	}{
		{"a bridge serve", "4242\n", "/home/tester/.local/bin/bridge\x00serve\x00--config\x00/home/tester/.config/1-bit-bridge/bridge.yaml\x00", status,
			dacService{pid: 4242, euid: 1000, egid: 1000, groups: []uint32{4, 24, 27, 1000}}, true},
		{"no supplementary group", "4242", "bridge\x00serve\x00", strings.Replace(status, "Groups:\t4 24 27 1000 \n", "Groups:\t\n", 1),
			dacService{pid: 4242, euid: 1000, egid: 1000}, true},
		{"another command of the bridge", "4242", "bridge\x00doctor\x00", status, dacService{}, false},
		{"no pid", "", "bridge\x00serve\x00", status, dacService{}, false},
		{"a pid that is no number", "a pid", "bridge\x00serve\x00", status, dacService{}, false},
		{"a process gone", "4243", "bridge\x00serve\x00", status, dacService{}, false},
		{"a status with no Uid line", "4242", "bridge\x00serve\x00", strings.Replace(status, "Uid:\t1000\t1000\t1000\t1000\n", "", 1), dacService{}, false},
		{"a group that is no number", "4242", "bridge\x00serve\x00", strings.Replace(status, " 27 ", " x ", 1), dacService{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			proc := filepath.Join(dir, "proc")
			writeTestFile(t, filepath.Join(proc, "4242", "cmdline"), tc.cmdline)
			writeTestFile(t, filepath.Join(proc, "4242", "status"), tc.status)
			pidFile := filepath.Join(dir, "data", "server.pid")
			writeTestFile(t, pidFile, tc.pid)
			got, running := readServingBridge(proc, pidFile)
			if running != tc.running || got.pid != tc.want.pid || got.euid != tc.want.euid || got.egid != tc.want.egid || !slices.Equal(got.groups, tc.want.groups) {
				t.Errorf("read %+v (running %v), want %+v (running %v)", got, running, tc.want, tc.running)
			}
		})
	}
	if _, running := readServingBridge(filepath.Join(t.TempDir(), "proc"), filepath.Join(t.TempDir(), "server.pid")); running {
		t.Error("a data directory with no pid file has a running bridge")
	}
}

// writeTestFile writes body at path, making its directory.
func writeTestFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestBothDoctorsCarryThePlayersDACLine pins the wiring (B676): bridge
// doctor's run (doctorCmd, through doctorDACHost) and the console's doctor
// (adminDoctorRunnerFor, on the serving process's host) each carry the line,
// right after player-storage, resolving the config's binding through the
// player's own binding machinery (playback.BindingLocator over the GO bar's
// trees) to card 5's node, the one stat'ed and asked; bridge doctor's run
// fails on a refused node and exits 1.
func TestBothDoctorsCarryThePlayersDACLine(t *testing.T) {
	needsDirectorySync(t)
	path, data := writeStorageConfig(t)
	appendDACBinding(t, path)
	standInForALinuxBox(t, ext4Storage, nil)
	trees := goBarTrees(t)
	cli, serving := newDACBox(), newDACBox()
	serving.serving = true
	withTrees := func(b *dacBox) dacAccessHost {
		h := b.host()
		h.locate = hostDACAccess(trees).locate
		return h
	}
	prev := doctorDACHost
	doctorDACHost = func() dacAccessHost { return withTrees(cli) }
	t.Cleanup(func() { doctorDACHost = prev })

	var out, errOut bytes.Buffer
	if code := doctorCmd([]string{"--config", path}, &out, &errOut); code != 1 {
		t.Errorf("bridge doctor over a refused node exited %d, want 1:\n%s", code, out.String())
	}
	report := out.String()
	want := "[FAIL] player-dac          the player cannot open its DAC: " + goBarNode + ", which the audio group owns, is refused to tester (this account): tester is not in audio"
	if !strings.Contains(report, want) {
		t.Errorf("bridge doctor printed\n%s\nwant %q", report, want)
	}
	if i, j := strings.Index(report, "player-storage"), strings.Index(report, "player-dac"); i < 0 || j < i {
		t.Errorf("the player-dac line is not after player-storage:\n%s", report)
	}
	if got := strings.Join(cli.asked, ", "); got != "stat "+goBarNode+", access "+goBarNode {
		t.Errorf("bridge doctor asked %s, want card 5's node, which the GO bar's binding resolves to", got)
	}
	var console *doctor.Check
	for _, c := range adminDoctorRunnerFor(path, nil, nil, withTrees(serving))(context.Background()).Checks {
		if c.Name == playerDACCheckName {
			console = &doctor.Check{Name: c.Name, Status: doctor.Status(c.Status), Summary: c.Summary, Hint: c.Hint}
		}
	}
	if console == nil {
		t.Fatal("the console's doctor has no player-dac line")
	}
	wantDACLine(t, *console, doctor.Fail, []string{"is refused to the bridge, as tester: tester is not in audio", dacJoinCommand}, []string{"this account"})
	if data == "" {
		t.Fatal("no data directory")
	}
}

// TestTheDefaultConsoleDoctorChecksTheServingProcessOverTheHostsTrees: the
// console's doctor as serve's tests and a console without its player wire it
// (adminDoctorRunner) carries the line on the serving process's host over
// hostTrees, which this suite points at empty trees: the box's cards cannot
// be read there, so the line is not checked, and no node is asked.
func TestTheDefaultConsoleDoctorChecksTheServingProcessOverTheHostsTrees(t *testing.T) {
	path, _ := writeStorageConfig(t)
	appendDACBinding(t, path)
	standInForALinuxBox(t, ext4Storage, nil)
	h := servingDACHost(hostTrees, nil)
	h.goos = "linux"
	var line *doctor.Check
	for _, c := range adminDoctorRunnerFor(path, nil, nil, h)(context.Background()).Checks {
		if c.Name == playerDACCheckName {
			line = &doctor.Check{Name: c.Name, Status: doctor.Status(c.Status), Summary: c.Summary, Hint: c.Hint}
		}
	}
	if line == nil {
		t.Fatal("the console's doctor has no player-dac line")
	}
	wantDACLine(t, *line, doctor.Warn, []string{"not checked: the box's sound cards could not be read"}, []string{"cannot open its DAC"})
	if opened := nodeOpens(); len(opened) > 0 {
		t.Errorf("the line asked the host's nodes %q", opened)
	}
}

// appendDACBinding binds the GO bar by serial in the config at path, which
// writeStorageConfig wrote with the player on.
func appendDACBinding(t *testing.T, path string) {
	t.Helper()
	body := mustRead(t, path) + "    device:\n      binding: serial\n      vendorId: \"20b1\"\n      productId: \"3009\"\n      serial: " + goBarSerial + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// ext4Storage is a medium the player-storage line takes, for a run whose
// subject is the player-dac line.
var ext4Storage = atomicwrite.Medium{Storage: "ext4"}
