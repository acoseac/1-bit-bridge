package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsactl"
	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
	"github.com/acoseac/1-bit-bridge/internal/player/binding"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
)

// Serve's player with the effective DSD policy's work off its supervisor's
// loop (B762): a silent probe the kernel holds, and a refresh held reading the
// bound card's controls, hold nothing of what the supervisor reports, and a
// write of the player's DSD state keeps the data directory's lock.

// kernelReleaseIn writes the kernel release the policy records evidence
// under (sys/kernel/osrelease) into trees' proc tree: a probe's
// driver_accepted is recorded only under one.
func kernelReleaseIn(t *testing.T, trees outputTrees) {
	t.Helper()
	dir := filepath.Join(trees.proc, "sys", "kernel")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "osrelease"), []byte("6.12.47-v8-16k+\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// heldDACProbe is serve's player's backend as production builds it
// (newServePlayerBackend), but for the silent probe, which asks a stand-in
// that opens nothing (TestMain's guard): its first call, when hold is set,
// waits there, the node "open", until the test lets it go, and each call
// answers DSD_U32_BE accepted at DSD64's frame rate and every other candidate
// refused, so what a probe keeps reaches the player's DSD state as the GO
// bar's driver_accepted.
type heldDACProbe struct {
	hold          bool
	came, gate    chan struct{}
	once, letOnce sync.Once
}

func newHeldDACProbe(hold bool) *heldDACProbe {
	return &heldDACProbe{hold: hold, came: make(chan struct{}), gate: make(chan struct{})}
}

func (h *heldDACProbe) backend(cfg playback.Config) (playBackend, error) {
	cfg.Probe = h.probe
	return newServePlayerBackend(cfg)
}

func (h *heldDACProbe) probe(ep alsapcm.Endpoint, c alsapcm.ProbeConfig) (alsapcm.ProbeReport, error) {
	first := false
	h.once.Do(func() {
		first = true
		close(h.came)
	})
	if first && h.hold {
		<-h.gate
	}
	rep := alsapcm.ProbeReport{Endpoint: ep, Opened: true, Results: make([]alsapcm.ProbeResult, len(c.Candidates))}
	for i, cand := range c.Candidates {
		answer := alsapcm.ProbeRefused
		if cand.Format == format.DSDU32BE && cand.RateHz == dsdstate.DSD64/32 {
			answer = alsapcm.ProbeAccepted
		}
		rep.Results[i] = alsapcm.ProbeResult{Candidate: cand, Answer: answer}
	}
	return rep, nil
}

// let lets the held call go.
func (h *heldDACProbe) let() { h.letOnce.Do(func() { close(h.gate) }) }

// await waits for the probe's first call, until the test's deadline.
func (h *heldDACProbe) await(t *testing.T) {
	t.Helper()
	select {
	case <-h.came:
	case <-serveGiveUp(t):
		t.Fatal("the bound DAC was never probed")
	}
}

// goBarProbeEvidence is the driver_accepted the player's DSD state in
// dataDir holds for the GO bar's DSD_U32_BE at DSD64, nil for none.
func goBarProbeEvidence(t *testing.T, dataDir string) *dsdstate.Evidence {
	t.Helper()
	st, _ := dsdStateIn(t, dataDir)
	d, ok := st.DAC(goBarKey(t))
	if !ok {
		return nil
	}
	for i, e := range d.Evidence {
		if e.Transport == dsdstate.TransportNative && e.Format == "DSD_U32_BE" && e.BitRate == dsdstate.DSD64 && !e.DriverAcceptedAt.IsZero() {
			return &d.Evidence[i]
		}
	}
	return nil
}

// TestTheSupervisorAnswersWhileTheSilentProbeIsHeld (B762): the silent probe
// runs on a goroutine of the backend's, so a probe the kernel holds in an open
// or an ioctl (a USB DAC stalling in a control request) holds nothing of
// serve's player's supervisor: the live config turning the player off reaches
// it while the probe is held. Whichever way the supervisor leaves, serve's stop
// or a panic in it, the probe ends with it: once the kernel lets it go nothing
// of it is kept, and since a probe held in the kernel writes nothing of the
// player's DSD state once its supervisor has left, the player's share of the
// data directory's lock is let go without waiting for it.
func TestTheSupervisorAnswersWhileTheSilentProbeIsHeld(t *testing.T) {
	needsDirectorySync(t)
	persistentMedium(t)
	for _, exit := range []string{"serve's stop", "a panic in the supervisor"} {
		t.Run(exit, func(t *testing.T) {
			box := newDSDBox(t)
			goBarDSD(t, box.trees)
			kernelReleaseIn(t, box.trees)
			probe := newHeldDACProbe(true)
			seams := noControlNodes()
			if exit == "a panic in the supervisor" {
				seams.reports = func(rp engine.OutputReport) {
					if rp.Reason == string(binding.StateDisabled) {
						panic("as the supervisor reports the player turned off")
					}
				}
			}
			ps := bootPlayerServe(t, playerServeCase{player: goBarPlayer, seams: seams, bounds: servePlayerBounds{backend: 200 * time.Millisecond},
				with: func(o *serveOpts) {
					box.with(o)
					o.playerBackend = probe.backend
				}})
			// Registered after the boot, so it runs before serve's drain.
			t.Cleanup(probe.let)
			eng := ps.runningEngine(t)
			availableOutput(t, eng)
			probe.await(t)
			ps.livePlayer(t, func(p *config.PlayerConfig) { p.Enabled = false })
			if exit == "serve's stop" {
				awaitEngine(t, eng, "the player turned off, reported while the probe is held", func(s *engine.Snapshot) bool {
					return !s.Output().Available && s.Output().Reason == string(binding.StateDisabled)
				})
				stopped(t, ps)
			} else {
				select {
				case <-ps.player.done:
				case <-serveGiveUp(t):
					t.Fatalf("the player did not stop after its supervisor's panic while the probe was held; stderr=%s", ps.stderr.String())
				}
			}
			select {
			case <-ps.player.settled:
			case <-serveGiveUp(t):
				t.Fatal("the player kept its share of the data directory's lock for a probe held in the kernel, which writes nothing once its supervisor has left")
			}
			probe.let()
			be, ok := ps.player.be.(*playback.Backend)
			if !ok {
				t.Fatalf("serve's player runs %T, want the backend production builds", ps.player.be)
			}
			wctx, cancel := context.WithDeadline(context.Background(), serveGiveUpTime(t))
			defer cancel()
			if err := be.Wait(wctx); err != nil {
				t.Fatal(err)
			}
			if e := goBarProbeEvidence(t, ps.dataDir); e != nil {
				t.Errorf("the player's DSD state holds %+v from a probe its supervisor had left", e)
			}
			if exit != "serve's stop" {
				stopped(t, ps)
			}
		})
	}
}

// heldStateWrites holds every write playback makes of the player's DSD state
// (DSDPolicy.Store's Update: what it observed of the DAC) until the test lets
// them go, as a write whose fsync does not return.
type heldStateWrites struct {
	came, gate    chan struct{}
	once, letOnce sync.Once
}

func (h *heldStateWrites) let() { h.letOnce.Do(func() { close(h.gate) }) }

// heldStore is the policy's store with its writes held.
type heldStore struct {
	playback.DSDStore
	h *heldStateWrites
}

func (s heldStore) Update(fn func(*dsdstate.State) error) error {
	s.h.once.Do(func() { close(s.h.came) })
	<-s.h.gate
	return s.DSDStore.Update(fn)
}

// TestServeKeepsTheDataDirLockWhileTheProbesEvidenceIsWritten (B762): what
// a probe kept is written into the player's DSD state by a write of the
// backend's, counted among its writes (DSDWrites): a write that has not
// returned when serve stops, past the backend's bound, keeps the player's
// share of the data directory's lock, said once, so no other serve takes the
// directory while it may still land; once it lands the share is let go.
func TestServeKeepsTheDataDirLockWhileTheProbesEvidenceIsWritten(t *testing.T) {
	needsDirectorySync(t)
	persistentMedium(t)
	rec := loggingtest.Record(t)
	box := newDSDBox(t)
	goBarDSD(t, box.trees)
	kernelReleaseIn(t, box.trees)
	writes := &heldStateWrites{came: make(chan struct{}), gate: make(chan struct{})}
	seams := noControlNodes()
	controls := seams.dsdPolicy
	seams.dsdPolicy = func(p *playback.DSDPolicy) {
		controls(p)
		p.Store = heldStore{DSDStore: p.Store, h: writes}
	}
	probe := newHeldDACProbe(false)
	ps := bootPlayerServe(t, playerServeCase{player: goBarPlayer, seams: seams, bounds: servePlayerBounds{backend: 200 * time.Millisecond},
		with: func(o *serveOpts) {
			box.with(o)
			o.playerBackend = probe.backend
		}})
	// Registered after the boot, so it runs before serve's drain.
	t.Cleanup(writes.let)
	availableOutput(t, ps.runningEngine(t))
	select {
	case <-writes.came:
	case <-serveGiveUp(t):
		t.Fatal("the probe's driver_accepted never reached the player's DSD state")
	}
	stopped(t, ps)
	release, err := lockServeDataDir(ps.dataDir)
	release()
	var held *serveDataDirHeldError
	if !errors.As(err, &held) {
		t.Fatalf("a second lock of the data directory once serve stopped, a write of the player's DSD state held: %v, want it refused", err)
	}
	if closedNow(ps.player.settled) {
		t.Error("the player settled with a write of its DSD state under way")
	}
	if lines := rec.Failures("player: a write of the player's DSD state did not finish within the stop's bound: the player keeps its share of the data directory's lock until it ends"); len(lines) != 1 {
		t.Errorf("the kept lock was said %q, want once", lines)
	}
	writes.let()
	select {
	case <-ps.player.settled:
	case <-serveGiveUp(t):
		t.Fatal("the player kept its share of the data directory's lock once the write landed")
	}
	release, err = lockServeDataDir(ps.dataDir)
	if err != nil {
		t.Fatalf("the data directory's lock once the write landed: %v", err)
	}
	release()
	if goBarProbeEvidence(t, ps.dataDir) == nil {
		t.Error("the probe's driver_accepted did not land once let go")
	}
}

// heldControlNode is the bound card's control node as the DoP gain check
// reads it (alsactl.Reader's Open): once armed, each open waits until the
// test lets it go, as an open or an ioctl the kernel holds; came is closed as
// the first comes.
type heldControlNode struct {
	armed         atomic.Bool
	came, gate    chan struct{}
	once, letOnce sync.Once
}

func (h *heldControlNode) open(int) (alsactl.Device, error) {
	if h.armed.Load() {
		h.once.Do(func() { close(h.came) })
		<-h.gate
	}
	return qualifyNoControls{}, nil
}

func (h *heldControlNode) let() {
	h.letOnce.Do(func() {
		h.armed.Store(false)
		close(h.gate)
	})
}

// TestTheSupervisorAnswersWhileARefreshIsHeldReadingTheControls (B762): the
// effective policy's refresh runs on a goroutine of the backend's, so a
// reading of the bound card's controls the kernel holds (the control node's
// open, an ioctl) holds nothing of serve's player's supervisor, nor does the
// refreshes' lock the held refresh holds: an unplug the watcher reads and the
// live config turning the player off both reach the engine while it is held.
func TestTheSupervisorAnswersWhileARefreshIsHeldReadingTheControls(t *testing.T) {
	needsDirectorySync(t)
	persistentMedium(t)
	box := newDSDBox(t)
	goBarDSD(t, box.trees)
	node := &heldControlNode{came: make(chan struct{}), gate: make(chan struct{})}
	seams := &servePlayerSeams{dsdPolicy: func(p *playback.DSDPolicy) {
		hb, ok := p.Box.(playback.HostBox)
		if !ok {
			return
		}
		hb.Controls = alsactl.Reader{Open: node.open}
		p.Box = hb
	}}
	probes := newProbeStandIn()
	ps := bootPlayerServe(t, playerServeCase{player: goBarPlayer, seams: seams, with: func(o *serveOpts) {
		box.with(o)
		o.playerBackend = probes.backend
	}})
	// Registered after the boot, so it runs before serve's drain.
	t.Cleanup(node.let)
	eng := ps.runningEngine(t)
	availableOutput(t, eng)
	node.armed.Store(true)
	select {
	case <-node.came:
	case <-serveGiveUp(t):
		t.Fatal("no refresh read the bound card's controls")
	}
	box.unplug(t)
	box.wake()
	awaitEngine(t, eng, "the unplug, reported while a refresh is held", func(s *engine.Snapshot) bool {
		return s.Output().Known && !s.Output().Available
	})
	ps.livePlayer(t, func(p *config.PlayerConfig) { p.Enabled = false })
	awaitEngine(t, eng, "the player turned off, reported while a refresh is held", func(s *engine.Snapshot) bool {
		return !s.Output().Available && s.Output().Reason == string(binding.StateDisabled)
	})
	node.let()
	stopped(t, ps)
}
