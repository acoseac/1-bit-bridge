package playback

import (
	"context"
	"errors"
	"sync"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/policy"
)

// A write of the bound DAC's DSD choice (B728 B9; the design's section 7.2),
// PUT /v1/outputs/{id}/dsd's, and what it asks of what plays.
//
// One write at a time, end to end (the writes' turn, choiceTurn): its
// reading of the box and of the state, the set it would publish, what that
// set would interrupt, the state's write, the publish, and the epoch its
// answer carries, read before the turn is given back. So a write that finds
// the revision moved answers stale only once the write that moved it has
// published, and the revision a stale answer carries is the one the next
// write compares with, which no other write can have moved past (binding
// note 2; round 2, point 4).
//
// Its checks are answered in the design's order, and what the write would
// interrupt last of them (round 2, point 3): an earlier write that landed and
// is not known to be durable fences every write until the next start, which is
// answered once the DAC is known and before anything of the state's is read;
// what the set would interrupt is classified before the state's write, with no
// lock of the store's held, and answered inside it, after the state's own
// checks and only where the state the write is made from gives the sets that
// classification read. And a write is judged against the set published (round
// 2, point 5): where its reading of the DAC gives another set than the one
// published, a reading that glitched or the box's change, that reading is
// published and reconciled first, a refresh of the write's own, and the write
// reads again, so what it is told it would interrupt is what reconciliation
// does with what plays then. A write that goes round maxChoiceRounds times,
// the state or the box moving under it each time, is not made.
//
// The write between its reading and its publish is pending (Backend.pending):
// the set it would publish, stored before what it would interrupt is
// classified. Every step that ends a DSD occurrence's open reads it under the
// session's lock (lockSettled), and one whose rate the pending set plays
// otherwise than the set published waits for the write to settle, its lock
// let go, a refusal of the policy's the write lifts among them, asked again
// once it settles (settleOpen, round 2); the producer holds back from a sink
// an opened occurrence at such a rate (next, transition, holdLocked), a
// gapless one only while its sink holds more than its floor; and an open of
// an occurrence's own sink, which the step that found no write pending
// claims (occ.claimed), is counted by the classification as reaching its
// sink, its adoption and activation waiting for the write (transition,
// resume, checkDSDActivation; round 2). So each occurrence the write could
// interrupt was either classified, under its session's lock, or is admitted
// under what the write leaves published, and what the answer promised holds
// across the publish (binding note 3). A write that fails before it lands
// leaves the set as it was; one that landed and is not known to be durable
// is published as the intersection of the two states' sets (landedChoice),
// until the next start reads the file. Every end of a write settles what it
// left pending, a panic's included, so nothing waits for a write that will
// not publish.
//
// Lock order: the writes' turn, then the refreshes' (dsdMu), then the
// backend's (b.mu), then each session's (s.mu), then a sink's. The state's
// store is taken alone: its Update runs with no lock of the backend's held,
// and its closure is pure, no lock taken inside it, so no fsync is ever
// waited for under a lock an occurrence's admission takes. Playback calls
// Store.Update only on goroutines of its own (recordDSD, through spawnWrite),
// under no lock; a choice's write calls it on its caller's goroutine, counted
// among those writes and refused once they are closed (dsdWrite,
// CloseDSDWrites), the gate's lock (gmu) let go before the Update. The probe's
// lock, which a written choice takes to owe its probe (oweProbe), is taken
// under none of these.

// ChoiceOutcome is how a write of the bound DAC's DSD choice ended (B728 B9;
// the design's section 7.2): written, or the word it was refused with.
type ChoiceOutcome string

const (
	ChoiceWritten                ChoiceOutcome = "written"
	ChoiceBadRequest             ChoiceOutcome = "bad_request"
	ChoiceDACUnavailable         ChoiceOutcome = "dac_unavailable"
	ChoiceDACChanged             ChoiceOutcome = "dac_changed"
	ChoiceStale                  ChoiceOutcome = "stale"
	ChoiceModeNotOffered         ChoiceOutcome = "mode_not_offered"
	ChoiceConfirmationRequired   ChoiceOutcome = "dop_confirmation_required"
	ChoiceFirstUseUnacknowledged ChoiceOutcome = "first_use_unacknowledged"
	ChoiceWouldInterrupt         ChoiceOutcome = "would_interrupt"
	ChoicePersistFailed          ChoiceOutcome = "persist_failed"
	ChoicePersistUncertain       ChoiceOutcome = "persist_uncertain"
	ChoiceStateUnreadable        ChoiceOutcome = "state_unreadable"
)

