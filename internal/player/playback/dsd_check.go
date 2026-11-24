package playback

import (
	"context"
	"crypto/hmac"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdchime"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
	"github.com/acoseac/1-bit-bridge/internal/player/policy"
)

// The player's DSD check (B728 B12 PR 3; the design's sections 6.2 to 6.4):
// the chime (internal/player/dsdchime) through the bound DAC at one tuple the
// effective set plays now, or the native candidate its native_check_required
// refusal carries (round 3), by that tuple's plan, while the engine's check hold
// (engine.EnterCheck) keeps the engine closed: the device slot's fourth holder,
// beside a session, the silent probe and the console's tone. Every end is a
// cause on its context, the first standing, which revokes its sink; the slot
// is given back only once its cleanup is confirmed. Each transition is
// published beside the set as it is, under b.mu, never behind the refreshes'
// lock (publishCheckLocked: B12 PR 4, which adds its routes and the
// listener's answer, AnswerCheck). Lock order: dsdMu, then b.mu, as a
// refresh's reconciliation takes them; gmu alone; and no step of the check's
// waits for dsdMu past its own context (refreshDSDWithin).

// The check's bounds: preparing (the chime's build) and playing (the locate,
// the open, the chime and the drain), 12 s in all, and StopCheck's wait.
const (
	CheckPrepareBound = 2 * time.Second
	CheckPlayBound    = 10 * time.Second
	CheckStopWait     = 2 * time.Second
)

// CheckPhase is where a check is: preparing or playing while it runs,
// stopping once it ended and its cleanup has not answered, then complete,
// stopped or failed; answered, a complete check a listener's answer was
// written for (AnswerCheck).
type CheckPhase string

const (
	CheckPreparing CheckPhase = "preparing"
	CheckPlaying   CheckPhase = "playing"
	CheckComplete  CheckPhase = "complete"
	CheckAnswered  CheckPhase = "answered"
	CheckStopping  CheckPhase = "stopping"
	CheckStopped   CheckPhase = "stopped"
	CheckFailed    CheckPhase = "failed"
)

// CheckPhases is every CheckPhase, in PROTOCOL.md's order.
func CheckPhases() []CheckPhase {
	return []CheckPhase{CheckPreparing, CheckPlaying, CheckComplete, CheckAnswered, CheckStopping, CheckStopped, CheckFailed}
}

// CheckReason is why a check stopped (stop, deleted, serve_stopping) or failed
// (the rest, release_unconfirmed a cleanup that did not answer cleanly).
type CheckReason string

const (
	CheckStop               CheckReason = "stop"
	CheckDeleted            CheckReason = "deleted"
	CheckServeStopping      CheckReason = "serve_stopping"
	CheckTimeout            CheckReason = "timeout"
	CheckPolicyChanged      CheckReason = "policy_changed"
	CheckOutputGone         CheckReason = "output_gone"
	CheckOutputUnavailable  CheckReason = "output_unavailable"
	CheckOutputChanged      CheckReason = "output_changed"
	CheckDeviceError        CheckReason = "device_error"
	CheckDeviceBusy         CheckReason = "device_busy"
	CheckWrongDevice        CheckReason = "wrong_device"
	CheckReleaseUnconfirmed CheckReason = "release_unconfirmed"
	CheckBackendFailed      CheckReason = "backend_failed"
)

// CheckReasons is every CheckReason, stopped's then failed's.
func CheckReasons() []CheckReason {
	return []CheckReason{CheckStop, CheckDeleted, CheckServeStopping, CheckTimeout, CheckPolicyChanged, CheckOutputGone,
		CheckOutputUnavailable, CheckOutputChanged, CheckDeviceError, CheckDeviceBusy, CheckWrongDevice, CheckReleaseUnconfirmed, CheckBackendFailed}
}

// The words a check is refused with beside the set's own for the rate, the
// supervisor's gate's (ToneOutput), dsd_not_in_this_release, serve_stopping,
// device_busy and release_unconfirmed: dac_unavailable for a reading that
// names no DAC or did not read.
const (
	CheckBadRequest     = "bad_request"
	CheckDACUnavailable = "dac_unavailable"
	CheckDACChanged     = "dac_changed"
	CheckConverted      = "converted"
	CheckOtherTransport = "other_transport"
)

// CheckAnswerable is how long after its completion a check is answered.
const CheckAnswerable = 10 * time.Minute

// CheckRequest is a check asked: the DAC's key and token as GET /v1/outputs
// published them, native or dop at DSD64, DSD128 or DSD256, and the engine's
// check hold's stop signal (engine.Hold.Stop).
type CheckRequest struct {
	DACKey    string
	DACToken  DACToken
	Transport string
	BitRate   int
	Stop      <-chan struct{}
}

