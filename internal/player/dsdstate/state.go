package dsdstate

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/acoseac/1-bit-bridge/internal/player/dacprofile"
	"github.com/acoseac/1-bit-bridge/internal/usbid"
)

// FileName is the state's file in the data directory.
const FileName = "player-dsd.json"

// SchemaVersion is the schema the state is written in. It is frozen: a later
// field, or a meaning a field of it does not have, is a new schema, which
// this build refuses (and sets aside) as it refuses any file it would not
// write.
const SchemaVersion = 1

// The choice's modes (the design's section 3.2). A DAC with no choice of its
// own takes defaultMode, which is auto or off.
const (
	ModeAuto    = "auto"
	ModeNative  = "native"
	ModeDoP     = "dop"
	ModeConvert = "convert"
	ModeOff     = "off"
)

// The transports an acknowledgement, an answer and evidence are of.
const (
	TransportNative = "native"
	TransportDoP    = "dop"
)

// Who wrote a choice or a DoP confirmation: the app, the console or the
// migration. Never who answered beyond that (the design's section 6.4).
const (
	ByApp       = "app"
	ByConsole   = "console"
	ByMigration = "migration"
)

// A listener's answers to the chime (the design's section 6).
const (
	AnswerClean   = "clean"
	AnswerNoise   = "noise"
	AnswerNothing = "nothing"
)

// The logical DSD rates a choice, an acknowledgement, an answer and evidence
// name: DSD64, DSD128 and DSD256.
const (
	DSD64  = 2822400
	DSD128 = 5644800
	DSD256 = 11289600
)

// The warnings' codes, a closed list (the design's section 7.1).
const (
	// WarningMigrationRecordRefused: the owner's record did not carry over,
	// for its Reason: owned by the DAC the migration's part went to.
	WarningMigrationRecordRefused = "migration_record_refused"
	// WarningMigrationRecordMissing: no record to carry over, none named or
	// none at its name: owned by the DAC the migration's part went to.
	WarningMigrationRecordMissing = "migration_record_missing"
	// WarningMigratedOff: an explicit opt-out became defaultMode off, for every
	// DAC with no choice of its own: the bridge's.
	WarningMigratedOff = "migrated_off"
	// WarningStateUnreadable: the state did not read and DSD is refused until
	// a choice is written again: the bridge's.
	WarningStateUnreadable = "state_unreadable"
)

// A refused record's reasons (the warning's Reason): the kind of the legacy
// reader's refusal (qualification.ErrStaleVersion, ErrIdentityMismatch,
// ErrHashMismatch), or unreadable for every other.
const (
	ReasonStaleVersion     = "stale_version"
	ReasonIdentityMismatch = "identity_mismatch"
	ReasonHashMismatch     = "hash_mismatch"
	ReasonUnreadable       = "unreadable"
)

// RefusalStateUnreadable is the refusal of every DSD rate while the state
// does not read (State.Unreadable), which the effective policy (B5) and the
// cutover (B7) give.
const RefusalStateUnreadable = "dsd_state_unreadable"

// The bounds of what the state holds, past which it does not decode.
const (
	maxFileBytes   = 1 << 20
	maxDACs        = 256
	maxListPerDAC  = 64
	maxWarnings    = 512
	maxKernelBytes = 256
	maxNameBytes   = 4096
)

// State is the player's DSD state. A zero State is not one: Defaults is a
// fresh install's, and Decode reads one from its file.
type State struct {
	// DefaultMode is the mode of a DAC with no choice of its own: auto, or
	// off where an explicit dsdMode: off (or dop with allowDoP false) was
	// migrated, or the state did not read.
	DefaultMode string
	// DACs are the DAC models the state holds something for, one entry a
	// model.
	DACs []DAC
	// MigratedFrom is the migration's marker: nil while no migration ran,
	// and then the migration runs (Migrate) when the legacy keys set any key.
	MigratedFrom *Marker
	// Warnings are what the app must be able to explain.
	Warnings []Warning
}

