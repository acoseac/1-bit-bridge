package alsapcm

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"github.com/acoseac/1-bit-bridge/internal/player/format"
)

// Endpoint is a physical PCM playback endpoint: a card index and a PCM
// device number, which name the node /dev/snd/pcmC<card>D<device>p. It is
// the only way to name one. There is no string form to parse, so "default",
// "hw:…", "plughw:…", dmix, softvol, an alias or a path cannot reach Open,
// and no ALSA configuration (.asoundrc, asound.conf) is ever read.
type Endpoint struct {
	Card   int
	Device int
}

// maxIndex bounds a card or a device number. ALSA has at most 256 cards
// (CONFIG_SND_MAX_CARDS), and its device minors leave no room for more PCM
// devices than that. It is a sanity bound: the identity check is INFO's,
// at open.
const maxIndex = 255

func (e Endpoint) String() string {
	return "card " + strconv.Itoa(e.Card) + " device " + strconv.Itoa(e.Device)
}

// Path is the endpoint's playback node.
func (e Endpoint) Path() string {
	return "/dev/snd/pcmC" + strconv.Itoa(e.Card) + "D" + strconv.Itoa(e.Device) + "p"
}

// Config is what Open asks the hardware for.
type Config struct {
	// RateHz is the frame rate, granted exactly or refused.
	RateHz int
	// Format is the ALSA format the samples are packed in, by the format
	// package's packers; granted exactly or refused.
	Format format.ALSAFormat
	// BufferTime is the hardware buffer to aim for; 0 means
	// DefaultBufferTime. What the hardware grants is in Params.
	BufferTime time.Duration
}

// DefaultBufferTime is the hardware buffer Open aims for: about 200 ms,
// the design's starting point (B470 §1), a tuning hypothesis the owner's
// hardware runs settle.
const DefaultBufferTime = 200 * time.Millisecond

// maxBufferTime bounds Config.BufferTime: a hardware buffer, not a queue.
const maxBufferTime = 2 * time.Second

// Channels is the one channel count the player opens: stereo.
const Channels = 2

// MinRateHz and MaxRateHz bound Config.RateHz to what a PCM DAC plays.
const (
	MinRateHz = 8000
	MaxRateHz = 1536000
)

// periodsTarget is how many periods Open aims to split the buffer into:
// a period of about 50 ms in the default 200.
const periodsTarget = 4

// stallFactor and minStall set how long a write waits for room in the
// hardware buffer before it calls the stream stalled (stallTimeout).
const (
	stallFactor = 10
	minStall    = time.Second
)

// Params is what the kernel committed for an open Device, read back after
// HW_PARAMS, and the software parameters Open set.
type Params struct {
	Endpoint Endpoint
	// Subdevice is the substream the kernel gave this open, from INFO.
	Subdevice int
	// ID and Name are the device's own, from INFO ("USB Audio").
	ID, Name string
	// Protocol is the kernel's PCM protocol version (ProtocolString).
	Protocol uint32
	Format   format.ALSAFormat
	Channels int
	RateHz   int
	// RateNum and RateDen are the rate as the kernel holds it, a fraction
	// whose value Open checked is exactly RateHz.
	RateNum, RateDen uint32
	// Msbits is the significant bits of a sample the hardware reports, 0
	// when it said nothing.
	Msbits int
	// PeriodFrames, Periods and BufferFrames are the geometry granted.
	PeriodFrames, Periods, BufferFrames int
	// FIFOFrames is the chip's FIFO, in frames, 0 when unknown.
	FIFOFrames uint64
	// Info is the setup's SNDRV_PCM_INFO_* flags (InfoFlags).
	Info uint32
	// StartThreshold, StopThreshold and AvailMin are the software
	// parameters: the stream starts once the buffer holds StartThreshold
	// frames, stops (an xrun) once it has StopThreshold frames free, which
	// is all of it, and a waiting writer wakes when AvailMin are free.
	StartThreshold, StopThreshold, AvailMin int
}

// FrameBytes is the bytes one frame takes in the committed format.
func (p Params) FrameBytes() int {
	return sampleFormats[p.Format].bytes * p.Channels
}

// BufferTime is the granted buffer as a duration.
func (p Params) BufferTime() time.Duration { return framesTime(p.BufferFrames, p.RateHz) }

// PeriodTime is the granted period as a duration.
func (p Params) PeriodTime() time.Duration { return framesTime(p.PeriodFrames, p.RateHz) }

// InfoFlags names the setup's info flags this backend reports: "pause",
// "resume", "batch" (the position moves a period at a time) and
// "block_transfer".
func (p Params) InfoFlags() []string {
	var out []string
	for _, f := range []struct {
		bit  uint32
		name string
	}{{infoPause, "pause"}, {infoResume, "resume"}, {infoBatch, "batch"}, {infoBlockTransfer, "block_transfer"}} {
		if p.Info&f.bit != 0 {
			out = append(out, f.name)
		}
	}
	return out
}