// CheckRefused is a check refused, by its word, nothing opened, with the pair
// published as its refusal was decided, after the fresh reading (Epoch: none
// for a request that is none, a backend with no DSD policy, serve's stop).
type CheckRefused struct {
	Word   string
	Policy PlayerPolicy
	Epoch  bool
}

func (e *CheckRefused) Error() string { return "player backend: the DSD check was refused: " + e.Word }

// CheckStatus is a check, one boot's: its opaque ID, its tuple, its phase and
// reason, the token and the identity of the set it was admitted under, and the
// identity its sink opened, nil before it opened one.
type CheckStatus struct {
	ID        string
	Transport string
	BitRate   int
	Phase     CheckPhase
	Reason    CheckReason
	Token     DACToken
	Identity  policy.Identity
	Opened    *policy.Identity
}

// checkHold is a check given the slot: what it was admitted with, the
// candidate among it (byCandidate: its plan the native_check_required
// refusal's candidate, which judges it, never the set's plan at the rate);
// its context, cancelled with its cause by whatever ends it; done, closed
// once it gave the slot back, its cleanup confirmed or not (confirmed, set
// before done closes); and, under b.mu, the set it plays under, its sink or
// its open's error, the identity its sink opened, its phase and its reason,
// and when it completed.
type checkHold struct {
	id, transport string
	bitRate       int
	token         DACToken
	identity      policy.Identity
	plan          policy.Plan
	byCandidate   bool
	ctx           context.Context
	cancel        context.CancelCauseFunc
	done          chan struct{}
	confirmed     bool

	admitted  *dsdSet
	sink      *alsapcm.DSDSink
	openErr   error
	opened    *policy.Identity
	phase     CheckPhase
	reason    CheckReason
	completed time.Time
}

// CheckHandle is a check Check admitted: its record and the pair published
// with it preparing, as the step that admitted it made them, and its own end
// (Done, Cleanup), never another check's.
type CheckHandle struct {
	Status CheckStatus
	Policy PlayerPolicy
	c      *checkHold
}

// CheckCleanup is how a check's cleanup ended: none for no check, pending
// while it has not answered, then confirmed, or failed (the device fenced
// until serve restarts).
type CheckCleanup string

const (
	CheckCleanupNone      CheckCleanup = "none"
	CheckCleanupPending   CheckCleanup = "pending"
	CheckCleanupConfirmed CheckCleanup = "confirmed"
	CheckCleanupFailed    CheckCleanup = "failed"
)

// Done closes once the check has given the slot back or fenced the device.
func (h *CheckHandle) Done() <-chan struct{} { return h.c.done }

// Cleanup is how the check's cleanup has ended so far.
func (h *CheckHandle) Cleanup() CheckCleanup {
	select {
	case <-h.c.done:
	default:
		return CheckCleanupPending
	}
	if h.c.confirmed {
		return CheckCleanupConfirmed
	}
	return CheckCleanupFailed
}

// checkCause is a check's end, its context's cause.
type checkCause struct {
	phase  CheckPhase
	reason CheckReason
}

func (c *checkCause) Error() string {
	return "player backend: the DSD check ended " + string(c.phase) + " " + string(c.reason)
}

// end ends c unless it has ended: the first cause stands.
func (c *checkHold) end(p CheckPhase, r CheckReason) { c.cancel(&checkCause{phase: p, reason: r}) }

// endCheckLocked ends c and publishes it stopping in the same step. b.mu is
// held.
func (b *Backend) endCheckLocked(c *checkHold, p CheckPhase, r CheckReason) {
	c.end(p, r)
	b.publishCheckLocked()
}

// Check starts the check of req's tuple and answers its handle once it holds
// the device slot, its run going on, on a goroutine the backend counts from
// before the slot (Wait). Refused, in order: a request that is none, a backend
// with no DSD policy, CloseChecks having run, then as reserveCheck says. ctx
// bounds its waits, and ends them with its error. The record is the one its
// admission built, and the count is given back on every way out before the
// hand-over to its run, a panic's included. Whatever ends the check, its end
// is published once its context has ended, on a goroutine of the backend's.
func (b *Backend) Check(ctx context.Context, req CheckRequest) (*CheckHandle, error) {
	word := ""
	switch {
	case req.Transport != dsdstate.TransportNative && req.Transport != dsdstate.TransportDoP, !nativeLogicalRate(req.BitRate), !req.DACToken.Valid(), req.Stop == nil:
		word = CheckBadRequest
	case b.cfg.DSD == nil:
		word = string(ReasonDSD)
	case !b.checkEnter():
		word = string(CheckServeStopping)
	}
	if word != "" {
		return nil, &CheckRefused{Word: word}
	}
	handed := false
	defer func() {
		if !handed {
			b.leave()
		}
	}()
	h, err := b.reserveCheck(ctx, req)
	if h == nil {
		return nil, err
	}
	c := h.c
	b.spawn(func() {
		select {
		case <-req.Stop:
			c.end(CheckStopped, CheckStop)
		case <-c.ctx.Done():
		}
		b.mu.Lock()
		b.publishCheckLocked()
		b.mu.Unlock()
	})
	handed = true
	go b.counted(false, func() { b.runCheck(c) })
	return h, nil
}

