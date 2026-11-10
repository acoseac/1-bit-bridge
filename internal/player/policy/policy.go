package policy

import (
	"slices"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsa"
	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/player/dacprofile"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
	"github.com/acoseac/1-bit-bridge/internal/usbid"
)

// The refusals of a DSD rate the effective set names: the design's section
// 3.2's closed list, dsd_state_unreadable (section 2.5) and
// rate_not_supported, the refusal of every rate past DSD256 in every mode.
const (
	ReasonOffByChoice          engine.Reason = "dsd_off_by_choice"
	ReasonNativeNotOffered     engine.Reason = "native_not_offered"
	ReasonDoPNotOffered        engine.Reason = "dop_not_offered"
	ReasonGainNotUnity         engine.Reason = "dop_gain_not_unity"
	ReasonGainUnverifiable     engine.Reason = "dop_gain_unverifiable"
	ReasonConfirmationRequired engine.Reason = "dop_confirmation_required"
	ReasonAboveChoice          engine.Reason = "dsd_rate_above_choice"
	ReasonFirstUsePending      engine.Reason = "first_use_pending"
	ReasonListenerRejected     engine.Reason = "listener_rejected"
	ReasonConversionNotOffered engine.Reason = "conversion_not_offered"
	ReasonStateUnreadable      engine.Reason = dsdstate.RefusalStateUnreadable
	ReasonRateNotSupported     engine.Reason = "rate_not_supported"
)

// Rung is a tuple's evidence, a word of the design's section 1.1 ladder.
// listener confirmed, streaming observed and driver accepted order what was
// established on this box; reported is the profile database's claim, which
// a tuple publishes only where nothing on this box was established. A tuple
// with none of them has no rung ("").
type Rung string

const (
	// RungReported: the profile database names the transport at the rate.
	RungReported Rung = "reported"
	// RungDriverAccepted: the kernel's driver took the exact format at the
	// rate on this endpoint, under this kernel. For DoP only the PCM
	// carrier, S32_LE with 24 to 32 significant bits: it says nothing of
	// DSD.
	RungDriverAccepted Rung = "driver_accepted"
	// RungStreamingObserved: an occurrence of the tuple played 5 s of its
	// content on this box with no fault.
	RungStreamingObserved Rung = "streaming_observed"
	// RungListenerConfirmed: the latest listener answer for the transport
	// and rate under this firmware is clean.
	RungListenerConfirmed Rung = "listener_confirmed"
)

// Outcome is what plays at a rate: native DSD, DoP, DSD converted to PCM by
// the player, or nothing (refused).
type Outcome string

const (
	OutcomeNative    Outcome = "native"
	OutcomeDoP       Outcome = "dop"
	OutcomeConverted Outcome = "converted"
	OutcomeRefused   Outcome = "refused"
)

// The pause methods a tuple names (the design's section 7.1).
const (
	PauseKernel  = "kernel"
	PauseSilence = "silence"
)

// DefaultTail is a tuple's tail, the DSD silence after a run's last frame
// (the design's section 7.1: tailMs defaults to 100 ms).
const DefaultTail = 100 * time.Millisecond

// dsdRates are the rates the policy decides, ascending.
var dsdRates = []int{dsdstate.DSD64, dsdstate.DSD128, dsdstate.DSD256}

// DSDRates are the one-bit rates a channel the policy decides: DSD64,
// DSD128 and DSD256, ascending, in a list of the caller's own. DSD512 and
// every other rate are refused in every mode (rate_not_supported).
func DSDRates() []int { return slices.Clone(dsdRates) }

// nativeFormats are the native DSD formats the sink opens, in the order the
// player prefers them (today's, authorization.go's).
var nativeFormats = []format.ALSAFormat{format.DSDU32BE, format.DSDU32LE, format.DSDU16LE}

