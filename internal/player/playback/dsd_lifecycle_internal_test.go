package playback

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsa"
	"github.com/acoseac/1-bit-bridge/internal/alsactl"
	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
	"github.com/acoseac/1-bit-bridge/internal/usbid"
)

const (
	fixtureBCD    = "0100"
	fixtureKernel = "6.12.47-v8-16k+"
)

var fixtureT0 = time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)

func fixtureUSB() alsa.USBIdentity {
	return alsa.USBIdentity{ID: alsa.USBID{Vendor: 0x1234, Product: 0x5678}, Serial: usbid.New("TESTDAC-0001"), Port: "platform-xhci-hcd.0/usb2-1", Interface: 0, DevNum: 2}
}

type fixtureBox struct {
	gone   *atomic.Bool
	broken *atomic.Bool
}

func (fixtureBox) UnitModel(int) (alsa.UnitModel, error) {
	return alsa.UnitModel{USB: fixtureUSB(), Manufacturer: "Test", Product: "Test DAC", BCDDevice: fixtureBCD, Config: 1}, nil
}
func (fixtureBox) CardUnit(int) (alsa.USBIdentity, bool) { return fixtureUSB(), true }
func (b fixtureBox) PlaybackAltsets(int, int) ([]alsa.Altset, error) {
	if b.broken != nil && b.broken.Load() {
		panic("stream read")
	}
	if b.gone != nil && b.gone.Load() {
		return nil, nil
	}
	two, one, two2, three, b24, b32 := 2, 1, 2, 3, 24, 32
	return []alsa.Altset{
		{Interface: &one, Altset: &two2, Formats: []string{"S24_3LE"}, Channels: &two, Rates: []int{44100, 88200, 176400}, Bits: &b24},
		{Interface: &one, Altset: &three, Formats: []string{"SPECIAL", "DSD_U32_BE"}, Channels: &two, Rates: []int{88200, 176400, 352800}, Bits: &b32},
	}, nil
}
func (fixtureBox) USBMixer(int) ([]byte, error) { return nil, errors.New("no table") }
func (fixtureBox) ReadControls(int) (alsactl.Snapshot, error) {
	return alsactl.Snapshot{}, errors.New("no controls")
}

type fixtureStore struct {
	mu sync.Mutex
	st dsdstate.State
}

func (c *fixtureStore) State() dsdstate.State {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.st.Clone()
}

func (c *fixtureStore) Update(fn func(*dsdstate.State) error) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	next := c.st.Clone()
	if err := fn(&next); err != nil {
		return err
	}
	c.st = next
	return nil
}

func nativeFixture(t *testing.T) dsdstate.State {
	t.Helper()
	key, err := dsdstate.KeyOf(fixtureUSB(), "Test DAC")
	if err != nil {
		t.Fatal(err)
	}
	return dsdstate.State{DefaultMode: dsdstate.ModeNative, DACs: []dsdstate.DAC{{
		Key:      key,
		FirstUse: []dsdstate.FirstUse{{Transport: dsdstate.TransportNative, BCDDevice: fixtureBCD, AcknowledgedAt: fixtureT0}},
	}}}
}

type fixtureSink struct{ pos alsapcm.Position }

func (c fixtureSink) Write(context.Context, []byte) (int, error) { panic("no write") }
func (c fixtureSink) Cut(int64) (bool, error)                    { panic("no cut") }
func (c fixtureSink) Pause() error                               { panic("no pause") }
func (c fixtureSink) Resume() error                              { panic("no resume") }
func (c fixtureSink) Drain(context.Context) error                { panic("no drain") }
func (c fixtureSink) Drop() error                                { return nil }
func (c fixtureSink) Close() error                               { return nil }
func (c fixtureSink) Params() alsapcm.Params                     { return alsapcm.Params{} }
func (c fixtureSink) Position() alsapcm.Position                 { return c.pos }
func (c fixtureSink) Changed() <-chan struct{}                   { return nil }

type replans struct {
	engine.Events
	mu  sync.Mutex
	ids []engine.OccurrenceID
}

func (r *replans) Replan(_ engine.SessionID, o engine.OccurrenceID, _ string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ids = append(r.ids, o)
}

func (r *replans) asked() []engine.OccurrenceID {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]engine.OccurrenceID(nil), r.ids...)
}

type plainEvents struct{ engine.Events }

func fixtureSessionBackend(t *testing.T, edit ...func(*DSDPolicy)) (*Backend, *fixtureStore) {
	t.Helper()
	store := &fixtureStore{st: nativeFixture(t)}
	pol := &DSDPolicy{Store: store, Kernel: fixtureKernel, Box: fixtureBox{}, Probes: alsapcm.NewProbeCache(),
		Bound: func() (BoundDAC, bool) { return BoundDAC{Card: 5, Identity: fixtureUSB(), Enumeration: "test/2"}, true },
		Now:   func() time.Time { return fixtureT0 }}
	for _, e := range edit {
		e(pol)
	}
	b, err := New(Config{Resolver: nativeFilesForRefusal{}, Locator: refuseLocator{}, DSD: pol})
	if err != nil {
		t.Fatal(err)
	}
	b.RefreshDSD(true)
	return b, store
}

func openedOcc(b *Backend, id engine.OccurrenceID) *occ {
	done := make(chan struct{})
	close(done)
	cur := b.dsd.Load()
	return &occ{id: id, done: done, admitted: cur, policyPlan: cur.set.At(dsdstate.DSD64).Plan,
		nativeFormat: format.NativeDSD{BitRateHz: dsdstate.DSD64, Channels: 2}}
}