// CanPause reports whether the setup offers the kernel's PAUSE: the
// committed parameters carry SNDRV_PCM_INFO_PAUSE.
func (p Params) CanPause() bool { return p.Info&infoPause != 0 }

// framesTime is n frames at rate as a duration.
func framesTime(n, rate int) time.Duration {
	if rate <= 0 {
		return 0
	}
	return time.Duration(int64(n) * int64(time.Second) / int64(rate))
}

// ProtocolString renders a protocol version as major.minor.micro.
func ProtocolString(v uint32) string {
	return fmt.Sprintf("%d.%d.%d", v>>16, v>>8&0xff, v&0xff)
}

// ProtocolFloor is the oldest kernel PCM protocol Open accepts
// (pcmVersionFloor).
func ProtocolFloor() uint32 { return pcmVersionFloor }

// PinnedKernel is the kernel release whose asound.h the backend's
// definitions follow (uapiKernel).
func PinnedKernel() string { return uapiKernel }

// State is a stream state as the kernel names it.
type State int32

// stateNames are the kernel's names for the states, as its status file
// prints them.
var stateNames = [...]string{"OPEN", "SETUP", "PREPARED", "RUNNING", "XRUN", "DRAINING", "PAUSED", "SUSPENDED", "DISCONNECTED"}

func (s State) String() string {
	if s >= 0 && int(s) < len(stateNames) {
		return stateNames[s]
	}
	return "state " + strconv.Itoa(int(s))
}

// Status is the kernel's account of the stream (STATUS).
type Status struct {
	State State
	// Delay is the frames written and not yet played as the kernel
	// estimates them; it reports 0 unless the stream is running or
	// draining.
	Delay int64
	// Avail is the frames free in the buffer, AvailMax the most free
	// since the last status.
	Avail, AvailMax uint64
	HWPtr, ApplPtr  uint64
}

// Counters counts what Write asked of the kernel since Open.
type Counters struct {
	// Writes is the WRITEI_FRAMES calls that took frames, and Partial the
	// ones of those that took fewer than they were offered.
	Writes, Partial int
	// Waits is the polls for room after EAGAIN, and Interrupted the calls
	// tried again after EINTR.
	Waits, Interrupted int
}

// Device is one open playback endpoint with committed parameters. It is
// not safe for concurrent use: one goroutine drives it, and a write is
// stopped by cancelling its context, never by closing the Device under it.
type Device struct {
	sys      sys
	now      func() time.Time
	fd       int
	closed   bool
	hwSet    bool
	params   Params
	counters Counters
}

// request is a Config checked and resolved: what setHW asks the kernel for.
type request struct {
	rate   uint32
	format format.ALSAFormat
	sample sampleFormat
	buffer time.Duration
}

// validate refuses an endpoint no card can have.
func (e Endpoint) validate() error {
	if e.Card < 0 || e.Card > maxIndex || e.Device < 0 || e.Device > maxIndex {
		return &Error{Reason: ReasonBadEndpoint, Op: "open", Endpoint: e,
			Detail: fmt.Sprintf("card and device are numbers from 0 to %d", maxIndex)}
	}
	return nil
}

// resolve checks cfg and fills in its defaults.
func (cfg Config) resolve(ep Endpoint) (request, error) {
	bad := func(detail string) (request, error) {
		return request{}, &Error{Reason: ReasonBadConfig, Op: "open", Endpoint: ep, Detail: detail}
	}
	sf, ok := sampleFormats[cfg.Format]
	if !ok {
		return bad(fmt.Sprintf("format %q is not one this backend packs (S16_LE, S24_3LE, S24_LE, S32_LE)", cfg.Format))
	}
	if cfg.RateHz < MinRateHz || cfg.RateHz > MaxRateHz {
		return bad(fmt.Sprintf("rate %d Hz is outside %d..%d", cfg.RateHz, MinRateHz, MaxRateHz))
	}
	buffer := cfg.BufferTime
	if buffer == 0 {
		buffer = DefaultBufferTime
	}
	if buffer < 0 || buffer > maxBufferTime {
		return bad(fmt.Sprintf("buffer time %v is outside 0..%v", cfg.BufferTime, maxBufferTime))
	}
	return request{rate: uint32(cfg.RateHz), format: cfg.Format, sample: sf, buffer: buffer}, nil
}

// open is Open over a kernel seam and a clock. Once the node is open, every
// refusal goes out through release, and so does a panic in its setup, which
// is the caller's to recover, so a refused open leaves nothing open.
func open(s sys, now func() time.Time, ep Endpoint, cfg Config) (*Device, error) {
	if err := ep.validate(); err != nil {
		return nil, err
	}
	want, err := cfg.resolve(ep)
	if err != nil {
		return nil, err
	}
	fd, err := s.open(ep.Path())
	if err != nil {
		return nil, openError(ep, err)
	}
	d := &Device{sys: s, now: now, fd: fd, params: Params{Endpoint: ep}}
	set := false
	defer func() {
		if !set {
			_ = d.release()
		}
	}()
	if err := d.setUp(want); err != nil {
		return nil, err
	}
	set = true
	return d, nil
}

