package policy

import (
	"errors"
	"slices"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/player/dacprofile"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
)

// A choice write (B728 B9; the design's sections 2.5 and 7.2): the checks of
// PUT /v1/outputs/{id}/dsd that read the state, 7 to 10 of section 7.2's
// order, and what the write does to the state. Choose is pure, so the writer
// runs it inside the store's one write (dsdstate.Store.Update), on the state
// that write is made from: the compare-and-set and every check that reads the
// state read the state the write replaces, never a copy read before it.

// ReasonChoiceUncertain is the refusal of a rate while a choice write that
// reached the state's file is not known to be durable (B728 B9): the state in
// memory and the one the file may hold at the next start play it differently.
const ReasonChoiceUncertain engine.Reason = "dsd_choice_uncertain"

// ChoiceReason is a word a choice write is refused with for what the state
// holds (the design's section 7.2, checks 7 to 10).
type ChoiceReason string

const (
	// ChoiceStale: the write's base is not the stored revision.
	ChoiceStale ChoiceReason = "stale"
	// ChoiceModeNotOffered: the DAC offers the mode at no rate.
	ChoiceModeNotOffered ChoiceReason = "mode_not_offered"
	// ChoiceConfirmationRequired: DoP at a rate neither the profile claims
	// nor a confirmation under the unit's firmware covers, and the write
	// carries no confirmDoP.
	ChoiceConfirmationRequired ChoiceReason = "dop_confirmation_required"
	// ChoiceFirstUseUnacknowledged was an explicit native or DoP written with
	// no firstUseAcknowledged where its first use was not acknowledged; since
	// B990 no write is refused with it, a word an older client knows.
	ChoiceFirstUseUnacknowledged ChoiceReason = "first_use_unacknowledged"
)

// ChoiceRefusal is a choice write refused, its word and its sentence, which
// names no serial or port.
type ChoiceRefusal struct {
	Reason ChoiceReason
	Detail string
}

func (r *ChoiceRefusal) Error() string { return string(r.Reason) + ": " + r.Detail }

// Choice is a write of a DAC's DSD choice: the revision the writer read, the
// mode and the cap, the two consents, and who writes it (the app or the
// console; the migration writes no choice through a write).
type Choice struct {
	BaseRevision         uint64
	Mode                 string
	MaxBitRate           int
	ConfirmDoP           bool
	FirstUseAcknowledged bool
	By                   string
}

// Check refuses a choice that is no write at all: a mode, a cap or a writer
// the state does not hold.
func (c Choice) Check() error {
	switch {
	case !slices.Contains([]string{dsdstate.ModeAuto, dsdstate.ModeNative, dsdstate.ModeDoP, dsdstate.ModeConvert, dsdstate.ModeOff}, c.Mode):
		return errors.New("policy: the choice's mode is not a mode")
	case c.MaxBitRate != 0 && !slices.Contains(dsdRates, c.MaxBitRate):
		return errors.New("policy: the choice's maxBitRate is neither 0 nor a DSD rate")
	case c.By != dsdstate.ByApp && c.By != dsdstate.ByConsole:
		return errors.New("policy: a choice is written by the app or the console")
	}
	return nil
}

// StoredRevision is the revision a choice write's base is compared with: the
// one the read side publishes for key's model of stamp's maker (viewOf): its
// own choice's, and 0 for one with none or over a state that does not read,
// where no choice applies. A write back of what GET published therefore
// matches.
func StoredRevision(st dsdstate.State, key dacprofile.Key, stamp dsdstate.Stamp) uint64 {
	if st.Unreadable() {
		return 0
	}
	if d, ok := st.DAC(key, stamp); ok && d.Choice != nil {
		return d.Choice.Revision
	}
	return 0
}

