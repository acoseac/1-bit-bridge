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
// set would interrupt, the state's write and the publish. So a write that
// finds the revision moved answers stale only once the write that moved it
// has published, and the revision a stale answer carries is the one the
// next write compares with (binding note 2).
//
// The write between its reading and its publish is pending (Backend.pending):
// the set it would publish, stored before what it would interrupt is
// classified. Every step that ends a DSD occurrence's open reads it under the
// session's lock (lockSettled), and one whose rate the pending set plays
// otherwise than the set published waits for the write to settle, its lock
// let go; the producer holds back from a sink an opened occurrence at such a
// rate (next, transition, holdLocked). So each occurrence the write could
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
// and its closure is pure, so no fsync is ever waited for under a lock an
// occurrence's admission takes. Playback calls Store.Update only on goroutines
// of its own (recordDSD), under no lock.

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
// /v1/outputs reads them afterwards (Policy).
type ChoiceResult struct {
	Outcome ChoiceOutcome
	Policy  PlayerPolicy
}

// maxChoiceRounds bounds the rounds a write takes when another writer moves
// what it would publish between its reading and its write (errChoiceRound):
// past it the write answers stale, and the client writes again.
const maxChoiceRounds = 8

// errChoiceRound is a write's closure finding the state moved since the
// write's reading in a way that changes what the write would publish, or
// what the state gives without it: nothing is written, and the write reads
// again.
var errChoiceRound = errors.New("player backend: what the choice's write would publish moved since it was read")

// ChooseDSD writes the bound DAC's DSD choice (the design's section 7.2), and
// answers how it ended with the epoch published then. Its checks, each
// answered with its word, nothing written and nothing published: the words
// of the choice (bad_request); a bound DAC read fresh (dac_unavailable);
// req.DACKey its key as the read side publishes it (wireKey: dac_changed);
// the policy's, inside the state's one write, on the state that write is made
// from (policy.Choose: stale, mode_not_offered, dop_confirmation_required,
// first_use_unacknowledged); and, without req.Interrupt, nothing the set the
// write would publish interrupts (writeInterrupts: would_interrupt). Then the
// state is written, durably (persist_failed before it lands, nothing changed;
// persist_uncertain once it lands and its sync fails, or where an earlier
// landed write fences it; state_unreadable where the file is refused in
// memory), a probe is made due (ProbeDSDPending), the set the state gives is
// published and reconciled (refreshDSD), and the write is answered from that
// epoch. A stale answer reads the policy again first, so the revision it
// carries is the state's. ctx bounds the wait for another write's turn alone:
// once the state's write begins nothing interrupts it.
func (b *Backend) ChooseDSD(ctx context.Context, req ChoiceRequest) ChoiceResult {
	j := choiceJournal{}
	out := b.chooseDSD(ctx, req, &j)
	attrs := []any{"dac", j.dac, "from", j.from, "to", j.to, "outcome", string(out)}
	if out == ChoicePersistFailed || out == ChoicePersistUncertain || out == ChoiceStateUnreadable {
		b.dsdLog().Warn("player: a write of the bound DAC's DSD choice was not made durable", attrs...)
	} else {
		b.dsdLog().Info("player: a write of the bound DAC's DSD choice", attrs...)
	}
	return ChoiceResult{Outcome: out, Policy: b.Policy()}
}

// choiceJournal is what a write's line says: the bound DAC's key as the wire
// names it, the mode that applied before the write and the one it asks; each
// "" where the write did not come to it.
type choiceJournal struct{ dac, from, to string }