// refusalOutcomes are the policy's refusals by the outcome each is.
var refusalOutcomes = map[policy.ChoiceReason]ChoiceOutcome{
	policy.ChoiceStale:                  ChoiceStale,
	policy.ChoiceModeNotOffered:         ChoiceModeNotOffered,
	policy.ChoiceConfirmationRequired:   ChoiceConfirmationRequired,
	policy.ChoiceFirstUseUnacknowledged: ChoiceFirstUseUnacknowledged,
}

// ChoiceRequest is a write of the bound DAC's DSD choice: the DAC's key as
// GET /v1/outputs publishes it, the choice, and whether the write may
// interrupt what plays.
type ChoiceRequest struct {
	DACKey    string
	Choice    policy.Choice
	Interrupt bool
}

// ChoiceResult is a write's outcome and the epoch published once it ended:
// the set and its read side a written choice published, read as GET
// /v1/outputs reads them afterwards (Policy), inside the writes' turn.
type ChoiceResult struct {
	Outcome ChoiceOutcome
	Policy  PlayerPolicy
}

// maxChoiceRounds bounds the rounds a write takes when what it would publish,
// or what the state gives without it, moves between its reading and its write
// (errChoiceRound: another writer's change of the state, its answers or its
// evidence), or when its reading of the DAC gives another set than the one
// published (a refresh of the write's own, then a round): past it the write
// is not made, persist_failed, nothing changed, and the client writes again,
// its revision not behind, so never stale.
const maxChoiceRounds = 8

// errChoiceRound is a write's closure finding the state moved since the
// write's reading in a way that changes what the write would publish, or
// what the state gives without it: nothing is written, and the write reads
// again.
var errChoiceRound = errors.New("player backend: what the choice's write would publish moved since it was read")

// errWouldInterrupt is a write's closure answering what its classification
// found, once the state's own checks passed on the state the write is made
// from: the set it would publish interrupts what plays, without the go-ahead.
var errWouldInterrupt = errors.New("player backend: the choice's write would interrupt what plays")

// ChooseDSD writes the bound DAC's DSD choice (the design's section 7.2), and
// answers how it ended with the epoch published then, read inside the writes'
// turn. Its checks, each answered with its word, nothing written and nothing
// published: the words of the choice (bad_request); a bound DAC read fresh
// (dac_unavailable); req.DACKey its key as the read side publishes it
// (wireKey: dac_changed); no earlier write landed and not known to be durable
// (persist_uncertain, the fence, answered before the state is read); the
// policy's, inside the state's one write, on the state that write is made from
// (policy.Choose: stale, mode_not_offered, dop_confirmation_required,
// first_use_unacknowledged); and then, without req.Interrupt, nothing the set
// the write would publish interrupts, against the set published
// (writeInterrupts, classified before the state's write: would_interrupt).
// Then the state is written, durably (persist_failed before it lands, nothing
// changed; persist_uncertain once it lands and its sync fails; state_unreadable
// where the file is refused in memory), a probe is made due and owed
// (oweProbe, ProbeDSDPending), the set the state gives is published and
// reconciled (refreshDSD), and the write is answered from that epoch. The
// state's write is one of the writes of the player's DSD state (dsdWrite):
// once the player's stop has closed them (CloseDSDWrites) it is not made,
// persist_failed, nothing changed, and one under way at the close keeps them
// open until it returns. A stale answer reads the policy again
// first, so the revision it carries is the state's. A write that cannot be made
// within maxChoiceRounds rounds, the state or the box moving under it, is
// persist_failed too: nothing changed either way, and the client writes again
// from the revision GET /v1/outputs publishes; its line says which. ctx bounds
// the wait for another write's turn alone (persist_failed where it ends
// first): once the state's write begins nothing interrupts it.
func (b *Backend) ChooseDSD(ctx context.Context, req ChoiceRequest) ChoiceResult {
	j := choiceJournal{}
	res := b.chooseDSD(ctx, req, &j)
	attrs := []any{"dac", j.dac, "from", j.from, "to", j.to, "outcome", string(res.Outcome)}
	switch {
	case j.why != "":
		b.dsdLog().Warn("player: a write of the bound DAC's DSD choice was not made", append(attrs, "why", j.why)...)
	case res.Outcome == ChoicePersistUncertain:
		b.dsdLog().Warn("player: a write of the bound DAC's DSD choice reached the state's file and is not known to be durable", attrs...)
	default:
		b.dsdLog().Info("player: a write of the bound DAC's DSD choice", attrs...)
	}
	return res
}

