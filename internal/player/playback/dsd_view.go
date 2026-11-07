package playback

import (
	"github.com/acoseac/1-bit-bridge/internal/player/dsdconv"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
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
	// AllowDoP reports whether DoP may play for the bound DAC at some rate,
	// by its profile (a heard or stated DoP rate the profile does not refuse)
	// or by the user's confirmation under the unit's firmware. It describes,
	// and permits nothing.
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
