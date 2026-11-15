package playback

import (
	"context"
	"errors"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/policy"
)

// A write of the bound DAC's DSD choice (B728 B9; the design's section 7.2),
// made only while the player is stopped: "Stop playback to change DSD" (the
// owner, 2026-11-10). A write never runs beside what plays, so it asks nothing
// of a session: no set is pending, no occurrence waits for a write, and none is
// judged against what a write would publish. B9 PR 2 rebuilt it so from #1434,
// which wrote beside playback and was narrowed after nineteen review rounds.
//
// One write at a time, end to end (the writes' turn, choiceTurn): its lease,
// its reading of the DAC, the state's one write, the publish, and the epoch its
// answer carries, read before the lease and the turn are let go. So a write
// that finds the revision moved answers stale only once the write that moved
// it has published, and the revision a stale answer carries is the one the
// next write compares with.
//
// The configuration lease (takeLease, Backend.lease) is the backend's one
// admission point beside it: a write takes it, after the turn, only in the
// step under the backend's lock that finds no session's record (a session
// starting, playing, paused with the device open or let go, releasing, or a
// retiring native tail), nothing holding the device and no release left
// unconfirmed, and holds it until its answer is read; meanwhile a Start waits
// before it registers (admitStart) and a probe is refused (reserveProbe),
// and a probe that held the device slot as the lease was taken is waited for.
//
// Lock order: the writes' turn, then the refreshes' (dsdMu), then the
// backend's (b.mu), then each session's. The state's store is taken alone: no
// lock of the backend's is held across its Update, whose fsync a stalled disk
// holds as long as it likes, and its closure is pure. The probe's lock, which a
// written choice takes to owe its probe (oweProbe), is taken under none of
// these.

// ChoiceOutcome is how a write of the bound DAC's DSD choice ended (B728 B9;
// the design's section 7.2): written, or the word it was refused with.
type ChoiceOutcome string

const (
	ChoiceWritten                ChoiceOutcome = "written"
	ChoiceBadRequest             ChoiceOutcome = "bad_request"
	ChoicePlaybackNotStopped     ChoiceOutcome = "playback_not_stopped"
	ChoiceDACUnavailable         ChoiceOutcome = "dac_unavailable"
	ChoiceDACChanged             ChoiceOutcome = "dac_changed"
	ChoiceStale                  ChoiceOutcome = "stale"
	ChoiceModeNotOffered         ChoiceOutcome = "mode_not_offered"
	ChoiceConfirmationRequired   ChoiceOutcome = "dop_confirmation_required"
	ChoiceFirstUseUnacknowledged ChoiceOutcome = "first_use_unacknowledged"
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
// GET /v1/outputs publishes it, and the choice.
type ChoiceRequest struct {
	DACKey string
	Choice policy.Choice
}

// ChoiceResult is a write's outcome and the epoch published once it ended:
// the set and its read side a written choice published, read as GET
// /v1/outputs reads them afterwards (Policy), before the write's lease and
// turn are let go.
type ChoiceResult struct {
	Outcome ChoiceOutcome
	Policy  PlayerPolicy
}

// ChooseDSD writes the bound DAC's DSD choice (the design's section 7.2), only
// while the player is stopped, and answers how it ended with the epoch
// published then. Its checks, in this order, each answered with its word,
// nothing written and nothing published: the words of the choice
// (bad_request); the writes' turn, waited for at most until ctx ends
// (persist_failed); the writes of the player's DSD state closed by the
// player's stop (CloseDSDWrites: persist_failed); the player stopped, through
// the configuration lease (takeLease: playback_not_stopped, at once), a probe
// that holds the device slot waited for within ctx (persist_failed); a backend
// with no DSD policy (Config.DSD nil: state_unreadable, before the DAC is
// read, which reads that policy's box); a bound DAC read fresh
// (dac_unavailable); req.DACKey its key as the read side publishes it
// (wireKey: dac_changed); a state that did not open (state_unreadable); no
// earlier write landed and not known to be durable (choiceFenced:
// persist_uncertain, a write of any writer's, an evidence write's included,
// answered before the state's checks); then the policy's, inside the state's
// one write, on the state that write is made from (policy.Choose: stale,
// mode_not_offered, dop_confirmation_required, first_use_unacknowledged), a
// stale answer publishing the state's revision first. Then the state is
// written durably (persist_failed before it lands, nothing changed;
// persist_uncertain once it lands and its sync fails, its state recorded
// (landedChoice) and the set published held to what the two states play alike
// until the next start; state_unreadable where the file is refused in
// memory), a probe is made due and owed (oweProbe), the set the state gives is
// published (refreshDSD), and the write is answered from that epoch.
//
// The lease is the backend's half of "stopped" alone: between an acknowledged
// Release and the Start that follows it (a seek, Next or Previous, a skip after
// a refusal, a Play while playing) the backend holds no session's record, and
// a write would be taken mid-transition. Its caller must therefore hold the
// engine's own configuration reservation, granted by the engine's owner in the
// step that finds the player stopped and held across this call, every
// transport command that would start a session refused or held meanwhile, and
// the write's persistence made off the owner's goroutine (B728 B9 PR 3 builds
// it, with the route). ctx bounds the waits for the turn and for a probe:
// once the state's write begins nothing interrupts it.
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
// its fence, the writes' close or a wait refused was not made.
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
	if !b.takeChoiceTurn(ctx) {
		j.why = "it could not begin within the request's time: another write held the turn"
		return b.answerNow(ChoicePersistFailed)
	}
	defer b.giveChoiceTurn()
	// A write of the player's DSD state (dsdWrite) from before the lease to
	// its publish: the player's stop waits for it, and once that stop has
	// closed the writes none begins.
	if !b.dsdWrite("a choice's write", func() { res = b.writeChoice(ctx, req, j) }) {
		j.why = "the player's stop closed the writes of its DSD state"
		return ChoiceResult{Outcome: ChoicePersistFailed, Policy: b.Policy()}
	}
	return res
}