// openError types a refusal of the open call itself.
func openError(ep Endpoint, err error) error {
	e := &Error{Op: "open", Endpoint: ep, Err: err}
	n, _ := errnoOf(err)
	switch {
	case errors.Is(err, errNotALSANode) || n == errnoELOOP:
		e.Reason, e.Detail = ReasonNotPCM, ep.Path()+" is not an ALSA playback node"
	case n == errnoENOENT || n == errnoENODEV || n == errnoENXIO:
		e.Reason, e.Detail = ReasonNoDevice, ep.Path()+" is not there: no such card or device"
	case n == errnoEACCES || n == errnoEPERM:
		e.Reason, e.Detail = ReasonPermission, "this process may not open "+ep.Path()+" (the audio group owns it)"
	case n == errnoEBUSY:
		e.Reason, e.Detail = ReasonBusy, "another process has "+ep.Path()+" open; the bridge never stops it"
	default:
		e.Reason, e.Detail = ReasonKernel, "opening "+ep.Path()
	}
	return e
}

// setUp checks the node and negotiates the stream, in the kernel's order.
func (d *Device) setUp(want request) error {
	if err := d.checkProtocol(); err != nil {
		return err
	}
	if err := d.checkInfo(); err != nil {
		return err
	}
	if err := d.setHW(want); err != nil {
		return err
	}
	if err := d.setSW(); err != nil {
		return err
	}
	if err := d.ioctlNone(ioctlPrepare); err != nil {
		return d.kernelError("prepare", err)
	}
	return nil
}

// checkProtocol refuses a kernel PCM protocol of another major or minor,
// or below pcmVersionFloor.
func (d *Device) checkProtocol() error {
	var v int32
	if err := ioctlOn(d, ioctlPVersion, &v); err != nil {
		return d.kernelError("pversion", err)
	}
	ver := uint32(v)
	d.params.Protocol = ver
	switch {
	case ver>>8 != pcmVersion>>8:
		return d.refuse("pversion", ReasonProtocol, fmt.Sprintf(
			"the kernel's PCM protocol is %s and this backend speaks %s: another major or minor is another interface",
			ProtocolString(ver), ProtocolString(pcmVersion)))
	case ver < pcmVersionFloor:
		return d.refuse("pversion", ReasonProtocol, fmt.Sprintf(
			"the kernel's PCM protocol is %s, older than %s, the oldest this backend opens",
			ProtocolString(ver), ProtocolString(pcmVersionFloor)))
	}
	return nil
}

// checkInfo refuses a node the kernel says is not the endpoint asked for,
// and records the substream it is.
func (d *Device) checkInfo() error {
	var info pcmInfo
	if err := ioctlOn(d, ioctlInfo, &info); err != nil {
		return d.kernelError("info", err)
	}
	ep := d.params.Endpoint
	if info.stream != streamPlayback || int(info.card) != ep.Card || int64(info.device) != int64(ep.Device) {
		dir := "playback"
		if info.stream != streamPlayback {
			dir = "stream " + strconv.Itoa(int(info.stream))
		}
		return d.refuse("info", ReasonNotPCM, fmt.Sprintf("%s is card %d device %d %s, not the playback endpoint asked for",
			ep.Path(), info.card, info.device, dir))
	}
	d.params.Subdevice = int(info.subdevice)
	d.params.ID = cString(info.id[:])
	d.params.Name = cString(info.name[:])
	return nil
}

// setHW negotiates the hardware parameters: the exact request refined,
// the geometry aimed, HW_PARAMS, and the committed values read back.
func (d *Device) setHW(want request) error {
	hw := anyParams()
	hw.setMask(hwParamAccess, accessRWInterleaved)
	hw.setMask(hwParamFormat, want.sample.code)
	hw.setMask(hwParamSubformat, subformatStd)
	hw.setInteger(hwParamChannels, Channels)
	hw.setInteger(hwParamRate, want.rate)
	if err := d.refine(&hw); err != nil {
		if n, _ := errnoOf(err); n == errnoEINVAL {
			return d.refuse("hw_refine", ReasonNotSupported, d.offered(want))
		}
		return d.kernelError("hw_refine", err)
	}
	d.aimGeometry(&hw, want)
	hw.rmask = allParams
	if err := ioctlOn(d, ioctlHWParams, &hw); err != nil {
		if n, _ := errnoOf(err); n == errnoEINVAL {
			return d.refuse("hw_params", ReasonNotSupported, "the kernel refused the parameters its own refine allowed")
		}
		return d.kernelError("hw_params", err)
	}
	d.hwSet = true
	return d.readBack(&hw, want)
}

