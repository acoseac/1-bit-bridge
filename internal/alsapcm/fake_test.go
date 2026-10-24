package alsapcm

import (
	"context"
	"flag"
	"slices"
	"sync"
	"testing"
	"time"
	"unsafe"
)

// fakeHW is the hardware behind a fake node: what HW_REFINE narrows to and
// HW_PARAMS chooses from.
type fakeHW struct {
	formats        []uint32 // snd_pcm_format_t codes
	rates          []uint32 // the discrete rates, as a USB DAC lists them
	chMin, chMax   uint32
	bufMin, bufMax uint32 // frames
	perMin, perMax uint32 // frames
	// granule, when not 0, is what every buffer and period size must be a
	// multiple of, so an exact size off it is refused and Open has to fall
	// back to a range.
	granule uint32
	msbits  uint32 // 0: the format's width
	info    uint32
}

// fakeWrite is one scripted answer to WRITEI_FRAMES: take frames of what
// was offered (all of it when take is negative), or fail with err.
type fakeWrite struct {
	take int
	err  error
}

// fakePoll is one scripted answer to poll: revents at once; or, with
// timeout, nothing until the timeout passes on the fake clock; or, with
// block, nothing until the caller's context ends.
type fakePoll struct {
	revents int16
	timeout bool
	block   bool
}

// fakeAt is something that happens to the node once it has been written
// frames frames in all, across every open: a device that goes away
// mid-stream.
type fakeAt struct {
	frames uint64
	do     func(k *fakeKernel)
}

// fakeCond is a condition a test waits for (when): its channel is closed
// the first time the condition holds after a call into the fake kernel.
type fakeCond struct {
	ok func(k *fakeKernel) bool
	ch chan struct{}
}

// fakeKernel is the kernel the tests drive a Device through: one PCM node,
// its hardware (fakeHW), scripted answers to writes, polls and status, and
// a record of every call. Each answer follows what sound/core/pcm_native.c
// does for the request, as far as these tests need it.
//
// With model set, it is also a DAC that plays: the frames written since
// PREPARE are consumed at the committed rate as the fake clock moves, and a
// write takes no more than the buffer has room for (EAGAIN when full). The
// clock moves only in a poll that waits, by the time the wait needs (until
// a period is free, until the buffer runs dry, until a timeout), and only
// as far as the simulated time the test has granted (grant, play): a poll
// that needs more waits for the test's next grant, for its context, or for
// the node to have something to report. So a test decides how much audio
// plays between two of its steps, and nothing depends on how fast the
// machine runs. free lets the waits that need time to finish, for room and
// for a drain's end, take it without a grant; a wait for an error condition
// alone (a paused stream, a writer waiting for frames) still makes no time
// pass by itself, since nothing would make it pass faster in a real system.
// heard is what the DAC consumed, in order, across every open; STATUS
// reports the pointers and the delay as the kernel does, a delay of 0 unless
// the stream runs or drains.
//
// Without model, a poll for no event that nothing has scripted waits until
// its context ends: nothing in this fake raises an error condition on its
// own.
type fakeKernel struct {
	t *testing.T

	mu sync.Mutex

	// The node.
	openErr error
	version uint32
	info    pcmInfo
	hw      fakeHW

	// Scripts.
	fail   map[uint32]error   // a request that fails, every time
	commit func(hw *hwParams) // alters what HW_PARAMS commits
	writes []fakeWrite
	polls  []fakePoll
	states []int32 // STATUS answers, then the stream's own state
	delay  int64
	clock  time.Time
	// entered is closed when a blocking poll starts waiting.
	entered chan struct{}

	// The playback model.
	model     bool
	unlimited bool              // the clock moves freely: no grant is needed
	budget    time.Duration     // simulated time granted and not yet used
	more      chan struct{}     // closed and replaced by each grant and each wake-up
	waiters   []context.Context // the contexts of the polls waiting: for a grant, or, without model, for their context
	onNow     func()            // called once, outside the lock, by the next now
	appl      uint64            // frames written since PREPARE (appl_ptr)
	consumed  uint64            // frames consumed since PREPARE (hw_ptr)
	frac      uint64            // what a part frame of play has used: nanoseconds times the rate
	extra     int64             // the driver's own delay past the buffer (runtime->delay)
	runBytes  []byte            // the bytes written since PREPARE
	heard     []byte            // the bytes the DAC consumed, across every open
	total     uint64            // frames written across every open
	at        []fakeAt
	gone      bool // the device went away: ENODEV to every call, POLLERR to a poll
	conds     []fakeCond
	// writeGate, when set, parks the next WRITEI_FRAMES inside the call,
	// none of its frames taken yet, until the gate is closed, as a driver
	// that holds a write does; writeParked is closed once it has parked
	// (parkNextWrite).
	writeGate, writeParked chan struct{}
	// drainGate and drainParked are the same for the next DRAIN
	// (parkNextDrain), and statusGate and statusParked for the next STATUS
	// (parkNextStatus).
	drainGate, drainParked   chan struct{}
	statusGate, statusParked chan struct{}

	// The record.
	opened     []string
	openAt     []int // len(played) at each open: where each open's bytes begin
	opens      int
	isOpen     bool
	fd         int
	closes     int
	calls      []uint32
	afterClose []uint32
	played     []byte
	state      int32
	committed  *hwParams
	sw         *swParams
	pollEvents []int16
	pollWaits  []time.Duration
	pauses     []uintptr
	epipes     int // writes answered EPIPE: offered to a stream an xrun stopped
}