// checkEnter counts a check among the backend's goroutines, under gmu alone,
// unless CloseChecks has run.
func (b *Backend) checkEnter() bool {
	b.gmu.Lock()
	defer b.gmu.Unlock()
	if !b.cclosed {
		b.startLocked(false)
	}
	return !b.cclosed
}

// reserveCheck reads the policy afresh within ctx (dac_unavailable where the
// reading failed), then takes the device slot for req as a Start's hold takes
// it, in one step under b.mu, in the route's order: refused once CloseChecks
// has run; then as the pair published now admits req (checkAdmits: the DAC,
// its key and token, the tuple); then while the supervisor's gate is shut
// (its word), a release was never confirmed, or a choice's lease, a session's
// record or hold, or another check stands (device_busy); the probe or the
// tone (ended preempted) waited for, the check among the waiters meanwhile,
// device_busy past its bound, each step asked again once it has; then
// admitted (admitCheckLocked), its record built and published in that step.
// The take moves starts, which voids an ending probe's answers as a Start's
// take does (takeProbe). A request that has ended is refused with its error
// first, before the slot is looked at, so no tone is preempted for it, and
// where its wait ends at its bound. A refusal carries the pair it was decided
// on.
func (b *Backend) reserveCheck(ctx context.Context, req CheckRequest) (*CheckHandle, error) {
	if _, err := b.refreshDSDWithin(ctx, "a DSD check's admission"); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &CheckRefused{Word: CheckDACUnavailable, Policy: b.Policy(), Epoch: true}
	}
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
		if err := ctx.Err(); err != nil {
			b.mu.Unlock()
			return nil, err
		}
		var held <-chan struct{}
		var bound time.Duration
		pub := b.dsdPub.Load()
		p, byCandidate, word := checkAdmits(pub, req)
		switch {
		case b.checksClosed:
			word = string(CheckServeStopping)
		case word != "":
		case !b.toneOpen:
			word = string(b.toneShut)
		case b.terminalUnconfirmed:
			word = string(CheckReleaseUnconfirmed)
		case b.lease != nil || len(b.sessions) > 0 || b.holder != nil || b.checking != nil:
			word = string(CheckDeviceBusy)
		default:
			if held, bound, _ = b.slotHeldLocked(); held == nil {
				if waiting {
					b.waiters--
					waiting = false
				}
				h := b.admitCheckLocked(req, pub, p, byCandidate)
				b.mu.Unlock()
				return h, nil
			}
		}
		if word != "" {
			r := &CheckRefused{Word: word, Policy: b.policyOf(pub), Epoch: word != string(CheckServeStopping)}
			b.mu.Unlock()
			return nil, r
		}
		if !waiting {
			b.waiters++
			waiting = true
		}
		b.mu.Unlock()
		if expired == nil {
			ch := make(chan struct{})
			stop = b.cfg.AfterFunc(bound, func() { close(ch) })
			expired = ch
		}
		select {
		case <-held:
		case <-expired:
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return nil, &CheckRefused{Word: string(CheckDeviceBusy), Policy: b.Policy(), Epoch: true}
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// checkAdmits is the check's admission by the pair pub, its decision alone,
// with no I/O: the plan req plays and whether the candidate gave it, or the
// word it is refused with: dac_unavailable while pub names no DAC; dac_changed
// for another key or token; then the set's plan at the rate, or, for a request
// of its transport, the candidate a native_check_required refusal carries
// there (policy.Plan.Candidate, B728 B12 PR 3 round 3), never one merely
// offered (the rate's own refusal word, converted, other_transport).
func checkAdmits(pub *dsdPublished, req CheckRequest) (p policy.Plan, byCandidate bool, word string) {
	switch d := pub.view.DAC; {
	case d == nil:
		return p, false, CheckDACUnavailable
	case d.Key != req.DACKey || !hmac.Equal([]byte(d.Token), []byte(req.DACToken)):
		return p, false, CheckDACChanged
	}
	p = pub.set.set.At(req.BitRate).Plan
	if c := p.Candidate; c != nil && tupleTransport(*c) == req.Transport {
		p, byCandidate = *c, true
	}
	switch {
	case p.Outcome == policy.OutcomeConverted:
		return p, false, CheckConverted
	case p.Outcome != policy.OutcomeNative && p.Outcome != policy.OutcomeDoP:
		return p, false, string(p.Reason)
	case tupleTransport(p) != req.Transport:
		return p, false, CheckOtherTransport
	}
	return p, byCandidate, ""
}

// CheckAdmits is the check's admission of req by the pair published now, the
// backend's own (checkAdmits), with no I/O: the word it is refused with, ""
// where it admits, and that pair. The route's precheck (B728 B12 PR 4).
func (b *Backend) CheckAdmits(req CheckRequest) (string, PlayerPolicy) {
	if b.cfg.DSD == nil {
		return string(ReasonDSD), b.Policy()
	}
	pub := b.dsdPub.Load()
	_, _, word := checkAdmits(pub, req)
	return word, b.policyOf(pub)
}

// admitCheckLocked gives req the slot with plan p, the candidate's where
// byCandidate, admitted by pub (checkAdmits), and publishes the check
// preparing in that step: its handle. b.mu is held.
func (b *Backend) admitCheckLocked(req CheckRequest, pub *dsdPublished, p policy.Plan, byCandidate bool) *CheckHandle {
	ctx, cancel := context.WithCancelCause(context.Background())
	c := &checkHold{id: fmt.Sprintf("%x", randomBoot()), transport: req.Transport, bitRate: req.BitRate, token: req.DACToken, identity: pub.set.set.Identity(),
		plan: p, byCandidate: byCandidate, ctx: ctx, cancel: cancel, done: make(chan struct{}), admitted: pub.set, phase: CheckPreparing}
	b.checking, b.checkRec = c, c
	b.starts.Add(1)
	b.publishCheckLocked()
	return &CheckHandle{Status: b.checkStatusLocked(c), Policy: b.policyOf(b.dsdPub.Load()), c: c}
}

// runCheck is a check's run, on the goroutine its count stands for, and, on
// every way out, its end published stopping, then its cleanup (cleanCheck,
// with its own recover) and the slot given back (endCheck).
func (b *Backend) runCheck(c *checkHold) {
	defer func() {
		b.mu.Lock()
		b.publishCheckLocked()
		b.mu.Unlock()
		ok, err := b.cleanCheck(c)
		b.endCheck(c, ok, err)
	}()
	defer func() {
		if r := recover(); r != nil {
			b.logPanic("the DSD check", r)
			c.end(CheckFailed, CheckBackendFailed)
		}
	}()
	b.playCheck(c)
}

// playCheck is the run: the chime built (preparing); then the policy read
// afresh and the check judged, the DoP gain among it; the bound unit located;
// the sink opened by the plan and its node checked (checkNode), its
// driver_accepted recorded, its monitor started; its activation judged as a
// session's; the chime written after the plan's prefill, and drained after its
// tail (playing). Each way it ends sets the check's cause; a call the check's
// context ended answers that end, the cause standing already.
func (b *Backend) playCheck(c *checkHold) {
	stop := b.cfg.AfterFunc(CheckPrepareBound, func() { c.end(CheckFailed, CheckTimeout) })
	b.checkPreparing(c.ctx)
	chime, err := dsdchime.Get(c.ctx, c.bitRate)
	stop()
	b.mu.Lock()
	playing := err == nil && c.ctx.Err() == nil
	if playing {
		c.phase = CheckPlaying
		b.publishCheckLocked()
	}
	b.mu.Unlock()
	if !playing {
		c.end(CheckFailed, CheckBackendFailed)
		return
	}
	defer b.cfg.AfterFunc(CheckPlayBound, func() { c.end(CheckFailed, CheckTimeout) })()
	if !b.judgeCheckFresh(c, "a DSD check's open") {
		return
	}
	u, err := b.cfg.Locator.Locate(c.ctx)
	if err != nil {
		c.end(CheckFailed, CheckOutputGone)
		return
	}
	plan := enginePlan(c.plan)
	pause := alsapcm.PauseKernel
	if plan.Pause == "silence" {
		pause = alsapcm.PauseZeros
	}
	sink, err := b.cfg.OpenNative(c.ctx, alsapcm.DSDConfig{Kind: nativeTransport(plan).Kind, Endpoint: u.Endpoint, Format: format.ALSAFormat(plan.Tuple.Format),
		BitRate: c.bitRate, Pause: pause, Tail: plan.Tail, Prefill: plan.Prefill, Check: func(n alsapcm.NodeInfo) error { return b.checkNode(c, u, n, &plan) }, Log: b.log})
	b.mu.Lock()
	c.sink, c.openErr = sink, err
	admitted := c.admitted
	b.mu.Unlock()
	if err != nil {
		c.end(CheckFailed, checkFault(err))
		return
	}
	b.noteDriverAccepted(admitted, c.plan)
	b.monitorCheck(c, sink)
	if !b.judgeCheckFresh(c, "a DSD check's activation") {
		return
	}
	if err := sink.Activate(c.ctx, 1); err != nil {
		c.end(CheckFailed, checkFault(err))
		return
	}
	planes := [][]byte{make([]byte, chunkFrames), make([]byte, chunkFrames)}
	for off := int64(0); ; {
		n, eof := chime.ReadAt(planes, off)
		if _, err := sink.WriteDSD(c.ctx, 1, planes[0][:n], planes[1][:n], 0, 8*n); err != nil {
			c.end(CheckFailed, checkFault(err))
			return
		}
		if off += int64(n); eof != nil {
			break
		}
	}
	if err := sink.Finish(c.ctx); err != nil {
		c.end(CheckFailed, checkFault(err))
		return
	}
	c.end(CheckComplete, "")
}

// checkNode is the check after open of the check's node, at its open and its
// activation: none once the check has ended; the unit's own (u.Admit,
// wrong_device); then checkDSDNode's (nodeIdentity) against the set the check
// plays under, DoP's gain read afresh (policy_changed), and the identity the
// check was admitted under (wrong_device). It records the identity opened.
func (b *Backend) checkNode(c *checkHold, u Unit, n alsapcm.NodeInfo, plan *engine.NativePlan) error {
	if err := c.ctx.Err(); err != nil {
		return err
	}
	if u.Admit != nil {
		if err := u.Admit(n); err != nil {
			c.end(CheckFailed, CheckWrongDevice)
			return err
		}
	}
	b.mu.Lock()
	admitted := c.admitted
	b.mu.Unlock()
	id, gain, err := nodeIdentity(b.cfg.DSD, n, admitted, plan)
	if err == nil && !id.Same(c.identity) {
		err = errors.New("player backend: the node opened is not the DAC the DSD check was admitted for")
	}
	if err != nil {
		why := CheckWrongDevice
		if gain {
			why = CheckPolicyChanged
		}
		c.end(CheckFailed, why)
		return err
	}
	b.mu.Lock()
	c.opened = &id
	b.mu.Unlock()
	return nil
}

// judgeCheckFresh is the check's judgement before its open and at its
// activation, as a session's activation's (checkDSDActivation): the policy
// read afresh within the check's context, which reconciles a change, then the
// check judged against the set published now, so one a refresh beside it has
// not reconciled yet is judged here.
func (b *Backend) judgeCheckFresh(c *checkHold, where string) bool {
	if _, err := b.refreshDSDWithin(c.ctx, where); err != nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return c.ctx.Err() == nil && b.judgeCheckLocked(c, b.dsd.Load())
}

// judgeCheckLocked judges c against next by the path that admitted it: kept,
// and moved to it, only where next was computed for the identity c was
// admitted under, compared first, before the open too, and then, for a check
// the set's plan admitted, policy.Reconcile keeps c's plan on its frames and
// the identity its sink opened, or, for one the candidate admitted
// (byCandidate), next's candidate at the rate is that plan (candidateKeeps),
// so one is never kept through the other's: a candidate gone or changed ends
// its check, a clean answer making it the set's plan included; ended
// policy_changed otherwise, never planned again. A driver_accepted the open
// records moves neither (its evidence is no part of Plan.Same, nor of
// dsdSet.same). b.mu is held.
func (b *Backend) judgeCheckLocked(c *checkHold, next *dsdSet) bool {
	if next == c.admitted {
		return true
	}
	keep := next.computed && next.set.Identity().Same(c.identity)
	switch {
	case !keep:
	case c.byCandidate:
		keep = candidateKeeps(next.set, c.plan)
	default:
		o := policy.Occurrence{Plan: c.plan, Opened: c.opened}
		if c.sink != nil {
			p := c.sink.Position()
			o.Submitted, o.Written, o.Played, o.Audible = p.Submitted, p.Written, p.Played, p.Played > 0
		}
		keep = policy.Reconcile(next.set, o).Action == policy.Keep
	}
	if keep {
		c.admitted = next
		return true
	}
	b.endCheckLocked(c, CheckFailed, CheckPolicyChanged)
	return false
}

// candidateKeeps reports whether next's candidate at p's rate
// (policy.Plan.Candidate) is p (policy.Plan.Same): what keeps a check its
// admission took by the candidate. Its caller compares the identity first,
// which the identity its sink opened is (checkNode); no session's
// reconciliation reads a candidate.
func candidateKeeps(next policy.Set, p policy.Plan) bool {
	c := next.At(p.BitRate).Plan.Candidate
	return c != nil && p.Same(*c)
}

// reconcileCheck judges c, the check that held the slot as reconcileDSD read
// the sessions, against next, a set a refresh published, under the refreshes'
// lock, in one step under b.mu that waits for nothing, while c holds the slot;
// a panic there ends it backend_failed.
func (b *Backend) reconcileCheck(c *checkHold, next *dsdSet) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.checking != c {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			b.logPanic("the DSD check's reconciliation", r)
			b.endCheckLocked(c, CheckFailed, CheckBackendFailed)
		}
	}()
	b.judgeCheckLocked(c, next)
}

