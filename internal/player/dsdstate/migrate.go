package dsdstate

import (
	"errors"
	"slices"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/player/dacprofile"
)

// Legacy is the three legacy keys as bridge.yaml holds them
// (config.PlayerConfig's raw fields, internal/config/player.go): DSDMode ""
// where unset, which its EffectiveDSDMode maps to off with an explicit off, a
// difference the migration keeps (the design's section 3.6); AllowDoP nil
// where unset, which the legacy reader took as false; DSDQualification ""
// where unset.
type Legacy struct {
	DSDMode          string
	AllowDoP         *bool
	DSDQualification string
}

// set reports whether the block sets any of the keys.
func (l Legacy) set() bool { return l.DSDMode != "" || l.AllowDoP != nil || l.DSDQualification != "" }

func (l Legacy) check() error {
	if !slices.Contains([]string{"", ModeOff, ModeNative, ModeDoP, ModeAuto}, l.DSDMode) {
		return errors.New("dsdstate: the legacy dsdMode is not a legacy mode")
	}
	if !validName(l.DSDQualification) {
		return errors.New("dsdstate: the legacy dsdQualification is not a name")
	}
	return nil
}

// RecordOutcome is what the legacy reader answered of the owner's record
// for the bound DAC.
type RecordOutcome int

const (
	// RecordMissing: no record to read, none named or none at its name.
	RecordMissing RecordOutcome = iota + 1
	// RecordRefused: the reader refused it, for Reason.
	RecordRefused
	// RecordTaken: the reader took it for the bound DAC's model.
	RecordTaken
)

// Record is the legacy reader's classified answer for the bound DAC
// (ReadRecord): what the migration's table takes of the owner's record.
type Record struct {
	Outcome RecordOutcome
	// Reason is a refusal's reason word: stale_version, identity_mismatch,
	// hash_mismatch or unreadable.
	Reason string
	// Err is the reader's refusal, for the caller's log line. The state
	// never holds it.
	Err error
	// What a record taken holds: its digest; the unit's firmware it was
	// qualified on, four lowercase hexadecimal digits; its qualification
	// date, "" for schema 1's undated record; and the rates it approved for
	// each transport, ascending, each once.
	SHA256, BCDDevice, QualifiedOn string
	Native, DoP                    []int
}

// Input is one migration step's input: the legacy keys as bridge.yaml holds
// them now, the DAC the binding resolves to now (nil while none does), the
// reader's answer for it when RecordToRead asks for one, and the time.
type Input struct {
	Legacy Legacy
	Bound  *Bound
	Record Record
	Now    time.Time
}

// Step is what one migration step did, for its caller's log lines.
type Step struct {
	// Changed reports a state the step changed, which its caller writes.
	Changed bool
	// Warnings are the warnings the step wrote, each to be logged beside it
	// (B6's Warn line).
	Warnings []Warning
}

// RecordToRead is the owner's record a step on s asks of the bound DAC, by
// the name the migration took (the legacy dsdQualification), and whether it
// asks one: while no migration ran, when the keys give a DAC something
// (native, dop with allowDoP, auto), by the name they hold now; then, while
// the part of a DAC's waits, by the name the marker keeps; and never once the
// part is done, so nothing reads the record again. The name may be "", which
// reads as no record (ReadRecord).
func RecordToRead(s State, l Legacy) (name string, ask bool) {
	m := s.MigratedFrom
	if m == nil {
		if !l.set() || !dacPart(l.DSDMode, l.AllowDoP != nil && *l.AllowDoP) {
			return "", false
		}
		return l.DSDQualification, true
	}
	if m.From != FromKeys || m.WaitingFor == "" {
		return "", false
	}
	return m.DSDQualification, true
}