// Inputs are what the effective policy is computed from, every one a value
// B7's adapters read, bounded, before the call.
type Inputs struct {
	// Unit is the bound DAC's model identity and firmware (bcdDevice), as
	// the sysfs reader reads them.
	Unit dacprofile.Unit
	// Profiles is the profile database (dacprofile.Default's). The DAC is
	// known when exactly one profile matches Unit; nil knows none.
	Profiles *dacprofile.Database
	// State is the player's DSD state as its store holds it.
	State dsdstate.State
	// Kernel is the running kernel's release (uname -r), under which a
	// durable driver acceptance counts.
	Kernel string
	// Altsets are the bound PCM device's playback altsets, its stream
	// file's.
	Altsets []alsa.Altset
	// Probe is the silent probe's answers for the unit now, its model,
	// firmware, kernel and enumeration (alsapcm.ProbeCache.Answer of each
	// alsapcm.ProbeCandidates candidate): a candidate it holds no answer for
	// is not probed, which is no refusal.
	Probe []alsapcm.ProbeResult
	// Mixer is what the DoP gain check reads of the card (CheckGain).
	Mixer Mixer
}

// WithState is in over st, every other input as it is: one reading of the box
// computed against more than one state (B728 B9: a choice write's set beside
// the one the state gives without it).
func (in Inputs) WithState(st dsdstate.State) Inputs {
	in.State = st
	return in
}

// Tuple is a native or DoP plan: the transport, the format, the rate and
// the timing, what reconciliation compares (the design's section 3.5).
type Tuple struct {
	// Transport is native or dop.
	Transport string
	// Format is the ALSA format the sink opens: DSD_U32_BE, DSD_U32_LE or
	// DSD_U16_LE for native, S32_LE for DoP.
	Format format.ALSAFormat
	// BitRate is the one-bit rate a channel.
	BitRate int
	// Pause is kernel where the probe found the refined setup offers the
	// kernel's PAUSE, and silence otherwise, a candidate not probed
	// included (the design's section 7.1).
	Pause string
	// Tail is DefaultTail; Prefill the profile's dsdPrefillMs, or
	// dacprofile.DefaultDSDPrefillMs.
	Tail, Prefill time.Duration
}

// Target is a conversion's PCM (the design's section 4.3): 176.4 kHz where
// the DAC takes it at 24 bits or more, else 88.2 kHz, else 44.1 kHz, at 24
// valid bits; 16 only for a DAC that takes no 24-bit target, at the best of
// those rates it takes 16 bits at. Packing is the first packing of the
// player's table at that depth (S24_3LE, S24_LE, S32_LE for 24 bits; S16_LE
// first for 16) that the DAC takes at the rate with the bits significant:
// what the converted occurrence opens and joins a sink as, and nothing else
// (B728 B11), so the cache of packings a PCM track took and a predecessor's
// transport never choose it.
type Target struct {
	RateHz    int
	ValidBits int
	Packing   format.ALSAFormat
}

// Plan is what plays at one rate under a set, or why nothing does.
type Plan struct {
	BitRate int
	Outcome Outcome
	// Tuple is a native or DoP plan's, Target a converted one's.
	Tuple  Tuple
	Target Target
	// Reason is a refusal's word, and Detail its sentence for a log line,
	// which names no serial or port.
	Reason engine.Reason
	Detail string
	// Evidence is a native or DoP plan's tuple's highest rung, and
	// EvidenceOn its date (YYYY-MM-DD): "" for a reported tuple, an undated
	// answer and an answer of the probe's alone, which nothing dates.
	Evidence   Rung
	EvidenceOn string
}

// Same reports whether q plays what p plays: both native or both DoP with
// the same tuple, transport, format, rate and timing alike, or both
// converted to the same target. Evidence and a refusal's words are not
// compared, and a refusal plays nothing, so it is never the same.
func (p Plan) Same(q Plan) bool {
	if p.Outcome != q.Outcome || p.BitRate != q.BitRate {
		return false
	}
	switch p.Outcome {
	case OutcomeNative, OutcomeDoP:
		return p.Tuple == q.Tuple
	case OutcomeConverted:
		return p.Target == q.Target
	}
	return false
}

