package alsapcm

import (
	"context"
	"errors"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
	"sync"
	"time"
)

// ReleaseMode controls the native sink's outgoing hardware prefix.
type ReleaseMode int

const (
	// Immediate discards all queued content and interrupts a graceful release.
	Immediate ReleaseMode = iota
	// TrackChange retains only the accepted hardware prefix and a bounded tail.
	TrackChange
)

// ErrReleaseUnconfirmed means cleanup still owns the device, or failed. Never
// open a successor on this answer; Done is the actual cleanup completion fence.
var ErrReleaseUnconfirmed = errors.New("native release unconfirmed")

// ErrActivationFenced is an activation the sink's own fence ended before its
// run's prefill was written whole (B728 B9 round 4): the sink revoked,
// released or being released (an Immediate release cuts a prefill short, the
// cancellation joined to this) or paused, before or while the activation was
// asked, at its admission's end, or before its writer ran it, while the sink's
// life runs and the sink has recorded no failure, a failure it recorded then
// joined to the command's own refusal (fenced, B728 B9 round 8). Once that
// life has ended the fence is never answered: the verdict answers the life's
// cancellation where the cleanup recorded no failure (B765), which a caller
// the life stopped takes for the stop it is, and the failure otherwise, this
// fence's chain dropped from it where the sink recorded the fence too
// (verdict, failureOf; rounds 9 and 10). It is never the device's answer: an
// admission the check refused, a write that failed and a reopen whose
// geometry changed are returned as they are, and so is a sink that failed
// before the activation; and a fence whose own close failed, a reopened
// node's, is that failure (ErrReleaseUnconfirmed), never this (round 9). A
// caller that dropped what it was activating tells by it that the failure was
// the drop's.
var ErrActivationFenced = errors.New("native activation fenced")

const nativeReleaseBudget = 2 * time.Second
const nativeCloseMargin = 500 * time.Millisecond
const nativeLedgerMax = 1024

// MaxDSDPrefill bounds the DSD silence a hardware run starts with
// (DSDConfig.Prefill): the most a reviewed DAC profile may give
// (dacprofile.MaxTimingMs), so a profile's resync delay never fails an open
// at run time (B728 B7: the Chord Hugo 2's is 700 ms, past the 500 ms this
// bound was). The silence is written a period at a time from the writer's
// scratch, so a longer one costs time and no memory.
const MaxDSDPrefill = 5 * time.Second

// DSDConfig is an explicit provisional native transport and lifecycle policy.
// Check re-reads the harness's descriptor and control snapshot after open and
// at activation/reopen. It never writes mixer controls.
type DSDConfig struct {
	// Kind selects native DSD (also the zero default) or explicit DoP.
	Kind                      format.Kind
	Endpoint                  Endpoint
	Format                    format.ALSAFormat
	BitRate                   int
	Pause                     PauseMode
	Tail, Prefill, BufferTime time.Duration
	RingBytes, LedgerLimit    int
	Check                     func(NodeInfo) error
	// Locate resolves the binding afresh before a released pause reopens.
	Locate func(context.Context) (Endpoint, error)
}

// DSDPosition counts logical bits per channel, excluding all silence/padding.
type DSDPosition struct {
	Submitted, Written, Played int64
	Paused, Released           bool
}
type nativeOccurrence struct {
	id  uint64
	end int64
}
type nativeRequest struct {
	ctx        context.Context
	gen        uint64
	resume     bool
	pauseEpoch uint64
	// reply is the writer's one answer, what it is beside its error
	// (nativeAnswer), buffered so the writer never waits on its caller.
	reply chan nativeAnswer
}

// DSDSink owns native data separately from the unchanged PCM sink. Its writer
// alone owns the device, including cleanup that outlives a caller's deadline.
type DSDSink struct {
	life              context.Context
	pauseEpoch        uint64
	waitIdle          bool
	releaseDeadline   time.Time
	releaseExpired    chan struct{}
	mu                sync.Mutex
	producer          sync.Mutex
	cfg               DSDConfig
	seams             nativeSeams
	devCfg            Config
	params            Params
	dev               *Device // writer-owned after open
	ring              ring
	run               run // writer-owned
	word              int
	carry             [2][4]byte
	carryBits         int
	inflight          int64
	submitted, played int64
	ledger            [nativeLedgerMax]nativeOccurrence
	ledgerN           int
	active            bool
	paused            bool
	pauseApplied      bool
	released          bool
	revoked           bool
	committed         bool
	finishing         bool
	releasing         bool
	generation        uint64
	pauseAt           time.Time
	mode              ReleaseMode
	releaseCtx        context.Context
	releaseCancel     context.CancelFunc
	waitCancel        context.CancelFunc
	activation        *nativeRequest
	err               error
	cleanupErr        error
	changed           chan struct{}
	kick              chan struct{}
	done              chan struct{}
	scratch           []byte
	dopScratch        []byte
	dopPhase          byte
}
type nativeSeams struct {
	open func(Endpoint, Config) (*Device, error)
	now  func() time.Time
	at   func(time.Time) (<-chan time.Time, func())
	// lifeEnded, nil but in a test, runs on the goroutine that revokes the
	// sink once its life, the open's context, has ended, just before the
	// revocation: a test parks it there, as a scheduler can leave it (B765).
	lifeEnded func()
	// awaiting, nil but in a test, runs on a request's caller as it begins to
	// wait for its answer (await), the request already the writer's: a test
	// holds the caller there while the writer answers it and the life ends,
	// as a scheduler can leave it (B728 B9 round 9).
	awaiting func()
	// arm, nil but in a test, names the one arm of a request's wait (await)
	// that may answer it, asked once the caller begins to select: the
	// others are left out of the select, so a test takes the arm a
	// scheduler may pick among the ready ones, and knows which it took
	// (B728 B9 round 10).
	arm func() awaitArm
	// recorded, nil but in a test, runs on a request's caller once its wait
	// on its own context's or the deadline's arm, the life ended, has waited
	// for the writer's end (endOfLife), and before it reads the writer's
	// reply and the cleanup's record: a test holds the caller there while
	// the writer's late close lands and the writer answers, as a scheduler
	// can leave it (B728 B9 round 11).
	recorded func()
}