// refine asks the kernel to narrow hw to what the hardware allows. A
// refusal (EINVAL: a parameter with nothing left) leaves hw as it was.
func (d *Device) refine(hw *hwParams) error {
	hw.rmask = allParams
	return ioctlOn(d, ioctlHWRefine, hw)
}

// offered says what the hardware takes, for a request it refused: a refine
// with nothing but the access asked for.
func (d *Device) offered(want request) string {
	asked := fmt.Sprintf("%s, %d channels, %d Hz", want.format, Channels, want.rate)
	hw := anyParams()
	hw.setMask(hwParamAccess, accessRWInterleaved)
	if err := d.refine(&hw); err != nil {
		return "the hardware does not take " + asked + ", nor interleaved writes at all"
	}
	return fmt.Sprintf("the hardware does not take %s: it offers %s, %s channels, %s Hz", asked,
		formatsOf(*hw.maskOf(hwParamFormat)), hw.intervalOf(hwParamChannels), hw.intervalOf(hwParamRate))
}

// aimGeometry narrows the buffer, then the period, toward the request's
// buffer time in periodsTarget periods. Each is tried exactly first, and
// when the hardware refuses that size, as a range the kernel then chooses
// from: the largest buffer at most the aim, the smallest period at least
// it. A range the hardware refuses too leaves the parameter to the kernel.
// Nothing here can fail the open: the geometry may be what the hardware
// has, and Params reports it.
func (d *Device) aimGeometry(hw *hwParams, want request) {
	buf := *hw.intervalOf(hwParamBufferSize)
	b := clamp(framesIn(want.rate, want.buffer), buf.lo(), buf.hi())
	if !d.tryRefine(hw, func(t *hwParams) { t.setInteger(hwParamBufferSize, b) }) {
		d.tryRefine(hw, func(t *hwParams) { t.setRange(hwParamBufferSize, buf.lo(), b) })
	}
	per := *hw.intervalOf(hwParamPeriodSize)
	p := clamp(hw.intervalOf(hwParamBufferSize).hi()/periodsTarget, per.lo(), per.hi())
	if !d.tryRefine(hw, func(t *hwParams) { t.setInteger(hwParamPeriodSize, p) }) {
		d.tryRefine(hw, func(t *hwParams) { t.setRange(hwParamPeriodSize, p, per.hi()) })
	}
}

// tryRefine refines a copy of hw with set applied, and keeps it only when
// the kernel accepts it.
func (d *Device) tryRefine(hw *hwParams, set func(*hwParams)) bool {
	trial := *hw
	set(&trial)
	if d.refine(&trial) != nil {
		return false
	}
	*hw = trial
	return true
}

// accessNames names the access types a read-back can show.
var accessNames = map[uint32]string{0: "MMAP_INTERLEAVED", 1: "MMAP_NONINTERLEAVED", 2: "MMAP_COMPLEX", accessRWInterleaved: "RW_INTERLEAVED", 4: "RW_NONINTERLEAVED"}

// readBack checks what HW_PARAMS committed against the request and fills
// Params from it. A rate, channel count, format or access other than the
// one asked for, a rate that is not exactly that rate, or a geometry the
// kernel left open, is a mismatch: the open fails, and nothing is taken in
// its place.
func (d *Device) readBack(hw *hwParams, want request) error {
	var bad []string
	maskValue := func(param int) string {
		if v, ok := hw.maskOf(param).single(); ok {
			return strconv.FormatUint(uint64(v), 10)
		}
		return "none or several"
	}
	if v, ok := hw.maskOf(hwParamAccess).single(); !ok || v != accessRWInterleaved {
		name, known := accessNames[v]
		if !ok || !known {
			name = maskValue(hwParamAccess)
		}
		bad = append(bad, "access: asked RW_INTERLEAVED, committed "+name)
	}
	if v, ok := hw.maskOf(hwParamFormat).single(); !ok || v != want.sample.code {
		name := maskValue(hwParamFormat)
		if ok {
			name = formatName(v)
		}
		bad = append(bad, fmt.Sprintf("format: asked %s, committed %s", want.format, name))
	}
	if v, ok := hw.intervalOf(hwParamChannels).exact(); !ok || v != Channels {
		bad = append(bad, fmt.Sprintf("channels: asked %d, committed %s", Channels, hw.intervalOf(hwParamChannels).raw()))
	}
	if v, ok := hw.intervalOf(hwParamRate).exact(); !ok || v != want.rate {
		bad = append(bad, fmt.Sprintf("rate: asked %d Hz, committed %s Hz", want.rate, hw.intervalOf(hwParamRate).raw()))
	} else if hw.rateDen == 0 || uint64(hw.rateNum) != uint64(want.rate)*uint64(hw.rateDen) {
		bad = append(bad, fmt.Sprintf("rate: asked %d/1 Hz exactly, committed %d/%d", want.rate, hw.rateNum, hw.rateDen))
	}
	period, okPeriod := hw.intervalOf(hwParamPeriodSize).exact()
	buffer, okBuffer := hw.intervalOf(hwParamBufferSize).exact()
	switch {
	case !okPeriod || period == 0:
		bad = append(bad, "period size: the kernel left it "+hw.intervalOf(hwParamPeriodSize).raw())
	case !okBuffer || buffer < period:
		bad = append(bad, "buffer size: the kernel left it "+hw.intervalOf(hwParamBufferSize).raw())
	}
	if len(bad) > 0 {
		return d.refuse("hw_params", ReasonMismatch, strings.Join(bad, "; "))
	}
	periods, ok := hw.intervalOf(hwParamPeriods).exact()
	if !ok {
		periods = buffer / period
	}
	p := &d.params
	p.Format, p.Channels, p.RateHz = want.format, Channels, int(want.rate)
	p.RateNum, p.RateDen = hw.rateNum, hw.rateDen
	p.Msbits = int(hw.msbits)
	p.PeriodFrames, p.Periods, p.BufferFrames = int(period), int(periods), int(buffer)
	p.FIFOFrames = hw.fifoSize
	p.Info = hw.info
	return nil
}