// Native is what the policy established of native DSD at a rate.
type Native struct {
	// Format is the first native format the DAC offers at the rate, in the
	// player's order: an altset of the bound PCM device lists it at the rate
	// over its width in two channels, and the probe did not refuse it. ""
	// while it offers none.
	Format format.ALSAFormat
	// Evidence and EvidenceOn are that format's tuple's (Plan's).
	Evidence   Rung
	EvidenceOn string
	// Acknowledged: native's first use is acknowledged under the unit's
	// firmware by an entry that covers the rate.
	Acknowledged bool
	// Rejected and Clean: the latest listener answer for native at the rate
	// under the unit's firmware is noise or nothing, or clean.
	Rejected, Clean bool
}

// DoP is what the policy established of DoP at a rate.
type DoP struct {
	// Offered: the carrier, S32_LE at the rate over 16, is offered: an
	// altset lists it in two channels with 24 to 32 significant bits, and
	// the probe did not refuse it, an acceptance's refined bits 24 to 32
	// (alsapcm.ProbeResult.DoPCarrier).
	Offered bool
	// Evidence and EvidenceOn are the carrier's tuple's.
	Evidence   Rung
	EvidenceOn string
	// Gain is the DoP gain check at the rate (CheckGain): the zero Gain
	// when no altset lists the carrier.
	Gain Gain
	// Heard and Stated are the profile's DoP claims at the rate, by its
	// heard and its stated maximum. ProfileRefused: the profile lists the
	// rate as refused, which takes both claims and the confirmation away
	// there, unless Clean.
	Heard, Stated, ProfileRefused bool
	// Confirmed: the DAC's DoP confirmation, under the unit's firmware,
	// counts at the rate.
	Confirmed bool
	// Acknowledged, Rejected and Clean are as Native's, for DoP.
	Acknowledged, Rejected, Clean bool
}

// Rate is the set at one rate: its plan, and what was established of each
// transport there, which B8 publishes as the mode's options.
type Rate struct {
	Plan   Plan
	Native Native
	DoP    DoP
}

// Set is the effective DSD set: one answer for every rate, which selection
// and publication both take (R10, R11), so what the epoch advertises is
// what plays.
type Set struct {
	// Key and BCDDevice are the model and the firmware the set was computed
	// for, what the check after open compares the opened node with.
	Key       dacprofile.Key
	BCDDevice string
	// Known reports that exactly one profile matched the unit, Profile its
	// ID.
	Known   bool
	Profile string
	// Mode and MaxBitRate are the choice that applied: the DAC's own (Own),
	// or defaultMode with no cap.
	Mode       string
	MaxBitRate int
	Own        bool
	// Unreadable reports that the state did not read, and every rate is
	// refused as dsd_state_unreadable.
	Unreadable bool
	// Conversion is the DAC's conversion target, nil when it takes none.
	Conversion *Target
	rates      []Rate
}

// Identity is a unit's identity as a set is computed for it and as a sink
// opens it: the DAC model's key and its firmware (bcdDevice), never a serial
// or a port.
type Identity struct {
	Key       dacprofile.Key
	BCDDevice string
}

// Same reports whether i and o are one identity: the keys by their canonical
// forms, the firmware by its value, an unread one ("") the same only as
// another unread one.
func (i Identity) Same(o Identity) bool {
	return i.Key.Equal(o.Key) && canonicalBCD(i.BCDDevice) == canonicalBCD(o.BCDDevice)
}

// Identity is the identity s was computed for.
func (s Set) Identity() Identity { return Identity{Key: s.Key, BCDDevice: s.BCDDevice} }

// canonicalBCD is a firmware in the state's form, four lowercase
// hexadecimal digits, or "" for one that does not read.
func canonicalBCD(s string) string {
	if v, err := usbid.ParseHex16(s); err == nil {
		return usbid.Hex16(v)
	}
	return ""
}

// All is the set at every rate it decides, DSD64 to DSD256, in a list of the
// caller's own.
func (s Set) All() []Rate { return slices.Clone(s.rates) }

