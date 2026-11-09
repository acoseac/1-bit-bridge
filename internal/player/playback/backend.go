package playback

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/logging"
	"github.com/acoseac/1-bit-bridge/internal/player/decode"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
)

// MeasureEvery is how often a session measures what its sink has played,
// for the boundaries it reports and the position, when Config.Tick is nil:
// the sink's own measurement comes about once a period, 50 ms by default.
const MeasureEvery = 50 * time.Millisecond

// Config is what New builds a backend from.
type Config struct {
	// DSD is production's DSD admission (B728 B7): the player's effective
	// DSD policy, which serve and bridge output play configure, and which
	// reads no approval record. Nil, with no Authorization, refuses all DSD
	// as dsd_not_in_this_release (ReasonDSD).
	DSD *DSDPolicy
	// Authorization is the harness's approved-only native DSD admission
	// (NewQualificationBackend, NewTransitionQualificationBackend, bridge
	// output play's report): it reads the owner's approval record at the
	// harness's version. Production passes DSD and never this; New refuses
	// a config that sets both.
	Authorization *Authorization
	// Resolver opens the content an occurrence's reference names. Required.
	Resolver Resolver
	// Locator resolves the configured binding to the unit to open.
	// Required.
	Locator Locator
	// OpenSink opens a sink; nil is alsapcm.OpenSink.
	OpenSink func(ctx context.Context, cfg alsapcm.SinkConfig) (*alsapcm.Sink, error)
	// OpenNative opens a native DSD or DoP sink for an approved plan; nil is
	// alsapcm.OpenDSDSink.
	OpenNative func(ctx context.Context, cfg alsapcm.DSDConfig) (*alsapcm.DSDSink, error)
	// Probe runs the silent probe (Backend.Probe); nil is alsapcm.Probe.
	Probe func(ep alsapcm.Endpoint, cfg alsapcm.ProbeConfig) (alsapcm.ProbeReport, error)
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
	// Installed observes occurrence boundaries separately from hardware lifecycle notes.
	Installed func(Note)
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
	// ProducerStep runs on session s's producer once it has decided its next
	// step (next), with no lock held, before it takes the step (B728 B9 round
	// 8): step is "stop" (the session faulted or ends), "write", "wait",
	// "transition" or "finish", and o the occurrence the step is for, 0 for
	// none. It may block, holding the producer there.
	ProducerStep func(s engine.SessionID, step string, o engine.OccurrenceID)
	// Fenced runs where the backend finds the session released, by its
	// Release or by the engine's taking it before that Release came
	// (engine.StartRequest.Released), at a step that would acquire, open,
	// admit, adopt, activate, resume, write or cut, and refuses that step
	// (B674 round 3): step names it ("start", "open", "admit", "adopt",
	// "activate", a successor's sink's activation, B728 B9 round 3,
	// "resume", "write", "cut"). A sink refused at its adoption is closed before it
	// runs; one refused at its check after open is closed by the sink after
	// it. It must not call the backend.
	Fenced func(step string)
	// StartWoke runs in a Start's wait for the silent probe (hold) once the
	// probe has ended, before the Start takes the device slot: the window in
	// which a second probe would take the slot from it (B728 B4 round 2).
	StartWoke func()
	// Activating runs in a native or DoP occurrence's activation check
	// (checkDSDActivation) once its read of the policy has returned, before
	// the check judges the occurrence: the window in which another refresh
	// publishes a set, and may reconcile the occurrence under it, which the
	// check must judge it by (B728 B7 round 4).
	Activating func()
	// Published runs in a refresh (refreshDSD) once it has published its set
	// and before it reconciles the sessions' occurrences against it, under
	// the refreshes' lock, so it must not call the backend: the window in
	// which a set is published that no reconciliation has judged an
	// occurrence by yet (B728 B7 round 4).
	Published func()
	// DSDSource, when set, is what a converted occurrence's converter reads
	// in place of the DSD source its open made (B728 B11): a test's wrapper
	// of src, for ref's content. It runs under the session's lock, in the
	// step that ends the open, and must return at once; the source it
	// returns is read on the producer's goroutine, with the session's life.
	DSDSource func(ref engine.Reference, src decode.DSDDecoder) decode.DSDDecoder
	// Converting runs with no lock held at four steps of a converted
	// occurrence's join (B728 B11). On the producer's goroutine, where it may
	// block, holding the producer there: "holding", once o is at its window
	// point, every frame of it before its window in its sink; "decided", once
	// o's join window is decided (continued into a successor or ended) and
	// before the window is read; and "ended", once o's last frame is in its
	// sink, before the producer takes its next step. On the goroutine of the
	// call that cuts: "cut", once a cut of o, whose first bits are in its
	// predecessor's window, is handed to the producer, before the call waits
	// for its answer.
	Converting func(step string, o engine.OccurrenceID)
	// Deciding runs on the producer in the decision of a converted occurrence
	// o's join window (holdLocked), once it has read the choice's write
	// pending and before it reads the set published, under the session's
	// lock (B728 B9 round 18): the window in which a write publishes, or lets
	// its pending go, between the decision's two reads. It may block, holding
	// the producer there with that lock held, so nothing that takes the lock (a
	// reconciliation, the reporter's step, a write's classification) goes on
	// until it returns; it must not call the backend.
	Deciding func(o engine.OccurrenceID)
	// Activation runs on the activating goroutine as an occurrence's output
	// is about to be activated, before the activation decides anything of
	// the occurrence (B728 B11 round 2): a test holds it there while a
	// refresh reconciles the occurrence. It may block.
	Activation func(o engine.OccurrenceID)
	// ChoicePending runs in a choice write (ChooseDSD, B728 B9) once the set
	// the write would publish is pending and what it would interrupt is
	// classified, before the state is written: the window in which an
	// occurrence is admitted beside a write. It may block.
	ChoicePending func()
	// ChoiceWritten runs in a choice write once the state's write has
	// returned, err its answer, before the set is published: the window in
	// which the published epoch trails the state. It may block.
	ChoiceWritten func(err error)
	// ChoiceQueued runs in a choice write that finds another write's turn
	// under way, before it waits for it. It must not block.
	ChoiceQueued func()
	// ChoiceAwaited runs where a step of occurrence o's waits for a pending
	// choice write that plays its rate otherwise to settle, its session's
	// lock let go: the step that ends its open, the producer's (a sink's
	// adoption or its activation, a join window's hold, a transport's open,
	// a gapless join) and a resume's open between transports. It must not
	// block.
	ChoiceAwaited func(o engine.OccurrenceID)
	// ChoiceWoke runs where a step that waited for a pending choice write
	// (ChoiceAwaited) has seen it settle, before it asks again whether a
	// write is pending, its session's lock let go (B728 B9 round 2). It may
	// block, holding that goroutine there.
	ChoiceWoke func(o engine.OccurrenceID)
	// Unjudged runs where a step of occurrence o's waits for the
	// reconciliation of a set published since o was admitted to judge it
	// (unjudgedLocked, B728 B9 round 17): the producer's transition before it
	// opens o's own sink, its gapless join and a converted join window above
	// the sink's floor, and a resume's open between transports, its session's
	// lock let go, before the wait. It may block, holding that goroutine
	// there.
	Unjudged func(o engine.OccurrenceID)
	// Opened runs on an opener's goroutine once occurrence o's open, of
	// ref's content, has returned, its admission made or refused, before the
	// step that ends the open takes the session's lock (B728 B9 round 2): the
	// window in which a choice's write publishes beside an open whose
	// admission read the set before it. It may block.
	Opened func(o engine.OccurrenceID, ref engine.Reference)
	// Admitting runs on an opener's goroutine as DSD occurrence o's admission
	// under the effective policy begins (admitDSD), before it reads the set
	// published or asks whether that set is stale, which takes the refreshes'
	// lock (B728 B9 round 18): a test knows by it that an admission a held
	// refresh keeps from ending has begun. It must not block or call the
	// backend.
	Admitting func(o engine.OccurrenceID)
	// Releasing runs in session s's Release once the session is marked
	// released and its native sinks have begun their release, before its
	// life ends, its lock let go (B728 B9 round 2): the window in which a
	// choice's write classifies what a session being released holds. It may
	// block.
	Releasing func(s engine.SessionID)
	// Probed runs on the silent probe's goroutine once its call has
	// returned and before its answers are taken (publishProbe): a test holds
	// it there while a Start takes the slot the probe let go of (B762 round
	// 2). It may block, and must not call the backend.
	Probed func()
	// ProbeDeciding runs on the silent probe's goroutine just before the step
	// that decides its answers (takeProbe: the Starts that took the device
	// slot compared and the cache's Record, one step under the backend's
	// lock), with the probe's lock held: a test takes the slot there through
	// a Start, which voids the answers (B762 round 3). It may block, and must
	// not call the backend.
	ProbeDeciding func()
	// ProbeDecided runs on the silent probe's goroutine just after that
	// step, the backend's lock let go and the probe's still held, before what
	// the probe accepted is written (noteProbed): a test takes the slot there
	// through a Start, which finds the answers recorded (B762 round 3). It may
	// block, and must not call the backend.
	ProbeDecided func()
}

