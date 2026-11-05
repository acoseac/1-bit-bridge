// Package dacprofile is the bridge player's database of DAC profiles (B728
// B2; ops/plan-b728-dsd-capability-design.md, section 2): what is known of a
// DAC model's DSD, keyed by the model's USB identity. It is a reviewed JSON
// file in this package, profiles.json, embedded in the binary: it changes by
// a PR and never at run time, and there is no local override, since the
// user's per-DAC choice is the override and a hand-edited file that enabled
// DoP for the wrong IDs would be one more way to send DoP to a DAC that does
// not decode it.
//
// A profile names its model by match alternatives, each an AND of the fields
// it sets: the vendor and product IDs (four lowercase hexadecimal digits,
// usbid.Hex16), and optionally the USB interface the card is, the product
// string exactly as the sysfs reader returns it (alsa.Qualification: white
// space trimmed, never empty, never holding a NUL, CR or LF), and a list of
// bcdDevice values that narrows a claim to known firmware. A unit is known
// when exactly one profile has an alternative it matches (Match): none, or
// two, and it is unknown. Nothing is matched by name: the iOS registry
// matches the name iOS gives a port, by substring, first hit first, because
// iOS shows no USB IDs, and none of that matcher is imported. A vendor and
// product ID that many products ship, a reference firmware's, is a shared
// reference ID (SharedReferenceIDs), and an alternative on one names its
// product string, which Lint requires and Match asks again.
//
// A profile's claims, each with its source and its date or version, one line
// a claim in its provenance, and never a host name, a serial, a person's name
// or an email:
//   - Native: the DSD formats and a maximum bit rate, from a device-specific
//     DSD quirk of the Linux kernel's USB audio driver at the tag
//     scripts/alsa-abi pins, which states no rate, or from a captured stream
//     file whose altsets list the format. A vendor-wide quirk names no model
//     and seeds nothing, and neither does a shared reference ID without its
//     product string. The live altsets still decide what is offered: the
//     claim makes the model known.
//   - DoP: the rates a listening heard on a device (never one a negotiation or
//     a DoP lock reached: those hear nothing), the maker's stated rate and the
//     rates a device refused, from the iOS registry at a named commit, which
//     with its caveats and a DSD resync delay (Timing) is all that is
//     imported from it, each heard and refused rate credited by a clause of
//     its dop line in evidence's one form ("heard DSD128: <source>", "refused
//     DSD256: <source>", "silence at DSD256: <source>"; Lint); and the model's
//     level controls the DoP gain check passes over (AnalogVolumeControls,
//     UnityAtRawMax), each with its own provenance or evidence.
//   - Timing: the DSD silence each hardware run starts with, which a model's
//     DSD resync delay in the iOS registry, a measured value, may seed
//     (DSDPrefillMs; "timing: iOS profile <row>'s DSD resync delay, 0.7 s
//     (iOS <commit>)"), never as a policy beyond that silence.
//   - Caveats, codes from a closed list, each with its note.
//
// Key is a model's identity as a choice and its evidence are keyed and as
// the wire names it (dacKey): the vendor and product IDs, the interface and
// the product string, never a serial, a port, a card number or a device
// number, compared by its canonical form and never field by field.
package dacprofile

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// SchemaVersion is the schema profiles.json is written in.
const SchemaVersion = 1

// DefaultDSDPrefillMs is the DSD silence a hardware run starts with where a
// profile gives none: iOS's DSD resync default (the design's section 2.1).
const DefaultDSDPrefillMs = 400

// The caveat codes, a closed list (the design's section 2.3).
const (
	// CaveatDoPNoiseInHiddenMode: a mode the bridge cannot see, such as a
	// DAC-side PEQ, makes the DAC play DoP as noise.
	CaveatDoPNoiseInHiddenMode = "dop_noise_in_hidden_mode"
	// CaveatModeSwitchCapsRates: a UAC1/UAC2 switch on the DAC caps its rates.
	CaveatModeSwitchCapsRates = "mode_switch_caps_rates"
	// CaveatTransitionClicks: the DAC clicks at a transition into DSD128 or
	// DSD256.
	CaveatTransitionClicks = "transition_clicks"
)