// newFake returns a fake node for card 5 device 0 on hardware that takes
// S16_LE, S24_3LE and S32_LE at the eight rates the iFi GO bar lists,
// stereo, with ample buffer room, speaking this backend's protocol.
func newFake(t *testing.T) *fakeKernel {
	k := &fakeKernel{
		t:       t,
		version: pcmVersion,
		hw: fakeHW{
			formats: []uint32{formatS16LE, formatS243LE, formatS32LE},
			rates:   []uint32{44100, 48000, 88200, 96000, 176400, 192000, 352800, 384000},
			chMin:   2, chMax: 2,
			bufMin: 64, bufMax: 1 << 20,
			perMin: 16, perMax: 1 << 18,
			info: infoBlockTransfer | infoResume,
		},
		fail:    map[uint32]error{},
		clock:   time.Unix(1_000_000, 0),
		entered: make(chan struct{}),
		more:    make(chan struct{}),
		fd:      -1,
	}
	k.info.card, k.info.device, k.info.subdevice, k.info.stream = 5, 0, 0, streamPlayback
	copy(k.info.id[:], "USB Audio")
	copy(k.info.name[:], "USB Audio")
	return k
}

// newModel is newFake with the playback model on, on hardware that offers
// the kernel's PAUSE, as USB audio does.
func newModel(t *testing.T) *fakeKernel {
	k := newFake(t)
	k.model = true
	k.hw.info |= infoPause | infoBatch
	return k
}

// testEndpoint is the endpoint newFake's node is.
var testEndpoint = Endpoint{Card: 5, Device: 0}

// openFake opens testEndpoint through k.
func openFake(k *fakeKernel, cfg Config) (*Device, error) {
	return open(k, k.now, testEndpoint, cfg)
}

// mustOpen opens testEndpoint through k and fails the test when it cannot.
func mustOpen(t *testing.T, k *fakeKernel, cfg Config) *Device {
	t.Helper()
	d, err := openFake(k, cfg)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return d
}

// now is the fake clock, which only a poll that waits moves. A test that
// sets onNow has it called once, by the next now, outside the lock: the
// sink reads its clock between deciding to wait and starting the wait.
func (k *fakeKernel) now() time.Time {
	k.mu.Lock()
	hook := k.onNow
	k.onNow = nil
	clock := k.clock
	k.mu.Unlock()
	if hook != nil {
		hook()
	}
	return clock
}

// atNow has f called once, outside the lock, by the first now for which
// ready holds; ready runs on the goroutine that reads the clock, which for
// a sink is its writer, so it may look at what the writer keeps.
func (k *fakeKernel) atNow(ready func() bool, f func()) {
	var hook func()
	hook = func() {
		if !ready() {
			k.inspect(func(k *fakeKernel) { k.onNow = hook })
			return
		}
		f()
	}
	k.inspect(func(k *fakeKernel) { k.onNow = hook })
}

func (k *fakeKernel) open(path string) (int, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	defer k.check()
	k.opened = append(k.opened, path)
	if k.openErr != nil {
		return -1, k.openErr
	}
	k.fd = 7
	k.state = stateOpen
	k.isOpen = true
	k.opens++
	k.openAt = append(k.openAt, len(k.played))
	return k.fd, nil
}

