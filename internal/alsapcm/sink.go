package alsapcm

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/player/format"
)

// The sink's sizes and bounds (B470 §1: about two seconds of decoded
// lookahead, capped at 32 MiB, preallocated; §3: release a pause after 30
// seconds).
const (
	// DefaultLookahead is the decoded audio a sink holds ahead of the
	// hardware buffer when SinkConfig.Lookahead is 0.
	DefaultLookahead = 2 * time.Second
	// MaxRingBytes caps the sink's frame store, lookahead and kept frames
	// together. At the backend's highest rate with four-byte samples two
	// seconds take 24.6 MB, so the cap binds only on a longer lookahead.
	MaxRingBytes = 32 << 20
	// DefaultReleaseAfter is how long a pause holds the device when
	// SinkConfig.ReleaseAfter is 0.
	DefaultReleaseAfter = 30 * time.Second

	minLookahead    = 100 * time.Millisecond
	maxLookahead    = 30 * time.Second
	minReleaseAfter = time.Second
	maxReleaseAfter = 10 * time.Minute
	// drainSlack is what a drain waits past the hardware buffer's own time
	// before it calls the drain stuck.
	drainSlack = 2 * time.Second
	// maxGaps bounds the gaps a sink keeps the details of; Gaps counts all.
	maxGaps = 8
)

// Transport is what a sink sends the DAC: the representation (PCM; native
// DSD and DoP are later PRs), the frame rate, the channels, the ALSA
// format, and the valid bits the samples were packed from, which is the
// packing together with the format (24 valid bits in S32_LE is not 32).
// Two tracks of one transport follow each other with nothing between
// them; any difference needs a drain, a reopen and a refill (B470 §3).
type Transport struct {
	Kind      format.Kind
	RateHz    int
	Channels  int
	Format    format.ALSAFormat
	ValidBits int
}

func (t Transport) String() string {
	return fmt.Sprintf("%s %d Hz, %d channels, %s from %d valid bits", t.Kind, t.RateHz, t.Channels, t.Format, t.ValidBits)
}

// Join is the answer to whether the next track may follow what a sink
// plays without a gap. The sink only answers; its caller decides.
type Join struct {
	// Gapless is true when the next track's transport is the sink's: its
	// frames are written straight after the last ones, with no drain,
	// reopen or silence between.
	Gapless bool
	// Differs names each part of the transport that differs, when not
	// gapless: such a track needs the sink drained and a sink opened for
	// it, an honest gap.
	Differs []string
}

// Join says how a track whose transport is next follows t.
func (t Transport) Join(next Transport) Join {
	var d []string
	if t.Kind != next.Kind {
		d = append(d, fmt.Sprintf("kind %s → %s", t.Kind, next.Kind))
	}
	if t.RateHz != next.RateHz {
		d = append(d, fmt.Sprintf("rate %d → %d Hz", t.RateHz, next.RateHz))
	}
	if t.Channels != next.Channels {
		d = append(d, fmt.Sprintf("channels %d → %d", t.Channels, next.Channels))
	}
	if t.Format != next.Format {
		d = append(d, fmt.Sprintf("format %s → %s", t.Format, next.Format))
	}
	if t.ValidBits != next.ValidBits {
		d = append(d, fmt.Sprintf("valid bits %d → %d", t.ValidBits, next.ValidBits))
	}
	return Join{Gapless: len(d) == 0, Differs: d}
}

// carries reports whether the format package packs validBits-bit samples
// as f: S16_LE carries 16, S24_3LE and S24_LE 24, S32_LE 24 or 32.
func carries(f format.ALSAFormat, validBits int) bool {
	switch f {
	case format.S16LE:
		return validBits == 16
	case format.S243LE, format.S24LE:
		return validBits == 24
	case format.S32LE:
		return validBits == 24 || validBits == 32
	}
	return false
}

// PauseMode is how a sink pauses.
type PauseMode int

const (
	// PauseAuto pauses with the kernel's PAUSE when the setup offers it
	// (Params.CanPause) and with zeros otherwise.
	PauseAuto PauseMode = iota
	// PauseKernel pauses with the kernel's PAUSE only: OpenSink refuses
	// hardware that does not offer it.
	PauseKernel
	// PauseZeros keeps the stream running on zeros, even on hardware that
	// could pause.
	PauseZeros
)

func (m PauseMode) String() string {
	switch m {
	case PauseAuto:
		return "auto"
	case PauseKernel:
		return "pause"
	case PauseZeros:
		return "zeros"
	}
	return "pause mode " + strconv.Itoa(int(m))
}