// Migrate is the migration's step (the design's section 3.6), pure: s's next
// state and what the step did. It runs in two parts, each once.
//
// The part of the bridge's runs while no migration ran (s.MigratedFrom nil)
// and the keys set any key, and writes the marker with the keys as they are
// now, which no later step reads again: with dsdMode unset, nothing else (the
// defaults; the marker still keeps a later edit of the keys from migrating
// over what the user chose since); with an explicit off, or dop with allowDoP
// false or unset, defaultMode off and the bridge's migrated_off warning; with
// native, dop with allowDoP, or auto, a part of a DAC's that waits for the
// binding to resolve to one. With no key set it writes nothing at all.
//
// The part of a DAC's runs once a DAC is bound while it waits, with the
// reader's answer for that DAC (in.Record; ReadRecord compares the model and
// never the port, and takes versions from 35 to this build's):
//   - a record taken: the record's rows: native's choice native with each
//     approved native rate's answer clean; dop's choice dop with each DoP
//     rate's; auto's choice auto with every rate's, native and DoP, whatever
//     allowDoP says (what was heard is a fact); each answer under the
//     record's firmware, on its qualification date or undated; first use
//     acknowledged, for the transports the mode plays (DoP under auto only
//     with allowDoP), at the approved rates alone (BitRates), and only for a
//     transport the record approved a rate of, which confirms DoP for a DoP
//     acknowledgement; and the record's digest in the marker;
//   - no record, or one refused: the fallback's rows: native's choice native
//     and dop's dop, with no answer, acknowledgement or confirmation, so each
//     refuses until the app acknowledges and never converts; auto's nothing
//     (the defaults); and the warning (migration_record_missing, or
//     migration_record_refused with the reason), owned by the DAC;
//   - a refusal of identity_mismatch gives the DAC the fallback and keeps the
//     record for its own model: when the binding later resolves to a DAC the
//     reader takes the record for, that DAC gets the record's rows. Any other
//     answer then writes nothing, and the record waits on.
//
// Once a part is done no step reads the record again, and a DAC bound
// afterwards takes the defaults. A DAC that has a choice of its own is never
// migrated: the part done for it writes nothing for it. A choice the
// migration writes clears no warning, the one written with it included.
func Migrate(s State, in Input) (State, Step, error) {
	if err := in.Legacy.check(); err != nil {
		return s, Step{}, err
	}
	next, step := s.Clone(), Step{}
	at := stamp(in.Now)
	warn := func(w Warning) {
		next.Warnings = append(next.Warnings, w)
		step.Warnings = append(step.Warnings, w)
	}
	if next.MigratedFrom == nil {
		l := in.Legacy
		if !l.set() {
			return s, Step{}, nil
		}
		m := &Marker{From: FromKeys, DSDMode: l.DSDMode, DSDQualification: l.DSDQualification}
		if l.AllowDoP != nil {
			v := *l.AllowDoP
			m.AllowDoP = &v
		}
		switch {
		case m.DSDMode == ModeOff, m.DSDMode == ModeDoP && !m.allowDoP():
			next.DefaultMode = ModeOff
			if !slices.ContainsFunc(next.Warnings, func(w Warning) bool { return w.Code == WarningMigratedOff }) {
				warn(Warning{Code: WarningMigratedOff, At: at})
			}
		case dacPart(m.DSDMode, m.allowDoP()):
			m.WaitingFor = WaitingForBoundDAC
		}
		next.MigratedFrom, step.Changed = m, true
	}
	m := next.MigratedFrom
	if m.From != FromKeys || m.WaitingFor == "" || in.Bound == nil {
		return next, step, nil
	}
	rec := in.Record
	if rec.Outcome < RecordMissing || rec.Outcome > RecordTaken {
		return s, Step{}, errors.New("dsdstate: the owner's record was not read for the bound DAC")
	}
	key := in.Bound.Key
	own := next.dacIndex(key) >= 0 && next.DACs[next.dacIndex(key)].Choice != nil
	switch m.WaitingFor {
	case WaitingForBoundDAC:
		switch {
		case rec.Outcome == RecordTaken:
			if !own {
				next.recordRows(key, *m, rec, at)
				m.RecordSHA256 = rec.SHA256
			}
			m.WaitingFor = ""
		default:
			if !own {
				next.fallbackRows(key, *m, at)
				w := Warning{Code: WarningMigrationRecordMissing, DAC: &key, At: at}
				if rec.Outcome == RecordRefused {
					w.Code, w.Reason = WarningMigrationRecordRefused, rec.Reason
				}
				warn(w)
			}
			m.WaitingFor = ""
			if rec.Outcome == RecordRefused && rec.Reason == ReasonIdentityMismatch {
				m.WaitingFor = WaitingForRecordModel
			}
		}
		step.Changed = true
	case WaitingForRecordModel:
		if rec.Outcome != RecordTaken {
			return next, step, nil
		}
		if !own {
			next.recordRows(key, *m, rec, at)
			m.RecordSHA256 = rec.SHA256
		}
		m.WaitingFor, step.Changed = "", true
	}
	return next, step, nil
}