func (k *fakeKernel) close(fd int) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	defer k.check()
	if fd != k.fd {
		k.t.Errorf("close(%d), but the node is fd %d", fd, k.fd)
	}
	if !k.isOpen {
		k.t.Errorf("close(%d) of a node that is not open: closed twice", fd)
	}
	k.isOpen = false
	k.closes++
	return nil
}

func (k *fakeKernel) poll(ctx context.Context, fd int, events int16, timeout time.Duration) (int16, error) {
	k.mu.Lock()
	if !k.isOpen {
		k.t.Errorf("poll after close")
	}
	k.pollEvents = append(k.pollEvents, events)
	k.pollWaits = append(k.pollWaits, timeout)
	if len(k.polls) == 0 && k.model {
		rev, err := k.modelPoll(ctx, events, timeout)
		k.check()
		k.mu.Unlock()
		return rev, err
	}
	if len(k.polls) == 0 && events&pollOut == 0 {
		// A wait for an error condition alone: nothing here raises one, so
		// it lasts until its context ends.
		k.enterWait(ctx)
		k.check()
		k.mu.Unlock()
		<-ctx.Done()
		k.mu.Lock()
		k.leaveWait(ctx)
		k.mu.Unlock()
		return 0, ctx.Err()
	}
	step := fakePoll{revents: pollOut}
	if len(k.polls) > 0 {
		step, k.polls = k.polls[0], k.polls[1:]
	}
	switch {
	case step.block:
		k.mu.Unlock()
		close(k.entered)
		<-ctx.Done()
		return 0, ctx.Err()
	case step.timeout:
		if k.model {
			k.advance(timeout)
		} else {
			k.clock = k.clock.Add(timeout)
		}
		k.check()
		k.mu.Unlock()
		return 0, nil
	}
	k.mu.Unlock()
	return step.revents, nil
}

// modelPoll is the playback model's poll, with k.mu held: what the node
// reports now, or after the DAC has played until it has something to
// report, or nothing at the timeout. Each stretch of play uses granted time.
func (k *fakeKernel) modelPoll(ctx context.Context, events int16, timeout time.Duration) (int16, error) {
	// The real poll returns a context that is already done before it waits.
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	deadline := k.clock.Add(timeout)
	for {
		if rev := k.events(events); rev != 0 {
			return rev, nil
		}
		left := deadline.Sub(k.clock)
		if left <= 0 {
			return 0, nil
		}
		step := left
		if need, ok := k.untilEvent(events); ok && need < step {
			step = need
		}
		// free gives time to a wait that needs it to finish, for room or
		// for a drain's end; a wait for an error condition alone waits for
		// a grant, as when time is not free.
		if !k.unlimited || (events&pollOut == 0 && k.state != stateDraining) {
			if k.budget == 0 {
				// Wait for a grant, or for the node to have something to
				// report (a device gone): either way, look again.
				more := k.more
				k.enterWait(ctx)
				k.check()
				k.mu.Unlock()
				select {
				case <-more:
				case <-ctx.Done():
				}
				k.mu.Lock()
				k.leaveWait(ctx)
				// The real poll looks at its wake-up first (linuxSys.poll): a
				// cancel that comes with a grant ends the wait.
				if err := ctx.Err(); err != nil {
					return 0, err
				}
				continue
			}
			step = min(step, k.budget)
			k.budget -= step
		}
		k.advance(step)
	}
}

// events is what the node's poll reports now for a wait on requested, as
// snd_pcm_poll computes it and the poll call filters it: room for a period
// in a stream that plays or fills, an error condition in any other state
// (and on a device gone), and nothing while a drain plays out.
func (k *fakeKernel) events(requested int16) int16 {
	if k.gone {
		return pollErr | requested&pollOut
	}
	switch k.state {
	case statePrepared, stateRunning, statePaused:
		if requested&pollOut != 0 && k.room() >= k.sw.availMin {
			return pollOut
		}
		return 0
	case stateDraining:
		return 0
	}
	return pollErr | requested&pollOut
}

// untilEvent is how long the DAC must play before the node has something
// to report: a period free, or the buffer run dry (an underrun, or a
// drain's end). A paused or filling stream plays nothing.
func (k *fakeKernel) untilEvent(requested int16) (time.Duration, bool) {
	switch k.state {
	case stateRunning:
		if requested&pollOut != 0 {
			return k.timeFor(k.sw.availMin - k.room()), true
		}
		return k.timeFor(k.queued()), true
	case stateDraining:
		return k.timeFor(k.queued()), true
	}
	return 0, false
}