// releasingSeam is the Releasing seam.
func (b *Backend) releasingSeam(s engine.SessionID) {
	if sm := b.cfg.Seams; sm != nil && sm.Releasing != nil {
		sm.Releasing(s)
	}
}

// choiceWoke is the ChoiceWoke seam.
func (b *Backend) choiceWoke(o engine.OccurrenceID) {
	if sm := b.cfg.Seams; sm != nil && sm.ChoiceWoke != nil {
		sm.ChoiceWoke(o)
	}
}

// unjudged is the Unjudged seam.
func (b *Backend) unjudged(o engine.OccurrenceID) {
	if sm := b.cfg.Seams; sm != nil && sm.Unjudged != nil {
		sm.Unjudged(o)
	}
}

// opened is the Opened seam.
func (b *Backend) opened(o *occ) {
	if sm := b.cfg.Seams; sm != nil && sm.Opened != nil {
		sm.Opened(o.id, o.ref)
	}
}

// probed is the Probed seam.
func (b *Backend) probed() {
	if sm := b.cfg.Seams; sm != nil && sm.Probed != nil {
		sm.Probed()
	}
}

// probeDeciding is the ProbeDeciding seam.
func (b *Backend) probeDeciding() {
	if sm := b.cfg.Seams; sm != nil && sm.ProbeDeciding != nil {
		sm.ProbeDeciding()
	}
}