// monitorCheck is the check's monitor of the policy, startDSDMonitor's tick
// from its open to its sink's end: each reads the policy again, the DoP gain
// check among it, which reconciles the check.
func (b *Backend) monitorCheck(c *checkHold, sink *alsapcm.DSDSink) {
	ticks, stop := b.dsdTicks()
	b.spawn(func() {
		defer stop()
		defer func() {
			if r := recover(); r != nil {
				b.logPanic("the DSD check's monitor", r)
				c.end(CheckFailed, CheckBackendFailed)
			}
		}()
		for {
			select {
			case <-sink.Done():
				return
			case <-ticks:
				_, _ = b.refreshDSD("the DSD check's monitor")
			}
		}
	})
}

// cleanCheck is the check's cleanup, with its own recover, and whether it is
// confirmed, with the error that left it not: nothing opened; a sink, by its
// CleanupResult once its release has ended (Done); a cancelled open, by its
// completion (CompletionResult); a refused open, by its own answer. Each is a
// failure exactly where that result wraps alsapcm.ErrReleaseUnconfirmed: an
// open's own refusal, or a panic the sink recovered whose release was
// confirmed (alsapcm.ErrPanicked alone), leaves the slot reusable; a panic in
// the cleanup itself answers an error that wraps it. A cleanup that never
// answers keeps the slot.
func (b *Backend) cleanCheck(c *checkHold) (confirmed bool, err error) {
	defer func() {
		if r := recover(); r != nil {
			b.logPanic("the DSD check's cleanup", r)
			confirmed, err = false, fmt.Errorf("player backend: the DSD check's cleanup panicked; the bridge's log has why: %w", alsapcm.ErrReleaseUnconfirmed)
		}
	}()
	b.mu.Lock()
	sink, err := c.sink, c.openErr
	b.mu.Unlock()
	var pending interface {
		Completion() <-chan struct{}
		CompletionResult() error
	}
	switch {
	case sink != nil:
		sink.BeginRelease(alsapcm.Immediate)
		<-sink.Done()
		err = sink.CleanupResult()
	case errors.As(err, &pending):
		<-pending.Completion()
		err = pending.CompletionResult()
	}
	return !errors.Is(err, alsapcm.ErrReleaseUnconfirmed), err
}