// rateOf is the committed rate.
func (k *fakeKernel) rateOf() uint64 {
	r, _ := k.committed.intervalOf(hwParamRate).exact()
	return uint64(r)
}

// bufferOf is the committed buffer size.
func (k *fakeKernel) bufferOf() uint64 {
	b, _ := k.committed.intervalOf(hwParamBufferSize).exact()
	return uint64(b)
}

// queued is the frames written and not yet consumed.
func (k *fakeKernel) queued() uint64 { return k.appl - k.consumed }

// room is the frames free in the buffer.
func (k *fakeKernel) room() uint64 { return k.bufferOf() - k.queued() }

// timeFor is how long the DAC takes to consume n more frames, rounded up.
func (k *fakeKernel) timeFor(n uint64) time.Duration {
	need := n * uint64(time.Second)
	if need <= k.frac {
		return 0
	}
	r := k.rateOf()
	return time.Duration((need - k.frac + r - 1) / r)
}

// advance moves the fake clock by d with the DAC playing: a running or
// draining stream consumes frames at the rate, and one that runs dry
// underruns (XRUN), or ends its drain (SETUP).
func (k *fakeKernel) advance(d time.Duration) {
	k.clock = k.clock.Add(d)
	if k.state != stateRunning && k.state != stateDraining {
		return
	}
	units := uint64(d)*k.rateOf() + k.frac
	n, frac := units/uint64(time.Second), units%uint64(time.Second)
	if q := k.queued(); n >= q {
		k.consume(q)
		k.frac = 0
		if k.state == stateDraining {
			k.state = stateSetup
		} else {
			k.state = stateXrun
		}
		return
	}
	k.consume(n)
	k.frac = frac
}

// consume plays n queued frames: the DAC hears them.
func (k *fakeKernel) consume(n uint64) {
	fb := uint64(k.frameBytes())
	from := k.consumed * fb
	k.heard = append(k.heard, k.runBytes[from:from+n*fb]...)
	k.consumed += n
}

// check closes the channel of every condition that now holds. k.mu is held.
func (k *fakeKernel) check() {
	kept := k.conds[:0]
	for _, c := range k.conds {
		if c.ok(k) {
			close(c.ch)
		} else {
			kept = append(kept, c)
		}
	}
	k.conds = kept
}

// when returns a channel that is closed once ok holds, evaluated with k.mu
// held after every call into the fake kernel and every grant.
func (k *fakeKernel) when(ok func(k *fakeKernel) bool) <-chan struct{} {
	k.mu.Lock()
	defer k.mu.Unlock()
	ch := make(chan struct{})
	if ok(k) {
		close(ch)
		return ch
	}
	k.conds = append(k.conds, fakeCond{ok: ok, ch: ch})
	return ch
}

// grant lets the DAC play d more of simulated time.
func (k *fakeKernel) grant(d time.Duration) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.budget += d
	close(k.more)
	k.more = make(chan struct{})
	k.check()
}

// revoke takes back the simulated time granted and not yet used: a grant
// sized for one wait, such as one that ends past a release's deadline, that
// a later wait must not play on. A writer with nothing to write plays the
// time it is given, as a DAC plays on, and a leftover grant would run a
// resumed stream dry before the test's Drain reached it.
func (k *fakeKernel) revoke() {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.budget = 0
	k.check()
}

// hold ends free and takes back what is left of a grant: the clock moves
// only by a grant from now on.
func (k *fakeKernel) hold() {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.unlimited, k.budget = false, 0
	k.check()
}

// free lets the clock move as far as every poll needs from now on.
func (k *fakeKernel) free() {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.unlimited = true
	close(k.more)
	k.more = make(chan struct{})
	k.check()
}

// enterWait records, with k.mu held, a poll that starts to wait with ctx;
// leaveWait records that it has stopped.
func (k *fakeKernel) enterWait(ctx context.Context) { k.waiters = append(k.waiters, ctx) }

func (k *fakeKernel) leaveWait(ctx context.Context) {
	if i := slices.Index(k.waiters, ctx); i >= 0 {
		k.waiters = slices.Delete(k.waiters, i, i+1)
	}
}

// starved reports whether a poll is waiting for time with none granted:
// the writer has used every grant and waits on the DAC. A poll whose
// context has ended is on its way out, not waiting: the producer's frames
// end the writer's wait without waiting for it to leave, so a test that
// submits and then looks for the writer's next wait must not find the one
// the frames just ended.
func starved(k *fakeKernel) bool {
	if k.budget != 0 {
		return false
	}
	for _, ctx := range k.waiters {
		if ctx.Err() == nil {
			return true
		}
	}
	return false
}