// At is the set at bitRate: refused as rate_not_supported for any rate but
// DSD64, DSD128 and DSD256.
func (s Set) At(bitRate int) Rate {
	for _, r := range s.rates {
		if r.Plan.BitRate == bitRate {
			return r
		}
	}
	return Rate{Plan: Plan{BitRate: bitRate, Outcome: OutcomeRefused, Reason: ReasonRateNotSupported,
		Detail: "DSD64, DSD128 and DSD256 only: no native, DoP or converted transport here carries this DSD rate"}}
}

// Effective is the effective DSD set for the bound DAC (the package's doc;
// the design's section 3.2). It reads nothing but in.
func Effective(in Inputs) Set {
	rd := newReading(in)
	s := Set{Key: in.Unit.Key, BCDDevice: rd.bcd}
	if rd.profile != nil {
		s.Known, s.Profile = true, rd.profile.ID
	}
	if in.State.Unreadable() {
		s.Unreadable = true
		for _, r := range dsdRates {
			s.rates = append(s.rates, Rate{Plan: Plan{BitRate: r, Outcome: OutcomeRefused, Reason: ReasonStateUnreadable,
				Detail: "the player's DSD state did not read: DSD is refused at every rate until a choice is written again"}})
		}
		return s
	}
	s.Mode, s.MaxBitRate, s.Own = in.State.ChoiceFor(in.Unit.Key)
	s.Conversion = rd.conversion()
	for _, r := range dsdRates {
		rate := Rate{Native: rd.native(r), DoP: rd.dop(r)}
		rate.Plan = rd.decide(r, s, rate.Native, rate.DoP)
		s.rates = append(s.rates, rate)
	}
	return s
}

// reading is what Effective read of its inputs, shared by every rate.
type reading struct {
	in Inputs
	// bcd is the unit's firmware in the state's form, four lowercase
	// hexadecimal digits, "" when it did not read: then no acknowledgement,
	// confirmation, answer or evidence, each held under a firmware, counts.
	bcd     string
	profile *dacprofile.Profile
	dac     dsdstate.DAC
	gain    *gainCheck
}

func newReading(in Inputs) *reading {
	rd := &reading{in: in, bcd: canonicalBCD(in.Unit.BCDDevice)}
	if in.Profiles != nil {
		if p, ok := in.Profiles.Match(in.Unit); ok {
			rd.profile = &p
		}
	}
	rd.dac, _ = in.State.DAC(in.Unit.Key)
	rd.gain = newGainCheck(in.Mixer, rd.profile, rd.bcd)
	return rd
}

// answer is the probe's answer for c, not probed when it holds none.
func (rd *reading) answer(c alsapcm.ProbeCandidate) alsapcm.ProbeResult {
	for _, r := range rd.in.Probe {
		if r.Candidate == c {
			return r
		}
	}
	return alsapcm.ProbeResult{Candidate: c, Answer: alsapcm.ProbeNotProbed}
}

// takes reports whether a lists f at rateHz frames a second in two
// channels, as a discrete rate or inside its continuous range.
func takes(a alsa.Altset, f format.ALSAFormat, rateHz int) bool {
	if a.Channels == nil || *a.Channels != 2 || !slices.Contains(a.Formats, string(f)) {
		return false
	}
	if slices.Contains(a.Rates, rateHz) {
		return true
	}
	return a.RateRange != nil && a.RateRange.Min <= rateHz && rateHz <= a.RateRange.Max
}

// DoPAltsets are the altsets of alts that list DoP's carrier at bitRate,
// S32_LE at bitRate over 16 in two channels: the ones the gain check reads at
// that rate (CheckGain), since the kernel chooses among them at the open.
func DoPAltsets(alts []alsa.Altset, bitRate int) []alsa.Altset {
	var out []alsa.Altset
	for _, a := range alts {
		if takes(a, format.S32LE, bitRate/16) {
			out = append(out, a)
		}
	}
	return out
}