// setSW sets the software parameters: the stream starts once the buffer is
// full; it stops, an xrun, the moment the buffer runs empty, so an underrun
// is never played as anything; a waiting writer wakes a period at a time;
// and the kernel never fills silence.
func (d *Device) setSW() error {
	p := &d.params
	sw := swParams{
		tstampMode:     tstampNone,
		periodStep:     1,
		availMin:       uint64(p.PeriodFrames),
		xferAlign:      1,
		startThreshold: uint64(p.BufferFrames),
		stopThreshold:  uint64(p.BufferFrames),
		proto:          pcmVersion,
	}
	if err := ioctlOn(d, ioctlSWParams, &sw); err != nil {
		return d.kernelError("sw_params", err)
	}
	p.StartThreshold, p.StopThreshold, p.AvailMin = int(sw.startThreshold), int(sw.stopThreshold), int(sw.availMin)
	return nil
}

// Params returns what the kernel committed.
func (d *Device) Params() Params { return d.params }

// NodeInfo is what the kernel says an open node is: its card, device and
// substream, whether it plays, and the device's own id and name.
type NodeInfo struct {
	Card, Device, Subdevice int
	Playback                bool
	ID, Name                string
}

// Node asks the kernel, now, what d's open node is: SNDRV_PCM_IOCTL_INFO on
// its descriptor, never the endpoint d was opened by or what Open recorded.
// The player's check after open (internal/player/binding) reads the card
// from it and compares that card's identity with the binding's before any
// frame is written.
func (d *Device) Node() (NodeInfo, error) {
	if d.closed {
		return NodeInfo{}, d.closedError("info")
	}
	var info pcmInfo
	if err := ioctlOn(d, ioctlInfo, &info); err != nil {
		return NodeInfo{}, d.kernelError("info", err)
	}
	return NodeInfo{
		Card: int(info.card), Device: int(info.device), Subdevice: int(info.subdevice),
		Playback: info.stream == streamPlayback, ID: cString(info.id[:]), Name: cString(info.name[:]),
	}, nil
}

// Counters returns what Write has asked of the kernel so far.
func (d *Device) Counters() Counters { return d.counters }

// Write plays frames: whole frames of interleaved samples packed in the
// committed format (Params.Format, Params.FrameBytes). It returns the
// frames the kernel took, all of them unless there is an error. A frame
// the kernel did not take is offered again; none is offered twice.
//
// The node is non-blocking: when the buffer is full the kernel returns
// EAGAIN and Write waits for room with poll, at most stallTimeout, and a
// cancelled ctx ends that wait at once (ReasonCancelled), leaving a Device
// that still drops and closes. EINTR is retried. EPIPE is an underrun
// (ReasonXrun) and ESTRPIPE a suspend (ReasonSuspended): the stream has
// stopped, and Write does not recover it. ENODEV, ENXIO, POLLHUP and
// POLLNVAL are a device that went away (ReasonDisconnected). POLLERR alone
// is not: the kernel raises it in the xrun and suspended states too, so
// Write asks the stream's state what it is (explain).
func (d *Device) Write(ctx context.Context, frames []byte) (int, error) {
	if d.closed {
		return 0, d.closedError("write")
	}
	fb := d.params.FrameBytes()
	if len(frames)%fb != 0 {
		return 0, d.refuse("write", ReasonBadConfig, fmt.Sprintf("%d bytes is not whole frames of %d bytes", len(frames), fb))
	}
	total, done := len(frames)/fb, 0
	unexplained := 0
	for done < total {
		if err := ctx.Err(); err != nil {
			return done, d.cancelled("write", err)
		}
		n, err := d.writei(frames[done*fb:], total-done)
		if err == nil {
			d.counters.Writes++
			if n < total-done {
				d.counters.Partial++
			}
			done += n
			unexplained = 0
			continue
		}
		switch e, _ := errnoOf(err); e {
		case errnoEINTR:
			d.counters.Interrupted++
		case errnoEAGAIN:
			d.counters.Waits++
			again, err := d.waitForRoom(ctx)
			if err != nil {
				return done, err
			}
			if again {
				unexplained++
				if unexplained > 1 {
					return done, d.refuse("write", ReasonKernel, "poll keeps reporting an error the stream's state does not explain")
				}
			}
		default:
			return done, d.streamError("write", err)
		}
	}
	return done, nil
}