// play lets the DAC play d of simulated time and returns once the writer
// has used all of it and waits on the DAC for more.
func (k *fakeKernel) play(t *testing.T, d time.Duration) {
	t.Helper()
	k.grant(d)
	await(t, k.when(starved), "the writer to use "+d.String()+" of play and wait for more")
}

// advanceNow moves the fake clock by d with the DAC playing, outside any
// poll: time that passes while no poll asks for it, as when a producer is
// late. A poll that waits looks at the node again afterwards, as the kernel
// wakes its sleepers when a stream's state changes.
func (k *fakeKernel) advanceNow(d time.Duration) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.advance(d)
	close(k.more)
	k.more = make(chan struct{})
	k.check()
}

// advanceUnwatched is advanceNow with no poll woken: the moment before the
// kernel's wake-up reaches a waiting writer, which a command or the
// producer's frames may beat.
func (k *fakeKernel) advanceUnwatched(d time.Duration) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.advance(d)
	k.check()
}

// disconnect is a device that goes away, with k.mu held: every call from
// now on fails as the kernel's shut-down node answers it, and a poll that
// waits for time wakes to report it, as the kernel wakes its sleepers.
func (k *fakeKernel) disconnect() {
	k.gone = true
	k.state = stateDisconnected
	close(k.more)
	k.more = make(chan struct{})
}

// suspend is the system suspending the stream, from a test's goroutine: a
// poll that waits for time wakes to report it.
func (k *fakeKernel) suspend() {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.state = stateSuspended
	close(k.more)
	k.more = make(chan struct{})
	k.check()
}

// unplug is disconnect from a test's goroutine.
func (k *fakeKernel) unplug() {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.disconnect()
	k.check()
}

// parkNextWrite parks the next WRITEI_FRAMES inside the call, before the
// kernel takes any of its frames, and returns a channel closed once it has
// parked and the release that lets it go on. A write that is never released
// is a kernel call that never returns.
func (k *fakeKernel) parkNextWrite() (parked <-chan struct{}, release func()) {
	gate, in := make(chan struct{}), make(chan struct{})
	k.inspect(func(k *fakeKernel) { k.writeGate, k.writeParked = gate, in })
	var once sync.Once
	return in, func() { once.Do(func() { close(gate) }) }
}

// parkNextDrain parks the next DRAIN inside the call, before the kernel
// acts on it, as parkNextWrite parks a write.
func (k *fakeKernel) parkNextDrain() (parked <-chan struct{}, release func()) {
	gate, in := make(chan struct{}), make(chan struct{})
	k.inspect(func(k *fakeKernel) { k.drainGate, k.drainParked = gate, in })
	var once sync.Once
	return in, func() { once.Do(func() { close(gate) }) }
}

// parkNextStatus parks the next STATUS inside the call, as parkNextWrite
// parks a write.
func (k *fakeKernel) parkNextStatus() (parked <-chan struct{}, release func()) {
	gate, in := make(chan struct{}), make(chan struct{})
	k.inspect(func(k *fakeKernel) { k.statusGate, k.statusParked = gate, in })
	var once sync.Once
	return in, func() { once.Do(func() { close(gate) }) }
}

// inspect runs f with k.mu held, for a test that reads the fake while a
// sink's writer may be calling it.
func (k *fakeKernel) inspect(f func(k *fakeKernel)) {
	k.mu.Lock()
	defer k.mu.Unlock()
	f(k)
}

// watchdog is how long await waits before it calls the wait a hang.
var watchdog = flag.Duration("watchdog", time.Minute, "how long a test waits for an event before it fails as hung")

// await waits for ch. The timer is a watchdog that turns a hang into a
// failure (-watchdog, a minute); nothing waits on it to let the code
// under test run.
func await(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	receive(t, ch, what)
}

// receive waits for a value on ch, under await's watchdog.
func receive[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	timer := time.NewTimer(*watchdog)
	defer timer.Stop()
	select {
	case v := <-ch:
		return v
	case <-timer.C:
		LogGoroutines(t)
		t.Fatalf("waited %v for %s", *watchdog, what)
	}
	var zero T
	return zero
}

