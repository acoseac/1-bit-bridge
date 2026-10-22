package playback

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/player/binding"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
)

// session is one playback session: the occurrences the engine handed it, in
// play order, the sinks it opened for them, and the goroutines that play
// and report them, the producer and the reporter, beside one opener a
// successor.
type session struct {
	b  *Backend
	id engine.SessionID

	// ctx is the session's life: every goroutine it starts and every sink it
	// opens ends with it, at its release.
	ctx    context.Context
	cancel context.CancelFunc

	startDone chan struct{} // closed when Start returns
	down      chan struct{} // closed once the session holds nothing

	// Set by Start before anything is opened, then only read.
	ev   engine.Events
	opts engine.SessionOptions

	mu        sync.Mutex
	started   bool
	releasing bool
	lateAck   bool // Release answered its context's error: the teardown acknowledges

	chain   []*occ
	writing *occ // the occurrence the producer writes, nil between occurrences
	// endAfter is the occurrence nothing follows (a Prepare with no
	// successor), 0 for none.
	endAfter engine.OccurrenceID
	ending   bool // the producer plays out the last content: Prepare answers ErrSessionEnding
	drained  bool // everything handed off has played out
	fault    *engine.Fault
	// decodeErr is a decode that failed after content played: the producer
	// plays out what was decoded before it, and the session faults.
	decodeErr          *engine.Fault
	faultSent, finSent bool

	cur  *output   // the sink that plays now, nil between two transports
	outs []*output // every sink the session opened

	inWrite bool               // the producer is in a sink's Write, or about to be
	wctx    context.Context    // that Write's context
	wcancel context.CancelFunc // which a cut that needs the producer ends
	reqs    []*cutRequest

	paused   bool
	pauseSeq int
	// The pause of a session that holds no device (paused between two
	// transports, or after the last frames of a sink played out): relStop
	// stops its release timer, relFired is the timer fired, relSent the
	// DeviceReleased reported.
	relStop  func() bool
	relFired bool
	relSent  bool
	between  *occ    // the producer waits, paused, to open a sink for it
	handed   *output // the sink a Resume opened for it

	firstIn   chan struct{} // closed once the first frames are in the sink, or the producer gave up
	firstSent bool
	firstErr  error

	posOcc    engine.OccurrenceID
	posFrames int64

	producerDone bool
	wakeP, wakeR chan struct{}

	wg sync.WaitGroup // the producer, the reporter and the openers
}

// output is one sink a session opened, for one transport.
type output struct {
	s         *session
	sink      *alsapcm.Sink
	transport alsapcm.Transport
	// Under the session's lock.
	submitted int64 // content frames handed to the sink, less those a cut dropped
	drained   bool  // a drain played everything submitted
	closed    bool  // its Close was called

	umu  sync.Mutex
	unit Unit // where it is open: Locate moves it at a reopen
}

// admit is the sink's check after open: the check of the unit the sink is
// opening now.
func (o *output) admit(n alsapcm.NodeInfo) error {
	o.umu.Lock()
	u := o.unit
	o.umu.Unlock()
	if u.Admit == nil {
		return nil
	}
	return u.Admit(n)
}

// locate is the sink's reopen locator: the binding resolved again, and the
// unit kept for the check after open that follows.
func (o *output) locate(ctx context.Context) (alsapcm.Endpoint, error) {
	u, err := o.s.b.cfg.Locator.Locate(ctx)
	if err != nil {
		return alsapcm.Endpoint{}, err
	}
	o.umu.Lock()
	o.unit = u
	o.umu.Unlock()
	return u.Endpoint, nil
}

func (o *output) unitLabel() string {
	o.umu.Lock()
	defer o.umu.Unlock()
	return o.unit.Label
}

// cutRequest is a cut the producer is to make, between two of its writes.
type cutRequest struct {
	o    *occ
	done chan cutAnswer
}

type cutAnswer struct {
	ok  bool
	err error
}

func newSession(b *Backend, id engine.SessionID) *session {
	ctx, cancel := context.WithCancel(context.Background())
	return &session{
		b: b, id: id, ctx: ctx, cancel: cancel,
		startDone: make(chan struct{}), down: make(chan struct{}), firstIn: make(chan struct{}),
		wakeP: make(chan struct{}, 1), wakeR: make(chan struct{}, 1),
	}
}

