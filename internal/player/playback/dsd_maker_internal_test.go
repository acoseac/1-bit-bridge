package playback

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsa"
	"github.com/acoseac/1-bit-bridge/internal/alsa/sysfstest"
	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
	"github.com/acoseac/1-bit-bridge/internal/player/policy"
)

// The backend's writes and checks of a unit's maker (B883): what playback
// observed is recorded for the maker it was observed under, and a node of
// another maker, or whose strings do not read, is refused at the open and at
// the probe's.

// makerBox is fixtureBox whose unit's manufacturer string a test sets.
type makerBox struct {
	fixtureBox
	mu    sync.Mutex
	maker string
}

func (m *makerBox) UnitModel(card int) (alsa.UnitModel, error) {
	u, err := m.fixtureBox.UnitModel(card)
	m.mu.Lock()
	defer m.mu.Unlock()
	u.Manufacturer = m.maker
	return u, err
}

func (m *makerBox) set(maker string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.maker = maker
}

// firstHold holds the first goroutine that reaches it until let: came is
// closed as that one comes, and every later one passes. The one held waits
// outside the Once its release takes, so a test lets it go in its cleanup
// whatever it holds.
type firstHold struct {
	came, gate chan struct{}
	first      atomic.Bool
	letOnce    sync.Once
}

func newFirstHold() *firstHold {
	return &firstHold{came: make(chan struct{}), gate: make(chan struct{})}
}

func (h *firstHold) reach() {
	if h.first.CompareAndSwap(false, true) {
		close(h.came)
		<-h.gate
	}
}

func (h *firstHold) let() { h.letOnce.Do(func() { close(h.gate) }) }

// heldStore is a fixtureStore whose first Update waits until the test lets
// it go (firstHold).
type heldStore struct {
	fixtureStore
	*firstHold
}

func newHeldStore(st dsdstate.State) *heldStore {
	return &heldStore{fixtureStore: fixtureStore{st: st}, firstHold: newFirstHold()}
}

func (h *heldStore) Update(fn func(*dsdstate.State) error) error {
	h.reach()
	return h.fixtureStore.Update(fn)
}

// waitFor waits for ch to close, 30 s at most.
func waitFor(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(30 * time.Second):
		t.Fatalf("waited 30 s for %s", what)
	}
}

// joined waits until no goroutine of b's runs (Wait), 30 s at most: what a
// write or a probe's end began is done.
func joined(t *testing.T, b *Backend) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := b.Wait(ctx); err != nil {
		t.Fatal(err)
	}
}

// probeAccepted reports whether d holds what the probe accepted here
// (acceptingProbe), and that alone: DSD_U32_BE at DSD64, driver_accepted
// natively.
func probeAccepted(d dsdstate.DAC) bool {
	return len(d.Evidence) == 1 && d.Evidence[0].Transport == dsdstate.TransportNative && d.Evidence[0].BitRate == dsdstate.DSD64 &&
		!d.Evidence[0].DriverAcceptedAt.IsZero()
}