// probeDecided is the ProbeDecided seam.
func (b *Backend) probeDecided() {
	if sm := b.cfg.Seams; sm != nil && sm.ProbeDecided != nil {
		sm.ProbeDecided()
	}
}

// activation is the Activation seam.
func (b *Backend) activation(o engine.OccurrenceID) {
	if sm := b.cfg.Seams; sm != nil && sm.Activation != nil {
		sm.Activation(o)
	}
}

// converting is the Converting seam.
func (b *Backend) converting(step string, o engine.OccurrenceID) {
	if sm := b.cfg.Seams; sm != nil && sm.Converting != nil {
		sm.Converting(step, o)
	}
}

// deciding is the Deciding seam.
func (b *Backend) deciding(o engine.OccurrenceID) {
	if sm := b.cfg.Seams; sm != nil && sm.Deciding != nil {
		sm.Deciding(o)
	}
}

// admitting is the Admitting seam.
func (b *Backend) admitting(o engine.OccurrenceID) {
	if sm := b.cfg.Seams; sm != nil && sm.Admitting != nil {
		sm.Admitting(o)
	}
}

// startWoke is the StartWoke seam.
func (b *Backend) startWoke() {
	if sm := b.cfg.Seams; sm != nil && sm.StartWoke != nil {
		sm.StartWoke()
	}
}

// activating is the Activating seam.
func (b *Backend) activating() {
	if sm := b.cfg.Seams; sm != nil && sm.Activating != nil {
		sm.Activating()
	}
}

// published is the Published seam.
func (b *Backend) published() {
	if sm := b.cfg.Seams; sm != nil && sm.Published != nil {
		sm.Published()
	}
}