// DAC is what the state holds for one DAC model.
type DAC struct {
	Key dacprofile.Key
	// Choice is the DAC's own choice, nil while it has none.
	Choice *Choice
	// DoPConfirmed is the user's confirmation that this DAC decodes DoP, or
	// the migrated owner's record's DoP evidence: nil while there is none.
	DoPConfirmed *DoPConfirmation
	// FirstUse holds at most one acknowledgement a transport.
	FirstUse []FirstUse
	// Listener holds the latest answer for each transport, rate and
	// firmware, whatever format the answer's chime played in.
	Listener []Answer
	// Evidence is what was observed on this box, by transport, format, rate
	// and firmware. A restore drops it.
	Evidence []Evidence
}

// Choice is a DAC's own choice.
type Choice struct {
	// Mode is auto, native, dop, convert or off.
	Mode string `json:"mode"`
	// MaxBitRate caps native and DoP: 0 for no cap, else DSD64, DSD128 or
	// DSD256.
	MaxBitRate int `json:"maxBitRate"`
	// Revision is the choice's, 1 for the first, one more at every choice
	// written since: what B9's compare-and-set compares.
	Revision uint64    `json:"revision"`
	SetAt    time.Time `json:"setAt"`
	// SetBy is app, console or migration.
	SetBy string `json:"setBy"`
}

// DoPConfirmation is a DAC's DoP confirmation. It holds for the firmware it
// was given under (BCDDevice) and lapses on another (the design's section
// 1.2).
type DoPConfirmation struct {
	// BCDDevice is the unit's descriptor firmware it was given under, four
	// lowercase hexadecimal digits.
	BCDDevice   string    `json:"bcdDevice"`
	ConfirmedAt time.Time `json:"confirmedAt"`
	// SetBy is app, console or migration.
	SetBy string `json:"setBy"`
}

// FirstUse is a transport's first-use acknowledgement, under the firmware it
// was given for.
type FirstUse struct {
	// Transport is native or dop.
	Transport string `json:"transport"`
	BCDDevice string `json:"bcdDevice"`
	// BitRates are the rates it covers, ascending: nil for every rate, which
	// a write that carried the acknowledgement makes, and the rates the
	// owner's record approved for a migrated one, never none (a present but
	// empty list does not decode: it would read as every rate).
	BitRates       []int     `json:"bitRates,omitempty"`
	AcknowledgedAt time.Time `json:"acknowledgedAt"`
	// ObservedAt is when 5 s of content played under it with no fault (B7):
	// zero until then. A restore drops it.
	ObservedAt time.Time `json:"observedAt,omitzero"`
}

// Covers reports whether f covers the rate.
func (f FirstUse) Covers(bitRate int) bool {
	return f.BitRates == nil || slices.Contains(f.BitRates, bitRate)
}

// Answer is the latest listener answer for a transport, a rate and a
// firmware.
type Answer struct {
	Transport string `json:"transport"`
	BitRate   int    `json:"bitRate"`
	BCDDevice string `json:"bcdDevice"`
	// Answer is clean, noise or nothing.
	Answer string `json:"answer"`
	// On is the answer's date, YYYY-MM-DD: "" for an undated one, which a
	// migrated schema-1 record gives.
	On string `json:"on,omitempty"`
}

// Evidence is what was observed on this box of a transport in a format, at a
// rate, under a firmware. Every field but the four that key it is an
// observation, which a restore drops.
type Evidence struct {
	Transport string `json:"transport"`
	// Format is the ALSA format: DSD_U32_BE, DSD_U32_LE or DSD_U16_LE for
	// native, S32_LE for DoP.
	Format    string `json:"format"`
	BitRate   int    `json:"bitRate"`
	BCDDevice string `json:"bcdDevice"`
	// DriverAcceptedAt is when the driver took the format exactly, with the
	// kernel release it was established under.
	DriverAcceptedAt time.Time `json:"driverAcceptedAt,omitzero"`
	Kernel           string    `json:"kernel,omitempty"`
	// StreamingObservedAt is when 5 s of its content played with no fault.
	StreamingObservedAt time.Time `json:"streamingObservedAt,omitzero"`
}

// observed reports whether e holds an observation.
func (e Evidence) observed() bool {
	return !e.DriverAcceptedAt.IsZero() || !e.StreamingObservedAt.IsZero() || e.Kernel != ""
}

// Warning is one thing the app must be able to explain.
type Warning struct {
	// Code is one of the four warning codes.
	Code string
	// Reason is migration_record_refused's reason word, "" for every other
	// code.
	Reason string
	// DAC is the DAC the warning is owned by: a choice written for it clears
	// the warning. Nil for a warning of the bridge's.
	DAC *dacprofile.Key
	At  time.Time
}