// TestWhatPlaybackObservedIsWrittenForTheMakerItWasObservedUnder: each of
// playback's evidence writers (a real open's driver_accepted, five seconds
// streamed with its first use observed, a probe's acceptance) writes into the
// entry of the model and maker its occurrence's set, or its probe, was taken
// for, captured then, never the unit published when the write is made: a
// write held at the store while another maker's unit of the model is bound
// and its set published goes to the first maker's entry; and so does, for
// the two playback writers, a write made once the other maker's set is
// published, from an occurrence admitted under the first's.
func TestWhatPlaybackObservedIsWrittenForTheMakerItWasObservedUnder(t *testing.T) {
	other := dsdstate.StampOf("Other Labs")
	for _, c := range []struct {
		name string
		note func(b *Backend, set *dsdSet, pp policy.Plan)
		// observed reports whether d holds what the note wrote.
		observed func(d dsdstate.DAC) bool
		// occurrence is a playback writer's, from an occurrence that keeps its
		// set past a publish.
		occurrence bool
	}{
		{"a real open's driver_accepted", func(b *Backend, set *dsdSet, pp policy.Plan) { b.noteDriverAccepted(set, pp) },
			func(d dsdstate.DAC) bool { return len(d.Evidence) == 1 && !d.Evidence[0].DriverAcceptedAt.IsZero() }, true},
		{"five seconds streamed and its first use", func(b *Backend, set *dsdSet, pp policy.Plan) { b.noteStreamingObserved(set, pp) },
			func(d dsdstate.DAC) bool {
				return len(d.Evidence) == 1 && !d.Evidence[0].StreamingObservedAt.IsZero() && len(d.FirstUse) == 1 && !d.FirstUse[0].ObservedAt.IsZero()
			}, true},
		{"a probe's acceptance", func(b *Backend, set *dsdSet, _ policy.Plan) {
			b.noteProbed(set.set.Key, set.set.Stamp, set.set.BCDDevice, alsapcm.ProbeReport{Opened: true, Results: []alsapcm.ProbeResult{
				{Candidate: alsapcm.ProbeCandidate{Format: format.DSDU32BE, RateHz: dsdstate.DSD64 / 32}, Answer: alsapcm.ProbeAccepted, Msbits: 32}}})
		}, probeAccepted, false},
	} {
		for _, s := range []struct {
			name string
			// after is the other maker's set published before the write is made.
			after bool
		}{{"its write held across the switch", false}, {"its write made after the switch", true}} {
			if s.after && !c.occurrence {
				continue
			}
			t.Run(c.name+"/"+s.name, func(t *testing.T) {
				box := &makerBox{maker: "Test"}
				store := newHeldStore(nativeFixture(t))
				b, _ := fixtureSessionBackend(t, func(p *DSDPolicy) { p.Box, p.Store = box, store })
				t.Cleanup(store.let)
				first := b.dsd.Load()
				if !first.computed || first.set.Stamp != fixtureStamp {
					t.Fatalf("the first set %+v: the case needs the fixture's maker's", first.set.Identity())
				}
				publishOther := func() {
					box.set("Other Labs")
					b.RefreshDSD(true)
					if now := b.dsd.Load(); now.set.Stamp != other {
						t.Fatalf("the set published is %+v, want the other maker's", now.set.Identity())
					}
				}
				if s.after {
					publishOther()
				}
				c.note(b, first, first.set.At(dsdstate.DSD64).Plan)
				waitFor(t, store.came, "the write")
				if !s.after {
					publishOther()
				}
				store.let()
				joined(t, b)
				st := store.State()
				if d, _ := st.DAC(first.set.Key, fixtureStamp); !c.observed(d) {
					t.Errorf("the fixture's maker's entry holds %+v, want what was observed under its set", d)
				}
				if d, ok := st.DAC(first.set.Key, other); ok {
					t.Errorf("the maker published at the write holds %+v, observed under another's set", d)
				}
			})
		}
	}
}

// locateCardFive locates the bound unit at card 5, with no check after open
// of its own.
type locateCardFive struct{}

func (locateCardFive) Locate(context.Context) (Unit, error) {
	return Unit{Endpoint: alsapcm.Endpoint{Card: 5}}, nil
}

// acceptingProbe is the silent probe as the kernel answers it here
// (Config.Probe): the node at ep opened, the check after open asked of it,
// and, once it passes, DSD_U32_BE at DSD64's frame rate accepted and every
// other candidate refused; a node the check refuses is asked nothing, as
// alsapcm's probe does.
func acceptingProbe(ep alsapcm.Endpoint, c alsapcm.ProbeConfig) (alsapcm.ProbeReport, error) {
	rep := alsapcm.ProbeReport{Endpoint: ep, Opened: true, Results: make([]alsapcm.ProbeResult, len(c.Candidates))}
	for i, cand := range c.Candidates {
		rep.Results[i] = alsapcm.ProbeResult{Candidate: cand, Answer: alsapcm.ProbeNotProbed}
	}
	if c.Admit != nil {
		if err := c.Admit(alsapcm.NodeInfo{Card: ep.Card, Device: ep.Device, Playback: true}); err != nil {
			return rep, &alsapcm.Error{Reason: alsapcm.ReasonWrongDevice, Op: "probe", Endpoint: ep, Err: err}
		}
	}
	for i, cand := range c.Candidates {
		rep.Results[i].Answer = alsapcm.ProbeRefused
		if cand.Format == format.DSDU32BE && cand.RateHz == dsdstate.DSD64/32 {
			rep.Results[i].Answer = alsapcm.ProbeAccepted
		}
	}
	return rep, nil
}

