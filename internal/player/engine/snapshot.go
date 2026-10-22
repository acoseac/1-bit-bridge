package engine

import (
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"
)

// State is the transport's coarse state, the one a generation follows.
type State int

const (
	// Stopped holds no session.
	Stopped State = iota
	// Playing has a session that plays, or is starting
	// (Snapshot.Starting: nothing of it audible yet).
	Playing
	// Paused has a paused session, or none: an edit that dropped the paused
	// content leaves the transport paused at the start of the selection,
	// and Play starts a session there.
	Paused
	// Releasing has asked a session to release and waits for the
	// acknowledgement; Snapshot.Pending is what follows it.
	Releasing
	// Maintenance is held (EnterMaintenance): every mutation is refused.
	Maintenance
)

var stateNames = [...]string{"stopped", "playing", "paused", "releasing", "maintenance"}

func (s State) String() string {
	if s >= 0 && int(s) < len(stateNames) {
		return stateNames[s]
	}
	return "state " + strconv.Itoa(int(s))
}

// Intent is what follows a release: the one pending intent, the latest
// command's winning.
type Intent int

const (
	// IntentStop leaves the transport stopped.
	IntentStop Intent = iota
	// IntentPause leaves it paused at the start of the selection.
	IntentPause
	// IntentPlay starts a session at the selection.
	IntentPlay
)

var intentNames = [...]string{"stop", "pause", "play"}

func (i Intent) String() string {
	if i >= 0 && int(i) < len(intentNames) {
		return intentNames[i]
	}
	return "intent " + strconv.Itoa(int(i))
}

// Generation names one controller-visible state of one run of the engine:
// a random ID drawn at New and a count that advances on every change of the
// transport state, the queue, the modes, the selected entry, maintenance,
// the output's availability and an unconfirmed release, and never on the
// position, buffering or a starting session's first sound. A command that
// carries IfGeneration runs only at that generation, and a generation from
// another run never matches. The zero Generation matches nothing.
type Generation struct {
	run uint64
	n   uint64
}

// String is the generation as ParseGeneration reads it.
func (g Generation) String() string { return fmt.Sprintf("%016x.%d", g.run, g.n) }

// IsZero reports whether g is the zero Generation.
func (g Generation) IsZero() bool { return g == Generation{} }

// ParseGeneration reads a Generation that String wrote.
func ParseGeneration(s string) (Generation, error) {
	run, n, ok := strings.Cut(s, ".")
	if !ok || len(run) != 16 {
		return Generation{}, fmt.Errorf("player: %q is not a generation", s)
	}
	r, err := strconv.ParseUint(run, 16, 64)
	if err != nil {
		return Generation{}, fmt.Errorf("player: %q is not a generation", s)
	}
	c, err := strconv.ParseUint(n, 10, 64)
	if err != nil {
		return Generation{}, fmt.Errorf("player: %q is not a generation", s)
	}
	return Generation{run: r, n: c}, nil
}

// Cond is a condition a mutating command carries.
type Cond struct{ gen Generation }

// IfGeneration runs a command only at generation g: at any other the
// command is refused with ReasonConflict and changes nothing.
func IfGeneration(g Generation) Cond { return Cond{gen: g} }

// Lease is what EnterMaintenance hands its caller, and only its caller:
// no snapshot holds it, so no reader can exit or renew a hold.
type Lease struct{ b [16]byte }

// String is the lease as ParseLease reads it.
func (l Lease) String() string { return hex.EncodeToString(l.b[:]) }

// IsZero reports whether l is the zero Lease, which no hold has.
func (l Lease) IsZero() bool { return l == Lease{} }

// ParseLease reads a Lease that String wrote.
func ParseLease(s string) (Lease, error) {
	var l Lease
	if len(s) != 2*len(l.b) {
		return Lease{}, fmt.Errorf("player: not a lease")
	}
	if _, err := hex.Decode(l.b[:], []byte(s)); err != nil {
		return Lease{}, fmt.Errorf("player: not a lease")
	}
	return l, nil
}

// Hold is what EnterMaintenance answers: the lease, the hold's expiry, and
// whether the release it asked for was confirmed (false: entered, release
// unconfirmed).
type Hold struct {
	Lease            Lease
	Expires          time.Time
	ReleaseConfirmed bool
}

