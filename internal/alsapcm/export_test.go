package alsapcm

import (
	"bytes"
	"context"
	"runtime/pprof"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/player/format"
)

// This file lends the fake kernel to the tests of the package alsapcm_test,
// the player's backend's (internal/player/playback) among them, which drive
// the real Sink through it: a test in another directory cannot reach this
// package's test files, and an external test package in this one can, but
// only through what this file exports.

// FakeKernel is the fake kernel as an external test drives it: one PCM node
// at card 5 device 0, the DAC that plays only the simulated time a test
// grants (newModel).
type FakeKernel struct {
	k *fakeKernel

	mu        sync.Mutex
	commanded func(SinkConfig, string) // OnCommand's
}

// NewFakeKernel is a fake node with the playback model on and the kernel's
// PAUSE offered, as USB audio does.
func NewFakeKernel(t *testing.T) *FakeKernel { return &FakeKernel{k: newModel(t)} }

// FakeEndpoint is the endpoint the fake node is, before Renumber.
func FakeEndpoint() Endpoint { return testEndpoint }

// Watchdog is how long the fake's tests wait for an event before they call
// it a hang (-watchdog).
func Watchdog() time.Duration { return *watchdog }

// OpenSink opens a sink on the fake node, on the fake's clock: what the
// player's backend takes as its Config.OpenSink.
func (tk *FakeKernel) OpenSink(ctx context.Context, cfg SinkConfig) (*Sink, error) {
	k := tk.k
	return openSink(ctx, cfg, sinkSeams{open: func(ep Endpoint, c Config) (*Device, error) { return open(k, k.now, ep, c) }, now: k.now,
		commanded: func(op string) { tk.onCommand(cfg, op) }})
}

// OnCommand calls f each time a sink opened on the fake has handed its
// writer a command (pause, resume, recover, drain or drop), on the sender's
// goroutine as it begins to wait for the answer: a test learns that a
// command was sent, and that its sender waits on the writer, which the
// kernel may hold in a call. cfg is the sink's, as it was opened.
func (tk *FakeKernel) OnCommand(f func(cfg SinkConfig, op string)) {
	tk.mu.Lock()
	defer tk.mu.Unlock()
	tk.commanded = f
}

func (tk *FakeKernel) onCommand(cfg SinkConfig, op string) {
	tk.mu.Lock()
	f := tk.commanded
	tk.mu.Unlock()
	if f != nil {
		f(cfg, op)
	}
}

// Formats sets what the DAC takes, in place of S16_LE, S24_3LE and S32_LE.
func (tk *FakeKernel) Formats(fs ...format.ALSAFormat) {
	tk.k.inspect(func(k *fakeKernel) {
		k.hw.formats = nil
		for _, f := range fs {
			k.hw.formats = append(k.hw.formats, sampleFormats[f].code)
		}
	})
}

// NoPause takes the kernel's PAUSE away: a pause is by zeros.
func (tk *FakeKernel) NoPause() { tk.k.inspect(func(k *fakeKernel) { k.hw.info &^= infoPause }) }

// Free lets the DAC play as fast as the waits need: no grant is needed.
func (tk *FakeKernel) Free() { tk.k.free() }

// Grant lets the DAC play d more of simulated time; Revoke takes back what
// is left of it.
func (tk *FakeKernel) Grant(d time.Duration) { tk.k.grant(d) }

func (tk *FakeKernel) Revoke() { tk.k.revoke() }

// Hold stops the DAC: what is left of a grant is taken back and Free ends,
// so no wait plays on, one for room or for a drain's end included, until the
// next Grant.
func (tk *FakeKernel) Hold() { tk.k.hold() }

// Play lets the DAC play d and returns once the writer has used it all and
// waits on the DAC for more.
func (tk *FakeKernel) Play(t *testing.T, d time.Duration) { tk.k.play(t, d) }

// Heard is the bytes the DAC consumed, across every open, and Given the
// bytes the kernel was given by every write.
func (tk *FakeKernel) Heard() []byte { return heard(tk.k) }

func (tk *FakeKernel) Given() []byte { return given(tk.k) }