// awaitArm is an arm of a request's wait (await), for a test that fixes
// which one answers it (nativeSeams.arm).
type awaitArm int

const (
	// armAny is the wait's select over every arm, production's.
	armAny awaitArm = iota
	// armReply is the writer's reply.
	armReply
	// armContext is the end of the request's own context.
	armContext
	// armDone is the writer's end.
	armDone
	// armDeadline is the release's shared deadline passing.
	armDeadline
)

// armOnly is a request's wait's channels with every arm but a nil, so never
// ready: the arm a test fixed (nativeSeams.arm); armAny leaves them all.
func armOnly[T any](a awaitArm, reply <-chan T, cancelled, done, expired <-chan struct{}) (<-chan T, <-chan struct{}, <-chan struct{}, <-chan struct{}) {
	if a == armAny {
		return reply, cancelled, done, expired
	}
	if a != armReply {
		reply = nil
	}
	if a != armContext {
		cancelled = nil
	}
	if a != armDone {
		done = nil
	}
	if a != armDeadline {
		expired = nil
	}
	return reply, cancelled, done, expired
}

// OpenDSDSink opens an inert native sink. Activate is mandatory. A cancelled
// opener error exposes Completion() <-chan struct{} and CompletionResult() error;
// that fence covers the worker's open, admission and cleanup. Callers must not
// open a successor or inspect final controls while that worker is outstanding.
func OpenDSDSink(ctx context.Context, c DSDConfig) (*DSDSink, error) {
	return openNativeBounded(ctx, c, nativeSeams{open: openNativeDevice, now: time.Now})
}
func (c DSDConfig) validate() (DSDConfig, error) {
	if c.Kind == format.KindDoP {
		if c.Format != format.S32LE {
			return c, errors.New("DoP requires S32_LE")
		}
	} else if (c.Kind != format.KindPCM && c.Kind != format.KindNativeDSD) || format.DSDWordBytes(c.Format) == 0 {
		return c, errors.New("native format required")
	}
	if c.BitRate != 2822400 && c.BitRate != 5644800 && c.BitRate != 11289600 {
		return c, errors.New("native logical rate unsupported")
	}
	if c.Pause != PauseKernel && c.Pause != PauseZeros {
		return c, errors.New("explicit native pause policy required")
	}
	if c.Tail < 0 || c.Tail > 100*time.Millisecond || c.Prefill < 0 || c.Prefill > MaxDSDPrefill {
		return c, errors.New("native silence policy out of range")
	}
	if c.BufferTime == 0 {
		c.BufferTime = DefaultBufferTime
	}
	if c.BufferTime < 0 || c.BufferTime > 500*time.Millisecond {
		return c, errors.New("native buffer exceeds 500 ms")
	}
	if c.RingBytes == 0 {
		c.RingBytes = 4 << 20
	}
	// Reserve space for the fixed ledger, carry and the period scratch below.
	if c.RingBytes < 8 || c.RingBytes > MaxRingBytes-128*1024 {
		return c, errors.New("native ring budget")
	}
	if c.LedgerLimit == 0 {
		c.LedgerLimit = nativeLedgerMax
	}
	if c.LedgerLimit < 1 || c.LedgerLimit > nativeLedgerMax {
		return c, errors.New("native ledger budget")
	}
	return c, nil
}
func openDSDSink(ctx context.Context, c DSDConfig, h nativeSeams) (*DSDSink, error) {
	var err error
	c, err = c.validate()
	if err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	if h.at == nil {
		h.at = func(at time.Time) (<-chan time.Time, func()) {
			timer := time.NewTimer(at.Sub(h.now()))
			return timer.C, func() { timer.Stop() }
		}
	}
	w := format.DSDWordBytes(c.Format)
	if c.Kind == format.KindDoP {
		w = 2
	}
	carrier := c.BitRate / (8 * w)
	if c.Kind == format.KindDoP {
		carrier = c.BitRate / 16
	}
	dc := Config{RateHz: carrier, Format: c.Format, BufferTime: c.BufferTime}
	d, err := h.open(c.Endpoint, dc)
	if err != nil {
		return nil, err
	}
	p := d.Params()
	if err = dopPrecision(c, p); err != nil {
		return nil, nativeCleanupError(err, d.releaseNative())
	}
	if p.BufferTime() > 500*time.Millisecond || p.BufferTime()+nativeFrameDuration(nativeFrames(p.RateHz, c.Tail), p.RateHz)+nativeCloseMargin >= nativeReleaseBudget {
		return nil, nativeCleanupError(errors.New("native readback exceeds release budget"), d.releaseNative())
	}
	if c.Pause == PauseKernel && !p.CanPause() {
		return nil, nativeCleanupError(errors.New("native kernel pause unavailable"), d.releaseNative())
	}
	if err = admit(c.Check, d, c.Endpoint, "native open"); err != nil {
		return nil, nativeCleanupError(err, d.releaseNative())
	}
	if c.RingBytes/(2*w) <= p.BufferFrames {
		return nil, nativeCleanupError(errors.New("native ring must retain hardware buffer plus lookahead"), d.releaseNative())
	}
	// DoP retains four canonical bytes per stereo frame, then expands to
	// eight hardware bytes in writer-owned scratch; budget both stores.
	scratchBytes := p.PeriodFrames * 2 * w
	packedBytes := 0
	if c.Kind == format.KindDoP {
		packedBytes = p.PeriodFrames * 8
	}
	if c.RingBytes+scratchBytes+packedBytes+nativeLedgerMax*16+8+4096 > MaxRingBytes {
		return nil, nativeCleanupError(errors.New("native aggregate memory budget"), d.releaseNative())
	}
	// A DoP run's markers start at 05: here for the first run, and again at
	// every activation that starts a run (activateNative), so a reopened run
	// never inherits the closed run's phase.
	// releaseExpired is made here, once, and closed when the release's shared
	// deadline passes (beginReleaseLocked), so a request's wait can watch it
	// from before the release begins (await, B728 B9 round 9).
	s := &DSDSink{life: ctx, cfg: c, seams: h, devCfg: dc, params: p, dev: d, word: w * 8, ring: newRing(int64(c.RingBytes/(2*w)), 2*w, int64(p.BufferFrames)), changed: make(chan struct{}), kick: make(chan struct{}, 1), done: make(chan struct{}), releaseExpired: make(chan struct{}), scratch: make([]byte, scratchBytes), dopScratch: make([]byte, packedBytes), dopPhase: 0x05}
	go s.loopNative()
	go func() {
		select {
		case <-ctx.Done():
			if h.lifeEnded != nil {
				h.lifeEnded()
			}
			s.Revoke()
		case <-s.done:
		}
	}()
	return s, nil
}