// CaveatCodes is the closed list of caveat codes.
func CaveatCodes() []string {
	return []string{CaveatDoPNoiseInHiddenMode, CaveatModeSwitchCapsRates, CaveatTransitionClicks}
}

// The kinds of a claim's source.
const (
	// SourceKernelQuirk is a device-specific DSD quirk in the Linux kernel's
	// sound/usb/quirks.c at a tag, named in the source's ref.
	SourceKernelQuirk = "kernel-quirk"
	// SourceCapture is a capture of a real unit on Linux, dated in the ref.
	SourceCapture = "capture"
	// SourceIOSRegistry is a row of the iOS app's DACProfile.swift at a commit:
	// "DACProfile.swift <row id> at iOS <commit>".
	SourceIOSRegistry = "ios-registry"
)

// Database is profiles.json.
type Database struct {
	SchemaVersion int `json:"schemaVersion"`
	// SharedReferenceIDs are the vendor and product IDs many products ship:
	// an alternative on one names its product string.
	SharedReferenceIDs []SharedID `json:"sharedReferenceIds"`
	Profiles           []Profile  `json:"profiles"`
}

// SharedID is a shared reference ID, and why it is one, with its source.
type SharedID struct {
	VendorID  string `json:"vendorId"`
	ProductID string `json:"productId"`
	Why       string `json:"why"`
}

// Profile is one DAC model and what is known of its DSD.
type Profile struct {
	// ID is the profile's dotted name ("chord.hugo2"); Name is a person's.
	ID   string `json:"id"`
	Name string `json:"name"`
	// Match is the model's alternatives: a unit matching one is this model.
	Match   []Match  `json:"match"`
	Native  *Native  `json:"native"`
	DoP     *DoP     `json:"dop"`
	Timing  *Timing  `json:"timing"`
	Caveats []Caveat `json:"caveats"`
	// Provenance is one line a claim: "identity: …", "native: …", "dop: …",
	// "timing: …" and "caveat <code>: …", each naming its source and its date
	// or version. A dop line's clauses (the text after "dop: ", split on
	// semicolons and trimmed) credit DoP rates only in one form: a marker,
	// heard, refused or silence at, its rates separated by commas, then a
	// colon and free text, or the clause's end ("refused DSD256: the unit
	// would not negotiate its carrier (iOS cd125088)"). A clause that starts
	// with no marker is free text and evidence of nothing; a marker clause of
	// another shape, or a rate both heard and refused (refused and silence at
	// are one result), is a problem.
	Provenance []string `json:"provenance"`
}

// Match is one alternative: every field it sets must hold. A nil field
// matches any value, a unit's absence of one included.
type Match struct {
	VendorID  string   `json:"vendorId"`
	ProductID string   `json:"productId"`
	Interface *int     `json:"interface"`
	Product   *string  `json:"product"`
	BCDDevice []string `json:"bcdDevice"`
}

// Native is a claim that the model plays native DSD.
type Native struct {
	// MaxBitRate is the highest one-bit rate of the 44.1 kHz family its source
	// shows, or 0 where the source states none, as a kernel quirk does: 0 is
	// unspecified, never unlimited. The rates offered are the live altsets'
	// and the probe's, and no rate's evidence is made of a claim that states
	// none.
	MaxBitRate int `json:"maxBitRate"`
	// Formats are ALSA's DSD formats the source names (DSD_U32_BE, …).
	Formats []string `json:"formats"`
	Source  Source   `json:"source"`
}