// wake pokes whoever waits on ch, once, without waiting.
func wake(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// bound is ctx ended also by the session's release.
func (s *session) bound(ctx context.Context) (context.Context, func()) {
	c, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(s.ctx, cancel)
	return c, func() {
		stop()
		cancel()
	}
}

// call runs f, a command to a sink, on a goroutine of its own and answers
// with what it returned, or with ctx's error once ctx ends: a sink whose
// writer the kernel holds answers no command, and the engine's call must
// not wait for it. f's goroutine then outlives the call, abandoned and
// counted (Stats.Goroutines), until the kernel returns.
func (s *session) call(ctx context.Context, f func() error) error {
	done := make(chan error, 1)
	s.b.spawn(func() { done <- f() })
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// newOccLocked appends an occurrence to the chain. s.mu is held.
func (s *session) newOccLocked(eo engine.Occurrence) *occ {
	o := &occ{id: eo.ID, entry: eo.Entry, ref: eo.Ref, pos: len(s.chain), done: make(chan struct{})}
	s.chain = append(s.chain, o)
	return o
}

// find is the occurrence id, nil when the session never had it.
func (s *session) find(id engine.OccurrenceID) *occ {
	for _, o := range s.chain {
		if o.id == id {
			return o
		}
	}
	return nil
}

// followingLocked is the first occurrence after o that is not dropped.
func (s *session) followingLocked(o *occ) *occ {
	for _, x := range s.chain[o.pos+1:] {
		if !x.dropped {
			return x
		}
	}
	return nil
}

// predecessorLocked is the last occurrence before o that is not dropped.
func (s *session) predecessorLocked(o *occ) *occ {
	for i := o.pos - 1; i >= 0; i-- {
		if x := s.chain[i]; !x.dropped {
			return x
		}
	}
	return nil
}

// tailLocked is the last occurrence not dropped.
func (s *session) tailLocked() *occ {
	for i := len(s.chain) - 1; i >= 0; i-- {
		if x := s.chain[i]; !x.dropped {
			return x
		}
	}
	return nil
}

// start is Start.
func (s *session) start(ctx context.Context, req engine.StartRequest) error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return fmt.Errorf("player backend: session %d started twice", s.id)
	}
	s.started = true
	if s.releasing {
		// Its release came first and holds nothing: this Start acquires
		// nothing either.
		s.mu.Unlock()
		close(s.startDone)
		s.b.letGo(s, true)
		return errReleased
	}
	s.ev, s.opts = req.Events, req.Options
	first := s.newOccLocked(req.First)
	s.mu.Unlock()
	defer close(s.startDone)
	if err := s.b.hold(s); err != nil {
		return err
	}
	cctx, stop := s.bound(ctx)
	defer stop()
	s.b.open(cctx, first)
	close(first.done)
	if first.refused != nil {
		s.mu.Lock()
		first.dropped = true
		s.mu.Unlock()
		u := *first.refused
		return &u
	}
	first.transport = transportFor(first.pcm.RateHz, first.pcm.ValidBits, s.b.preferred(first.pcm.RateHz, first.pcm.ValidBits))
	out, err := s.openOutput(cctx, first)
	if err != nil {
		var te *transportError
		if errors.As(err, &te) {
			return &engine.Unplayable{Reason: ReasonTransport, Detail: te.Error()}
		}
		return fmt.Errorf("player backend: the device could not be opened: %w", err)
	}
	s.mu.Lock()
	if s.releasing {
		// The release came while the device opened: what was opened is
		// closed at once, never written to.
		s.mu.Unlock()
		s.closeOutput(out)
		return errReleased
	}
	s.cur = out
	s.outs = append(s.outs, out)
	first.out, first.start, first.transport = out, 0, out.transport
	s.writing = first
	s.wg.Add(2)
	s.mu.Unlock()
	s.b.spawn(func() {
		defer s.wg.Done()
		s.produce()
	})
	s.b.spawn(func() {
		defer s.wg.Done()
		s.report()
	})
	select {
	case <-s.firstIn:
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.firstErr
	case <-cctx.Done():
		return cctx.Err()
	}
}

// signalFirst tells Start its first frames are in the sink (err nil), or
// that the producer will put none there.
func (s *session) signalFirst(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.firstSent {
		return
	}
	s.firstSent, s.firstErr = true, err
	close(s.firstIn)
}

// transportError is a DAC that took none of a transport's packings.
type transportError struct {
	rate, bits int
	formats    []string
	err        error
}

func (e *transportError) Error() string {
	return fmt.Sprintf("the DAC takes %d-bit PCM at %d Hz in none of %s: %v", e.bits, e.rate, strings.Join(e.formats, ", "), e.err)
}