// nativeFrames rounds silence up, never shortening an explicitly chosen time.
func nativeFrames(rate int, d time.Duration) int {
	return int((int64(rate)*int64(d) + int64(time.Second) - 1) / int64(time.Second))
}
func nativeFrameDuration(n, rate int) time.Duration {
	return time.Duration((int64(n)*int64(time.Second) + int64(rate) - 1) / int64(rate))
}
func (s *DSDSink) notify() { close(s.changed); s.changed = make(chan struct{}); poke(s.kick) }
func (s *DSDSink) interrupt() {
	if s.waitCancel != nil {
		s.waitCancel()
	}
	s.notify()
}

// Activate rechecks admission before the first write, including silence. gen
// must increase for a new activation after pause/release; a revoked sink cannot
// be reactivated. A pause racing the admission check wins before the first write.
// One the sink's own fence ends answers ErrActivationFenced while the sink's
// life runs, a failure the sink recorded joined to a refusal (fenced, B728 B9
// round 8). A caller whose context has ended, or ends first, revokes the sink
// (await, abandoned); once the life has ended, every answer, at the entry and
// through the wait alike, is the verdict's (settled, verdict; rounds 9 and
// 10).
func (s *DSDSink) Activate(ctx context.Context, gen uint64) error {
	if e := ctx.Err(); e != nil {
		// The revocation ends the writer, whatever the life: the answer
		// waits for that end only once the life has ended (B728 B9 round 9),
		// the request refused at its entry (round 10).
		s.Revoke()
		if s.life.Err() == nil {
			return s.abandoned(e)
		}
		return s.settled(refusedAnswer("native activation", e))
	}
	s.mu.Lock()
	if s.err != nil || s.activation != nil || gen == 0 || gen <= s.generation {
		e := s.fenced("native activation", errors.New("native activation refused"))
		s.mu.Unlock()
		return s.settled(refusedAnswer("native activation", e))
	}
	if s.released || s.revoked || s.releasing || s.paused {
		e := s.fenced("native activation", ErrActivationFenced)
		s.mu.Unlock()
		return s.settled(refusedAnswer("native activation", e))
	}
	r := &nativeRequest{ctx: ctx, gen: gen, reply: make(chan nativeAnswer, 1)}
	s.activation = r
	s.interrupt()
	s.mu.Unlock()
	return s.await(ctx, r)
}