// KernelView is what a test asks of the fake kernel at one moment.
type KernelView struct {
	// Opens and Closes count the node's opens and closes; Open is it open
	// now. Opened are the paths opened, in order.
	Opens, Closes int
	Open          bool
	Opened        []string
	// OpenAt is where each open's bytes begin in Given.
	OpenAt []int
	// Committed is the format the last HW_PARAMS committed, "" before one.
	Committed format.ALSAFormat
	// Appl and Consumed are the frames written and played since the last
	// PREPARE; Total the frames written across every open; GivenBytes the
	// bytes every write gave, across every open.
	Appl, Consumed, Total uint64
	GivenBytes            int
	// State is the stream's state as the kernel names it (RUNNING, PAUSED…).
	State string
	// Starved is a writer waiting on the DAC for time none was granted.
	Starved bool
	// Writes, Prepares, Pauses and Drains count those requests.
	Writes, Prepares, Pauses, Drains int
	// Card is the card the node says it is.
	Card int
}

// view is the fake as a KernelView; k.mu is held.
func (k *fakeKernel) view() KernelView {
	v := KernelView{
		Opens: k.opens, Closes: k.closes, Open: k.isOpen, Opened: slices.Clone(k.opened), OpenAt: slices.Clone(k.openAt),
		Appl: k.appl, Consumed: k.consumed, Total: k.total, GivenBytes: len(k.played),
		State: State(k.state).String(), Starved: starved(k), Card: int(k.info.card),
	}
	if k.committed != nil {
		if f, ok := k.committed.maskOf(hwParamFormat).single(); ok {
			for name, sf := range sampleFormats {
				if sf.code == f {
					v.Committed = name
				}
			}
		}
	}
	for _, c := range k.calls {
		switch c {
		case ioctlWriteIFrames:
			v.Writes++
		case ioctlPrepare:
			v.Prepares++
		case ioctlDrain:
			v.Drains++
		}
	}
	v.Pauses = len(k.pauses)
	return v
}

// View is the fake now.
func (tk *FakeKernel) View() KernelView {
	var v KernelView
	tk.k.inspect(func(k *fakeKernel) { v = k.view() })
	return v
}

// When returns a channel closed once cond holds of the fake, asked after
// every call into it and every grant.
func (tk *FakeKernel) When(cond func(KernelView) bool) <-chan struct{} {
	return tk.k.when(func(k *fakeKernel) bool { return cond(k.view()) })
}

// Await waits for cond under the watchdog.
func (tk *FakeKernel) Await(t *testing.T, what string, cond func(KernelView) bool) {
	t.Helper()
	await(t, tk.When(cond), what)
}

// Renumber makes the node another card's, as a DAC unplugged and plugged in
// again comes back: INFO names card from now on, and its node is
// pcmC<card>D0p.
func (tk *FakeKernel) Renumber(card int) {
	tk.k.inspect(func(k *fakeKernel) { k.info.card = int32(card) })
}

// Busy makes every open refused with EBUSY, as while MPD holds the device,
// until it is called with false.
func (tk *FakeKernel) Busy(on bool) {
	tk.k.inspect(func(k *fakeKernel) {
		k.openErr = nil
		if on {
			k.openErr = errnoEBUSY
		}
	})
}

// Unplug is the device going away: ENODEV to every call from now on.
func (tk *FakeKernel) Unplug() { tk.k.unplug() }

// XrunAt underruns the stream once frames frames have been written in all:
// the DAC ran dry.
func (tk *FakeKernel) XrunAt(frames uint64) {
	tk.k.inspect(func(k *fakeKernel) {
		k.at = append(k.at, fakeAt{frames: frames, do: func(k *fakeKernel) { k.state = stateXrun }})
	})
}

// FailStatusAt makes STATUS fail with an errno no state explains once frames
// frames have been written in all: a device error.
func (tk *FakeKernel) FailStatusAt(frames uint64) {
	tk.k.inspect(func(k *fakeKernel) {
		k.at = append(k.at, fakeAt{frames: frames, do: func(k *fakeKernel) { k.fail[ioctlStatus] = errno(5) }})
	})
}