func (e *transportError) Unwrap() error { return e.err }

// openOutput locates the bound unit and opens a sink on it for o's rate and
// depth, offering the packings in their order (formatsFor) and keeping the
// one the DAC took. The sink's Admit is the unit's check after open, and
// its Locate finds the unit again at a reopen. A DAC that refuses every
// packing is a transportError; any other refusal (busy, the check after
// open, the unit not located) is returned as it is.
func (s *session) openOutput(ctx context.Context, o *occ) (*output, error) {
	u, err := s.b.cfg.Locator.Locate(ctx)
	if err != nil {
		return nil, err
	}
	rate, bits := o.pcm.RateHz, o.pcm.ValidBits
	te := &transportError{rate: rate, bits: bits}
	for _, f := range s.b.formatsFor(rate, bits) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		out := &output{s: s, transport: transportFor(rate, bits, f), unit: u}
		sink, err := s.b.cfg.OpenSink(s.ctx, alsapcm.SinkConfig{
			Endpoint: u.Endpoint, Transport: out.transport, Pause: alsapcm.PauseAuto,
			ReleaseAfter: s.opts.PauseRelease, Admit: out.admit, Locate: out.locate,
		})
		if err == nil {
			out.sink = sink
			s.b.remember(rate, bits, f)
			s.b.notify(Note{Session: s.id, Kind: NoteOpened, Params: sink.Params(), Transport: out.transport, Unit: u.Label})
			return out, nil
		}
		switch alsapcm.ReasonOf(err) {
		case alsapcm.ReasonNotSupported, alsapcm.ReasonMismatch:
			s.b.forget(rate, bits, f)
			te.formats, te.err = append(te.formats, string(f)), err
			continue
		}
		return nil, err
	}
	return nil, te
}

// closeOutput closes out's sink, once, and says so. A sink whose writer the
// kernel holds in a call never returns from Close: the goroutine closing it
// is then abandoned.
func (s *session) closeOutput(out *output) {
	s.mu.Lock()
	if out.closed {
		s.mu.Unlock()
		return
	}
	out.closed = true
	s.mu.Unlock()
	_ = out.sink.Close()
	s.b.notify(Note{Session: s.id, Kind: NoteClosed, Params: out.sink.Params(), Transport: out.transport, Unit: out.unitLabel()})
}

// drainOutput has out play everything submitted. A paused sink holds what
// it was given, so a drain it refuses for its pause waits for the resume,
// asking again when its state has changed since the refusal. The drain is
// a call (call): a sink whose writer the kernel holds answers no command,
// and the producer leaves at the session's release all the same.
func (s *session) drainOutput(out *output) error {
	for {
		changed := out.sink.Changed()
		err := s.call(s.ctx, func() error { return out.sink.Drain(s.ctx) })
		switch {
		case err == nil:
			return nil
		case alsapcm.ReasonOf(err) == alsapcm.ReasonBadState:
			st := out.sink.Position().State
			select {
			case <-changed:
				continue
			default:
			}
			if st != alsapcm.SinkPaused && st != alsapcm.SinkReleased {
				return err
			}
			select {
			case <-changed:
			case <-s.ctx.Done():
				return s.ctx.Err()
			}
		default:
			return err
		}
	}
}

// prepare is Prepare.
func (s *session) prepare(ctx context.Context, after engine.OccurrenceID, next *engine.Occurrence) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.callableLocked(); err != nil {
		return err
	}
	if s.ending || s.drained || s.fault != nil || s.decodeErr != nil {
		return engine.ErrSessionEnding
	}
	a := s.find(after)
	if a == nil || a.dropped {
		return fmt.Errorf("player backend: occurrence %d is not one session %d plays", after, s.id)
	}
	if f := s.followingLocked(a); f != nil {
		ok, err := s.cutFromLocked(ctx, f)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("player backend: occurrence %d's frames have been or will be played, so nothing replaces it", f.id)
		}
		if err := s.callableLocked(); err != nil {
			return err
		}
		if s.ending || a.dropped {
			return engine.ErrSessionEnding
		}
	}
	if next == nil {
		s.endAfter = a.id
	} else {
		s.endAfter = 0
		o := s.newOccLocked(*next)
		octx, cancel := context.WithCancel(s.ctx)
		o.cancel = cancel
		s.wg.Add(1)
		s.b.spawn(func() {
			defer s.wg.Done()
			s.openSuccessor(octx, o)
		})
	}
	wake(s.wakeP)
	return nil
}