// listed reports whether an altset of the bound PCM device lists f at
// rateHz in two channels.
func (rd *reading) listed(f format.ALSAFormat, rateHz int) bool {
	return slices.ContainsFunc(rd.in.Altsets, func(a alsa.Altset) bool { return takes(a, f, rateHz) })
}

// listener is the latest listener answer for transport at bitRate under the
// unit's firmware, nil when there is none.
func (rd *reading) listener(transport string, bitRate int) *dsdstate.Answer {
	if rd.bcd == "" {
		return nil
	}
	for i, a := range rd.dac.Listener {
		if a.Transport == transport && a.BitRate == bitRate && a.BCDDevice == rd.bcd {
			return &rd.dac.Listener[i]
		}
	}
	return nil
}

// acknowledged reports whether transport's first use is acknowledged under
// the unit's firmware by an entry that covers bitRate: a write's covers
// every rate, a migrated one only the rates its record approved
// (dsdstate.FirstUse.Covers). A rejection re-arms first use by taking the
// acknowledgement away in the write that records it (B12; the design's
// section 6.4), so the acknowledgement is read as the state holds it.
func (rd *reading) acknowledged(transport string, bitRate int) bool {
	return rd.bcd != "" && slices.ContainsFunc(rd.dac.FirstUse, func(f dsdstate.FirstUse) bool {
		return f.Transport == transport && f.BCDDevice == rd.bcd && f.Covers(bitRate)
	})
}

// evidence is what the state holds as observed on this box of transport in
// f at bitRate under the unit's firmware, nil when nothing.
func (rd *reading) evidence(transport string, f format.ALSAFormat, bitRate int) *dsdstate.Evidence {
	if rd.bcd == "" {
		return nil
	}
	for i, e := range rd.dac.Evidence {
		if e.Transport == transport && e.Format == string(f) && e.BitRate == bitRate && e.BCDDevice == rd.bcd {
			return &rd.dac.Evidence[i]
		}
	}
	return nil
}

// rung is a tuple's highest rung and its date, from the latest listener
// answer, what the state observed of the tuple on this box, the probe's
// acceptance (which carries no date) and the profile's claim (reported).
// A driver acceptance the state holds counts only under the running kernel,
// and a rung under another firmware never counts (the design's section 1.2).
func (rd *reading) rung(transport string, f format.ALSAFormat, bitRate int, accepted, reported bool) (Rung, string) {
	if a := rd.listener(transport, bitRate); a != nil && a.Answer == dsdstate.AnswerClean {
		return RungListenerConfirmed, a.On
	}
	e := rd.evidence(transport, f, bitRate)
	switch {
	case e != nil && !e.StreamingObservedAt.IsZero():
		return RungStreamingObserved, day(e.StreamingObservedAt)
	case e != nil && !e.DriverAcceptedAt.IsZero() && e.Kernel != "" && e.Kernel == rd.in.Kernel:
		return RungDriverAccepted, day(e.DriverAcceptedAt)
	case accepted:
		return RungDriverAccepted, ""
	case reported:
		return RungReported, ""
	}
	return "", ""
}

// day is t's date in UTC, YYYY-MM-DD.
func day(t time.Time) string { return t.UTC().Format(time.DateOnly) }

// native is what the policy established of native DSD at bitRate.
func (rd *reading) native(bitRate int) Native {
	var n Native
	if a := rd.listener(dsdstate.TransportNative, bitRate); a != nil {
		n.Clean = a.Answer == dsdstate.AnswerClean
		n.Rejected = !n.Clean
	}
	n.Acknowledged = rd.acknowledged(dsdstate.TransportNative, bitRate)
	f, res, ok := rd.nativeOffered(bitRate)
	if !ok {
		return n
	}
	n.Format = f
	n.Evidence, n.EvidenceOn = rd.rung(dsdstate.TransportNative, f, bitRate, res.Answer == alsapcm.ProbeAccepted, rd.reportsNative(f, bitRate))
	return n
}