// endCheck gives the slot back once the check's cleanup has answered, in one
// step under b.mu that publishes it: the check's end recorded, its cause's
// (a complete one's completion dated, by the policy's clock), or failed
// release_unconfirmed for a cleanup not confirmed, which fences the device as
// a release never confirmed does, until serve restarts; then done closed and
// the end said, a fence at Warn with what its cleanup answered.
func (b *Backend) endCheck(c *checkHold, confirmed bool, err error) {
	phase, reason := CheckFailed, CheckBackendFailed
	var cause *checkCause
	if errors.As(context.Cause(c.ctx), &cause) {
		phase, reason = cause.phase, cause.reason
	}
	if !confirmed {
		phase, reason = CheckFailed, CheckReleaseUnconfirmed
	}
	b.mu.Lock()
	b.terminalUnconfirmed = b.terminalUnconfirmed || !confirmed
	c.phase, c.reason, c.confirmed = phase, reason, confirmed
	if phase == CheckComplete {
		c.completed = b.dsdNow()
	}
	if b.checking == c {
		b.checking = nil
	}
	b.publishCheckLocked()
	b.mu.Unlock()
	c.end(phase, reason)
	close(c.done)
	if !confirmed {
		b.log.Warn("player: the DSD check's cleanup did not answer cleanly: nothing opens the DAC until bridge serve restarts", "err", errorText(err))
		return
	}
	b.log.Info("player: the DSD check ended", "transport", c.transport, "bitRate", c.bitRate, "phase", string(phase), "reason", string(reason))
}