// The migration marker's origins.
const (
	// FromKeys: the legacy keys were migrated.
	FromKeys = "keys"
	// FromStateUnreadable: the state did not read, and the fresh state that
	// took its place says no migration runs over it.
	FromStateUnreadable = "state_unreadable"
)

// What a migration's part still waits for.
const (
	// WaitingForBoundDAC: the migration's part for a DAC waits for the
	// binding to resolve to one.
	WaitingForBoundDAC = "bound_dac"
	// WaitingForRecordModel: the DAC part went to a DAC of another model than
	// the owner's record's, and the record waits for its own.
	WaitingForRecordModel = "record_model"
)

// Marker is the migration's marker (the design's sections 2.5 and 3.6): set
// once the migration has run, so that it never runs again over the state,
// and holding what the migration took, the legacy keys as bridge.yaml held
// them at its first start, and what of it still waits.
type Marker struct {
	// From is keys or state_unreadable.
	From string `json:"from"`
	// DSDMode, AllowDoP and DSDQualification are the legacy keys as written:
	// "" (or nil) where unset.
	DSDMode          string `json:"dsdMode,omitempty"`
	AllowDoP         *bool  `json:"allowDoP,omitempty"`
	DSDQualification string `json:"dsdQualification,omitempty"`
	// WaitingFor is what the migration's part for a DAC still waits for:
	// bound_dac, record_model, or "" when nothing does.
	WaitingFor string `json:"waitingFor,omitempty"`
	// RecordSHA256 is the owner's record's digest once the migration took
	// it, "" until then and when it never does.
	RecordSHA256 string `json:"recordSHA256,omitempty"`
}

// allowDoP is allowDoP as the legacy reader took it: nil is false.
func (m Marker) allowDoP() bool { return m.AllowDoP != nil && *m.AllowDoP }

// Defaults is a fresh install's state: defaultMode auto and nothing else.
func Defaults() State { return State{DefaultMode: ModeAuto} }

// Refusing is the state that takes the place of one that did not read (the
// design's section 2.5): defaultMode off, so a DAC with no choice of its own
// is never converted on a guess once a choice for another clears the
// refusal; the state_unreadable warning, which refuses DSD at every rate until
// a choice is written; and a marker, so the next start does not migrate the
// legacy keys over it.
func Refusing(at time.Time) State {
	return State{
		DefaultMode:  ModeOff,
		MigratedFrom: &Marker{From: FromStateUnreadable},
		Warnings:     []Warning{{Code: WarningStateUnreadable, At: stamp(at)}},
	}
}

// stamp is a time as the state records it: UTC, to the second.
func stamp(t time.Time) time.Time { return t.UTC().Truncate(time.Second) }

// Unreadable reports whether the state refuses DSD at every rate because a
// state did not read (RefusalStateUnreadable): its state_unreadable warning,
// which a choice written for any DAC clears.
func (s State) Unreadable() bool {
	return slices.ContainsFunc(s.Warnings, func(w Warning) bool { return w.Code == WarningStateUnreadable })
}

// DAC is a copy of what the state holds for key's model.
func (s State) DAC(key dacprofile.Key) (DAC, bool) {
	if i := s.dacIndex(key); i >= 0 {
		return s.DACs[i].clone(), true
	}
	return DAC{}, false
}

// ChoiceFor is the choice that applies to key's model: its own when it has
// one, else defaultMode with no cap (own false). It says nothing of a state
// that does not read, which refuses every rate whatever the choice
// (Unreadable): ask that first.
func (s State) ChoiceFor(key dacprofile.Key) (mode string, maxBitRate int, own bool) {
	if i := s.dacIndex(key); i >= 0 && s.DACs[i].Choice != nil {
		return s.DACs[i].Choice.Mode, s.DACs[i].Choice.MaxBitRate, true
	}
	return s.DefaultMode, 0, false
}

// ClearWarningsFor is what a choice written for key's model by the app or
// the console clears (the design's section 2.5): that DAC's warnings, and
// state_unreadable, which no DAC owns. migrated_off stays, since defaultMode
// off still applies to every DAC with no choice. A choice the migration
// writes clears nothing, not the warning written with it.
func (s *State) ClearWarningsFor(key dacprofile.Key) {
	s.Warnings = slices.DeleteFunc(s.Warnings, func(w Warning) bool {
		return w.Code == WarningStateUnreadable || w.DAC != nil && w.DAC.Equal(key)
	})
}