// callableLocked refuses a call to a session being released.
func (s *session) callableLocked() error {
	if s.releasing {
		return errReleased
	}
	return nil
}

// openSuccessor opens o, a successor, on its own goroutine, and decides
// how it follows the occurrence before it: gaplessly when its transport is
// that occurrence's sink's, the packing it would take included; otherwise
// the producer drains that sink, closes it and opens one for o, an honest
// gap. The reporter reports Prepared or Refused.
func (s *session) openSuccessor(ctx context.Context, o *occ) {
	defer o.cancel()
	s.b.open(ctx, o)
	s.mu.Lock()
	defer func() {
		close(o.done)
		s.mu.Unlock()
		wake(s.wakeP)
		wake(s.wakeR)
	}()
	if o.dropped {
		closeOcc(o)
		return
	}
	if o.refused != nil {
		o.dropped = true
		closeOcc(o)
		return
	}
	rate, bits := o.pcm.RateHz, o.pcm.ValidBits
	var before alsapcm.Transport
	if p := s.predecessorLocked(o); p != nil {
		before = p.transport
		if p.out != nil {
			before = p.out.transport
		}
	}
	f := s.b.preferred(rate, bits)
	if before.RateHz == rate && before.ValidBits == bits {
		f = before.Format
	}
	o.transport = transportFor(rate, bits, f)
	j := before.Join(o.transport)
	o.join = engine.Join{Gapless: j.Gapless, Differs: j.Differs}
}

// cut is Cut.
func (s *session) cut(ctx context.Context, from engine.OccurrenceID) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.callableLocked(); err != nil {
		return false, err
	}
	o := s.find(from)
	if o == nil {
		// Never handed to this session: nothing of it plays.
		return true, nil
	}
	return s.cutFromLocked(ctx, o)
}

// cutFromLocked drops o and everything after it, cutting their frames from
// the sink where they are, and reports whether it could: false once a frame
// of o has been or will be played. Frames in the sink are cut at o's first
// frame (alsapcm.Sink.Cut), which only the producer side may do: here when
// the producer is not in a Write, and otherwise by the producer, whose Write
// the request ends, at once. s.mu is held, and let go while the producer
// answers.
func (s *session) cutFromLocked(ctx context.Context, o *occ) (bool, error) {
	switch {
	case o.dropped:
		return true, nil
	case o.crossed:
		return false, nil
	case o.written == 0 && (s.writing != o || !s.inWrite):
		s.dropLocked(o)
		return true, nil
	case !s.inWrite || s.producerDone:
		return s.sinkCutLocked(o)
	}
	r := &cutRequest{o: o, done: make(chan cutAnswer, 1)}
	s.reqs = append(s.reqs, r)
	if s.wcancel != nil {
		s.wcancel()
	}
	s.mu.Unlock()
	wake(s.wakeP)
	var a cutAnswer
	select {
	case a = <-r.done:
	case <-ctx.Done():
		a = cutAnswer{err: ctx.Err()}
	}
	s.mu.Lock()
	return a.ok, a.err
}

// sinkCutLocked cuts o's frames from its sink, the producer side free: true
// and o dropped with everything after it when none of them has reached the
// kernel, false and nothing dropped otherwise. A sink played out, or
// closed, has played o: false.
func (s *session) sinkCutLocked(o *occ) (bool, error) {
	switch {
	case o.dropped:
		return true, nil
	case o.crossed:
		return false, nil
	case o.out == nil:
		s.dropLocked(o)
		return true, nil
	case o.out != s.cur || o.out.drained || o.out.closed:
		return false, nil
	}
	ok, err := o.out.sink.Cut(o.start)
	if err != nil || !ok {
		return false, err
	}
	o.out.submitted = o.start
	s.dropLocked(o)
	return true, nil
}

// dropLocked marks o and everything after it dropped: an open under way is
// cancelled and closes what it opened itself; one finished and not taken is
// closed here; the occurrence the producer writes is the producer's to
// close. An end named after a dropped occurrence goes with it: the content
// before o has no successor until the next Prepare.
func (s *session) dropLocked(o *occ) {
	for _, x := range s.chain[o.pos:] {
		if x.dropped {
			continue
		}
		x.dropped = true
		if x.cancel != nil {
			x.cancel()
		}
		if x.opened() && x != s.writing {
			closeOcc(x)
		}
		if s.endAfter == x.id {
			s.endAfter = 0
		}
	}
	wake(s.wakeP)
	wake(s.wakeR)
}

