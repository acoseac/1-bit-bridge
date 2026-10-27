package playback

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"runtime/debug"
	"sync"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/logging"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
)

// MeasureEvery is how often a session measures what its sink has played,
// for the boundaries it reports and the position, when Config.Tick is nil:
// the sink's own measurement comes about once a period, 50 ms by default.
const MeasureEvery = 50 * time.Millisecond

// Config is what New builds a backend from.
type Config struct {
	// Resolver opens the content an occurrence's reference names. Required.
	Resolver Resolver
	// Locator resolves the configured binding to the unit to open.
	// Required.
	Locator Locator
	// OpenSink opens a sink; nil is alsapcm.OpenSink.
	OpenSink func(ctx context.Context, cfg alsapcm.SinkConfig) (*alsapcm.Sink, error)
	// Tick is what a session measures its sink's position on: a channel that
	// ticks and the function that stops it. Nil ticks every MeasureEvery.
	Tick func() (<-chan time.Time, func())
	// AfterFunc arms the timer that reports a paused session that holds no
	// device (paused between two transports) released once its pause
	// release has passed; nil is time.AfterFunc.
	AfterFunc func(d time.Duration, f func()) (stop func() bool)
	// Notify, when set, is told of each sink a session opens, closes, lets
	// go of during a pause, or reopens, on the goroutine that did it: a
	// diagnostic observer, as the harness's printer. It must not block.
	Notify func(Note)
	// Log is where the backend logs; nil is the player component.
	Log *slog.Logger
	// Seams are where a test holds the backend between two of its steps,
	// to reproduce an interleaving that timing alone reaches only rarely;
	// nil in production.
	Seams *Seams
}

// Seams are the backend's test seams (Config.Seams). Each runs on the
// goroutine that reaches it, and may block to hold that goroutine there.
type Seams struct {
	// SinkCall observes the context bounding a call to a sink, before the
	// command starts. The command itself has no context argument; call owns
	// this bound while its goroutine waits for the sink's answer. An optional
	// returned observer receives the result on the caller's goroutine.
	SinkCall func(context.Context) func(error)
	// PauseRefused runs in Pause once a sink has refused it because it is
	// playing out its last frames or has played them out, before Pause
	// decides what the refusal means.
	PauseRefused func()
	// TransportDrained runs in the producer once the sink it drained for a
	// change of transport has played out its last frames, still the current
	// one, before the producer closes it and records the occurrence it opens
	// a sink for next.
	TransportDrained func()
}

// pauseRefused is the PauseRefused seam.
func (b *Backend) pauseRefused() {
	if sm := b.cfg.Seams; sm != nil && sm.PauseRefused != nil {
		sm.PauseRefused()
	}
}

// transportDrained is the TransportDrained seam.
func (b *Backend) transportDrained() {
	if sm := b.cfg.Seams; sm != nil && sm.TransportDrained != nil {
		sm.TransportDrained()
	}
}

// NoteKind is what a Note says happened to a sink.
type NoteKind int

const (
	// NoteOpened is a sink opened: the device opened and checked, before a
	// frame.
	NoteOpened NoteKind = iota + 1
	// NoteClosed is a sink closed: the device closed, its stream dropped or
	// drained.
	NoteClosed
	// NoteReleased is a paused sink that let the device go after its pause
	// release.
	NoteReleased
	// NoteReopened is a released sink that opened the device again on a
	// resume, at the endpoint the binding resolves to now.
	NoteReopened
)

func (k NoteKind) String() string {
	switch k {
	case NoteOpened:
		return "opened"
	case NoteClosed:
		return "closed"
	case NoteReleased:
		return "released"
	case NoteReopened:
		return "reopened"
	}
	return fmt.Sprintf("note %d", int(k))
}

// Note is one thing a session's sink did, for Config.Notify.
type Note struct {
	Session engine.SessionID
	Kind    NoteKind
	// Params is what the kernel committed for the device the sink has, or
	// last had, open.
	Params    alsapcm.Params
	Transport alsapcm.Transport
	// Unit names the bound unit the sink was opened on.
	Unit string
}

// StatusPath is the kernel's status file of the substream a Note's device
// opened, which reads "closed" once nothing holds it.
func (n Note) StatusPath() string {
	return fmt.Sprintf("/proc/asound/card%d/pcm%dp/sub%d/status", n.Params.Endpoint.Card, n.Params.Endpoint.Device, n.Params.Subdevice)
}

// Backend plays the engine's sessions (engine.Backend) over the decoders and
// alsapcm.Sink, PCM only. It is the package documentation's contract, term
// by term.
type Backend struct {
	nativeAdmission func(format.NativeDSD) (engine.NativePlan, error)
	nativeOpen      func(context.Context, alsapcm.DSDConfig) (*alsapcm.DSDSink, error)
	cfg             Config
	log             *slog.Logger

	mu       sync.Mutex
	sessions map[engine.SessionID]*session
	// holder is the session that holds the device or is opening it, until
	// its release is done: a Start beside it is refused (ErrOverlap).
	holder              *session
	took                map[packKey]format.ALSAFormat
	overlaps            int
	terminalUnconfirmed bool

	// The goroutines the backend started and that have not returned.
	gmu  sync.Mutex
	live int
	zero chan struct{} // closed while live is 0
}

