package alsapcm

import (
	"context"
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

// fakeKernel is the kernel the tests drive a Device through: one PCM node,
// its hardware (fakeHW), scripted answers to writes, polls and status, and
// a record of every call. Each answer follows what sound/core/pcm_native.c
// does for the request, as far as these tests need it.
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

	// The record.
	opened     []string
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
		fd:      -1,
	}
	k.info.card, k.info.device, k.info.subdevice, k.info.stream = 5, 0, 0, streamPlayback
	copy(k.info.id[:], "USB Audio")
	copy(k.info.name[:], "USB Audio")
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

// now is the fake clock, which only a poll that times out moves.
func (k *fakeKernel) now() time.Time {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.clock
}

func (k *fakeKernel) open(path string) (int, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.opened = append(k.opened, path)
	if k.openErr != nil {
		return -1, k.openErr
	}
	k.fd = 7
	k.state = stateOpen
	return k.fd, nil
}

func (k *fakeKernel) close(fd int) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if fd != k.fd {
		k.t.Errorf("close(%d), but the node is fd %d", fd, k.fd)
	}
	k.closes++
	return nil
}

func (k *fakeKernel) poll(ctx context.Context, fd int, events int16, timeout time.Duration) (int16, error) {
	k.mu.Lock()
	if k.closes > 0 {
		k.t.Errorf("poll after close")
	}
	k.pollEvents = append(k.pollEvents, events)
	k.pollWaits = append(k.pollWaits, timeout)
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
		k.clock = k.clock.Add(timeout)
		k.mu.Unlock()
		return 0, nil
	}
	k.mu.Unlock()
	return step.revents, nil
}

func (k *fakeKernel) ioctl(fd int, req uint32, arg unsafe.Pointer) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.closes > 0 {
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
		switch k.state {
		case statePrepared, stateRunning:
			k.state = stateDraining
		case stateXrun:
			k.state = stateSetup
		}
		return errnoEAGAIN
	case ioctlStatus:
		if len(k.states) > 0 {
			k.state, k.states = k.states[0], k.states[1:]
		}
		st := (*pcmStatus)(arg)
		*st = pcmStatus{state: k.state, delay: k.delay}
	case ioctlDelay:
		*(*int64)(arg) = k.delay
	case ioctlWriteIFrames:
		return k.writei((*xferi)(arg))
	default:
		k.t.Errorf("unknown ioctl %#x", req)
		return errnoENOTTY
	}
	return nil
}

// writei takes frames as the script says, recording their bytes in played.
func (k *fakeKernel) writei(x *xferi) error {
	if k.committed == nil {
		return errnoEBADFD
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
	case stateXrun:
		return errnoEPIPE
	default:
		return errnoEBADFD
	}
	take := int(x.frames)
	if step.take >= 0 && step.take < take {
		take = step.take
	}
	frameBytes := int(k.frameBytes())
	k.played = append(k.played, unsafe.Slice((*byte)(x.buf), take*frameBytes)...)
	x.result = int64(take)
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