func (k *fakeKernel) ioctl(fd int, req uint32, arg unsafe.Pointer) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	defer k.check()
	if !k.isOpen {
		k.afterClose = append(k.afterClose, req)
		return errnoEBADF
	}
	if fd != k.fd {
		k.t.Errorf("ioctl %#x on fd %d, but the node is fd %d", req, fd, k.fd)
	}
	k.calls = append(k.calls, req)
	if err := k.fail[req]; err != nil {
		return err
	}
	if k.gone && req != ioctlPVersion && req != ioctlInfo {
		return errnoENODEV
	}
	switch req {
	case ioctlPVersion:
		*(*int32)(arg) = int32(k.version)
	case ioctlInfo:
		*(*pcmInfo)(arg) = k.info
	case ioctlHWRefine:
		return k.refine((*hwParams)(arg))
	case ioctlHWParams:
		return k.hwParams((*hwParams)(arg))
	case ioctlHWFree:
		if k.state == stateRunning || k.state == stateDraining {
			return errnoEBADFD
		}
		k.state = stateOpen
	case ioctlSWParams:
		return k.swParams((*swParams)(arg))
	case ioctlPrepare:
		if k.committed == nil {
			return errnoEBADFD
		}
		if k.model {
			switch k.state {
			case stateRunning, stateDraining:
				return errnoEBUSY
			case stateOpen, stateDisconnected:
				return errnoEBADFD
			}
			k.appl, k.consumed, k.frac, k.runBytes = 0, 0, 0, nil
		}
		k.state = statePrepared
	case ioctlStart:
		if k.state != statePrepared {
			return errnoEBADFD
		}
		k.state = stateRunning
	case ioctlDrop:
		if k.state == stateOpen || k.state == stateDisconnected {
			return errnoEBADFD
		}
		k.state = stateSetup
	case ioctlDrain:
		if g := k.drainGate; g != nil {
			parked := k.drainParked
			k.drainGate, k.drainParked = nil, nil
			k.mu.Unlock()
			close(parked)
			<-g
			k.mu.Lock()
		}
		switch k.state {
		case statePrepared:
			if k.model && k.queued() == 0 {
				k.state = stateSetup
			} else {
				k.state = stateDraining
			}
		case stateRunning:
			k.state = stateDraining
		case statePaused:
			if k.model {
				k.state = stateDraining
			}
		case stateXrun:
			k.state = stateSetup
		}
		return errnoEAGAIN
	case ioctlStatus:
		if g := k.statusGate; g != nil {
			parked := k.statusParked
			k.statusGate, k.statusParked = nil, nil
			k.mu.Unlock()
			close(parked)
			<-g
			k.mu.Lock()
		}
		if len(k.states) > 0 {
			k.state, k.states = k.states[0], k.states[1:]
		}
		st := (*pcmStatus)(arg)
		if !k.model {
			*st = pcmStatus{state: k.state, delay: k.delay}
			break
		}
		*st = pcmStatus{state: k.state, applPtr: k.appl, hwPtr: k.consumed}
		if k.state == stateRunning || k.state == stateDraining {
			st.delay = int64(k.queued()) + k.extra
		}
		if k.committed != nil {
			st.avail = k.room()
		}
	case ioctlDelay:
		if k.model {
			*(*int64)(arg) = int64(k.queued()) + k.extra
			break
		}
		*(*int64)(arg) = k.delay
	case ioctlWriteIFrames:
		return k.writei((*xferi)(arg))
	default:
		k.t.Errorf("unknown ioctl %#x", req)
		return errnoENOTTY
	}
	return nil
}

func (k *fakeKernel) ioctlValue(fd int, req uint32, arg uintptr) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	defer k.check()
	if !k.isOpen {
		k.afterClose = append(k.afterClose, req)
		return errnoEBADF
	}
	if fd != k.fd {
		k.t.Errorf("ioctl %#x on fd %d, but the node is fd %d", req, fd, k.fd)
	}
	k.calls = append(k.calls, req)
	if err := k.fail[req]; err != nil {
		return err
	}
	switch req {
	case ioctlPause:
		k.pauses = append(k.pauses, arg)
		if k.gone {
			return errnoENODEV
		}
		// snd_pcm_pre_pause: the hardware must offer it, a push needs a
		// running stream and a release a paused one.
		if k.hw.info&infoPause == 0 {
			return errnoENOSYS
		}
		switch {
		case arg != 0 && k.state == stateRunning:
			k.state = statePaused
		case arg == 0 && k.state == statePaused:
			k.state = stateRunning
		default:
			return errnoEBADFD
		}
	default:
		k.t.Errorf("unknown value ioctl %#x", req)
		return errnoENOTTY
	}
	return nil
}