// await is a request's wait for its answer (Activate, Resume,
// ActivatePaused): the writer's reply, which it also sends as it ends,
// after its close; the writer's end; its caller's context's end, which
// revokes the sink; or the release's shared deadline passing, the writer
// still in its cleanup, which stays the writer's. While the sink's life runs
// the reply is the answer, a context's end its cancellation at once
// (abandoned) and the deadline the release unconfirmed (B728 B9 rounds 8 and
// 9). Once the life has ended every arm answers by one verdict, the reply
// the writer sent read whichever arm won, without waiting where the writer
// has ended or the budget passed, so no caller, on the life or off it, waits
// past that deadline, and none is answered by the arm a scheduler picked
// (verdict, round 10).
func (s *DSDSink) await(ctx context.Context, r *nativeRequest) error {
	if h := s.seams.awaiting; h != nil {
		h()
	}
	reply, cancelled, done, expired := (<-chan nativeAnswer)(r.reply), ctx.Done(), (<-chan struct{})(s.done), (<-chan struct{})(s.releaseExpired)
	if pick := s.seams.arm; pick != nil {
		reply, cancelled, done, expired = armOnly(pick(), reply, cancelled, done, expired)
	}
	select {
	case a := <-reply:
		return s.settled(a)
	case <-done:
		return s.settled(<-r.reply)
	case <-cancelled:
		s.Revoke()
		if s.life.Err() == nil {
			return s.abandoned(ctx.Err())
		}
	case <-expired:
		if s.life.Err() == nil {
			if a, ok := sent(r); ok {
				return a.err
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			return errors.Join(ErrReleaseUnconfirmed, s.err)
		}
	}
	rec := s.endOfLife()
	if h := s.seams.recorded; h != nil {
		h()
	}
	a, _ := sent(r)
	return verdict(a, rec)
}

// sent is the writer's answer to r where it has sent one, read without
// waiting; with none sent, answerNone.
func sent(r *nativeRequest) (nativeAnswer, bool) {
	select {
	case a := <-r.reply:
		return a, true
	default:
		return nativeAnswer{kind: answerNone, op: "native activation"}, false
	}
}

// errNativeActivationCancelled is the words of the writer's answer to a
// request it ended without running where the sink recorded a failure, the
// failure joined to it, never the sink's fence (B728 B9 rounds 4 and 8).
var errNativeActivationCancelled = errors.New("native activation cancelled")

// answerKind is what the answer to a request of Activate, Resume or
// ActivatePaused is, by which its verdict reads it once the sink's life has
// ended (verdict, B728 B9 round 10).
type answerKind int

const (
	// answerNone is no answer: the release's budget passed with the request
	// unanswered, the writer still in its cleanup.
	answerNone answerKind = iota
	// answerSuccess is the request done: an activation or a resume the
	// writer ran to its end, or a resume of a sink not paused.
	answerSuccess
	// answerFault is a failure of the request's own (its device's, its
	// admission's, its reopen's), which the writer records but where a
	// release it did not cause excused it, the life running.
	answerFault
	// answerHeld is a paused sink's resume, or its first activation, refused
	// while the life runs: a refusal the writer holds for the next resume and
	// records nowhere.
	answerHeld
	// answerFenced is the sink's fence (ErrActivationFenced,
	// errNativeResumeFenced), a cancellation the life or a release caused,
	// the life's typed one among them, or a refusal at a command's entry,
	// which no writer ran.
	answerFenced
	// answerNeverRan is the writer's answer to a request it ended without
	// running, sent as it ends, after its close.
	answerNeverRan
)

// nativeAnswer is the answer to a request: its error, which its caller is
// answered while the sink's life runs; what it is (kind), by which the
// verdict reads it once that life has ended; and the command that asked
// (op), for that life's typed cancellation.
type nativeAnswer struct {
	err  error
	kind answerKind
	op   string
}

// replied is the writer's answer to a request it ran, e its error, held
// where the writer holds the refusal for the next resume.
func replied(e error, held bool) nativeAnswer {
	a := nativeAnswer{err: e, kind: answerFenced, op: "native activation"}
	switch {
	case e == nil:
		a.kind = answerSuccess
	case held:
		a.kind = answerHeld
	case failureOf(e) != nil:
		a.kind = answerFault
	}
	return a
}

// refusedAnswer is a command's answer at its entry, e its refusal, or its
// caller's context already ended: no writer ran it, so the verdict reads it
// as the sink's fence.
func refusedAnswer(op string, e error) nativeAnswer {
	return nativeAnswer{err: e, kind: answerFenced, op: op}
}

// nativeRecord is what the cleanup recorded once the sink's life has ended
// (endOfLife): s.err (err), where every failure of the writer and of its
// release is joined, a failed close's with ErrReleaseUnconfirmed, final where
// the writer has ended (ended) and so far where the release's budget passed
// first; and the life's end and the endpoint, for that life's typed
// cancellation.
type nativeRecord struct {
	err      error
	ended    bool
	end      error
	endpoint Endpoint
}

// endOfLife is the cleanup's record once the sink's life has ended: the
// life's Immediate release, begun here if no one has, waited for, the
// writer's end within the release's shared budget, no lock held while it
// waits; the writer has ended where its end is there once the wait is over,
// so where its end and the deadline are both there the completed cleanup
// decides (B728 B9 rounds 9 and 10). cleanupErr holds the closes' failures
// alone, which CleanupResult reports.
func (s *DSDSink) endOfLife() nativeRecord {
	s.Revoke()
	select {
	case <-s.done:
	case <-s.releaseExpired:
	}
	ended := false
	select {
	case <-s.done:
		ended = true
	default:
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return nativeRecord{err: s.err, ended: ended, end: s.life.Err(), endpoint: s.params.Endpoint}
}

// verdict is the answer to a request of Activate, Resume or ActivatePaused
// whose sink's life has ended (B728 B9 round 10): the one rule every arm that
// answers such a request answers by, its wait's (await: the writer's reply,
// the writer's end, the request's own context's end, the release's deadline,
// the reply read whichever won) and its command's entry's (Activate's on a
// context already ended and its refusals, Resume's refusal and its answer to
// a sink not paused, ActivatePaused's refusal and the Resume it hands to:
// settled), from what the request was answered (a) and what the cleanup
// recorded (rec), in four states:
//
//   - nothing recorded, the writer ended: a success stays a success and a
//     fault of the request's own stays itself, as they were where nothing
//     failed; a refusal the writer held, the sink's fence and a request the
//     writer never ran answer the life's typed cancellation (B765), as does
//     a request no answer reached;
//   - a device's failure recorded, the release confirmed: that failure,
//     beside the request's own fault or held refusal where the record does
//     not carry it, and no ErrReleaseUnconfirmed the record does not carry;
//   - a close that failed: that failure, ErrReleaseUnconfirmed with it,
//     beside the request's own fault or held refusal;
//   - the release's budget passed, the writer still in its cleanup, which
//     stays its own, its late outcome landing in cleanupErr:
//     ErrReleaseUnconfirmed, beside the request's own fault or held refusal
//     and what is recorded by then.
//
// A failure answered carries no cancellation and not the sink's fence: those
// a record holds (a TrackChange drain the deadline cut, recorded beside
// ErrReleaseUnconfirmed; a fence its request's own context's end made the
// writer record) keep their words and lose their chain (failureOf), so its
// caller never takes a failure for a stop (context.Canceled, ReasonOf) or for
// its drop's fence; a record of nothing but those is nothing recorded.
func verdict(a nativeAnswer, rec nativeRecord) error {
	fault := failureOf(rec.err)
	var own error
	if (a.kind == answerFault || a.kind == answerHeld) && !errors.Is(rec.err, a.err) {
		own = failureOf(a.err)
	}
	switch {
	case !rec.ended && errors.Is(fault, ErrReleaseUnconfirmed):
		return errors.Join(own, fault)
	case !rec.ended:
		return errors.Join(ErrReleaseUnconfirmed, own, fault)
	case fault != nil && own != nil:
		return errors.Join(own, fault)
	case fault != nil:
		return fault
	case a.kind == answerSuccess:
		return nil
	case a.kind == answerFault:
		return a.err
	}
	return lifeCancelledAt(a.op, rec.endpoint, rec.end)
}

// failureOf is the failure e carries, in e's own words: e itself where it
// reaches no cancellation and not the sink's fence; otherwise e's words
// (%v, never %w), with what of it is neither (ErrReleaseUnconfirmed, a
// device's failure, a close's) as their chain and the rest's chain dropped;
// nil where e is nothing but those, or nil (B728 B9 round 10).
func failureOf(e error) error {
	if !stopsOrFences(e) {
		return e
	}
	parts := failures(e)
	if len(parts) == 0 {
		return nil
	}
	return &nativeFailure{words: e.Error(), parts: parts}
}

// stopsOrFences says whether e reaches a cancellation (context.Canceled, or
// an error of ReasonCancelled anywhere in its chain, where ReasonOf reads the
// first error's reason alone) or the sink's fence (ErrActivationFenced,
// errNativeResumeFenced).
func stopsOrFences(e error) bool {
	if e == nil {
		return false
	}
	return errors.Is(e, context.Canceled) || errors.Is(e, ErrActivationFenced) || errors.Is(e, errNativeResumeFenced) || cancelledIn(e)
}

// cancelledIn says whether an error of ReasonCancelled is in e's chain.
func cancelledIn(e error) bool {
	if x, ok := e.(*Error); ok && x != nil && x.Reason == ReasonCancelled {
		return true
	}
	switch u := e.(type) {
	case interface{ Unwrap() error }:
		if w := u.Unwrap(); w != nil {
			return cancelledIn(w)
		}
	case interface{ Unwrap() []error }:
		for _, w := range u.Unwrap() {
			if w != nil && cancelledIn(w) {
				return true
			}
		}
	}
	return false
}

// failures is what of e is no cancellation and not the sink's fence: e where
// it reaches neither; otherwise what of each error it wraps is neither; and
// nothing of a cancellation or the fence itself, an error of ReasonCancelled
// with the context's error it wraps (its chain goes whole).
func failures(e error) []error {
	if e == nil {
		return nil
	}
	if !stopsOrFences(e) {
		return []error{e}
	}
	if x, ok := e.(*Error); ok && x != nil && x.Reason == ReasonCancelled {
		return nil
	}
	switch u := e.(type) {
	case interface{ Unwrap() []error }:
		var parts []error
		for _, w := range u.Unwrap() {
			parts = append(parts, failures(w)...)
		}
		return parts
	case interface{ Unwrap() error }:
		return failures(u.Unwrap())
	}
	return nil
}

// nativeFailure is a failure failureOf re-expressed: the words of what the
// sink recorded or answered, and as its chain the failures among what that
// wrapped, so errors.Is finds each of them and none of what it dropped.
type nativeFailure struct {
	words string
	parts []error
}

func (f *nativeFailure) Error() string   { return f.words }
func (f *nativeFailure) Unwrap() []error { return f.parts }

// settled is the answer to a request of Activate, Resume or ActivatePaused
// answered a, by the writer or at its command's entry: a's error while the
// sink's life runs; once the life has ended, the verdict's, the cleanup's
// record awaited (endOfLife; B728 B9 rounds 9 and 10). Pause's and WriteDSD's
// own waits are not settled: they answer their caller's context, and their
// refusals what is recorded as they answer.
func (s *DSDSink) settled(a nativeAnswer) error {
	if s.life.Err() == nil {
		return a.err
	}
	return verdict(a, s.endOfLife())
}

// abandoned is the answer, while the sink's life runs, to a request whose
// caller's context ended (cause), at Activate's entry or before the writer
// answered it, the sink revoked: that cause, with the failure the sink
// recorded joined, at once (B728 B9 rounds 8 and 9): its revocation ends the
// writer, which it does not wait for. Once the life has ended the verdict
// answers it (await, Activate; round 10).
func (s *DSDSink) abandoned(cause error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return errors.Join(cause, s.err)
	}
	return cause
}

// Revoke permanently cancels activation and begins Immediate cleanup.
func (s *DSDSink) Revoke() { s.beginRelease(Immediate) }

// lifeCancelled is the answer of a command the sink's ended life (end, its
// error) fenced or cancelled: that life's cancellation, typed as the
// package's cancellations are, so a caller the life stopped takes it for the
// stop it is, whatever refusal of its own the command met (B765 round 3).
// With s.mu held.
func (s *DSDSink) lifeCancelled(op string, end error) error {
	return lifeCancelledAt(op, s.params.Endpoint, end)
}

// lifeCancelledAt is lifeCancelled's error for the endpoint ep.
func lifeCancelledAt(op string, ep Endpoint, end error) error {
	return &Error{Reason: ReasonCancelled, Op: op, Endpoint: ep, Detail: "the sink's life ended", Err: end}
}

// fenced is a command's refusal, refusal, by the one rule every command's
// refusal and the writer's loop-end reply answer by (B728 B9 round 8): a
// failure the sink recorded (s.err: its device's, or its release's,
// ErrReleaseUnconfirmed) is joined to the refusal whether or not the sink's
// life has ended, so a caller the life stopped still sees it; with none, the
// refusal itself while the life runs, and the life's cancellation once that
// has ended, which releases the sink whatever the command met first (B765).
// With s.mu held.
func (s *DSDSink) fenced(op string, refusal error) error {
	if s.err != nil {
		return errors.Join(refusal, s.err)
	}
	if end := s.life.Err(); end != nil {
		return s.lifeCancelled(op, end)
	}
	return refusal
}

// WriteDSD returns accepted logical bits PER CHANNEL, including bits in the
// partial-word carry. validBits is a count starting at offset in each equally
// sized plane. On partial acceptance/error, retry with offset+n, validBits-n;
// never resend the accepted prefix. Accepted input is copied before return.
// Zero bits completes an empty occurrence without allocating a ledger entry;
// Finish, not a zero-bit Write, declares final EOF.
func (s *DSDSink) WriteDSD(ctx context.Context, id uint64, left, right []byte, offset, validBits int) (int, error) {
	if len(left) != len(right) || offset < 0 || validBits < 0 || offset/8 > len(left) || int64(validBits) > int64(len(left))*8-int64(offset) {
		return 0, errors.New("invalid native bit counts")
	}
	s.producer.Lock()
	defer s.producer.Unlock()
	accepted := 0
	for accepted < validBits {
		if err := ctx.Err(); err != nil {
			return accepted, err
		}
		s.mu.Lock()
		if s.releasing || s.revoked || s.finishing || s.err != nil {
			e := s.fenced("native write", errors.New("native admission closed"))
			s.mu.Unlock()
			return accepted, e
		}
		if s.ledgerN == 0 || s.ledger[s.ledgerN-1].id != id {
			if s.ledgerN == s.cfg.LedgerLimit {
				s.mu.Unlock()
				return accepted, errors.New("native occurrence budget exceeded")
			}
			s.ledger[s.ledgerN] = nativeOccurrence{id: id, end: s.submitted}
			s.ledgerN++
		}
		if s.ring.free() == 0 {
			ch := s.changed
			s.mu.Unlock()
			select {
			case <-ctx.Done():
				return accepted, ctx.Err()
			case <-ch:
			}
			continue
		}
		n := min(validBits-accepted, s.word-s.carryBits)
		for ch, src := range [2][]byte{left, right} {
			if (offset+accepted)%8 == 0 && s.carryBits%8 == 0 && n%8 == 0 {
				copy(s.carry[ch][s.carryBits/8:], src[(offset+accepted)/8:(offset+accepted+n)/8])
				continue
			}
			for i := range n {
				at := offset + accepted + i
				dst := s.carryBits + i
				bit := (src[at/8] >> uint(7-at%8)) & 1
				s.carry[ch][dst/8] = (s.carry[ch][dst/8] &^ (1 << uint(7-dst%8))) | (bit << uint(7-dst%8))
			}
		}
		accepted += n
		s.submitted += int64(n)
		s.carryBits += n
		s.ledger[s.ledgerN-1].end = s.submitted
		if s.carryBits == s.word {
			s.flushCarry()
		}
		if s.waitIdle && s.waitCancel != nil {
			s.waitCancel()
		}
		s.notify()
		s.mu.Unlock()
	}
	return accepted, nil
}
func (s *DSDSink) flushCarry() {
	var b [8]byte
	w := s.word / 8
	if s.cfg.Kind == format.KindDoP {
		copy(b[:w], s.carry[0][:w])
		copy(b[w:2*w], s.carry[1][:w])
	} else {
		_, _ = format.PackDSD(b[:], s.carry[0][:w], s.carry[1][:w], s.cfg.Format)
	}
	s.ring.put(s.ring.sub, b[:2*w])
	s.ring.sub++
	s.carry = [2][4]byte{}
	s.carryBits = 0
}

// Finish ends admission, pads final carry with phased 0x69 and drains naturally.
func (s *DSDSink) Finish(ctx context.Context) error {
	if e := ctx.Err(); e != nil {
		s.cancelFinish()
		return errors.Join(ErrReleaseUnconfirmed, e)
	}
	s.producer.Lock()
	s.mu.Lock()
	if s.releasing {
		s.mu.Unlock()
		s.producer.Unlock()
		return s.waitRelease(ctx)
	}
	if s.carryBits > 0 {
		for i := s.carryBits; i < s.word; i++ {
			bit := (byte(0x69) >> uint(7-i%8)) & 1
			for ch := range 2 {
				s.carry[ch][i/8] |= bit << uint(7-i%8)
			}
		}
		s.flushCarry()
	}
	s.finishing = true
	s.interrupt()
	s.mu.Unlock()
	s.producer.Unlock()
	for {
		s.mu.Lock()
		releasing, ch := s.releasing, s.changed
		s.mu.Unlock()
		if releasing {
			return s.waitRelease(ctx)
		}
		select {
		case <-s.done:
			return s.result()
		case <-ctx.Done():
			s.cancelFinish()
			return errors.Join(ErrReleaseUnconfirmed, ctx.Err())
		case <-ch:
		}
	}
}

// cancelFinish establishes an Immediate release only if no owner has selected
// a cutoff. Cancelling the producer cannot escalate a chosen TrackChange.
func (s *DSDSink) cancelFinish() {
	s.mu.Lock()
	if !s.releasing {
		s.beginReleaseLocked(Immediate)
	}
	s.mu.Unlock()
}

// BeginRelease establishes the cutoff synchronously, without waiting for cleanup.
// Callers can cancel producers afterwards and still process Stop during the tail.
func (s *DSDSink) BeginRelease(m ReleaseMode) {
	if m != Immediate && m != TrackChange {
		return
	}
	s.beginRelease(m)
}
func (s *DSDSink) beginRelease(m ReleaseMode) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.beginReleaseLocked(m)
}
func (s *DSDSink) beginReleaseLocked(m ReleaseMode) {
	interrupt := !s.releasing || (m == Immediate && s.mode != Immediate)
	if !s.releasing {
		s.releasing = true
		s.mode = m
		s.releaseDeadline = s.seams.now().Add(nativeReleaseBudget)
		s.releaseCtx, s.releaseCancel = context.WithCancel(context.Background())
		ticks, stop := s.seams.at(s.releaseDeadline)
		go func() {
			defer stop()
			select {
			case <-ticks:
				s.releaseCancel()
				close(s.releaseExpired)
			case <-s.done:
			}
		}()
	} else if m == Immediate {
		s.mode = Immediate
	}
	if m == Immediate {
		s.revoked = true
		if s.releaseCancel != nil {
			s.releaseCancel()
		}
	}
	if interrupt {
		s.interrupt()
	}
}