// nativeOffered is the first native format offered at bitRate and the
// probe's answer for it.
func (rd *reading) nativeOffered(bitRate int) (format.ALSAFormat, alsapcm.ProbeResult, bool) {
	for _, f := range nativeFormats {
		frame := bitRate / (8 * format.DSDWordBytes(f))
		if !rd.listed(f, frame) {
			continue
		}
		res := rd.answer(alsapcm.ProbeCandidate{Format: f, RateHz: frame})
		if res.Answer == alsapcm.ProbeRefused {
			continue
		}
		return f, res, true
	}
	return "", alsapcm.ProbeResult{}, false
}

// reportsNative reports whether the profile reports f at bitRate: a native
// claim that names the format and states a maximum at or above the rate. A
// kernel quirk's claim states none (MaxBitRate 0): it makes the model known
// and reports no rate (B2).
func (rd *reading) reportsNative(f format.ALSAFormat, bitRate int) bool {
	n := rd.profileNative()
	return n != nil && n.MaxBitRate != 0 && bitRate <= n.MaxBitRate && slices.Contains(n.Formats, string(f))
}

func (rd *reading) profileNative() *dacprofile.Native {
	if rd.profile == nil {
		return nil
	}
	return rd.profile.Native
}

func (rd *reading) profileDoP() *dacprofile.DoP {
	if rd.profile == nil {
		return nil
	}
	return rd.profile.DoP
}

// dop is what the policy established of DoP at bitRate.
func (rd *reading) dop(bitRate int) DoP {
	var d DoP
	if a := rd.listener(dsdstate.TransportDoP, bitRate); a != nil {
		d.Clean = a.Answer == dsdstate.AnswerClean
		d.Rejected = !d.Clean
	}
	d.Acknowledged = rd.acknowledged(dsdstate.TransportDoP, bitRate)
	// A profile's refused rate outranks its stated maximum and the user's
	// confirmation: only this box's own clean answer at the rate overrides
	// it (B5's decision; the design's section 3.2).
	blocked := false
	if p := rd.profileDoP(); p != nil {
		d.ProfileRefused = slices.Contains(p.RefusedBitRates, bitRate)
		blocked = d.ProfileRefused && !d.Clean
		d.Heard = !blocked && p.HeardMaxBitRate != 0 && bitRate <= p.HeardMaxBitRate
		d.Stated = !blocked && p.StatedMaxBitRate != 0 && bitRate <= p.StatedMaxBitRate
	}
	c := rd.dac.DoPConfirmed
	d.Confirmed = !blocked && c != nil && rd.bcd != "" && c.BCDDevice == rd.bcd
	carrier := alsapcm.ProbeCandidate{Format: format.S32LE, RateHz: bitRate / 16}
	res := rd.answer(carrier)
	switch res.Answer {
	case alsapcm.ProbeRefused:
	case alsapcm.ProbeAccepted:
		// The refined setup's significant bits decide: they are what the
		// open commits, and the DoP sink refuses fewer than 24.
		d.Offered = rd.listed(format.S32LE, carrier.RateHz) && res.DoPCarrier()
	default:
		d.Offered = slices.ContainsFunc(rd.in.Altsets, func(a alsa.Altset) bool {
			return takes(a, format.S32LE, carrier.RateHz) && a.Bits != nil && *a.Bits >= 24 && *a.Bits <= 32
		})
	}
	if sels := DoPAltsets(rd.in.Altsets, bitRate); len(sels) > 0 {
		d.Gain = rd.gain.at(sels)
	}
	if d.Offered {
		d.Evidence, d.EvidenceOn = rd.rung(dsdstate.TransportDoP, format.S32LE, bitRate, res.DoPCarrier(), d.Heard || d.Stated)
	}
	return d
}