// checkFault is the reason a failed call to the check's sink ends it with.
func checkFault(err error) CheckReason {
	switch alsapcm.ReasonOf(err) {
	case alsapcm.ReasonDisconnected, alsapcm.ReasonNoDevice:
		return CheckOutputGone
	case alsapcm.ReasonBusy:
		return CheckDeviceBusy
	case alsapcm.ReasonWrongDevice:
		return CheckWrongDevice
	case alsapcm.ReasonPanicked:
		return CheckBackendFailed
	}
	return CheckDeviceError
}

// StopCheck ends the check in the slot (stopped, deleted: DELETE's), in a
// step that publishes it stopping, and waits for its cleanup at most
// CheckStopWait or until ctx ends: how that check's cleanup ended (none with
// no check in the slot; pending while it has not answered, the slot still
// the check's), with the pair published as it answers.
func (b *Backend) StopCheck(ctx context.Context) (CheckCleanup, PlayerPolicy) {
	b.mu.Lock()
	c := b.checking
	if c != nil {
		b.endCheckLocked(c, CheckStopped, CheckDeleted)
	}
	b.mu.Unlock()
	if c == nil {
		return CheckCleanupNone, b.Policy()
	}
	expired := make(chan struct{})
	defer b.cfg.AfterFunc(CheckStopWait, func() { close(expired) })()
	select {
	case <-c.done:
	case <-expired:
	case <-ctx.Done():
	}
	h := CheckHandle{c: c}
	return h.Cleanup(), b.Policy()
}