// Release returns within the shared two-second release budget. A timeout never
// transfers device ownership: the writer keeps cleaning up; Done is its fence.
// An Immediate call can escalate a TrackChange even while an ioctl is parked.
func (s *DSDSink) Release(ctx context.Context, m ReleaseMode) error {
	if m != Immediate && m != TrackChange {
		return errors.New("invalid native release mode")
	}
	s.beginRelease(m)
	return s.waitRelease(ctx)
}
func (s *DSDSink) waitRelease(ctx context.Context) error {
	select {
	case <-s.done:
		return s.result()
	default:
	}
	s.mu.Lock()
	expired := s.releaseExpired
	s.mu.Unlock()
	select {
	case <-s.done:
		return s.result()
	case <-ctx.Done():
		return errors.Join(ErrReleaseUnconfirmed, ctx.Err())
	case <-expired:
		select {
		case <-s.done:
			return s.result()
		default:
		}
		return ErrReleaseUnconfirmed
	}
}
func (s *DSDSink) result() error { s.mu.Lock(); defer s.mu.Unlock(); return s.err }

// Pause freezes content progress; after thirty seconds hardware is released.
func (s *DSDSink) Pause(ctx context.Context) error {
	s.mu.Lock()
	if s.releasing {
		e := s.fenced("native pause", errors.New("native releasing"))
		s.mu.Unlock()
		return e
	}
	if !s.paused {
		s.pauseAt = s.seams.now()
	}
	s.pauseEpoch++
	s.paused = true
	s.interrupt()
	for s.active && !s.pauseApplied {
		ch := s.changed
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.done:
			return s.result()
		case <-ch:
		}
		s.mu.Lock()
	}
	s.mu.Unlock()
	return nil
}