// pause is Pause.
func (s *session) pause(ctx context.Context) error {
	s.mu.Lock()
	if err := s.callableLocked(); err != nil {
		s.mu.Unlock()
		return err
	}
	out := s.cur
	if out != nil && (out.drained || out.closed) {
		out = nil
	}
	s.mu.Unlock()
	if out != nil {
		err := s.call(ctx, out.sink.Pause)
		if err == nil {
			s.mu.Lock()
			s.pausedLocked(false)
			s.mu.Unlock()
			return nil
		}
		// A sink playing out its last frames takes no pause, and one that has
		// played them out holds no device: the session pauses after them, with
		// the device let go. Anything else is the pause's refusal.
		st := out.sink.Position().State
		if !(st == alsapcm.SinkDraining && alsapcm.ReasonOf(err) == alsapcm.ReasonBadState) && st != alsapcm.SinkStopped {
			return err
		}
	}
	s.mu.Lock()
	s.pausedLocked(true)
	s.mu.Unlock()
	return nil
}

// pausedLocked records a pause; one that holds no device arms the release
// it reports once the pause release has passed, as a sink's own would.
func (s *session) pausedLocked(noDevice bool) {
	s.paused = true
	s.pauseSeq++
	s.relSent, s.relFired = false, false
	if s.relStop != nil {
		s.relStop()
		s.relStop = nil
	}
	if noDevice {
		seq := s.pauseSeq
		s.relStop = s.b.cfg.AfterFunc(s.opts.PauseRelease, func() {
			s.mu.Lock()
			if s.paused && s.pauseSeq == seq {
				s.relFired = true
			}
			s.mu.Unlock()
			wake(s.wakeR)
		})
	}
}

// unpausedLocked records a resume.
func (s *session) unpausedLocked() {
	s.paused = false
	s.relSent, s.relFired = false, false
	if s.relStop != nil {
		s.relStop()
		s.relStop = nil
	}
}

// ResumeError is a resume the backend refused: the session stays paused,
// and Code says why (busy, wrong_device, not_located, no_device,
// permission_denied, transport_not_supported or device_error).
type ResumeError struct {
	Code string
	Err  error
}

// resumeCodes says what each code means to a person.
var resumeCodes = map[string]string{
	"busy":                    "another process holds the device (MPD, say)",
	"wrong_device":            "the check after open refused the node opened: it is not the bound unit",
	"not_located":             "the binding names no unit now",
	"no_device":               "the device's node is not there",
	"permission_denied":       "this process may not open the device",
	"transport_not_supported": "the DAC does not take the content's transport",
	"device_error":            "the device could not be opened again",
}

func (e *ResumeError) Error() string {
	return fmt.Sprintf("player: the resume was refused, and the session stays paused: %s: %s: %v", e.Code, resumeCodes[e.Code], e.Err)
}

func (e *ResumeError) Unwrap() error { return e.Err }

// resumeError types a refused reopen.
func resumeError(err error) error {
	code := "device_error"
	var ue *UnitError
	var te *transportError
	switch {
	case errors.As(err, &ue):
		code = "not_located"
	case errors.As(err, &te):
		code = "transport_not_supported"
	default:
		switch alsapcm.ReasonOf(err) {
		case alsapcm.ReasonBusy:
			code = "busy"
		case alsapcm.ReasonWrongDevice:
			code = "wrong_device"
		case alsapcm.ReasonNotLocated:
			code = "not_located"
		case alsapcm.ReasonNoDevice, alsapcm.ReasonNotPCM:
			code = "no_device"
		case alsapcm.ReasonPermission:
			code = "permission_denied"
		}
	}
	return &ResumeError{Code: code, Err: err}
}