// fenced is the Fenced seam.
func (b *Backend) fenced(step string) {
	if sm := b.cfg.Seams; sm != nil && sm.Fenced != nil {
		sm.Fenced(step)
	}
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

// producerStep is the ProducerStep seam, for session id's step st.
func (b *Backend) producerStep(id engine.SessionID, st step) {
	if sm := b.cfg.Seams; sm != nil && sm.ProducerStep != nil {
		var o engine.OccurrenceID
		if st.o != nil {
			o = st.o.id
		}
		sm.ProducerStep(id, st.kind.String(), o)
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
	// NoteInstalled identifies each installed occurrence, including gapless successors.
	NoteInstalled
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
	case NoteInstalled:
		return "installed"
	}
	return fmt.Sprintf("note %d", int(k))
}

// Note is one thing a session's sink did, for Config.Notify.
type Note struct {
	// Plan is detached native policy metadata on an installation observation.
	Plan       *engine.NativePlan
	Occurrence engine.OccurrenceID
	output     *output
	Session    engine.SessionID
	Kind       NoteKind
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

// Backend plays the engine's sessions (engine.Backend) over the decoders,
// alsapcm.Sink for PCM and alsapcm.DSDSink for approved DSD. It is the
// package documentation's contract, term by term.
type Backend struct {
	// dsd is the effective DSD set published now (B728 B7), which selection
	// admits by, swapped in one step, and dsdPub the pair publication reads:
	// that set and its read side (B728 B8), swapped right after it, and alone
	// when the read side alone changes. Neither is nil once New has returned,
	// for a backend with a DSD policy. dsdMu serializes the refreshes that swap
	// them, dsdRead is when the last one read the policy, dsdJudged is the set
	// the last reconciliation that returned judged every session by, under
	// dsdMu (B728 B9 round 18: a set published whose reconciliation did not
	// return, a refresh that panicked between the two, is reconciled by the next
	// refresh, which finds it the same), dsdFenceOnce says once that the state's
	// writes are fenced, and probeState is the silent probe's bookkeeping
	// (ProbeDSD).
	dsd          atomic.Pointer[dsdSet]
	dsdPub       atomic.Pointer[dsdPublished]
	dsdMu        sync.Mutex
	dsdRead      time.Time
	dsdJudged    *dsdSet
	dsdFenceOnce sync.Once
	probeState   dsdProbe
	// choiceTurn is the writes' turn: one choice write at a time (B728 B9,
	// ChooseDSD); pending is the write between its reading and its settle, nil
	// while none is; landedChoice is the state a choice's write left in the
	// state's file, landed and not known to be durable, which every refresh
	// holds the set to until the next start (setOver), nil while there is none.
	choiceTurn   chan struct{}
	pending      atomic.Pointer[pendingChoice]
	landedChoice atomic.Pointer[dsdstate.State]

	policyMu   sync.Mutex
	policy     PlayerPolicy
	liveFacts  *PlaybackFacts
	liveOutput *output
	// liveClipped is the current converted occurrence's count of saturated
	// samples, which its observation reads as the producer counts (B728 B11);
	// nil for every other occurrence.
	liveClipped         *atomic.Int64
	publishedPermission *permission
	cliCandidate        bool
	transitions         *TransitionPlans
	lastEdge            string
	lastPlan            *engine.NativePlan
	nativeAdmission     func(format.NativeDSD) (engine.NativePlan, error)
	nativeOpen          func(context.Context, alsapcm.DSDConfig) (*alsapcm.DSDSink, error)
	cfg                 Config
	log                 *slog.Logger

	mu       sync.Mutex
	sessions map[engine.SessionID]*session
	// holder is the session that holds the device or is opening it, until
	// its release is done: a Start beside it is refused (ErrOverlap), and so
	// is a probe (Probe).
	holder *session
	// probing is the silent probe that holds the device slot, nil while none
	// does (Probe): a Start waits for it, at most ProbeWait, and no other
	// probe starts beside it.
	probing *probeHold
	// waiters counts the Starts that found a probe holding the slot and
	// have not taken it yet: a probe is refused while one waits.
	waiters             int
	took                map[packKey]format.ALSAFormat
	overlaps            int
	terminalUnconfirmed bool
	// starts counts the Starts that took the device slot (hold), directly or
	// once a probe let it go: a probe a Start took the slot from keeps nothing
	// of what it asked (publishProbe, B762). A Start that gave its wait up, or
	// that its context or its release ended first, took nothing and is not
	// counted (round 2). It moves under mu, which the probe's answers are
	// decided under too (takeProbe, round 3).
	starts atomic.Uint64

	// The supervisor's refresh (RequestRefreshDSD, B762): its goroutine, and a
	// forced refresh asked while one ran.
	refreshRun    coalesced
	refreshForced atomic.Bool

	// The goroutines the backend started and that have not returned; those of
	// them that write the player's DSD state, or may start a write
	// (spawnWrite), for which a share of the data directory's lock is kept
	// (DSDWrites, B762); and whether the writes are closed to any asked from
	// now on (CloseDSDWrites, B762 round 2). One lock, so a count of both is
	// one reading (Running).
	gmu     sync.Mutex
	live    int
	zero    chan struct{} // closed while live is 0
	writes  int
	wzero   chan struct{} // closed while writes is 0
	wclosed bool
	// wrefused says, once, that a write came once the writes were closed.
	wrefused sync.Once
}

// New builds a backend.
func New(cfg Config) (*Backend, error) {
	switch {
	case cfg.Resolver == nil:
		return nil, errors.New("player backend: a Resolver is required")
	case cfg.Locator == nil:
		return nil, errors.New("player backend: a Locator is required")
	case cfg.DSD != nil && cfg.Authorization != nil:
		return nil, errors.New("player backend: production's effective DSD policy and the harness's approval admission are not configured together")
	}
	if cfg.OpenSink == nil {
		cfg.OpenSink = alsapcm.OpenSink
	}
	if cfg.OpenNative == nil {
		cfg.OpenNative = alsapcm.OpenDSDSink
	}
	if cfg.Probe == nil {
		cfg.Probe = alsapcm.Probe
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
	reason := "DSD is off"
	if cfg.Authorization == nil {
		reason = string(ReasonDSD)
	}
	b := &Backend{policy: PlayerPolicy{BootID: fmt.Sprintf("%x", randomBoot()), Generation: 1, Reason: reason}, cfg: cfg, log: cfg.Log, sessions: map[engine.SessionID]*session{}, took: map[packKey]format.ALSAFormat{}, zero: zero, wzero: zero,
		choiceTurn: make(chan struct{}, 1)}
	if cfg.DSD != nil {
		// Its read side the state's warnings of the bridge's already, a state
		// that did not read among them, before the first refresh reads the box.
		set, view := noSet("the effective DSD set has not been computed yet", b.dsdState())
		set.gen = 1
		b.dsd.Store(set)
		b.dsdPub.Store(&dsdPublished{set: set, view: view})
		// No session yet, so nothing for a reconciliation to judge by it.
		b.dsdJudged = set
	}
	return b, nil
}

// Stats are the backend's counts, for diagnostics and the tests.
type Stats struct {
	// Goroutines is the goroutines the backend started that have not
	// returned: each session's producer, reporter and openers, a release's
	// teardown, a call waiting on its sink for its caller, the writes of the
	// player's DSD state (a choice's, made on its caller's goroutine, counted
	// as one: dsdWrite), and the silent probe's and the refresh's that the
	// supervisor asks for (RequestProbeDSD, RequestRefreshDSD). A device the
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

// spawnWrite runs f as spawn does, counted among the writes of the player's
// DSD state too (DSDWrites) until it returns: a write (recordDSD), or the
// silent probe's goroutine, which may start one (RequestProbeDSD). It reports
// whether f runs: once the writes are closed (CloseDSDWrites) it runs
// nothing, and says so once whatever asks, what naming the first.
func (b *Backend) spawnWrite(what string, f func()) bool {
	b.gmu.Lock()
	if b.wclosed {
		b.gmu.Unlock()
		b.wrefused.Do(func() { b.dsdLog().Warn(dsdWriteRefused, "what", what) })
		return false
	}
	b.startLocked(true)
	b.gmu.Unlock()
	go b.counted(true, f)
	return true
}

// dsdWrite runs f, a write of the player's DSD state made on its caller's
// goroutine (a choice's, ChooseDSD, B728 B9), counted as a write spawnWrite
// starts is (DSDWrites, Running, Wait) until it returns, a panic's way out
// included, so serve's player's stop keeps its share of the data directory's
// lock until it ends and says so as it says a write left running. It reports
// whether f ran: once the writes are closed (CloseDSDWrites) it runs nothing,
// said once as spawnWrite says it.
func (b *Backend) dsdWrite(what string, f func()) bool {
	b.gmu.Lock()
	if b.wclosed {
		b.gmu.Unlock()
		b.wrefused.Do(func() { b.dsdLog().Warn(dsdWriteRefused, "what", what) })
		return false
	}
	b.startLocked(true)
	b.gmu.Unlock()
	b.counted(true, f)
	return true
}

// Running is how many of the goroutines the backend started have not
// returned, and how many of those are writes of the player's DSD state or may
// start one (spawnWrite), a choice's write on its caller's goroutine counted
// among both (dsdWrite), from one reading: serve's player's stop says what
// each kind left past its bound (B762 round 2).
func (b *Backend) Running() (goroutines, writes int) {
	b.gmu.Lock()
	defer b.gmu.Unlock()
	return b.live, b.writes
}

// spawn runs f on a goroutine the backend counts until it returns. Each f
// recovers its own panic as the session's fault (session.recoverFault and
// the rest).
func (b *Backend) spawn(f func()) {
	b.gmu.Lock()
	b.startLocked(false)
	b.gmu.Unlock()
	go b.counted(false, f)
}

// startLocked counts a goroutine about to start, among the writes too when it
// is one. b.gmu is held.
func (b *Backend) startLocked(write bool) {
	if b.live == 0 {
		b.zero = make(chan struct{})
	}
	b.live++
	if write {
		if b.writes == 0 {
			b.wzero = make(chan struct{})
		}
		b.writes++
	}
}

// counted runs f, and once it returns counts its goroutine done, among the
// writes too when it is one, in one step.
func (b *Backend) counted(write bool, f func()) {
	defer func() {
		b.gmu.Lock()
		defer b.gmu.Unlock()
		if write {
			if b.writes--; b.writes == 0 {
				close(b.wzero)
			}
		}
		if b.live--; b.live == 0 {
			close(b.zero)
		}
	}()
	f()
}

// notify tells Config.Notify of a note.
func (b *Backend) notify(n Note) {
	b.observeNote(n)
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
// does (ErrOverlap). A silent probe that holds the device slot (Probe) is
// waited for, holding nothing, and the slot taken in the step that finds it
// free; the wait lasts at most ProbeWait, armed on Config.AfterFunc, and
// then the device is busy (engine.ErrDeviceBusy), the probe's node still
// open. ctx's end, and s's release, end the wait too. While it waits the
// Start counts among the waiters, whom reserveProbe refuses a probe for, so
// no second probe takes the slot between the first's end and this Start's
// taking it (B728 B4 round 2). It counts the Start (starts) in the step that
// takes the slot, under the lock the probe's answers are decided under
// (takeProbe): a probe whose answers are decided after it keeps nothing of
// what it asked (B762), and one whose answers were decided before, or beside
// a Start that gave its wait up, keeps them (rounds 2 and 3).
func (b *Backend) hold(ctx context.Context, s *session) error {
	var expired chan struct{}
	stop := func() bool { return false }
	waiting := false
	defer func() {
		stop()
		if waiting {
			b.mu.Lock()
			b.waiters--
			b.mu.Unlock()
		}
	}()
	for {
		b.mu.Lock()
		if b.terminalUnconfirmed {
			b.mu.Unlock()
			return engine.ErrReleaseUnconfirmed
		}
		p := b.probing
		if p == nil {
			if waiting {
				b.waiters--
				waiting = false
			}
			if h := b.holder; h != nil && h != s {
				b.overlaps++
				b.log.Error("player backend: a session started while another still held the device", "session", uint64(s.id), "holder", uint64(h.id))
				b.mu.Unlock()
				return fmt.Errorf("%w: session %d has not released it", ErrOverlap, h.id)
			}
			b.holder = s
			b.starts.Add(1)
			b.mu.Unlock()
			return nil
		}
		if !waiting {
			b.waiters++
			waiting = true
		}
		b.mu.Unlock()
		if expired == nil {
			ch := make(chan struct{})
			stop = b.cfg.AfterFunc(ProbeWait, func() { close(ch) })
			expired = ch
		}
		select {
		case <-p.done:
			b.startWoke()
		case <-expired:
			return fmt.Errorf("player backend: the device is busy: the silent probe has held it for %v: %w", ProbeWait, engine.ErrDeviceBusy)
		case <-ctx.Done():
			return ctx.Err()
		case <-s.ctx.Done():
			return errReleased
		case <-s.released:
			return errReleased
		}
	}
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

func randomBoot() []byte {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}