// Resume revalidates a released endpoint before resuming retained content.
// Once the sink's life has ended its answers are the verdict's, a sink not
// paused answered as a success (settled, verdict; B728 B9 round 10).
func (s *DSDSink) Resume(ctx context.Context) error {
	s.mu.Lock()
	if s.generation == 0 || s.releasing || s.revoked || s.err != nil || s.activation != nil {
		e := s.fenced("native resume", errors.New("native resume fenced"))
		s.mu.Unlock()
		return s.settled(refusedAnswer("native resume", e))
	}
	if !s.paused {
		s.mu.Unlock()
		return s.settled(nativeAnswer{kind: answerSuccess, op: "native resume"})
	}
	r := &nativeRequest{ctx: ctx, gen: s.generation + 1, resume: true, pauseEpoch: s.pauseEpoch, reply: make(chan nativeAnswer, 1)}
	s.activation = r
	s.interrupt()
	s.mu.Unlock()
	return s.await(ctx, r)
}

// Committed is true once any hardware frame, even silence, was accepted.
func (s *DSDSink) Committed() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.committed }

// Position returns measured content accounting.
func (s *DSDSink) Position() DSDPosition {
	s.mu.Lock()
	defer s.mu.Unlock()
	return DSDPosition{Submitted: s.submitted, Written: min(s.ring.wr*int64(s.word), s.submitted), Played: s.played, Paused: s.paused, Released: s.released}
}