// writei takes frames as the script says, recording their bytes in played.
// The model takes no more than the buffer has room for.
func (k *fakeKernel) writei(x *xferi) error {
	if k.committed == nil {
		return errnoEBADFD
	}
	if g := k.writeGate; g != nil {
		parked := k.writeParked
		k.writeGate, k.writeParked = nil, nil
		k.mu.Unlock()
		close(parked)
		<-g
		k.mu.Lock()
	}
	step := fakeWrite{take: -1}
	if len(k.writes) > 0 {
		step, k.writes = k.writes[0], k.writes[1:]
	}
	if step.err != nil {
		if e, ok := step.err.(errno); ok {
			x.result = -int64(e)
		}
		return step.err
	}
	switch k.state {
	case statePrepared, stateRunning:
	case statePaused:
		if !k.model {
			return errnoEBADFD
		}
	case stateXrun:
		k.epipes++
		return errnoEPIPE
	case stateSuspended:
		if k.model {
			return errnoESTRPIPE
		}
		return errnoEBADFD
	default:
		return errnoEBADFD
	}
	take := int(x.frames)
	if step.take >= 0 && step.take < take {
		take = step.take
	}
	if k.model {
		room := int(k.room())
		if room == 0 {
			x.result = -int64(errnoEAGAIN)
			return errnoEAGAIN
		}
		take = min(take, room)
	}
	frameBytes := int(k.frameBytes())
	b := unsafe.Slice((*byte)(x.buf), take*frameBytes)
	k.played = append(k.played, b...)
	x.result = int64(take)
	if k.model {
		k.runBytes = append(k.runBytes, b...)
		k.appl += uint64(take)
		k.total += uint64(take)
		kept := k.at[:0]
		for _, a := range k.at {
			if k.total >= a.frames {
				a.do(k)
			} else {
				kept = append(kept, a)
			}
		}
		k.at = kept
		if k.state == statePrepared && k.appl >= k.sw.startThreshold {
			k.state = stateRunning
		}
		return nil
	}
	if k.state == statePrepared && k.sw != nil && uint64(len(k.played)/frameBytes) >= k.sw.startThreshold {
		k.state = stateRunning
	}
	return nil
}

// frameBytes is the committed frame's size.
func (k *fakeKernel) frameBytes() uint32 {
	f, _ := k.committed.maskOf(hwParamFormat).single()
	ch, _ := k.committed.intervalOf(hwParamChannels).exact()
	for _, sf := range sampleFormats {
		if sf.code == f {
			return uint32(sf.bytes) * ch
		}
	}
	k.t.Errorf("committed format %d has no size", f)
	return 1
}

// refine narrows hw to the hardware, as HW_REFINE does, and leaves hw as it
// was when a parameter has nothing left (EINVAL).
func (k *fakeKernel) refine(hw *hwParams) error {
	p := *hw
	if !k.narrow(&p) {
		return errnoEINVAL
	}
	p.rmask = 0
	*hw = p
	return nil
}

// narrow applies the hardware's constraints to p, and reports false when a
// parameter is left empty.
func (k *fakeKernel) narrow(p *hwParams) bool {
	keep := func(param int, allowed []uint32) bool {
		m := p.maskOf(param)
		var out mask
		for _, v := range m.values() {
			if slices.Contains(allowed, v) {
				out.bits[v/32] |= 1 << (v % 32)
			}
		}
		*m = out
		return len(out.values()) > 0
	}
	if !keep(hwParamAccess, []uint32{accessRWInterleaved}) ||
		!keep(hwParamFormat, k.hw.formats) ||
		!keep(hwParamSubformat, []uint32{subformatStd}) {
		return false
	}
	within := func(param int, lo, hi uint32) (uint32, uint32, bool) {
		iv := p.intervalOf(param)
		lo, hi = max(lo, iv.lo()), min(hi, iv.hi())
		if g := k.hw.granule; g != 0 && (param == hwParamBufferSize || param == hwParamPeriodSize) {
			lo = (lo + g - 1) / g * g
			hi = hi / g * g
		}
		if lo > hi {
			return 0, 0, false
		}
		*iv = interval{min: lo, max: hi, flags: intervalInteger}
		return lo, hi, true
	}
	if _, _, ok := within(hwParamChannels, k.hw.chMin, k.hw.chMax); !ok {
		return false
	}
	rate := p.intervalOf(hwParamRate)
	var rates []uint32
	for _, r := range k.hw.rates {
		if r >= rate.lo() && r <= rate.hi() {
			rates = append(rates, r)
		}
	}
	if len(rates) == 0 {
		return false
	}
	*rate = interval{min: rates[0], max: rates[len(rates)-1], flags: intervalInteger}
	bufLo, bufHi, ok := within(hwParamBufferSize, k.hw.bufMin, k.hw.bufMax)
	if !ok {
		return false
	}
	perLo, perHi, ok := within(hwParamPeriodSize, k.hw.perMin, min(k.hw.perMax, bufHi/2))
	if !ok {
		return false
	}
	if _, _, ok := within(hwParamBufferSize, max(bufLo, 2*perLo), bufHi); !ok {
		return false
	}
	*p.intervalOf(hwParamPeriods) = interval{min: max(2, bufLo/perHi), max: bufHi / perLo, flags: intervalInteger}
	p.info = k.hw.info
	if f, ok := p.maskOf(hwParamFormat).single(); ok {
		p.msbits = k.hw.msbits
		if p.msbits == 0 {
			p.msbits = map[uint32]uint32{formatS16LE: 16, formatS243LE: 24, formatS24LE: 24, formatS32LE: 32}[f]
		}
	}
	if r, ok := rate.exact(); ok {
		p.rateNum, p.rateDen = r, 1
	}
	return true
}