// New builds a backend.
func New(cfg Config) (*Backend, error) {
	switch {
	case cfg.Resolver == nil:
		return nil, errors.New("player backend: a Resolver is required")
	case cfg.Locator == nil:
		return nil, errors.New("player backend: a Locator is required")
	}
	if cfg.OpenSink == nil {
		cfg.OpenSink = alsapcm.OpenSink
	}
	if cfg.Tick == nil {
		cfg.Tick = func() (<-chan time.Time, func()) {
			t := time.NewTicker(MeasureEvery)
			return t.C, t.Stop
		}
	}
	if cfg.AfterFunc == nil {
		cfg.AfterFunc = func(d time.Duration, f func()) func() bool { return time.AfterFunc(d, f).Stop }
	}
	if cfg.Log == nil {
		cfg.Log = logging.Component("player")
	}
	zero := make(chan struct{})
	close(zero)
	return &Backend{cfg: cfg, log: cfg.Log, sessions: map[engine.SessionID]*session{}, took: map[packKey]format.ALSAFormat{}, zero: zero}, nil
}

// Stats are the backend's counts, for diagnostics and the tests.
type Stats struct {
	// Goroutines is the goroutines the backend started that have not
	// returned: each session's producer, reporter and openers, a release's
	// teardown, and a call waiting on its sink for its caller. A device the
	// kernel holds in a call that never returns keeps the sink's writer in
	// it, and with it the teardown that closes that sink and any call that
	// waits on it: those are abandoned, never joined, and stay counted
	// until the kernel returns.
	Goroutines int
	// Sessions is the sessions the backend knows: started and not released,
	// or released and not yet started (a Release that came first).
	Sessions int
	// Overlaps is the Starts refused because another session still held the
	// device (ErrOverlap): always 0 under the engine's contract.
	Overlaps int
	// Sinks is the sinks the sessions hold, open or closed, and Occurrences
	// the occurrences they hold.
	Sinks, Occurrences int
}

// Stats is the backend now.
func (b *Backend) Stats() Stats {
	b.mu.Lock()
	st := Stats{Sessions: len(b.sessions), Overlaps: b.overlaps}
	ss := make([]*session, 0, len(b.sessions))
	for _, s := range b.sessions {
		ss = append(ss, s)
	}
	b.mu.Unlock()
	// A session's lock is taken after the backend's is let go: a session
	// takes the backend's under its own.
	for _, s := range ss {
		s.mu.Lock()
		st.Sinks += len(s.outs)
		st.Occurrences += len(s.chain)
		s.mu.Unlock()
	}
	b.gmu.Lock()
	st.Goroutines = b.live
	b.gmu.Unlock()
	return st
}

// Wait waits until no goroutine the backend started runs, at most until ctx
// ends: then it says how many still run, those an abandoned kernel call
// holds among them (Stats.Goroutines). The owner calls it once the engine's
// Run has returned.
func (b *Backend) Wait(ctx context.Context) error {
	b.gmu.Lock()
	z, n := b.zero, b.live
	b.gmu.Unlock()
	select {
	case <-z:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("player backend: %d goroutines still run (a sink held in a kernel call keeps its teardown): %w", n, ctx.Err())
	}
}

// panicMessage is the line a panic the backend recovers is logged with.
const panicMessage = "player: a panic was recovered in the backend; the session faults and lets go of the device"

// logPanic logs a panic recovered on one of the backend's goroutines, once:
// where, its stack, and its value only when it is the runtime's own error,
// which carries nothing the backend was handed (a value code panics with
// could quote a path).
func (b *Backend) logPanic(where string, r any) {
	attrs := []any{"where", where, "stack", string(debug.Stack())}
	if rerr, ok := r.(runtime.Error); ok {
		attrs = append(attrs, "error", rerr.Error())
	}
	b.log.Error(panicMessage, attrs...)
}

// spawn runs f on a goroutine the backend counts until it returns. Each f
// recovers its own panic as the session's fault (session.recoverFault and
// the rest).
func (b *Backend) spawn(f func()) {
	b.gmu.Lock()
	if b.live == 0 {
		b.zero = make(chan struct{})
	}
	b.live++
	b.gmu.Unlock()
	go func() {
		defer func() {
			b.gmu.Lock()
			b.live--
			if b.live == 0 {
				close(b.zero)
			}
			b.gmu.Unlock()
		}()
		f()
	}()
}

// notify tells Config.Notify of a note.
func (b *Backend) notify(n Note) {
	if b.cfg.Notify != nil {
		b.cfg.Notify(n)
	}
}

// session is id's record, made by whichever of its calls comes first: the
// engine runs each call on a goroutine of its own, so a Release may reach
// the backend before its Start.
func (b *Backend) session(id engine.SessionID) *session {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.sessions[id]
	if s == nil {
		s = newSession(b, id)
		b.sessions[id] = s
	}
	return s
}

