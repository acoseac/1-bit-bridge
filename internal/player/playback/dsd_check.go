package playback

import (
	"context"
	"crypto/hmac"
	"errors"
	"fmt"
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
// effective set plays now, by that tuple's plan, while the engine's check hold
// (engine.EnterCheck) keeps the engine closed: the device slot's fourth holder,
// beside a session, the silent probe and the console's tone. Every end is a
// cause on its context, the first standing, which revokes its sink; the slot
// is given back only once its cleanup is confirmed. Its routes and the
// listener's answer are B12 PR 4's. Lock order: dsdMu, then b.mu, as a
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
// stopped or failed.
type CheckPhase string

const (
	CheckPreparing CheckPhase = "preparing"
	CheckPlaying   CheckPhase = "playing"
	CheckComplete  CheckPhase = "complete"
	CheckStopping  CheckPhase = "stopping"
	CheckStopped   CheckPhase = "stopped"
	CheckFailed    CheckPhase = "failed"
)

// CheckPhases is every CheckPhase.
func CheckPhases() []CheckPhase {
	return []CheckPhase{CheckPreparing, CheckPlaying, CheckComplete, CheckStopping, CheckStopped, CheckFailed}
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
// supervisor's gate's (ToneOutput), dsd_not_in_this_release, dsd_dac_unread,
// serve_stopping, device_busy and release_unconfirmed.
const (
	CheckBadRequest     = "bad_request"
	CheckDACChanged     = "dac_changed"
	CheckConverted      = "converted"
	CheckOtherTransport = "other_transport"
)

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

// CheckRefused is a check refused, by its word: nothing was opened.
type CheckRefused struct{ Word string }

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

// checkHold is a check given the slot: what it was admitted with; its context,
// cancelled with its cause by whatever ends it; done, closed once it gave the
// slot back; and, under b.mu, the set it plays under, its sink or its open's
// error, the identity its sink opened, its phase and its reason.
type checkHold struct {
	id, transport string
	bitRate       int
	token         DACToken
	identity      policy.Identity
	plan          policy.Plan
	ctx           context.Context
	cancel        context.CancelCauseFunc
	done          chan struct{}

	admitted *dsdSet
	sink     *alsapcm.DSDSink
	openErr  error
	opened   *policy.Identity
	phase    CheckPhase
	reason   CheckReason
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

// Check starts the check of req's tuple and answers its record once it holds
// the device slot, its run going on, on a goroutine the backend counts from
// before the slot (Wait). Refused, in order: a request that is none, a backend
// with no DSD policy, CloseChecks having run, then as reserveCheck says. ctx
// bounds its waits, and ends them with its error. The record is the one its
// admission built, and the count is given back on every way out before the
// hand-over to its run, a panic's included.
func (b *Backend) Check(ctx context.Context, req CheckRequest) (CheckStatus, error) {
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
		return CheckStatus{}, &CheckRefused{Word: word}
	}
	handed := false
	defer func() {
		if !handed {
			b.leave()
		}
	}()
	c, st, err := b.reserveCheck(ctx, req)
	if c == nil {
		return CheckStatus{}, err
	}
	b.spawn(func() {
		select {
		case <-req.Stop:
			c.end(CheckStopped, CheckStop)
		case <-c.ctx.Done():
		}
	})
	handed = true
	go b.counted(false, func() { b.runCheck(c) })
	return st, nil
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

// reserveCheck reads the policy afresh within ctx, then takes the device slot
// for req as a Start's hold takes it, in one step under b.mu: refused while
// CloseChecks has run, the supervisor's gate is shut (its word), a release was
// never confirmed, or a choice's lease, a session's record or hold, or another
// check stands (device_busy); the probe or the tone (ended preempted) waited
// for, the check among the waiters meanwhile, device_busy past its bound; then
// admitted (admitCheckLocked), its record built in that step. The take moves
// starts, which voids an ending probe's answers as a Start's take does
// (takeProbe). A request that has ended is refused with its error first,
// before the slot is looked at, so no tone is preempted for it, and where its
// wait ends at its bound.
func (b *Backend) reserveCheck(ctx context.Context, req CheckRequest) (*checkHold, CheckStatus, error) {
	if _, err := b.refreshDSDWithin(ctx, "a DSD check's admission"); err != nil {
		if ctx.Err() != nil {
			return nil, CheckStatus{}, ctx.Err()
		}
		return nil, CheckStatus{}, &CheckRefused{Word: string(ReasonDACUnread)}
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
			return nil, CheckStatus{}, err
		}
		var held <-chan struct{}
		var bound time.Duration
		word := ""
		switch {
		case b.checksClosed:
			word = string(CheckServeStopping)
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
				var c *checkHold
				if c, word = b.admitCheckLocked(req); c != nil {
					st := b.checkStatusLocked(c)
					b.mu.Unlock()
					return c, st, nil
				}
			}
		}
		if word != "" {
			b.mu.Unlock()
			return nil, CheckStatus{}, &CheckRefused{Word: word}
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
				return nil, CheckStatus{}, err
			}
			return nil, CheckStatus{}, &CheckRefused{Word: string(CheckDeviceBusy)}
		case <-ctx.Done():
			return nil, CheckStatus{}, ctx.Err()
		}
	}
}