// writei hands the kernel up to frames frames from b in one WRITEI_FRAMES
// call and returns how many it took. b stays alive until the kernel has
// returned, and nothing of it is kept after.
func (d *Device) writei(b []byte, frames int) (int, error) {
	x := xferi{buf: unsafe.Pointer(unsafe.SliceData(b)), frames: uint64(frames)}
	err := ioctlOn(d, ioctlWriteIFrames, &x)
	runtime.KeepAlive(b)
	if err != nil {
		return 0, err
	}
	if x.result <= 0 || x.result > int64(frames) {
		return 0, fmt.Errorf("the kernel reported %d frames written of %d", x.result, frames)
	}
	return int(x.result), nil
}

// stallTimeout is how long a write waits for room before it calls the
// stream stalled: stallFactor buffers, and at least minStall.
func (d *Device) stallTimeout() time.Duration {
	return max(stallFactor*d.params.BufferTime(), minStall)
}

// waitForRoom waits for the kernel to have room after EAGAIN. again is true
// when it woke on a POLLERR the stream's state explained away.
func (d *Device) waitForRoom(ctx context.Context) (again bool, err error) {
	rev, err := d.sys.poll(ctx, d.fd, pollOut, d.stallTimeout())
	if err != nil {
		if ctx.Err() != nil {
			return false, d.cancelled("write", ctx.Err())
		}
		return false, d.kernelError("poll", err)
	}
	switch {
	case rev == 0:
		return false, d.refuse("write", ReasonStalled, fmt.Sprintf("no room in the hardware buffer for %v", d.stallTimeout()))
	case rev&(pollHup|pollNval) != 0:
		return false, &Error{Reason: ReasonDisconnected, Op: "write", Endpoint: d.params.Endpoint,
			Detail: "the device went away", Err: pollEvents(rev)}
	case rev&pollErr != 0:
		return true, d.explain("write")
	}
	return false, nil
}

// pollEvents is a poll's revents as an error that names them.
type pollEvents int16

func (p pollEvents) Error() string {
	var names []string
	for _, f := range []struct {
		bit  int16
		name string
	}{{pollOut, "POLLOUT"}, {pollErr, "POLLERR"}, {pollHup, "POLLHUP"}, {pollNval, "POLLNVAL"}} {
		if int16(p)&f.bit != 0 {
			names = append(names, f.name)
		}
	}
	return "poll: " + strings.Join(names, "|")
}

// explain asks the kernel why a poll reported an error condition. The
// kernel's poll raises POLLERR for every state but running, prepared,
// paused and draining, so the state says which it was; one that plays is
// no error, and the write goes on.
func (d *Device) explain(op string) error {
	st, err := d.status()
	if err != nil {
		return d.streamError(op, err)
	}
	switch st.state {
	case stateRunning, statePrepared, statePaused, stateDraining:
		return nil
	}
	return d.stateError(op, st.state)
}

// Start starts a prepared stream before its buffer is full, which starts
// it on its own.
func (d *Device) Start() error {
	if d.closed {
		return d.closedError("start")
	}
	if err := d.ioctlNone(ioctlStart); err != nil {
		return d.streamError("start", err)
	}
	return nil
}

// Status asks the kernel for the stream's state and counters (STATUS).
func (d *Device) Status() (Status, error) {
	if d.closed {
		return Status{}, d.closedError("status")
	}
	st, err := d.status()
	if err != nil {
		return Status{}, d.streamError("status", err)
	}
	return Status{
		State: State(st.state), Delay: st.delay,
		Avail: st.avail, AvailMax: st.availMax,
		HWPtr: st.hwPtr, ApplPtr: st.applPtr,
	}, nil
}

// status is one STATUS call.
func (d *Device) status() (pcmStatus, error) {
	var st pcmStatus
	err := ioctlOn(d, ioctlStatus, &st)
	return st, err
}

// Delay asks the kernel for the frames written and not yet played (DELAY).
func (d *Device) Delay() (int64, error) {
	if d.closed {
		return 0, d.closedError("delay")
	}
	var n int64
	if err := ioctlOn(d, ioctlDelay, &n); err != nil {
		return 0, d.streamError("delay", err)
	}
	return n, nil
}