// hwParams refines p and then chooses one value for each parameter, as
// HW_PARAMS does: the first access, format and subformat, the fewest
// channels, the lowest rate, the smallest period, and the largest buffer
// that is a whole number of periods.
func (k *fakeKernel) hwParams(p *hwParams) error {
	switch k.state {
	case stateOpen, stateSetup, statePrepared:
	default:
		return errnoEBADFD
	}
	c := *p
	if !k.narrow(&c) {
		return errnoEINVAL
	}
	for _, param := range []int{hwParamAccess, hwParamFormat, hwParamSubformat} {
		c.setMask(param, c.maskOf(param).values()[0])
	}
	c.setInteger(hwParamChannels, c.intervalOf(hwParamChannels).lo())
	c.setInteger(hwParamRate, c.intervalOf(hwParamRate).lo())
	period := c.intervalOf(hwParamPeriodSize).lo()
	buffer := c.intervalOf(hwParamBufferSize).hi() / period * period
	if buffer < c.intervalOf(hwParamBufferSize).lo() {
		return errnoEINVAL
	}
	c.setInteger(hwParamPeriodSize, period)
	c.setInteger(hwParamBufferSize, buffer)
	c.setInteger(hwParamPeriods, buffer/period)
	if !k.narrow(&c) {
		return errnoEINVAL
	}
	c.rmask = 0
	if k.commit != nil {
		k.commit(&c)
	}
	*p = c
	committed := c
	k.committed = &committed
	k.state = stateSetup
	return nil
}

// swParams checks p as SW_PARAMS does and records it.
func (k *fakeKernel) swParams(p *swParams) error {
	if k.committed == nil {
		return errnoEBADFD
	}
	if p.availMin == 0 || p.tstampMode != tstampNone || p.silenceSize > p.silenceThreshold {
		return errnoEINVAL
	}
	sw := *p
	k.sw = &sw
	return nil
}

// callsOf counts the calls of one request.
func (k *fakeKernel) callsOf(req uint32) int {
	k.mu.Lock()
	defer k.mu.Unlock()
	n := 0
	for _, c := range k.calls {
		if c == req {
			n++
		}
	}
	return n
}

// checkClosedOnce fails the test unless the node was closed exactly once
// and nothing reached the kernel after it.
func (k *fakeKernel) checkClosedOnce(t *testing.T) {
	t.Helper()
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.closes != 1 {
		t.Errorf("the node was closed %d times, want once", k.closes)
	}
	if len(k.afterClose) > 0 {
		t.Errorf("ioctls after close: %#x", k.afterClose)
	}
}

// checkClosedEach fails the test unless every open of the node was closed
// exactly once (close itself fails the test on a second close of one open)
// and nothing reached the kernel while it was closed.
func (k *fakeKernel) checkClosedEach(t *testing.T) {
	t.Helper()
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.isOpen || k.closes != k.opens {
		t.Errorf("the node was opened %d times and closed %d (open now: %v); want each open closed once", k.opens, k.closes, k.isOpen)
	}
	if len(k.afterClose) > 0 {
		t.Errorf("ioctls after close: %#x", k.afterClose)
	}
}