// Params returns the exact committed transport geometry.
func (s *DSDSink) Params() Params { s.mu.Lock(); defer s.mu.Unlock(); return s.params }

// Done closes only after actual cleanup, including a parked close, completes.
func (s *DSDSink) Done() <-chan struct{} { return s.done }

// Transport identifies the native source and exact opened carrier separately.
func (s *DSDSink) Transport() Transport {
	p := s.Params()
	kind := format.KindNativeDSD
	if s.cfg.Kind == format.KindDoP {
		kind = format.KindDoP
	}
	return Transport{Kind: kind, RateHz: p.RateHz, Channels: 2, Format: s.cfg.Format, ValidBits: 1, LogicalRate: s.cfg.BitRate, PackingVersion: 1}
}

// Changed closes on the next measured position or lifecycle change.
func (s *DSDSink) Changed() <-chan struct{} { s.mu.Lock(); defer s.mu.Unlock(); return s.changed }

// Result reports playback and cleanup errors after Done.
func (s *DSDSink) Result() error { return s.result() }

// Cut removes only wholly software-owned content, never a committed mixed word.
func (s *DSDSink) Cut(at int64) (bool, error) {
	s.producer.Lock()
	defer s.producer.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if at < 0 || at > s.submitted || s.releasing {
		return false, errors.New("invalid native cut")
	}
	if at < max(s.ring.wr, s.inflight)*int64(s.word) {
		return false, nil
	}
	frame := at / int64(s.word)
	bits := int(at % int64(s.word))
	if bits > 0 && frame < s.ring.sub {
		b := s.ring.span(frame, 1)
		w := s.word / 8
		for ch := range 2 {
			for i := range w {
				j := i
				if s.cfg.Kind != format.KindDoP && s.cfg.Format != format.DSDU32BE {
					j = w - 1 - i
				}
				s.carry[ch][i] = b[ch*w+j]
			}
		}
	}
	for ch := range 2 {
		for i := bits; i < s.word; i++ {
			s.carry[ch][i/8] &^= 1 << uint(7-i%8)
		}
	}
	s.carryBits = bits
	s.ring.sub = frame
	s.submitted = at
	n := 0
	for n < s.ledgerN && s.ledger[n].end <= at {
		n++
	}
	s.ledgerN = n
	s.notify()
	return true, nil
}