// Pause pauses a running stream (on) or releases a paused one (off) with
// the kernel's PAUSE. A paused stream keeps its buffer and its pointers:
// what was queued plays on from where it stopped once it is released.
// Hardware without PAUSE (Params.CanPause) refuses it
// (ReasonNotSupported); a stream in another state is typed by its state.
func (d *Device) Pause(on bool) error {
	if d.closed {
		return d.closedError("pause")
	}
	var v uintptr
	if on {
		v = 1
	}
	if err := d.sys.ioctlValue(d.fd, ioctlPause, v); err != nil {
		if e, _ := errnoOf(err); e == errnoENOSYS {
			return &Error{Reason: ReasonNotSupported, Op: "pause", Endpoint: d.params.Endpoint,
				Detail: "the hardware does not pause", Err: err}
		}
		return d.streamError("pause", err)
	}
	return nil
}

// Prepare prepares the stream again (PREPARE), after an xrun or a drop:
// the kernel's pointers start again from zero, and the next writes fill
// the buffer until the stream starts.
func (d *Device) Prepare() error {
	if d.closed {
		return d.closedError("prepare")
	}
	if err := d.ioctlNone(ioctlPrepare); err != nil {
		return d.streamError("prepare", err)
	}
	return nil
}

// wait waits on a stream nothing is writing, a paused, a filling or a
// running one, for an error condition the kernel raises (an underrun, a
// suspend, a device gone), for at most timeout, or until ctx ends. It
// polls for no event: room in the buffer, which the kernel reports with
// POLLOUT, does not end it, so the wait cannot spin.
func (d *Device) wait(ctx context.Context, timeout time.Duration) (int16, error) {
	if d.closed {
		return 0, d.closedError("wait")
	}
	return d.sys.poll(ctx, d.fd, 0, timeout)
}

// Drop stops the stream at once and discards what the buffer holds
// (DROP). The Device stays open; Close closes it.
func (d *Device) Drop() error {
	if d.closed {
		return d.closedError("drop")
	}
	if err := d.ioctlNone(ioctlDrop); err != nil {
		return d.streamError("drop", err)
	}
	return nil
}

// Drain plays out what the buffer holds and stops the stream. On the
// non-blocking node DRAIN returns at once, and Drain waits for the
// kernel to finish: it re-asks the stream's state at least once a period,
// and waits in between for nothing but the error condition the kernel
// raises when the drain ends, so a poll cannot spin. It never waits past
// timeout: then, or when ctx ends, it drops the stream and says so
// (ReasonDrainTimeout, ReasonCancelled).
func (d *Device) Drain(ctx context.Context, timeout time.Duration) error {
	if d.closed {
		return d.closedError("drain")
	}
	deadline := d.now().Add(timeout)
	// A stream that already stopped has nothing to play out. DRAIN moves an
	// XRUN stream to SETUP and answers EAGAIN, so asking the state only
	// after DRAIN would read an underrun after the last write as a clean
	// end (the Claude API review and Codex on #1301).
	pre, err := d.status()
	if err != nil {
		return d.streamError("drain", err)
	}
	if pre.state == stateXrun || pre.state == stateSuspended || pre.state == stateDisconnected {
		return d.stateError("drain", pre.state)
	}
	// The check leaves a window: an underrun between it and DRAIN is erased
	// by DRAIN too. A running stream with frames left is seen DRAINING first
	// when its drain is clean, so SETUP straight after DRAIN from a stream
	// the check saw RUNNING is that underrun. It errs only when the buffer
	// was all but empty at the check, an underrun in the making anyway.
	wasRunning, sawDraining := pre.state == stateRunning, false
	for {
		err := d.ioctlNone(ioctlDrain)
		if err == nil {
			break
		}
		if e, _ := errnoOf(err); e == errnoEAGAIN {
			break
		} else if e != errnoEINTR || !d.now().Before(deadline) {
			return d.streamError("drain", err)
		}
	}
	for {
		st, err := d.status()
		if err != nil {
			return d.streamError("drain", err)
		}
		switch st.state {
		case stateSetup:
			if wasRunning && !sawDraining {
				return &Error{Reason: ReasonXrun, Op: "drain", Endpoint: d.params.Endpoint,
					Detail: "the stream stopped before it drained: an underrun between the last write and the drain"}
			}
			return nil
		case stateDraining:
			sawDraining = true
		case stateRunning:
		default:
			return d.stateError("drain", st.state)
		}
		left := deadline.Sub(d.now())
		if left <= 0 {
			_ = d.ioctlNone(ioctlDrop)
			return d.refuse("drain", ReasonDrainTimeout, fmt.Sprintf("the stream was still draining after %v; dropped", timeout))
		}
		rev, err := d.sys.poll(ctx, d.fd, 0, min(left, max(d.params.PeriodTime(), time.Millisecond)))
		if err != nil {
			_ = d.ioctlNone(ioctlDrop)
			if ctx.Err() != nil {
				return d.cancelled("drain", ctx.Err())
			}
			return d.kernelError("poll", err)
		}
		if rev&(pollHup|pollNval) != 0 {
			return &Error{Reason: ReasonDisconnected, Op: "drain", Endpoint: d.params.Endpoint,
				Detail: "the device went away", Err: pollEvents(rev)}
		}
	}
}

