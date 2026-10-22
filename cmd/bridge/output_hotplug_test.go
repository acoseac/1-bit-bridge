package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/player/binding"
	"github.com/acoseac/1-bit-bridge/internal/player/hotplug"
)

// hotplugServe is a serve a test booted and what it decided about the
// player's hotplug watcher.
type hotplugServe struct {
	svc        *hotplug.Service
	holder     *config.RuntimeConfig
	done       <-chan struct{}
	configPath string
	stop       context.CancelFunc
	exited     <-chan struct{}
	exitCode   <-chan int
	stderr     *safeBuffer
}

// hearsNothing is the event source the boot tests run the watcher on: no
// test opens the kernel's socket.
const hearsNothing = "a boot test hears no kernel"

// bootHotplugServe boots serve on a config whose output block is output,
// standing in for goos, and returns once serve has decided about the
// watcher. The watcher reads the suite's hostTrees, two empty directories.
func bootHotplugServe(t *testing.T, goos, output string) hotplugServe {
	t.Helper()
	root := t.TempDir()
	music, dataDir := filepath.Join(root, "Music"), filepath.Join(root, "data")
	if err := os.MkdirAll(music, 0o755); err != nil {
		t.Fatal(err)
	}
	lan, admin := holdLoopback(t), holdLoopback(t)
	configPath := filepath.Join(root, "bridge.yaml")
	yamlText := "libraryRoots:\n  - " + music + "\ndataDir: " + dataDir + "\nadminAddress: " + admin.addr + "\noutput:\n" + output
	if err := os.WriteFile(configPath, []byte(yamlText), 0o600); err != nil {
		t.Fatal(err)
	}
	type decision struct {
		svc    *hotplug.Service
		holder *config.RuntimeConfig
		done   <-chan struct{}
	}
	boot := make(chan decision, 1)
	opts := serveOpts{
		configPath:    configPath,
		addrOverride:  lan.addr,
		lanListener:   lan.ln,
		adminListener: admin.ln,
		playerGOOS:    goos,
		hotplugSource: func(context.Context) hotplug.Source { return hotplug.PollingSource(hearsNothing) },
		hotplugBoot: func(svc *hotplug.Service, holder *config.RuntimeConfig, done <-chan struct{}) {
			boot <- decision{svc, holder, done}
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	stdout, stderr := &safeBuffer{}, &safeBuffer{}
	exitCode := make(chan int, 1)
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		exitCode <- runServe(ctx, opts, stdout, stderr)
	}()
	drainServeOnCleanup(t, cancel, exited, exitCode, stderr)
	select {
	case d := <-boot:
		return hotplugServe{svc: d.svc, holder: d.holder, done: d.done, configPath: configPath, stop: cancel, exited: exited,
			exitCode: exitCode, stderr: stderr}
	case <-exited:
		t.Fatalf("serve exited with code %d before its hotplug decision; stderr=%s", <-exitCode, stderr.String())
		return hotplugServe{}
	case <-testDeadline(t):
		t.Fatal("serve never decided about the hotplug watcher")
		return hotplugServe{}
	}
}

// waitHotplug waits, on the service's Updates, until its status satisfies
// cond, until the test binary's deadline.
func waitHotplug(t *testing.T, svc *hotplug.Service, what string, cond func(hotplug.Status) bool) hotplug.Status {
	t.Helper()
	for {
		ch := svc.Updates()
		st := svc.Status()
		if cond(st) {
			return st
		}
		select {
		case <-ch:
		case <-testDeadline(t):
			t.Fatalf("waiting for %s: the watcher is at %+v", what, st)
		}
	}
}

// testDeadline is a channel closed at the test binary's deadline, or never.
func testDeadline(t *testing.T) <-chan time.Time {
	d, ok := t.Deadline()
	if !ok {
		return nil
	}
	return time.After(time.Until(d) - 5*time.Second)
}

// stopServe stops a booted serve and requires a clean exit.
func (s hotplugServe) stopServe(t *testing.T) {
	t.Helper()
	s.stop()
	<-s.exited
	if code := <-s.exitCode; code != 0 {
		t.Fatalf("serve exit code %d; stderr=%s", code, s.stderr.String())
	}
}

// TestServeRunsThePlayersWatcherWhileThePlayerIsOn: on Linux, with the
// player enabled, serve runs the hotplug watcher on the event source it is
// handed, reading the trees hostTrees names (in this suite two empty ones,
// whose readings fail and are reported, never the host's), and its
// shutdown stops the watcher and joins its service before serve returns.
func TestServeRunsThePlayersWatcherWhileThePlayerIsOn(t *testing.T) {
	s := bootHotplugServe(t, "linux", "  player:\n    enabled: true\n")
	if s.svc == nil {
		t.Fatal("serve on Linux with the player on started no watcher")
	}
	st := waitHotplug(t, s.svc, "a watch that has read once", func(st hotplug.Status) bool { return st.Running && st.Stats.Reconciles >= 1 })
	if st.Source.Mode != hotplug.ModePolling || st.Source.Reason != hearsNothing {
		t.Errorf("the watcher's source: %+v", st.Source)
	}
	if st.Failure == nil || !strings.Contains(st.Failure.Err, "cards") {
		t.Errorf("a reading of the suite's empty trees did not fail as a box with no ALSA: %+v", st.Failure)
	}
	s.stopServe(t)
	select {
	case <-s.done:
	default:
		t.Fatal("serve returned with the watcher's service still running")
	}
	if st := s.svc.Status(); st.Running {
		t.Errorf("after serve stopped: %+v", st)
	}
}

// TestServeStartsNoPlayerWatcherOffLinux: off Linux, where the player
// never runs, serve starts no watcher, the player enabled or not.
func TestServeStartsNoPlayerWatcherOffLinux(t *testing.T) {
	s := bootHotplugServe(t, "darwin", "  player:\n    enabled: true\n")
	if s.svc != nil || s.done != nil {
		t.Fatalf("serve off Linux started a watcher: %+v", s.svc.Status())
	}
	s.stopServe(t)
}

// TestServeStartsThePlayersWatcherWhenTheConfigTurnsItOn: with the player
// off, the watcher's service runs no watch; turning the player on in the
// live config (the holder every settings write goes through) starts one,
// and turning it off again stops it and publishes the player disabled.
func TestServeStartsThePlayersWatcherWhenTheConfigTurnsItOn(t *testing.T) {
	s := bootHotplugServe(t, "linux", "  mpd: off\n")
	if s.svc == nil {
		t.Fatal("serve on Linux started no service to wait for the player")
	}
	if st := s.svc.Status(); st.Running || st.Stats.Sessions != 0 {
		t.Fatalf("a watch runs with the player off: %+v", st)
	}
	set := func(on bool) {
		t.Helper()
		if err := s.holder.Update(s.configPath, func(c *config.Config) error {
			c.Output.Player.Enabled = on
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	set(true)
	waitHotplug(t, s.svc, "the watch the config started", func(st hotplug.Status) bool { return st.Running && st.Stats.Sessions == 1 })
	set(false)
	waitHotplug(t, s.svc, "the watch the config stopped", func(st hotplug.Status) bool { return !st.Running && st.Stats.Publishes >= 1 })
	if snap, ok := s.svc.Feed().Latest(); !ok || snap.Resolution().State != binding.StateDisabled {
		t.Errorf("after the player was turned off: %v, %+v", ok, snap.Resolution())
	}
	s.stopServe(t)
}