// CloseChecks closes the checks for good, as CloseTones closes the tones: none
// counted from then on, under gmu, and under b.mu none admitted and the check
// in the slot ended stopped, serve_stopping.
func (b *Backend) CloseChecks() {
	b.gmu.Lock()
	b.cclosed = true
	b.gmu.Unlock()
	b.mu.Lock()
	defer b.mu.Unlock()
	b.checksClosed = true
	if c := b.checking; c != nil {
		b.endCheckLocked(c, CheckStopped, CheckServeStopping)
	}
}

// publishCheckLocked publishes the check's member of the read side as its
// record stands (withCheckLocked), beside the set as it is, a new pair where
// it changed: the step every transition of the check publishes by, under
// b.mu, which every store of the pair takes, never the refreshes' lock, which
// a refresh holds across the box's reading. A transition that meets a
// choice's write holding its reading stores nothing and owes a refresh
// (checkOwed, which that write's end makes refreshOwed): no pair is stored
// between that write's reading and its publication, which takes the record as
// it stands then. b.mu is held.
func (b *Backend) publishCheckLocked() {
	pub := b.dsdPub.Load()
	switch {
	case pub == nil:
	case b.choiceReading:
		b.checkOwed = true
	default:
		if v := b.withCheckLocked(pub.view); !reflect.DeepEqual(v, pub.view) {
			b.dsdPub.Store(&dsdPublished{set: pub.set, view: v, seq: pub.seq + 1})
		}
	}
}

// withCheckLocked is v with the check's member (DSDDAC.Check) as checkRec
// stands, the running check or the last, while v's DAC is the one that check
// was admitted for (its token), and none otherwise; v's DAC is copied, never
// written, where the member changes. b.mu is held.
func (b *Backend) withCheckLocked(v DSDView) DSDView {
	if v.DAC == nil {
		return v
	}
	var m *DSDCheck
	if c := b.checkRec; c != nil && c.token == v.DAC.Token {
		s := b.checkStatusLocked(c)
		m = &DSDCheck{ID: s.ID, Transport: s.Transport, BitRate: s.BitRate, Phase: s.Phase, Reason: s.Reason}
	}
	if reflect.DeepEqual(m, v.DAC.Check) {
		return v
	}
	d := *v.DAC
	d.Check = m
	v.DAC = &d
	return v
}

// CheckStatus is the check in the slot or the last one, false while there was
// none, read under b.mu with no I/O (checkStatusLocked).
func (b *Backend) CheckStatus() (CheckStatus, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.checkRec == nil {
		return CheckStatus{}, false
	}
	return b.checkStatusLocked(b.checkRec), true
}