// dacIndex is key's entry, or -1.
func (s State) dacIndex(key dacprofile.Key) int {
	return slices.IndexFunc(s.DACs, func(d DAC) bool { return d.Key.Equal(key) })
}

// Clone is a copy of s that shares nothing with it.
func (s State) Clone() State {
	c := s
	c.DACs = nil
	for _, d := range s.DACs {
		c.DACs = append(c.DACs, d.clone())
	}
	if s.MigratedFrom != nil {
		m := *s.MigratedFrom
		if m.AllowDoP != nil {
			v := *m.AllowDoP
			m.AllowDoP = &v
		}
		c.MigratedFrom = &m
	}
	c.Warnings = nil
	for _, w := range s.Warnings {
		if w.DAC != nil {
			k := *w.DAC
			w.DAC = &k
		}
		c.Warnings = append(c.Warnings, w)
	}
	return c
}

// clone is a copy of d that shares nothing with it.
func (d DAC) clone() DAC {
	c := d
	if d.Choice != nil {
		v := *d.Choice
		c.Choice = &v
	}
	if d.DoPConfirmed != nil {
		v := *d.DoPConfirmed
		c.DoPConfirmed = &v
	}
	c.FirstUse = nil
	for _, f := range d.FirstUse {
		f.BitRates = slices.Clone(f.BitRates)
		c.FirstUse = append(c.FirstUse, f)
	}
	c.Listener = slices.Clone(d.Listener)
	c.Evidence = slices.Clone(d.Evidence)
	return c
}

// empty reports whether d holds nothing, which the state does not keep.
func (d DAC) empty() bool {
	return d.Choice == nil && d.DoPConfirmed == nil && len(d.FirstUse) == 0 && len(d.Listener) == 0 && len(d.Evidence) == 0
}

// The file's shape. Every key is written, every list as [], a key's absent
// interface and product as null.
type fileJSON struct {
	SchemaVersion int           `json:"schemaVersion"`
	DefaultMode   string        `json:"defaultMode"`
	DACs          []dacJSON     `json:"dacs"`
	MigratedFrom  *Marker       `json:"migratedFrom"`
	Warnings      []warningJSON `json:"warnings"`
}

type keyJSON struct {
	VendorID  string  `json:"vendorId"`
	ProductID string  `json:"productId"`
	Interface *int    `json:"interface"`
	Product   *string `json:"product"`
}

type dacJSON struct {
	Key          keyJSON          `json:"key"`
	Choice       *Choice          `json:"choice"`
	DoPConfirmed *DoPConfirmation `json:"dopConfirmed"`
	FirstUse     []FirstUse       `json:"firstUse"`
	Listener     []Answer         `json:"listener"`
	Evidence     []Evidence       `json:"evidence"`
}

type warningJSON struct {
	Code   string    `json:"code"`
	Reason string    `json:"reason,omitempty"`
	DAC    *keyJSON  `json:"dac,omitempty"`
	At     time.Time `json:"at"`
}

// keyOf is a key as the file holds it.
func keyOf(k dacprofile.Key) keyJSON {
	j := keyJSON{VendorID: usbid.Hex16(k.VendorID()), ProductID: usbid.Hex16(k.ProductID())}
	if i, ok := k.Interface(); ok {
		j.Interface = &i
	}
	if p, ok := k.Product(); ok {
		j.Product = &p
	}
	return j
}

// key is the key j holds: its IDs in their canonical form, four lowercase
// hexadecimal digits, and what dacprofile.NewKey takes.
func (j keyJSON) key() (dacprofile.Key, error) {
	v, err := hex16(j.VendorID)
	if err != nil {
		return dacprofile.Key{}, fmt.Errorf("vendorId %w", err)
	}
	p, err := hex16(j.ProductID)
	if err != nil {
		return dacprofile.Key{}, fmt.Errorf("productId %w", err)
	}
	return dacprofile.NewKey(v, p, j.Interface, j.Product)
}