// probingBackend is a backend over box whose silent probe the kernel
// answers here (acceptingProbe) at card 5, the fixture's DAC bound there,
// its state the fixture's in memory, seams set, and its set published.
func probingBackend(t *testing.T, box DSDBox, seams *Seams) (*Backend, *fixtureStore) {
	t.Helper()
	store := &fixtureStore{st: nativeFixture(t)}
	pol := &DSDPolicy{Store: store, Kernel: fixtureKernel, Box: box, Probes: alsapcm.NewProbeCache(),
		Bound: func() (BoundDAC, bool) { return BoundDAC{Card: 5, Identity: fixtureUSB(), Enumeration: "test/2"}, true },
		Now:   func() time.Time { return fixtureT0 }}
	b, err := New(Config{Resolver: nativeFilesForRefusal{}, Locator: locateCardFive{}, DSD: pol, Probe: acceptingProbe, Seams: seams})
	if err != nil {
		t.Fatal(err)
	}
	b.RefreshDSD(true)
	return b, store
}

// probeOn runs b's silent probe as the supervisor asks it (ProbeDSD, its
// refresh after it included) on a goroutine of its own, and answers a
// channel closed as it returns.
func probeOn(b *Backend) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.ProbeDSD(context.Background(), ProbeConditions{Available: true, Fresh: true})
	}()
	return done
}

// TestAProbesAcceptanceIsWrittenForTheMakerItWasBegunFor: what the silent
// probe accepted (ProbeDSD) is written into the entry of the model and maker
// of the unit the probe was begun for, as the box read it then, never the
// unit whose set is published as the probe's answers are taken: neither
// another maker's unit of the model bound and its set published once the
// probe's call has returned (held at the Probed seam), nor another maker's set
// published before the probe begins while the box reads the first maker's
// unit, its refresh after the probe publishing the first's again.
func TestAProbesAcceptanceIsWrittenForTheMakerItWasBegunFor(t *testing.T) {
	other := dsdstate.StampOf("Other Labs")
	key, err := dsdstate.KeyOf(fixtureUSB(), "Test DAC")
	if err != nil {
		t.Fatal(err)
	}
	written := func(t *testing.T, st dsdstate.State) {
		t.Helper()
		if d, _ := st.DAC(key, fixtureStamp); !probeAccepted(d) {
			t.Errorf("the fixture's maker's entry holds %+v, want what its probe accepted", d)
		}
		if d, ok := st.DAC(key, other); ok {
			t.Errorf("the other maker's entry holds %+v, which a probe of the fixture's maker's unit accepted", d)
		}
	}
	t.Run("another maker published once the probe's call has returned", func(t *testing.T) {
		box := &makerBox{maker: "Test"}
		probed := newFirstHold()
		b, store := probingBackend(t, box, &Seams{Probed: probed.reach})
		t.Cleanup(probed.let)
		if now := b.dsd.Load(); !now.computed || now.set.Stamp != fixtureStamp {
			t.Fatalf("the set published is %+v: the case needs the fixture's maker's", now.set.Identity())
		}
		done := probeOn(b)
		waitFor(t, probed.came, "the probe's call to return")
		box.set("Other Labs")
		b.RefreshDSD(true)
		if now := b.dsd.Load(); now.set.Stamp != other {
			t.Fatalf("the set published is %+v, want the other maker's", now.set.Identity())
		}
		probed.let()
		waitFor(t, done, "the probe to end")
		joined(t, b)
		written(t, store.State())
	})
	t.Run("another maker published before the probe begins", func(t *testing.T) {
		box := &makerBox{maker: "Other Labs"}
		b, store := probingBackend(t, box, nil)
		if now := b.dsd.Load(); !now.computed || now.set.Stamp != other {
			t.Fatalf("the set published is %+v: the case needs the other maker's", now.set.Identity())
		}
		box.set("Test")
		waitFor(t, probeOn(b), "the probe to end")
		joined(t, b)
		if now := b.dsd.Load(); now.set.Stamp != fixtureStamp {
			t.Errorf("after the probe's refresh the set published is %+v, want the fixture's maker's, the unit the box reads", now.set.Identity())
		}
		written(t, store.State())
	})
}