// tuple is the plan of transport in f at bitRate, with its timing: the
// pause the probe's answer for the candidate offers, DefaultTail, and the
// profile's prefill.
func (rd *reading) tuple(transport string, f format.ALSAFormat, bitRate int, c alsapcm.ProbeCandidate) Tuple {
	t := Tuple{Transport: transport, Format: f, BitRate: bitRate, Pause: PauseSilence, Tail: DefaultTail,
		Prefill: dacprofile.DefaultDSDPrefillMs * time.Millisecond}
	if res := rd.answer(c); res.Answer == alsapcm.ProbeAccepted && res.Pause {
		t.Pause = PauseKernel
	}
	if rd.profile != nil && rd.profile.Timing != nil && rd.profile.Timing.DSDPrefillMs > 0 {
		t.Prefill = time.Duration(rd.profile.Timing.DSDPrefillMs) * time.Millisecond
	}
	return t
}

func (rd *reading) nativePlan(bitRate int, n Native) Plan {
	frame := bitRate / (8 * format.DSDWordBytes(n.Format))
	return Plan{BitRate: bitRate, Outcome: OutcomeNative, Evidence: n.Evidence, EvidenceOn: n.EvidenceOn,
		Tuple: rd.tuple(dsdstate.TransportNative, n.Format, bitRate, alsapcm.ProbeCandidate{Format: n.Format, RateHz: frame})}
}

func (rd *reading) dopPlan(bitRate int, d DoP) Plan {
	return Plan{BitRate: bitRate, Outcome: OutcomeDoP, Evidence: d.Evidence, EvidenceOn: d.EvidenceOn,
		Tuple: rd.tuple(dsdstate.TransportDoP, format.S32LE, bitRate, alsapcm.ProbeCandidate{Format: format.S32LE, RateHz: bitRate / 16})}
}

func refused(bitRate int, reason engine.Reason, detail string) Plan {
	return Plan{BitRate: bitRate, Outcome: OutcomeRefused, Reason: reason, Detail: detail}
}

// decide is section 3.2's table at bitRate under the choice s holds. Where
// several conditions fail under an explicit native or dop, the reason is the
// first in this order: what the DAC does not offer, then a listener's
// rejection, then what the gain check cannot see, then what the user can
// change (the level, the confirmation, the cap, the acknowledgement).
func (rd *reading) decide(bitRate int, s Set, n Native, d DoP) Plan {
	under := s.MaxBitRate == 0 || bitRate <= s.MaxBitRate
	offered := n.Format != ""
	converted := func() Plan {
		if s.Conversion == nil {
			return refused(bitRate, ReasonConversionNotOffered, "the DAC takes no conversion target: no 176.4, 88.2 or 44.1 kHz PCM in two channels")
		}
		return Plan{BitRate: bitRate, Outcome: OutcomeConverted, Target: *s.Conversion}
	}
	switch s.Mode {
	case dsdstate.ModeAuto:
		if s.Known {
			if offered && n.Acknowledged && !n.Rejected && under {
				return rd.nativePlan(bitRate, n)
			}
			if (d.Heard || d.Confirmed) && d.Offered && d.Gain.Unity && d.Acknowledged && !d.Rejected && under {
				return rd.dopPlan(bitRate, d)
			}
		} else {
			if n.Clean && offered && n.Acknowledged && under {
				return rd.nativePlan(bitRate, n)
			}
			if d.Clean && d.Confirmed && d.Offered && d.Gain.Unity && d.Acknowledged && under {
				return rd.dopPlan(bitRate, d)
			}
		}
		return converted()
	case dsdstate.ModeNative:
		switch {
		case !offered:
			return refused(bitRate, ReasonNativeNotOffered, "the DAC offers no native DSD format at this rate")
		case n.Rejected:
			return refused(bitRate, ReasonListenerRejected, "a listener's latest answer for native DSD at this rate is a rejection")
		case !under:
			return refused(bitRate, ReasonAboveChoice, "the rate is above the DAC's chosen maximum")
		case !n.Acknowledged:
			return refused(bitRate, ReasonFirstUsePending, "native DSD's first use is not acknowledged at this rate on this firmware")
		}
		return rd.nativePlan(bitRate, n)
	case dsdstate.ModeDoP:
		switch {
		case !d.Offered:
			return refused(bitRate, ReasonDoPNotOffered, "the DAC offers no DoP carrier at this rate: S32_LE at the rate over 16 with 24 to 32 significant bits")
		case d.ProfileRefused && !d.Clean:
			return refused(bitRate, ReasonDoPNotOffered, "the DAC list records DoP at this rate as refused or heard as silence")
		case d.Rejected:
			return refused(bitRate, ReasonListenerRejected, "a listener's latest answer for DoP at this rate is a rejection")
		case !d.Gain.Unity && d.Gain.Reason == ReasonGainUnverifiable:
			return refused(bitRate, ReasonGainUnverifiable, d.Gain.Detail)
		case !d.Gain.Unity:
			return refused(bitRate, ReasonGainNotUnity, d.Gain.Detail)
		case !d.Heard && !d.Stated && !d.Confirmed:
			return refused(bitRate, ReasonConfirmationRequired, "the DAC list claims no DoP at this rate, and DoP is not confirmed on this firmware")
		case !under:
			return refused(bitRate, ReasonAboveChoice, "the rate is above the DAC's chosen maximum")
		case !d.Acknowledged:
			return refused(bitRate, ReasonFirstUsePending, "DoP's first use is not acknowledged at this rate on this firmware")
		}
		return rd.dopPlan(bitRate, d)
	case dsdstate.ModeConvert:
		return converted()
	case dsdstate.ModeOff:
		return refused(bitRate, ReasonOffByChoice, "DSD is off by the DAC's choice or the bridge's default")
	}
	// A state that decodes holds no other mode; one that holds it is a state
	// that would not read.
	return refused(bitRate, ReasonStateUnreadable, "the choice that applies is not a mode")
}

