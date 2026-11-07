package playback

import (
	"slices"

	"github.com/acoseac/1-bit-bridge/internal/alsa"
	"github.com/acoseac/1-bit-bridge/internal/player/dacprofile"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdconv"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/policy"
)

// The read side of the player's DSD (B728 B8; the design's section 7.1):
// what GET /v1/outputs publishes of the bound DAC's DSD beside the set's
// tuples and converted rates. A refresh makes it (readDSD) from the reading
// and the state it computes the set from, and the set published carries it
// (dsdSet.view), so a reader of Facts reads no state file, no box and no lock
// a reading holds, and never sees one generation's tuples beside another
// reading's choice or first use.

// The words of a transport's first use (dsdDAC.firstUse[].state).
const (
	// FirstUsePending: the transport's first use is not acknowledged under
	// the unit's firmware.
	FirstUsePending = "pending"
	// FirstUseAcknowledged: it is, and no occurrence of the transport has yet
	// played 5 s of its content without a fault under it.
	FirstUseAcknowledged = "acknowledged"
	// FirstUseObserved: it is, and one has (streaming observed).
	FirstUseObserved = "observed"
)

// optionModes are the modes the read side says what each offers, in the
// order it lists them: every mode a DAC's choice takes but auto, which is no
// explicit choice and resolves to what the set plays.
var optionModes = []string{dsdstate.ModeNative, dsdstate.ModeDoP, dsdstate.ModeConvert, dsdstate.ModeOff}

// DSDView is the bound DAC's DSD as the set it was made with says it.
type DSDView struct {
	// Mode is the choice word that applies to the bound DAC: its own
	// choice's mode, or the state's defaultMode for a DAC with none; off
	// while no DAC is available or the state does not read.
	Mode string
	// AllowDoP reports whether DoP may play for the bound DAC at some rate it
	// offers DoP at, by its profile (a heard or stated DoP rate the profile
	// does not refuse) or by the user's confirmation under the unit's
	// firmware: neither the gain check nor first use, which the DoP option and
	// first use say. It describes, and permits nothing.
	AllowDoP bool
	// Evidence is each of the set's tuples' rung and date, in the tuples'
	// order (PlayerPolicy.Tuples).
	Evidence []DSDEvidence
	// Refused is each rate from DSD64 to DSD256 the set plays neither way,
	// with the word it refuses that rate with.
	Refused []DSDRefusal
	// Warnings are what the app must be able to explain: the bridge's, and
	// the bound DAC's own.
	Warnings []DSDWarning
	// DAC is the bound DAC, nil while no DAC is available: no set was
	// computed for one.
	DAC *DSDDAC
}

// DSDEvidence is a rung of the evidence ladder (policy.Rung) and its date,
// YYYY-MM-DD: each "" where there is none.
type DSDEvidence struct {
	Rung, On string
}

// DSDRefusal is a rate the set plays neither way, and its word.
type DSDRefusal struct {
	BitRate int
	Reason  string
}

// DSDWarning is a warning of the state's, its code and its reason word.
type DSDWarning struct {
	Code, Reason string
}

// DSDDAC is the bound DAC: its key on the wire, its profile, the choice
// that applies, each mode's option and each transport's first use.
type DSDDAC struct {
	// Key is the DAC's dacKey: the model key's (dacprofile.Key.DACKey), its
	// product string with the unit's own serial and port taken out first, so
	// the key is no function of either.
	Key      string
	Profile  *DSDProfile
	Choice   DSDChoice
	Options  []DSDOption
	FirstUse []DSDFirstUse
}

// DSDProfile is the profile the DAC matched: nil for an unknown DAC.
type DSDProfile struct {
	ID, Name string
	Caveats  []DSDCaveat
}

// DSDCaveat is one of a profile's caveats: its code, the transport it
// concerns ("" for both) and its note.
type DSDCaveat struct {
	Code, Transport, Note string
}

// DSDChoice is the choice that applies: its mode and cap, the revision of
// the DAC's own choice (0 for none) and whether its DoP confirmation counts
// under the unit's firmware.
type DSDChoice struct {
	Mode         string
	MaxBitRate   int
	Revision     uint64
	DoPConfirmed bool
}

// DSDOption is what a mode offers the DAC.
type DSDOption struct {
	Mode    string
	Offered bool
	// Rates are the rates native or DoP is offered at, with their evidence:
	// non-nil for those two, nil for convert and off.
	Rates []DSDOptionRate
	// NeedsConfirmation is DoP's: a rate it offers needs the user's
	// confirmation, the profile neither hearing nor stating DoP there.
	NeedsConfirmation bool
	// Blocked is DoP's: the gain check's word where it refuses DoP at every
	// rate DoP is offered at, "" otherwise.
	Blocked string
	// Plan is convert's, the target it converts to; nil while not offered.
	Plan *dsdconv.Plan
}