// choiceJournal is what a write's line says: the bound DAC's key as the wire
// names it, the mode that applied before the write and the one it asks, each
// "" where the write did not come to it; and why a write the state's store,
// its fence or its rounds refused was not made.
type choiceJournal struct{ dac, from, to, why string }

// answerNow is a write's answer made outside the writes' turn, nothing read
// or written: the epoch published now.
func (b *Backend) answerNow(o ChoiceOutcome) ChoiceResult {
	return ChoiceResult{Outcome: o, Policy: b.Policy()}
}

func (b *Backend) chooseDSD(ctx context.Context, req ChoiceRequest, j *choiceJournal) (res ChoiceResult) {
	if err := req.Choice.Check(); err != nil {
		return b.answerNow(ChoiceBadRequest)
	}
	j.to = req.Choice.Mode
	p := b.cfg.DSD
	if p == nil {
		return b.answerNow(ChoiceDACUnavailable)
	}
	if !b.takeChoiceTurn(ctx) {
		j.why = "it could not begin within the request's time: another write held the turn"
		return b.answerNow(ChoicePersistFailed)
	}
	defer b.giveChoiceTurn()
	// The epoch the answer carries, read at every return inside the turn,
	// before the turn is given back (round 2, point 4): read after it, the
	// next write could have published, and a stale answer carry a revision
	// that write moved past.
	defer func() { res.Policy = b.Policy() }()
	// The round's pending set, settled at every end of the write, before its
	// turn is given back: a write that panics (the API's recoverer answers
	// it) leaves nothing pending, so no occurrence waits for a write that will
	// not publish, and what it wrote, if it wrote, the next refresh publishes.
	var pend *pendingChoice
	defer func() { b.settleChoice(pend) }()
	for round := 0; ; round++ {
		if round == maxChoiceRounds {
			j.why = "what it would publish, or the DAC as it read it, moved under it in every one of its rounds"
			return ChoiceResult{Outcome: ChoicePersistFailed}
		}
		r, _ := b.readBox()
		if r == nil {
			return ChoiceResult{Outcome: ChoiceDACUnavailable}
		}
		j.dac = wireKey(r.key, r.m.USB)
		if j.dac != req.DACKey {
			return ChoiceResult{Outcome: ChoiceDACChanged}
		}
		if p.Store == nil {
			j.why = "the player's DSD state did not open"
			return ChoiceResult{Outcome: ChoiceStateUnreadable}
		}
		if b.choiceFenced() {
			// No write is made until the next start reads the file: answered
			// first, before any of the state's checks, which a write that
			// cannot be made does not make.
			j.why = "an earlier write reached the state's file and is not known to be durable"
			return ChoiceResult{Outcome: ChoicePersistUncertain}
		}
		now := b.dsdNow()
		st := p.Store.State()
		j.from, _, _ = st.ChoiceFor(r.key)
		if st.Unreadable() {
			j.from = dsdstate.ModeOff
		}
		// What the write would publish, and what the state gives without it,
		// from one reading: the pending set, stored before what it would
		// interrupt is classified. A choice the state as read refuses has
		// none, and goes to the store's write all the same, whose closure
		// decides on the state the write is made from.
		pend = nil
		var cur, want *dsdSet
		interrupts := false
		if next, err := policy.Choose(r.inputs(st), req.Choice, now); err == nil {
			cur, _ = b.setOver(r, st)
			if !cur.same(b.dsd.Load()) {
				// The write's reading gives another set than the one published:
				// published and reconciled first, so the write is judged against
				// what plays then, and read again (round 2, point 5).
				b.refreshDSD()
				continue
			}
			want, _ = b.setOver(r, next)
			pend = &pendingChoice{set: want, done: make(chan struct{})}
			b.pending.Store(pend)
			interrupts = b.writeInterrupts(cur, want)
			b.choicePending()
		}
		var wrote dsdstate.State
		var err error
		if !b.dsdWrite("a choice's write", func() {
			err = p.Store.Update(func(s *dsdstate.State) error {
				next, err := policy.Choose(r.inputs(*s), req.Choice, now)
				if err != nil {
					return err
				}
				if pend == nil {
					return errChoiceRound
				}
				if c, _ := b.setOver(r, *s); !c.same(cur) {
					return errChoiceRound
				}
				if n, _ := b.setOver(r, next); !n.same(want) {
					return errChoiceRound
				}
				// What the write would interrupt, the last of its checks, after
				// the state's own on the state it is made from (round 2, point
				// 3): its classification was made before, under the pending set
				// and no lock of the store's, of the two sets this state gives.
				if interrupts && !req.Interrupt {
					return errWouldInterrupt
				}
				*s, wrote = next, next.Clone()
				return nil
			})
		}) {
			j.why = "the player's stop closed the writes of its DSD state"
			return ChoiceResult{Outcome: ChoicePersistFailed}
		}
		b.choiceWritten(err)
		var refusal *policy.ChoiceRefusal
		switch {
		case errors.Is(err, errWouldInterrupt):
			return ChoiceResult{Outcome: ChoiceWouldInterrupt}
		case errors.Is(err, errChoiceRound):
			b.settleChoice(pend)
			continue
		case errors.As(err, &refusal):
			b.settleChoice(pend)
			if refusal.Reason == policy.ChoiceStale {
				b.refreshDSD()
			}
			if out, ok := refusalOutcomes[refusal.Reason]; ok {
				return ChoiceResult{Outcome: out}
			}
			return ChoiceResult{Outcome: ChoiceBadRequest}
		case errors.Is(err, dsdstate.ErrNotWritable):
			j.why = "the player's DSD state file is refused in memory"
			return ChoiceResult{Outcome: ChoiceStateUnreadable}
		case errors.Is(err, dsdstate.ErrLandedNotHeld):
			j.why = "an earlier write reached the state's file and is not known to be durable"
			return ChoiceResult{Outcome: ChoicePersistUncertain}
		case errors.Is(err, atomicwrite.ErrLanded):
			// The file holds the write's state, not known to be durable, and the
			// store the state as it was: until the next start reads the file, a
			// rate plays only where the two play it alike.
			b.landedChoice.Store(&wrote)
			b.refreshDSD()
			b.settleChoice(pend)
			return ChoiceResult{Outcome: ChoicePersistUncertain}
		case err != nil:
			j.why = "the state's write failed before it replaced the file"
			return ChoiceResult{Outcome: ChoicePersistFailed}
		}
		b.oweProbe()
		b.refreshDSD()
		b.settleChoice(pend)
		return ChoiceResult{Outcome: ChoiceWritten}
	}
}