// goBarBox is the box's own readers (HostBox) over a sysfs tree of the GO
// bar at card 5, with its product, firmware and stream and no manufacturer
// string; at is where its manufacturer string would be, and bd the binding
// resolved to it.
func goBarBox(t *testing.T) (at string, box HostBox, bd BoundDAC) {
	t.Helper()
	const goBar = "sys/devices/platform/axi/1000120000.pcie/1f00200000.usb/xhci-hcd.0/usb1/1-1"
	stream, err := os.ReadFile(filepath.Join("..", "..", "alsa", "testdata", "dsd256-native", "proc", "asound", "card5", "stream0"))
	if err != nil {
		t.Fatal(err)
	}
	root := sysfstest.Build(t, filepath.Join("..", "..", "alsa", "testdata", "sysfs", "pi5-go-bar.txt"))
	for name, text := range map[string]string{
		filepath.Join(root, goBar, "product"):                     "GO bar\n",
		filepath.Join(root, goBar, "bcdDevice"):                   "0123\n",
		filepath.Join(root, "proc", "asound", "card5", "stream0"): string(stream),
	} {
		if err := os.WriteFile(name, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	box = HostBox{Sysfs: alsa.SysfsReader{Root: filepath.Join(root, "sys")}, Proc: alsa.Reader{ProcRoot: filepath.Join(root, "proc")}}
	id, ok := box.Sysfs.Card(5)
	if !ok || id.USB == nil {
		t.Fatal("the GO bar's card does not read")
	}
	return filepath.Join(root, goBar, "manufacturer"), box, BoundDAC{Card: 5, Identity: *id.USB, Enumeration: "test/2"}
}

// TestTheOpenAndTheProbeRefuseANodeOfAnotherMakerOrWhoseStringsDoNotRead: the
// check after open refuses a node whose unit is another maker's than the one
// its set was computed for, the model, key and firmware alike, or has no
// manufacturer string where the set's had one, as wrong_device; and the
// probe's check refuses another maker's unit than the one it was begun for.
// A unit whose manufacturer string does not read is refused at both, never
// taken for one of none: through the box's own readers over a sysfs tree
// (HostBox), the unit with no manufacturer string its set was computed for
// is taken at both, and the same unit's string there and not read (a NUL in
// it, or a directory at its name) is refused at both.
func TestTheOpenAndTheProbeRefuseANodeOfAnotherMakerOrWhoseStringsDoNotRead(t *testing.T) {
	box := &makerBox{maker: "Test"}
	b, _ := fixtureSessionBackend(t, func(p *DSDPolicy) { p.Box = box })
	set := b.dsd.Load()
	m, err := box.UnitModel(5)
	if err != nil {
		t.Fatal(err)
	}
	probed := probedUnit(box, m, set.set.Key)
	for _, c := range []struct {
		name, maker string
		wrong       bool
	}{
		{"the same maker", "Test", false},
		{"another maker", "Other Labs", true},
		{"no manufacturer string", "", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			box.set(c.maker)
			defer box.set("Test")
			out := &output{s: &session{b: b}}
			for what, err := range map[string]error{"the open": out.checkDSDNode(alsapcm.NodeInfo{Card: 5}, set), "the probe": probed(alsapcm.NodeInfo{Card: 5})} {
				var ae *alsapcm.Error
				switch {
				case c.wrong && (!errors.As(err, &ae) || ae.Reason != alsapcm.ReasonWrongDevice):
					t.Errorf("%s of a node of %q = %v, want wrong_device", what, c.maker, err)
				case !c.wrong && err != nil:
					t.Errorf("%s of the set's own unit = %v", what, err)
				}
			}
		})
	}
	for _, c := range []struct {
		name string
		lay  func(t *testing.T, at string)
	}{
		{"a manufacturer string with a NUL in it", func(t *testing.T, at string) {
			if err := os.WriteFile(at, []byte("Test\x00\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"a directory at the manufacturer string's name", func(t *testing.T, at string) {
			if err := os.Mkdir(at, 0o755); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			at, box, bd := goBarBox(t)
			b, _ := fixtureSessionBackend(t, func(p *DSDPolicy) {
				p.Box, p.Bound = box, func() (BoundDAC, bool) { return bd, true }
			})
			set := b.dsd.Load()
			if !set.computed || set.set.Stamp != dsdstate.StampOf("") {
				t.Fatalf("the set %+v (%s): the case needs one computed for a unit with no manufacturer string", set.set.Identity(), set.why)
			}
			m, err := box.UnitModel(5)
			if err != nil {
				t.Fatal(err)
			}
			probed := probedUnit(box, m, set.set.Key)
			checks := func() map[string]error {
				out := &output{s: &session{b: b}}
				return map[string]error{"the open": out.checkDSDNode(alsapcm.NodeInfo{Card: 5}, set), "the probe": probed(alsapcm.NodeInfo{Card: 5})}
			}
			for what, err := range checks() {
				if err != nil {
					t.Fatalf("%s of the unit with no manufacturer string = %v, want it taken", what, err)
				}
			}
			c.lay(t, at)
			for what, err := range checks() {
				if err == nil {
					t.Errorf("%s of a unit whose manufacturer string does not read was taken", what)
				}
			}
		})
	}
}