// recordRows writes the rows of a record taken for key (Migrate's doc), into
// an entry with no choice of its own: an answer, an acknowledgement or a
// confirmation the entry holds already stays.
func (s *State) recordRows(key dacprofile.Key, m Marker, rec Record, at time.Time) {
	d := s.entry(key)
	mode := m.DSDMode
	d.Choice = &Choice{Mode: mode, Revision: 1, SetAt: at, SetBy: ByMigration}
	clean := func(transport string, rates []int) {
		for _, r := range rates {
			if !slices.ContainsFunc(d.Listener, func(a Answer) bool {
				return a.Transport == transport && a.BitRate == r && a.BCDDevice == rec.BCDDevice
			}) {
				d.Listener = append(d.Listener, Answer{Transport: transport, BitRate: r, BCDDevice: rec.BCDDevice, Answer: AnswerClean, On: rec.QualifiedOn})
			}
		}
	}
	acknowledge := func(transport string, rates []int) {
		if len(rates) == 0 || slices.ContainsFunc(d.FirstUse, func(f FirstUse) bool { return f.Transport == transport }) {
			return
		}
		d.FirstUse = append(d.FirstUse, FirstUse{Transport: transport, BCDDevice: rec.BCDDevice, BitRates: slices.Clone(rates), AcknowledgedAt: at})
	}
	confirm := func() {
		if len(rec.DoP) > 0 && d.DoPConfirmed == nil {
			d.DoPConfirmed = &DoPConfirmation{BCDDevice: rec.BCDDevice, ConfirmedAt: at, SetBy: ByMigration}
		}
	}
	switch mode {
	case ModeNative:
		clean(TransportNative, rec.Native)
		acknowledge(TransportNative, rec.Native)
	case ModeDoP:
		clean(TransportDoP, rec.DoP)
		acknowledge(TransportDoP, rec.DoP)
		confirm()
	case ModeAuto:
		clean(TransportNative, rec.Native)
		clean(TransportDoP, rec.DoP)
		acknowledge(TransportNative, rec.Native)
		if m.allowDoP() {
			acknowledge(TransportDoP, rec.DoP)
			confirm()
		}
	}
}

// fallbackRows writes the rows of no record, or a refused one, for key
// (Migrate's doc): native's choice native, dop's dop, auto's nothing.
func (s *State) fallbackRows(key dacprofile.Key, m Marker, at time.Time) {
	if m.DSDMode != ModeNative && m.DSDMode != ModeDoP {
		return
	}
	s.entry(key).Choice = &Choice{Mode: m.DSDMode, Revision: 1, SetAt: at, SetBy: ByMigration}
}

// entry is key's entry, added when the state holds none.
func (s *State) entry(key dacprofile.Key) *DAC {
	if i := s.dacIndex(key); i >= 0 {
		return &s.DACs[i]
	}
	s.DACs = append(s.DACs, DAC{Key: key})
	return &s.DACs[len(s.DACs)-1]
}