// SinkConfig is what OpenSink opens.
type SinkConfig struct {
	Endpoint  Endpoint
	Transport Transport
	// BufferTime is the hardware buffer to aim for (Config.BufferTime); 0
	// means DefaultBufferTime.
	BufferTime time.Duration
	// Lookahead is the decoded audio the sink holds ahead of the hardware
	// buffer; 0 means DefaultLookahead. With the frames kept for a replay
	// it is capped by MaxRingBytes.
	Lookahead time.Duration
	// Pause is how Pause pauses.
	Pause PauseMode
	// ReleaseAfter is how long a pause holds the device before the sink
	// releases it; 0 means DefaultReleaseAfter.
	ReleaseAfter time.Duration
}

// SinkState is where a sink is.
type SinkState int

const (
	// SinkPlaying has the device open and its writer feeding it: filling
	// the hardware buffer until the stream starts, then keeping it full.
	SinkPlaying SinkState = iota
	// SinkPaused holds the device paused: by the kernel's PAUSE, by zeros,
	// or, for a stream that had not started, by writing nothing.
	SinkPaused
	// SinkReleased was paused for ReleaseAfter and has released the
	// device; Resume opens it again.
	SinkReleased
	// SinkXrun has stopped at an underrun: Recover prepares the stream and
	// goes on, counting the gap; Drop and Close end it.
	SinkXrun
	// SinkDraining is playing out what was submitted: the producer is
	// done. A pause still pauses it until the kernel plays out the last
	// hardware buffer, which only a drop interrupts.
	SinkDraining
	// SinkStopped was dropped or drained: the device is released and the
	// sink is done.
	SinkStopped
	// SinkEnded was ended by a disconnect, a suspend, a cancel or another
	// failure (Position.Err): the device is closed and the sink is done.
	SinkEnded
)

var sinkStateNames = [...]string{"playing", "paused", "released", "xrun", "draining", "stopped", "ended"}

func (s SinkState) String() string {
	if s >= 0 && int(s) < len(sinkStateNames) {
		return sinkStateNames[s]
	}
	return "sink state " + strconv.Itoa(int(s))
}

// Position is a sink's account of the content it has played, for a later
// OpenHome Time to read. Content frames are the frames the producer wrote,
// counted from the sink's open; silence is never content.
type Position struct {
	State SinkState
	// Stream is the kernel's state of the stream at the last measurement.
	Stream State
	// Played is the content frames the hardware has played, as the kernel
	// establishes it: the frames written minus those it still holds
	// (STATUS's delay while the stream runs, its pointers while it is
	// paused, all of them before it starts). It never counts silence,
	// never goes back, never passes Written, and does not move while the
	// sink is paused, released, stopped by an xrun or done.
	Played int64
	// Written is the content frames handed to the kernel, and Submitted the
	// content frames the producer handed the sink: Written plus the
	// lookahead. A release takes back from Written the frames the DAC had
	// not played: they are written again after the reopen.
	Written, Submitted int64
	// Queued is the frames, content and silence, the kernel held at the
	// last measurement.
	Queued int64
	// RateHz converts frames to time.
	RateHz int
	// Gaps counts the xruns: each is a gap in the sound, reported, never
	// hidden.
	Gaps int
	// Measured is when Played was established, on the sink's clock.
	Measured time.Time
	// Err is why the sink stopped, for SinkXrun and SinkEnded.
	Err error
}

// Seconds is Played as content time.
func (p Position) Seconds() float64 {
	if p.RateHz <= 0 {
		return 0
	}
	return float64(p.Played) / float64(p.RateHz)
}

// Gap is one xrun, as the sink recorded it.
type Gap struct {
	// At is the content frame at which the stream stopped: the kernel had
	// consumed every frame written before it, and the next content frame
	// is the one after the gap.
	At int64
	// Lookahead is the content frames the sink held, not yet written, when
	// it stopped. None was lost: they play after Recover.
	Lookahead int64
	// Op is what found it: "write", "wait" (the writer's wait on the
	// stream while it has nothing to write), "status", "pause", "resume"
	// or "drain".
	Op string
	// Noticed is when the sink saw it, and Recovered when Recover
	// prepared the stream again (zero until then).
	Noticed, Recovered time.Time
}