// DSDOptionRate is a rate a mode is offered at, and its evidence.
type DSDOptionRate struct {
	BitRate  int
	Evidence DSDEvidence
}

// DSDFirstUse is a transport's first use: its state, and the rates a
// migrated acknowledgement covers (nil for every rate).
type DSDFirstUse struct {
	Transport, State string
	BitRates         []int
}

// noView is the read side of a set that could not be computed: no DAC, off,
// every rate refused for want of a reading of the DAC (ReasonDACUnread, the
// word selection refuses it with), and the bridge's warnings of st, a state
// that did not read among them, which shows with no DAC available.
func noView(st dsdstate.State) DSDView {
	v := DSDView{Mode: dsdstate.ModeOff, Evidence: []DSDEvidence{}, Refused: []DSDRefusal{}, Warnings: warningsOf(st, nil)}
	for _, r := range policy.DSDRates() {
		v.Refused = append(v.Refused, DSDRefusal{BitRate: r, Reason: string(ReasonDACUnread)})
	}
	return v
}

// viewOf is the read side of d, a set computed for the unit usb from the
// state st: what each rate's plan and facts (policy.Rate) say, the choice st
// holds for the set's model, and its first use under the set's firmware.
// Its free text (the profile's name, its caveats' notes) goes out with the
// unit's own serial and port taken out (unitText), as the model does, and
// its key with them taken out of the product string (wireKey).
func viewOf(d *dsdSet, st dsdstate.State, usb alsa.USBIdentity) DSDView {
	s := d.set
	key, bcd := s.Key, canonicalBCD(s.BCDDevice)
	mode, maxBitRate, own := st.ChoiceFor(key)
	if s.Unreadable {
		// No choice applies over a state that did not read: every rate is
		// refused before one is read (the design's section 3.2).
		mode, maxBitRate, own = dsdstate.ModeOff, 0, false
	}
	dac, _ := st.DAC(key)
	v := DSDView{Mode: mode, Evidence: []DSDEvidence{}, Refused: []DSDRefusal{}, Warnings: warningsOf(st, &key)}
	choice := DSDChoice{Mode: mode, MaxBitRate: maxBitRate}
	if own && dac.Choice != nil {
		choice.Revision = dac.Choice.Revision
	}
	if c := dac.DoPConfirmed; c != nil && bcd != "" && c.BCDDevice == bcd {
		choice.DoPConfirmed = true
	}
	options := map[string]DSDOption{
		dsdstate.ModeNative:  {Mode: dsdstate.ModeNative, Rates: []DSDOptionRate{}},
		dsdstate.ModeDoP:     {Mode: dsdstate.ModeDoP, Rates: []DSDOptionRate{}},
		dsdstate.ModeConvert: {Mode: dsdstate.ModeConvert},
		dsdstate.ModeOff:     {Mode: dsdstate.ModeOff, Offered: true},
	}
	native, dop := options[dsdstate.ModeNative], options[dsdstate.ModeDoP]
	unblocked := false
	for _, r := range s.All() {
		switch r.Plan.Outcome {
		case policy.OutcomeNative, policy.OutcomeDoP:
			v.Evidence = append(v.Evidence, DSDEvidence{Rung: string(r.Plan.Evidence), On: r.Plan.EvidenceOn})
		case policy.OutcomeRefused:
			v.Refused = append(v.Refused, DSDRefusal{BitRate: r.Plan.BitRate, Reason: string(r.Plan.Reason)})
		}
		if n := r.Native; n.Format != "" {
			native.Offered = true
			native.Rates = append(native.Rates, DSDOptionRate{BitRate: r.Plan.BitRate, Evidence: DSDEvidence{Rung: string(n.Evidence), On: n.EvidenceOn}})
		}
		p := r.DoP
		if !p.Offered || p.ProfileRefused && !p.Clean {
			// A rate the profile refuses DoP at is not offered, but for this
			// box's own clean answer there (the design's section 3.2).
			continue
		}
		dop.Offered = true
		dop.Rates = append(dop.Rates, DSDOptionRate{BitRate: r.Plan.BitRate, Evidence: DSDEvidence{Rung: string(p.Evidence), On: p.EvidenceOn}})
		if p.Heard || p.Stated || p.Confirmed {
			v.AllowDoP = true
		} else {
			dop.NeedsConfirmation = true
		}
		if p.Gain.Unity {
			unblocked = true
		} else if dop.Blocked == "" {
			dop.Blocked = string(p.Gain.Reason)
		}
	}
	if unblocked {
		dop.Blocked = ""
	}
	options[dsdstate.ModeNative], options[dsdstate.ModeDoP] = native, dop
	if t := s.Conversion; t != nil {
		p := convPlanOf(*t)
		options[dsdstate.ModeConvert] = DSDOption{Mode: dsdstate.ModeConvert, Offered: true, Plan: &p}
	}
	out := &DSDDAC{Key: wireKey(key, usb), Choice: choice}
	for _, m := range optionModes {
		out.Options = append(out.Options, options[m])
	}
	for _, t := range []string{dsdstate.TransportNative, dsdstate.TransportDoP} {
		f := DSDFirstUse{Transport: t, State: FirstUsePending}
		for _, e := range dac.FirstUse {
			if e.Transport != t || bcd == "" || e.BCDDevice != bcd {
				continue
			}
			f.State, f.BitRates = FirstUseAcknowledged, slices.Clone(e.BitRates)
			if !e.ObservedAt.IsZero() {
				f.State = FirstUseObserved
			}
		}
		out.FirstUse = append(out.FirstUse, f)
	}
	if p := d.profile; p != nil {
		// The profile's free text, its name and its caveats' notes, as the
		// model's; its ID and a caveat's code and transport are words of the
		// reviewed database's, which taking out a short serial would mangle.
		out.Profile = &DSDProfile{ID: p.ID, Name: unitText(p.Name, usb), Caveats: []DSDCaveat{}}
		for _, c := range p.Caveats {
			out.Profile.Caveats = append(out.Profile.Caveats, DSDCaveat{Code: c.Code, Transport: c.Transport, Note: unitText(c.Note, usb)})
		}
	}
	v.DAC = out
	return v
}