// The conversion targets of the design's section 4.3, best first, and the
// packings a 24-bit target takes, in the table's order (packing.go).
var (
	conversionRates = []int{176400, 88200, 44100}
	packings24      = []format.ALSAFormat{format.S243LE, format.S24LE, format.S32LE}
	packings16      = []format.ALSAFormat{format.S16LE, format.S243LE, format.S24LE, format.S32LE}
)

// conversion is the DAC's conversion target, nil when it takes none: the
// best rate it takes at 24 bits or more in a 24-bit packing, else the best
// it takes at 16, in the first of the depth's packings that carries them.
func (rd *reading) conversion() *Target {
	for _, bits := range []int{24, 16} {
		packings := packings24
		if bits == 16 {
			packings = packings16
		}
		for _, rate := range conversionRates {
			for _, f := range packings {
				if rd.pcmTaken(f, rate, bits) {
					return &Target{RateHz: rate, ValidBits: bits, Packing: f}
				}
			}
		}
	}
	return nil
}

// pcmTaken reports whether the DAC takes f at rateHz in two channels with
// at least bits significant: an altset lists it, the probe did not refuse
// it, and its significant bits are bits or more: the altset's Bits line, or
// the format's own depth with none, held to an accepted probe's refined bits
// when they are fewer, as they hold the DoP carrier: what the open commits.
func (rd *reading) pcmTaken(f format.ALSAFormat, rateHz, bits int) bool {
	res := rd.answer(alsapcm.ProbeCandidate{Format: f, RateHz: rateHz})
	if res.Answer == alsapcm.ProbeRefused {
		return false
	}
	for _, a := range rd.in.Altsets {
		if !takes(a, f, rateHz) {
			continue
		}
		have := pcmDepth(f)
		if a.Bits != nil {
			have = *a.Bits
		}
		if res.Answer == alsapcm.ProbeAccepted && res.Msbits > 0 {
			have = min(have, res.Msbits)
		}
		if have >= bits {
			return true
		}
	}
	return false
}

// pcmDepth is a PCM packing's own depth.
func pcmDepth(f format.ALSAFormat) int {
	switch f {
	case format.S16LE:
		return 16
	case format.S243LE, format.S24LE:
		return 24
	case format.S32LE:
		return 32
	}
	return 0
}