// SinkStats is a sink's bounded diagnostics.
type SinkStats struct {
	// Params is what the kernel committed for the device the sink has or
	// last had open.
	Params Params
	// RingFrames and RingBytes are the frame store preallocated at open;
	// LookaheadFrames is the part of it the producer may fill ahead, and
	// KeptFrames the frames written that it keeps for a replay.
	RingFrames, LookaheadFrames, KeptFrames int64
	RingBytes                               int
	// Counters is what Write asked of the kernel, summed over every open.
	Counters Counters
	// Silence is the frames of zeros written by pauses.
	Silence int64
	// Pauses, Releases and Reopens count those events; PausedBy is how
	// the current or last pause paused: "pause", "zeros" or "idle".
	Pauses, Releases, Reopens int
	PausedBy                  string
	// Gaps is the last maxGaps xruns, oldest first; Position.Gaps counts
	// them all.
	Gaps []Gap
}

// Sink plays a stream of PCM frames through one endpoint (B470 PR 2b). A
// producer writes frames in the transport's format into a store
// preallocated at open, which holds them ahead of the hardware buffer; one
// writer goroutine owns the device and moves them into it with
// Device.Write, continuing partial writes. Pause, Resume, Recover, Drain
// and Drop are commands to that goroutine, safe from any goroutine; Write
// is for one producer at a time. A sink is done once dropped, drained or
// ended, and Close must be called to stop its goroutine.
type Sink struct {
	cfg               SinkConfig
	devCfg            Config
	life              context.Context
	open              func(Endpoint, Config) (*Device, error)
	now               func() time.Time
	producerWaits     func()
	producerPublishes func()
	afterFunc         func(context.Context, func()) func() bool

	// The writer's own: nothing else reads or writes them.
	dev         *Device
	run         run
	runPlayed   int64 // the frames of the run the kernel has played, as last established
	zeros       []byte
	deadline    time.Time // when the pause releases the device
	unexplained int       // POLLERRs in a row that woke a wait on the stream and its state explained away
	xrunPaused  bool      // the last xrun stopped a paused sink: Recover keeps the pause
	drainStop   func() bool

	wmu sync.Mutex // one producer at a time

	// Under mu. The writer is the only one that changes state, err,
	// pausedBy, params and drainCmd, so it reads them without the lock.
	mu       sync.Mutex
	ring     ring
	state    SinkState
	err      error
	pausedBy string
	params   Params
	drainCmd *sinkCmd // the Drain waiting for its end, which its cancel's callback looks for
	pos      Position // Stream, Played, Queued and Measured
	silence  int64
	counters Counters // the devices closed
	live     Counters // the device open
	pauses   int
	releases int
	reopens  int
	gaps     [maxGaps]Gap
	ngaps    int
	cmds     []*sinkCmd
	wake     context.Context
	wakeStop context.CancelFunc
	watching bool // the writer waits on the stream with nothing to write: frames submitted cancel its wait
	changed  chan struct{}
	// finishing is a Drain under way: the producer is done. finalDrain is
	// its last part, the kernel playing out the buffer, which a command
	// other than a Drop must not interrupt: an interrupted Device.Drain
	// drops the stream and cuts off the end.
	finishing, finalDrain bool

	kick   chan struct{} // frames or a command for the writer
	room   chan struct{} // room, or a new state, for the producer
	exited chan struct{} // closed when the writer returns
}

// sinkOp is a command to the writer.
type sinkOp int

const (
	opPause sinkOp = iota
	opResume
	opRecover
	opDrain
	opDrop
)

var sinkOpNames = [...]string{"pause", "resume", "recover", "drain", "drop"}

// sinkCmd is one command and where its answer goes.
type sinkCmd struct {
	op    sinkOp
	ctx   context.Context // Drain's
	reply chan error
}

// sinkSeams is what openSink takes from its caller: the opener of a
// device and the clock, and for the tests a hook called each time the
// producer starts to wait for room, one called each time it has copied
// frames into the store and is about to publish them, and how the sink
// registers what the end of a Drain's context runs (context.AfterFunc
// when nil), which a test can hold back.
type sinkSeams struct {
	open              func(Endpoint, Config) (*Device, error)
	now               func() time.Time
	producerWaits     func()
	producerPublishes func()
	afterFunc         func(context.Context, func()) func() bool
}

// OpenSink opens cfg.Endpoint for cfg.Transport and starts the sink's
// writer. ctx bounds the sink's life: when it ends, the sink drops the
// stream, closes the device and ends (ReasonCancelled).
func OpenSink(ctx context.Context, cfg SinkConfig) (*Sink, error) {
	return openSink(ctx, cfg, sinkSeams{open: Open, now: time.Now})
}