// ActivatePaused grants initial activation to a sink installed while paused.
// Resume alone deliberately cannot activate an inert sink. Once the sink's
// life has ended its answers are the verdict's, the Resume it hands an
// activated sink to answering by it too (settled, verdict; B728 B9 round
// 10).
func (s *DSDSink) ActivatePaused(ctx context.Context) error {
	s.mu.Lock()
	if s.generation != 0 {
		s.mu.Unlock()
		return s.Resume(ctx)
	}
	if !s.paused || s.releasing || s.revoked || s.err != nil || s.activation != nil {
		e := s.fenced("native activation", errors.New("native initial paused activation fenced"))
		s.mu.Unlock()
		return s.settled(refusedAnswer("native activation", e))
	}
	r := &nativeRequest{ctx: ctx, gen: 1, resume: true, pauseEpoch: s.pauseEpoch, reply: make(chan nativeAnswer, 1)}
	s.activation = r
	s.interrupt()
	s.mu.Unlock()
	return s.await(ctx, r)
}

// CleanupResult reports only failures to relinquish the device. Done must close
// before a nil result establishes release; playback faults remain in Result.
func (s *DSDSink) CleanupResult() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cleanupErr
}

// dopPrecision is shared by initial admission and every reopened hardware run.
func dopPrecision(c DSDConfig, p Params) error {
	if c.Kind == format.KindDoP && !dopBits(p.Msbits) {
		return errors.New("DoP requires 24..32 significant bits")
	}
	return nil
}