// admitCheckLocked admits req by the pair published now and gives it the slot:
// the DAC's key and token the read side's (dac_changed), and the tuple the
// set's plan at the rate, never one merely offered (the rate's own refusal
// word, converted, other_transport). b.mu is held.
func (b *Backend) admitCheckLocked(req CheckRequest) (*checkHold, string) {
	pub := b.dsdPub.Load()
	if d := pub.view.DAC; d == nil || d.Key != req.DACKey || !hmac.Equal([]byte(d.Token), []byte(req.DACToken)) {
		return nil, CheckDACChanged
	}
	p := pub.set.set.At(req.BitRate).Plan
	switch {
	case p.Outcome == policy.OutcomeConverted:
		return nil, CheckConverted
	case p.Outcome != policy.OutcomeNative && p.Outcome != policy.OutcomeDoP:
		return nil, string(p.Reason)
	case tupleTransport(p) != req.Transport:
		return nil, CheckOtherTransport
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	c := &checkHold{id: fmt.Sprintf("%x", randomBoot()), transport: req.Transport, bitRate: req.BitRate, token: req.DACToken, identity: pub.set.set.Identity(),
		plan: p, ctx: ctx, cancel: cancel, done: make(chan struct{}), admitted: pub.set, phase: CheckPreparing}
	b.checking, b.checkRec = c, c
	b.starts.Add(1)
	return c, ""
}

// runCheck is a check's run, on the goroutine its count stands for, and, on
// every way out, its cleanup (cleanCheck, with its own recover) and the slot
// given back (endCheck).
func (b *Backend) runCheck(c *checkHold) {
	defer func() {
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

// judgeCheckLocked judges c against next: kept, and moved to it, only where
// next was computed for the identity c was admitted under, compared first,
// before the open too, and policy.Reconcile keeps c's plan on its frames and
// the identity its sink opened; ended policy_changed otherwise, never planned
// again. b.mu is held.
func (b *Backend) judgeCheckLocked(c *checkHold, next *dsdSet) bool {
	if next == c.admitted {
		return true
	}
	o := policy.Occurrence{Plan: c.plan, Opened: c.opened}
	if c.sink != nil {
		p := c.sink.Position()
		o.Submitted, o.Written, o.Played, o.Audible = p.Submitted, p.Written, p.Played, p.Played > 0
	}
	if next.computed && next.set.Identity().Same(c.identity) && policy.Reconcile(next.set, o).Action == policy.Keep {
		c.admitted = next
		return true
	}
	c.end(CheckFailed, CheckPolicyChanged)
	return false
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
			c.end(CheckFailed, CheckBackendFailed)
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
// step under b.mu: the check's end recorded, its cause's, or failed
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
	c.phase, c.reason = phase, reason
	if b.checking == c {
		b.checking = nil
	}
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

// StopCheck ends the check in the slot (stopped, deleted: B12 PR 4's DELETE)
// and waits for the slot back at most CheckStopWait or until ctx ends: true
// once it is back, or with no check in the slot; false while its cleanup has
// not answered, the slot still the check's.
func (b *Backend) StopCheck(ctx context.Context) bool {
	b.mu.Lock()
	c := b.checking
	if c != nil {
		c.end(CheckStopped, CheckDeleted)
	}
	b.mu.Unlock()
	if c == nil {
		return true
	}
	expired := make(chan struct{})
	defer b.cfg.AfterFunc(CheckStopWait, func() { close(expired) })()
	select {
	case <-c.done:
		return true
	case <-expired:
	case <-ctx.Done():
	}
	return false
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
		c.end(CheckStopped, CheckServeStopping)
	}
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

// checkPreparing is the CheckPreparing seam.
func (b *Backend) checkPreparing(ctx context.Context) {
	if sm := b.cfg.Seams; sm != nil && sm.CheckPreparing != nil {
		sm.CheckPreparing(ctx)
	}
}