// resolve checks cfg and fills in its defaults.
func (cfg SinkConfig) resolve() (SinkConfig, error) {
	bad := func(format string, args ...any) (SinkConfig, error) {
		return cfg, &Error{Reason: ReasonBadConfig, Op: "open", Endpoint: cfg.Endpoint, Detail: fmt.Sprintf(format, args...)}
	}
	t := cfg.Transport
	switch {
	case t.Kind != format.KindPCM:
		return bad("the sink plays PCM, not %s: native DSD and DoP are later work", t.Kind)
	case t.Channels != Channels:
		return bad("the sink plays %d channels, not %d", Channels, t.Channels)
	case !carries(t.Format, t.ValidBits):
		return bad("%s does not carry %d valid bits: S16_LE carries 16, S24_3LE and S24_LE 24, S32_LE 24 or 32", t.Format, t.ValidBits)
	}
	if cfg.Lookahead == 0 {
		cfg.Lookahead = DefaultLookahead
	}
	if cfg.Lookahead < minLookahead || cfg.Lookahead > maxLookahead {
		return bad("a lookahead of %v is outside %v..%v", cfg.Lookahead, minLookahead, maxLookahead)
	}
	if cfg.ReleaseAfter == 0 {
		cfg.ReleaseAfter = DefaultReleaseAfter
	}
	if cfg.ReleaseAfter < minReleaseAfter || cfg.ReleaseAfter > maxReleaseAfter {
		return bad("a release after %v is outside %v..%v", cfg.ReleaseAfter, minReleaseAfter, maxReleaseAfter)
	}
	switch cfg.Pause {
	case PauseAuto, PauseKernel, PauseZeros:
	default:
		return bad("pause mode %d is none of auto, pause and zeros", int(cfg.Pause))
	}
	return cfg, nil
}

// openSink is OpenSink over its seams.
func openSink(ctx context.Context, cfg SinkConfig, seams sinkSeams) (*Sink, error) {
	cfg, err := cfg.resolve()
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, &Error{Reason: ReasonCancelled, Op: "open", Endpoint: cfg.Endpoint, Err: err}
	}
	devCfg := Config{RateHz: cfg.Transport.RateHz, Format: cfg.Transport.Format, BufferTime: cfg.BufferTime}
	dev, err := seams.open(cfg.Endpoint, devCfg)
	if err != nil {
		return nil, err
	}
	p := dev.Params()
	if err := checkSetup(cfg, p); err != nil {
		_ = dev.Close()
		return nil, err
	}
	fb, keep := p.FrameBytes(), int64(p.BufferFrames)
	frames := int64(framesIn(uint32(cfg.Transport.RateHz), cfg.Lookahead)) + keep
	if frames*int64(fb) > MaxRingBytes {
		frames = MaxRingBytes / int64(fb)
	}
	if frames < keep+int64(p.PeriodFrames) {
		_ = dev.Close()
		return nil, &Error{Reason: ReasonBadConfig, Op: "open", Endpoint: cfg.Endpoint, Detail: fmt.Sprintf(
			"a store of at most %d bytes cannot hold the hardware buffer of %d frames and a period of %d more", MaxRingBytes, keep, p.PeriodFrames)}
	}
	if seams.afterFunc == nil {
		seams.afterFunc = context.AfterFunc
	}
	s := &Sink{
		cfg: cfg, devCfg: devCfg, life: ctx,
		open: seams.open, now: seams.now, producerWaits: seams.producerWaits, producerPublishes: seams.producerPublishes, afterFunc: seams.afterFunc,
		dev:     dev,
		zeros:   make([]byte, p.PeriodFrames*fb),
		ring:    newRing(frames, fb, keep),
		params:  p,
		changed: make(chan struct{}),
		kick:    make(chan struct{}, 1),
		room:    make(chan struct{}, 1),
		exited:  make(chan struct{}),
	}
	s.pos.Stream = State(statePrepared)
	go s.loop()
	return s, nil
}

// checkSetup refuses a device the sink cannot use as configured: one
// without PAUSE when only PAUSE may pause.
func checkSetup(cfg SinkConfig, p Params) error {
	if cfg.Pause == PauseKernel && !p.CanPause() {
		return &Error{Reason: ReasonNotSupported, Op: "open", Endpoint: cfg.Endpoint,
			Detail: "the hardware does not offer PAUSE, and the sink is to pause with PAUSE alone"}
	}
	return nil
}