// writeChoice is the write inside its turn: the lease, the reading, the
// state's one write and the publish (ChooseDSD). The lease is let go on every
// way out, a panic's included, after the answer's epoch is read.
func (b *Backend) writeChoice(ctx context.Context, req ChoiceRequest, j *choiceJournal) (res ChoiceResult) {
	l, refused := b.takeLease(ctx, j)
	if l == nil {
		return ChoiceResult{Outcome: refused, Policy: b.Policy()}
	}
	defer b.letLease(l)
	// The epoch the answer carries, read at every return before the lease and
	// the turn are let go: read after them, the next write could have
	// published, and a stale answer carry a revision that write moved past.
	defer func() { res.Policy = b.Policy() }()
	p := b.cfg.DSD
	if p == nil {
		j.why = "the player has no DSD policy"
		return ChoiceResult{Outcome: ChoiceStateUnreadable}
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
		// before any of the state's checks, which a write that cannot be made
		// does not make.
		j.why = "an earlier write reached the state's file and is not known to be durable"
		return ChoiceResult{Outcome: ChoicePersistUncertain}
	}
	if st := p.Store.State(); st.Unreadable() {
		j.from = dsdstate.ModeOff
	} else {
		j.from, _, _ = st.ChoiceFor(r.key)
	}
	now := b.dsdNow()
	var wrote dsdstate.State
	err := p.Store.Update(func(s *dsdstate.State) error {
		next, err := policy.Choose(r.inputs(*s), req.Choice, now)
		if err != nil {
			return err
		}
		*s, wrote = next, next.Clone()
		return nil
	})
	b.choiceWritten(err)
	var refusal *policy.ChoiceRefusal
	switch {
	case errors.As(err, &refusal):
		if refusal.Reason == policy.ChoiceStale {
			// Another writer moved the revision (the migration's part for a
			// DAC, which writes a choice): published first, so the answer
			// carries the revision the next write compares with.
			_, _ = b.refreshDSD("a stale choice's write")
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
		// rate plays only where the two play it alike (setOver). Nothing is
		// rolled back. A crash between the rename and the directory's sync may
		// leave either choice in the file.
		b.landedChoice.Store(&wrote)
		_, _ = b.refreshDSD("a landed choice's write")
		return ChoiceResult{Outcome: ChoicePersistUncertain}
	case err != nil:
		j.why = "the state's write failed before it replaced the file"
		return ChoiceResult{Outcome: ChoicePersistFailed}
	}
	b.oweProbe()
	_, _ = b.refreshDSD("a choice's write")
	return ChoiceResult{Outcome: ChoiceWritten}
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

// configLease is the configuration lease a choice's write holds (takeLease):
// free is closed once it is let go.
type configLease struct{ free chan struct{} }

// takeLease takes the configuration lease for a choice's write, in one step
// under the backend's lock, the lock a Start registers its session under
// (admitStart) and a probe takes the device slot under (reserveProbe), and
// only while no session has a record (one starting, its Start between its
// registration and its hold of the device included; playing; paused, the
// device open or let go by the pause's release; releasing, a retiring native
// tail included; or a Start whose Release came first), nothing holds the
// device and no release was left unconfirmed: otherwise it answers
// playback_not_stopped at once, nothing taken. From then on no Start
// registers and no probe takes the slot. A probe that held the slot as the
// lease was taken is waited for, the lease held, at most until ctx ends:
// then the lease is let go, persist_failed. The lease is its caller's only
// once this returns it: let go here on every other way out, a panic's
// included.
func (b *Backend) takeLease(ctx context.Context, j *choiceJournal) (*configLease, ChoiceOutcome) {
	b.mu.Lock()
	if len(b.sessions) > 0 || b.holder != nil || b.terminalUnconfirmed {
		b.mu.Unlock()
		return nil, ChoicePlaybackNotStopped
	}
	l := &configLease{free: make(chan struct{})}
	b.lease = l
	p := b.probing
	b.mu.Unlock()
	handed := false
	defer func() {
		if !handed {
			b.letLease(l)
		}
	}()
	b.choiceLeased()
	if p != nil {
		select {
		case <-p.done:
		case <-ctx.Done():
			j.why = "the silent probe held the DAC past the request's time"
			return nil, ChoicePersistFailed
		}
	}
	handed = true
	return l, ""
}

// letLease lets l go: a Start that waits for it registers, a probe takes the
// slot again.
func (b *Backend) letLease(l *configLease) {
	b.mu.Lock()
	if b.lease == l {
		b.lease = nil
	}
	b.mu.Unlock()
	close(l.free)
}

// oweProbe makes the silent probe due and owed, which the supervisor's next
// ask runs once the device slot is free (ProbeDSDPending, RequestProbeDSD), as
// after a choice written for the DAC (the design's section 5), and asks it
// anew (asked), so a run begun before the write, one that finds nothing to ask
// or one that publishes after the write in the window between its slot's end
// and its answers (publishProbe), never settles the probe the write owes. It
// drops none of the probe's answers and moves no generation: they are the
// kernel's and the descriptors', which a choice does not change. A backend
// with no probe cache owes none.
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

// The write's seams (Seams.ChoiceLeased and the rest).

func (b *Backend) choiceLeased() {
	if sm := b.cfg.Seams; sm != nil && sm.ChoiceLeased != nil {
		sm.ChoiceLeased()
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