// Choose is what a write of c, at now, does to in.State for the bound DAC
// (in.Unit, of in.Stamp's maker: its own entry, never another maker's for
// the key), or the check that refuses it (a *ChoiceRefusal, the state
// untouched). Its other errors are a choice that is no write at all: a mode,
// a cap or a writer the state does not hold.
//
// The checks, in the design's order, each on the state as it is: the base is
// the stored revision (StoredRevision); the mode is offered at some rate,
// whatever the cap (native and DoP where a rate offers the transport, DoP's
// profile-refused rates aside; convert where the DAC takes a target; auto and
// off always); DoP carries confirmDoP where a rate it is offered at has
// neither the profile's claim nor a confirmation under the firmware, as the
// read side's needsConfirmation says. No first use is asked (B990): an
// explicit native or DoP is taken with no firstUseAcknowledged. A state that
// does not read is read as the write leaves it, its refusal cleared, so a
// write over it sees what the DAC offers.
//
// The write: the DAC's choice at the next revision (one more than its own,
// whatever was published, so a revision never goes back), set now by c.By;
// confirmDoP, on a dop or auto write, records the confirmation under the
// unit's firmware where none counts there, and is kept by every later write
// (the confirmation is the model's, not the mode's); firstUseAcknowledged
// records an acknowledgement of every rate under the firmware for each
// transport the write plays once acknowledged (an explicit native or dop its
// own transport; auto each transport its set would play at some rate, under
// its cap, its rejections kept and DoP by the profile's claim or a
// confirmation; convert and off none), replacing a migrated entry, one under
// another firmware and, for an explicit mode, one a rejection re-armed, and
// keeping a whole entry as it is, its observation with it; it gates no rate
// since B990 and clears no rejection. An explicit native or dop that makes
// its transport the DAC's mode, its own choice another mode or none, is that
// transport's retry: it clears the transport's rejections under the firmware
// at the rates its cap admits; a write that leaves the mode as it was clears
// none. A unit whose firmware does not read takes the choice and records
// neither consent: nothing is held under no firmware, and nothing cleared.
// Every write clears the DAC's warnings and state_unreadable
// (dsdstate.State.ClearWarningsFor).
func Choose(in Inputs, c Choice, now time.Time) (dsdstate.State, error) {
	if err := c.Check(); err != nil {
		return in.State, err
	}
	key, stamp := in.Unit.Key, in.Stamp
	if got := StoredRevision(in.State, key, stamp); c.BaseRevision != got {
		return in.State, &ChoiceRefusal{Reason: ChoiceStale, Detail: "the choice was written since the revision the write was made from"}
	}
	st := in.State.Clone()
	st.ClearWarningsFor(key)
	read := in.WithState(st)
	rd := newReading(read)
	s := Effective(read)
	switch c.Mode {
	case dsdstate.ModeNative:
		if !slices.ContainsFunc(s.rates, func(r Rate) bool { return r.Native.Format != "" }) {
			return in.State, &ChoiceRefusal{Reason: ChoiceModeNotOffered, Detail: "the DAC offers no native DSD format at any rate"}
		}
	case dsdstate.ModeDoP:
		if !slices.ContainsFunc(s.rates, func(r Rate) bool { return dopOffered(r.DoP) }) {
			return in.State, &ChoiceRefusal{Reason: ChoiceModeNotOffered, Detail: "the DAC offers no DoP carrier at any rate"}
		}
		if !c.ConfirmDoP && slices.ContainsFunc(s.rates, func(r Rate) bool { return dopOffered(r.DoP) && !r.DoP.Heard && !r.DoP.Stated && !r.DoP.Confirmed }) {
			return in.State, &ChoiceRefusal{Reason: ChoiceConfirmationRequired, Detail: "DoP is offered at a rate the DAC list claims no DoP at and no confirmation covers on this firmware"}
		}
	case dsdstate.ModeConvert:
		if s.Conversion == nil {
			return in.State, &ChoiceRefusal{Reason: ChoiceModeNotOffered, Detail: "the DAC takes no conversion target: no 176.4, 88.2 or 44.1 kHz PCM in two channels"}
		}
	}
	explicit := transportOf(c.Mode)
	d := st.Entry(key, stamp)
	var rev uint64
	var was string
	if d.Choice != nil {
		rev, was = d.Choice.Revision, d.Choice.Mode
	}
	at := now.UTC().Truncate(time.Second)
	d.Choice = &dsdstate.Choice{Mode: c.Mode, MaxBitRate: c.MaxBitRate, Revision: rev + 1, SetAt: at, SetBy: c.By}
	if rd.bcd == "" {
		return st, nil
	}
	if explicit != "" && was != c.Mode {
		// The transport's retry: its rejections at the rates the cap admits go.
		d.Listener = slices.DeleteFunc(d.Listener, func(a dsdstate.Answer) bool {
			return a.Transport == explicit && a.BCDDevice == rd.bcd && a.Answer != dsdstate.AnswerClean && (c.MaxBitRate == 0 || a.BitRate <= c.MaxBitRate)
		})
	}
	if c.ConfirmDoP && (c.Mode == dsdstate.ModeDoP || c.Mode == dsdstate.ModeAuto) && (d.DoPConfirmed == nil || d.DoPConfirmed.BCDDevice != rd.bcd) {
		d.DoPConfirmed = &dsdstate.DoPConfirmation{BCDDevice: rd.bcd, ConfirmedAt: at, SetBy: c.By}
	}
	if !c.FirstUseAcknowledged {
		return st, nil
	}
	var transports []string
	switch c.Mode {
	case dsdstate.ModeNative, dsdstate.ModeDoP:
		transports = []string{explicit}
	case dsdstate.ModeAuto:
		transports = autoTransports(read, st, key, stamp, rd.bcd, at)
	}
	for _, t := range transports {
		whole := rd.wholeAcknowledgement(t) || c.Mode == dsdstate.ModeAuto && rd.wholeEntry(t)
		if !whole {
			d.FirstUse = slices.DeleteFunc(d.FirstUse, func(f dsdstate.FirstUse) bool { return f.Transport == t })
			d.FirstUse = append(d.FirstUse, dsdstate.FirstUse{Transport: t, BCDDevice: rd.bcd, AcknowledgedAt: at})
		}
	}
	return st, nil
}