// ParkNextWrite parks the next WRITEI_FRAMES inside the kernel call until
// release: never released, a kernel call that does not return.
func (tk *FakeKernel) ParkNextWrite() (parked <-chan struct{}, release func()) {
	return tk.k.parkNextWrite()
}

// ParkNextDrain parks the next DRAIN inside the kernel call until release,
// as ParkNextWrite parks a write.
func (tk *FakeKernel) ParkNextDrain() (parked <-chan struct{}, release func()) {
	return tk.k.parkNextDrain()
}

// ParkNextStatus parks the next STATUS inside the kernel call until release,
// as ParkNextWrite parks a write.
func (tk *FakeKernel) ParkNextStatus() (parked <-chan struct{}, release func()) {
	return tk.k.parkNextStatus()
}

// ParkStatusAfterWritten parks the first STATUS after the kernel takes
// frames frames across its opens, before that STATUS reads the pointers.
func (tk *FakeKernel) ParkStatusAfterWritten(frames uint64) (parked <-chan struct{}, release func()) {
	gate, arrived := make(chan struct{}), make(chan struct{})
	tk.k.inspect(func(k *fakeKernel) {
		k.at = append(k.at, fakeAt{frames: frames, do: func(k *fakeKernel) {
			k.statusGate, k.statusParked = gate, arrived
		}})
	})
	var once sync.Once
	return arrived, func() { once.Do(func() { close(gate) }) }
}

// AdvanceUnwatched lets the DAC play d while a kernel call is parked,
// before a wake-up reaches the writer, without waking a poll.
func (tk *FakeKernel) AdvanceUnwatched(d time.Duration) { tk.k.advanceUnwatched(d) }

// PanicAtNextClockRead makes the next read of the fake's clock panic with v:
// a sink's writer is the one goroutine that reads it, so the panic is the
// writer's.
func (tk *FakeKernel) PanicAtNextClockRead(v any) {
	tk.k.atNow(func() bool { return true }, func() { panic(v) })
}

// CheckClosedEach fails the test unless every open of the node was closed
// once and nothing reached it while closed.
func (tk *FakeKernel) CheckClosedEach(t *testing.T) { tk.k.checkClosedEach(t) }

// LogGoroutines records all stacks before a watchdog failure, while the
// stalled session still exists and before cleanup cancels it.
func LogGoroutines(t *testing.T) {
	t.Helper()
	var b bytes.Buffer
	_ = pprof.Lookup("goroutine").WriteTo(&b, 2)
	t.Logf("goroutines at watchdog:\n%s", &b)
}

// HardwareFormats sets literal UAPI codes, without consulting either pack table.
func (tk *FakeKernel) HardwareFormats(codes ...uint32) {
	tk.k.inspect(func(k *fakeKernel) { k.hw.formats = append([]uint32(nil), codes...) })
}

// OpenNativeSink is the real native sink over the same simulated kernel.
func (tk *FakeKernel) OpenNativeSink(ctx context.Context, c DSDConfig) (*DSDSink, error) {
	return openNativeBounded(ctx, c, nativeSeams{open: func(ep Endpoint, c Config) (*Device, error) { return openNative(tk.k, tk.k.now, ep, c) }, now: tk.k.now, at: tk.k.nativeAt})
}

// ParkNextOpen blocks the real opener below negotiation until released.
func (tk *FakeKernel) ParkNextOpen() (<-chan struct{}, func()) {
	entered, gate := make(chan struct{}), make(chan struct{})
	tk.k.inspect(func(k *fakeKernel) { k.openParked, k.openGate = entered, gate })
	var once sync.Once
	return entered, func() { once.Do(func() { close(gate) }) }
}

// ParkNextClose blocks the real native or PCM closer before its acknowledgement.
func (tk *FakeKernel) ParkNextClose() (<-chan struct{}, func()) {
	entered, gate := make(chan struct{}), make(chan struct{})
	tk.k.inspect(func(k *fakeKernel) { k.closeParked, k.closeGate = entered, gate })
	var once sync.Once
	return entered, func() { once.Do(func() { close(gate) }) }
}
