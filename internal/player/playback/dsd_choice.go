package playback

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
	"time"

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
// its reading of the DAC, the state's one write, the publication of that
// reading, and the epoch its answer carries, captured from that publication
// before the lease and the turn are let go. So a write that finds the revision
// moved answers stale only once the write that moved it has published, and the
// revision a stale answer carries is the one the next write compares with.
//
// The configuration lease (takeLease, Backend.lease) is the backend's one
// admission point beside it: a write takes it, after the turn, only in the
// step under the backend's lock that finds no session's record (a session
// starting, playing, paused with the device open or let go, releasing, or a
// retiring native tail), nothing holding the device and no release left
// unconfirmed, and holds it until its answer is captured; meanwhile a Start
// waits before it registers (admitStart) and a probe is refused (reserveProbe),
// and a probe that held the device slot as the lease was taken is waited for.
//
// The write's reading and its publication (B728 B9 PR 3): the write reads the
// DAC and the state under the refreshes' lock (dsdMu), as a refresh reads
// them (takeChoiceReading), and from that reading until it has published it no
// refresh reads or publishes: one asked meanwhile is owed (refreshOwed) and
// asked once the write has published. Every outcome after the reading
// publishes it, through the refreshes' own step (publishLocked), from the state
// the write's decision read, and the answer is the pair that publication
// stored, captured under the same hold of dsdMu (publishChoice).
//
// Lock order: the writes' turn, then the refreshes' (dsdMu, which the write's
// reading and its publication take), then the backend's (b.mu), then each
// session's. The state's store is taken alone: no lock of the backend's is
// held across its Update, whose fsync a stalled disk holds as long as it
// likes, and its closure is pure. The probe's lock, which a written choice
// takes to owe its probe (oweProbe), is taken under none of these.

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
	// ChoiceCheckNotAnswerable is a listener's answer to a DSD check that
	// cannot be taken (AnswerCheck, B728 B12 PR 4).
	ChoiceCheckNotAnswerable ChoiceOutcome = "check_not_answerable"
)

// refusalOutcomes are the policy's refusals by the outcome each is.
var refusalOutcomes = map[policy.ChoiceReason]ChoiceOutcome{
	policy.ChoiceStale:                  ChoiceStale,
	policy.ChoiceModeNotOffered:         ChoiceModeNotOffered,
	policy.ChoiceConfirmationRequired:   ChoiceConfirmationRequired,
	policy.ChoiceFirstUseUnacknowledged: ChoiceFirstUseUnacknowledged,
}

// DACToken names the DAC a write's read side was computed for, as this boot
// of the backend names it (B728 B9 PR 3; GET /v1/outputs' dsdDAC.dacToken):
// "t1-" and 32 lowercase hexadecimal digits, the first 16 bytes of an
// HMAC-SHA-256, under a key the backend draws as it starts (tokenKey), of the
// reading's model key, its maker's stamp and its firmware (dacToken). It tells
// a client nothing of the unit's strings, a serial among them, and holds for
// one boot of the backend.
type DACToken string

// dacTokenDomain is what a token's MAC is taken under, ahead of its inputs,
// each after a NUL.
const dacTokenDomain = "onebit:dsd:dac-token:v1"