// checkStatusLocked is c's record, stopping while it holds the slot past its
// end: what CheckStatus reads and what Check answers, built in its admission's
// step. One literal from a phase of its own, as ToneStatus (internal/api's
// census of the protocol's observation states). b.mu is held.
func (b *Backend) checkStatusLocked(c *checkHold) CheckStatus {
	phase, reason := c.phase, c.reason
	if b.checking == c && c.ctx.Err() != nil {
		phase, reason = CheckStopping, ""
	}
	var opened *policy.Identity
	if c.opened != nil {
		id := *c.opened
		opened = &id
	}
	return CheckStatus{ID: c.id, Transport: c.transport, BitRate: c.bitRate, Phase: phase, Reason: reason, Token: c.token, Identity: c.identity, Opened: opened}
}

// AnswerRequest is a listener's answer to a check (POST
// /v1/outputs/{id}/dsd/check/answer): the DAC's key and token as the read
// side published them, the check's ID, and what was heard: clean, noise or
// nothing.
type AnswerRequest struct {
	DACKey   string
	DACToken DACToken
	CheckID  string
	Heard    string
}

// Check refuses a request that is no answer at all: a token not of a token's
// form, no check's ID, or a word heard that is none of the three.
func (r AnswerRequest) Check() error {
	if !r.DACToken.Valid() || r.CheckID == "" || r.Heard != dsdstate.AnswerClean && r.Heard != dsdstate.AnswerNoise && r.Heard != dsdstate.AnswerNothing {
		return errors.New("player backend: the answer is not one the check takes")
	}
	return nil
}

// AnswerCheck writes a listener's answer to the check req names (the
// design's section 6.4) by a choice's path (ChooseDSD's checks, in its order:
// bad_request; the turn; the writes closed; the lease, playback_not_stopped;
// the DAC read, dac_unavailable; its key and token, dac_changed; the state
// opened; the fence, persist_uncertain), with a second decision past the
// fence, under b.mu: the check answerable (check_not_answerable for an ID of
// no check of this boot, one that did not complete or completed more than
// CheckAnswerable ago, one answered, one whose token is not this reading's or
// whose firmware did not read). The answer replaces the transport's at the
// rate under that check's firmware, in the entry of the identity it was
// admitted for, dated, and noise or nothing takes the transport's first use
// away, in the store's one write; a write that reached the file, durable or
// not, makes the check answered, published by the write's publication, whose
// pair the answer carries as ChooseDSD's does.
func (b *Backend) AnswerCheck(ctx context.Context, req AnswerRequest) ChoiceResult {
	if req.Check() != nil {
		return ChoiceResult{Outcome: ChoiceBadRequest}
	}
	var c *checkHold
	j := choiceJournal{to: req.Heard}
	res := b.writeState(ctx, stateWrite{what: "a DSD check's answer", key: req.DACKey, token: req.DACToken,
		admit: func(r *dsdReading) ChoiceOutcome {
			b.mu.Lock()
			defer b.mu.Unlock()
			c = b.checkRec
			if c == nil || c.id != req.CheckID || b.checking == c || c.phase != CheckComplete || c.token != r.token ||
				canonicalBCD(c.identity.BCDDevice) == "" || b.dsdNow().Sub(c.completed) > CheckAnswerable {
				return ChoiceCheckNotAnswerable
			}
			return ""
		},
		apply: func(s dsdstate.State, _ *dsdReading, now time.Time) (dsdstate.State, error) {
			n := s.Clone()
			d, bcd := n.Entry(c.identity.Key, c.identity.Stamp), canonicalBCD(c.identity.BCDDevice)
			d.Listener = append(slices.DeleteFunc(d.Listener, func(a dsdstate.Answer) bool {
				return a.Transport == c.transport && a.BitRate == c.bitRate && a.BCDDevice == bcd
			}), dsdstate.Answer{Transport: c.transport, BitRate: c.bitRate, BCDDevice: bcd, Answer: req.Heard, On: now.UTC().Format(time.DateOnly)})
			if req.Heard != dsdstate.AnswerClean {
				d.FirstUse = slices.DeleteFunc(d.FirstUse, func(f dsdstate.FirstUse) bool { return f.Transport == c.transport })
			}
			return n, nil
		},
		wrote: func(bool) {
			b.mu.Lock()
			c.phase = CheckAnswered
			b.publishCheckLocked()
			b.mu.Unlock()
		}}, &j)
	say := b.dsdLog().Info
	if j.why != "" || res.Outcome == ChoicePersistUncertain {
		say = b.dsdLog().Warn
	}
	say("player: a listener's answer to the DSD check", "dac", j.dac, "heard", req.Heard, "outcome", string(res.Outcome), "why", j.why)
	return res
}

// checkPreparing is the CheckPreparing seam.
func (b *Backend) checkPreparing(ctx context.Context) {
	if sm := b.cfg.Seams; sm != nil && sm.CheckPreparing != nil {
		sm.CheckPreparing(ctx)
	}
}