// hex16 reads four lowercase hexadecimal digits, usbid.Hex16's form alone.
func hex16(s string) (uint16, error) {
	v, err := usbid.ParseHex16(s)
	if err != nil || usbid.Hex16(v) != s {
		return 0, errors.New("is not four lowercase hexadecimal digits")
	}
	return v, nil
}

// Decode reads a state from its file's bytes, refusing anything a writer of
// this schema would not write: data past the file's bound, an unknown field,
// data after the value, another schema, and any value check refuses. It
// writes nothing and keeps nothing.
func Decode(data []byte) (State, error) {
	if len(data) > maxFileBytes {
		return State{}, fmt.Errorf("dsdstate: more than %d bytes", maxFileBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var f fileJSON
	if err := dec.Decode(&f); err != nil {
		return State{}, fmt.Errorf("dsdstate: does not parse: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return State{}, errors.New("dsdstate: data after the state")
	}
	if f.SchemaVersion != SchemaVersion {
		return State{}, fmt.Errorf("dsdstate: schema %d, not %d", f.SchemaVersion, SchemaVersion)
	}
	s := State{DefaultMode: f.DefaultMode, MigratedFrom: f.MigratedFrom}
	for i, d := range f.DACs {
		k, err := d.Key.key()
		if err != nil {
			return State{}, fmt.Errorf("dsdstate: dacs[%d].key: %w", i, err)
		}
		s.DACs = append(s.DACs, DAC{Key: k, Choice: d.Choice, DoPConfirmed: d.DoPConfirmed, FirstUse: orNil(d.FirstUse), Listener: orNil(d.Listener), Evidence: orNil(d.Evidence)})
	}
	for i, w := range f.Warnings {
		out := Warning{Code: w.Code, Reason: w.Reason, At: w.At}
		if w.DAC != nil {
			k, err := w.DAC.key()
			if err != nil {
				return State{}, fmt.Errorf("dsdstate: warnings[%d].dac: %w", i, err)
			}
			out.DAC = &k
		}
		s.Warnings = append(s.Warnings, out)
	}
	if err := s.check(); err != nil {
		return State{}, err
	}
	return s, nil
}

// encode is the file s is written as: s normalized (Normalize), checked,
// indented, and a newline after it, and refused (ErrTooLarge) past the bytes
// Decode reads, so no write leaves a file the next start refuses.
func (s State) encode() ([]byte, error) {
	n := s.normalized()
	if err := n.check(); err != nil {
		return nil, err
	}
	f := fileJSON{SchemaVersion: SchemaVersion, DefaultMode: n.DefaultMode, DACs: []dacJSON{}, MigratedFrom: n.MigratedFrom, Warnings: []warningJSON{}}
	for _, d := range n.DACs {
		f.DACs = append(f.DACs, dacJSON{Key: keyOf(d.Key), Choice: d.Choice, DoPConfirmed: d.DoPConfirmed,
			FirstUse: orEmpty(d.FirstUse), Listener: orEmpty(d.Listener), Evidence: orEmpty(d.Evidence)})
	}
	for _, w := range n.Warnings {
		out := warningJSON{Code: w.Code, Reason: w.Reason, At: w.At}
		if w.DAC != nil {
			k := keyOf(*w.DAC)
			out.DAC = &k
		}
		f.Warnings = append(f.Warnings, out)
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return nil, err
	}
	if len(data)+1 > maxFileBytes {
		return nil, ErrTooLarge
	}
	return append(data, '\n'), nil
}

// ErrTooLarge is a write refused because the state's file would be past the
// bytes its reader reads (1 MiB, Decode's bound), which the next start would
// refuse (Codex on #1405: the structural bounds let 100 DACs of 64 answers
// each through at about 1.1 MB). The state stays as it was, as for any write
// not made; B9 answers it as it answers a write it could not make
// (persist_failed, nothing changed), so the app keeps the choice it read.
var ErrTooLarge = fmt.Errorf("dsdstate: the state's file would be more than the %d bytes its reader reads", maxFileBytes)

// orNil is v, or nil in an empty list's place: a state in memory holds no
// empty list, as the file writes none but [], which reads as nil. A first
// use's bitRates is not such a list: there nil and empty differ, and check
// refuses the empty one.
func orNil[T any](v []T) []T {
	if len(v) == 0 {
		return nil
	}
	return v
}

// orEmpty is v, or an empty list in nil's place, so the file writes [].
func orEmpty[T any](v []T) []T {
	if v == nil {
		return []T{}
	}
	return v
}

// normalized is s in the one order the file is written in: the DACs by
// their keys' canonical forms, a DAC entry that holds nothing dropped, each
// list by what keys its entries (native before DoP), and the warnings the
// bridge's first, then by their DACs' keys, then by code.
func (s State) normalized() State {
	n := s.Clone()
	n.DACs = slices.DeleteFunc(n.DACs, DAC.empty)
	slices.SortFunc(n.DACs, func(a, b DAC) int { return cmp.Compare(a.Key.Canonical(), b.Key.Canonical()) })
	for i := range n.DACs {
		d := &n.DACs[i]
		slices.SortFunc(d.FirstUse, func(a, b FirstUse) int { return cmp.Compare(transportOrder(a.Transport), transportOrder(b.Transport)) })
		slices.SortFunc(d.Listener, func(a, b Answer) int {
			return cmp.Or(cmp.Compare(transportOrder(a.Transport), transportOrder(b.Transport)), cmp.Compare(a.BitRate, b.BitRate), cmp.Compare(a.BCDDevice, b.BCDDevice))
		})
		slices.SortFunc(d.Evidence, func(a, b Evidence) int {
			return cmp.Or(cmp.Compare(transportOrder(a.Transport), transportOrder(b.Transport)), cmp.Compare(a.Format, b.Format), cmp.Compare(a.BitRate, b.BitRate), cmp.Compare(a.BCDDevice, b.BCDDevice))
		})
	}
	slices.SortStableFunc(n.Warnings, func(a, b Warning) int {
		return cmp.Or(cmp.Compare(ownerOrder(a), ownerOrder(b)), cmp.Compare(a.Code, b.Code))
	})
	return n
}

// transportOrder puts native before DoP.
func transportOrder(t string) int {
	if t == TransportNative {
		return 0
	}
	return 1
}

// ownerOrder is a warning's owner as it sorts: the bridge's first.
func ownerOrder(w Warning) string {
	if w.DAC == nil {
		return ""
	}
	return "=" + w.DAC.Canonical()
}

// check refuses a state no writer of this schema writes. Its refusals name
// the field and never quote a value.
func (s State) check() error {
	if s.DefaultMode != ModeAuto && s.DefaultMode != ModeOff {
		return errors.New("dsdstate: defaultMode is neither auto nor off")
	}
	if len(s.DACs) > maxDACs {
		return fmt.Errorf("dsdstate: more than %d DACs", maxDACs)
	}
	seen := map[string]bool{}
	for i, d := range s.DACs {
		c := d.Key.Canonical()
		if seen[c] {
			return fmt.Errorf("dsdstate: dacs[%d] is a model another entry holds", i)
		}
		seen[c] = true
		if d.empty() {
			return fmt.Errorf("dsdstate: dacs[%d] holds nothing", i)
		}
		if err := d.check(); err != nil {
			return fmt.Errorf("dsdstate: dacs[%d].%w", i, err)
		}
	}
	if s.MigratedFrom != nil {
		if err := s.MigratedFrom.check(); err != nil {
			return fmt.Errorf("dsdstate: migratedFrom: %w", err)
		}
	}
	if len(s.Warnings) > maxWarnings {
		return fmt.Errorf("dsdstate: more than %d warnings", maxWarnings)
	}
	owned := map[string]bool{}
	for i, w := range s.Warnings {
		if err := w.check(); err != nil {
			return fmt.Errorf("dsdstate: warnings[%d]: %w", i, err)
		}
		id := w.Code + " " + ownerOrder(w)
		if owned[id] {
			return fmt.Errorf("dsdstate: warnings[%d] repeats a code its owner has", i)
		}
		owned[id] = true
	}
	return nil
}

func (d DAC) check() error {
	if c := d.Choice; c != nil {
		switch {
		case !slices.Contains([]string{ModeAuto, ModeNative, ModeDoP, ModeConvert, ModeOff}, c.Mode):
			return errors.New("choice.mode is not a mode")
		case c.MaxBitRate != 0 && !validRate(c.MaxBitRate):
			return errors.New("choice.maxBitRate is neither 0 nor a DSD rate")
		case c.Revision < 1:
			return errors.New("choice.revision is not 1 or more")
		case c.SetAt.IsZero():
			return errors.New("choice.setAt is missing")
		case !validBy(c.SetBy):
			return errors.New("choice.setBy is not app, console or migration")
		}
	}
	if c := d.DoPConfirmed; c != nil {
		switch {
		case !canonicalHex16(c.BCDDevice):
			return errors.New("dopConfirmed.bcdDevice is not four lowercase hexadecimal digits")
		case c.ConfirmedAt.IsZero():
			return errors.New("dopConfirmed.confirmedAt is missing")
		case !validBy(c.SetBy):
			return errors.New("dopConfirmed.setBy is not app, console or migration")
		}
	}
	transports := map[string]bool{}
	for i, f := range d.FirstUse {
		switch {
		case !validTransport(f.Transport):
			return fmt.Errorf("firstUse[%d].transport is neither native nor dop", i)
		case transports[f.Transport]:
			return fmt.Errorf("firstUse[%d] is a second acknowledgement of its transport", i)
		case !canonicalHex16(f.BCDDevice):
			return fmt.Errorf("firstUse[%d].bcdDevice is not four lowercase hexadecimal digits", i)
		case f.BitRates != nil && len(f.BitRates) == 0:
			return fmt.Errorf("firstUse[%d].bitRates is empty, which would read as every rate", i)
		case !ascendingRates(f.BitRates):
			return fmt.Errorf("firstUse[%d].bitRates are not DSD rates, each once, ascending", i)
		case f.AcknowledgedAt.IsZero():
			return fmt.Errorf("firstUse[%d].acknowledgedAt is missing", i)
		}
		transports[f.Transport] = true
	}
	if len(d.Listener) > maxListPerDAC || len(d.Evidence) > maxListPerDAC {
		return fmt.Errorf("listener or evidence holds more than %d entries", maxListPerDAC)
	}
	answers := map[string]bool{}
	for i, a := range d.Listener {
		id := a.Transport + "/" + strconv.Itoa(a.BitRate) + "/" + a.BCDDevice
		switch {
		case !validTransport(a.Transport):
			return fmt.Errorf("listener[%d].transport is neither native nor dop", i)
		case !validRate(a.BitRate):
			return fmt.Errorf("listener[%d].bitRate is not a DSD rate", i)
		case !canonicalHex16(a.BCDDevice):
			return fmt.Errorf("listener[%d].bcdDevice is not four lowercase hexadecimal digits", i)
		case a.Answer != AnswerClean && a.Answer != AnswerNoise && a.Answer != AnswerNothing:
			return fmt.Errorf("listener[%d].answer is not clean, noise or nothing", i)
		case a.On != "" && !validDate(a.On):
			return fmt.Errorf("listener[%d].on is not a date", i)
		case answers[id]:
			return fmt.Errorf("listener[%d] is a second answer for its transport, rate and firmware", i)
		}
		answers[id] = true
	}
	observed := map[string]bool{}
	for i, e := range d.Evidence {
		id := e.Transport + "/" + e.Format + "/" + strconv.Itoa(e.BitRate) + "/" + e.BCDDevice
		switch {
		case !validFormat(e.Transport, e.Format):
			return fmt.Errorf("evidence[%d] is not a transport in a format it plays", i)
		case !validRate(e.BitRate):
			return fmt.Errorf("evidence[%d].bitRate is not a DSD rate", i)
		case !canonicalHex16(e.BCDDevice):
			return fmt.Errorf("evidence[%d].bcdDevice is not four lowercase hexadecimal digits", i)
		case !e.observed():
			return fmt.Errorf("evidence[%d] holds no observation", i)
		case e.Kernel != "" && e.DriverAcceptedAt.IsZero():
			return fmt.Errorf("evidence[%d].kernel is given without driverAcceptedAt", i)
		case !validText(e.Kernel, maxKernelBytes):
			return fmt.Errorf("evidence[%d].kernel is not a kernel release", i)
		case observed[id]:
			return fmt.Errorf("evidence[%d] repeats its transport, format, rate and firmware", i)
		}
		observed[id] = true
	}
	return nil
}

func (m Marker) check() error {
	switch m.From {
	case FromStateUnreadable:
		if m != (Marker{From: FromStateUnreadable}) {
			return errors.New("a state_unreadable marker holds nothing else")
		}
		return nil
	case FromKeys:
	default:
		return errors.New("from is neither keys nor state_unreadable")
	}
	part := dacPart(m.DSDMode, m.allowDoP())
	switch {
	case !slices.Contains([]string{"", ModeOff, ModeNative, ModeDoP, ModeAuto}, m.DSDMode):
		return errors.New("dsdMode is not a legacy mode")
	case m.DSDMode == "" && m.AllowDoP == nil && m.DSDQualification == "":
		return errors.New("a migration of the keys names none")
	case !validName(m.DSDQualification):
		return errors.New("dsdQualification is not a name")
	case m.WaitingFor != "" && m.WaitingFor != WaitingForBoundDAC && m.WaitingFor != WaitingForRecordModel:
		return errors.New("waitingFor is neither bound_dac nor record_model")
	case m.WaitingFor != "" && !part:
		return errors.New("waitingFor is given for keys that give a DAC nothing")
	case m.WaitingFor == WaitingForRecordModel && m.DSDQualification == "":
		return errors.New("waitingFor record_model names no record")
	case m.RecordSHA256 != "" && (!validDigest(m.RecordSHA256) || m.WaitingFor != "" || m.DSDQualification == "" || !part):
		return errors.New("recordSHA256 is not the digest of a record the keys named, taken")
	}
	return nil
}

func (w Warning) check() error {
	switch w.Code {
	case WarningMigrationRecordRefused:
		if !slices.Contains([]string{ReasonStaleVersion, ReasonIdentityMismatch, ReasonHashMismatch, ReasonUnreadable}, w.Reason) {
			return errors.New("migration_record_refused's reason is not a reason word")
		}
	case WarningMigrationRecordMissing, WarningMigratedOff, WarningStateUnreadable:
		if w.Reason != "" {
			return errors.New("a reason is given for a code that has none")
		}
	default:
		return errors.New("code is not a warning code")
	}
	perDAC := w.Code == WarningMigrationRecordRefused || w.Code == WarningMigrationRecordMissing
	switch {
	case perDAC && w.DAC == nil:
		return errors.New("a migration's warning names no DAC")
	case !perDAC && w.DAC != nil:
		return errors.New("a warning of the bridge's names a DAC")
	case w.At.IsZero():
		return errors.New("at is missing")
	}
	return nil
}

// dacPart reports whether the legacy keys give the DAC the migration's part
// goes to anything: native, dop with allowDoP, and auto; never unset, off,
// or dop without allowDoP, which are the bridge's alone.
func dacPart(mode string, allowDoP bool) bool {
	return mode == ModeNative || mode == ModeAuto || mode == ModeDoP && allowDoP
}

func validRate(r int) bool { return r == DSD64 || r == DSD128 || r == DSD256 }

// ascendingRates reports whether rates are DSD rates, each once, ascending.
func ascendingRates(rates []int) bool {
	for i, r := range rates {
		if !validRate(r) || i > 0 && rates[i-1] >= r {
			return false
		}
	}
	return true
}

func validTransport(t string) bool { return t == TransportNative || t == TransportDoP }

func validBy(by string) bool { return by == ByApp || by == ByConsole || by == ByMigration }

// validFormat reports whether format is one transport plays in.
func validFormat(transport, format string) bool {
	switch transport {
	case TransportNative:
		return format == "DSD_U32_BE" || format == "DSD_U32_LE" || format == "DSD_U16_LE"
	case TransportDoP:
		return format == "S32_LE"
	}
	return false
}

func canonicalHex16(s string) bool {
	_, err := hex16(s)
	return err == nil
}

// validDate is YYYY-MM-DD, a real day.
func validDate(s string) bool {
	d, err := time.Parse(time.DateOnly, s)
	return err == nil && d.Format(time.DateOnly) == s
}

// validDigest is a SHA-256 in lowercase hexadecimal.
func validDigest(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// validText is "" or UTF-8 of at most limit bytes holding no control
// character.
func validText(s string, limit int) bool {
	return len(s) <= limit && utf8.ValidString(s) && !strings.ContainsFunc(s, unicode.IsControl)
}

// validName is "" or a name of a file: UTF-8 of at most maxNameBytes bytes
// with no NUL.
func validName(s string) bool {
	return len(s) <= maxNameBytes && utf8.ValidString(s) && !strings.ContainsRune(s, 0)
}