func (b *Backend) chooseDSD(ctx context.Context, req ChoiceRequest, j *choiceJournal) ChoiceOutcome {
	if err := req.Choice.Check(); err != nil {
		return ChoiceBadRequest
	}
	j.to = req.Choice.Mode
	p := b.cfg.DSD
	if p == nil {
		return ChoiceDACUnavailable
	}
	if !b.takeChoiceTurn(ctx) {
		return ChoicePersistFailed
	}
	defer b.giveChoiceTurn()
	// The round's pending set, settled at every end of the write, before its
	// turn is given back: a write that panics (the API's recoverer answers
	// it) leaves nothing pending, so no occurrence waits for a write that will
	// not publish, and what it wrote, if it wrote, the next refresh publishes.
	var pend *pendingChoice
	defer func() { b.settleChoice(pend) }()
	for round := 0; ; round++ {
		r, _ := b.readBox()
		if r == nil {
			return ChoiceDACUnavailable
		}
		j.dac = wireKey(r.key, r.m.USB)
		if j.dac != req.DACKey {
			return ChoiceDACChanged
		}
		if p.Store == nil {
			return ChoiceStateUnreadable
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
		if next, err := policy.Choose(r.inputs(st), req.Choice, now); err == nil {
			cur, _ = b.setOver(r, st)
			want, _ = b.setOver(r, next)
			pend = &pendingChoice{set: want, done: make(chan struct{})}
			b.pending.Store(pend)
			interrupts := b.writeInterrupts(cur, want)
			b.choicePending()
			if interrupts && !req.Interrupt {
				b.settleChoice(pend)
				return ChoiceWouldInterrupt
			}
		}
		var wrote dsdstate.State
		err := p.Store.Update(func(s *dsdstate.State) error {
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
			*s, wrote = next, next.Clone()
			return nil
		})
		b.choiceWritten(err)
		var refusal *policy.ChoiceRefusal
		switch {
		case errors.Is(err, errChoiceRound):
			b.settleChoice(pend)
			if round+1 < maxChoiceRounds {
				continue
			}
			b.refreshDSD()
			return ChoiceStale
		case errors.As(err, &refusal):
			b.settleChoice(pend)
			if refusal.Reason == policy.ChoiceStale {
				b.refreshDSD()
			}
			if out, ok := refusalOutcomes[refusal.Reason]; ok {
				return out
			}
			return ChoiceBadRequest
		case errors.Is(err, dsdstate.ErrNotWritable):
			b.settleChoice(pend)
			return ChoiceStateUnreadable
		case errors.Is(err, dsdstate.ErrLandedNotHeld):
			b.settleChoice(pend)
			return ChoicePersistUncertain
		case errors.Is(err, atomicwrite.ErrLanded):
			// The file holds the write's state, not known to be durable, and the
			// store the state as it was: until the next start reads the file, a
			// rate plays only where the two play it alike.
			b.landedChoice.Store(&wrote)
			b.refreshDSD()
			b.settleChoice(pend)
			return ChoicePersistUncertain
		case err != nil:
			b.settleChoice(pend)
			return ChoicePersistFailed
		}
		b.oweProbe()
		b.refreshDSD()
		b.settleChoice(pend)
		return ChoiceWritten
	}
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

// oweProbe makes the silent probe due, which the supervisor's next ask runs
// once the device slot is free (ProbeDSDPending, ProbeDSD), as after a choice
// written for the DAC (the design's section 5). It drops none of the probe's
// answers: they are the kernel's and the descriptors', which a choice does
// not change, and with them dropped while a session holds the device, where
// no probe can run, each tuple's pause would fall back to silence and what
// the probe refused would be offered again, so a write that changes nothing
// that plays would change the set and interrupt what it promised to leave
// alone. A backend with no probe cache owes none.
func (b *Backend) oweProbe() {
	if p := b.cfg.DSD; p == nil || p.Probes == nil {
		return
	}
	b.probeState.mu.Lock()
	b.probeState.forced = true
	b.probeState.mu.Unlock()
}

// ProbeDSDPending reports whether a probe is owed that only the supervisor's
// next ask makes (ProbeDSD): one a choice written for the DAC, or a
// migration's part for it (ForgetDSDProbe), made due, and that has not run
// since. Serve's supervisor and output play's watch ask the probe at each
// policy tick while it is.
func (b *Backend) ProbeDSDPending() bool {
	b.probeState.mu.Lock()
	defer b.probeState.mu.Unlock()
	return b.probeState.forced
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
// published: o a DSD occurrence (admitted under a set) not refused. Two
// refusals play it alike, whatever their words.
func (p *pendingChoice) changes(cur *dsdSet, o *occ) bool {
	if o.admitted == nil || o.refused != nil {
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