// resume is Resume.
func (s *session) resume(ctx context.Context) (engine.ResumeResult, error) {
	s.mu.Lock()
	if err := s.callableLocked(); err != nil {
		s.mu.Unlock()
		return 0, err
	}
	out, between := s.cur, s.between
	if out != nil && (out.drained || out.closed) {
		out = nil
	}
	s.mu.Unlock()
	if out != nil {
		released := out.sink.Position().State == alsapcm.SinkReleased
		err := s.call(ctx, out.sink.Resume)
		if err == nil {
			s.mu.Lock()
			s.unpausedLocked()
			s.mu.Unlock()
			wake(s.wakeR)
			if released {
				s.b.notify(Note{Session: s.id, Kind: NoteReopened, Params: out.sink.Params(), Transport: out.transport, Unit: out.unitLabel()})
				return engine.ResumeReopened, nil
			}
			return engine.ResumeContinued, nil
		}
		// A sink playing out its last frames, or done with them, held no
		// pause (pause): the session was paused with no device, and resumes
		// as one.
		st := out.sink.Position().State
		if !(st == alsapcm.SinkDraining && alsapcm.ReasonOf(err) == alsapcm.ReasonBadState) && st != alsapcm.SinkStopped {
			return 0, resumeError(err)
		}
	}
	if between == nil {
		s.mu.Lock()
		s.unpausedLocked()
		s.mu.Unlock()
		wake(s.wakeP)
		wake(s.wakeR)
		return engine.ResumeContinued, nil
	}
	// Paused between two transports: the resume opens the next one's sink
	// itself, so a device another process took meanwhile refuses the resume
	// and leaves the session paused.
	cctx, stop := s.bound(ctx)
	defer stop()
	next, err := s.openOutput(cctx, between)
	if err != nil {
		var te *transportError
		if !errors.As(err, &te) {
			return 0, resumeError(err)
		}
		s.mu.Lock()
		if !between.dropped {
			between.refused = &engine.Unplayable{Reason: ReasonTransport, Detail: te.Error()}
			between.dropped = true
			closeOcc(between)
		}
		s.unpausedLocked()
		s.mu.Unlock()
		wake(s.wakeP)
		wake(s.wakeR)
		return engine.ResumeContinued, nil
	}
	s.mu.Lock()
	if s.releasing || s.between != between || between.dropped {
		s.mu.Unlock()
		s.closeOutput(next)
		s.mu.Lock()
		s.unpausedLocked()
		s.mu.Unlock()
		wake(s.wakeP)
		return engine.ResumeContinued, nil
	}
	s.handed = next
	s.unpausedLocked()
	s.mu.Unlock()
	wake(s.wakeP)
	wake(s.wakeR)
	return engine.ResumeReopened, nil
}

// release is Release.
func (s *session) release(ctx context.Context) error {
	s.mu.Lock()
	first := !s.releasing
	s.releasing = true
	if first && !s.started {
		// Its Start has not begun: nothing is held, and that Start, when it
		// comes, acquires nothing.
		close(s.down)
		s.mu.Unlock()
		s.cancel()
		return nil
	}
	s.mu.Unlock()
	// The session's life ends: its sinks drop their streams and close their
	// devices on their own, and its goroutines return.
	s.cancel()
	if first {
		s.b.spawn(s.teardown)
	}
	select {
	case <-s.down:
		return nil
	case <-ctx.Done():
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.down:
		return nil
	default:
	}
	s.lateAck = true
	return ctx.Err()
}

// teardown waits for Start, joins the session's goroutines, closes every
// decoder and content and then every sink it opened, ends its hold of the
// device, and acknowledges the release: at once when Release still waits,
// through Events.Released when Release has answered its context's error. A
// sink the kernel holds in a call keeps it in Close, abandoned, its files
// already closed, and the release is never acknowledged: the session still
// holds the device.
func (s *session) teardown() {
	s.mu.Lock()
	started := s.started
	s.mu.Unlock()
	if started {
		<-s.startDone
	}
	s.wg.Wait()
	s.mu.Lock()
	for _, o := range s.chain {
		closeOcc(o)
	}
	outs := slices.Clone(s.outs)
	if s.handed != nil {
		outs = append(outs, s.handed)
		s.handed = nil
	}
	if s.relStop != nil {
		s.relStop()
		s.relStop = nil
	}
	s.mu.Unlock()
	for _, out := range outs {
		s.closeOutput(out)
	}
	s.b.letGo(s, started)
	s.mu.Lock()
	close(s.down)
	late, ev := s.lateAck, s.ev
	s.mu.Unlock()
	if late && ev != nil {
		ev.Released(s.id)
	}
}

// faultReason is the engine's reason for a sink that stopped.
func faultReason(err error) engine.Reason {
	var ue *UnitError
	if errors.As(err, &ue) && ue.State == binding.StateDeviceMissing {
		return engine.ReasonDisconnected
	}
	switch alsapcm.ReasonOf(err) {
	case alsapcm.ReasonXrun:
		return engine.ReasonXrun
	case alsapcm.ReasonDisconnected, alsapcm.ReasonNoDevice:
		return engine.ReasonDisconnected
	}
	return engine.ReasonDeviceError
}

// errorText is err's text, "" for nil.
func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