// DoP is what is known of the model's DSD over PCM.
type DoP struct {
	// HeardMaxBitRate is the highest rate a listening on a device heard,
	// never one a negotiation, a hardware confirmation or a DoP lock reached
	// (section 1.1: the iOS lock hears nothing back), and a heard clause of a
	// dop provenance line lists it ("heard DSD128: <source>", the form
	// Provenance gives); StatedMaxBitRate is the maker's figure; 0 is none.
	HeardMaxBitRate  int `json:"heardMaxBitRate"`
	StatedMaxBitRate int `json:"statedMaxBitRate"`
	// RefusedBitRates are the rates a device refused, or played as silence,
	// each listed by a refused or a silence at clause of a dop provenance line
	// ("refused DSD256: <source>", "silence at DSD256: <source>").
	RefusedBitRates []int `json:"refusedBitRates"`
	// AnalogVolumeControls are the controls this model applies after its DoP
	// decoder, which the DoP gain check passes over.
	AnalogVolumeControls []AnalogControl `json:"analogVolumeControls"`
	// UnityAtRawMax are the level controls with no dB scale whose raw maximum
	// is unity on this model, on the firmware each lists.
	UnityAtRawMax []RawMaxControl `json:"unityAtRawMax"`
	Source        Source          `json:"source"`
}

// ControlID names a card's control as ALSA does: its interface (MIXER, CARD,
// PCM and the rest, as amixer prints them), its name and its index.
type ControlID struct {
	Interface string `json:"interface"`
	Name      string `json:"name"`
	Index     int    `json:"index"`
}

// AnalogControl is a control the model applies after its DoP decoder, with
// its own provenance line.
type AnalogControl struct {
	ControlID
	Provenance string `json:"provenance"`
}

// RawMaxControl is a level control with no dB scale whose raw maximum is
// unity on this model, on the firmware BCDDevice lists, as its evidence
// shows.
type RawMaxControl struct {
	ControlID
	RawMax    int64    `json:"rawMax"`
	BCDDevice []string `json:"bcdDevice"`
	Evidence  Evidence `json:"evidence"`
}

// Evidence is what shows a control's raw maximum to be unity: the maker's
// document, by its title and section, or a measurement on a named firmware.
type Evidence struct {
	// Kind is "document" or "measured".
	Kind    string `json:"kind"`
	Title   string `json:"title,omitempty"`
	Section string `json:"section,omitempty"`
	// Method is how a measurement showed it: "digital-capture" (the unit's
	// digital output or loopback at the raw maximum, compared byte for byte
	// with the PCM sent in) or "dop-indicator-heard" (a DoP stream at the raw
	// maximum the DAC's own indicator reports as DSD, heard clean).
	Method string `json:"method,omitempty"`
	// BCDDevice is the firmware a measurement was taken on.
	BCDDevice string `json:"bcdDevice,omitempty"`
	// Ref is the measurement's record, with its date.
	Ref string `json:"ref,omitempty"`
}

// Source is where a claim comes from: its kind and a reference a reviewer
// can follow.
type Source struct {
	Kind string `json:"kind"`
	Ref  string `json:"ref"`
}

// Timing is the model's DSD timing.
type Timing struct {
	// DSDPrefillMs is the DSD silence written before content in each hardware
	// run; 0 is DefaultDSDPrefillMs.
	DSDPrefillMs int `json:"dsdPrefillMs"`
	// PCMEntryMs is the settle the model needs before PCM after DSD; 0 is
	// none given. It is in the design's example and read by nothing, so no
	// seed sets it until its meaning and its reader are specified.
	PCMEntryMs int `json:"pcmEntryMs"`
}

// Caveat is a code from the closed list, the transport it concerns ("dop",
// "native", or "" for both) and the note a person is shown.
type Caveat struct {
	Code      string `json:"code"`
	Transport string `json:"transport"`
	Note      string `json:"note"`
}

//go:embed profiles.json
var embedded []byte

// Default is the embedded database, parsed and linted afresh at each call,
// so no caller shares it with another.
func Default() (*Database, error) {
	return Parse(embedded)
}

// Parse decodes a database strictly (a field the schema does not have, or
// anything after the object, is refused) and lints it, refusing it whole on
// any problem.
func Parse(data []byte) (*Database, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var db Database
	if err := dec.Decode(&db); err != nil {
		return nil, fmt.Errorf("dacprofile: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("dacprofile: something follows the database's object")
	}
	if problems := db.Lint(); len(problems) > 0 {
		return nil, fmt.Errorf("dacprofile: %w", errors.Join(problems...))
	}
	return &db, nil
}