// Close stops the stream at once (DROP), frees its hardware parameters
// (HW_FREE) and closes the node, once: a second Close does nothing. The
// node is closed even when the kernel refuses DROP or HW_FREE, as it does
// for a device that went away.
func (d *Device) Close() error {
	if d.closed {
		return nil
	}
	return d.release()
}

// release is every way out once the node is open, Open's refusals and
// Close alike: DROP and HW_FREE when HW_PARAMS had set the hardware, then
// close, exactly once.
func (d *Device) release() error {
	d.closed = true
	if d.hwSet {
		_ = d.ioctlNone(ioctlDrop)
		_ = d.ioctlNone(ioctlHWFree)
	}
	if err := d.sys.close(d.fd); err != nil {
		return &Error{Reason: ReasonKernel, Op: "close", Endpoint: d.params.Endpoint, Err: err}
	}
	return nil
}

// errnoOf returns the errno in err's chain.
func errnoOf(err error) (errno, bool) {
	var e errno
	if errors.As(err, &e) {
		return e, true
	}
	return 0, false
}

// refuse is a refusal without an error from the kernel behind it.
func (d *Device) refuse(op string, reason Reason, detail string) error {
	return &Error{Reason: reason, Op: op, Endpoint: d.params.Endpoint, Detail: detail}
}

// cancelled is an operation whose context ended.
func (d *Device) cancelled(op string, err error) error {
	return &Error{Reason: ReasonCancelled, Op: op, Endpoint: d.params.Endpoint, Err: err}
}

// closedError is an operation on a closed Device.
func (d *Device) closedError(op string) error {
	return &Error{Reason: ReasonClosed, Op: op, Endpoint: d.params.Endpoint, Detail: "the device is closed"}
}

// kernelError is an error the kernel returned during the open or a call
// that does not move the stream; a device gone is still typed as one.
func (d *Device) kernelError(op string, err error) error {
	if e, _ := errnoOf(err); e == errnoENODEV || e == errnoENXIO {
		return &Error{Reason: ReasonDisconnected, Op: op, Endpoint: d.params.Endpoint, Detail: "the device went away", Err: err}
	}
	return &Error{Reason: ReasonKernel, Op: op, Endpoint: d.params.Endpoint, Err: err}
}

// streamError types an error from a call that moves the stream: EPIPE an
// xrun, ESTRPIPE a suspend, ENODEV and ENXIO a device gone, and EBADFD,
// a state that does not allow the call, as whatever the state is.
func (d *Device) streamError(op string, err error) error {
	e, ok := errnoOf(err)
	switch {
	case !ok:
		return &Error{Reason: ReasonKernel, Op: op, Endpoint: d.params.Endpoint, Err: err}
	case e == errnoEPIPE:
		return &Error{Reason: ReasonXrun, Op: op, Endpoint: d.params.Endpoint,
			Detail: "the buffer ran empty and the stream stopped (an underrun)", Err: err}
	case e == errnoESTRPIPE:
		return &Error{Reason: ReasonSuspended, Op: op, Endpoint: d.params.Endpoint,
			Detail: "the system suspended the stream", Err: err}
	case e == errnoEBADFD:
		st, serr := d.status()
		if serr != nil {
			// Linux answers every PCM ioctl, STATUS included, with EBADFD
			// once the runtime is DISCONNECTED: one PCM removed (a codec
			// unbound), where a whole card's removal gives ENODEV.
			if se, _ := errnoOf(serr); se == errnoEBADFD {
				return &Error{Reason: ReasonDisconnected, Op: op, Endpoint: d.params.Endpoint, Detail: "the device went away", Err: err}
			}
			return d.kernelError(op, serr)
		}
		return d.stateError(op, st.state)
	}
	return d.kernelError(op, err)
}

// stateError types a stream the state says has stopped, or cannot do
// what was asked.
func (d *Device) stateError(op string, state int32) error {
	e := &Error{Op: op, Endpoint: d.params.Endpoint}
	switch state {
	case stateXrun:
		e.Reason, e.Detail = ReasonXrun, "the buffer ran empty and the stream stopped (an underrun)"
	case stateSuspended:
		e.Reason, e.Detail = ReasonSuspended, "the system suspended the stream"
	case stateDisconnected:
		e.Reason, e.Detail = ReasonDisconnected, "the device went away"
	default:
		e.Reason, e.Detail = ReasonBadState, "the stream is "+State(state).String()
	}
	return e
}

// cString is a NUL-terminated string from a fixed-size kernel field.
func cString(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}