// Audible is the occurrence whose content is being heard.
type Audible struct {
	Occurrence OccurrenceID
	Entry      uint32
}

// OutputReport is the output's availability as an adapter reports it
// (4b2 adapts 4a2's hotplug watcher to it).
type OutputReport struct {
	// Generation orders reports: a report whose Generation is not above the
	// last applied one is discarded. It starts above 0 and is monotonic for
	// the engine's whole lifetime, which is the adapter's to keep across a
	// replaced watcher, or the input freezes.
	Generation uint64
	Available  bool
	// Reason is why it is unavailable (device_missing, device_ambiguous, …).
	Reason string
	// UnitChanged is the bound unit changed or re-enumerated since the last
	// report.
	UnitChanged bool
}

// OutputState is the last report applied. Before the first, Known is false
// and the output counts as unavailable.
type OutputState struct {
	Known      bool
	Available  bool
	Reason     string
	Generation uint64
}

// ReleaseInfo is a release that was not confirmed by its deadline.
type ReleaseInfo struct {
	Session SessionID
	Since   time.Time
	// Paused is a paused session's own release of the device after
	// PauseRelease, rather than one the engine asked for.
	Paused bool
}

// MaintenanceInfo is a maintenance hold as every reader sees it: never its
// lease.
type MaintenanceInfo struct {
	Reason           string
	Since, Expires   time.Time
	ReleaseConfirmed bool
}

// Skip is why an entry was skipped: refused before any of its content
// played. It stays until the entry plays or is deleted.
type Skip struct {
	Reason Reason
	Detail string
	At     time.Time
}

// StopInfo is why playback last stopped.
type StopInfo struct {
	Reason Reason
	Detail string
	// Entry is the selection when it stopped.
	Entry uint32
	At    time.Time
}

// EditState says whether inserts and the other token-advancing edits are
// taken: Refused is "" when they are, and otherwise the reason each is
// refused with (a reservation that could not be made durable, a file that
// could not be moved aside, IDs exhausted).
type EditState struct {
	Refused Reason
	Detail  string
}

// PersistState is the queue file's: the revision of the state, the last
// one written, and the last write's error and the failures in a row until
// one succeeds.
type PersistState struct {
	Revision uint64
	Written  uint64
	Err      string
	Failures int
}

// MaxFaults is the number of faults a snapshot keeps the details of.
const MaxFaults = 8

// Snapshot is the engine's state after one change: immutable, published by
// the owner after every change, and read without waiting on it. Every
// accessor returns a copy.
type Snapshot struct {
	gen         Generation
	state       State
	starting    bool
	pending     Intent
	q           *queue
	token       uint32
	selected    uint32
	audible     Audible
	pos         Position
	repeat      bool
	shuffle     bool
	output      OutputState
	maint       *MaintenanceInfo
	unconfirmed *ReleaseInfo
	devReleased bool
	resumed     ResumeResult
	resumeErr   string
	lastStop    StopInfo
	skips       map[uint32]Skip
	faults      []Fault
	edits       EditState
	persist     PersistState
	join        *Join
	closed      bool
}

// Generation is the snapshot's generation.
func (s *Snapshot) Generation() Generation { return s.gen }

// State is the transport's state.
func (s *Snapshot) State() State { return s.state }

// Starting reports a playing session nothing of which is audible yet.
func (s *Snapshot) Starting() bool { return s.starting }

// Pending is what follows the release while Releasing.
func (s *Snapshot) Pending() Intent { return s.pending }

// Selected is the selected entry: what a controller reads as the current
// ID. It moves on an audible report during playback, and at once on a
// command, a deletion or a skip; it is 0 or an ID in the queue.
func (s *Snapshot) Selected() uint32 { return s.selected }

// Audible is the occurrence being heard, and false when none is: after a
// command, until the new session reports.
func (s *Snapshot) Audible() (Audible, bool) { return s.audible, s.audible.Occurrence != 0 }

// Position is the content of the audible occurrence played, as the backend
// last measured it; it stays where it was when playback pauses or stops.
func (s *Snapshot) Position() Position { return s.pos }

// Repeat is repeat-all.
func (s *Snapshot) Repeat() bool { return s.repeat }