// Transport is the sink's.
func (s *Sink) Transport() Transport { return s.cfg.Transport }

// Next says how a track whose transport is t follows what the sink plays.
func (s *Sink) Next(t Transport) Join { return s.cfg.Transport.Join(t) }

// Params is what the kernel committed for the device the sink has, or
// last had, open.
func (s *Sink) Params() Params {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.params
}

// Changed returns a channel that is closed the next time the sink's state
// changes, so a caller learns of a pause, a release, an xrun or the end
// without asking Position over and over.
func (s *Sink) Changed() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.changed
}

// Write hands the sink whole frames in its transport's format and returns
// the frames it took: all of them, unless an error ends the call. It waits
// while the store is full, at most until ctx ends (ReasonCancelled, the
// frames taken so far counted). It refuses frames once the sink has
// stopped at an xrun (the xrun's error, until Recover), is draining, or is
// done, and frames it was copying when one of those came are not taken:
// a frame counts once it is published, which the sink's state is asked
// again for. The frames are copied: Write keeps nothing of the caller's
// slice and allocates nothing.
func (s *Sink) Write(ctx context.Context, frames []byte) (int, error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	fb := int64(s.ring.frameBytes)
	if int64(len(frames))%fb != 0 {
		return 0, &Error{Reason: ReasonBadConfig, Op: "sink write", Endpoint: s.cfg.Endpoint,
			Detail: fmt.Sprintf("%d bytes is not whole frames of %d bytes", len(frames), fb)}
	}
	total, done := int64(len(frames))/fb, int64(0)
	for done < total {
		if err := ctx.Err(); err != nil {
			return int(done), &Error{Reason: ReasonCancelled, Op: "sink write", Endpoint: s.cfg.Endpoint, Err: err}
		}
		s.mu.Lock()
		if err := s.acceptLocked(); err != nil {
			s.mu.Unlock()
			return int(done), err
		}
		free, at := s.ring.free(), s.ring.sub
		s.mu.Unlock()
		if free == 0 {
			// A token already waiting may be stale: take it and look again
			// before waiting, so a wait is one with nothing pending.
			select {
			case <-s.room:
				continue
			default:
			}
			if s.producerWaits != nil {
				s.producerWaits()
			}
			select {
			case <-s.room:
			case <-ctx.Done():
			}
			continue
		}
		n := min(free, total-done)
		s.ring.put(at, frames[done*fb:(done+n)*fb])
		if s.producerPublishes != nil {
			s.producerPublishes()
		}
		// The sink is asked again under the lock that publishes the frames:
		// a Drain, an xrun or an end that came while they were copied takes
		// none of them.
		s.mu.Lock()
		if err := s.acceptLocked(); err != nil {
			s.mu.Unlock()
			return int(done), err
		}
		s.ring.sub += n
		if s.watching && s.wakeStop != nil {
			s.wakeStop()
		}
		s.mu.Unlock()
		poke(s.kick)
		done += n
	}
	return int(done), nil
}

// acceptLocked is why the sink takes no frames now, or nil.
func (s *Sink) acceptLocked() error {
	switch {
	case s.state == SinkStopped:
		return s.stoppedError("sink write")
	case s.state == SinkXrun || s.state == SinkEnded:
		return &Error{Reason: ReasonOf(s.err), Op: "sink write", Endpoint: s.cfg.Endpoint, Detail: "the sink has stopped: " + s.state.String(), Err: s.err}
	case s.finishing:
		return &Error{Reason: ReasonBadState, Op: "sink write", Endpoint: s.cfg.Endpoint, Detail: "the sink is draining and takes no more frames"}
	}
	return nil
}

// stoppedError is an operation on a sink that was dropped or drained.
func (s *Sink) stoppedError(op string) error {
	return &Error{Reason: ReasonClosed, Op: op, Endpoint: s.cfg.Endpoint, Detail: "the sink has stopped"}
}

// Pause pauses playback: with the kernel's PAUSE when the hardware offers
// it and the mode allows (at once, the frames queued kept), with zeros
// otherwise (after the frames already queued, at most the hardware buffer,
// have played), or, for a stream that has not started, by writing nothing.
// After ReleaseAfter the sink releases the device and keeps the position.
func (s *Sink) Pause() error { return s.command(opPause, nil) }

