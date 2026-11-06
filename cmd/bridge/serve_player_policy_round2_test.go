package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsactl"
	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/hotplug"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
	"github.com/acoseac/1-bit-bridge/internal/player/qualification"
)

// Round 2 of B728 B7's review: serve's player probing through a stand-in, a
// probe a DAC's part of the migration forces on the supervisor's tick, and
// output play's trees and its --report run.

// probeStandIn is serve's player's backend as production builds it
// (newServePlayerBackend), but for the silent probe, which asks a stand-in
// that opens nothing and counts each ask: serve's player probes the bound
// DAC once its output is available (B728 B7), and no test may open a PCM node
// (TestMain's guard).
type probeStandIn struct {
	mu      sync.Mutex
	asked   []alsapcm.Endpoint
	changed chan struct{}
}

func newProbeStandIn() *probeStandIn { return &probeStandIn{changed: make(chan struct{})} }

// backend builds serve's player's backend with the stand-in for its probe.
func (p *probeStandIn) backend(cfg playback.Config) (playBackend, error) {
	cfg.Probe = p.probe
	return newServePlayerBackend(cfg)
}

func (p *probeStandIn) probe(ep alsapcm.Endpoint, c alsapcm.ProbeConfig) (alsapcm.ProbeReport, error) {
	p.mu.Lock()
	p.asked = append(p.asked, ep)
	close(p.changed)
	p.changed = make(chan struct{})
	p.mu.Unlock()
	rep := alsapcm.ProbeReport{Endpoint: ep, Results: make([]alsapcm.ProbeResult, len(c.Candidates))}
	for i, cand := range c.Candidates {
		rep.Results[i] = alsapcm.ProbeResult{Candidate: cand, Answer: alsapcm.ProbeNotProbed}
	}
	return rep, errors.New("the test's probe opens no node")
}

// await waits until the probe has been asked n times, until the test's
// deadline.
func (p *probeStandIn) await(t *testing.T, what string, n int) {
	t.Helper()
	for {
		p.mu.Lock()
		asked, ch := len(p.asked), p.changed
		p.mu.Unlock()
		if asked >= n {
			return
		}
		select {
		case <-ch:
		case <-serveGiveUp(t):
			t.Fatalf("waiting for %s: the probe was asked %d times, want %d", what, asked, n)
		}
	}
}

// noControlNodes are serve's player's seams that read the bound card's
// controls, for the DoP gain check, through a reader standing in for the box,
// which has no control node: no test opens one (TestMain's guard).
func noControlNodes() *servePlayerSeams {
	return &servePlayerSeams{dsdPolicy: func(p *playback.DSDPolicy) {
		box, ok := p.Box.(playback.HostBox)
		if !ok {
			return
		}
		box.Controls = alsactl.Reader{Open: func(int) (alsactl.Device, error) { return qualifyNoControls{}, nil }}
		p.Box = box
	}}
}

// TestAProbeTheMigrationForcesOnTheTickRuns (B728 B7 round 2): a DAC's part of
// the migration made on the supervisor's tick, its model read at last with no
// new snapshot behind it, drops the probe's answers and makes a probe due
// (ForgetDSDProbe), and the supervisor runs that probe at the tick, never
// waiting for a snapshot an unchanged DAC does not give: the bound DAC is
// probed once at the snapshot that made the output available, and again once
// its part is made.
func TestAProbeTheMigrationForcesOnTheTickRuns(t *testing.T) {
	needsDirectorySync(t)
	persistentMedium(t)
	box := newDSDBox(t)
	id := goBarLiveIdentity(t, box.trees)
	manufacturer := filepath.Join(box.trees.sysfs, qualifyGoBar, "manufacturer")
	if err := os.Remove(manufacturer); err != nil {
		t.Fatal(err)
	}
	log := watchLog(t)
	probes := newProbeStandIn()
	ps := bootPlayerServe(t, playerServeCase{player: goBarPlayer + "    dsdMode: native\n    dsdQualification: approved.json\n", seams: noControlNodes(),
		before: func(dataDir string) { legacyRecordIn(t, dataDir, id, qualification.Version, nativeAt(dsdstate.DSD64)) },
		with: func(o *serveOpts) {
			box.with(o)
			o.playerBackend = probes.backend
		}})
	availableOutput(t, ps.runningEngine(t))
	probes.await(t, "the probe at the snapshot that made the output available", 1)
	log.await(t, "its model could not be read")
	snap, ok := ps.player.feed.Latest()
	if !ok {
		t.Fatal("the watcher published nothing")
	}
	if err := os.WriteFile(manufacturer, []byte("iFi"), 0o600); err != nil {
		t.Fatal(err)
	}
	log.await(t, "player: the legacy DSD keys were migrated into the player's DSD state", "dac=20b1:3009 GO bar")
	probes.await(t, "the probe the DAC's part forced", 2)
	if now, _ := ps.player.feed.Latest(); now.Generation() != snap.Generation() {
		t.Errorf("the watcher published generation %d after %d: a reading, not the supervisor's tick, made the part", now.Generation(), snap.Generation())
	}
	stopped(t, ps)
}