// choiceFenced reports whether an earlier write landed and is not known to
// be durable, which fences every write of the state until the next start:
// one of the backend's own choices (landedChoice), or any write of the
// store's, an evidence write's included, where the store says so
// (dsdstate.Store's Landed).
func (b *Backend) choiceFenced() bool {
	if b.landedChoice.Load() != nil {
		return true
	}
	l, ok := b.cfg.DSD.Store.(interface{ Landed() error })
	return ok && l.Landed() != nil
}

// takeChoiceTurn takes the writes' turn, waiting for a write under way at
// most until ctx ends.
func (b *Backend) takeChoiceTurn(ctx context.Context) bool {
	select {
	case b.choiceTurn <- struct{}{}:
		return true
	default:
	}
	b.choiceQueued()
	select {
	case b.choiceTurn <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

func (b *Backend) giveChoiceTurn() { <-b.choiceTurn }

// settleChoice ends p's pending, if p is pending, and wakes whatever waits
// for it, the first time it is called for p. A nil p, a write the state as
// read refused, has nothing pending.
func (b *Backend) settleChoice(p *pendingChoice) {
	if p == nil {
		return
	}
	p.once.Do(func() {
		b.pending.CompareAndSwap(p, nil)
		close(p.done)
	})
}

// oweProbe makes the silent probe due and owed, which the supervisor's next
// ask runs once the device slot is free (ProbeDSDPending, RequestProbeDSD), as
// after a choice written for the DAC (the design's section 5), and asks it
// anew (asked), so a run begun before the write that finds nothing to ask
// leaves it owed (settled). It drops none of the probe's answers and moves
// no generation: they are the kernel's and the descriptors', which a choice
// does not change, so a probe under way keeps what it asks, which is the
// probe owed (publishProbe); and with them dropped while a session holds the
// device, where no probe can run, each tuple's pause would fall back to
// silence and what the probe refused would be offered again, so a write that
// changes nothing that plays would change the set and interrupt what it
// promised to leave alone. A backend with no probe cache owes none.
func (b *Backend) oweProbe() {
	if p := b.cfg.DSD; p == nil || p.Probes == nil {
		return
	}
	ps := &b.probeState
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.asked++
	ps.forced, ps.pending = true, true
}

// pendingChoice is a choice write between its reading and its settle: the
// set it would publish, and done, closed once the write has published,
// failed or panicked (settleChoice, once).
type pendingChoice struct {
	set  *dsdSet
	done chan struct{}
	once sync.Once
}

// planAt is d's plan at bitRate: a set that could not be computed refuses
// every rate.
func (d *dsdSet) planAt(bitRate int) policy.Plan {
	if !d.computed {
		return policy.Plan{BitRate: bitRate, Outcome: policy.OutcomeRefused, Reason: ReasonDACUnread}
	}
	return d.set.At(bitRate).Plan
}

// changes reports whether p's set plays o's rate otherwise than cur, the set
// published, which is what o is admitted under, or would be, until the write
// publishes: o a DSD occurrence admitted under a set and not refused, or one
// a set's plan refused at a rate the policy decides (refusedUnder, B728 B9
// round 2), whose refusal the write may lift. The set published, never the
// one the write was made from: one that has published what the write would
// (a session's start reading the durable state before the write's own
// publish) changes nothing the write would. Two refusals play it alike,
// whatever their words.
func (p *pendingChoice) changes(cur *dsdSet, o *occ) bool {
	switch {
	case o.admitted != nil && o.refused == nil:
	case o.refusedUnder != nil:
	default:
		return false
	}
	a, c := cur.planAt(o.nativeFormat.BitRateHz), p.set.planAt(o.nativeFormat.BitRateHz)
	if !plays(a) && !plays(c) {
		return false
	}
	return !a.Same(c)
}

// pendingFor is the pending write o waits for, nil while no write pending
// changes its rate. s.mu is held.
func (b *Backend) pendingFor(o *occ) *pendingChoice {
	if p := b.pending.Load(); p != nil && p.changes(b.dsd.Load(), o) {
		return p
	}
	return nil
}

// unjudgedLocked reports whether o, a DSD occurrence admitted under a set,
// was admitted under one a publish has replaced since, which that publish's
// reconciliation has not judged yet (B728 B9 round 17). A choice's write
// publishes before it reconciles, as every refresh does, and is pending for
// o only until it publishes (pendingFor compares the set published), while
// the set published alone cannot say whether o is kept, handed off again or
// the session's DSD revoked: the reconciliation decides that in one step
// under s.mu (reconcileDSD), after which o is admitted under the set
// published or stale, the revocation's fault recorded where a sink plays the
// session's DSD, and Revoke, just after, drops what the revocation takes,
// each step closing s.judged. What would open o's own sink, join o to a sink
// or end a window before it waits for them (s.judged); one admitted under
// the set published now waits for nothing, and one still opening is its
// open's end's to admit again (readmitDSDLocked), never a reconciliation's.
// Every caller asks it of an occurrence no sink of its own holds, which every
// reconciliation judges once its open has ended. s.mu is held.
func (s *session) unjudgedLocked(o *occ) bool {
	return o.opened() && o.admitted != nil && o.admitted != s.b.dsd.Load() && !o.stale && !o.dropped
}

// settleOpen takes s.mu for the step that ends o's open, as lockSettled
// does, and asks again, its lock let go, an admission the effective policy
// refused under a set that is not the one published now (B728 B9 round 2,
// point 2): lifted by a choice's write, pending when the refusal was made
// (lockSettled waits for it) or published since, or by any change of the
// set. The content's own checks are made again with it (openNativeContent:
// its first chunk read, its conversion's plan), outside the lock, so an
// enabled rate's content that gives no bits is refused as that; a refusal
// that is the content's (its decoder's, a rate no transport carries) is
// never asked again. It returns with s.mu held.
func (s *session) settleOpen(ctx context.Context, o *occ) {
	for {
		s.lockSettled(ctx, o)
		u := o.refusedUnder
		if u == nil || u == s.b.dsd.Load() || ctx.Err() != nil || s.ctx.Err() != nil {
			return
		}
		s.mu.Unlock()
		s.b.reaskDSD(ctx, o)
	}
}

// reaskDSD asks o's admission again, its refusal by the effective policy
// forgotten, on its opener's goroutine before its open ends (settleOpen),
// with no lock held: the native open's admission and what follows it
// (openNativeContent, whose reservation is charged once).
func (b *Backend) reaskDSD(ctx context.Context, o *occ) {
	o.refused, o.refusedUnder, o.admitted, o.policyPlan, o.plan = nil, nil, nil, policy.Plan{}, nil
	b.openNativeContent(ctx, o, o.dsd)
}

// lockSettled takes s.mu for the step that ends o's open: at once while no
// write pending changes o's rate; otherwise, its lock let go, once the write
// has settled (or ctx or the session has ended), so the step admits o under
// what the write left published, and the write's classification never missed
// it (binding note 3). It returns with s.mu held.
func (s *session) lockSettled(ctx context.Context, o *occ) {
	for {
		s.mu.Lock()
		p := s.b.pendingFor(o)
		if p == nil {
			return
		}
		s.mu.Unlock()
		s.b.choiceAwaited(o.id)
		select {
		case <-p.done:
			s.b.choiceWoke(o.id)
		case <-ctx.Done():
			s.mu.Lock()
			return
		case <-s.ctx.Done():
			s.mu.Lock()
			return
		}
	}
}

// awaitWrite waits, its session's lock let go, while a choice's write
// pending plays o's rate otherwise than the set published (B728 B9 round 2,
// point 1): the activation of o's sink (checkDSDActivation) then judges o
// under what the write left published. The write settles at every end of
// it, so the wait is bounded by the write's (its state's fsync and its
// publish); it ends too with ctx or the session's life, which a release ends.
// It returns with s.mu not held.
func (s *session) awaitWrite(ctx context.Context, o *occ) {
	for {
		s.mu.Lock()
		p := s.b.pendingFor(o)
		s.mu.Unlock()
		if p == nil {
			return
		}
		s.b.choiceAwaited(o.id)
		select {
		case <-p.done:
			s.b.choiceWoke(o.id)
		case <-ctx.Done():
			return
		case <-s.ctx.Done():
			return
		}
	}
}

// writeInterrupts reports whether publishing next in cur's place would
// interrupt what plays (the design's sections 3.5 and 7.2, check 8): an
// occurrence of a session's that the classification reconciliation acts by
// (judgeDSDLocked) interrupts against next and does not against cur, the set
// the state gives without the write, so what a change of the box does anyway
// is no interruption of the write's. Each session is read under its lock.
func (b *Backend) writeInterrupts(cur, next *dsdSet) bool {
	b.mu.Lock()
	ss := make([]*session, 0, len(b.sessions))
	for _, s := range b.sessions {
		ss = append(ss, s)
	}
	b.mu.Unlock()
	for _, s := range ss {
		if s.writeInterrupts(cur, next) {
			return true
		}
	}
	return false
}

func (s *session) writeInterrupts(cur, next *dsdSet) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, replans := s.ev.(engine.ReplanEvents)
	for _, o := range s.chain {
		if s.judgeDSDLocked(o, next, replans).interrupts && !s.judgeDSDLocked(o, cur, replans).interrupts {
			return true
		}
	}
	return false
}

// The write's seams (Seams.ChoicePending and the rest).

func (b *Backend) choicePending() {
	if sm := b.cfg.Seams; sm != nil && sm.ChoicePending != nil {
		sm.ChoicePending()
	}
}

func (b *Backend) choiceWritten(err error) {
	if sm := b.cfg.Seams; sm != nil && sm.ChoiceWritten != nil {
		sm.ChoiceWritten(err)
	}
}

func (b *Backend) choiceQueued() {
	if sm := b.cfg.Seams; sm != nil && sm.ChoiceQueued != nil {
		sm.ChoiceQueued()
	}
}

func (b *Backend) choiceAwaited(o engine.OccurrenceID) {
	if sm := b.cfg.Seams; sm != nil && sm.ChoiceAwaited != nil {
		sm.ChoiceAwaited(o)
	}
}