// warningsOf are st's warnings the read side publishes, by their codes and
// reasons, in the state's order: the bridge's, and the DAC's of key, nil for
// none; a warning another DAC owns is that DAC's to explain.
func warningsOf(st dsdstate.State, key *dacprofile.Key) []DSDWarning {
	out := []DSDWarning{}
	for _, w := range st.Warnings {
		if w.DAC == nil || key != nil && w.DAC.Equal(*key) {
			out = append(out, DSDWarning{Code: w.Code, Reason: w.Reason})
		}
	}
	return out
}

// wireKey is the dacKey of key's model as it goes out: the model key's
// (dacprofile.Key.DACKey), its product string with the unit's own serial and
// port taken out first (unitText). A product string that holds the serial
// would make the key a hash of it, and the model published beside it, the
// serial redacted, gives every other byte that hash was taken of: a short
// serial could be found from the key by trying each. Taken out, the key is no
// function of the serial, and two units of the model share it. A product
// string that holds neither keys the model's own dacKey.
func wireKey(key dacprofile.Key, usb alsa.USBIdentity) string {
	product, ok := key.Product()
	if !ok {
		return key.DACKey()
	}
	clean := unitText(product, usb)
	if clean == product {
		return key.DACKey()
	}
	var ifacep *int
	if iface, ok := key.Interface(); ok {
		ifacep = &iface
	}
	k, err := dacprofile.NewKey(key.VendorID(), key.ProductID(), ifacep, &clean)
	if err != nil {
		// A product string the redaction left no product string of (none is
		// refused that a reading gave): the model with no product, never the
		// hash of the serial.
		k, _ = dacprofile.NewKey(key.VendorID(), key.ProductID(), ifacep, nil)
	}
	return k.DACKey()
}

// clone is a copy of v that shares nothing with it.
func (v DSDView) clone() DSDView {
	c := v
	c.Evidence, c.Refused, c.Warnings = slices.Clone(v.Evidence), slices.Clone(v.Refused), slices.Clone(v.Warnings)
	if v.DAC == nil {
		return c
	}
	d := *v.DAC
	if p := v.DAC.Profile; p != nil {
		cp := *p
		cp.Caveats = slices.Clone(p.Caveats)
		d.Profile = &cp
	}
	d.Options = nil
	for _, o := range v.DAC.Options {
		o.Rates = slices.Clone(o.Rates)
		if o.Plan != nil {
			p := *o.Plan
			o.Plan = &p
		}
		d.Options = append(d.Options, o)
	}
	d.FirstUse = nil
	for _, f := range v.DAC.FirstUse {
		f.BitRates = slices.Clone(f.BitRates)
		d.FirstUse = append(d.FirstUse, f)
	}
	c.DAC = &d
	return c
}