// known is id's record, nil when the backend has none: a call for a session
// that never started, or whose release is done.
func (b *Backend) known(id engine.SessionID) *session {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sessions[id]
}

// errUnknownSession answers a call for a session the backend holds no
// record of.
func errUnknownSession(id engine.SessionID) error {
	return fmt.Errorf("player backend: no session %d is playing", id)
}

// errReleased answers a Start whose session's release came first, and a call
// to a session being released.
var errReleased = errors.New("player backend: the session's release came first: nothing was opened")

// hold makes s the session that holds the device, refusing when another
// does (ErrOverlap).
func (b *Backend) hold(s *session) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.terminalUnconfirmed {
		return engine.ErrReleaseUnconfirmed
	}
	if h := b.holder; h != nil && h != s {
		b.overlaps++
		b.log.Error("player backend: a session started while another still held the device", "session", uint64(s.id), "holder", uint64(h.id))
		return fmt.Errorf("%w: session %d has not released it", ErrOverlap, h.id)
	}
	b.holder = s
	return nil
}

// letGo ends s's hold, and its record once its Start has come.
func (b *Backend) letGo(s *session, started bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.holder == s {
		b.holder = nil
	}
	if started && b.sessions[s.id] == s {
		delete(b.sessions, s.id)
	}
}

// Start begins a session: the first occurrence's content opened and its
// first chunk decoded (refused before the device is touched when it cannot
// play), the bound unit located and its device opened, checked and given the
// first frames. It answers once those frames are in the sink.
func (b *Backend) Start(ctx context.Context, req engine.StartRequest) error {
	s := b.session(req.Session)
	return s.start(ctx, req)
}

// Prepare names what follows after: next's content opened and its join
// decided on a goroutine of its own, reported Prepared or Refused, or, with
// next nil, that the session finishes after after. It replaces what followed
// after, cutting it from the sink when its frames are there, and answers
// ErrSessionEnding once the session plays out its last content.
func (b *Backend) Prepare(ctx context.Context, id engine.SessionID, after engine.OccurrenceID, next *engine.Occurrence) error {
	s := b.known(id)
	if s == nil {
		if next != nil {
			next.Reservation.ReleaseBackend()
		}
		return errUnknownSession(id)
	}
	return s.prepare(ctx, after, next)
}

// Cut ends the content before from at its end and discards from and what
// follows it, and answers whether it could: false once a frame of from has
// been or will be played.
func (b *Backend) Cut(ctx context.Context, id engine.SessionID, from engine.OccurrenceID) (bool, error) {
	s := b.known(id)
	if s == nil {
		return false, errUnknownSession(id)
	}
	return s.cut(ctx, from)
}

// Pause pauses the session's sink: the kernel's PAUSE, or zeros.
func (b *Backend) Pause(ctx context.Context, id engine.SessionID) error {
	s := b.known(id)
	if s == nil {
		return errUnknownSession(id)
	}
	return s.pause(ctx)
}

// Resume plays on: continued, or reopened at the endpoint the binding
// resolves to now and checked, or refused with the reason, the session
// left paused.
func (b *Backend) Resume(ctx context.Context, id engine.SessionID) (engine.ResumeResult, error) {
	s := b.known(id)
	if s == nil {
		return 0, errUnknownSession(id)
	}
	return s.resume(ctx)
}

// Release stops the session's sink, closes the device and the decoders,
// joins the session's goroutines, and answers nil once it holds nothing.
// Past ctx's end it answers ctx's error and reports Events.Released once it
// does.
func (b *Backend) Release(ctx context.Context, id engine.SessionID) error {
	return b.session(id).release(ctx)
}

// ReleaseAll releases every session the backend holds, as a Release of each
// would, for an owner that can release none: serve's player calls it once
// its engine's Run has ended in a panic, whose own shutdown, which releases
// the session it plays, never ran. It answers nil once nothing is held, and
// at ctx's end says how many sessions still hold something: their
// teardowns go on, and let go of the device once the kernel lets go of
// their sinks.
func (b *Backend) ReleaseAll(ctx context.Context) error {
	b.mu.Lock()
	ss := make([]*session, 0, len(b.sessions))
	for _, s := range b.sessions {
		ss = append(ss, s)
	}
	b.mu.Unlock()
	held := 0
	for _, s := range ss {
		if err := s.release(ctx); err != nil {
			held++
		}
	}
	if held > 0 {
		return fmt.Errorf("player backend: %d session(s) still hold something past the release's bound: %w", held, ctx.Err())
	}
	return nil
}

// Discard drops what the backend learnt of the bound unit outside a
// session: the packings it took.
func (b *Backend) Discard(context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	clear(b.took)
	return nil
}

var _ engine.Backend = (*Backend)(nil)

// OwnsOccurrenceReservations enables shared engine/backend resource accounting.
func (*Backend) OwnsOccurrenceReservations() bool { return true }