// Valid reports whether t has a token's form: "t1-" and 32 lowercase
// hexadecimal digits.
func (t DACToken) Valid() bool {
	digits, ok := strings.CutPrefix(string(t), "t1-")
	if !ok || len(digits) != 32 {
		return false
	}
	for i := range len(digits) {
		if c := digits[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// dacToken is the token of the unit id names, and the one function that makes
// one (readBox mints it once a reading): the MAC, under the backend's key, of
// the domain, the model key's canonical form, the maker's stamp and the
// firmware in the state's form (canonicalBCD: "" for one that does not read,
// alike on every side), each after a NUL, which none of them holds.
func (b *Backend) dacToken(id policy.Identity) DACToken {
	m := hmac.New(sha256.New, b.tokenKey[:])
	for i, part := range []string{dacTokenDomain, id.Key.Canonical(), string(id.Stamp), canonicalBCD(id.BCDDevice)} {
		if i > 0 {
			m.Write([]byte{0})
		}
		m.Write([]byte(part))
	}
	return DACToken("t1-" + hex.EncodeToString(m.Sum(nil)[:16]))
}

// randomTokenKey is a token key, 32 random bytes; a read that fails panics, as
// randomBoot's does.
func randomTokenKey() (k [32]byte) {
	if _, err := rand.Read(k[:]); err != nil {
		panic(err)
	}
	return k
}

// ChoiceRequest is a write of the bound DAC's DSD choice: the DAC's key and
// token as GET /v1/outputs published them with the read side the write was
// made from (dsdDAC.dacKey and dsdDAC.dacToken), and the choice.
type ChoiceRequest struct {
	DACKey string
	// DACToken is the token of the DAC that read side was computed for: one
	// that is not the token of the write's own reading (another model, maker or
	// firmware, another unit of the key the wire names, or another boot of the
	// backend) is dac_changed, and nothing is written.
	DACToken DACToken
	Choice   policy.Choice
}

// Check refuses a request that is no write at all: the choice's words
// (policy.Choice.Check), or a token not of a token's form. It reads nothing.
func (r ChoiceRequest) Check() error {
	if err := r.Choice.Check(); err != nil {
		return err
	}
	if !r.DACToken.Valid() {
		return errors.New("player backend: the write's DAC token is not of a token's form")
	}
	return nil
}

// ChoiceResult is how a write ended: its outcome, the epoch its answer carries
// and the cause, where one occurred.
type ChoiceResult struct {
	Outcome ChoiceOutcome
	// Policy is the epoch the answer carries while Epoch holds: for an outcome
	// decided before the write read the DAC, the pair published as it
	// answered; for one decided after, the pair the write published from that
	// reading, captured as it was stored.
	Policy PlayerPolicy
	// Epoch reports whether Policy is such an epoch: never for bad_request,
	// which is no write, nor where the write's publication panicked before it
	// stored its pair.
	Epoch bool
	// Err is the cause of an outcome that has one: the context's end at the
	// turn's wait, the refreshes' lock, the reading or the probe's wait;
	// ErrDSDWritesClosed; the store's error; the fence of a write that landed;
	// a state that did not open. Nil for written and every refusal's word.
	Err error
}

// ErrDSDWritesClosed is the cause of a write asked once the player's stop
// closed the writes of its DSD state (CloseDSDWrites): persist_failed.
var ErrDSDWritesClosed = errors.New("player backend: the player's stop closed the writes of its DSD state")

// The causes of a write refused for its state or for an earlier write.
var (
	errStateNotOpened = errors.New("player backend: the player's DSD state did not open")
	errChoiceFenced   = errors.New("player backend: an earlier write reached the state's file and is not known to be durable")
)

// ChooseDSD writes the bound DAC's DSD choice (the design's section 7.2), only
// while the player is stopped, and answers how it ended with the epoch its
// answer carries (ChoiceResult). Its checks, in this order, each answered with
// its word, nothing written: the request's words (Check: the choice's, and a
// token of a token's form: bad_request, before anything else); the writes'
// turn, waited for at most until ctx ends (persist_failed); the writes of the
// player's DSD state closed by the player's stop (CloseDSDWrites:
// persist_failed); the player stopped, through the configuration lease
// (takeLease: playback_not_stopped, at once), a probe that holds the device
// slot waited for within ctx (persist_failed); a backend with no DSD policy
// (Config.DSD nil: state_unreadable, before the DAC is read, which reads that
// policy's box); the refreshes' lock, which a refresh holds across its reading
// of the box, waited for and ctx asked once it is held (persist_failed, nothing
// read); the DAC and the state read under it (takeChoiceReading), and ctx asked
// again (persist_failed); a bound DAC read fresh (dac_unavailable); req.DACKey
// its key as the read side publishes it (wireKey: dac_changed), and
// req.DACToken the token of that reading (dacToken: dac_changed, another model,
// maker or firmware, another unit of the wire's key, or another boot's); a
// state that did not open (state_unreadable); no earlier write landed and not
// known to be durable (choiceFenced: persist_uncertain, a write of any
// writer's, an evidence write's included, answered before the state's checks);
// then the policy's, inside the state's one write, on the state that write is
// made from (policy.Choose: stale, mode_not_offered,
// dop_confirmation_required). Then the state is written durably (persist_failed
// before it lands, nothing changed; persist_uncertain once it lands and its
// sync fails, its state recorded (landedChoice) and the set published held to
// what the two states play alike until the next start; state_unreadable where
// the file is refused in memory) and a probe is made due and owed (oweProbe).
// Each check after the DAC's reading publishes that reading first, from the
// state the write's decision read (publishChoice), and every answer carries the
// pair published as it answered: an outcome before the reading the pair
// standing, one after it the pair the write published; bad_request none.
//
// The lease is the backend's half of "stopped" alone: between an acknowledged
// Release and the Start that follows it (a seek, Next or Previous, PlayID or
// PlayIndex while playing, a skip after a refusal, a Play during a release to
// play) the backend holds no session's record, and a write would be taken
// mid-transition. Its caller holds the engine's configuration reservation
// (engine.ReserveConfiguration) across this call, granted by the engine's
// owner in the step that finds the player stopped, every command that would
// start a session refused meanwhile, and lets it go once this call has
// returned. ctx bounds the waits for the turn, the refreshes' lock and a probe:
// the box's reading and the state's write, once begun, are not interrupted.
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

// answerNow is an answer made before the write read the DAC: outcome o, its
// cause err, and the epoch published now.
func (b *Backend) answerNow(o ChoiceOutcome, err error) ChoiceResult {
	return ChoiceResult{Outcome: o, Policy: b.Policy(), Epoch: true, Err: err}
}

func (b *Backend) chooseDSD(ctx context.Context, req ChoiceRequest, j *choiceJournal) ChoiceResult {
	if req.Check() != nil {
		return ChoiceResult{Outcome: ChoiceBadRequest}
	}
	j.to = req.Choice.Mode
	return b.writeState(ctx, stateWrite{what: "a choice's write", key: req.DACKey, token: req.DACToken,
		apply: func(s dsdstate.State, r *dsdReading, now time.Time) (dsdstate.State, error) {
			return policy.Choose(r.inputs(s), req.Choice, now)
		},
		wrote: func(durable bool) {
			if durable {
				b.oweProbe()
			}
		}}, j)
}

// stateWrite is one write of the player's DSD state by a choice's path (B728
// B9; a listener's answer to the DSD check, B12 PR 4): what it is, the DAC it
// is for, its second decision past the fence ("" admits; nil for none), its
// change of the state inside the store's one write, and what follows a write
// that reached the file, durable or not.
type stateWrite struct {
	what  string
	key   string
	token DACToken
	admit func(r *dsdReading) ChoiceOutcome
	apply func(s dsdstate.State, r *dsdReading, now time.Time) (dsdstate.State, error)
	wrote func(durable bool)
}

// writeState is w in the writes' turn, waited for at most until ctx ends
// (persist_failed), as a write of the player's DSD state (dsdWrite) from
// before the lease to its publication: the player's stop waits for it, and
// once that stop has closed the writes none begins (persist_failed).
func (b *Backend) writeState(ctx context.Context, w stateWrite, j *choiceJournal) (res ChoiceResult) {
	if !b.takeChoiceTurn(ctx) {
		j.why = "it could not begin within the request's time: another write held the turn"
		return b.answerNow(ChoicePersistFailed, ctx.Err())
	}
	defer b.giveChoiceTurn()
	if !b.dsdWrite(w.what, func() { res = b.writeChoice(ctx, w, j) }) {
		j.why = "the player's stop closed the writes of its DSD state"
		return b.answerNow(ChoicePersistFailed, ErrDSDWritesClosed)
	}
	return res
}

// writeChoice is the write inside its turn: the lease, the reading, the
// state's one write, the publication and the answer (ChooseDSD). The reading's
// hold ends on every way out (endChoiceReading), and the lease is let go after
// it, a panic's way included, once the answer is captured.
func (b *Backend) writeChoice(ctx context.Context, w stateWrite, j *choiceJournal) ChoiceResult {
	l, refused, err := b.takeLease(ctx, j)
	if l == nil {
		return b.answerNow(refused, err)
	}
	defer b.letLease(l)
	p := b.cfg.DSD
	if p == nil {
		j.why = "the player has no DSD policy"
		return b.answerNow(ChoiceStateUnreadable, nil)
	}
	rd, err := b.takeChoiceReading(ctx)
	if err != nil {
		j.why = "it could not read the DAC within the request's time: a reading of the box held it"
		return b.answerNow(ChoicePersistFailed, err)
	}
	defer b.endChoiceReading(ctx)
	res := b.publishChoice(ctx, rd, b.decideChoice(ctx, p, w, rd, j))
	b.choiceAnswered()
	return res
}

// choiceRead is what a write read under the refreshes' lock
// (takeChoiceReading): when, the bound DAC (nil where it did not read, why
// saying why) and the player's DSD state, as a refresh reads them.
type choiceRead struct {
	at  time.Time
	r   *dsdReading
	why string
	st  dsdstate.State
}

// takeChoiceReading reads the DAC and the state for a write as a refresh reads
// them (readDSD), under the refreshes' lock, which a refresh under way holds
// across its reading of the box: the write waits for it at most until ctx
// ends (lockReading), and once it holds the lock asks ctx, answering its
// end with nothing read. From its reading on, the
// write holds it (choiceReading): no refresh reads or publishes until the
// write has published it (publishChoice), and one asked meanwhile is owed
// (refreshOwed). A panic in the reading lets the lock go, nothing held.
func (b *Backend) takeChoiceReading(ctx context.Context) (rd choiceRead, err error) {
	if !b.lockReading(ctx) {
		return choiceRead{}, ctx.Err()
	}
	defer b.dsdMu.Unlock()
	if err := ctx.Err(); err != nil {
		return choiceRead{}, err
	}
	rd.at = b.dsdNow()
	rd.st = b.dsdState()
	rd.r, rd.why = b.readBox()
	b.mu.Lock()
	b.choiceReading = true
	b.mu.Unlock()
	return rd, nil
}

// lockReading takes the refreshes' lock for a write's reading within ctx: a
// waiter on a goroutine of the backend's (spawn) takes it, and one
// compare-and-swap decides who owns it: the write, which reads on its own
// goroutine, or, its wait given up, the waiter, which lets it go having read
// nothing.
func (b *Backend) lockReading(ctx context.Context) bool {
	if b.dsdMu.TryLock() {
		return true
	}
	var owner atomic.Int32 // 1 the write's, 2 the waiter's
	got := make(chan struct{})
	b.spawn(func() {
		if b.dsdMu.Lock(); !owner.CompareAndSwap(0, 1) {
			b.dsdMu.Unlock()
		} else {
			close(got)
		}
	})
	select {
	case <-got:
	case <-ctx.Done():
		if owner.CompareAndSwap(0, 2) {
			return false
		}
		<-got
	}
	return true
}

// endChoiceReading ends a write's hold of its reading where its publication
// did not (a panic on the way to it), and asks a refresh owed meanwhile of the
// refreshes' goroutine, a DSD check's transition's included (checkOwed). Its
// lock is taken whatever ctx says: only the first wait follows the request
// (lockReading), and the reading's hold keeps the box unread under the lock
// meanwhile.
func (b *Backend) endChoiceReading(ctx context.Context) {
	b.dsdMu.Lock()
	b.mu.Lock()
	held, owed := b.choiceReading, b.refreshOwed || b.checkOwed
	if held {
		b.choiceReading, b.refreshOwed, b.checkOwed = false, false, false
	}
	b.mu.Unlock()
	b.dsdMu.Unlock()
	if held && owed {
		b.askChoiceRefresh(ctx)
	}
}

// choiceDecision is how a write ended once it had read the DAC: its outcome,
// its cause, and the state its decision read, which its publication is made
// from: the state read with the DAC, the state the store's write was made
// from where that write ran its closure, or the state it wrote.
type choiceDecision struct {
	outcome ChoiceOutcome
	err     error
	st      dsdstate.State
}

// decideChoice makes a write's checks after its reading (ChooseDSD), w's own
// past the fence (stateWrite.admit), and the state's one write (w.apply).
func (b *Backend) decideChoice(ctx context.Context, p *DSDPolicy, w stateWrite, rd choiceRead, j *choiceJournal) choiceDecision {
	d := choiceDecision{st: rd.st}
	if err := ctx.Err(); err != nil {
		// The box's reading outlived the request's time: nothing is written,
		// and what was read is published all the same.
		j.why = "the request's time ran out while the DAC was read"
		d.outcome, d.err = ChoicePersistFailed, err
		return d
	}
	r := rd.r
	if r == nil {
		d.outcome = ChoiceDACUnavailable
		return d
	}
	j.dac = wireKey(r.key, r.m.USB)
	if j.dac != w.key || !hmac.Equal([]byte(w.token), []byte(r.token)) {
		// Another DAC model, or a unit whose read side the write was not made
		// from (another maker or firmware, another unit of the wire's key, or
		// another boot's token): answered before anything is read of the
		// state's entry, its fence or its revision.
		d.outcome = ChoiceDACChanged
		return d
	}
	if p.Store == nil {
		j.why = "the player's DSD state did not open"
		d.outcome, d.err = ChoiceStateUnreadable, errStateNotOpened
		return d
	}
	if b.choiceFenced() {
		// No write is made until the next start reads the file: answered
		// before any of the state's checks, which a write that cannot be made
		// does not make.
		j.why = "an earlier write reached the state's file and is not known to be durable"
		d.outcome, d.err = ChoicePersistUncertain, errChoiceFenced
		return d
	}
	if w.admit != nil {
		if o := w.admit(r); o != "" {
			d.outcome = o
			return d
		}
	}
	if rd.st.Unreadable() {
		j.from = dsdstate.ModeOff
	} else {
		j.from, _, _ = rd.st.ChoiceFor(r.key, r.stamp)
	}
	now := b.dsdNow()
	var wrote, decided dsdstate.State
	ran := false
	err := p.Store.Update(func(s *dsdstate.State) error {
		decided, ran = s.Clone(), true
		next, err := w.apply(*s, r, now)
		if err != nil {
			return err
		}
		*s, wrote = next, next.Clone()
		return nil
	})
	b.choiceWritten(err)
	if ran {
		// The state the policy's checks read, which another writer may have
		// moved since the reading (the migration's part for a DAC, which writes
		// a choice): a stale answer then publishes the revision the next write
		// compares with.
		d.st = decided
	}
	var refusal *policy.ChoiceRefusal
	switch {
	case errors.As(err, &refusal):
		if out, ok := refusalOutcomes[refusal.Reason]; ok {
			d.outcome = out
			return d
		}
		d.outcome = ChoiceBadRequest
	case errors.Is(err, errCheckNotAnswerable):
		d.outcome = ChoiceCheckNotAnswerable
	case errors.Is(err, dsdstate.ErrNotWritable):
		j.why = "the player's DSD state file is refused in memory"
		d.outcome, d.err = ChoiceStateUnreadable, err
	case errors.Is(err, dsdstate.ErrLandedNotHeld):
		j.why = "an earlier write reached the state's file and is not known to be durable"
		d.outcome, d.err = ChoicePersistUncertain, err
	case errors.Is(err, atomicwrite.ErrLanded):
		// The file holds the write's state, not known to be durable, and the
		// store the state as it was: until the next start reads the file, a
		// rate plays only where the two play it alike (setOver). Nothing is
		// rolled back. A crash between the rename and the directory's sync may
		// leave either choice in the file.
		b.landedChoice.Store(&wrote)
		w.wrote(false)
		d.outcome, d.err = ChoicePersistUncertain, err
	case err != nil:
		j.why = "the state's write failed before it replaced the file"
		d.outcome, d.err = ChoicePersistFailed, err
	default:
		w.wrote(true)
		d.outcome, d.st = ChoiceWritten, wrote
	}
	return d
}

// publishChoice publishes the reading rd a write decided on, from the state its
// decision read (d.st), through the refreshes' own step (publishLocked): no set
// for a DAC that did not read (noSet), else the set setOver makes, which holds
// it to what a landed write's state plays alike. The answer is the pair that
// step stored, or the one standing where it changed nothing, captured under
// the same hold of the refreshes' lock (policyOf), so no refresh publishes
// between the write's reading, its publication and its answer. The hold ends
// there, and a refresh is asked of the refreshes' goroutine where one was owed
// meanwhile, where the state the store holds now would publish another pair
// from the same reading (a write of another writer's that landed after the
// decision: published after the write's, at a larger publication), or where
// the publication panicked, contained as a refresh's reading is (containDSD):
// its answer keeps the write's outcome, with no epoch unless its pair was
// stored. bad_request, no write, carries no epoch. Its lock is taken as
// endChoiceReading's is, whatever ctx says.
func (b *Backend) publishChoice(ctx context.Context, rd choiceRead, d choiceDecision) ChoiceResult {
	res := ChoiceResult{Outcome: d.outcome, Err: d.err}
	owed := func() (owed bool) {
		b.dsdMu.Lock()
		defer b.dsdMu.Unlock()
		defer func() {
			if r := recover(); r != nil {
				b.containDSD("the publication of a choice's write", r)
				owed = true
			}
			b.mu.Lock()
			b.choiceReading = false
			if b.checkOwed {
				b.checkOwed, b.refreshOwed = false, true
			}
			b.mu.Unlock()
			if b.refreshOwed {
				b.refreshOwed, owed = false, true
			}
		}()
		next, view := b.setOf(rd, d.st)
		b.publishLocked(rd.at, next, view, func(pub *dsdPublished) { res.Policy, res.Epoch = b.policyOf(pub), true })
		b.choicePublished()
		now, nowView := b.setOf(rd, b.dsdState())
		b.mu.Lock()
		defer b.mu.Unlock()
		pub := b.dsdPub.Load()
		return !pub.set.same(now) || !reflect.DeepEqual(pub.view, b.withCheckLocked(nowView))
	}()
	if owed {
		b.askChoiceRefresh(ctx)
	}
	if res.Outcome == ChoiceBadRequest {
		res.Policy, res.Epoch = PlayerPolicy{}, false
	}
	return res
}

// setOf is the set a write's reading rd makes with st, and its read side, as
// a refresh makes them (readDSD).
func (b *Backend) setOf(rd choiceRead, st dsdstate.State) (*dsdSet, DSDView) {
	if rd.r == nil {
		return noSet(rd.why, st)
	}
	return b.setOver(rd.r, st)
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
// device, the console's test tone and the DSD check included (B601, B728 B12),
// and no release was left
// unconfirmed: otherwise it answers playback_not_stopped at once, nothing
// taken. From then on no Start registers, no probe takes the slot and no tone
// is admitted. A probe that held the slot as the lease was taken is waited
// for, the lease held, at most until ctx ends: then the lease is let go,
// persist_failed, the cause ctx's error. The lease is its caller's only once
// this returns it: let go here on every other way out, a panic's included.
func (b *Backend) takeLease(ctx context.Context, j *choiceJournal) (*configLease, ChoiceOutcome, error) {
	b.mu.Lock()
	if len(b.sessions) > 0 || b.holder != nil || b.terminalUnconfirmed || b.toning != nil || b.checking != nil {
		b.mu.Unlock()
		return nil, ChoicePlaybackNotStopped, nil
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
			return nil, ChoicePersistFailed, ctx.Err()
		}
	}
	handed = true
	return l, "", nil
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

func (b *Backend) choicePublished() {
	if sm := b.cfg.Seams; sm != nil && sm.ChoicePublished != nil {
		sm.ChoicePublished()
	}
}

func (b *Backend) choiceAnswered() {
	if sm := b.cfg.Seams; sm != nil && sm.ChoiceAnswered != nil {
		sm.ChoiceAnswered()
	}
}

func (b *Backend) choiceRefreshed() {
	if sm := b.cfg.Seams; sm != nil && sm.ChoiceRefreshed != nil {
		sm.ChoiceRefreshed()
	}
}