// dopOffered reports whether d offers DoP at its rate as the read side says
// it (viewOf): the carrier offered, and the profile not refusing the rate but
// for this box's own clean answer there.
func dopOffered(d DoP) bool { return d.Offered && !(d.ProfileRefused && !d.Clean) }

// transportOf is the transport an explicit mode plays: native's, DoP's, or ""
// for every other mode.
func transportOf(mode string) string {
	switch mode {
	case dsdstate.ModeNative:
		return dsdstate.TransportNative
	case dsdstate.ModeDoP:
		return dsdstate.TransportDoP
	}
	return ""
}

// wholeEntry reports whether transport's acknowledgement under the unit's
// firmware covers every rate.
func (rd *reading) wholeEntry(transport string) bool {
	return rd.bcd != "" && slices.ContainsFunc(rd.dac.FirstUse, func(f dsdstate.FirstUse) bool {
		return f.Transport == transport && f.BCDDevice == rd.bcd && f.BitRates == nil
	})
}

// wholeAcknowledgement reports whether transport's acknowledgement stands
// whole, which a write that carries one keeps as it is: its entry under the
// unit's firmware covers every rate, and no listener's latest answer there
// rejects it at any rate (a rejection re-arms it).
func (rd *reading) wholeAcknowledgement(transport string) bool {
	if !rd.wholeEntry(transport) {
		return false
	}
	return !slices.ContainsFunc(rd.dac.Listener, func(a dsdstate.Answer) bool {
		return a.Transport == transport && a.BCDDevice == rd.bcd && a.Answer != dsdstate.AnswerClean
	})
}

// autoTransports are the transports an auto write's set plays at some rate
// once acknowledged: st, the state the write leaves so far, with the entry of
// key's model of stamp's maker holding both transports acknowledged at every
// rate under bcd and every rejection kept, read as in reads.
func autoTransports(in Inputs, st dsdstate.State, key dacprofile.Key, stamp dsdstate.Stamp, bcd string, at time.Time) []string {
	probe := st.Clone()
	d := probe.Entry(key, stamp)
	d.FirstUse = []dsdstate.FirstUse{
		{Transport: dsdstate.TransportNative, BCDDevice: bcd, AcknowledgedAt: at},
		{Transport: dsdstate.TransportDoP, BCDDevice: bcd, AcknowledgedAt: at},
	}
	var out []string
	for _, r := range Effective(in.WithState(probe)).rates {
		var t string
		switch r.Plan.Outcome {
		case OutcomeNative:
			t = dsdstate.TransportNative
		case OutcomeDoP:
			t = dsdstate.TransportDoP
		default:
			continue
		}
		if !slices.Contains(out, t) {
			out = append(out, t)
		}
	}
	return out
}

// Intersect is cur while a choice write that reached the state's file is not
// known to be durable, landed the set of the state it left there, both
// computed from one reading of the unit: each rate cur plays plays only where
// landed plays it the same way (Plan.Same), and is refused as
// dsd_choice_uncertain otherwise, so what plays now plays under either state
// the next start may read; a rate cur refuses keeps its own word, and its
// Candidate only where landed's is the same plan, so the DSD check plays
// nothing either state would not let it. The identity, the choice and each
// rate's facts are cur's.
func Intersect(cur, landed Set) Set {
	out := cur
	out.rates = nil
	for _, r := range cur.rates {
		l := landed.At(r.Plan.BitRate).Plan
		if r.Plan.Outcome != OutcomeRefused && !l.Same(r.Plan) {
			r.Plan = refused(r.Plan.BitRate, ReasonChoiceUncertain,
				"a choice write reached the state's file and is not known to be durable, and the state it left plays this rate another way: the next start reads the file")
		}
		if c := r.Plan.Candidate; c != nil && (l.Candidate == nil || !l.Candidate.Same(*c)) {
			r.Plan.Candidate = nil
		}
		out.rates = append(out.rates, r)
	}
	return out
}