// TestOutputPlayReadsTheHostsTreesWhereNoneIsNamed (B728 B7 round 2): output
// play reads the box through the host's trees (hostTrees) when its env names
// none, as serve does (outputTrees.or), for everything it reads of the box:
// the locator's and the watcher's observer, the effective policy's box, and
// the kernel release, which a run in a directory holding a
// sys/kernel/osrelease of its own never takes from there.
func TestOutputPlayReadsTheHostsTreesWhereNoneIsNamed(t *testing.T) {
	c := newPlayCase(t, goBarPlayer)
	cwd := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cwd, "sys", "kernel"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, "sys", "kernel", "osrelease"), []byte("a-stray-release\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(cwd)
	env := c.env("linux")
	env.trees = outputTrees{}
	var wired playback.Config
	env.backend = func(cfg playback.Config) (playBackend, error) {
		wired = cfg
		return nil, errors.New("the test ends the run at its backend")
	}
	stdout, stderr := newPlayBuf(), newPlayBuf()
	if code := runOutputPlay(context.Background(), append([]string{"--config", c.configPath}, c.files...), env, stdout, stderr); code != 1 {
		t.Fatalf("exit %d, want the backend's refusal:\n%s", code, stderr.String())
	}
	pol := wiredPolicyOf(t, wired)
	box, ok := pol.Box.(playback.HostBox)
	if !ok {
		t.Fatalf("the policy reads the box through %T, want the box's trees", pol.Box)
	}
	if box.Sysfs.Root != hostTrees.sysfs || box.Proc.ProcRoot != hostTrees.proc {
		t.Errorf("the policy reads the box at %q and %q, want the host's trees, %q and %q", box.Sysfs.Root, box.Proc.ProcRoot, hostTrees.sysfs, hostTrees.proc)
	}
	if want := kernelRelease(hostTrees); pol.Kernel != want {
		t.Errorf("the kernel release is %q, want the host tree's %q, never the working directory's", pol.Kernel, want)
	}
	if loc, ok := wired.Locator.(playback.BindingLocator); !ok || loc.Observer.Sysfs.Root != hostTrees.sysfs || loc.Observer.Proc.ProcRoot != hostTrees.proc {
		t.Errorf("the locator reads %+v, want the host's trees", wired.Locator)
	}
}

// TestOutputPlayWithAReportRunsPastItsFirstSnapshot (B728 B7 round 2): with
// --report the run keeps no DSD state of its own, the harness's approval
// admission being its DSD, and its watch loop goes on all the same: the first
// snapshot's report, the first track heard, and a quit with no panic.
// playerDSD's resolve and retry, which the loop calls at each snapshot and
// each tick, do nothing on a nil state.
func TestOutputPlayWithAReportRunsPastItsFirstSnapshot(t *testing.T) {
	c := newPlayCase(t, goBarPlayer)
	evidence := t.TempDir()
	contact := qualification.Contact{}
	bundle, err := json.Marshal(contactBundle{Receipts: []contactReceipt{{Contact: contact, SHA256: qualification.Digest(contact)}}})
	if err != nil {
		t.Fatal(err)
	}
	contactPath, mixerPath, report := filepath.Join(evidence, "contact.json"), filepath.Join(evidence, "mixer.txt"), filepath.Join(evidence, "report.json")
	for path, data := range map[string][]byte{contactPath: bundle, mixerPath: []byte("# card=5\n")} {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	c.standIn.hold = true
	pr, pw := io.Pipe()
	c.stdin = pr
	stdout, stderr := newPlayBuf(), newPlayBuf()
	done := make(chan int, 1)
	go func() {
		done <- runOutputPlay(context.Background(), append([]string{"--config", c.configPath, "--keys", "--report", report, "--contact-report", contactPath, "--mixer-snapshot", mixerPath}, c.files...),
			c.env("linux"), stdout, stderr)
	}()
	stdout.waitFor(t, "output report 1: available")
	stdout.waitFor(t, "a.flac) is audible")
	var none *playerDSD
	none.resolve(context.Background(), hotplug.Snapshot{})
	none.retry(context.Background())
	_, _ = pw.Write([]byte("q"))
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("exit %d:\n%s\n%s", code, stdout.String(), stderr.String())
		}
	case <-time.After(playWatch):
		t.Fatalf("output play did not end after q:\n%s\n%s", stdout.String(), stderr.String())
	}
	_ = pw.Close()
	if _, err := os.Stat(report); err != nil {
		t.Errorf("the run wrote no report: %v", err)
	}
}