// Resume plays on from where a pause stopped. A released sink opens the
// device again (busy, gone and every other refusal come back typed, and
// the sink stays released) and writes again first the frames the hardware
// had not played.
func (s *Sink) Resume() error { return s.command(opResume, nil) }

// Recover prepares a stream an xrun stopped and goes on from the next
// content frame. The gap stays counted in Position.Gaps and recorded in
// Stats; nothing is written twice and nothing skipped. A sink the xrun
// stopped while paused stays paused, writing nothing until Resume, and
// releases the device when the pause would have. Until Recover, Pause,
// Resume and Drain answer with the xrun's error, as Write does.
func (s *Sink) Recover() error { return s.command(opRecover, nil) }

// Drain says the producer is done, and waits until everything submitted
// has played: the lookahead is written out, the kernel plays out the
// hardware buffer (Device.Drain, bounded by the buffer's time and
// drainSlack), and the device is released. Until that last part Pause and
// Resume work as ever, and a paused drain waits for the resume (a release
// and a reopen on the way); during it, only a Drop interrupts. ctx ends the
// wait early: the stream is dropped (ReasonCancelled), as it is by a Drop.
// An xrun ends the Drain with the xrun's error, and the sink waits for
// Recover, Drop or Close; a sink an xrun had stopped already answers with
// it too. A paused, released or stopped sink has nothing it may play out
// until it is resumed, and refuses.
func (s *Sink) Drain(ctx context.Context) error { return s.command(opDrain, ctx) }

// Drop stops at once: the stream is dropped and the device released, and
// the lookahead is never written. The sink is done. Dropping a sink that
// is done does nothing.
func (s *Sink) Drop() error { return s.command(opDrop, nil) }

// Close drops a sink that is not done and waits for its writer to exit.
// A second Close does nothing.
func (s *Sink) Close() error {
	err := s.Drop()
	<-s.exited
	return err
}

// command hands op to the writer and waits for its answer. What the state
// refuses outright is refused here: everything once the sink is done (a
// Drop of it does nothing), a second Drain, and all but a Drop while the
// kernel plays out the last buffer, whose wait a command would cut short.
func (s *Sink) command(op sinkOp, ctx context.Context) error {
	name := sinkOpNames[op]
	s.mu.Lock()
	switch {
	case s.state == SinkStopped:
		s.mu.Unlock()
		if op == opDrop {
			return nil
		}
		return s.stoppedError(name)
	case s.state == SinkEnded:
		err := s.err
		s.mu.Unlock()
		if op == opDrop {
			return nil
		}
		return &Error{Reason: ReasonOf(err), Op: name, Endpoint: s.cfg.Endpoint, Detail: "the sink has ended", Err: err}
	case s.finalDrain && op != opDrop:
		s.mu.Unlock()
		return &Error{Reason: ReasonBadState, Op: name, Endpoint: s.cfg.Endpoint, Detail: "the sink is playing out its last frames: only a drop stops it"}
	case s.finishing && op == opDrain:
		s.mu.Unlock()
		return &Error{Reason: ReasonBadState, Op: name, Endpoint: s.cfg.Endpoint, Detail: "the sink is draining already"}
	}
	c := &sinkCmd{op: op, ctx: ctx, reply: make(chan error, 1)}
	s.cmds = append(s.cmds, c)
	if s.wakeStop != nil {
		s.wakeStop()
	}
	s.mu.Unlock()
	poke(s.kick)
	return <-c.reply
}

// Position returns the sink's account of what it has played.
func (s *Sink) Position() Position {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.pos
	p.State, p.Err = s.state, s.err
	p.Written, p.Submitted = s.ring.wr, s.ring.sub
	p.RateHz = s.cfg.Transport.RateHz
	p.Gaps = s.ngaps
	return p
}

// Stats returns the sink's diagnostics.
func (s *Sink) Stats() SinkStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := SinkStats{
		Params:     s.params,
		RingFrames: s.ring.frames, LookaheadFrames: s.ring.frames - s.ring.keep, KeptFrames: s.ring.keep,
		RingBytes: len(s.ring.buf),
		Counters:  addCounters(s.counters, s.live),
		Silence:   s.silence,
		Pauses:    s.pauses,
		Releases:  s.releases,
		Reopens:   s.reopens,
		PausedBy:  s.pausedBy,
	}
	for i := max(0, s.ngaps-maxGaps); i < s.ngaps; i++ {
		st.Gaps = append(st.Gaps, s.gaps[i%maxGaps])
	}
	return st
}

// poke wakes whoever waits on ch, once, without waiting.
func poke(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}