// Shuffle is always false in this release.
func (s *Snapshot) Shuffle() bool { return s.shuffle }

// Token advances on every change of the queue's membership or order. It is
// 0, no token, while the run has none it may hand out: its reservation is
// not durable yet (Edits says why), or every token has been handed out.
func (s *Snapshot) Token() uint32 { return s.token }

// Len is the number of entries.
func (s *Snapshot) Len() int { return s.q.len() }

// IDs is the entries' IDs in order.
func (s *Snapshot) IDs() []uint32 { return s.q.ids() }

// IDArray is the token and the IDs, from this one snapshot.
func (s *Snapshot) IDArray() (uint32, []uint32) { return s.token, s.q.ids() }

// Read is the entry id.
func (s *Snapshot) Read(id uint32) (Entry, bool) {
	e, ok := s.q.get(id)
	if !ok {
		return Entry{}, false
	}
	return e.public(), true
}

// ReadList is the entries ids names that the queue holds, in the order
// asked; an ID it does not hold is left out. More than MaxReadList IDs is
// refused with ReasonTooMany.
func (s *Snapshot) ReadList(ids []uint32) ([]Entry, error) {
	if len(ids) > MaxReadList {
		return nil, refuse("read list", ReasonTooMany, fmt.Sprintf("%d IDs, more than %d", len(ids), MaxReadList))
	}
	out := make([]Entry, 0, len(ids))
	for _, id := range ids {
		if e, ok := s.q.get(id); ok {
			out = append(out, e.public())
		}
	}
	return out, nil
}

// IndexOf is id's index in the queue.
func (s *Snapshot) IndexOf(id uint32) (int, bool) { return s.q.pos(id) }

// IDAt is the ID at index i.
func (s *Snapshot) IDAt(i int) (uint32, bool) {
	if i < 0 || i >= s.q.len() {
		return 0, false
	}
	return s.q.entries[i].id, true
}

// Output is the output's availability as last reported.
func (s *Snapshot) Output() OutputState { return s.output }

// Maintenance is the hold, when one is held.
func (s *Snapshot) Maintenance() (MaintenanceInfo, bool) {
	if s.maint == nil {
		return MaintenanceInfo{}, false
	}
	return *s.maint, true
}

// ReleaseUnconfirmed is the release past its deadline, when there is one:
// until its acknowledgement nothing opens the device.
func (s *Snapshot) ReleaseUnconfirmed() (ReleaseInfo, bool) {
	if s.unconfirmed == nil {
		return ReleaseInfo{}, false
	}
	return *s.unconfirmed, true
}

// DeviceReleased reports a paused session that has released the device.
func (s *Snapshot) DeviceReleased() bool { return s.devReleased }

// LastResume is how the last resume went on, and the reason the last
// refused one gave until a resume succeeds.
func (s *Snapshot) LastResume() (ResumeResult, string) { return s.resumed, s.resumeErr }

// LastStop is why playback last stopped.
func (s *Snapshot) LastStop() StopInfo { return s.lastStop }

// Skipped is why entry id was skipped, when it was.
func (s *Snapshot) Skipped(id uint32) (Skip, bool) {
	sk, ok := s.skips[id]
	return sk, ok
}

// Skips is every entry skipped and why, in a map of the caller's own.
func (s *Snapshot) Skips() map[uint32]Skip {
	m := maps.Clone(s.skips)
	if m == nil {
		m = map[uint32]Skip{}
	}
	return m
}

// Faults is the last MaxFaults faults, oldest first.
func (s *Snapshot) Faults() []Fault { return slices.Clone(s.faults) }

// Edits says whether inserts and the other token-advancing edits are taken.
func (s *Snapshot) Edits() EditState { return s.edits }

// Persist is the queue file's state.
func (s *Snapshot) Persist() PersistState { return s.persist }

// NextJoin is how the prepared successor joins, when one is prepared.
func (s *Snapshot) NextJoin() (Join, bool) {
	if s.join == nil {
		return Join{}, false
	}
	return Join{Gapless: s.join.Gapless, Differs: slices.Clone(s.join.Differs)}, true
}

// Closed reports an engine that has begun to shut down.
func (s *Snapshot) Closed() bool { return s.closed }
